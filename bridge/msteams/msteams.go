package msteams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/msteamsclient"
	"github.com/sirupsen/logrus"
)

const (
	protocolName         = "msteams"
	defaultPostSizeLimit = 28000
	defaultScrollback    = 60
	maxScrollback        = 1000
	msgConvCacheSize     = 2000
	lastSentSummaryLen   = 90
)

// CachedPost stores parent summary context matching bridge/mattermost.CachedPost.
type CachedPost struct {
	RootID     string
	ParentUser *bridge.UserInfo
	ReplyMsg   string
}

type MSTeams struct {
	mc          *msteamsclient.Client
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	connected   atomic.Bool

	done       chan struct{}
	logoutOnce sync.Once

	unregisterReloadHook func()

	dmChannelCache   *lru.Cache[string, string]
	msgParentCache   *lru.Cache[string, CachedPost]
	msgLastSentCache *lru.Cache[string, string]
	msgConvCache     *lru.Cache[string, string]

	namesMu   sync.RWMutex
	chanByID  map[string]string
	idByChan  map[string]string
	nickByMRI map[string]string
	mriByNick map[string]string

	cfg *config.Config

	//nolint:containedctx // Tied to bridge lifecycle, matching bridge/mattermost and bridge/matrix.
	ctx    context.Context
	cancel context.CancelFunc

	lastTypingSent sync.Map
}

var (
	logger     *logrus.Entry
	loggerOnce sync.Once
)

// New logs in to Microsoft Teams and returns a bridge.Bridger plus the
// underlying msteamsclient.Client, matching bridge/mattermost.New's conventions.
//
//nolint:funlen
func New(
	ctx context.Context,
	cfg *config.Config,
	cred bridge.Credentials,
	eventChan chan *bridge.Event,
	onWsConnect func(),
	onWsDisconnect func(error),
	onAuthPrompt func(msteamsclient.AuthPrompt),
) (bridge.Bridger, *msteamsclient.Client, error) {
	rc := cfg.Current()

	loggerOnce.Do(func() {
		l := logrus.New()
		l.SetFormatter(&prefixed.TextFormatter{
			PrefixPadding: 13,
			DisableColors: false,
			FullTimestamp: true,
		})
		logger = l.WithFields(logrus.Fields{"prefix": "bridge/msteams"})
	})

	ourlog := logger.Logger
	applyBridgeLogLevel(ourlog, rc)

	bridgeCtx, bridgeCancel := context.WithCancel(ctx)

	m := &MSTeams{
		credentials: cred,
		eventChan:   eventChan,
		cfg:         cfg,
		done:        make(chan struct{}),
		ctx:         bridgeCtx,
		cancel:      bridgeCancel,
		chanByID:    make(map[string]string),
		idByChan:    make(map[string]string),
		nickByMRI:   make(map[string]string),
		mriByNick:   make(map[string]string),
	}

	m.dmChannelCache, _ = lru.New[string, string](128)
	m.msgParentCache, _ = lru.New[string, CachedPost](256)
	m.msgLastSentCache, _ = lru.New[string, string](128)
	m.msgConvCache, _ = lru.New[string, string](msgConvCacheSize)

	mc := m.buildClient(rc, cred, onWsConnect, onWsDisconnect, onAuthPrompt)

	setBridgeLogLevels := func(rc *config.RuntimeConfig) {
		applyBridgeLogLevel(ourlog, rc)
		applyClientLogLevels(mc, rc)
	}

	setBridgeLogLevels(rc)
	m.unregisterReloadHook = cfg.RegisterReloadHook(setBridgeLogLevels)

	err := mc.Login(bridgeCtx)
	if err != nil {
		m.unregisterReloadHook()
		bridgeCancel()

		return nil, nil, err
	}

	m.mc = mc
	m.connected.Store(true)
	m.rebuildNames()

	go m.handleMSTeams()

	return m, mc, nil
}

