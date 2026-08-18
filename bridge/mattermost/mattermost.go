package mattermost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/davecgh/go-spew/spew"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/matterclient"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/mitchellh/mapstructure"
	"github.com/sirupsen/logrus"
)

const systemUser = "system"

type Mattermost struct {
	mc          *matterclient.Client
	credentials bridge.Credentials
	quitChan    []chan struct{}
	eventChan   chan *bridge.Event
	connected   bool
	instanceTag string

	dmChannelCache   *lru.Cache[string, string]
	msgParentCache   *lru.Cache[string, CachedPost]
	msgLastSentCache *lru.Cache[string, string]

	cfg *config.Config

	//nolint:containedctx
	wsCtx    context.Context
	wsCancel context.CancelFunc

	lastBannerText string
}

type CachedPost struct {
	RootID   string
	ReplyMsg string
}

var logger *logrus.Entry

func (m *Mattermost) Config() any {
	return &m.cfg.Current().Mattermost
}

func (m *Mattermost) BridgeConfig() *config.BridgeConfig {
	return &m.cfg.Current().Mattermost.Bridge
}

func (m *Mattermost) FormatterConfig() *config.FormatterConfig {
	return &m.cfg.Current().Mattermost.Formatter
}

func (m *Mattermost) Connected() bool {
	return m.connected
}

func (m *Mattermost) GetLastSentMsgs() []string {
	data := make([]string, 0)

	for _, k := range m.msgLastSentCache.Keys() {
		if msg, ok := m.msgLastSentCache.Get(k); ok {
			data = append(data, "[@@"+fmt.Sprint(k)+"] "+msg)
		}
	}

	return data
}

func (m *Mattermost) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	// TODO: Switch from using GetPostsSince() to GetPostsAfter()
	// TODO: which also does pagination rather than the 200 post limit.
	// TODO: Or maybe a combination with GetPostsSince() getting the
	// TODO: first post ID to use for GetPostsAfter().
	return m.postListToEvents(ctx, m.mc.GetPostsSince(ctx, channelID, since), "replay", since)
}

//nolint:funlen
func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onWsConnect func()) (bridge.Bridger, *matterclient.Client, error) {
	m := &Mattermost{
		credentials: cred,
		eventChan:   eventChan,
		cfg:         cfg,
	}

	rc := cfg.Current()

	m.dmChannelCache, _ = lru.New[string, string](128)
	m.msgParentCache, _ = lru.New[string, CachedPost](128)
	m.msgLastSentCache, _ = lru.New[string, string](128)

	ourlog := logrus.New()
	ourlog.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	logger = ourlog.WithFields(logrus.Fields{"prefix": "bridge/mattermost"})
	if rc.Debug {
		ourlog.SetLevel(logrus.DebugLevel)
	}

	if rc.Trace {
		ourlog.SetLevel(logrus.TraceLevel)
	}

	mc, err := m.loginToMattermost(ctx, onWsConnect)
	if err != nil {
		return nil, nil, err
	}

	// Helper closure to set bridge levels
	setBridgeLogLevels := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			ourlog.SetLevel(logrus.TraceLevel)
		case rc.Debug:
			ourlog.SetLevel(logrus.DebugLevel)
		default:
			ourlog.SetLevel(logrus.InfoLevel)
		}

		// Configure matterclient base logger
		mc.SetLogLevel("info")
		if rc.Mattermost.MatterclientLogLevel != "" {
			logger.Infof("enabling matterclient logging: level: %s", rc.Mattermost.MatterclientLogLevel)
			mc.SetLogLevel(strings.ToLower(rc.Mattermost.MatterclientLogLevel))
		}

		// Configure matterclient API logger
		mc.SetLogAPICalls("error")
		if rc.Profiling {
			if rc.Trace {
				logger.Infof("enabling matterclient API logging: level: trace")
				mc.SetLogAPICalls("trace")
			} else if rc.Debug {
				logger.Infof("enabling matterclient API logging: level: warn")
				mc.SetLogAPICalls("warn")
			}
		}
	}

	// Set initial log level
	setBridgeLogLevels(rc)

	// Register hook to update live on reloads
	cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
		setBridgeLogLevels(newRC)
	})

	m.mc = mc
	m.connected = true

	// Create a unique matterircd instance tag so we don't relay messages sent from it.
	charset := []byte("abcdefghijklmnopqrstuvwxyz")
	b := make([]byte, 8)
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	m.instanceTag = string(b)

	return m, mc, nil
}

func (m *Mattermost) loginToMattermost(ctx context.Context, onWsConnect func()) (*matterclient.Client, error) {
	rc := m.cfg.Current()

	matterclient.Matterircd = true

	mc := matterclient.New(m.credentials.Login, m.credentials.Pass, m.credentials.Team, m.credentials.Server, m.credentials.MFAToken)
	if rc.Mattermost.Insecure {
		mc.Credentials.NoTLS = true
	}

	mc.AntiIdle = !rc.Mattermost.DisableAutoView || rc.Mattermost.ForceAntiIdle
	mc.AntiIdleChan = rc.Mattermost.AntiIdleChannel
	mc.AntiIdleIntvl = rc.Mattermost.AntiIdleInterval
	mc.ForceSyncOnReconnect = rc.Mattermost.ForceSyncOnReconnect
	mc.UserAgent = config.UserAgent

	mc.CacheClearCutoff = rc.Mattermost.HeavySyncThreshold
	if mc.CacheClearCutoff == 0 {
		mc.CacheClearCutoff = 15 * time.Minute
	}

	mc.Ctx = ctx
	mc.OnWsConnect = onWsConnect

	mc.Timeout = rc.ClientTimeout
	if mc.Timeout == 0 {
		mc.Timeout = 10
	}

	if rc.Debug {
		mc.SetLogLevel("debug")
		mc.SetLogAPICalls("warn")
	}

	if rc.Trace {
		mc.SetLogLevel("trace")
		mc.SetLogAPICalls("trace")
	}

	mc.Credentials.SkipTLSVerify = rc.Mattermost.SkipTLSVerify

	logger.Infof("login as %s (team: %s) on %s", m.credentials.Login, m.credentials.Team, m.credentials.Server)

	if err := mc.Login(ctx); err != nil {
		logger.Error("login failed: ", err)
		return nil, err
	}

	logger.Info("login succeeded")

	m.mc = mc
	m.mc.WsQuit = false

	quitChan := make(chan struct{})
	m.quitChan = append(m.quitChan, quitChan)

	m.wsCtx, m.wsCancel = context.WithCancel(context.Background())

	// Start a pool of 10 concurrent workers
	for i := range 10 {
		// Extract the current prefix
		currentPrefix := ""
		if p, ok := logger.Data["prefix"].(string); ok {
			currentPrefix = p + ": "
		}

		// Create a new logger instance for this specific worker
		workerLogger := logger.WithField("prefix", fmt.Sprintf("%shandleWsMsg%d", currentPrefix, i))

		//nolint:contextcheck
		go m.handleWsMessage(m.wsCtx, quitChan, workerLogger)
	}

	return mc, nil
}

//nolint:cyclop,funlen,gocognit,gocyclo
func (m *Mattermost) handleWsMessage(ctx context.Context, quitChan chan struct{}, logger *logrus.Entry) {
	for {
		logger.Trace("in handleWsMessage")

		select {
		case <-quitChan:
			logger.Trace("exiting handleWsMessage")
			return
		case message := <-m.mc.MessageChan:
			eventType := message.Raw.EventType()

			if logger.Logger.IsLevelEnabled(logrus.DebugLevel) { //nolint:nestif
				userInfo := ""
				data := message.Raw.GetData()

				if userPtr, ok := data["user"].(*model.User); ok {
					if userPtr.Username != "" {
						userInfo = " [User: " + userPtr.Username + " (ID: " + userPtr.Id + ")]"
					}
				} else if userStr, ok := data["user"].(string); ok && userStr != "" {
					var summary *model.User
					_ = json.NewDecoder(strings.NewReader(userStr)).Decode(&summary)
					if summary.Username != "" {
						userInfo = " [User: " + summary.Username + " (ID: " + summary.Id + ")]"
					}
				} else if userID, ok := data["user_id"].(string); ok && userID != "" {
					userInfo = " [UserID: " + userID + "]"
				}

				switch eventType {
				case model.WebsocketEventTyping, model.WebsocketEventUserUpdated:
					logger.Tracef("WsRecvr%s: %#v", userInfo, message.Raw)
				case model.WebsocketEventMultipleChannelsViewed:
					logger.Tracef("WsRecvr%s: %#v", userInfo, message.Raw)
				case model.WebsocketEventPreferencesChanged, model.WebsocketEventSidebarCategoryUpdated:
					logger.Tracef("WsRecvr%s: %#v", userInfo, message.Raw)
				default:
					logger.Debugf("WsRcvr%s: %#v", userInfo, message.Raw)
				}

				if logger.Logger.IsLevelEnabled(logrus.TraceLevel) {
					logger.Tracef("%s %s", userInfo, spew.Sdump(message))
				}
			}

			switch eventType {
			case model.WebsocketEventConfigChanged:
				m.handleConfigChangedEvent(message.Raw, logger)
			case model.WebsocketEventPosted:
				m.handleWsActionPost(ctx, message.Raw, logger)
			case model.WebsocketEventPostEdited:
				m.handleWsActionPost(ctx, message.Raw, logger)
			case model.WebsocketEventPostDeleted:
				m.handleWsActionPost(ctx, message.Raw, logger)
			case model.WebsocketEventEphemeralMessage:
				m.handleWsActionPost(ctx, message.Raw, logger)
			case model.WebsocketEventUserRemoved:
				m.handleWsActionUserRemoved(ctx, message.Raw, logger)
			case model.WebsocketEventUserAdded:
				m.handleWsActionUserAdded(ctx, message.Raw, logger)
			case model.WebsocketEventChannelCreated:
				m.handleWsActionChannelCreated(message.Raw, logger)
			case model.WebsocketEventChannelDeleted:
				m.handleWsActionChannelDeleted(message.Raw, logger)
			case model.WebsocketEventChannelRestored:
				m.handleWsActionChannelCreated(message.Raw, logger)
			case model.WebsocketEventChannelUpdated:
				m.handleWsActionChannelUpdated(message.Raw, logger)
			case model.WebsocketEventUserUpdated:
				m.handleWsActionUserUpdated(message.Raw, logger)
			case model.WebsocketEventStatusChange:
				m.handleStatusChangeEvent(message.Raw, logger)
			case model.WebsocketEventReactionAdded, model.WebsocketEventReactionRemoved:
				m.handleReactionEvent(ctx, message.Raw, logger)
			case model.WebsocketEventTyping:
				m.handleTypingEvent(ctx, message.Raw, logger)
			}
		}
	}
}

