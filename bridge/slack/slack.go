package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/davecgh/go-spew/spew"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/slackclient"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

const (
	slackThreadRootCacheSize = 4096
	slackParentCacheSize     = 2048
	slackLastSentCacheSize   = 128
	slackLastSentSummaryLen  = 80

	subTypeMessageChanged = "message_changed"
	subTypeMessageDeleted = "message_deleted"
	eventChannelTopic     = "channel_topic"
	eventChannelPurpose   = "channel_purpose"
)

type slackCachedPost struct {
	rootTS  string
	sender  *bridge.UserInfo
	message string
}

type Slack struct {
	sync.RWMutex

	sc                 *slackclient.Client
	connected          bool
	credentials        bridge.Credentials
	eventChan          chan *bridge.Event
	onConnect          func()
	userByNick         map[string]string
	msgThreadRootCache *lru.Cache[string, string]
	msgParentCache     *lru.Cache[string, slackCachedPost]
	msgLastSentCache   *lru.Cache[string, string]

	cfg *config.Config
}

const systemUser = "system"

var logger *logrus.Entry

func New(cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, error) {
	threadRootCache, _ := lru.New[string, string](slackThreadRootCacheSize)
	parentCache, _ := lru.New[string, slackCachedPost](slackParentCacheSize)
	lastSentCache, _ := lru.New[string, string](slackLastSentCacheSize)

	s := &Slack{
		credentials:        cred,
		eventChan:          eventChan,
		onConnect:          onConnect,
		userByNick:         make(map[string]string),
		msgThreadRootCache: threadRootCache,
		msgParentCache:     parentCache,
		msgLastSentCache:   lastSentCache,
		cfg:                cfg,
	}

	rc := cfg.Current()

	ourlog := logrus.New()
	ourlog.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	logger = ourlog.WithFields(logrus.Fields{"prefix": "bridge/slack"})
	if rc.Debug {
		ourlog.SetLevel(logrus.DebugLevel)
	}

	if rc.Trace {
		ourlog.SetLevel(logrus.TraceLevel)
	}

	sc, err := s.loginToSlack()
	if err != nil {
		return nil, err
	}
	// Set initial log level
	configureBridgeLogging(ourlog, sc, rc)

	// Register hook to update live on reloads
	cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
		configureBridgeLogging(ourlog, sc, newRC)
	})

	s.sc = sc
	s.sc.InitUsers()

	return s, nil
}

func configureBridgeLogging(ourlog *logrus.Logger, sc *slackclient.Client, rc *config.RuntimeConfig) {
	switch {
	case rc.Trace:
		ourlog.SetLevel(logrus.TraceLevel)
	case rc.Debug:
		ourlog.SetLevel(logrus.DebugLevel)
	default:
		ourlog.SetLevel(logrus.InfoLevel)
	}

	sc.SetLogLevel("info")

	if rc.Slack.SlackclientLogLevel != "" {
		logger.Infof("enabling slackclient logging: level: %s", rc.Slack.SlackclientLogLevel)
		sc.SetLogLevel(strings.ToLower(rc.Slack.SlackclientLogLevel))
	}

	sc.SetLogAPICalls("error")

	if rc.Profiling {
		if rc.Trace {
			logger.Infof("enabling slackclient API logging: level: trace")
			sc.SetLogAPICalls("trace")
		} else if rc.Debug {
			logger.Infof("enabling slackclient API logging: level: warn")
			sc.SetLogAPICalls("warn")
		}
	}
}

func (s *Slack) Invite(ctx context.Context, channelID, username string) error {
	return s.sc.Invite(channelID, username)
}

func (s *Slack) IsChannelMember(channelID string) bool {
	return s.sc.IsChannelMember(channelID)
}

func (s *Slack) Join(ctx context.Context, channelName string) (string, string, error) {
	return s.sc.JoinChannel(channelName)
}

func (s *Slack) List(ctx context.Context) (map[string]string, error) {
	channelinfo := make(map[string]string)

	conversations, _ := s.sc.GetConversations()
	for _, channel := range conversations {
		channelinfo["#"+channel.Name] = strings.ReplaceAll(channel.Topic.Value, "\n", " | ")
	}

	return channelinfo, nil
}

func (s *Slack) Part(ctx context.Context, channelID string) error {
	return s.sc.LeaveChannel(channelID)
}

func (s *Slack) UpdateChannels(ctx context.Context) error {
	return nil
}

func (s *Slack) Logout(ctx context.Context) error {
	err := s.sc.Logout()
	if err != nil {
		return err
	}

	// The event loop may never have started, or it ended already when the user disconnected
	// (ctx is canceled then), so don't wait for it forever.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	select {
	case s.eventChan <- &bridge.Event{Type: "logout", Data: &bridge.LogoutEvent{}}:
	case <-ctx.Done():
	case <-timer.C:
	}

	s.connected = false

	return nil
}

func slackActionText(text string) string {
	// CTCP ACTION (/me)
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		text = strings.TrimSuffix(after, "\x01")
		return "*" + text + "*"
	}

	return text
}

func (s *Slack) MsgUser(ctx context.Context, username, text string) (string, error) {
	return s.MsgUserThread(ctx, username, "", text)
}

func (s *Slack) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return s.MsgChannelThread(ctx, channelID, "", text)
}

func (s *Slack) Topic(ctx context.Context, channelID string) string {
	return s.sc.Topic(channelID)
}

func (s *Slack) SetTopic(ctx context.Context, channelID, text string) error {
	return s.sc.SetTopic(channelID, text)
}