func (m *MSTeams) buildClient(
	rc *config.RuntimeConfig,
	cred bridge.Credentials,
	onWsConnect func(),
	onWsDisconnect func(error),
	onAuthPrompt func(msteamsclient.AuthPrompt),
) *msteamsclient.Client {
	clientCred := buildClientCredentials(rc, cred)

	mc := msteamsclient.New(clientCred)
	mc.UserAgent = config.UserAgent
	mc.Timeout = rc.ClientTimeout
	mc.ForceSyncOnReconnect = rc.MSTeams.ForceSyncOnReconnect
	mc.PollFallback = rc.MSTeams.PollFallback
	mc.OnAuthPrompt = onAuthPrompt
	mc.OnWsConnect = func() {
		if rc.MSTeams.ForceSyncOnReconnect {
			_ = mc.UpdateChannels(m.ctx)
			m.rebuildNames()
		}

		if onWsConnect != nil {
			onWsConnect()
		}
	}
	mc.OnWsDisconnect = onWsDisconnect

	tokenFile := rc.MSTeams.TokenSaveFile
	if tokenFile != "" {
		mc.OnTokenUpdate = func(state msteamsclient.TokenState) {
			saveErr := saveTokenState(tokenFile, state)
			if saveErr != nil {
				logger.Warnf("Failed to persist Teams token state to %s: %v", tokenFile, saveErr)
			}
		}
	}

	return mc
}

func buildClientCredentials(rc *config.RuntimeConfig, cred bridge.Credentials) msteamsclient.Credentials {
	clientCred := msteamsclient.Credentials{
		AccountType:   parseAccountType(rc.MSTeams.AccountType),
		TenantID:      rc.MSTeams.TenantID,
		ClientID:      rc.MSTeams.ClientID,
		SkipTLSVerify: rc.MSTeams.SkipTLSVerify,
	}

	if rc.MSTeams.TokenSaveFile != "" {
		saved, err := loadTokenState(rc.MSTeams.TokenSaveFile)
		if err == nil && saved != nil {
			clientCred.TokenState = saved
		} else if err != nil && !os.IsNotExist(err) {
			logger.Warnf("Failed to load Teams token state from %s: %v", rc.MSTeams.TokenSaveFile, err)
		}
	}

	// Credentials.Server may carry an account type ("work"/"personal") or tenant override,
	// or a regional chatService URL when logging in with a browser skypetoken.
	if server := strings.TrimSpace(cred.Server); server != "" {
		switch strings.ToLower(server) {
		case "work", "personal", "auto":
			clientCred.AccountType = parseAccountType(server)
		default:
			if strings.HasPrefix(server, "https://") || strings.HasPrefix(server, "http://") {
				clientCred.ChatServiceURL = server
			} else {
				clientCred.TenantID = server
				clientCred.AccountType = msteamsclient.AccountWork
			}
		}
	}

	// Credentials.Login selects the explicit token mode ("refresh" or "token") when set.
	switch strings.ToLower(strings.TrimSpace(cred.Login)) {
	case "refresh":
		clientCred.RefreshToken = strings.TrimSpace(cred.Token)
		clientCred.TokenState = nil
	case "token":
		clientCred.SkypeToken = strings.TrimSpace(cred.Token)
		clientCred.TokenState = nil
	default:
		if tok := strings.TrimSpace(cred.Token); tok != "" {
			if strings.HasPrefix(tok, "skypetoken=") || strings.HasPrefix(tok, "eyJ") {
				clientCred.SkypeToken = tok
			} else {
				clientCred.RefreshToken = tok
			}
		}
	}

	return clientCred
}

func parseAccountType(raw string) msteamsclient.AccountType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "work":
		return msteamsclient.AccountWork
	case "personal":
		return msteamsclient.AccountPersonal
	default:
		return msteamsclient.AccountAuto
	}
}

func applyBridgeLogLevel(ourlog *logrus.Logger, rc *config.RuntimeConfig) {
	switch {
	case rc.Trace:
		ourlog.SetLevel(logrus.TraceLevel)
	case rc.Debug:
		ourlog.SetLevel(logrus.DebugLevel)
	default:
		ourlog.SetLevel(logrus.InfoLevel)
	}
}

func applyClientLogLevels(mc *msteamsclient.Client, rc *config.RuntimeConfig) {
	mc.SetLogLevel("info")

	if rc.MSTeams.MsTeamsclientLogLevel != "" {
		logger.Infof("enabling msteamsclient logging: level: %s", rc.MSTeams.MsTeamsclientLogLevel)
		mc.SetLogLevel(strings.ToLower(rc.MSTeams.MsTeamsclientLogLevel))
	}

	mc.SetLogAPICalls("error")

	if rc.Profiling {
		if rc.Trace {
			logger.Infof("enabling msteamsclient API logging: level: trace")
			mc.SetLogAPICalls("trace")
		} else if rc.Debug {
			logger.Infof("enabling msteamsclient API logging: level: warn")
			mc.SetLogAPICalls("warn")
		}
	}
}

