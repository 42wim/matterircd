package googlechat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/matterbridge/googlechatclient"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
)

const (
	protocolName         = "googlechat"
	defaultPostSizeLimit = 4096
	defaultScrollback    = 60
	maxScrollback        = 200
	msgMetaCacheSize     = 4096
	lastSentSummaryLen   = 90
)

// CachedPost stores parent summary context matching bridge/mattermost.CachedPost.
type CachedPost struct {
	RootID     string
	ParentUser *bridge.UserInfo
	ReplyMsg   string
}

type msgMeta struct {
	convID  string
	topicID string
	isDM    bool
}

type resolvedPost struct {
	convID   string
	topicID  string
	rawMsgID string
	isDM     bool
}

type GoogleChat struct {
	gc          *googlechatclient.Client
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	connected   atomic.Bool

	done       chan struct{}
	logoutOnce sync.Once

	unregisterReloadHook func()

	dmChannelCache   *lru.Cache[string, string]
	msgParentCache   *lru.Cache[string, CachedPost]
	msgLastSentCache *lru.Cache[string, string]
	msgMetaCache     *lru.Cache[string, msgMeta]
	replayedMsgCache *lru.Cache[string, struct{}]

	namesMu     sync.RWMutex
	chanByID    map[string]string
	idByChan    map[string]string
	nickByID    map[string]string
	idByNick    map[string]string
	tokenSaveMu sync.Mutex

	cfg *config.Config

	//nolint:containedctx // Tied to bridge lifecycle, matching bridge/mattermost and bridge/msteams.
	ctx    context.Context
	cancel context.CancelFunc

	lastTypingSent sync.Map
}

var (
	logger     *logrus.Entry
	loggerOnce sync.Once
)

// New logs in to Google Chat and returns a bridge.Bridger plus the underlying
// googlechatclient.Client, matching bridge/mattermost.New and bridge/msteams.New.
//
//nolint:funlen
func New(
	ctx context.Context,
	cfg *config.Config,
	cred bridge.Credentials,
	eventChan chan *bridge.Event,
	onWsConnect func(),
	onWsDisconnect func(error),
	onAuthPrompt func(googlechatclient.AuthPrompt),
) (bridge.Bridger, *googlechatclient.Client, error) {
	rc := cfg.Current()

	loggerOnce.Do(func() {
		l := logrus.New()
		l.SetFormatter(&prefixed.TextFormatter{
			PrefixPadding: 13,
			DisableColors: false,
			FullTimestamp: true,
		})
		logger = l.WithFields(logrus.Fields{"prefix": "bridge/googlechat"})
	})

	ourlog := logger.Logger
	applyBridgeLogLevel(ourlog, rc)

	bridgeCtx, bridgeCancel := context.WithCancel(ctx)

	g := &GoogleChat{
		credentials: cred,
		eventChan:   eventChan,
		cfg:         cfg,
		done:        make(chan struct{}),
		ctx:         bridgeCtx,
		cancel:      bridgeCancel,
		chanByID:    make(map[string]string),
		idByChan:    make(map[string]string),
		nickByID:    make(map[string]string),
		idByNick:    make(map[string]string),
	}

	g.dmChannelCache, _ = lru.New[string, string](128)
	g.msgParentCache, _ = lru.New[string, CachedPost](256)
	g.msgLastSentCache, _ = lru.New[string, string](128)
	g.msgMetaCache, _ = lru.New[string, msgMeta](msgMetaCacheSize)
	g.replayedMsgCache, _ = lru.New[string, struct{}](1024)

	gc := g.buildClient(rc, cred, onWsConnect, onWsDisconnect, onAuthPrompt)

	setBridgeLogLevels := func(rc *config.RuntimeConfig) {
		applyBridgeLogLevel(ourlog, rc)
		applyClientLogLevels(gc, rc)
	}

	setBridgeLogLevels(rc)
	g.unregisterReloadHook = cfg.RegisterReloadHook(setBridgeLogLevels)

	err := gc.Login(bridgeCtx)
	if err != nil {
		g.unregisterReloadHook()
		bridgeCancel()

		return nil, nil, err
	}

	g.gc = gc
	g.connected.Store(true)
	g.resolveConversationMembers(bridgeCtx)
	g.rebuildNames()

	go g.handleGoogleChat()

	return g, gc, nil
}