func (s *Slack) HasCustomStatus(userID string) bool {
	return false
}

func (s *Slack) IsOnline(status string) bool {
	return status == "active"
}

func (s *Slack) StatusUser(ctx context.Context, name string) (string, error) {
	return "", nil
}

func (s *Slack) StatusUsers(ctx context.Context) (map[string]string, error) {
	return make(map[string]string), nil
}

func (s *Slack) Protocol() string {
	return "slack"
}

func (s *Slack) Kick(ctx context.Context, channelID, username string) error {
	return s.sc.Kick(channelID, username)
}

func (s *Slack) SetStatus(ctx context.Context, status string) error {
	return s.sc.SetStatus(status)
}

func (s *Slack) Nick(ctx context.Context, name string) error {
	return nil
}

func (s *Slack) GetChannelName(ctx context.Context, channelID string) string {
	return s.sc.GetChannelName(channelID)
}

func (s *Slack) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	susers, err := s.sc.GetChannelUsers(channelID)
	if err != nil {
		return nil, err
	}

	users := make([]*bridge.UserInfo, 0, len(susers)+1)
	for _, suser := range susers {
		users = append(users, s.createUser(suser))
	}

	users = append(users, s.GetMe())

	return users, nil
}

func (s *Slack) GetDMChannelID(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}

	if strings.HasPrefix(strings.ToUpper(userID), "D") {
		return strings.ToUpper(userID)
	}

	chID, err := s.sc.OpenDMChannel(ctx, userID)
	if err != nil {
		logger.Debugf("GetDMChannelID %s: %s", userID, err)
		return ""
	}

	return chID
}

func (s *Slack) GetDMChannelName(userID1 string, userID2 string) string {
	return ""
}

func (s *Slack) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetDMUserIDs(channelName string) (string, string, bool) {
	return "", "", false
}

func (s *Slack) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	return "", false
}

func (s *Slack) GetUsers() []*bridge.UserInfo {
	susers := s.sc.GetUsers()
	users := make([]*bridge.UserInfo, 0, len(susers))

	for i := range susers {
		users = append(users, s.createUser(&susers[i]))
	}

	return users
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	teamID := s.sc.TeamID()

	for _, mmchannel := range s.sc.GetChannels() {
		channels = append(channels, &bridge.ChannelInfo{
			Name:    mmchannel.Name,
			ID:      mmchannel.ID,
			TeamID:  teamID,
			DM:      mmchannel.IsIM || mmchannel.IsMpIM,
			Private: !mmchannel.IsOpen,
		})
	}

	return channels
}

func (s *Slack) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	return nil, errors.New("not implemented yet")
}

func (s *Slack) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	channels := s.GetChannels()
	for _, channel := range channels {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (s *Slack) GetChannelCount() int {
	return 0
}

func (s *Slack) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return s.createUser(s.sc.GetUser(userID))
}

func (s *Slack) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	return []string{}, nil
}

func (s *Slack) GetUserCount() int {
	return 0
}

func (s *Slack) GetMe() *bridge.UserInfo {
	return s.createUser(s.sc.GetMe())
}

func (s *Slack) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetTeamName(ctx context.Context, teamID string) string {
	return s.sc.TeamName()
}

func (s *Slack) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (s *Slack) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) IsDMChannelName(channelName string) bool {
	return false
}

func (s *Slack) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) UpdateLastViewed(ctx context.Context, channelID string) {
}

func (s *Slack) UpdateLastViewedUser(ctx context.Context, userID string) error {
	return nil
}

func (s *Slack) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	return []*bridge.File{}
}

func (s *Slack) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (s *Slack) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) cacheThreadRoot(channelID, msgTS, rootTS string) {
	if s.msgThreadRootCache == nil || channelID == "" || msgTS == "" || rootTS == "" {
		return
	}

	if id := encodeSlackID(channelID, msgTS); id != "" {
		s.msgThreadRootCache.Add(id, rootTS)
	}
}

func (s *Slack) cachePost(channelID, ts, rootTS string, sender *bridge.UserInfo, message string) {
	if channelID == "" || ts == "" {
		return
	}

	if rootTS == "" {
		rootTS = ts
	}

	s.cacheThreadRoot(channelID, ts, rootTS)

	if s.msgParentCache == nil {
		return
	}

	if id := encodeSlackID(channelID, ts); id != "" {
		s.msgParentCache.Add(id, slackCachedPost{
			rootTS:  rootTS,
			sender:  sender,
			message: message,
		})
	}
}

func (s *Slack) msgAuthorGhost(ctx context.Context, m *slack.Msg) *bridge.UserInfo {
	if m == nil {
		return &bridge.UserInfo{Nick: systemUser}
	}

	switch {
	case m.User == "USLACKBOT" || (m.SubType == "bot_message" && m.BotID == "B01" && m.Username == ""):
		return &bridge.UserInfo{Nick: systemUser}
	case m.User != "":
		return s.GetUser(ctx, m.User)
	case m.BotID != "" || m.Username != "":
		nick := firstNonEmpty(m.Username, "bot")
		return &bridge.UserInfo{Nick: nick, User: m.BotID, Ghost: true}
	default:
		return &bridge.UserInfo{Nick: systemUser}
	}
}

func (s *Slack) getCachedPostOnly(channelID, ts string) (slackCachedPost, bool) {
	if channelID == "" || ts == "" || s.msgParentCache == nil {
		return slackCachedPost{}, false
	}

	id := encodeSlackID(channelID, ts)
	if id == "" {
		return slackCachedPost{}, false
	}

	return s.msgParentCache.Get(id)
}