func loadTokenState(path string) (*msteamsclient.TokenState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var state msteamsclient.TokenState

	err = json.Unmarshal(data, &state)
	if err != nil {
		return nil, err
	}

	return &state, nil
}

func saveTokenState(path string, state msteamsclient.TokenState) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

	//nolint:gosec // G117: persisting rotated OAuth tokens to 0600 TokenSaveFile is intentional.
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	tmpPath := path + ".tmp"

	err = os.WriteFile(tmpPath, data, 0o600)
	if err != nil {
		return err
	}

	return os.Rename(tmpPath, path)
}

func (m *MSTeams) Protocol() string {
	return protocolName
}

func (m *MSTeams) Config() any {
	return &m.cfg.Current().MSTeams
}

func (m *MSTeams) BridgeConfig() *config.BridgeConfig {
	return &m.cfg.Current().MSTeams.Bridge
}

func (m *MSTeams) FormatterConfig() *config.FormatterConfig {
	return &m.cfg.Current().MSTeams.Formatter
}

func (m *MSTeams) Connected() bool {
	return m.connected.Load()
}

func (m *MSTeams) GetPostSizeLimit() int {
	return defaultPostSizeLimit
}

func (m *MSTeams) Logout(ctx context.Context) error {
	var err error

	m.logoutOnce.Do(func() {
		m.connected.Store(false)
		close(m.done)

		if m.unregisterReloadHook != nil {
			m.unregisterReloadHook()
		}

		if m.cancel != nil {
			m.cancel()
		}

		if m.mc != nil {
			err = m.mc.Logout(ctx)
		}

		m.eventChan <- &bridge.Event{
			Type: "logout",
			Data: &bridge.LogoutEvent{},
		}
	})

	return err
}

func (m *MSTeams) Ping(ctx context.Context, _ ...string) error {
	if m.mc == nil {
		return errors.New("client not initialized")
	}

	return m.mc.Ping(ctx)
}

func (m *MSTeams) NormalizePostID(msgID string) string {
	if _, _, ok := decodeTeamsID(msgID); ok {
		return strings.ToLower(strings.TrimSpace(msgID))
	}

	if convID, rawMsgID, ok := parseTeamsMessageURL(msgID); ok {
		encoded := encodeTeamsID(convID, rawMsgID)
		m.rememberMessageConv(encoded, convID)

		return encoded
	}

	return ""
}

// PostURL implements the optional postURLFormatter interface used by /msg msteams details.
func (m *MSTeams) PostURL(_ context.Context, postID string, _ []*bridge.Event) string {
	convID, rawMsgID, ok := m.resolvePostID(postID)
	if !ok {
		return ""
	}

	return teamsMessageURL(m.mc.TokenState().AccountType, convID, rawMsgID)
}

// ChannelUserPrefix implements bridge.ChannelRolePrefixer for Teams channel/chat owners and admins.
func (m *MSTeams) ChannelUserPrefix(channelID, userID string) string {
	if m.mc.IsChannelAdmin(channelID, userID) {
		return "@"
	}

	return ""
}

func (m *MSTeams) UpdateChannels(ctx context.Context) error {
	err := m.mc.UpdateChannels(ctx)
	if err != nil {
		return err
	}

	m.rebuildNames()

	return nil
}

func (m *MSTeams) Invite(ctx context.Context, channelID, username string) error {
	u := m.GetUserByUsername(ctx, username)
	if u == nil {
		return fmt.Errorf("user %s not found", username)
	}

	return m.mc.AddChannelMember(ctx, channelID, u.User)
}

func (m *MSTeams) Join(ctx context.Context, channelName string) (string, string, error) {
	convID := m.conversationIDByName(channelName)
	if convID == "" {
		return "", "", fmt.Errorf("channel %s not found", channelName)
	}

	return convID, m.Topic(ctx, convID), nil
}