func (g *GoogleChat) buildClient(
	rc *config.RuntimeConfig,
	cred bridge.Credentials,
	onWsConnect func(),
	onWsDisconnect func(error),
	onAuthPrompt func(googlechatclient.AuthPrompt),
) *googlechatclient.Client {
	clientCred := buildClientCredentials(rc, cred)

	gc := googlechatclient.New(clientCred)
	gc.UserAgent = config.UserAgent
	gc.Timeout = rc.ClientTimeout
	gc.ForceSyncOnReconnect = rc.GoogleChat.ForceSyncOnReconnect
	gc.PollFallback = rc.GoogleChat.PollFallback
	gc.OnAuthPrompt = onAuthPrompt
	gc.OnWsConnect = func() {
		if rc.GoogleChat.ForceSyncOnReconnect {
			_ = gc.UpdateChannels(g.ctx)
			g.rebuildNames()
		}

		if onWsConnect != nil {
			onWsConnect()
		}
	}
	gc.OnWsDisconnect = onWsDisconnect

	tokenFile := rc.GoogleChat.TokenSaveFile
	if tokenFile != "" {
		gc.OnTokenUpdate = func(state googlechatclient.TokenState) {
			g.tokenSaveMu.Lock()
			saveErr := saveTokenState(tokenFile, state)
			g.tokenSaveMu.Unlock()

			if saveErr != nil {
				logger.Warnf("Failed to persist Google Chat token state to %s: %v", tokenFile, saveErr)
			}
		}
	}

	return gc
}

func buildClientCredentials(rc *config.RuntimeConfig, cred bridge.Credentials) googlechatclient.Credentials {
	clientCred := googlechatclient.Credentials{
		AccountType:   parseAccountType(rc.GoogleChat.AccountType),
		AuthUser:      max(0, rc.GoogleChat.AuthUser),
		SkipTLSVerify: rc.GoogleChat.SkipTLSVerify,
	}

	if rc.GoogleChat.TokenSaveFile != "" {
		saved, err := loadTokenState(rc.GoogleChat.TokenSaveFile)
		if err == nil && saved != nil {
			clientCred.TokenState = saved
		} else if err != nil && !os.IsNotExist(err) {
			logger.Warnf("Failed to load Google Chat token state from %s: %v", rc.GoogleChat.TokenSaveFile, err)
		}
	}

	// Credentials.Server carries an account type ("work"/"personal"/"auto") or numeric AuthUser index.
	if server := strings.TrimSpace(cred.Server); server != "" {
		switch strings.ToLower(server) {
		case "work", "personal", "auto":
			clientCred.AccountType = parseAccountType(server)
		default:
			idx, err := strconv.Atoi(server)
			if err == nil && idx >= 0 {
				clientCred.AuthUser = idx
			}
		}
	}

	// Inline cookies override saved cookies when supplied via LOGIN cookie <cookies> or PASS.
	if tok := strings.TrimSpace(cred.Token); tok != "" {
		clientCred.RawCookies = tok
		if strings.EqualFold(strings.TrimSpace(cred.Login), "cookie") {
			clientCred.TokenState = nil
		}
	}

	return clientCred
}