func (s *Slack) evictCachedPost(channelID, ts string) {
	if channelID == "" || ts == "" {
		return
	}

	id := encodeSlackID(channelID, ts)
	if id == "" {
		return
	}

	if s.msgParentCache != nil {
		s.msgParentCache.Remove(id)
	}

	if s.msgThreadRootCache != nil {
		s.msgThreadRootCache.Remove(id)
	}
}

func (s *Slack) getCachedPost(ctx context.Context, channelID, ts string) (slackCachedPost, bool) {
	if cp, ok := s.getCachedPostOnly(channelID, ts); ok {
		return cp, true
	}

	if s.sc == nil {
		return slackCachedPost{}, false
	}

	msgs, err := s.sc.GetConversationReplies(ctx, channelID, ts)
	if err != nil || len(msgs) == 0 {
		return slackCachedPost{}, false
	}

	m := msgs[0]
	rootTS := m.Timestamp

	if m.ThreadTimestamp != "" {
		rootTS = m.ThreadTimestamp
	}

	cleanMsg := ""
	if rawText, ok := formatSlackMsg(&m.Msg); ok {
		cleanMsg = s.cleanupMessage(rawText)
	}

	ghost := s.msgAuthorGhost(ctx, &m.Msg)
	s.cachePost(channelID, ts, rootTS, ghost, cleanMsg)

	return slackCachedPost{
		rootTS:  rootTS,
		sender:  ghost,
		message: cleanMsg,
	}, true
}

func slackSummaryOpts(rc *config.RuntimeConfig) utils.SummaryOpts {
	return utils.SummaryOpts{
		DisableEmoji:    true,
		DisableMarkdown: true,
		MaxLength:       rc.Slack.ShortenRepliesTo,
		UncountedPrefix: "@",
		Unicode:         rc.Slack.Formatter.Unicode,
	}
}

func formatReplySummary(rc *config.RuntimeConfig, cp slackCachedPost) string {
	if rc == nil || rc.Slack.HideReplies {
		return ""
	}

	summary := utils.FormatAndShortenSummary(cp.message, slackSummaryOpts(rc))
	if cp.sender == nil || cp.sender.Nick == "" {
		return " (re: " + summary + ")"
	}

	return " (re @" + cp.sender.Nick + ": " + summary + ")"
}

func (s *Slack) formatReplySuffix(ctx context.Context, channelID, ts string) string {
	if s.cfg == nil || channelID == "" || ts == "" {
		return ""
	}

	rc := s.cfg.Current()
	if rc.Slack.HideReplies {
		return ""
	}

	cp, ok := s.getCachedPost(ctx, channelID, ts)
	if !ok {
		return ""
	}

	return formatReplySummary(rc, cp)
}

func (s *Slack) resolveThreadRoot(ctx context.Context, channelID, ts string) string {
	if channelID == "" || ts == "" {
		return ts
	}

	if s.msgThreadRootCache != nil {
		if id := encodeSlackID(channelID, ts); id != "" {
			if rootTS, ok := s.msgThreadRootCache.Get(id); ok && rootTS != "" {
				return rootTS
			}
		}
	}

	msgs, err := s.sc.GetConversationReplies(ctx, channelID, ts)
	if err != nil || len(msgs) == 0 {
		return ts
	}

	rootTS := ts
	if msgs[0].ThreadTimestamp != "" {
		rootTS = msgs[0].ThreadTimestamp
	}

	s.cacheThreadRoot(channelID, ts, rootTS)

	return rootTS
}

func (s *Slack) slackMsgToEvent(ctx context.Context, channelID string, m slack.Msg) *bridge.Event {
	ghost := s.msgAuthorGhost(ctx, &m)

	rawText, ok := formatSlackMsg(&m)
	if !ok {
		return nil
	}

	text := s.cleanupMessage(rawText)
	msgID := encodeSlackID(channelID, m.Timestamp)

	parentID := ""
	rootTS := m.Timestamp

	if m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
		parentID = encodeSlackID(channelID, m.ThreadTimestamp)
		rootTS = m.ThreadTimestamp
	}

	s.cachePost(channelID, m.Timestamp, rootTS, ghost, text)

	createAt := slackTSMillis(m.Timestamp)

	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)

		return &bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: channelID,
				Sender:    ghost,
				Receiver:  receiver,
				MessageID: msgID,
				ParentID:  parentID,
				CreateAt:  createAt,
			},
		}
	}

	return &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:      text,
			ChannelID: channelID,
			Sender:    ghost,
			MessageID: msgID,
			ParentID:  parentID,
			CreateAt:  createAt,
		},
	}
}

func (s *Slack) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	channelID, ts, ok := decodeSlackID(postID)
	if !ok {
		return []*bridge.Event{}
	}

	rootTS := s.resolveThreadRoot(ctx, channelID, ts)

	msgs, err := s.sc.GetConversationReplies(ctx, channelID, rootTS)
	if err != nil {
		logger.Errorf("slack: fetching thread %s: %s", postID, err)
		return []*bridge.Event{}
	}

	events := make([]*bridge.Event, 0, len(msgs))
	for _, m := range msgs {
		if ev := s.slackMsgToEvent(ctx, channelID, m.Msg); ev != nil {
			events = append(events, ev)
		}
	}

	return events
}

func (s *Slack) GetChannelID(ctx context.Context, name, teamID string) string {
	return ""
}