func (m *MSTeams) List(_ context.Context) (map[string]string, error) {
	convs := m.mc.GetChannels()
	out := make(map[string]string, len(convs))

	for _, conv := range convs {
		if conv.Type == msteamsclient.ConversationDM || conv.Type == msteamsclient.ConversationSelfNote {
			continue
		}

		name := "#" + m.ircChannelName(conv.ID)
		out[name] = conv.Topic
	}

	return out, nil
}

func (m *MSTeams) Part(ctx context.Context, channel string) error {
	convID := m.conversationIDByName(channel)
	if convID == "" {
		convID = channel
	}

	if m.cfg.MSTeams().Bridge.PartFake {
		return nil
	}

	return m.mc.LeaveChannel(ctx, convID)
}

func (m *MSTeams) SetTopic(ctx context.Context, channelID, text string) error {
	return m.mc.SetChannelTopic(ctx, channelID, text)
}

func (m *MSTeams) Topic(_ context.Context, channelID string) string {
	if conv := m.mc.GetChannel(channelID); conv != nil {
		return conv.Topic
	}

	return ""
}

func (m *MSTeams) Kick(ctx context.Context, channelID, username string) error {
	u := m.GetUserByUsername(ctx, username)
	if u == nil {
		return fmt.Errorf("user %s not found", username)
	}

	return m.mc.RemoveChannelMember(ctx, channelID, u.User)
}

func (m *MSTeams) Nick(_ context.Context, _ string) error {
	return errors.New("changing nick is not supported by Microsoft Teams")
}

func (m *MSTeams) MsgUser(ctx context.Context, userID, text string) (string, error) {
	convID, err := m.mc.GetOrCreateDM(ctx, userID)
	if err != nil {
		return "", err
	}

	m.dmChannelCache.Add(userID, convID)

	return m.MsgChannel(ctx, convID, text)
}

func (m *MSTeams) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	convID, err := m.mc.GetOrCreateDM(ctx, userID)
	if err != nil {
		return "", err
	}

	m.dmChannelCache.Add(userID, convID)

	return m.MsgChannelThread(ctx, convID, parentID, text)
}

func (m *MSTeams) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	sendText := msteamsActionText(text)

	msg, err := m.mc.SendMessage(ctx, channelID, sendText)
	if err != nil {
		return "", err
	}

	encoded := encodeTeamsID(channelID, msg.ID)
	m.recordSentPost(encoded, channelID, sendText)

	return encoded, nil
}

func (m *MSTeams) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	rawParentID := parentID
	if _, decodedID, ok := m.resolvePostID(parentID); ok {
		rawParentID = decodedID
	}

	sendText := msteamsActionText(text)

	msg, err := m.mc.SendMessageThread(ctx, channelID, rawParentID, sendText)
	if err != nil {
		return "", err
	}

	encoded := encodeTeamsID(channelID, msg.ID)
	m.recordSentPost(encoded, channelID, sendText)

	return encoded, nil
}

func msteamsActionText(text string) string {
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		return "\x1d" + strings.TrimSuffix(after, "\x01") + "\x1d"
	}

	return text
}

func (m *MSTeams) recordSentPost(encodedID, channelID, text string) {
	if encodedID == "" {
		return
	}

	m.rememberMessageConv(encodedID, channelID)
	m.msgLastSentCache.Add(encodedID, m.formatSummary(text, lastSentSummaryLen))
	m.cachePostContext(encodedID, "", m.GetMe(), text, m.cfg.MSTeams().ShortenRepliesTo)
}

func (m *MSTeams) GetLastSentMsgs() []string {
	keys := m.msgLastSentCache.Keys()
	data := make([]string, 0, len(keys))

	for _, k := range keys {
		if msg, ok := m.msgLastSentCache.Get(k); ok {
			data = append(data, "[@@"+k+"] "+msg)
		}
	}

	return data
}

func (m *MSTeams) ModifyPost(ctx context.Context, msgID, text string) error {
	convID, rawMsgID, ok := m.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	if text == "" {
		return m.mc.DeleteMessage(ctx, convID, rawMsgID)
	}

	return m.mc.EditMessage(ctx, convID, rawMsgID, text)
}

func (m *MSTeams) AddReaction(ctx context.Context, msgID, emoji string) error {
	convID, rawMsgID, ok := m.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	key := ircEmojiToTeamsEmotion(emoji, m.cfg.MSTeams().Formatter.CustomEmoji)

	return m.mc.AddReaction(ctx, convID, rawMsgID, key)
}