func parseAccountType(raw string) googlechatclient.AccountType {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "work":
		return googlechatclient.AccountWork
	case "personal":
		return googlechatclient.AccountPersonal
	default:
		return googlechatclient.AccountAuto
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

func applyClientLogLevels(gc *googlechatclient.Client, rc *config.RuntimeConfig) {
	gc.SetLogLevel("info")

	if rc.GoogleChat.GooglechatclientLogLevel != "" {
		logger.Infof("enabling googlechatclient logging: level: %s", rc.GoogleChat.GooglechatclientLogLevel)
		gc.SetLogLevel(strings.ToLower(rc.GoogleChat.GooglechatclientLogLevel))
	}

	gc.SetLogAPICalls("error")

	if rc.Profiling {
		if rc.Trace {
			logger.Infof("enabling googlechatclient API logging: level: trace")
			gc.SetLogAPICalls("trace")
		} else if rc.Debug {
			logger.Infof("enabling googlechatclient API logging: level: warn")
			gc.SetLogAPICalls("warn")
		}
	}
}

// loadTokenState reads TokenSaveFile, accepting a googlechatclient.TokenState
// JSON snapshot, a browser DevTools JSON cookie array, a flat JSON object, or a
// raw "Cookie:" header string.
func loadTokenState(path string) (*googlechatclient.TokenState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var state googlechatclient.TokenState

	err = json.Unmarshal(data, &state)
	if err == nil && len(state.Cookies) > 0 {
		return &state, nil
	}

	cookies := googlechatclient.ParseCookies(string(data))
	if len(cookies) > 0 {
		return &googlechatclient.TokenState{
			Cookies: cookies,
		}, nil
	}

	return nil, errors.New("token file does not contain valid Google Chat cookies")
}

func saveTokenState(path string, state googlechatclient.TokenState) error {
	dir := filepath.Dir(path)

	err := os.MkdirAll(dir, 0o700)
	if err != nil {
		return err
	}

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

func (g *GoogleChat) Protocol() string {
	return protocolName
}

func (g *GoogleChat) Config() any {
	return &g.cfg.Current().GoogleChat
}

func (g *GoogleChat) BridgeConfig() *config.BridgeConfig {
	return &g.cfg.Current().GoogleChat.Bridge
}

func (g *GoogleChat) FormatterConfig() *config.FormatterConfig {
	return &g.cfg.Current().GoogleChat.Formatter
}

func (g *GoogleChat) Connected() bool {
	return g.connected.Load()
}

func (g *GoogleChat) GetPostSizeLimit() int {
	return defaultPostSizeLimit
}

func (g *GoogleChat) Logout(ctx context.Context) error {
	var err error

	g.logoutOnce.Do(func() {
		g.connected.Store(false)
		close(g.done)

		if g.unregisterReloadHook != nil {
			g.unregisterReloadHook()
		}

		if g.cancel != nil {
			g.cancel()
		}

		if g.gc != nil {
			err = g.gc.Logout(ctx)
		}

		g.eventChan <- &bridge.Event{
			Type: "logout",
			Data: &bridge.LogoutEvent{},
		}
	})

	return err
}

func (g *GoogleChat) Ping(ctx context.Context, _ ...string) error {
	if g.gc == nil {
		return errors.New("client not initialized")
	}

	return g.gc.Ping(ctx)
}

func (g *GoogleChat) NormalizePostID(msgID string) string {
	trimmed := strings.TrimSpace(msgID)
	if _, ok := decodeGoogleChatID(trimmed); ok {
		return trimmed
	}

	if isValidGoogleChatRawID(trimmed) {
		return encodeGoogleChatID(trimmed)
	}

	if convID, topicID, rawMsgID, isDM, ok := parseGoogleChatMessageURL(trimmed); ok {
		g.rememberMessageMeta(rawMsgID, convID, topicID, isDM)

		return encodeGoogleChatID(rawMsgID)
	}

	return ""
}

// PostURL implements the optional postURLFormatter interface used by /msg googlechat details.
func (g *GoogleChat) PostURL(_ context.Context, postID string, _ []*bridge.Event) string {
	rp, ok := g.resolvePostID(postID)
	if !ok {
		return ""
	}

	return googleChatMessageURL(g.gc.TokenState().AuthUser, rp.convID, rp.topicID, rp.rawMsgID, rp.isDM)
}

// ChannelUserPrefix implements bridge.ChannelRolePrefixer for Space owners/managers.
func (g *GoogleChat) ChannelUserPrefix(channelID, userID string) string {
	if g.gc.IsChannelAdmin(channelID, userID) {
		return "@"
	}

	return ""
}

func (g *GoogleChat) UpdateChannels(ctx context.Context) error {
	err := g.gc.UpdateChannels(ctx)
	if err != nil {
		return err
	}

	g.resolveConversationMembers(ctx)
	g.rebuildNames()

	return nil
}

func (g *GoogleChat) resolveConversationMembers(ctx context.Context) {
	seen := make(map[string]struct{})
	ids := make([]string, 0)

	for _, conv := range g.gc.GetChannels() {
		for _, uid := range conv.Members {
			if uid == "" {
				continue
			}

			if _, dup := seen[uid]; !dup {
				seen[uid] = struct{}{}
				ids = append(ids, uid)
			}
		}
	}

	if len(ids) > 0 {
		_, _ = g.gc.ResolveUsers(ctx, ids)
	}
}

func (g *GoogleChat) Invite(ctx context.Context, channelID, username string) error {
	u := g.GetUserByUsername(ctx, username)
	if u != nil {
		return g.gc.AddChannelMember(ctx, channelID, u.User)
	}

	if strings.Contains(username, "@") {
		return g.gc.AddChannelMember(ctx, channelID, username)
	}

	return fmt.Errorf("user %s not found", username)
}

func (g *GoogleChat) Join(ctx context.Context, channelName string) (string, string, error) {
	convID := g.conversationIDByName(channelName)
	if convID == "" {
		return "", "", fmt.Errorf("channel %s not found", channelName)
	}

	return convID, g.Topic(ctx, convID), nil
}

func (g *GoogleChat) List(_ context.Context) (map[string]string, error) {
	convs := g.gc.GetChannels()
	out := make(map[string]string, len(convs))

	for _, conv := range convs {
		if conv.Type == googlechatclient.ConversationDM || conv.Type == googlechatclient.ConversationSelfNote || conv.IsHidden {
			continue
		}

		name := "#" + g.ircChannelName(conv.ID)
		out[name] = conv.Topic
	}

	return out, nil
}

func (g *GoogleChat) Part(ctx context.Context, channel string) error {
	convID := g.conversationIDByName(channel)
	if convID == "" {
		convID = channel
	}

	if g.cfg.GoogleChat().Bridge.PartFake {
		return nil
	}

	return g.gc.LeaveChannel(ctx, convID)
}

func (g *GoogleChat) SetTopic(ctx context.Context, channelID, text string) error {
	return g.gc.SetChannelTopic(ctx, channelID, text)
}

func (g *GoogleChat) Topic(_ context.Context, channelID string) string {
	if conv := g.gc.GetChannel(channelID); conv != nil {
		return conv.Topic
	}

	return ""
}

func (g *GoogleChat) Kick(ctx context.Context, channelID, username string) error {
	u := g.GetUserByUsername(ctx, username)
	if u == nil {
		return fmt.Errorf("user %s not found", username)
	}

	return g.gc.RemoveChannelMember(ctx, channelID, u.User)
}

func (g *GoogleChat) Nick(_ context.Context, _ string) error {
	return errors.New("changing nick is not supported by Google Chat")
}

func (g *GoogleChat) MsgUser(ctx context.Context, userID, text string) (string, error) {
	conv, err := g.gc.GetOrCreateDM(ctx, []string{userID})
	if err != nil {
		return "", err
	}

	g.dmChannelCache.Add(userID, conv.ID)

	return g.MsgChannel(ctx, conv.ID, text)
}

func (g *GoogleChat) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	conv, err := g.gc.GetOrCreateDM(ctx, []string{userID})
	if err != nil {
		return "", err
	}

	g.dmChannelCache.Add(userID, conv.ID)

	return g.MsgChannelThread(ctx, conv.ID, parentID, text)
}

func (g *GoogleChat) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	sendText := g.prepareOutgoingText(text)

	msg, err := g.gc.SendMessage(ctx, channelID, sendText)
	if err != nil {
		return "", err
	}

	encoded := encodeGoogleChatID(msg.ID)
	g.recordSentPost(msg.ID, encoded, channelID, msg.TopicID, sendText)

	return encoded, nil
}