func (m *Mattermost) Invite(ctx context.Context, channelID, username string) error {
	_, err := m.mc.AddChannelMember(ctx, channelID, username)
	return err
}

func (m *Mattermost) IsChannelMember(channelID string) bool {
	return m.mc.IsChannelMember(channelID)
}

func (m *Mattermost) Join(ctx context.Context, channelName string) (string, string, error) {
	teamID := ""

	sp := strings.Split(channelName, "/")
	if len(sp) > 1 {
		team, err := m.mc.GetTeamByName(ctx, sp[0])
		if team == nil {
			if err != nil {
				return "", "", fmt.Errorf("team not found: %v", err)
			}
			return "", "", fmt.Errorf("team not found")
		}

		teamID = team.Id
		channelName = sp[1]
	}

	if teamID == "" {
		teamID = m.mc.Team.ID
	}

	channelID := m.mc.GetChannelID(ctx, channelName, teamID)
	if channelID == "" {
		return "", "", fmt.Errorf("channel not found: check if the channel exists, if you are using the URL slug, or if it is private")
	}

	err := m.mc.JoinChannel(ctx, channelID)
	logger.Debugf("Join channel %s (id: %s), err: %v", channelName, channelID, err)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel: %v", err)
	}

	topic := m.mc.GetChannelHeader(ctx, channelID)

	return channelID, topic, nil
}

func (m *Mattermost) List(ctx context.Context) (map[string]string, error) {
	channelinfo := make(map[string]string)

	for _, channel := range append(m.mc.GetChannels(), m.mc.GetMoreChannels()...) {
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		if strings.Contains(channel.Name, "__") {
			continue
		}

		channelName := "#" + channel.Name
		// prefix channels outside of our team with team name
		if channel.TeamId != m.mc.Team.ID {
			channelName = m.mc.GetTeamName(ctx, channel.TeamId) + "/" + channel.Name
		}

		channelinfo[channelName] = strings.ReplaceAll(channel.Header, "\n", " | ")
	}

	return channelinfo, nil
}

func (m *Mattermost) Part(ctx context.Context, channelID string) error {
	return m.mc.RemoveUserFromChannel(ctx, channelID, m.mc.User.Id)
}

func (m *Mattermost) UpdateChannels(ctx context.Context) error {
	return m.mc.UpdateChannels(ctx)
}

func (m *Mattermost) Logout(ctx context.Context) error {
	if m.mc.WsClient != nil {
		_ = m.mc.Logout(ctx)
		logger.Info("logout succeeded")
	}

	m.eventChan <- &bridge.Event{
		Type: "logout",
		Data: &bridge.LogoutEvent{},
	}

	m.mc.WsQuit = true

	if m.wsCancel != nil {
		m.wsCancel()
	}

	for _, c := range m.quitChan {
		close(c)
	}

	m.quitChan = nil
	m.connected = false

	return nil
}

func (m *Mattermost) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return m.MsgUserThread(ctx, userID, "", text)
}

func (m *Mattermost) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	channelID, err := m.getDMChannelID(ctx, userID)
	if err != nil {
		return "", err
	}

	// build & send the message
	text = strings.ReplaceAll(text, "\r", "")

	return m.MsgChannelThread(ctx, channelID, parentID, text)
}

func (m *Mattermost) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return m.MsgChannelThread(ctx, channelID, "", text)
}

func (m *Mattermost) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	props := map[string]interface{}{
		"matterircd_" + m.mc.User.Id: m.instanceTag,
	}

	msgType := ""
	// CTCP ACTION (/me)
	if strings.HasPrefix(text, "\x01ACTION ") {
		text = strings.TrimPrefix(text, "\x01ACTION ")
		text = strings.TrimSuffix(text, "\x01")
		msgType = "me"
	}

	post := &model.Post{
		UserId:    m.mc.User.Id,
		ChannelId: channelID,
		Message:   text,
		RootId:    parentID,
		Type:      msgType,
	}

	post.SetProps(props)

	rp, err := m.mc.CreatePost(ctx, post)
	if err != nil {
		return "", err
	}

	return rp.Id, nil
}

func (m *Mattermost) ModifyPost(ctx context.Context, msgID, text string) error {
	if text == "" {
		return m.mc.DeleteMessage(ctx, msgID)
	}

	_, err := m.mc.PatchPost(ctx, msgID, &model.PostPatch{
		Message: &text,
	})

	return err
}

func (m *Mattermost) AddReaction(ctx context.Context, msgID, emoji string) error {
	logger.Debugf("adding reaction %#v, %#v", msgID, emoji)
	reaction := &model.Reaction{
		UserId:    m.mc.User.Id,
		PostId:    msgID,
		EmojiName: emoji,
		CreateAt:  0,
	}

	_, err := m.mc.SaveReaction(ctx, reaction)

	return err
}

func (m *Mattermost) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	logger.Debugf("removing reaction %#v, %#v", msgID, emoji)
	reaction := &model.Reaction{
		UserId:    m.mc.User.Id,
		PostId:    msgID,
		EmojiName: emoji,
		CreateAt:  0,
	}

	return m.mc.DeleteReaction(ctx, reaction)
}

func (m *Mattermost) Topic(ctx context.Context, channelID string) string {
	return m.mc.GetChannelHeader(ctx, channelID)
}

func (m *Mattermost) SetTopic(ctx context.Context, channelID, text string) error {
	logger.Debugf("Updating channel header/topic %#v, %#v", channelID, text)

	patch := &model.ChannelPatch{
		Header: &text,
	}

	_, err := m.mc.PatchChannel(ctx, channelID, patch)

	return err
}

func (m *Mattermost) StatusUser(ctx context.Context, userID string) (string, error) {
	return m.mc.GetStatus(ctx, userID), nil
}

func (m *Mattermost) StatusUsers(ctx context.Context) (map[string]string, error) {
	return m.mc.GetStatuses(ctx), nil
}

func (m *Mattermost) Protocol() string {
	return "mattermost"
}

func (m *Mattermost) Kick(ctx context.Context, channelID, username string) error {
	return m.mc.RemoveUserFromChannel(ctx, channelID, username)
}

func (m *Mattermost) SetStatus(ctx context.Context, status string) error {
	return m.mc.UpdateStatus(ctx, m.mc.User.Id, status)
}

func (m *Mattermost) Nick(ctx context.Context, name string) error {
	return m.mc.UpdateUserNick(ctx, name)
}

func (m *Mattermost) GetChannelName(ctx context.Context, channelID string) string {
	var name string

	if channelID == "" || strings.HasPrefix(channelID, "&") || channelID == m.mc.User.Nickname || channelID == m.mc.User.Username {
		return channelID
	}

	rc := m.cfg.Current()

	channelName := m.mc.GetChannelName(ctx, channelID)

	if channelName == "" {
		channel := m.mc.GetChannel(ctx, channelID)
		if channel == nil {
			logger.Warnf("Could not resolve missing channel name for %s", channelID)
		}
	}

	channelName = m.mc.GetChannelName(ctx, channelID)

	// return DM channels immediately
	if strings.Contains(channelName, "__") {
		return channelName
	}

	teamID := m.mc.GetTeamFromChannel(ctx, channelID)
	teamName := m.mc.GetTeamName(ctx, teamID)

	if channelName != "" {
		if (teamName != "" && teamID != m.mc.Team.ID) || rc.Mattermost.PrefixMainTeam {
			name = "#" + teamName + "/" + channelName
		}
		if teamID == m.mc.Team.ID && !rc.Mattermost.PrefixMainTeam {
			name = "#" + channelName
		}
		if teamID == "G" {
			name = "#" + channelName
		}
	} else {
		name = channelID
	}

	return name
}

func (m *Mattermost) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	mmUsers, err := m.mc.GetChannelUsers(ctx, channelID)
	if err != nil {
		return nil, err
	}

	users := make([]*bridge.UserInfo, 0, len(mmUsers))
	for _, mmuser := range mmUsers {
		users = append(users, m.createUser(mmuser))
	}

	return users, nil
}

func (m *Mattermost) GetUsers() []*bridge.UserInfo {
	mmusers := m.mc.GetUsers()
	users := make([]*bridge.UserInfo, 0, len(mmusers))

	for _, mmuser := range mmusers {
		users = append(users, m.createUser(mmuser))
	}

	return users
}

func (m *Mattermost) GetChannels() []*bridge.ChannelInfo {
	mmchannels := m.mc.GetChannels()
	channels := make([]*bridge.ChannelInfo, 0, len(mmchannels))

	chanMap := make(map[string]bool, len(mmchannels))

	for _, mmchannel := range mmchannels {
		// don't add the same channel twice
		// the same direct messages channels get listed for each team
		if chanMap[mmchannel.Id] {
			continue
		}

		channels = append(channels, &bridge.ChannelInfo{
			Name:       mmchannel.Name,
			ID:         mmchannel.Id,
			TeamID:     mmchannel.TeamId,
			DM:         mmchannel.IsGroupOrDirect(),
			Private:    !mmchannel.IsOpen(),
			LastPostAt: mmchannel.LastPostAt,
			DeleteAt:   mmchannel.DeleteAt,
		})

		chanMap[mmchannel.Id] = true
	}

	return channels
}

func (m *Mattermost) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	teamID := m.GetMe().TeamID

	// Map user input to Mattermost channel types, defaulting to Open/Public
	cType := model.ChannelTypeOpen

	switch strings.ToLower(channelType) {
	case "private", "p", string(model.ChannelTypePrivate):
		cType = model.ChannelTypePrivate
	}

	mmchan, err := m.mc.CreateChannel(ctx, teamID, channelName, cType)
	if err != nil {
		return nil, err
	}

	return &bridge.ChannelInfo{
		Name:       mmchan.Name,
		ID:         mmchan.Id,
		TeamID:     mmchan.TeamId,
		DM:         mmchan.Type == model.ChannelTypeDirect || mmchan.Type == model.ChannelTypeGroup,
		Private:    mmchan.Type == model.ChannelTypePrivate,
		LastPostAt: mmchan.LastPostAt,
		DeleteAt:   mmchan.DeleteAt,
	}, nil
}