func (m *MSTeams) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	convID, rawMsgID, ok := m.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	key := ircEmojiToTeamsEmotion(emoji, m.cfg.MSTeams().Formatter.CustomEmoji)

	return m.mc.RemoveReaction(ctx, convID, rawMsgID, key)
}

func (m *MSTeams) HasCustomStatus(userID string) bool {
	return m.mc.GetStatusNote(userID) != ""
}

func (m *MSTeams) IsOnline(status string) bool {
	switch strings.ToLower(status) {
	case "online", "away", "dnd":
		return true
	default:
		return false
	}
}

func (m *MSTeams) StatusUser(ctx context.Context, userID string) (string, error) {
	p := m.mc.GetPresenceFresh(ctx, userID)
	if p == nil {
		return m.mc.GetStatus(userID), nil
	}

	status := msteamsclient.MapAvailabilityToStatus(p.Availability)
	localTime := m.mc.GetUserLocalTime(userID)

	var lastOnline string

	if p.LastActiveAt > 0 {
		diff := max(0, time.Now().Unix()-p.LastActiveAt)
		if status == "online" {
			lastOnline = utils.FormatLastOnline(p.LastActiveAt, true)
		} else if diff >= 20*60 {
			lastOnline = utils.FormatLastOnline(p.LastActiveAt)
		}
	}

	return formatStatusWithDetails(status, lastOnline, localTime, m.mc.FormatStatusNote(userID)), nil
}

func formatStatusWithDetails(status, lastOnline, localTime, note string) string {
	var details string

	switch {
	case lastOnline != "" && localTime != "":
		details = fmt.Sprintf("%s, local time: %s", lastOnline, localTime)
	case lastOnline != "":
		details = lastOnline
	case localTime != "":
		details = fmt.Sprintf("local time: %s", localTime)
	default:
		if note != "" {
			return fmt.Sprintf("%s: %s", status, note)
		}

		return status
	}

	if note != "" {
		return fmt.Sprintf("%s (%s): %s", status, details, note)
	}

	return fmt.Sprintf("%s (%s)", status, details)
}

func (m *MSTeams) StatusUsers(ctx context.Context) (map[string]string, error) {
	users := m.mc.GetUsers()
	mris := make([]string, 0, len(users))

	for _, u := range users {
		if u.MRI != "" && !u.IsBot {
			mris = append(mris, u.MRI)
		}
	}

	if len(mris) > 0 {
		_, _ = m.mc.GetPresences(ctx, mris)
	}

	return m.mc.GetStatuses(), nil
}

func (m *MSTeams) SetStatus(ctx context.Context, status string) error {
	return m.mc.SetPresence(ctx, status)
}

func (m *MSTeams) CreateChannel(ctx context.Context, channelName, _ string) (*bridge.ChannelInfo, error) {
	topic := strings.TrimPrefix(channelName, "#")

	conv, err := m.mc.CreateGroupChat(ctx, nil, topic)
	if err != nil {
		return nil, err
	}

	m.rebuildNames()

	return m.conversationToChannelInfo(conv), nil
}

func (m *MSTeams) GetChannels() []*bridge.ChannelInfo {
	convs := m.mc.GetChannels()
	out := make([]*bridge.ChannelInfo, 0, len(convs))

	for _, conv := range convs {
		out = append(out, m.conversationToChannelInfo(conv))
	}

	return out
}

func (m *MSTeams) GetChannel(_ context.Context, channelID string) (*bridge.ChannelInfo, error) {
	conv := m.mc.GetChannel(channelID)
	if conv == nil {
		return nil, fmt.Errorf("channel %s not found", channelID)
	}

	return m.conversationToChannelInfo(conv), nil
}

func (m *MSTeams) GetChannelCount() int {
	return len(m.mc.GetChannels())
}

func (m *MSTeams) GetChannelName(_ context.Context, channelID string) string {
	if channelID == "" || strings.HasPrefix(channelID, "&") || strings.HasPrefix(channelID, "#") {
		return channelID
	}

	if m.isDMConversation(channelID) {
		return channelID
	}

	return "#" + m.ircChannelName(channelID)
}