func (s *Slack) allowedLogin() error {
	rc := s.cfg.Current()
	// we only know which server we are connecting to when we actually are connected.
	// disconnect if we're not allowed
	if len(rc.Slack.Bridge.Restrict) > 0 {
		ok := slices.Contains(rc.Slack.Bridge.Restrict, s.sc.TeamDomain())
		if !ok {
			_ = s.sc.Disconnect()
			return errors.New("Not allowed to connect to " + s.sc.TeamDomain() + " slack")
		}
	}
	// we only know which user we are when we actually are connected.
	// disconnect if we're not allowed
	if len(rc.Slack.DenyUsers) > 0 {
		ok := slices.Contains(rc.Slack.DenyUsers, s.sc.CurrentUserName())
		if ok {
			_ = s.sc.Disconnect()
			return errors.New("not allowed to connect")
		}
	}

	return nil
}

func (s *Slack) loginToSlack() (*slackclient.Client, error) {
	rc := s.cfg.Current()

	sc := slackclient.New(slackclient.Credentials{
		Login: s.credentials.Login,
		Team:  s.credentials.Team,
		Pass:  s.credentials.Pass,
		Token: s.credentials.Token,
	})

	if rc.Debug {
		sc.SetLogLevel("debug")
		sc.SetLogAPICalls("warn")
	}

	if rc.Trace {
		sc.SetLogLevel("trace")
		sc.SetLogAPICalls("trace")
	}

	err := sc.Login()
	if err != nil {
		return nil, err
	}

	s.sc = sc

	err = s.allowedLogin()
	if err != nil {
		return nil, err
	}

	go s.handleSlack(context.TODO())
	go s.onConnect()

	s.connected = true

	return s.sc, nil
}

//nolint:gocyclo
func (s *Slack) handleSlack(ctx context.Context) {
	for msg := range s.sc.IncomingEvents() {
		logger.Tracef("handleSlack %s", spew.Sdump(msg))

		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			switch ev.SubType {
			case "group_join", "channel_join", "group_leave", "channel_leave":
			default:
				s.handleSlackActionPost(ctx, ev)
			}
		case *slack.MemberLeftChannelEvent:
			s.handleMemberLeftChannel(ctx, ev)
		case *slack.MemberJoinedChannelEvent:
			s.handleMemberJoinedChannel(ctx, ev)
		case *slack.DisconnectedEvent:
			logger.Debug("disconnected event received, we should reconnect now..")
		case *slack.ReactionAddedEvent:
			logger.Debugf("ReactionAdded msg %#v", ev)
			s.handleSlackReaction(ctx, ev.User, ev.Item.Channel, ev.Item.Timestamp, ev.Reaction, true)
		case *slack.ReactionRemovedEvent:
			logger.Debugf("ReactionRemoved msg %#v", ev)
			s.handleSlackReaction(ctx, ev.User, ev.Item.Channel, ev.Item.Timestamp, ev.Reaction, false)
		case *slack.StarAddedEvent:
			logger.Debugf("StarAdded msg %#v", ev)

			if ev.Item.Message == nil {
				continue
			}

			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message starred")
		case *slack.StarRemovedEvent:
			logger.Debugf("StarRemoved msg %#v", ev)

			if ev.Item.Message == nil {
				continue
			}

			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message unstarred")
		case *slack.PinAddedEvent:
			logger.Debugf("PinAdded msg %#v", ev)

			if ev.Item.Message == nil {
				continue
			}

			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message pinned")
		case *slack.PinRemovedEvent:
			logger.Debugf("PinRemoved msg %#v", ev)

			if ev.Item.Message == nil {
				continue
			}

			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message unpinned")
		}
	}
}

func (s *Slack) resolveDMReceiver(ghost *bridge.UserInfo, channelID string) *bridge.UserInfo {
	receiver := ghost
	if ghost != nil && ghost.Me {
		members, _ := s.sc.GetUsersInConversation(channelID)
		for _, member := range members {
			if member == s.GetMe().User {
				continue
			}

			ghostuser, _ := s.sc.GetUserInfo(member)
			receiver = s.createUser(ghostuser)
		}
	}

	return receiver
}

func (s *Slack) cachedParentID(channelID, ts string) string {
	if s.msgThreadRootCache == nil || channelID == "" || ts == "" {
		return ""
	}

	msgID := encodeSlackID(channelID, ts)
	if msgID == "" {
		return ""
	}

	if rootTS, ok := s.msgThreadRootCache.Get(msgID); ok && rootTS != "" && rootTS != ts {
		return encodeSlackID(channelID, rootTS)
	}

	return ""
}

func (s *Slack) handleSlackReaction(ctx context.Context, userID, channelID, ts, reaction string, added bool) {
	if channelID == "" || ts == "" {
		return
	}

	suser, err := s.sc.GetUserInfo(userID)
	if err != nil {
		return
	}

	ghost := s.createUser(suser)
	msgID := encodeSlackID(channelID, ts)

	parentID := s.cachedParentID(channelID, ts)
	if cp, ok := s.getCachedPost(ctx, channelID, ts); ok && cp.rootTS != "" && cp.rootTS != ts {
		parentID = encodeSlackID(channelID, cp.rootTS)
	}

	replyMsg := s.formatReplySuffix(ctx, channelID, ts)

	channelType := ""
	receiver := ghost

	if strings.HasPrefix(channelID, "D") {
		channelType = "D"
		receiver = s.resolveDMReceiver(ghost, channelID)
	}

	if added {
		s.eventChan <- &bridge.Event{
			Type: "reaction_add",
			Data: &bridge.ReactionAddEvent{
				ChannelID:   channelID,
				MessageID:   msgID,
				ParentID:    parentID,
				Message:     replyMsg,
				Receiver:    receiver,
				Sender:      ghost,
				Reaction:    reaction,
				ChannelType: channelType,
			},
		}

		return
	}

	s.eventChan <- &bridge.Event{
		Type: "reaction_remove",
		Data: &bridge.ReactionRemoveEvent{
			ChannelID:   channelID,
			MessageID:   msgID,
			ParentID:    parentID,
			Message:     replyMsg,
			Receiver:    receiver,
			Sender:      ghost,
			Reaction:    reaction,
			ChannelType: channelType,
		},
	}
}