func (m *Mattermost) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	if channelID == "" || strings.HasPrefix(channelID, "&") || channelID == m.mc.User.Nickname || channelID == m.mc.User.Username {
		return nil, errors.New("invalid channel id")
	}

	mmchannel := m.mc.GetChannel(ctx, channelID)
	if mmchannel == nil {
		return nil, errors.New("channel not found")
	}

	return &bridge.ChannelInfo{
		Name:       mmchannel.Name,
		ID:         mmchannel.Id,
		TeamID:     mmchannel.TeamId,
		DM:         mmchannel.IsGroupOrDirect(),
		Private:    !mmchannel.IsOpen(),
		LastPostAt: mmchannel.LastPostAt,
		DeleteAt:   mmchannel.DeleteAt,
	}, nil
}

func (m *Mattermost) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return m.createUser(m.mc.GetUser(ctx, userID))
}

func (m *Mattermost) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.User)
}

func (m *Mattermost) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return m.createUser(m.mc.GetUserByUsername(ctx, username))
}

func (m *Mattermost) createUser(mmuser *model.User) *bridge.UserInfo {
	if mmuser == nil {
		return &bridge.UserInfo{}
	}

	rc := m.cfg.Current()

	nick := mmuser.Username
	if rc.Mattermost.PreferNickname && isValidNick(mmuser.Nickname) {
		nick = mmuser.Nickname
	}

	me := false
	teamID := ""
	if m.mc.User != nil {
		me = mmuser.Id == m.mc.User.Id
		if me && m.mc.Team != nil {
			teamID = m.mc.Team.ID
		}
	}

	var realName string
	switch {
	case mmuser.FirstName != "" && mmuser.LastName != "":
		realName = mmuser.FirstName + " " + mmuser.LastName
	case mmuser.FirstName != "":
		realName = mmuser.FirstName
	case mmuser.Nickname != "":
		realName = mmuser.Nickname
	case mmuser.LastName != "":
		realName = mmuser.LastName
	default:
		realName = mmuser.Username
	}

	// We only care about mentions for ourselves
	var mentionKeys []string
	if me && m.mc.User != nil && m.mc.User.NotifyProps != nil {
		if keys := m.mc.User.NotifyProps["mention_keys"]; keys != "" {
			mentionKeys = strings.Split(keys, ",")
		}
	}

	info := &bridge.UserInfo{
		Nick:        nick,
		User:        mmuser.Id,
		Real:        realName,
		Host:        m.credentials.Server,
		Roles:       mmuser.Roles,
		Ghost:       true,
		Me:          me,
		TeamID:      teamID,
		Username:    mmuser.Username,
		FirstName:   mmuser.FirstName,
		LastName:    mmuser.LastName,
		MentionKeys: mentionKeys,
	}

	return info
}

//nolint:cyclop
func isValidNick(s string) bool {
	/* IRC RFC ([0] - see below) mentions a limit of 9 chars for
	 * IRC nicks, but modern clients allow more than that. Let's
	 * use a "sane" big value, the triple of the spec.
	 */
	if len(s) < 1 || len(s) > 27 {
		return false
	}

	/* According to IRC RFC [0], the allowed chars to have as nick
	 * are: ( letter / special-'-' ).*( letter / digit / special ),
	 * where:
	 * letter = [a-z / A-Z]; digit = [0-9];
	 * special = [';', '[', '\', ']', '^', '_', '`', '{', '|', '}', '-']
	 *
	 * ASCII codes (decimal) for the allowed chars:
	 * letter = [65-90,97-122]; digit = [48-57]
	 * special = [59, 91-96, 123-125, 45]
	 * [0] RFC 2812 (tools.ietf.org/html/rfc2812)
	 */

	if s[0] != 59 && (s[0] < 65 || s[0] > 125) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if s[i] != 45 && s[i] != 59 && (s[i] < 65 || s[i] > 125) {
			if s[i] < 48 || s[i] > 57 {
				return false
			}
		}
	}

	return true
}

const (
	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"
)

//nolint:funlen,gocyclo
func (m *Mattermost) wsActionPostSkip(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) bool {
	postData, ok := rmsg.GetData()["post"].(string)
	if !ok {
		return true
	}

	rc := m.cfg.Current()

	customEmoji := rc.Mattermost.Formatter.CustomEmoji
	disableEmoji := rc.Mattermost.Formatter.DisableEmoji
	disableMarkdown := rc.Mattermost.Formatter.DisableMarkdown
	useUnicode := rc.Mattermost.Formatter.Unicode
	blockquoteChar := blockquoteCharNonUnicode
	inlineCode := rc.Mattermost.Formatter.MarkdownInlineCode
	if useUnicode {
		blockquoteChar = blockquoteCharUnicode
	}
	shortenMsgLen := rc.Mattermost.ShortenRepliesTo

	var data model.Post
	if err := json.NewDecoder(strings.NewReader(postData)).Decode(&data); err != nil {
		logger.Errorf("failed to unmarshal post: %v", err)
		return true
	}

	if data.UserId != m.GetMe().User {
		return false
	}

	extraProps := data.GetProps()
	if tag, ok := extraProps["matterircd_"+m.GetMe().User]; !ok || tag != m.instanceTag {
		return false
	}

	if data.Type == model.PostTypeLeaveChannel || data.Type == model.PostTypeJoinChannel {
		logger.Tracef("our own join/leave message. not relaying %#v", data.Message)
		return true
	}

	// Show own edited / deleted
	if !rc.Mattermost.DisableShowOwnModified && (rmsg.EventType() == model.WebsocketEventPostEdited || rmsg.EventType() == model.WebsocketEventPostDeleted) {
		return false
	}

	channel := m.GetChannelName(ctx, data.ChannelId)

	if strings.Contains(channel, "__") {
		receiver := m.getDMUser(ctx, channel)
		channel = receiver.Username
	}

	msgID := data.Id
	var sbSuffix strings.Builder
	sbSuffix.Grow(shortenMsgLen + 32)

	if data.RootId != "" {
		msgID = data.RootId
		if !rc.Mattermost.HideReplies {
			cachedRoot, err := m.getCachedPostInfo(ctx, data.RootId, nil, shortenMsgLen, "@", useUnicode, logger)
			if err == nil {
				sbSuffix.WriteString(cachedRoot.ReplyMsg)
			}
		}
	}

	opts := utils.SummaryOpts{
		DisableEmoji:    disableEmoji,
		CustomEmoji:     customEmoji,
		DisableMarkdown: disableMarkdown,
		BlockquoteChar:  blockquoteChar,
		InlineCodeChar:  inlineCode,
		MaxLength:       90,
		UncountedPrefix: "@",
		Unicode:         useUnicode,
	}

	lastSentMsg := utils.FormatAndShortenSummary(data.Message, opts)
	cachedMsg := channel + ": " + lastSentMsg + sbSuffix.String()
	m.msgLastSentCache.Add(msgID, cachedMsg)

	logger.Tracef("message is sent from this matterircd instance, not relaying %#v", data.Message)

	return true
}

var markdownReplacer = strings.NewReplacer(
	"\n", " ",
	// Since we're combining multi lines into one, make code blocks single code/monospace
	"```", "`",
	"~~~", "`",
)

//nolint:funlen,unparam
func (m *Mattermost) getCachedPostInfo(ctx context.Context, postID string, preFetchedPost *model.Post, newLen int, uncounted string, unicode bool, logger *logrus.Entry) (CachedPost, error) {
	rc := m.cfg.Current()

	// Search and use cached reply if it exists.
	// None found, so we'll need to create one and save it for future uses.
	if cp, ok := m.msgParentCache.Get(postID); ok {
		logger.Tracef("Found saved reply for parent post %s, using:%s", postID, cp.ReplyMsg)
		return cp, nil
	}

	var post *model.Post
	var err error

	if preFetchedPost != nil {
		post = preFetchedPost
	} else {
		post, err = m.mc.GetPost(ctx, postID)
		if err != nil {
			return CachedPost{}, err
		}
	}

	msg := post.Message
	if msg == "" {
		// If we have message attachments and there is a fallback message, use it.
		if attachments := post.Attachments(); len(attachments) > 0 {
			if attachments[0].Fallback != "" {
				msg = attachments[0].Fallback
			} else if attachments[0].Text != "" {
				msg = attachments[0].Text
			}
		}
	}

	if !rc.Mattermost.Formatter.DisableMarkdown {
		msg = markdownReplacer.Replace(msg)
		blockquoteChar := blockquoteCharNonUnicode
		if unicode {
			blockquoteChar = blockquoteCharUnicode
		}
		msg = utils.Markdown2irc(msg, blockquoteChar, rc.Mattermost.Formatter.MarkdownInlineCode)
	} else {
		msg = strings.ReplaceAll(msg, "\n", " ")
	}

	if !rc.Mattermost.Formatter.DisableEmoji {
		msg = utils.EmojiReplaceAliases(msg, rc.Mattermost.Formatter.CustomEmoji)
	}


	opts := utils.SummaryOpts{
		DisableMarkdown: true,
		DisableEmoji:    true,
		MaxLength:       newLen,
		UncountedPrefix: uncounted,
		Unicode:         unicode,
	}

	parentUser := m.GetUser(ctx, post.UserId)
	parentMessage := utils.FormatAndShortenSummary(msg, opts)

	cp := CachedPost{
		RootID:   post.RootId,
		// Fast native string concatenation
		ReplyMsg: " (re @" + parentUser.Nick + ": " + parentMessage + ")",
	}

	logger.Tracef("Created cached post info for %s: %s", postID, cp.ReplyMsg)
	m.msgParentCache.Add(postID, cp)

	return cp, nil
}

var (
	validIRCNickRegExp    = regexp.MustCompile("^[a-zA-Z0-9_]*$")
	channelMentionsRegExp = regexp.MustCompile(`@(channel|all|here)\W`)
)