func (g *GoogleChat) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	topicID := parentID
	if rp, ok := g.resolvePostID(parentID); ok {
		if rp.topicID != "" {
			topicID = rp.topicID
		} else {
			topicID = rp.rawMsgID
		}
	} else if raw, ok := decodeGoogleChatID(parentID); ok {
		topicID = raw
	}

	sendText := g.prepareOutgoingText(text)

	msg, err := g.gc.SendMessageThread(ctx, channelID, topicID, sendText)
	if err != nil {
		return "", err
	}

	encoded := encodeGoogleChatID(msg.ID)
	g.recordSentPost(msg.ID, encoded, channelID, topicID, sendText)

	return encoded, nil
}

func (g *GoogleChat) prepareOutgoingText(text string) string {
	sendText := googleChatActionText(text)
	fc := g.cfg.GoogleChat().Formatter

	if !fc.DisableEmoji {
		sendText = utils.EmojiReplaceAliases(sendText, fc.CustomEmoji)
	}

	return sendText
}

func googleChatActionText(text string) string {
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		return "*" + strings.TrimSuffix(after, "\x01") + "*"
	}

	return text
}

func (g *GoogleChat) recordSentPost(rawID, encodedID, channelID, topicID, text string) {
	if encodedID == "" {
		return
	}

	g.rememberMessageMeta(rawID, channelID, topicID, g.isDMConversation(channelID))
	g.msgLastSentCache.Add(encodedID, g.formatSummary(text, lastSentSummaryLen))
	g.cachePostContext(encodedID, "", g.GetMe(), text, g.cfg.GoogleChat().ShortenRepliesTo)
}

func (g *GoogleChat) GetLastSentMsgs() []string {
	keys := g.msgLastSentCache.Keys()
	data := make([]string, 0, len(keys))

	for _, k := range keys {
		if msg, ok := g.msgLastSentCache.Get(k); ok {
			data = append(data, "[@@"+k+"] "+msg)
		}
	}

	return data
}