func (s *Slack) handleItemMisc(ctx context.Context, userID, channelID string, itemMsg *slack.Message, action string) {
	msgID := encodeSlackID(channelID, itemMsg.Timestamp)

	parentID := s.cachedParentID(channelID, itemMsg.Timestamp)
	if parentID == "" && itemMsg.ThreadTimestamp != "" && itemMsg.ThreadTimestamp != itemMsg.Timestamp {
		parentID = encodeSlackID(channelID, itemMsg.ThreadTimestamp)
	}

	msg := s.cleanupMessage(action + " (" + itemMsg.Text + ")")
	if itemMsg.ThreadTimestamp != "" && itemMsg.ThreadTimestamp != itemMsg.Timestamp {
		msg += s.formatReplySuffix(ctx, channelID, itemMsg.ThreadTimestamp)
	}

	s.handleActionMisc(userID, channelID, msgID, parentID, msg, slackTSMillis(itemMsg.Timestamp))
}

func (s *Slack) handleActionMisc(userID, channelID, msgID, parentID, msg string, createAt int64) {
	suser, err := s.sc.GetUserInfo(userID)
	if err != nil {
		return
	}

	// create new "ghost" user
	ghost := s.createUser(suser)

	// direct message
	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)
		s.sendDirectMessage(ghost, receiver, msg, channelID, msgID, parentID, createAt)

		return
	}

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, "", createAt)
}

func (s *Slack) handleMemberLeftChannel(ctx context.Context, rmsg *slack.MemberLeftChannelEvent) {
	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Removed: []*bridge.UserInfo{
				s.GetUser(ctx, rmsg.User),
			},
			ChannelID: rmsg.Channel,
		},
	}

	s.eventChan <- event
}

func (s *Slack) handleMemberJoinedChannel(ctx context.Context, rmsg *slack.MemberJoinedChannelEvent) {
	var adder *bridge.UserInfo

	if rmsg.Inviter != "" {
		adder = s.GetUser(ctx, rmsg.Inviter)
	}

	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				s.GetUser(ctx, rmsg.User),
			},
			Adder:     adder,
			ChannelID: rmsg.Channel,
		},
	}
	s.eventChan <- event
}

func (s *Slack) sendDirectMessage(sender, receiver *bridge.UserInfo, msg, channelID, msgID, parentID string, createAt int64) {
	event := &bridge.Event{
		Type: "direct_message",
	}

	d := &bridge.DirectMessageEvent{
		Text:      msg,
		ChannelID: channelID,
		MessageID: msgID,
		ParentID:  parentID,
		CreateAt:  createAt,
	}

	d.Sender = sender
	d.Receiver = receiver

	event.Data = d

	s.eventChan <- event
}

func (s *Slack) sendPublicMessage(ghost *bridge.UserInfo, msg, channelID, msgID, parentID, eventType string, createAt int64) {
	event := &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:      msg,
			ChannelID: channelID,
			Sender:    ghost,
			MessageID: msgID,
			ParentID:  parentID,
			Event:     eventType,
			CreateAt:  createAt,
		},
	}

	s.eventChan <- event
}

func formatSlackMsg(m *slack.Msg) (string, bool) {
	if m == nil {
		return "", false
	}

	var msgs []string

	msghandled := false

	if m.Text != "" {
		msgs = append(msgs, strings.Split(m.Text, "\n")...)
		msghandled = true
	}

	for _, attach := range m.Attachments {
		if attach.Pretext != "" {
			msgs = append(msgs, strings.Split(attach.Pretext, "\n")...)
			msghandled = true
		}

		if attach.Text == "" {
			continue
		}

		for i, row := range strings.Split(attach.Text, "\n") {
			msgs = append(msgs, "> "+row)

			if i > 4 {
				msgs = append(msgs, "> ...")
				break
			}
		}

		msghandled = true
	}

	for _, file := range m.Files {
		msgs = append(msgs, "Uploaded "+file.Mode+" "+
			file.Name+" / "+file.Title+" ("+file.Filetype+"): "+file.URLPrivate)
		msghandled = true
	}

	if !msghandled {
		return "", false
	}

	return strings.Join(msgs, "\n"), true
}

func slackActionSuffix(unicode bool, kind string) string {
	switch kind {
	case subTypeMessageChanged:
		if unicode {
			return " \x1d(✏️edited)\x1d"
		}

		return " \x1d(edited)\x1d"
	case subTypeMessageDeleted:
		if unicode {
			return " \x1d(🗑️deleted)\x1d"
		}

		return " \x1d(deleted)\x1d"
	default:
		return ""
	}
}