//nolint:funlen,gocognit,gocyclo,cyclop,forcetypeassert
func (m *Mattermost) handleWsActionPost(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionPost")
	wsData := rmsg.GetData()
	postData, ok := wsData["post"].(string)
	if !ok {
		return
	}

	var data model.Post
	if err := json.NewDecoder(strings.NewReader(postData)).Decode(&data); err != nil {
		logger.Errorf("failed to unmarshal postData: %v", err)
		return
	}
	extraProps := data.GetProps()

	logger.Tracef("receiving userid %s", data.UserId)
	if m.wsActionPostSkip(ctx, rmsg, logger) {
		return
	}

	rc := m.cfg.Current()

	useUnicode := rc.Mattermost.Formatter.Unicode

	var sbSuffix strings.Builder
	sbSuffix.Grow(rc.Mattermost.ShortenRepliesTo + 32)

	if !rc.Mattermost.HideReplies && data.RootId != "" {
		cachedRoot, err := m.getCachedPostInfo(ctx, data.RootId, nil, rc.Mattermost.ShortenRepliesTo, "@", useUnicode, logger)
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v", data) //nolint:govet
		} else {
			sbSuffix.WriteString(cachedRoot.ReplyMsg)
		}
	}

	// create new "ghost" user
	ghost := m.GetUser(ctx, data.UserId)
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == m.GetMe().User {
		ghost = m.GetMe()
	}

	if ghost == nil {
		ghost = &bridge.UserInfo{
			Nick: data.UserId,
		}
	}

	// check if we have a override_username (from webhooks) and use it
	overrideUsername, _ := extraProps["override_username"].(string)
	if overrideUsername != "" {
		logger.Debugf("found override username %s", overrideUsername)
		// only allow valid irc nicks
		if validIRCNickRegExp.MatchString(overrideUsername) {
			ghost.Nick = overrideUsername
			ghost.Me = false
		}
	}

	channelType := ""
	if t, ok := wsData["channel_type"].(string); ok {
		channelType = t
	}
	dmchannel := ""
	if t, ok := wsData["channel_name"].(string); ok {
		dmchannel = t
	}

	sendSystemDM := func(text string, eventType string) {
		d := &bridge.DirectMessageEvent{
			Text:      text,
			ChannelID: data.ChannelId,
			MessageID: data.Id,
			Event:     eventType,
		}

		userUpdated, _ := extraProps["username"].(string)

		if userUpdated == m.GetMe().Nick {
			d.Sender = ghost
			d.Receiver = m.getDMUser(ctx, dmchannel)
		} else {
			d.Sender = m.getDMUser(ctx, dmchannel)
			d.Receiver = ghost
		}

		if d.Sender == nil || d.Receiver == nil {
			logger.Errorf("dm: couldn't resolve sender or receiver: %#v", rmsg)
			return
		}

		event := &bridge.Event{
			Type: "direct_message",
			Data: d,
		}
		m.eventChan <- event
	}

	switch data.Type {
	case model.PostTypeJoinChannel, model.PostTypeLeaveChannel, model.PostTypeAddToChannel, model.PostTypeRemoveFromChannel:
		myUser := m.GetMe().User
		addedUserID, _ := data.GetProps()["addedUserId"].(string)
		if data.UserId != myUser && addedUserID != myUser {
			logger.Tracef("Skipping channel sync because user %s joined/left, not us.", data.UserId)
		} else if data.Type == model.PostTypeLeaveChannel {
			logger.Tracef("Left channel %s, skipping full channel sync", data.ChannelId)
		} else if _, err := m.GetChannel(ctx, data.ChannelId); err != nil {
			logger.Errorf("Failed to fetch new channel %s: %v", data.ChannelId, err)
		} else {
			logger.Debugf("Successfully synced single channel %s", data.ChannelId)
		}

		m.wsActionPostJoinLeave(ctx, &data, extraProps, logger)
		return

	case model.PostTypeHeaderChange:
		topic, ok := extraProps["new_header"].(string)
		if !ok {
			return
		}

		if channelType == "D" {
			sendSystemDM("\x01ACTION updated topic to: "+topic+"\x01", "dm_topic")
			return
		}

		event := &bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				Text:      topic,
				ChannelID: data.ChannelId,
				UserID:    data.UserId,
			},
		}

		m.eventChan <- event
		return

	default:
		if !strings.HasPrefix(data.Type, model.PostSystemMessagePrefix) {
			break
		}

		ghost = &bridge.UserInfo{Nick: systemUser}
		msgID := ""
		parentID := ""

		if channelType == "D" {
			sendSystemDM(data.Message, string(rmsg.EventType()))
			return
		}

		event := &bridge.Event{
			Type: "channel_message",
			Data: &bridge.ChannelMessageEvent{
				Text:        data.Message,
				ChannelID:   data.ChannelId,
				Sender:      ghost,
				ChannelType: channelType,
				MessageID:   msgID,
				ParentID:    parentID,
				Event:       string(rmsg.EventType()),
			},
		}

		m.eventChan <- event
		return
	}

	eventType := rmsg.EventType()
	// Check for edits/deletes to manage cache
	if eventType == model.WebsocketEventPostEdited || eventType == model.WebsocketEventPostDeleted {
		// check if we have an edited direct message (channels have __)
		name := m.GetChannelName(ctx, data.ChannelId)
		if strings.Contains(name, "__") {
			channelType = "D"
		}
		dmchannel = name

		// We need to remove it from the cache so that replies use the latest msg.
		m.msgParentCache.Remove(data.Id)
	}

	formattedMsg := m.formatMessage(ctx, &data, string(eventType), logger)

	switch {
	case channelType == "D":
		event := &bridge.Event{
			Type: "direct_message",
		}

		d := &bridge.DirectMessageEvent{
			Text:      formattedMsg,
			ChannelID: data.ChannelId,
			MessageID: data.Id,
			Event:     string(eventType),
			ParentID:  data.RootId,
			CreateAt:  data.CreateAt,
		}

		if ghost.Me {
			d.Sender = ghost
			d.Receiver = m.getDMUser(ctx, dmchannel)
		} else {
			d.Sender = m.getDMUser(ctx, dmchannel)
			d.Receiver = ghost
		}

		if d.Sender == nil || d.Receiver == nil {
			logger.Errorf("dm: couldn't resolve sender or receiver: %#v", rmsg)
			return
		}

		event.Data = d
		m.eventChan <- event

	default:
		messageType := ""
		if !rc.Mattermost.DisableDefaultMentions && channelMentionsRegExp.MatchString(formattedMsg) {
			messageType = "notice"
		}

		event := &bridge.Event{
			Type: "channel_message",
			Data: &bridge.ChannelMessageEvent{
				Text:        formattedMsg,
				ChannelID:   data.ChannelId,
				Sender:      ghost,
				MessageType: messageType,
				ChannelType: channelType,
				MessageID:   data.Id,
				Event:       string(eventType),
				ParentID:    data.RootId,
				CreateAt:    data.CreateAt,
			},
		}

		m.eventChan <- event
	}

	if len(data.FileIds) > 0 {
		m.handleFileEvent(ctx, channelType, ghost, &data, rmsg, logger)
	}

	logger.Debugf("user %s sent %#v", ghost.Nick, formattedMsg)
	logger.Tracef("%#v", data) //nolint:govet
}

func (m *Mattermost) handleFileEvent(ctx context.Context, channelType string, ghost *bridge.UserInfo, data *model.Post, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	event := &bridge.Event{
		Type: "file_event",
	}

	fileEvent := &bridge.FileEvent{
		Sender:      ghost,
		Receiver:    ghost,
		ChannelType: channelType,
		ChannelID:   data.ChannelId,
		MessageID:   data.Id,
		ParentID:    data.RootId,
	}

	event.Data = fileEvent

	if len(data.FileIds) > 0 {
		fileEvent.Files = m.GetFilesInfo(ctx, data.FileIds)
	}

	if len(fileEvent.Files) == 0 {
		logger.Debugf("handleFileEvent: user %s sent 0 files %#v", ghost.Nick, data.FileIds)
		return
	}

	switch {
	case channelType == "D":
		if ghost.Me {
			fileEvent.Sender = ghost
			fileEvent.Receiver = m.getDMUser(ctx, rmsg.GetData()["channel_name"])
		} else {
			fileEvent.Sender = m.getDMUser(ctx, rmsg.GetData()["channel_name"])
			fileEvent.Receiver = ghost
		}

		if fileEvent.Sender == nil || fileEvent.Receiver == nil {
			logger.Errorf("filedm: couldn't resolve sender or receiver: %#v", rmsg)
			return
		}

		m.eventChan <- event
	default:
		m.eventChan <- event
	}

	logger.Debugf("handleFileEvent: user %s sent %d files %#v", ghost.Nick, len(fileEvent.Files), data.FileIds)
}

func (m *Mattermost) wsActionPostJoinLeave(ctx context.Context, data *model.Post, extraProps map[string]interface{}, logger *logrus.Entry) {
	logger.Debugf("wsActionPostJoinLeave: extraProps: %#v", extraProps)
	switch data.Type {
	case "system_add_to_channel":
		if added, ok := extraProps["addedUsername"].(string); ok {
			if adder, ok := extraProps["username"].(string); ok {
				event := &bridge.Event{
					Type: "channel_add",
					Data: &bridge.ChannelAddEvent{
						Added: []*bridge.UserInfo{
							m.GetUserByUsername(ctx, added),
						},
						Adder:     m.GetUserByUsername(ctx, adder),
						ChannelID: data.ChannelId,
					},
				}

				m.eventChan <- event
			}
		}
	case "system_remove_from_channel":
		if removed, ok := extraProps["removedUsername"].(string); ok {
			event := &bridge.Event{
				Type: "channel_remove",
				Data: &bridge.ChannelRemoveEvent{
					Removed: []*bridge.UserInfo{
						m.GetUserByUsername(ctx, removed),
					},
					ChannelID: data.ChannelId,
				},
			}

			m.eventChan <- event
		}
	}
}