func (m *MSTeams) GetChannelID(_ context.Context, name, _ string) string {
	return m.conversationIDByName(name)
}

func (m *MSTeams) GetLastViewedAt(_ context.Context, channelID string) int64 {
	return m.mc.GetChannelLastViewedAt(channelID)
}

func (m *MSTeams) UpdateLastViewed(ctx context.Context, channelID string) {
	if m.cfg.MSTeams().DisableAutoView || channelID == "" {
		return
	}

	err := m.mc.MarkConversationRead(ctx, channelID, time.Now().UnixMilli())
	if err != nil {
		logger.Debugf("UpdateLastViewed failed for %s: %v", channelID, err)
	}
}

func (m *MSTeams) UpdateLastViewedUser(ctx context.Context, userID string) error {
	if m.cfg.MSTeams().DisableAutoView {
		return nil
	}

	convID := m.GetDMChannelID(ctx, userID)
	if convID == "" {
		return nil
	}

	return m.mc.MarkConversationRead(ctx, convID, time.Now().UnixMilli())
}

func (m *MSTeams) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	members, err := m.mc.GetChannelMembers(ctx, channelID)
	if err != nil {
		return nil, err
	}

	if len(members) > 0 {
		_, _ = m.mc.ResolveUsers(ctx, members)
	}

	users := make([]*bridge.UserInfo, 0, len(members))
	for _, mri := range members {
		if info := m.resolveUserInfo(ctx, mri, ""); info != nil {
			users = append(users, info)
		}
	}

	return users, nil
}

func (m *MSTeams) GetUsers() []*bridge.UserInfo {
	rawUsers := m.mc.GetUsers()
	out := make([]*bridge.UserInfo, 0, len(rawUsers))

	for _, u := range rawUsers {
		out = append(out, m.userToUserInfo(u))
	}

	return out
}

func (m *MSTeams) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return m.resolveUserInfo(ctx, userID, "")
}

func (m *MSTeams) GetUserChannels(_ context.Context, userID string) ([]string, error) {
	convs := m.mc.GetChannels()
	out := make([]string, 0)

	for _, conv := range convs {
		if slices.Contains(conv.Members, userID) {
			out = append(out, conv.ID)
		}
	}

	return out, nil
}

func (m *MSTeams) GetUserCount() int {
	return m.mc.GetUserCount()
}

func (m *MSTeams) GetMe() *bridge.UserInfo {
	me := m.mc.Me()
	if me == nil {
		return &bridge.UserInfo{Me: true}
	}

	info := m.userToUserInfo(me)
	info.Me = true
	info.Ghost = false

	return info
}

func (m *MSTeams) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	if username == "" {
		return nil
	}

	if mri := m.mriForNick(username); mri != "" {
		return m.resolveUserInfo(ctx, mri, "")
	}

	if u := m.mc.GetUser(username); u != nil {
		return m.userToUserInfo(u)
	}

	if u := m.mc.GetUserByEmail(username); u != nil {
		return m.userToUserInfo(u)
	}

	return nil
}

func (m *MSTeams) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	users, err := m.mc.SearchUsers(ctx, query, 50)
	if err != nil {
		return nil, err
	}

	out := make([]*bridge.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, m.userToUserInfo(u))
	}

	return out, nil
}

func (m *MSTeams) GetDMChannelID(_ context.Context, userID string) string {
	if cached, ok := m.dmChannelCache.Get(userID); ok {
		return cached
	}

	convID := m.mc.GetDMConversationID(userID)
	if convID != "" {
		m.dmChannelCache.Add(userID, convID)
	}

	return convID
}

func (m *MSTeams) GetDMChannelName(_, userID2 string) string {
	return m.mc.GetDMConversationID(userID2)
}

func (m *MSTeams) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	me := m.mc.Me()

	myMRI := ""
	if me != nil {
		myMRI = me.MRI
	}

	otherMRI, ok := m.GetDMOtherUserID(channelName, myMRI)
	if !ok {
		return nil
	}

	return m.resolveUserInfo(ctx, otherMRI, "")
}

func (m *MSTeams) GetDMUserIDs(channelName string) (string, string, bool) {
	me := m.mc.Me()
	if me == nil {
		return "", "", false
	}

	other, ok := m.GetDMOtherUserID(channelName, me.MRI)
	if !ok {
		return "", "", false
	}

	return me.MRI, other, true
}