func formatDeletedSlackMsg(rc *config.RuntimeConfig, origMsg, replySuffix string) string {
	summary := origMsg
	if origMsg != "" && rc != nil {
		summary = utils.FormatAndShortenSummary(origMsg, slackSummaryOpts(rc))
	}

	unicode := rc != nil && rc.Slack.Formatter.Unicode
	out := summary + replySuffix + slackActionSuffix(unicode, subTypeMessageDeleted)

	return strings.TrimLeft(out, " ")
}

func (s *Slack) resolveSlackPostGhost(rmsg *slack.MessageEvent) (*bridge.UserInfo, bool) {
	isDM := strings.HasPrefix(rmsg.Channel, "D")
	userID := rmsg.User

	if rmsg.SubType == subTypeMessageChanged && rmsg.SubMessage != nil {
		userID = rmsg.SubMessage.User
	}

	isSlackbot := userID == "USLACKBOT" || (rmsg.SubType == "bot_message" && rmsg.BotID == "B01" && rmsg.Username == "")

	if !isDM && isSlackbot {
		return &bridge.UserInfo{Nick: systemUser}, true
	}

	// TODO: cache userinfo
	suser, err := s.sc.GetUserFromMessage(rmsg)
	if err != nil {
		logger.Errorf("couldn't find user in message %#v: %s", spew.Sdump(rmsg), err)
		return nil, false
	}

	return s.createUser(suser), true
}

func (s *Slack) deletedMsgSender(ctx context.Context, rmsg *slack.MessageEvent, cp slackCachedPost, hasCached bool) *bridge.UserInfo {
	switch {
	case rmsg.PreviousMessage != nil && (rmsg.PreviousMessage.User != "" || rmsg.PreviousMessage.BotID != "" || rmsg.PreviousMessage.Username != ""):
		return s.msgAuthorGhost(ctx, rmsg.PreviousMessage)
	case hasCached && cp.sender != nil:
		return cp.sender
	case rmsg.User != "":
		if ghost, ok := s.resolveSlackPostGhost(rmsg); ok {
			return ghost
		}
	}

	return &bridge.UserInfo{Nick: systemUser}
}

func (s *Slack) deletedMsgOrigText(rmsg *slack.MessageEvent, cp slackCachedPost, hasCached bool) string {
	if rmsg.PreviousMessage != nil {
		if rawText, ok := formatSlackMsg(rmsg.PreviousMessage); ok {
			return s.cleanupMessage(rawText)
		}
	}

	if hasCached {
		return cp.message
	}

	return ""
}

func deletedMsgParentTS(rmsg *slack.MessageEvent, cp slackCachedPost, hasCached bool, msgTS string) string {
	if rmsg.ThreadTimestamp != "" && rmsg.ThreadTimestamp != msgTS {
		return rmsg.ThreadTimestamp
	}

	if rmsg.PreviousMessage != nil && rmsg.PreviousMessage.ThreadTimestamp != "" && rmsg.PreviousMessage.ThreadTimestamp != msgTS {
		return rmsg.PreviousMessage.ThreadTimestamp
	}

	if hasCached && cp.rootTS != "" && cp.rootTS != msgTS {
		return cp.rootTS
	}

	return ""
}

func (s *Slack) handleSlackMessageDeleted(ctx context.Context, rmsg *slack.MessageEvent) {
	channelID := rmsg.Channel

	msgTS := rmsg.DeletedTimestamp
	if msgTS == "" {
		msgTS = rmsg.Timestamp
	}

	msgID := encodeSlackID(channelID, msgTS)
	cp, hasCached := s.getCachedPostOnly(channelID, msgTS)
	parentTS := deletedMsgParentTS(rmsg, cp, hasCached, msgTS)

	parentID := ""
	if parentTS != "" {
		parentID = encodeSlackID(channelID, parentTS)
	}

	ghost := s.deletedMsgSender(ctx, rmsg, cp, hasCached)
	origMsg := s.deletedMsgOrigText(rmsg, cp, hasCached)

	replySuffix := ""
	if parentID != "" {
		replySuffix = s.formatReplySuffix(ctx, channelID, parentTS)
	}

	var rc *config.RuntimeConfig
	if s.cfg != nil {
		rc = s.cfg.Current()
	}

	msg := formatDeletedSlackMsg(rc, origMsg, replySuffix)
	createAt := slackTSMillis(msgTS)

	s.evictCachedPost(channelID, msgTS)

	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)
		s.sendDirectMessage(ghost, receiver, msg, channelID, msgID, parentID, createAt)

		return
	}

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, "post_deleted", createAt)
}