func (g *GoogleChat) ModifyPost(ctx context.Context, msgID, text string) error {
	rp, ok := g.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	if text == "" {
		return g.gc.DeleteMessage(ctx, rp.convID, rp.topicID, rp.rawMsgID)
	}

	_, err := g.gc.EditMessage(ctx, rp.convID, rp.topicID, rp.rawMsgID, g.prepareOutgoingText(text))

	return err
}

func (g *GoogleChat) AddReaction(ctx context.Context, msgID, emoji string) error {
	rp, ok := g.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	key := ircEmojiToGoogleChatEmoji(emoji, g.cfg.GoogleChat().Formatter.CustomEmoji)

	return g.gc.AddReaction(ctx, rp.convID, rp.topicID, rp.rawMsgID, key)
}

func (g *GoogleChat) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	rp, ok := g.resolvePostID(msgID)
	if !ok {
		return fmt.Errorf("message %s not found", msgID)
	}

	key := ircEmojiToGoogleChatEmoji(emoji, g.cfg.GoogleChat().Formatter.CustomEmoji)

	return g.gc.RemoveReaction(ctx, rp.convID, rp.topicID, rp.rawMsgID, key)
}

func (g *GoogleChat) HasCustomStatus(userID string) bool {
	return g.gc.GetStatusNote(g.ctx, userID) != ""
}

func (g *GoogleChat) IsOnline(status string) bool {
	if status == "online" {
		return true
	}

	return strings.HasPrefix(status, "online (") && !strings.Contains(status, "): ")
}

func (g *GoogleChat) StatusUser(ctx context.Context, userID string) (string, error) {
	p, _ := g.gc.GetPresenceFresh(ctx, userID)
	if p == nil {
		return g.gc.GetStatus(ctx, userID), nil
	}

	status := googlechatclient.MapAvailabilityToStatus(p)

	note := googlechatclient.FormatStatusNote(p)
	if note != "" {
		return fmt.Sprintf("%s: %s", status, note), nil
	}

	return status, nil
}

func (g *GoogleChat) StatusUsers(ctx context.Context) (map[string]string, error) {
	users := g.gc.GetUsers()
	ids := make([]string, 0, len(users))

	for _, u := range users {
		if u.ID != "" && !u.IsBot {
			ids = append(ids, u.ID)
		}
	}

	if len(ids) == 0 {
		return make(map[string]string), nil
	}

	return g.gc.GetStatuses(ctx, ids), nil
}

func (g *GoogleChat) SetStatus(ctx context.Context, status string) error {
	return g.gc.SetPresence(ctx, status)
}

func (g *GoogleChat) CreateChannel(ctx context.Context, channelName, _ string) (*bridge.ChannelInfo, error) {
	name := strings.TrimPrefix(channelName, "#")

	conv, err := g.gc.CreateSpace(ctx, name)
	if err != nil {
		return nil, err
	}

	g.rebuildNames()

	return g.conversationToChannelInfo(conv), nil
}

func (g *GoogleChat) GetChannels() []*bridge.ChannelInfo {
	convs := g.gc.GetChannels()
	out := make([]*bridge.ChannelInfo, 0, len(convs))

	for _, conv := range convs {
		if conv.IsHidden {
			continue
		}

		out = append(out, g.conversationToChannelInfo(conv))
	}

	return out
}

func (g *GoogleChat) GetChannel(_ context.Context, channelID string) (*bridge.ChannelInfo, error) {
	conv := g.gc.GetChannel(channelID)
	if conv == nil {
		return nil, fmt.Errorf("channel %s not found", channelID)
	}

	return g.conversationToChannelInfo(conv), nil
}

func (g *GoogleChat) GetChannelCount() int {
	return g.gc.GetChannelCount()
}

func (g *GoogleChat) GetChannelName(_ context.Context, channelID string) string {
	if channelID == "" || strings.HasPrefix(channelID, "&") || strings.HasPrefix(channelID, "#") {
		return channelID
	}

	if g.isDMConversation(channelID) {
		return channelID
	}

	return "#" + g.ircChannelName(channelID)
}

func (g *GoogleChat) GetChannelID(_ context.Context, name, _ string) string {
	return g.conversationIDByName(name)
}

func (g *GoogleChat) GetLastViewedAt(_ context.Context, channelID string) int64 {
	return g.gc.GetChannelLastViewedAt(channelID)
}