func (m *Mattermost) handleWsActionUserAdded(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionUserAdded")
	userID, ok := rmsg.GetData()["user_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				m.GetUser(ctx, userID),
			},
			Adder: &bridge.UserInfo{
				Nick: systemUser,
			},
			ChannelID: rmsg.GetBroadcast().ChannelId,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserRemoved(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	wsData := rmsg.GetData()
	userID, ok := wsData["user_id"].(string)
	if !ok {
		userID = rmsg.GetBroadcast().UserId
	}

	removerID, ok := wsData["remover_id"].(string)
	if !ok {
		logger.Error("not ok removerID", removerID)
		return
	}

	channelID, ok := wsData["channel_id"].(string)
	if !ok {
		channelID = rmsg.GetBroadcast().ChannelId
	}

	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover: m.GetUser(ctx, removerID),
			Removed: []*bridge.UserInfo{
				m.GetUser(ctx, userID),
			},
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserUpdated(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionUserUpdated")
	var info model.User

	err := Decode(rmsg.GetData()["user"], &info)
	if err != nil {
		logger.Error("decode", err)
		return
	}

	event := &bridge.Event{
		Type: "user_updated",
		Data: &bridge.UserUpdateEvent{
			User: m.createUser(&info),
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelCreated(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionChannelCreated")
	channelID, ok := rmsg.GetData()["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_create",
		Data: &bridge.ChannelCreateEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelDeleted(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionChannelDeleted")
	channelID, ok := rmsg.GetData()["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_delete",
		Data: &bridge.ChannelDeleteEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleStatusChangeEvent(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	var info model.Status

	err := Decode(rmsg.GetData(), &info)
	if err != nil {
		logger.Error("decode", err)

		return
	}

	event := &bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: info.UserId,
			Status: info.Status,
		},
	}

	m.eventChan <- event
}

//nolint:funlen
func (m *Mattermost) handleReactionEvent(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	reactionData, ok := rmsg.GetData()["reaction"].(string)
	if !ok {
		return
	}

	var reaction model.Reaction
	if err := json.NewDecoder(strings.NewReader(reactionData)).Decode(&reaction); err != nil {
		logger.Errorf("failed to unmarshal reactionData: %v", err)
		return
	}

	userID := m.GetUser(ctx, reaction.UserId)
	sender := userID
	receiver := m.GetMe()
	rc := m.cfg.Current()

	// Don't show our own reaction messages unless mattermost.showownreactions is enabled.
	if userID.Me && !rc.Mattermost.ShowOwnReactions {
		logger.Tracef("Not showing own reaction: %s: %s", rmsg.EventType(), reaction.EmojiName)
		return
	}

	var event *bridge.Event

	channelType := ""
	channelID := rmsg.GetBroadcast().ChannelId
	name := m.GetChannelName(ctx, channelID)
	if strings.Contains(name, "__") {
		channelType = "D"
		dmUser := m.getDMUser(ctx, name)
		if dmUser == nil {
			logger.Errorf("reaction: unable to resolve DM peer for channel %q", name)
			return
		}
		if userID.Me {
			receiver = m.getDMUser(ctx, name)
		} else {
			receiver = sender
			sender = m.getDMUser(ctx, name)
		}
	}

	var parentUser *bridge.UserInfo
	var sbSuffix strings.Builder
	sbSuffix.Grow(rc.Mattermost.ShortenRepliesTo + 32)

	parentID := reaction.PostId
	// Fetch the post being reacted to (hits cache if already seen)
	cachedPost, err := m.getCachedPostInfo(ctx, reaction.PostId, nil, rc.Mattermost.ShortenRepliesTo, "@", rc.Mattermost.Formatter.Unicode, logger)
	if err == nil {
		if cachedPost.RootID != "" {
			parentID = cachedPost.RootID
		}

		if !rc.Mattermost.HideReplies {
			sbSuffix.WriteString(cachedPost.ReplyMsg)
		}
	} else {
		logger.Errorf("Unable to get post info for reaction %#v: %v", reaction, err)
	}

	switch rmsg.EventType() {
	case model.WebsocketEventReactionAdded:
		event = &bridge.Event{
			Type: "reaction_add",
			Data: &bridge.ReactionAddEvent{
				ChannelID:   channelID,
				MessageID:   reaction.PostId,
				Receiver:    receiver,
				Sender:      sender,
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
				ParentUser:  parentUser,
				Message:     sbSuffix.String(),
				ParentID:    parentID,
			},
		}
	case model.WebsocketEventReactionRemoved:
		event = &bridge.Event{
			Type: "reaction_remove",
			Data: &bridge.ReactionRemoveEvent{
				ChannelID:   channelID,
				MessageID:   reaction.PostId,
				Receiver:    receiver,
				Sender:      sender,
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
				ParentUser:  parentUser,
				Message:     sbSuffix.String(),
				ParentID:    parentID,
			},
		}
	}

	m.eventChan <- event
}

func (m *Mattermost) GetTeamName(ctx context.Context, teamID string) string {
	return m.mc.GetTeamName(ctx, teamID)
}

func (m *Mattermost) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	x := m.mc.GetLastViewedAt(ctx, channelID)
	logger.Tracef("getLastViewedAt %s: %#v", channelID, x)

	return x
}

func (m *Mattermost) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	// TODO: Switch from using GetPostsSince() to GetPostsAfter()
	// TODO: which also does pagination rather than the 200 post limit.
	// TODO: Or maybe a combination with GetPostsSince() getting the
	// TODO: first post ID to use for GetPostsAfter().
	return m.postListToEvents(ctx, m.mc.GetPostsSince(ctx, channelID, since), "scrollback", since)
}

func (m *Mattermost) UpdateLastViewed(ctx context.Context, channelID string) {
	logger.Tracef("Updatelastviewed %s", channelID)
	err := m.mc.UpdateLastViewed(ctx, channelID)
	if err != nil {
		logger.Errorf("updateLastViewed failed: %s", err)
	}
}

func (m *Mattermost) UpdateLastViewedUser(ctx context.Context, userID string) error {
	channelID, err := m.getDMChannelID(ctx, userID)
	if err != nil {
		return err
	}

	return m.mc.UpdateLastViewed(ctx, channelID)
}

func (m *Mattermost) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return m.postListToEvents(ctx, m.mc.SearchPosts(ctx, search), "search", 0)
}

func (m *Mattermost) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	mcFiles := m.mc.GetFilesInfo(ctx, fileIDs)
	files := make([]*bridge.File, 0, len(mcFiles))

	for _, f := range mcFiles {
		files = append(files, &bridge.File{
			Name: f.Name,
			Size: f.Size,
			URL:  f.URL,
		})
	}

	return files
}

func (m *Mattermost) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return m.postListToEvents(ctx, m.mc.GetPosts(ctx, channelID, limit), "scrollback", 0)
}

func (m *Mattermost) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	return m.postListToEvents(ctx, m.mc.GetPostThread(ctx, postID), "details", 0)
}

func (m *Mattermost) GetChannelID(ctx context.Context, name, teamID string) string {
	// Try standard public/private channel lookup
	id := m.mc.GetChannelID(ctx, name, teamID)
	if id != "" {
		return id
	}

	// Fallback: Check if 'name' is actually a username for a DM replay.
	// We need the Mattermost UserID to construct the DM channel string.
	user := m.GetUserByUsername(ctx, name)
	if user != nil && user.User != "" {
		targetID := user.User
		myID := m.mc.User.Id
		dmName := m.mc.GetDMChannelName(myID, targetID)

		return m.mc.GetChannelID(ctx, dmName, teamID)
	}

	return ""
}

func (m *Mattermost) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	users, err := m.mc.SearchUsers(ctx, &model.UserSearch{Term: query})
	if err != nil {
		return nil, err
	}

	brusers := make([]*bridge.UserInfo, 0, len(users))

	for _, u := range users {
		brusers = append(brusers, m.createUser(u))
	}

	return brusers, nil
}

func Decode(input interface{}, output interface{}) error {
	config := &mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   output,
		TagName:  "json",
	}

	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}

	return decoder.Decode(input)
}

func (m *Mattermost) handleWsActionChannelUpdated(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleWsActionChannelUpdated")

	channelStr, ok := rmsg.GetData()["channel"].(string)
	if !ok || channelStr == "" {
		return
	}

	var updatedChannel model.Channel

	err := json.NewDecoder(strings.NewReader(channelStr)).Decode(&updatedChannel)
	if err != nil {
		logger.Errorf("Failed to decode updated channel: %v", err)
		return
	}

	m.eventChan <- &bridge.Event{
		Type: "channel_update",
		Data: &bridge.ChannelUpdateEvent{
			ChannelID:   updatedChannel.Id,
			Name:        updatedChannel.Name,
			DisplayName: updatedChannel.DisplayName,
		},
	}
}

func (m *Mattermost) handleConfigChangedEvent(rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleConfigChangedEvent")

	data := rmsg.GetData()

	configMap, ok := data["config"].(map[string]any)
	if !ok {
		return
	}

	bannerEnabled := false
	if enableStr, ok := configMap["EnableBanner"].(string); ok {
		bannerEnabled = (enableStr == "true")
	} else if enableBool, ok := configMap["EnableBanner"].(bool); ok {
		bannerEnabled = enableBool
	}

	bannerText := ""
	if text, ok := configMap["BannerText"].(string); ok {
		bannerText = text
	}

	// Trigger only if enabled and actually changed
	if bannerEnabled && bannerText != "" {
		if m.lastBannerText != bannerText {
			m.lastBannerText = bannerText

			m.eventChan <- &bridge.Event{
				Type: "banner_change",
				Data: &bridge.BannerChangeEvent{
					Text: bannerText,
				},
			}
		}
	} else {
		// Banner disabled or cleared
		m.lastBannerText = ""
	}
}