//nolint:funlen,gocyclo
func (s *Slack) handleSlackActionPost(ctx context.Context, rmsg *slack.MessageEvent) {
	logger.Debugf("handleSlackActionPost: receiving msg %#v", rmsg)

	// Thread metadata updates carry no new message; the actual reply arrives in its own event.
	if rmsg.SubType == "message_replied" {
		return
	}

	// Is this our own message
	if s.sc.IsOwnMessageEcho(rmsg.User, rmsg.Channel, rmsg.Timestamp) {
		return
	}

	if rmsg.SubType == subTypeMessageDeleted {
		s.handleSlackMessageDeleted(ctx, rmsg)
		return
	}

	if rmsg.SubType == eventChannelTopic {
		topic := s.cleanupMessage(rmsg.Topic)
		if !strings.HasPrefix(rmsg.Channel, "D") {
			s.eventChan <- &bridge.Event{
				Type: eventChannelTopic,
				Data: &bridge.ChannelTopicEvent{
					Text:      topic,
					ChannelID: rmsg.Channel,
					UserID:    rmsg.User,
				},
			}

			return
		}

		rmsg.Text = "\x01ACTION updated topic to: " + topic + "\x01"
	}

	eventType := ""
	if rmsg.SubType == eventChannelPurpose && !strings.HasPrefix(rmsg.Channel, "D") {
		eventType = eventChannelPurpose

		if rmsg.Purpose != "" {
			rmsg.Text = "set the channel description: " + rmsg.Purpose
		}
	}

	msgTS := rmsg.Timestamp
	parentTS := rmsg.ThreadTimestamp

	if rmsg.SubType == subTypeMessageChanged {
		if rmsg.SubMessage == nil {
			return
		}

		eventType = "post_edited"
		msgTS = rmsg.SubMessage.Timestamp

		if rmsg.SubMessage.ThreadTimestamp != "" {
			parentTS = rmsg.SubMessage.ThreadTimestamp
		}
	}

	ghost, ok := s.resolveSlackPostGhost(rmsg)
	if !ok {
		return
	}

	var (
		rawText    string
		msghandled bool
	)

	if rmsg.SubType == subTypeMessageChanged && rmsg.SubMessage != nil {
		rawText, msghandled = formatSlackMsg(rmsg.SubMessage)
	} else {
		rawText, msghandled = formatSlackMsg(&rmsg.Msg)
	}

	if !msghandled {
		return
	}

	msg := s.cleanupMessage(rawText)
	channelID := rmsg.Channel
	msgID := encodeSlackID(channelID, msgTS)

	parentID := ""
	rootTS := msgTS

	if parentTS != "" && parentTS != msgTS {
		parentID = encodeSlackID(channelID, parentTS)
		rootTS = parentTS
	}

	switch rmsg.SubType {
	case eventChannelTopic, eventChannelPurpose:
		s.cacheThreadRoot(channelID, msgTS, rootTS)
	default:
		s.cachePost(channelID, msgTS, rootTS, ghost, msg)
	}

	if parentID != "" {
		msg = strings.TrimRight(msg, "\n") + s.formatReplySuffix(ctx, channelID, parentTS)
	}

	if rmsg.SubType == subTypeMessageChanged {
		msg = strings.TrimRight(msg, "\n") + slackActionSuffix(s.FormatterConfig().Unicode, subTypeMessageChanged)
	}

	createAt := slackTSMillis(msgTS)

	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)
		s.sendDirectMessage(ghost, receiver, msg, channelID, msgID, parentID, createAt)

		return
	}

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, eventType, createAt)
}

func isEnterpriseUser(slackuser *slack.User) bool {
	return slackuser.Enterprise.EnterpriseID != "" ||
		strings.HasPrefix(slackuser.ID, "W") ||
		(slackuser.ID != "" && strings.EqualFold(slackuser.Name, slackuser.ID))
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if sanitized := sanitizeSlackNick(v); sanitized != "" {
			return sanitized
		}
	}

	return ""
}

func (s *Slack) resolveSlackNick(slackuser *slack.User, rc *config.RuntimeConfig) string {
	realName := slackuser.RealName
	if realName == "" {
		realName = slackuser.Profile.RealName
	}

	displayName := slackuser.Profile.DisplayName
	enterprise := isEnterpriseUser(slackuser)

	var nick string

	switch {
	case rc.Slack.UseRealName:
		nick = firstNonEmpty(realName, displayName, slackuser.Name, slackuser.ID)
	case rc.Slack.PreferNickname || rc.Slack.UseDisplayName:
		nick = firstNonEmpty(displayName, realName, slackuser.Name, slackuser.ID)
	case enterprise:
		nick = firstNonEmpty(displayName, realName, slackuser.Name, slackuser.ID)
	default:
		nick = firstNonEmpty(slackuser.Name, slackuser.ID)
	}

	if slackuser.ID == "" {
		return nick
	}

	if enterprise && !strings.EqualFold(nick, slackuser.ID) {
		nick += "-" + strings.ToLower(slackuser.ID)
	}

	s.Lock()
	defer s.Unlock()

	if owner, taken := s.userByNick[nick]; taken && owner != slackuser.ID {
		nick += "-" + strings.ToLower(slackuser.ID)
	}

	s.userByNick[nick] = slackuser.ID

	return nick
}

func (s *Slack) createUser(slackuser *slack.User) *bridge.UserInfo {
	if slackuser == nil {
		return &bridge.UserInfo{}
	}

	rc := s.cfg.Current()
	nick := s.resolveSlackNick(slackuser, rc)

	me := slackuser.ID == s.sc.UserID()

	info := &bridge.UserInfo{
		Nick:        nick,
		User:        slackuser.ID,
		Real:        slackuser.RealName,
		Host:        "host",
		Roles:       "",
		DisplayName: slackuser.Profile.DisplayName,
		Ghost:       true,
		Me:          me,
		Username:    slackuser.Profile.RealName,
		FirstName:   slackuser.Profile.FirstName,
		LastName:    slackuser.Profile.LastName,
		TeamID:      s.sc.TeamID(),
	}

	return info
}

func (s *Slack) Connected() bool {
	return s.connected
}

func (s *Slack) recordSent(ctx context.Context, channelID, msgID, threadTS, parentID, text string) {
	if s.msgLastSentCache == nil || msgID == "" {
		return
	}

	name := s.GetChannelName(ctx, channelID)
	if name == "" {
		name = channelID
	}

	summary := strings.Join(strings.Fields(text), " ")
	if len(summary) > slackLastSentSummaryLen {
		summary = strings.ToValidUTF8(summary[:slackLastSentSummaryLen], "") + "..."
	}

	if threadTS != "" {
		summary += s.formatReplySuffix(ctx, channelID, threadTS)
	}

	key := msgID
	if parentID != "" {
		key = parentID
	}

	s.msgLastSentCache.Add(key, name+": "+summary)
}