func (g *GoogleChat) UpdateLastViewed(ctx context.Context, channelID string) {
	if channelID == "" {
		return
	}

	err := g.gc.MarkConversationRead(ctx, channelID, time.Now())
	if err != nil {
		logger.Debugf("UpdateLastViewed failed for %s: %v", channelID, err)
	}
}

func (g *GoogleChat) UpdateLastViewedUser(ctx context.Context, userID string) error {
	convID := g.GetDMChannelID(ctx, userID)
	if convID == "" {
		return nil
	}

	return g.gc.MarkConversationRead(ctx, convID, time.Now())
}

func (g *GoogleChat) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	members := g.gc.GetChannelMembers(channelID)
	if len(members) == 0 {
		conv, err := g.gc.SyncConversation(ctx, channelID)
		if err == nil && conv != nil {
			members = conv.Members
		}
	}

	if len(members) > 0 {
		_, _ = g.gc.ResolveUsers(ctx, members)
	}

	users := make([]*bridge.UserInfo, 0, len(members))

	for _, uid := range members {
		if info := g.resolveUserInfo(ctx, uid, ""); info != nil {
			users = append(users, info)
		}
	}

	return users, nil
}

func (g *GoogleChat) GetUsers() []*bridge.UserInfo {
	rawUsers := g.gc.GetUsers()
	out := make([]*bridge.UserInfo, 0, len(rawUsers))

	for _, u := range rawUsers {
		out = append(out, g.userToUserInfo(u))
	}

	return out
}

func (g *GoogleChat) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return g.resolveUserInfo(ctx, userID, "")
}

func (g *GoogleChat) GetUserChannels(_ context.Context, userID string) ([]string, error) {
	convs := g.gc.GetChannels()
	out := make([]string, 0)

	for _, conv := range convs {
		if conv.IsHidden || conv.Type == googlechatclient.ConversationDM || conv.Type == googlechatclient.ConversationSelfNote {
			continue
		}

		if slices.Contains(conv.Members, userID) {
			out = append(out, g.ircChannelName(conv.ID))
		}
	}

	return out, nil
}

func (g *GoogleChat) GetUserCount() int {
	return g.gc.GetUserCount()
}

func (g *GoogleChat) GetMe() *bridge.UserInfo {
	me := g.gc.Me()
	if me == nil {
		return &bridge.UserInfo{Me: true}
	}

	info := g.userToUserInfo(me)
	info.Me = true
	info.Ghost = false

	return info
}

func (g *GoogleChat) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	if username == "" {
		return nil
	}

	if uid := g.userIDForNick(username); uid != "" {
		return g.resolveUserInfo(ctx, uid, "")
	}

	if u := g.gc.GetUser(username); u != nil {
		return g.userToUserInfo(u)
	}

	if u := g.gc.GetUserByEmail(username); u != nil {
		return g.userToUserInfo(u)
	}

	return nil
}

func (g *GoogleChat) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	users, err := g.gc.SearchUsers(ctx, query, 50)
	if err != nil {
		return nil, err
	}

	out := make([]*bridge.UserInfo, 0, len(users))
	for _, u := range users {
		out = append(out, g.userToUserInfo(u))
	}

	return out, nil
}

func (g *GoogleChat) GetDMChannelID(_ context.Context, userID string) string {
	if cached, ok := g.dmChannelCache.Get(userID); ok {
		return cached
	}

	convID := g.gc.GetDMConversationID(userID)
	if convID != "" {
		g.dmChannelCache.Add(userID, convID)
	}

	return convID
}

func (g *GoogleChat) GetDMChannelName(_, userID2 string) string {
	return g.gc.GetDMConversationID(userID2)
}

func (g *GoogleChat) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	me := g.gc.Me()

	myID := ""
	if me != nil {
		myID = me.ID
	}

	otherID, ok := g.GetDMOtherUserID(channelName, myID)
	if !ok {
		return nil
	}

	return g.resolveUserInfo(ctx, otherID, "")
}

func (g *GoogleChat) GetDMUserIDs(channelName string) (string, string, bool) {
	me := g.gc.Me()
	if me == nil {
		return "", "", false
	}

	other, ok := g.GetDMOtherUserID(channelName, me.ID)
	if !ok {
		return "", "", false
	}

	return me.ID, other, true
}

func (g *GoogleChat) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	conv := g.gc.GetChannel(channelName)
	if conv == nil {
		return "", false
	}

	if conv.Type == googlechatclient.ConversationSelfNote {
		return myUserID, myUserID != ""
	}

	if conv.Type != googlechatclient.ConversationDM {
		return "", false
	}

	for _, member := range conv.Members {
		if member != "" && member != myUserID {
			return member, true
		}
	}

	return "", false
}