func (m *Mattermost) handleTypingEvent(ctx context.Context, rmsg *model.WebSocketEvent, logger *logrus.Entry) {
	logger.Trace("in handleTypingEvent")
	// Extract the user ID from the event data or broadcast info
	typingUserID, ok := rmsg.GetData()["user_id"].(string)
	if !ok || typingUserID == "" {
		// Fallback depending on MM version
		typingUserID = rmsg.GetBroadcast().UserId
	}

	if typingUserID == "" {
		return
	}

	channelID := rmsg.GetBroadcast().ChannelId
	if channelID == "" {
		return
	}

	// Resolve the user
	userID := m.GetUser(ctx, typingUserID)
	if userID == nil {
		return
	}

	// Ignore our own typing events
	if userID.Me {
		return
	}

	sender := userID
	receiver := m.GetMe()
	channelType := ""
	name := m.GetChannelName(ctx, channelID)

	// Check if this is a Direct Message
	if strings.Contains(name, "__") {
		channelType = "D"

		dmUser := m.getDMUser(ctx, name)
		if dmUser == nil {
			logger.Tracef("typing: unable to resolve DM peer for channel %q", name)
			return
		}

		if userID.Me {
			receiver = m.getDMUser(ctx, name)
		} else {
			receiver = sender
			sender = m.getDMUser(ctx, name)
		}
	}

	// Send it down the internal event channel
	m.eventChan <- &bridge.Event{
		Type: "typing",
		Data: &bridge.TypingEvent{
			ChannelID:   channelID,
			ChannelType: channelType,
			Receiver:    receiver,
			Sender:      sender,
		},
	}
}

func (m *Mattermost) getDMChannelID(ctx context.Context, userID string) (string, error) {
	if channelID, ok := m.dmChannelCache.Get(userID); ok {
		return channelID, nil
	}

	dchannel, err := m.mc.CreateDirectChannel(ctx, m.mc.User.Id, userID)
	if err != nil {
		return "", err
	}

	m.dmChannelCache.Add(userID, dchannel.Id)

	return dchannel.Id, nil
}

func (m *Mattermost) getDMUser(ctx context.Context, name interface{}) *bridge.UserInfo {
	if channel, ok := name.(string); ok {
		channelmembers := strings.Split(channel, "__")
		if len(channelmembers) != 2 {
			logger.Errorf("not a DM message, incorrect channelID: %s", channel)
			return nil
		}

		// ourself
		if channelmembers[0] == channelmembers[1] {
			return m.createUser(m.mc.User)
		}

		otheruser := m.GetUser(ctx, channelmembers[1])
		if channelmembers[1] == m.mc.User.Id {
			otheruser = m.GetUser(ctx, channelmembers[0])
		}

		return otheruser
	}

	return nil
}

//nolint:funlen,gocyclo
func (m *Mattermost) formatMessage(ctx context.Context, data *model.Post, eventType string, logger *logrus.Entry) string {
	rc := m.cfg.Current()
	useUnicode := rc.Mattermost.Formatter.Unicode

	var sbSuffix strings.Builder
	sbSuffix.Grow(rc.Mattermost.ShortenRepliesTo + 32)

	if !rc.Mattermost.HideReplies && data.RootId != "" {
		cachedRoot, err := m.getCachedPostInfo(ctx, data.RootId, nil, rc.Mattermost.ShortenRepliesTo, "@", useUnicode, logger)
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v", data)
		} else {
			sbSuffix.WriteString(cachedRoot.ReplyMsg)
		}
	}

	if eventType == string(model.WebsocketEventPostEdited) {
		sbSuffix.WriteString(" \x1d(edited)\x1d")
	} else if eventType == string(model.WebsocketEventPostDeleted) {
		sbSuffix.WriteString(" \x1d(deleted)\x1d")
	}

	msg := data.Message
	// Manually slice off trailing newlines
	for len(msg) > 0 && msg[len(msg)-1] == '\n' {
		msg = msg[:len(msg)-1]
	}

	var sbMsg strings.Builder
	sbMsg.Grow(len(msg) + sbSuffix.Len() + 64)
	attachments := data.Attachments()

	switch {
	case data.Type == "me":
		sbMsg.WriteString("\x01ACTION ")
		sbMsg.WriteString(msg)
		sbMsg.WriteString(sbSuffix.String())
		sbMsg.WriteString("\x01")
	case data.Type == "slack_attachment":
		if len(msg) > 0 {
			sbMsg.WriteString(msg)
		}
		useFallback := len(msg) == 0
		// https://docs.slack.dev/tools/node-slack-sdk/reference/web-api/interfaces/MessageAttachment/
		m.parseMessageAttachments(&sbMsg, attachments, useFallback, msg)
	case data.Type == "custom_matterpoll":
		pollMsg := parseMatterpollToMsg(attachments, useUnicode)
		sbMsg.WriteString(msg)
		sbMsg.WriteString(pollMsg)
	case len(attachments) > 0:
		if len(msg) > 0 {
			sbMsg.WriteString(msg)
		}
		useFallback := len(msg) == 0
		// https://developers.mattermost.com/integrate/reference/message-attachments/
		m.parseMessageAttachments(&sbMsg, attachments, useFallback, msg)
	default:
		sbMsg.WriteString(msg)
	}

	// Attachments and raw messages often leave trailing newlines in the builder.
	// Strip them here so suffixes (like thread replies) stay on the same line.
	finalBody := sbMsg.String()
	for len(finalBody) > 0 && (finalBody[len(finalBody)-1] == '\n' || finalBody[len(finalBody)-1] == '\r') {
		finalBody = finalBody[:len(finalBody)-1]
	}

	// Reset the builder and write the cleaned string back
	sbMsg.Reset()
	sbMsg.WriteString(finalBody)

	if sbSuffix.Len() > 0 && data.Type != "me" {
		sbMsg.WriteString(sbSuffix.String())
	}

	// We can't use data.GetPreviewPost() due to a bug so use our own
	previewText, previewUserID, previewChannelID, replyCount, lastReplyAt := extractPreviewData(data.Metadata)
	if !(previewText == "" && previewUserID == "" && previewChannelID == "") {
		nick := previewUserID
		if user := m.GetUser(ctx, previewUserID); user != nil {
			nick = user.Nick
		}

		channel := m.GetChannelName(ctx, previewChannelID)
		if strings.Contains(channel, "__") {
			channel = ""
		}

		m.parsePreviewPost(&sbMsg, nick, channel, previewText, replyCount, lastReplyAt)
	}

	return sbMsg.String()
}

const (
	messageAttachmentCharNonUnicode = "|"
	// right one quarter block (U+1FB87)
	messageAttachmentCharUnicode     = "🮇"
	messageAttachmentSpaceNonUnicode = " "
	// non-breaking space / no-break space / nbsp (U+00A0)
	messageAttachmentSpaceUnicode = " "
)

func parseMatterpollToMsg(attachments []*model.SlackAttachment, unicode bool) string {
	msg := ""
	prefixChar := messageAttachmentCharNonUnicode
	spaceChar := messageAttachmentSpaceNonUnicode
	if unicode {
		prefixChar = messageAttachmentCharUnicode
		spaceChar = messageAttachmentSpaceUnicode
	}
	for _, attachment := range attachments {
		prefix := "\x02\x0302" + prefixChar + "\x0f" + spaceChar

		if attachment.AuthorName != "" {
			msg += prefix + "@" + attachment.AuthorName + "\n"
		}
		if attachment.Title != "" {
			msg += prefix + "\x02" + attachment.Title + "\x02\n"
		}

		for _, action := range attachment.Actions {
			if strings.HasPrefix(action.Id, "vote") {
				msg += prefix + "•" + spaceChar + action.Name + "\n"
			}
		}

		if attachment.Text != "" {
			lines := strings.Split(attachment.Text, "\n")
			for _, text := range lines {
				msg += prefix + text + "\n"
			}
		}
		if !strings.HasPrefix(attachment.Text, "This poll has ended.") {
			msg += prefix + "\n"
			msg += prefix + "\x1dUse the web UI to cast your vote\x1d"
		}

		for _, field := range attachment.Fields {
			msg += prefix + "•" + spaceChar + field.Title + ":" + spaceChar
			lines := strings.Split(fmt.Sprintf("%s", field.Value), "\n")
			newPrefix := ""
			for _, text := range lines {
				msg += newPrefix + text + "\n"
				newPrefix = prefix
			}
		}
	}

	return strings.TrimRight(msg, "\n")
}

const blockQuoteCharDefault = utils.BlockQuoteCharDefault