func (s *Slack) MsgUserThread(ctx context.Context, username, parentID, text string) (string, error) {
	channelID := s.GetDMChannelID(ctx, username)
	if channelID == "" {
		return "", errors.New("could not open DM channel for " + username)
	}

	return s.MsgChannelThread(ctx, channelID, parentID, text)
}

func (s *Slack) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	text = slackActionText(text)

	var threadTS, resolvedParentID string

	if parentID != "" {
		decodedCh, decodedTS, ok := decodeSlackID(parentID)
		if !ok {
			return "", errors.New("invalid Slack thread ID: " + parentID)
		}

		if !strings.EqualFold(decodedCh, channelID) {
			return "", errors.New("thread ID belongs to a different channel")
		}

		threadTS = s.resolveThreadRoot(ctx, channelID, decodedTS)
		resolvedParentID = encodeSlackID(channelID, threadTS)
	}

	ts, err := s.sc.PostMessageThread(ctx, channelID, threadTS, text)
	if err != nil {
		return "", err
	}

	msgID := encodeSlackID(channelID, ts)

	rootTS := ts
	if threadTS != "" {
		rootTS = threadTS
	}

	s.cachePost(channelID, ts, rootTS, s.GetMe(), text)
	s.recordSent(ctx, channelID, msgID, threadTS, resolvedParentID, text)

	return msgID, nil
}

func (s *Slack) ModifyPost(ctx context.Context, msgID, text string) error {
	channelID, ts, ok := decodeSlackID(msgID)
	if !ok {
		return errors.New("invalid Slack message ID: " + msgID)
	}

	if text == "" {
		return s.sc.DeleteMessage(ctx, channelID, ts)
	}

	return s.sc.UpdateMessage(ctx, channelID, ts, text)
}

func (s *Slack) AddReaction(ctx context.Context, msgID, emoji string) error {
	channelID, ts, ok := decodeSlackID(msgID)
	if !ok {
		return errors.New("invalid Slack message ID: " + msgID)
	}

	return s.sc.AddReaction(ctx, channelID, ts, emoji)
}

func (s *Slack) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	channelID, ts, ok := decodeSlackID(msgID)
	if !ok {
		return errors.New("invalid Slack message ID: " + msgID)
	}

	return s.sc.RemoveReaction(ctx, channelID, ts, emoji)
}

func (s *Slack) GetLastSentMsgs() []string {
	if s.msgLastSentCache == nil {
		return []string{}
	}

	keys := s.msgLastSentCache.Keys()
	data := make([]string, 0, len(keys))

	for _, k := range keys {
		if v, ok := s.msgLastSentCache.Get(k); ok {
			data = append(data, fmt.Sprintf("[@@%s] %s", k, v))
		}
	}

	return data
}

func (s *Slack) Config() any {
	return &s.cfg.Current().Slack
}

func (s *Slack) BridgeConfig() *config.BridgeConfig {
	return &s.cfg.Current().Slack.Bridge
}

func (s *Slack) FormatterConfig() *config.FormatterConfig {
	return &s.cfg.Current().Slack.Formatter
}

func (s *Slack) GetPostChannelID(ctx context.Context, postID string) string {
	channelID, _, ok := decodeSlackID(postID)
	if !ok {
		return ""
	}

	return channelID
}

func (s *Slack) GetPostSizeLimit() int {
	return 4000
}

func (s *Slack) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) Ping(ctx context.Context, proto ...string) error {
	return nil
}

func firstEventRootTS(events []*bridge.Event) string {
	if len(events) == 0 {
		return ""
	}

	var firstMsgID string

	switch e := events[0].Data.(type) {
	case *bridge.ChannelMessageEvent:
		firstMsgID = e.MessageID
	case *bridge.DirectMessageEvent:
		firstMsgID = e.MessageID
	}

	if firstMsgID == "" {
		return ""
	}

	if _, rootTS, ok := decodeSlackID(firstMsgID); ok {
		return rootTS
	}

	return ""
}

func (s *Slack) PostURL(ctx context.Context, postID string, events []*bridge.Event) string {
	channelID, ts, ok := decodeSlackID(postID)
	if !ok {
		return ""
	}

	if s.sc != nil {
		permalink, err := s.sc.GetPermalink(ctx, channelID, ts)
		if err == nil && permalink != "" {
			return permalink
		}
	}

	rootTS := firstEventRootTS(events)
	if rootTS == "" {
		if parentID := s.cachedParentID(channelID, ts); parentID != "" {
			_, rootTS, _ = decodeSlackID(parentID)
		}
	}

	domain := ""
	if s.sc != nil {
		domain = s.sc.TeamDomain()
	}

	return formatSlackArchiveURL(domain, channelID, ts, rootTS)
}

func (s *Slack) NormalizePostID(msgID string) string {
	if ch, ts, ok := parseSlackArchiveURL(msgID); ok {
		return encodeSlackID(ch, ts)
	}

	msgID = strings.ToLower(msgID)
	if _, _, ok := decodeSlackID(msgID); ok {
		return msgID
	}

	return ""
}

func (s *Slack) SendTyping(ctx context.Context, channelName string) {
}