func (g *GoogleChat) IsDMChannelName(channelName string) bool {
	return g.isDMConversation(channelName)
}

func (g *GoogleChat) GetTeamName(_ context.Context, _ string) string {
	return ""
}

func (g *GoogleChat) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	msgs, err := g.gc.GetMessagesSince(ctx, channelID, time.UnixMilli(since), defaultScrollback)
	if err != nil {
		logger.Debugf("GetPostsSince(%s) failed: %v", channelID, err)

		return nil
	}

	for _, msg := range msgs {
		if msg != nil && msg.ID != "" {
			g.replayedMsgCache.Add(msg.ID, struct{}{})
		}
	}

	return g.chatMessagesToEvents(ctx, msgs, "replay")
}

func (g *GoogleChat) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	if limit <= 0 {
		limit = defaultScrollback
	}

	if limit > maxScrollback {
		limit = maxScrollback
	}

	msgs, err := g.gc.GetMessages(ctx, channelID, limit)
	if err != nil {
		logger.Debugf("GetPosts(%s) failed: %v", channelID, err)

		return nil
	}

	return g.chatMessagesToEvents(ctx, msgs, "scrollback")
}

func (g *GoogleChat) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return g.GetPostsSince(ctx, channelID, since)
}

func (g *GoogleChat) GetPostChannelID(_ context.Context, postID string) string {
	rp, ok := g.resolvePostID(postID)
	if !ok {
		return ""
	}

	return rp.convID
}

func (g *GoogleChat) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	rp, ok := g.resolvePostID(postID)
	if !ok {
		return nil
	}

	topicID := rp.topicID
	if topicID == "" {
		topicID = rp.rawMsgID
	}

	msgs, err := g.gc.GetThreadMessages(ctx, rp.convID, topicID, defaultScrollback)
	if err != nil {
		logger.Debugf("GetPostThread(%s) failed: %v", postID, err)

		return nil
	}

	return g.chatMessagesToEvents(ctx, msgs, "thread")
}

func (g *GoogleChat) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	msgs, err := g.gc.SearchMessages(ctx, search, 50)
	if err != nil {
		logger.Debugf("SearchPosts failed: %v", err)

		return nil
	}

	return g.chatMessagesToEvents(ctx, msgs, "search")
}

func (g *GoogleChat) GetFilesInfo(_ context.Context, _ []string) []*bridge.File {
	return nil
}

func (g *GoogleChat) IsChannelMember(channelID string) bool {
	me := g.gc.Me()
	if me == nil {
		return false
	}

	return g.gc.IsChannelMember(channelID, me.ID)
}