//nolint:funlen,gocognit,gocyclo
func (m *Mattermost) parseMessageAttachments(b *strings.Builder, attachments []*model.SlackAttachment, useFallback bool, rootMsg string) {
	// If the main message builder already has content, add a newline before our preview
	if b.Len() > 0 {
		b.WriteByte('\n')
	}

	rc := m.cfg.Current()

	useUnicode := rc.Mattermost.Formatter.Unicode
	syntaxHighlighting := rc.Mattermost.Formatter.SyntaxHighlighting
	codeBlockPrefix := rc.Mattermost.Formatter.CodeBlockPrefix
	codeBlockSeparator := rc.Mattermost.Formatter.CodeBlockSeparator
	disableMarkdown := rc.Mattermost.Formatter.DisableMarkdown
	disableEmoji := rc.Mattermost.Formatter.DisableEmoji
	customEmoji := rc.Mattermost.Formatter.CustomEmoji
	enableIRCHexColors := rc.Mattermost.Formatter.EnableIRCHexColors
	messageAttachmentShortFieldMaxLineLength := rc.Mattermost.MessageAttachmentShortFieldMaxLineLength

	if messageAttachmentShortFieldMaxLineLength == 0 {
		messageAttachmentShortFieldMaxLineLength = 100
	}

	prefixChar := messageAttachmentCharNonUnicode
	spaceChar := messageAttachmentSpaceNonUnicode
	blockquoteChar := blockquoteCharNonUnicode
	inlineCode := rc.Mattermost.Formatter.MarkdownInlineCode
	if useUnicode {
		prefixChar = messageAttachmentCharUnicode
		spaceChar = messageAttachmentSpaceUnicode
		blockquoteChar = blockquoteCharUnicode
		// Downgrade heavy vertical to light as we're using heavy already
		if strings.ContainsAny(codeBlockPrefix, "┃🮇▎") {
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "┃", "│", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "🮇", "▕", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "▎", "▏", 1)
		}
	}

	for _, attachment := range attachments {
		prefix := "\x02" + prefixChar + "\x0f" + spaceChar
		switch {
		// https://docs.slack.dev/tools/node-slack-sdk/reference/web-api/interfaces/MessageAttachment/#color
		case attachment.Color == "danger":
			prefix = "\x0304" + prefixChar + "\x0f" + spaceChar
		case attachment.Color == "good":
			prefix = "\x0303" + prefixChar + "\x0f" + spaceChar
		case attachment.Color == "warning":
			prefix = "\x0308" + prefixChar + "\x0f" + spaceChar
		case strings.HasPrefix(attachment.Color, "#"):
			hex := strings.TrimPrefix(attachment.Color, "#")
			if enableIRCHexColors {
				// https://modern.ircdocs.horse/formatting.html#hex-color
				// Make sure the hex is uppercase for best compatibility
				prefix = "\x02\x04" + strings.ToUpper(hex) + prefixChar + "\x0f" + spaceChar
			} else {
				// Use the closest standard/extended \x03 code
				closestMircCode := utils.FindClosestIRCColor(hex)
				prefix = "\x02\x03" + closestMircCode + prefixChar + "\x0f" + spaceChar
			}
		}

		var fallbackText, printedFallback string

		if useFallback {
			fallbackText, _, _ = strings.Cut(attachment.Fallback, "\n")
			fallbackText = strings.TrimSuffix(fallbackText, "\r")

			// In some cases, no fallback message present
			// e.g. https://github.com/fluxcd/notification-controller/pull/1322
			if fallbackText == "" {
				fallbackText, _, _ = strings.Cut(attachment.Text, "\n")
				fallbackText = strings.TrimSuffix(fallbackText, "\r")
				if attachment.AuthorName != "" {
					fallbackText = attachment.AuthorName + ":" + spaceChar + fallbackText
				}
			}

			attFallbackStr := attachment.Fallback
			for len(attFallbackStr) > 0 && attFallbackStr[len(attFallbackStr)-1] == '\n' {
				attFallbackStr = attFallbackStr[:len(attFallbackStr)-1]
			}

			// Only write to buffer if it's not a substring duplicate of the main message
			isFallbackDup := attFallbackStr != "" && strings.Contains(rootMsg, attFallbackStr)
			if !isFallbackDup {
				outFallback := fallbackText
				if !disableMarkdown {
					outFallback = utils.Markdown2irc(outFallback, blockquoteChar, inlineCode)
				}

				if !disableEmoji {
					outFallback = utils.EmojiReplaceAliases(outFallback, rc.Mattermost.Formatter.CustomEmoji)
				}

				b.WriteString(outFallback)
				b.WriteByte('\n')

				// Mark that we printed the fallback so we can deduplicate against it!
				printedFallback = attFallbackStr
			}
		}

		// Zero-allocation closure to check both rootMsg and the fallback text we just printed
		isDup := func(s string) bool {
			if s == "" {
				return false
			}

			return strings.Contains(rootMsg, s) || (printedFallback != "" && strings.Contains(printedFallback, s))
		}

		// Deduplicate Author block
		isAuthorDup := attachment.AuthorName != "" &&
			isDup(attachment.AuthorName) &&
			(attachment.AuthorLink == "" || isDup(attachment.AuthorLink))

		if attachment.AuthorName != "" && !isAuthorDup {
			b.WriteString(prefix)
			authorName := attachment.AuthorName
			if !disableEmoji {
				authorName = utils.EmojiReplaceAliases(authorName, rc.Mattermost.Formatter.CustomEmoji)
			}
			b.WriteString(authorName)
			if attachment.AuthorLink != "" {
				b.WriteString(spaceChar)
				b.WriteString("(")
				b.WriteString(attachment.AuthorLink)
				b.WriteString(")")
			}
			b.WriteByte('\n')
		}

		// Deduplicate Title block
		isTitleDup := attachment.Title != "" &&
			isDup(attachment.Title) &&
			(attachment.TitleLink == "" || isDup(attachment.TitleLink))

		if attachment.Title != "" && !isTitleDup {
			b.WriteString(prefix)
			b.WriteByte('\x02')
			title := attachment.Title
			if !disableEmoji {
				title = utils.EmojiReplaceAliases(title, rc.Mattermost.Formatter.CustomEmoji)
			}
			b.WriteString(title)
			b.WriteByte('\x02')
			if attachment.TitleLink != "" {
				b.WriteString(" (\x1d")
				b.WriteString(attachment.TitleLink)
				b.WriteByte('\x1d')
				b.WriteByte(')')
			}
			b.WriteByte('\n')
		}

		attTextStr := attachment.Text
		for len(attTextStr) > 0 && attTextStr[len(attTextStr)-1] == '\n' {
			attTextStr = attTextStr[:len(attTextStr)-1]
		}

		// Deduplicate Text block
		isTextDup := attTextStr != "" && isDup(attTextStr)
		opts := utils.ProcessMessageOpts{
			DisableEmoji:       disableEmoji,
			CustomEmoji:        customEmoji,
			DisableMarkdown:    disableMarkdown,
			SyntaxHighlighting: syntaxHighlighting,
			CodeBlockPrefix:    codeBlockPrefix,
			CodeBlockSeparator: codeBlockSeparator,
			BlockquoteChar:     blockquoteChar,
			InlineCodeChar:     inlineCode,
		}

		if attachment.Text != "" && !isTextDup {
			utils.ProcessMessageText(attachment.Text, opts, func(line string) {
				b.WriteString(prefix)
				b.WriteString(line)
				b.WriteByte('\n')
			})
		}

		if attachment.ImageURL != "" {
			b.WriteString(prefix)
			b.WriteString(attachment.ImageURL)
			b.WriteByte('\n')
		}

		if len(attachment.Fields) > 0 {
			maxLineLength := messageAttachmentShortFieldMaxLineLength
			m.formatAttachmentFields(b, attachment.Fields, prefix, prefixChar, useFallback, fallbackText, opts, maxLineLength)
		}
	}
}

//nolint:funlen,gocognit,gocyclo
func (m *Mattermost) formatAttachmentFields(b *strings.Builder, fields []*model.SlackAttachmentField, prefix string, prefixChar string, useFallback bool, fallbackText string, opts utils.ProcessMessageOpts, maxLineLength int) {
	const gutter = 2

	var candidates [3]struct {
		title  string
		rawVal string
		lines  []string
	}

	for i := 0; i < len(fields); {
		groupSize := 1

		// Check if this field and the next field are both flagged as "short"
		if fields[i].Short {
			if i+2 < len(fields) && fields[i+1].Short && fields[i+2].Short {
				groupSize = 3
			} else if i+1 < len(fields) && fields[i+1].Short {
				groupSize = 2
			}
		}

		var targetColWidth int

		if groupSize > 1 { //nolint:nestif
			// FAST PASS: Extract raw values for length evaluation
			for j := range groupSize {
				field := fields[i+j]

				if s, ok := field.Value.(string); ok {
					candidates[j].rawVal = s
				} else {
					candidates[j].rawVal = fmt.Sprintf("%v", field.Value)
				}
			}

			// FAST PASS: Calculate column fitting without excessive nesting
			if maxLineLength > 0 {
				for groupSize > 1 {
					totalGutters := (groupSize - 1) * gutter
					targetColWidth = (maxLineLength - totalGutters) / groupSize

					if fieldsFitColWidth(fields[i:i+groupSize], candidates[:groupSize], targetColWidth) {
						break
					}

					groupSize--
				}
			}
		}

		if groupSize == 1 {
			m.formatSingleAttachmentField(b, fields[i], prefix, prefixChar, useFallback, fallbackText, opts)
			i++

			continue
		}

		// EXPENSIVE PASS: Process only confirmed columns
		for j := range groupSize {
			field := fields[i+j]

			title := utils.FormatMarkdownAndEmoji(field.Title, true, opts.DisableEmoji, "", "", opts.CustomEmoji)
			candidates[j].title = title
			valStr := candidates[j].rawVal

			valStr = strings.TrimPrefix(valStr, "\n")

			if !opts.DisableMarkdown && strings.HasPrefix(valStr, blockQuoteCharDefault) {
				valStr = strings.Replace(valStr, blockQuoteCharDefault, prefixChar, 1)
			}

			lines := strings.Split(valStr, "\n")

			for k, line := range lines {
				lines[k] = utils.FormatMarkdownAndEmoji(
					line,
					opts.DisableMarkdown,
					opts.DisableEmoji,
					opts.BlockquoteChar,
					opts.InlineCodeChar,
					opts.CustomEmoji,
				)
			}

			candidates[j].lines = lines
		}

		// Print Field Titles
		b.WriteString(prefix)
		b.WriteByte('\x02')

		for j := range groupSize {
			b.WriteString(candidates[j].title)

			if j < groupSize-1 {
				padWidth := targetColWidth - len(candidates[j].title) + gutter

				for range padWidth {
					b.WriteByte(' ')
				}
			}
		}

		b.WriteByte('\x02')
		b.WriteByte('\n')

		// Calculate max line depth
		maxLines := 0

		for j := range groupSize {
			if len(candidates[j].lines) > maxLines {
				maxLines = len(candidates[j].lines)
			}
		}

		// Print Field Values row by row
		for lineIdx := range maxLines {
			b.WriteString(prefix)

			for j := range groupSize {
				v := ""

				if lineIdx < len(candidates[j].lines) {
					v = candidates[j].lines[lineIdx]
				}

				b.WriteString(v)

				if j < groupSize-1 {
					padWidth := targetColWidth - len(v) + gutter

					for range padWidth {
						b.WriteByte(' ')
					}
				}
			}

			b.WriteByte('\n')
		}

		i += groupSize
	}
}

// fieldsFitColWidth checks if all candidate fields and their multiline values fit within targetColWidth.
func fieldsFitColWidth(
	fields []*model.SlackAttachmentField,
	candidates []struct {
		title  string
		rawVal string
		lines  []string
	},
	targetColWidth int,
) bool {
	for j, field := range fields {
		if len(field.Title) > targetColWidth {
			return false
		}

		start := 0
		valStr := candidates[j].rawVal

		for k := 0; k <= len(valStr); k++ {
			if k == len(valStr) || valStr[k] == '\n' {
				if (k - start) > targetColWidth {
					return false
				}

				start = k + 1
			}
		}
	}

	return true
}