func (m *MSTeams) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	conv := m.mc.GetChannel(channelName)
	if conv == nil {
		return "", false
	}

	if conv.Type == msteamsclient.ConversationSelfNote {
		return myUserID, myUserID != ""
	}

	if conv.Type != msteamsclient.ConversationDM {
		return "", false
	}

	for _, member := range conv.Members {
		if member != "" && member != myUserID {
			return member, true
		}
	}

	return "", false
}

func (m *MSTeams) IsDMChannelName(channelName string) bool {
	return m.isDMConversation(channelName)
}

func (m *MSTeams) GetTeamName(_ context.Context, teamID string) string {
	return m.mc.GetTeamName(teamID)
}

func (m *MSTeams) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	msgs, err := m.mc.GetMessagesSince(ctx, channelID, since)
	if err != nil {
		logger.Debugf("GetPostsSince(%s) failed: %v", channelID, err)

		return nil
	}

	return m.chatMessagesToEvents(ctx, msgs, "replay")
}

func (m *MSTeams) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	if limit <= 0 {
		limit = defaultScrollback
	}

	if limit > maxScrollback {
		limit = maxScrollback
	}

	msgs, err := m.mc.GetMessages(ctx, channelID, limit)
	if err != nil {
		logger.Debugf("GetPosts(%s) failed: %v", channelID, err)

		return nil
	}

	return m.chatMessagesToEvents(ctx, msgs, "scrollback")
}

func (m *MSTeams) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return m.GetPostsSince(ctx, channelID, since)
}

func (m *MSTeams) GetPostChannelID(_ context.Context, postID string) string {
	convID, _, ok := m.resolvePostID(postID)
	if !ok {
		return ""
	}

	return convID
}

func (m *MSTeams) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	convID, rawMsgID, ok := m.resolvePostID(postID)
	if !ok {
		return nil
	}

	msgs, err := m.mc.GetThreadMessages(ctx, convID, rawMsgID, defaultScrollback)
	if err != nil {
		logger.Debugf("GetPostThread(%s) failed: %v", postID, err)

		return nil
	}

	return m.chatMessagesToEvents(ctx, msgs, "thread")
}

func (m *MSTeams) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	msgs, err := m.mc.SearchMessages(ctx, search, 50)
	if err != nil {
		logger.Debugf("SearchPosts failed: %v", err)

		return nil
	}

	return m.chatMessagesToEvents(ctx, msgs, "search")
}

func (m *MSTeams) GetFilesInfo(_ context.Context, _ []string) []*bridge.File {
	return nil
}

func (m *MSTeams) IsChannelMember(channelID string) bool {
	return m.mc.IsChannelMember(channelID)
}

func (m *MSTeams) SendTyping(ctx context.Context, channelName string) {
	rc := m.cfg.Current()
	if !rc.EnableTyping || rc.DisableSendTyping || strings.HasPrefix(channelName, "&") {
		return
	}

	lastVal, ok := m.lastTypingSent.Load(channelName)
	if !ok {
		lastVal, _ = m.lastTypingSent.LoadOrStore(channelName, new(atomic.Int64))
	}

	lastPtr, ok := lastVal.(*atomic.Int64)
	if !ok {
		return
	}

	now := time.Now().Unix()
	last := lastPtr.Load()

	if now-last < 4 || !lastPtr.CompareAndSwap(last, now) {
		return
	}

	convID := m.resolveTypingTarget(ctx, channelName)
	if convID == "" {
		return
	}

	logger.Tracef("Sending +typing: %s (%s)", channelName, convID)

	_ = m.mc.SendTyping(ctx, convID)
}

func (m *MSTeams) resolveTypingTarget(ctx context.Context, target string) string {
	if pID, ok := strings.CutPrefix(target, "@@"); ok {
		convID, _, found := m.resolvePostID(pID)
		if found {
			return convID
		}

		return ""
	}

	if convID := m.conversationIDByName(target); convID != "" {
		return convID
	}

	if u := m.GetUserByUsername(ctx, target); u != nil {
		return m.GetDMChannelID(ctx, u.User)
	}

	return ""
}