func (g *GoogleChat) SendTyping(ctx context.Context, channelName string) {
	rc := g.cfg.Current()
	if !rc.EnableTyping || rc.DisableSendTyping || strings.HasPrefix(channelName, "&") {
		return
	}

	lastVal, ok := g.lastTypingSent.Load(channelName)
	if !ok {
		lastVal, _ = g.lastTypingSent.LoadOrStore(channelName, new(atomic.Int64))
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

	convID, topicID := g.resolveTypingTarget(ctx, channelName)
	if convID == "" {
		return
	}

	logger.Tracef("Sending +typing: %s (%s)", channelName, convID)

	_ = g.gc.SendTyping(ctx, convID, topicID)
}

func (g *GoogleChat) resolveTypingTarget(ctx context.Context, target string) (string, string) {
	if pID, ok := strings.CutPrefix(target, "@@"); ok {
		rp, found := g.resolvePostID(pID)
		if found {
			topicID := rp.topicID
			if topicID == "" {
				topicID = rp.rawMsgID
			}

			return rp.convID, topicID
		}

		return "", ""
	}

	if convID := g.conversationIDByName(target); convID != "" {
		return convID, ""
	}

	if u := g.GetUserByUsername(ctx, target); u != nil {
		return g.GetDMChannelID(ctx, u.User), ""
	}

	return "", ""
}

func (g *GoogleChat) chatMessagesToEvents(ctx context.Context, msgs []*googlechatclient.ChatMessage, eventKind string) []*bridge.Event {
	out := make([]*bridge.Event, 0, len(msgs))
	for _, msg := range msgs {
		if ev := g.chatMessageToEvent(ctx, msg, eventKind); ev != nil {
			out = append(out, ev)
		}
	}

	return out
}

func (g *GoogleChat) rememberMessageMeta(rawID, convID, topicID string, isDM bool) {
	if rawID == "" || convID == "" {
		return
	}

	if topicID == "" {
		topicID = rawID
	}

	g.msgMetaCache.Add(rawID, msgMeta{
		convID:  convID,
		topicID: topicID,
		isDM:    isDM,
	})
}

func (g *GoogleChat) resolvePostID(postID string) (resolvedPost, bool) {
	if convID, topicID, rawMsgID, isDM, ok := parseGoogleChatMessageURL(postID); ok {
		g.rememberMessageMeta(rawMsgID, convID, topicID, isDM)

		return resolvedPost{
			convID:   convID,
			topicID:  topicID,
			rawMsgID: rawMsgID,
			isDM:     isDM,
		}, true
	}

	rawMsgID, ok := decodeGoogleChatID(postID)
	if !ok {
		trimmed := strings.TrimSpace(postID)
		if !isValidGoogleChatRawID(trimmed) {
			return resolvedPost{}, false
		}

		rawMsgID = trimmed
	}

	if meta, found := g.msgMetaCache.Get(rawMsgID); found && meta.convID != "" {
		return resolvedPost{
			convID:   meta.convID,
			topicID:  meta.topicID,
			rawMsgID: rawMsgID,
			isDM:     meta.isDM,
		}, true
	}

	if cached := g.gc.GetCachedPost(rawMsgID); cached != nil && cached.ConversationID != "" {
		isDM := g.isDMConversation(cached.ConversationID)
		g.rememberMessageMeta(rawMsgID, cached.ConversationID, cached.TopicID, isDM)

		return resolvedPost{
			convID:   cached.ConversationID,
			topicID:  cached.TopicID,
			rawMsgID: rawMsgID,
			isDM:     isDM,
		}, true
	}

	return resolvedPost{}, false
}

func (g *GoogleChat) conversationToChannelInfo(conv *googlechatclient.Conversation) *bridge.ChannelInfo {
	isDM := conv.Type == googlechatclient.ConversationDM || conv.Type == googlechatclient.ConversationSelfNote
	isPrivate := conv.IsPrivate || conv.Type == googlechatclient.ConversationGroupDM

	name := g.ircChannelName(conv.ID)
	if isDM {
		name = conv.ID
	}

	return &bridge.ChannelInfo{
		Name:        name,
		ID:          conv.ID,
		DM:          isDM,
		Private:     isPrivate,
		DisplayName: conv.DisplayName,
		LastPostAt:  conv.LastPostAt,
	}
}

func (g *GoogleChat) resolveUserInfo(ctx context.Context, userID, fallbackDisplay string) *bridge.UserInfo {
	if userID == "" {
		return nil
	}

	u := g.gc.GetUser(userID)
	if u == nil {
		resolved, _ := g.gc.ResolveUsers(ctx, []string{userID})
		u = resolved[userID]
	}

	if u == nil {
		u = &googlechatclient.User{
			ID:          userID,
			DisplayName: fallbackDisplay,
		}
		g.gc.CacheUser(u)

		if fallbackDisplay != "" {
			g.rebuildNames()
		}
	} else if u.DisplayName == "" && fallbackDisplay != "" {
		u.DisplayName = fallbackDisplay
		g.gc.CacheUser(u)
		g.rebuildNames()
	}

	return g.userToUserInfo(u)
}

func (g *GoogleChat) userToUserInfo(u *googlechatclient.User) *bridge.UserInfo {
	nick := g.nickForUserID(u.ID)

	realName := u.DisplayName
	if realName == "" {
		realName = nick
	}

	host := "googlechat"
	if _, domain, ok := strings.Cut(u.Email, "@"); ok && domain != "" {
		host = domain
	}

	isMe := g.isOwnUserID(u.ID)
	dirURL := utils.ExpandDirectoryURL(
		g.cfg.GoogleChat().Bridge.DirectoryURL,
		nick,
		u.Email,
		u.Email,
		u.FirstName,
		u.LastName,
	)

	return &bridge.UserInfo{
		Nick:        nick,
		User:        u.ID,
		Real:        realName,
		Host:        host,
		DisplayName: realName,
		Ghost:       !isMe,
		Me:          isMe,
		Username:    nick,
		TeamID:      u.OrganizationID,
		FirstName:   u.FirstName,
		LastName:    u.LastName,
		Email:       u.Email,
		Directory:   dirURL,
		Photo:       u.AvatarURL,
	}
}

func (g *GoogleChat) isOwnUserID(userID string) bool {
	if userID == "" {
		return false
	}

	me := g.gc.Me()

	return me != nil && me.ID == userID
}