func (m *Mattermost) formatSingleAttachmentField(b *strings.Builder, field *model.SlackAttachmentField, prefix string, prefixChar string, useFallback bool, fallbackText string, opts utils.ProcessMessageOpts) {
	var valStr string

	if s, ok := field.Value.(string); ok {
		valStr = s
	} else {
		valStr = fmt.Sprintf("%v", field.Value)
	}

	valStr = strings.TrimPrefix(valStr, "\n")

	if !opts.DisableMarkdown && strings.HasPrefix(valStr, blockQuoteCharDefault) {
		valStr = strings.Replace(valStr, blockQuoteCharDefault, prefixChar, 1)
	}

	if field.Title != "" {
		b.WriteString(prefix)
		b.WriteByte('\x02')

		title := utils.FormatMarkdownAndEmoji(field.Title, true, opts.DisableEmoji, "", "", opts.CustomEmoji)

		b.WriteString(title)
		b.WriteByte('\x02')
		b.WriteByte('\n')
	}

	utils.ProcessMessageText(valStr, opts, func(line string) {
		// Ignore duplicate content when field value is the same as fallback
		isDuplicate := useFallback && fallbackText != "" && line == fallbackText

		if !isDuplicate {
			b.WriteString(prefix)
			b.WriteString(line)
			b.WriteByte('\n')
		}
	})
}

// XXX: Bug in Mattermost itself and PostEmbed Data interface{}
//
//nolint:gocyclo
func extractPreviewData(metadata *model.PostMetadata) (string, string, string, int64, int64) {
	if metadata == nil {
		return "", "", "", 0, 0
	}

	for _, embed := range metadata.Embeds {
		if embed.Type != "permalink" || embed.Data == nil {
			continue
		}

		dataMap, ok := embed.Data.(map[string]any)
		if !ok {
			continue
		}

		postMap, ok := dataMap["post"].(map[string]any)
		if !ok {
			continue
		}

		var (
			msg, userID, channelID  string
			replyCount, lastReplyAt int64
		)

		if val, ok := postMap["message"].(string); ok {
			msg = val
		}

		if val, ok := postMap["user_id"].(string); ok {
			userID = val
		}

		if val, ok := postMap["channel_id"].(string); ok {
			channelID = val
		}

		if val, ok := postMap["reply_count"].(float64); ok {
			replyCount = int64(val)
		}

		if val, ok := postMap["last_reply_at"].(float64); ok {
			lastReplyAt = int64(val)
		}

		// Fall back to update_at (or create_at) if last_reply_at is 0
		if lastReplyAt == 0 {
			if val, ok := postMap["update_at"].(float64); ok && int64(val) > 0 {
				lastReplyAt = int64(val)
			} else if val, ok := postMap["create_at"].(float64); ok {
				lastReplyAt = int64(val)
			}
		}

		return msg, userID, channelID, replyCount, lastReplyAt
	}

	return "", "", "", 0, 0
}

//nolint:funlen
func (m *Mattermost) parsePreviewPost(b *strings.Builder, user string, channel string, text string, replyCount int64, lastReplyAt int64) {
	// If the main message builder already has content, add a newline before our preview
	if b.Len() > 0 {
		b.WriteByte('\n')
	}

	rc := m.cfg.Current()

	useUnicode := rc.Mattermost.Formatter.Unicode
	syntaxHighlighting := rc.Mattermost.Formatter.SyntaxHighlighting
	codeBlockPrefix := rc.Mattermost.Formatter.CodeBlockPrefix
	codeBlockSeparator := rc.Mattermost.Formatter.CodeBlockSeparator
	disableMarkdown := rc.Mattermost.Formatter.DisableMarkdown
	disableEmoji := rc.Mattermost.Formatter.DisableEmoji
	customEmoji := rc.Mattermost.Formatter.CustomEmoji

	prefixChar := messageAttachmentCharNonUnicode
	spaceChar := messageAttachmentSpaceNonUnicode
	blockquoteChar := blockquoteCharNonUnicode
	inlineCode := rc.Mattermost.Formatter.MarkdownInlineCode

	if useUnicode {
		prefixChar = messageAttachmentCharUnicode
		spaceChar = messageAttachmentSpaceUnicode
		blockquoteChar = blockquoteCharUnicode
		// Downgrade heavy vertical to light as we're using heavy already
		if strings.ContainsAny(codeBlockPrefix, "┃🮇▎") {
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "┃", "│", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "🮇", "▕", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "▎", "▏", 1)
		}
	}

	b.Grow(len(text) + 128)
	prefix := prefixChar + spaceChar

	b.WriteString(prefix)
	b.WriteString("\x02@")
	b.WriteString(user)
	b.WriteString("\x02 wrote")
	if channel != "" {
		b.WriteString(" in \x1d")
		b.WriteString(channel)
		b.WriteByte('\x1d')
	}

	// Append reply metadata if replies exist
	if replyCount > 0 {
		b.WriteString(" (")
		b.WriteString(strconv.FormatInt(replyCount, 10))

		if replyCount == 1 {
			b.WriteString(" reply")
		} else {
			b.WriteString(" replies")
		}

		b.WriteString(", last at ")
		// Mattermost timestamps are in Unix milliseconds
		t := time.Unix(lastReplyAt/1000, 0)
		b.WriteString(t.Format("2006-01-02 15:04:05"))
		b.WriteByte(')')
	}

	b.WriteString(":\n")

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockquoteChar,
		InlineCodeChar:     inlineCode,
	}

	first := true

	utils.ProcessMessageText(text, opts, func(line string) {
		// Mirror original behavior: write newline before subsequent lines,
		// avoiding a trailing newline at the very end.
		if !first {
			b.WriteByte('\n')
		}

		first = false
		b.WriteString(prefix)
		b.WriteString(line)
	})
}


func (m *Mattermost) postListToEvents(ctx context.Context, postlist interface{}, eventType string, since int64) []*bridge.Event {
	if postlist == nil {
		return nil
	}

	mmPostList, ok := postlist.(*model.PostList)
	if !ok || mmPostList == nil || len(mmPostList.Order) == 0 {
		return []*bridge.Event{}
	}

	events := make([]*bridge.Event, 0, len(mmPostList.Order))

	// Enforce strict newest-first sorting before the backwards loop processes it.
	// This fixes the Mattermost API quirk where thread root posts are placed at index 0.
	sort.Slice(mmPostList.Order, func(i, j int) bool {
		pI, okI := mmPostList.Posts[mmPostList.Order[i]]
		pJ, okJ := mmPostList.Posts[mmPostList.Order[j]]
		if !okI || !okJ {
			return false
		}
		// Sort descending (newest first) so the backwards loop below prints oldest first
		return pI.CreateAt > pJ.CreateAt
	})

	// traverse the order in reverse
	for i := len(mmPostList.Order) - 1; i >= 0; i-- {
		p := mmPostList.Posts[mmPostList.Order[i]]

		// GetPostsSince will return older messages with reaction
		// changes since LastViewedAt. This will be confusing as
		// the user will think it's a duplicate, or a post out of
		// order. Plus, we don't show reaction changes when
		// relaying messages/logs so let's skip these.
		//
		// See https://github.com/mattermost/mattermost/issues/13846 and https://github.com/anneschuth/claude-threads/pull/66
		if eventType == "replay" && (p.DeleteAt > p.CreateAt || p.CreateAt < since) {
			continue
		} else if eventType != "details" && eventType != "replay" && p.DeleteAt > p.CreateAt {
			continue
		}

		if ev := m.postToEvent(ctx, p, eventType); ev != nil {
			events = append(events, ev)
		}
	}
	return events
}

//nolint:funlen
func (m *Mattermost) postToEvent(ctx context.Context, p *model.Post, eventType string) *bridge.Event {
	channelName := m.GetChannelName(ctx, p.ChannelId)
	isDM := strings.Contains(channelName, "__")

	props := p.GetProps()
	botname, override := props["override_username"].(string)
	user := m.GetUser(ctx, p.UserId)
	if override {
		user.Nick = botname
	}

	switch p.Type {
	case model.PostTypeAddToTeam, model.PostTypeJoinChannel, model.PostTypeAddToChannel:
		targetUser := user
		if addedUserID, ok := props["addedUserId"].(string); ok {
			targetUser = m.GetUser(ctx, addedUserID)
		}

		return &bridge.Event{
			Type: "channel_add",
			Data: &bridge.ChannelAddEvent{
				Added:     []*bridge.UserInfo{targetUser},
				ChannelID: p.ChannelId,
				Text:      p.Message,
				CreateAt:  p.CreateAt,
			},
		}

	case model.PostTypeRemoveFromTeam, model.PostTypeLeaveChannel, model.PostTypeRemoveFromChannel:
		targetUser := user
		if removedUserID, ok := props["removedUserId"].(string); ok {
			targetUser = m.GetUser(ctx, removedUserID)
		}

		return &bridge.Event{
			Type: "channel_remove",
			Data: &bridge.ChannelRemoveEvent{
				Removed:   []*bridge.UserInfo{targetUser},
				ChannelID: p.ChannelId,
				Text:      p.Message,
				CreateAt:  p.CreateAt,
			},
		}

	case model.PostTypeHeaderChange:
		newHeader, _ := props["new_header"].(string)

		return &bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				ChannelID: p.ChannelId,
				Text:      newHeader,
				UserID:    p.UserId,
			},
		}

	default:
		var files []*bridge.File
		if len(p.FileIds) > 0 {
			files = m.GetFilesInfo(ctx, p.FileIds)
		}

		formattedMsg := m.formatMessage(ctx, p, eventType, logger)

		sender := user
		msgID := p.Id
		parentID := p.RootId

		if strings.HasPrefix(p.Type, model.PostSystemMessagePrefix) {
			sender = &bridge.UserInfo{Nick: systemUser}
			msgID = ""
			parentID = ""
		}

		if isDM {
			return &bridge.Event{
				Type: "direct_message",
				Data: &bridge.DirectMessageEvent{
					Text:      formattedMsg,
					ChannelID: p.ChannelId,
					Sender:    sender,
					Receiver:  m.getDMUser(ctx, channelName),
					Files:     files,
					MessageID: msgID,
					ParentID:  parentID,
					Event:     eventType,
					CreateAt:  p.CreateAt,
				},
			}
		}
		return &bridge.Event{
			Type: "channel_message",
			Data: &bridge.ChannelMessageEvent{
				Text:      formattedMsg,
				ChannelID: p.ChannelId,
				Sender:    sender,
				Files:     files,
				MessageID: msgID,
				ParentID:  parentID,
				Event:     eventType,
				CreateAt:  p.CreateAt,
			},
		}
	}
}