func (m *MSTeams) chatMessagesToEvents(ctx context.Context, msgs []*msteamsclient.ChatMessage, eventKind string) []*bridge.Event {
	out := make([]*bridge.Event, 0, len(msgs))
	for _, msg := range msgs {
		if ev := m.chatMessageToEvent(ctx, msg, eventKind); ev != nil {
			out = append(out, ev)
		}
	}

	return out
}

func (m *MSTeams) rememberMessageConv(encodedID, convID string) {
	if encodedID != "" && convID != "" {
		m.msgConvCache.Add(encodedID, convID)
	}
}

func (m *MSTeams) resolvePostID(postID string) (string, string, bool) {
	if convID, rawMsgID, ok := parseTeamsMessageURL(postID); ok {
		return convID, rawMsgID, true
	}

	rawMsgID, fp, ok := decodeTeamsID(postID)
	if !ok {
		return "", "", false
	}

	norm := strings.ToLower(strings.TrimSpace(postID))
	if convID, found := m.msgConvCache.Get(norm); found && convID != "" {
		return convID, rawMsgID, true
	}

	// Zero-API fallback: match the 40-bit conversation fingerprint across cached channels.
	for _, conv := range m.mc.GetChannels() {
		if convFingerprint(conv.ID) == fp {
			m.rememberMessageConv(norm, conv.ID)

			return conv.ID, rawMsgID, true
		}
	}

	return "", "", false
}

func (m *MSTeams) conversationToChannelInfo(conv *msteamsclient.Conversation) *bridge.ChannelInfo {
	isDM := conv.Type == msteamsclient.ConversationDM || conv.Type == msteamsclient.ConversationSelfNote
	isPrivate := conv.IsPrivate || conv.Type == msteamsclient.ConversationGroup || conv.Type == msteamsclient.ConversationMeeting

	name := m.ircChannelName(conv.ID)
	if isDM {
		name = conv.ID
	}

	return &bridge.ChannelInfo{
		Name:        name,
		ID:          conv.ID,
		TeamID:      conv.TeamID,
		DM:          isDM,
		Private:     isPrivate,
		DisplayName: conv.DisplayName,
		LastPostAt:  conv.LastPostAt,
	}
}

func (m *MSTeams) resolveUserInfo(ctx context.Context, mri, fallbackDisplay string) *bridge.UserInfo {
	if mri == "" {
		return nil
	}

	u := m.mc.GetUser(mri)
	if u == nil {
		resolved, _ := m.mc.ResolveUsers(ctx, []string{mri})
		u = resolved[mri]
	}

	if u == nil {
		u = &msteamsclient.User{
			MRI:         mri,
			DisplayName: fallbackDisplay,
		}
		m.mc.CacheUser(u)

		if fallbackDisplay != "" {
			m.rebuildNames()
		}
	} else if u.DisplayName == "" && fallbackDisplay != "" {
		u.DisplayName = fallbackDisplay
		m.mc.CacheUser(u)
		m.rebuildNames()
	}

	return m.userToUserInfo(u)
}

func (m *MSTeams) userToUserInfo(u *msteamsclient.User) *bridge.UserInfo {
	nick := m.nickForMRI(u.MRI)

	realName := u.DisplayName
	if realName == "" {
		realName = nick
	}

	host := "msteams"
	if _, domain, ok := strings.Cut(u.Email, "@"); ok && domain != "" {
		host = domain
	}

	isMe := m.isOwnMRI(u.MRI)
	dirURL := utils.ExpandDirectoryURL(
		m.cfg.MSTeams().Bridge.DirectoryURL,
		nick,
		u.Email,
		u.Email,
		u.GivenName,
		u.Surname,
	)

	position := u.JobTitle
	if u.Location != "" {
		if position != "" {
			position = position + " (" + u.Location + ")"
		} else {
			position = u.Location
		}
	}

	return &bridge.UserInfo{
		Nick:        nick,
		User:        u.MRI,
		Real:        realName,
		Host:        host,
		DisplayName: realName,
		Ghost:       !isMe,
		Me:          isMe,
		Username:    nick,
		TeamID:      u.TenantID,
		FirstName:   u.GivenName,
		LastName:    u.Surname,
		Email:       u.Email,
		Directory:   dirURL,
		Position:    position,
	}
}

func (m *MSTeams) isOwnMRI(mri string) bool {
	if mri == "" {
		return false
	}

	me := m.mc.Me()

	return me != nil && me.MRI == mri
}
