package slack

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	slackLastSentSummaryLen  = 90

	subTypeMessageChanged = "message_changed"
	subTypeMessageDeleted = "message_deleted"
	subTypePinAdded       = "pin_added"
	subTypePinRemoved     = "pin_removed"
	eventChannelTopic     = "channel_topic"
	eventGroupTopic       = "group_topic"
	eventChannelPurpose   = "channel_purpose"
	eventGroupPurpose     = "group_purpose"
)

type slackCachedPost struct {
	rootTS  string
	sender  *bridge.UserInfo
	message string
}

type Slack struct {
	sync.RWMutex

	sc                   *slackclient.Client
	connected            atomic.Bool
	credentials          bridge.Credentials
	eventChan            chan *bridge.Event
	done                 chan struct{}
	onConnect            func()
	onWsError            func(error)
	wasDisconnected      atomic.Bool
	disconnectedAt       atomic.Int64
	unregisterReloadHook func()
	logoutOnce           sync.Once
	wsCancel             context.CancelFunc
	userByNick           map[string]string
	msgThreadRootCache   *lru.Cache[string, string]
	msgParentCache       *lru.Cache[string, slackCachedPost]
	msgLastSentCache     *lru.Cache[string, string]
	lastTypingSent       sync.Map

	cfg *config.Config
}

const systemUser = "system"

var logger = newLogger()

func newLogger() *logrus.Entry {
	ourlog := logrus.New()
	ourlog.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	return ourlog.WithFields(logrus.Fields{"prefix": "bridge/slack"})
}

func New(cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func(), onWsError ...func(error)) (bridge.Bridger, error) {
	threadRootCache, _ := lru.New[string, string](slackThreadRootCacheSize)
	parentCache, _ := lru.New[string, slackCachedPost](slackParentCacheSize)
	lastSentCache, _ := lru.New[string, string](slackLastSentCacheSize)

	var wsErrFn func(error)
	if len(onWsError) > 0 {
		wsErrFn = onWsError[0]
	}

	s := &Slack{
		credentials:        cred,
		eventChan:          eventChan,
		done:               make(chan struct{}),
		onConnect:          onConnect,
		onWsError:          wsErrFn,
		userByNick:         make(map[string]string),
		msgThreadRootCache: threadRootCache,
		msgParentCache:     parentCache,
		msgLastSentCache:   lastSentCache,
		cfg:                cfg,
	}

	rc := cfg.Current()
	ourlog := logger.Logger

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
	s.unregisterReloadHook = cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
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

func isSlackIDWithPrefix(id string, prefixes ...byte) bool {
	if len(id) < 2 {
		return false
	}

	first := id[0]
	if first >= 'a' && first <= 'z' {
		if len(id) < 9 {
			return false
		}

		first -= 'a' - 'A'
	}

	if !slices.Contains(prefixes, first) {
		return false
	}

	hasDigit := false

	for i := 1; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z' && len(id) >= 9:
		default:
			return false
		}
	}

	return hasDigit
}

func (s *Slack) sendEvent(ev *bridge.Event) {
	if s.eventChan == nil || ev == nil {
		return
	}

	select {
	case s.eventChan <- ev:
	case <-s.done:
	}
}

func (s *Slack) Invite(ctx context.Context, channelID, username string) error {
	if !isSlackIDWithPrefix(username, 'U', 'W') {
		if u := s.GetUserByUsername(ctx, username); u != nil && u.User != "" {
			username = u.User
		}
	}

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
		if channel.IsIM || channel.IsMpIM || channel.Name == "" {
			continue
		}

		channelinfo["#"+channel.Name] = strings.ReplaceAll(channel.Topic.Value, "\n", " | ")
	}

	return channelinfo, nil
}

func (s *Slack) Part(ctx context.Context, channelID string) error {
	return s.sc.LeaveChannel(channelID)
}

func (s *Slack) UpdateChannels(ctx context.Context) error {
	_, err := s.sc.RefreshConversations()
	return err
}

func (s *Slack) Logout(ctx context.Context) error {
	var logoutErr error

	s.logoutOnce.Do(func() {
		logoutErr = s.logout(ctx)
	})

	return logoutErr
}

func (s *Slack) logout(ctx context.Context) error {
	s.connected.Store(false)

	if s.done != nil {
		defer close(s.done)
	}

	if s.unregisterReloadHook != nil {
		s.unregisterReloadHook()
	}

	if s.wsCancel != nil {
		s.wsCancel()
	}

	var err error
	if s.sc != nil {
		err = s.sc.Logout()
	}

	// The event loop may never have started, or it ended already when the user disconnected
	// (ctx is canceled then), so don't wait for it forever.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	if s.eventChan != nil {
		select {
		case s.eventChan <- &bridge.Event{Type: "logout", Data: &bridge.LogoutEvent{}}:
		case <-ctx.Done():
		case <-timer.C:
		}
	}

	return err
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

const (
	statusOnline = "online"
	statusAway   = "away"
)

func (s *Slack) HasCustomStatus(userID string) bool {
	return s.sc.GetUserCustomStatus(userID) != ""
}

func (s *Slack) IsOnline(status string) bool {
	if status == statusOnline || status == "active" {
		return true
	}

	return (strings.HasPrefix(status, statusOnline+" (") || strings.HasPrefix(status, "active (")) && !strings.Contains(status, "): ")
}

func normalizeSlackPresence(presence string) string {
	switch presence {
	case "active":
		return statusOnline
	case statusAway:
		return statusAway
	default:
		return presence
	}
}

func formatSlackUserStatus(prefix, localTime, custom string, lastActivity int64) string {
	var lastOnline string

	if lastActivity > 0 {
		diff := max(0, time.Now().Unix()-lastActivity)
		if prefix == statusOnline {
			lastOnline = utils.FormatLastOnline(lastActivity, true)
		} else if diff >= 20*60 {
			lastOnline = utils.FormatLastOnline(lastActivity)
		}
	}

	var details string

	switch {
	case lastOnline != "" && localTime != "":
		details = lastOnline + ", local time: " + localTime
	case lastOnline != "":
		details = lastOnline
	case localTime != "":
		details = "local time: " + localTime
	}

	status := prefix
	if details != "" {
		status += " (" + details + ")"
	}

	if custom != "" {
		status += ": " + custom
	}

	return status
}

func (s *Slack) StatusUser(ctx context.Context, userID string) (string, error) {
	if userID == "" {
		return "", nil
	}

	up, err := s.sc.GetUserPresence(ctx, userID)
	if err != nil {
		return "", err
	}

	if up == nil {
		return "", nil
	}

	prefix := normalizeSlackPresence(up.Presence)
	if prefix == "" {
		if up.Online {
			prefix = statusOnline
		} else {
			prefix = statusAway
		}
	}

	localTime := s.sc.GetUserLocalTime(userID)
	custom := s.sc.GetUserCustomStatus(userID)

	return formatSlackUserStatus(prefix, localTime, custom, int64(up.LastActivity)), nil
}

func (s *Slack) StatusUsers(ctx context.Context) (map[string]string, error) {
	return s.sc.GetStatuses(), nil
}

func (s *Slack) Protocol() string {
	return "slack"
}

func (s *Slack) Kick(ctx context.Context, channelID, username string) error {
	if !isSlackIDWithPrefix(username, 'U', 'W') {
		if u := s.GetUserByUsername(ctx, username); u != nil && u.User != "" {
			username = u.User
		}
	}

	return s.sc.Kick(channelID, username)
}

func (s *Slack) SetStatus(ctx context.Context, status string) error {
	return s.sc.SetStatus(status)
}

func (s *Slack) Nick(ctx context.Context, name string) error {
	err := s.sc.SetNick(ctx, name)
	if err != nil {
		return err
	}

	if me := s.GetMe(); me != nil && me.User != "" {
		s.sendEvent(&bridge.Event{
			Type: "user_update",
			Data: &bridge.UserUpdateEvent{
				User: me,
			},
		})
	}

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

	if isSlackIDWithPrefix(userID, 'D') {
		return strings.ToUpper(userID)
	}

	if !isSlackIDWithPrefix(userID, 'U', 'W') {
		if u := s.GetUserByUsername(ctx, userID); u != nil && u.User != "" {
			userID = u.User
		}
	}

	chID, err := s.sc.OpenDMChannel(ctx, strings.ToUpper(userID))
	if err != nil {
		logger.Debugf("GetDMChannelID %s: %s", userID, err)
		return ""
	}

	return chID
}

func (s *Slack) GetDMChannelName(userID1 string, userID2 string) string {
	if userID2 != "" && userID2 != s.sc.UserID() {
		return userID2
	}

	return userID1
}

func (s *Slack) resolveDMPeer(channelName string) *bridge.UserInfo {
	if !s.IsDMChannelName(channelName) || s.sc == nil {
		return nil
	}

	otherUserID, ok := s.GetDMOtherUserID(channelName, s.sc.UserID())
	if !ok || otherUserID == "" {
		return nil
	}

	if u := s.sc.GetUser(otherUserID); u != nil {
		return s.createUser(u)
	}

	u, err := s.sc.GetUserInfo(otherUserID)
	if err == nil && u != nil {
		return s.createUser(u)
	}

	return nil
}

func (s *Slack) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	return s.resolveDMPeer(channelName)
}

func (s *Slack) GetDMUserIDs(channelName string) (string, string, bool) {
	if s.sc == nil {
		return "", "", false
	}

	myID := s.sc.UserID()

	otherID, ok := s.GetDMOtherUserID(channelName, myID)
	if !ok {
		return "", "", false
	}

	return myID, otherID, true
}

func (s *Slack) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	if s.sc == nil {
		return "", false
	}

	return s.sc.GetDMOtherUserID(channelName, myUserID)
}

func (s *Slack) GetUsers() []*bridge.UserInfo {
	susers := s.sc.GetUsers()
	users := make([]*bridge.UserInfo, 0, len(susers))

	for i := range susers {
		users = append(users, s.createUser(&susers[i]))
	}

	return users
}

func slackChannelToInfo(teamID string, ch *slack.Channel) *bridge.ChannelInfo {
	if ch == nil {
		return nil
	}

	var lastPostAt int64
	if ch.Latest != nil && ch.Latest.Timestamp != "" {
		lastPostAt = slackTSMillis(ch.Latest.Timestamp)
	}

	name := ch.Name
	if (ch.IsIM || isSlackIDWithPrefix(ch.ID, 'D')) && name == "" {
		name = ch.ID
	}

	return &bridge.ChannelInfo{
		Name:        name,
		ID:          ch.ID,
		TeamID:      teamID,
		DM:          ch.IsIM || ch.IsMpIM,
		Private:     ch.IsPrivate || ch.IsGroup,
		DisplayName: ch.Name,
		LastPostAt:  lastPostAt,
	}
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	teamID := s.sc.TeamID()

	for _, mmchannel := range s.sc.GetChannels() {
		channels = append(channels, slackChannelToInfo(teamID, &mmchannel))
	}

	return channels
}

func (s *Slack) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	isPrivate := strings.EqualFold(channelType, "private") || strings.EqualFold(channelType, "P")

	ch, err := s.sc.CreateChannel(ctx, strings.TrimPrefix(channelName, "#"), isPrivate)
	if err != nil {
		return nil, err
	}

	return &bridge.ChannelInfo{
		ID:          ch.ID,
		Name:        ch.Name,
		DisplayName: ch.Name,
		TeamID:      s.sc.TeamID(),
		Private:     ch.IsPrivate,
	}, nil
}

func (s *Slack) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	if channelID == "" || strings.HasPrefix(channelID, "&") {
		return nil, errors.New("invalid channel id")
	}

	if strings.HasPrefix(channelID, "#") {
		chID := s.GetChannelID(ctx, channelID, "")
		if chID == "" {
			return nil, errors.New("channel not found")
		}

		channelID = chID
	}

	channels := s.GetChannels()
	for _, channel := range channels {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	if s.sc != nil {
		ch, err := s.sc.GetConversationInfo(ctx, channelID)
		if err == nil && ch != nil {
			return slackChannelToInfo(s.sc.TeamID(), ch), nil
		}
	}

	return nil, errors.New("channel not found")
}

func (s *Slack) GetChannelCount() int {
	return s.sc.GetChannelCount()
}

func (s *Slack) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return s.createUser(s.sc.GetUser(userID))
}

func isPublicSlackChannel(ch slack.Channel) bool {
	return !ch.IsPrivate && !ch.IsGroup && !ch.IsIM && !ch.IsMpIM
}

func (s *Slack) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return []string{}, nil
	}

	if userID == s.sc.UserID() {
		chs := s.sc.GetChannels()
		names := make([]string, 0, len(chs))

		for _, ch := range chs {
			if isPublicSlackChannel(ch) {
				names = append(names, ch.Name)
			}
		}

		return names, nil
	}

	return s.sc.GetUserChannels(ctx, userID)
}

func (s *Slack) GetUserCount() int {
	return s.sc.GetUserCount()
}

func (s *Slack) GetMe() *bridge.UserInfo {
	return s.createUser(s.sc.GetMe())
}

func (s *Slack) lookupUserIDByNick(nick string) string {
	if nick == "" {
		return ""
	}

	s.RLock()
	defer s.RUnlock()

	if uid, ok := s.userByNick[nick]; ok {
		return uid
	}

	for k, uid := range s.userByNick {
		if strings.EqualFold(k, nick) {
			return uid
		}
	}

	return ""
}

func (s *Slack) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	if uid := s.lookupUserIDByNick(username); uid != "" {
		if u := s.sc.GetUser(uid); u != nil {
			return s.createUser(u)
		}
	}

	u := s.sc.GetUserByUsername(username)
	if u == nil {
		return nil
	}

	return s.createUser(u)
}

func (s *Slack) GetTeamName(ctx context.Context, teamID string) string {
	return s.sc.TeamName()
}

func (s *Slack) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	info, err := s.sc.GetConversationInfo(ctx, channelID)
	if err != nil || info == nil || info.LastRead == "" {
		return 0
	}

	return slackTSMillis(info.LastRead)
}

func (s *Slack) msgsToEvents(ctx context.Context, channelID string, msgs []slack.Message) []*bridge.Event {
	events := make([]*bridge.Event, 0, len(msgs))
	for _, m := range msgs {
		if ev := s.slackMsgToEvent(ctx, channelID, m.Msg); ev != nil {
			events = append(events, ev)
		}
	}

	return events
}

func (s *Slack) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	if channelID == "" {
		return []*bridge.Event{}
	}

	oldest := ""
	if since > 0 {
		oldest = fmt.Sprintf("%.6f", float64(since)/1000.0)
	}

	msgs, err := s.sc.GetConversationHistory(ctx, channelID, oldest, 0)
	if err != nil {
		logger.Errorf("slack: GetPostsSince %s: %s", channelID, err)
		return []*bridge.Event{}
	}

	return s.msgsToEvents(ctx, channelID, msgs)
}

func (s *Slack) IsDMChannelName(channelName string) bool {
	return isSlackIDWithPrefix(channelName, 'D')
}

func (s *Slack) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	matches, err := s.sc.SearchMessages(ctx, search, 100)
	if err != nil {
		logger.Errorf("slack: SearchPosts %q: %s", search, err)
		return []*bridge.Event{}
	}

	events := make([]*bridge.Event, 0, len(matches))
	for _, m := range matches {
		if m.Channel.ID != "" && m.Channel.Name != "" && !s.IsDMChannelName(m.Channel.ID) {
			s.sc.UpdateChannel(m.Channel.ID, m.Channel.Name)
		}

		msg := slack.Msg{
			Timestamp:       m.Timestamp,
			ThreadTimestamp: threadTSFromPermalink(m.Permalink),
			User:            m.User,
			Username:        m.Username,
			Text:            m.Text,
			Attachments:     m.Attachments,
			Blocks:          m.Blocks,
		}
		if ev := s.slackMsgToEvent(ctx, m.Channel.ID, msg); ev != nil {
			events = append(events, ev)
		}
	}

	return events
}

func (s *Slack) UpdateLastViewed(ctx context.Context, channelID string) {
	ts := fmt.Sprintf("%d.000000", time.Now().Unix())

	msgs, err := s.sc.GetConversationHistory(ctx, channelID, "", 1)
	if err == nil && len(msgs) > 0 && msgs[0].Timestamp != "" {
		ts = msgs[0].Timestamp
	}

	err = s.sc.MarkConversation(ctx, channelID, ts)
	if err != nil {
		logger.Debugf("UpdateLastViewed %s: %s", channelID, err)
	}
}

func (s *Slack) UpdateLastViewedUser(ctx context.Context, userID string) error {
	chID := s.GetDMChannelID(ctx, userID)
	if chID == "" {
		return errors.New("DM channel not found")
	}

	s.UpdateLastViewed(ctx, chID)

	return nil
}

func slackFileToBridgeFile(f *slack.File) *bridge.File {
	if f == nil {
		return nil
	}

	fileURL := cmp.Or(f.URLPrivateDownload, f.URLPrivate, f.Permalink)
	if fileURL == "" {
		return nil
	}

	name := cmp.Or(f.Name, f.Title, f.ID)

	return &bridge.File{
		Name: name,
		Size: int64(f.Size),
		URL:  fileURL,
	}
}

func slackFilesToBridgeFiles(files []slack.File) []*bridge.File {
	if len(files) == 0 {
		return nil
	}

	out := make([]*bridge.File, 0, len(files))

	for i := range files {
		if bf := slackFileToBridgeFile(&files[i]); bf != nil {
			out = append(out, bf)
		}
	}

	return out
}

func (s *Slack) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	files := make([]*bridge.File, 0, len(fileIDs))

	for _, id := range fileIDs {
		f, err := s.sc.GetFileInfo(ctx, id)
		if err != nil {
			continue
		}

		if bf := slackFileToBridgeFile(f); bf != nil {
			files = append(files, bf)
		}
	}

	return files
}

func (s *Slack) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	susers := s.sc.SearchUsers(query)
	users := make([]*bridge.UserInfo, 0, len(susers))

	for i := range susers {
		users = append(users, s.createUser(&susers[i]))
	}

	return users, nil
}

func (s *Slack) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	if channelID == "" {
		return []*bridge.Event{}
	}

	if limit <= 0 {
		limit = 100
	}

	limit = min(limit, 1000)

	msgs, err := s.sc.GetConversationHistory(ctx, channelID, "", limit)
	if err != nil {
		logger.Errorf("slack: GetPosts %s: %s", channelID, err)
		return []*bridge.Event{}
	}

	return s.msgsToEvents(ctx, channelID, msgs)
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
	shortFieldMaxLen, omitFieldTitles := s.attachmentFormattingOpts()

	if rawText, ok := formatSlackMsgOpts(&m.Msg, shortFieldMaxLen, omitFieldTitles); ok {
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
	if rc.Slack.DisableDefaultMentions {
		summary = utils.SanitizeDefaultMentions(summary, rc.Slack.Formatter.Unicode)
	}

	if cp.sender == nil || cp.sender.Nick == "" {
		return " (re: " + summary + ")"
	}

	return " (re @" + cp.sender.Nick + ": " + summary + ")"
}

var slackChannelMentionsRegExp = regexp.MustCompile(`@(?:channel|all|here|everyone)(?:\W|$)`)

func (s *Slack) channelMessageType(text string) string {
	if s.cfg != nil && !s.cfg.Current().Slack.DisableDefaultMentions && slackChannelMentionsRegExp.MatchString(text) {
		return "notice"
	}

	return ""
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
	shortFieldMaxLen, omitFieldTitles := s.attachmentFormattingOpts()
	rawText, ok := formatSlackMsgOpts(&m, shortFieldMaxLen, omitFieldTitles)
	files := slackFilesToBridgeFiles(m.Files)

	if !ok && len(files) == 0 {
		return nil
	}

	text := ""
	if ok {
		text = s.cleanupMessage(rawText)
	}

	msgID := encodeSlackID(channelID, m.Timestamp)
	parentID := ""
	rootTS := m.Timestamp

	if m.ThreadTimestamp != "" && m.ThreadTimestamp != m.Timestamp {
		parentID = encodeSlackID(channelID, m.ThreadTimestamp)
		rootTS = m.ThreadTimestamp
	}

	s.cachePost(channelID, m.Timestamp, rootTS, ghost, text)

	if parentID != "" && text != "" {
		text = strings.TrimRight(text, "\n") + s.formatReplySuffix(ctx, channelID, m.ThreadTimestamp)
	}

	createAt := slackTSMillis(m.Timestamp)

	if strings.HasPrefix(channelID, "D") {
		return &bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: channelID,
				Sender:    ghost,
				Receiver:  s.resolveDMReceiver(ghost, channelID),
				MessageID: msgID,
				ParentID:  parentID,
				CreateAt:  createAt,
				Files:     files,
			},
		}
	}

	return &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   channelID,
			Sender:      ghost,
			MessageType: s.channelMessageType(text),
			MessageID:   msgID,
			ParentID:    parentID,
			CreateAt:    createAt,
			Files:       files,
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
	name, isChan := strings.CutPrefix(name, "#")
	if name == "" {
		return ""
	}

	if !isChan {
		if isSlackIDWithPrefix(name, 'D') {
			return strings.ToUpper(name)
		}

		if isSlackIDWithPrefix(name, 'U', 'W') {
			if chID := s.GetDMChannelID(ctx, strings.ToUpper(name)); chID != "" {
				return chID
			}
		}
	}

	if chID := s.sc.GetChannelID(ctx, name); chID != "" {
		return chID
	}

	if !isChan {
		if u := s.GetUserByUsername(ctx, name); u != nil && u.User != "" {
			return s.GetDMChannelID(ctx, u.User)
		}
	}

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
		Login:    s.credentials.Login,
		Team:     s.credentials.Team,
		Pass:     s.credentials.Pass,
		Token:    s.credentials.Token,
		MFAToken: s.credentials.MFAToken,
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

	wsCtx, wsCancel := context.WithCancel(context.Background())
	s.wsCancel = wsCancel

	go s.handleSlack(wsCtx)
	go s.onConnect()

	s.connected.Store(true)

	return s.sc, nil
}

func (s *Slack) handleSlack(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-s.sc.IncomingEvents():
			if !ok {
				return
			}

			logger.Tracef("handleSlack %s", spew.Sdump(msg))
			s.handleSlackEvent(ctx, msg)
		}
	}
}

func (s *Slack) handleSlackEvent(ctx context.Context, msg slack.RTMEvent) {
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
	case *slack.ReactionAddedEvent:
		logger.Debugf("ReactionAdded msg %#v", ev)
		s.handleSlackReaction(ctx, ev.User, ev.Item.Channel, ev.Item.Timestamp, ev.Reaction, true)
	case *slack.ReactionRemovedEvent:
		logger.Debugf("ReactionRemoved msg %#v", ev)
		s.handleSlackReaction(ctx, ev.User, ev.Item.Channel, ev.Item.Timestamp, ev.Reaction, false)
	case *slack.PresenceChangeEvent:
		s.handlePresenceChange(ev.User, ev.Users, ev.Presence)
	case *slack.ManualPresenceChangeEvent:
		s.handlePresenceChange(s.sc.UserID(), nil, ev.Presence)
	case *slack.UserChangeEvent:
		s.sc.UpdateUser(ev.User)
		s.emitUserUpdate(ev.User)
	case *slack.UserProfileChangedEvent:
		s.sc.UpdateUser(ev.User)
		s.emitUserUpdate(ev.User)
	case *slack.UserStatusChangedEvent:
		s.handleUserStatusChanged(ev.User)
	case *slack.TeamJoinEvent:
		s.sc.UpdateUser(ev.User)
		s.emitUserUpdate(ev.User)
	case *slack.LatencyReport:
		s.sc.NotifyPong()
	default:
		if !s.handleSlackItemEvent(ctx, msg.Data) {
			s.handleSlackLifecycleEvent(msg.Data)
		}
	}
}

func (s *Slack) handleSlackConnected() {
	if !s.wasDisconnected.CompareAndSwap(true, false) {
		return
	}

	if discAt := s.disconnectedAt.Swap(0); discAt > 0 && s.sc != nil {
		thresh := 15 * time.Minute

		if s.cfg != nil {
			if rc := s.cfg.Current(); rc != nil && rc.Slack.HeavySyncThreshold > 0 {
				thresh = rc.Slack.HeavySyncThreshold
			}
		}

		if time.Since(time.Unix(discAt, 0)) >= thresh {
			s.sc.InvalidateCaches()
		}
	}

	if s.onConnect != nil {
		go s.onConnect()
	}
}

func (s *Slack) handleSlackDisconnected(ev *slack.DisconnectedEvent) {
	if logger != nil {
		logger.Debugf("disconnected event received (intentional=%v, cause=%v)", ev.Intentional, ev.Cause)
	}

	if !ev.Intentional {
		if s.wasDisconnected.CompareAndSwap(false, true) {
			s.disconnectedAt.Store(time.Now().Unix())
		}

		if s.onWsError != nil {
			s.onWsError(ev.Cause)
		}
	}
}

func (s *Slack) handleUserStatusChanged(user slack.User) {
	s.sc.UpdateUser(user)
	s.emitUserUpdate(user)

	if user.ID != "" && user.ID != s.sc.UserID() && !s.HasCustomStatus(user.ID) && s.eventChan != nil {
		s.sendEvent(&bridge.Event{
			Type: "status_change",
			Data: &bridge.StatusChangeEvent{
				UserID: user.ID,
				Status: statusOnline,
			},
		})
	}
}

func (s *Slack) emitUserUpdate(user slack.User) {
	if s.eventChan == nil {
		return
	}

	target := &user
	if user.ID != "" && s.sc != nil {
		if cached := s.sc.GetUser(user.ID); cached != nil {
			target = cached
		}
	}

	info := s.createUser(target)
	if info == nil || info.User == "" {
		return
	}

	s.sendEvent(&bridge.Event{
		Type: "user_update",
		Data: &bridge.UserUpdateEvent{
			User: info,
		},
	})
}

func (s *Slack) handleSlackItemEvent(ctx context.Context, data any) bool {
	switch ev := data.(type) {
	case *slack.StarAddedEvent:
		logger.Debugf("StarAdded msg %#v", ev)

		if ev.Item.Message != nil {
			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message starred")
		}

		return true
	case *slack.StarRemovedEvent:
		logger.Debugf("StarRemoved msg %#v", ev)

		if ev.Item.Message != nil {
			s.handleItemMisc(ctx, ev.User, ev.Item.Channel, ev.Item.Message, "Message unstarred")
		}

		return true
	case *slack.PinAddedEvent:
		logger.Debugf("PinAdded msg %#v", ev)

		if ev.Item.Message != nil {
			s.handleItemPin(ctx, ev.User, ev.Item.Channel, ev.Item.Message, subTypePinAdded)
		}

		return true
	case *slack.PinRemovedEvent:
		logger.Debugf("PinRemoved msg %#v", ev)

		if ev.Item.Message != nil {
			s.handleItemPin(ctx, ev.User, ev.Item.Channel, ev.Item.Message, subTypePinRemoved)
		}

		return true
	default:
		return false
	}
}

func (s *Slack) handleSlackLifecycleEvent(data any) {
	switch ev := data.(type) {
	case *slack.ChannelCreatedEvent:
		s.emitChannelCreate(ev.Channel.ID, ev.Channel.Name)
	case *slack.GroupCreatedEvent:
		s.emitChannelCreate(ev.Channel.ID, ev.Channel.Name)
	case *slack.ChannelRenameEvent:
		s.emitChannelUpdate(ev.Channel.ID, ev.Channel.Name)
	case *slack.GroupRenameEvent:
		s.emitChannelUpdate(ev.Group.ID, ev.Group.Name)
	case *slack.ChannelDeletedEvent:
		s.emitChannelDelete(ev.Channel)
	case *slack.ChannelArchiveEvent:
		s.emitChannelDelete(ev.Channel)
	case *slack.GroupArchiveEvent:
		s.emitChannelDelete(ev.Channel)
	case *slack.ChannelJoinedEvent:
		s.emitChannelAdd(ev.Channel.ID, ev.Channel.Name)
	case *slack.GroupJoinedEvent:
		s.emitChannelAdd(ev.Channel.ID, ev.Channel.Name)
	case *slack.ChannelLeftEvent:
		s.emitChannelRemove(ev.Channel)
	case *slack.GroupLeftEvent:
		s.emitChannelRemove(ev.Channel)
	case *slack.UserTypingEvent:
		s.handleSlackTyping(ev)
	case *slack.ConnectedEvent:
		s.handleSlackConnected()
	case *slack.DisconnectedEvent:
		s.handleSlackDisconnected(ev)
	}
}

func (s *Slack) handleSlackTyping(ev *slack.UserTypingEvent) {
	if ev == nil || ev.User == "" || ev.Channel == "" || ev.User == s.sc.UserID() {
		return
	}

	sender := s.createUser(s.sc.GetUser(ev.User))
	if sender == nil || sender.User == "" {
		return
	}

	channelType := ""
	receiver := sender

	if strings.HasPrefix(ev.Channel, "D") {
		channelType = "D"
		receiver = s.GetMe()
	}

	s.sendEvent(&bridge.Event{
		Type: "typing",
		Data: &bridge.TypingEvent{
			ChannelID:   ev.Channel,
			ChannelType: channelType,
			Receiver:    receiver,
			Sender:      sender,
		},
	})
}

func (s *Slack) emitChannelCreate(channelID, name string) {
	if channelID == "" {
		return
	}

	s.sc.UpdateChannel(channelID, name)
	s.sc.SetChannelJoined(channelID, true)
	s.sc.InvalidateUserChannels("")

	chName := ""
	if name != "" {
		chName = "#" + strings.TrimPrefix(name, "#")
	}

	s.sendEvent(&bridge.Event{
		Type: "channel_create",
		Data: &bridge.ChannelCreateEvent{
			ChannelID: channelID,
			Name:      chName,
		},
	})
}

func (s *Slack) emitChannelUpdate(channelID, name string) {
	if channelID == "" || name == "" {
		return
	}

	cleanName := strings.TrimPrefix(name, "#")

	s.sc.UpdateChannel(channelID, cleanName)
	s.sc.InvalidateUserChannels("")

	s.sendEvent(&bridge.Event{
		Type: "channel_update",
		Data: &bridge.ChannelUpdateEvent{
			ChannelID:   channelID,
			Name:        cleanName,
			DisplayName: cleanName,
		},
	})
}

func (s *Slack) emitChannelDelete(channelID string) {
	if channelID == "" {
		return
	}

	s.sc.RemoveChannel(channelID)
	s.sc.InvalidateUserChannels("")

	s.sendEvent(&bridge.Event{
		Type: "channel_delete",
		Data: &bridge.ChannelDeleteEvent{
			ChannelID: channelID,
		},
	})
}

func (s *Slack) emitChannelAdd(channelID, name string) {
	if channelID == "" {
		return
	}

	s.sc.UpdateChannel(channelID, name)
	s.sc.SetChannelJoined(channelID, true)

	me := s.GetMe()

	s.sendEvent(&bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Adder:     me,
			Added:     []*bridge.UserInfo{me},
			ChannelID: channelID,
		},
	})
}

func (s *Slack) emitChannelRemove(channelID string) {
	if channelID == "" {
		return
	}

	s.sc.SetChannelJoined(channelID, false)

	me := s.GetMe()

	s.sendEvent(&bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover:   me,
			Removed:   []*bridge.UserInfo{me},
			ChannelID: channelID,
		},
	})
}

func (s *Slack) handlePresenceChange(user string, users []string, presence string) {
	targets := users
	if len(targets) == 0 && user != "" {
		targets = []string{user}
	}

	normalized := normalizeSlackPresence(presence)

	for _, uid := range targets {
		if uid == "" {
			continue
		}

		s.sc.UpdatePresence(uid, presence)

		s.sendEvent(&bridge.Event{
			Type: "status_change",
			Data: &bridge.StatusChangeEvent{
				UserID: uid,
				Status: normalized,
			},
		})
	}
}

func (s *Slack) resolveDMReceiver(ghost *bridge.UserInfo, channelID string) *bridge.UserInfo {
	receiver := ghost
	if ghost != nil && ghost.Me {
		if dmUser := s.resolveDMPeer(channelID); dmUser != nil && dmUser.User != "" {
			return dmUser
		}

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
	if ghost.Me && s.cfg != nil && !s.cfg.Current().Slack.ShowOwnReactions {
		return
	}

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
		s.sendEvent(&bridge.Event{
			Type: "reaction_add",
			Data: &bridge.ReactionAddEvent{
				ChannelID: channelID, MessageID: msgID, ParentID: parentID,
				Message: replyMsg, Receiver: receiver, Sender: ghost,
				Reaction: reaction, ChannelType: channelType,
			},
		})

		return
	}

	s.sendEvent(&bridge.Event{
		Type: "reaction_remove",
		Data: &bridge.ReactionRemoveEvent{
			ChannelID: channelID, MessageID: msgID, ParentID: parentID,
			Message: replyMsg, Receiver: receiver, Sender: ghost,
			Reaction: reaction, ChannelType: channelType,
		},
	})
}

func (s *Slack) resolveItemParent(ctx context.Context, channelID string, itemMsg *slack.Message) (string, string) {
	parentID := s.cachedParentID(channelID, itemMsg.Timestamp)
	if parentID == "" && itemMsg.ThreadTimestamp != "" && itemMsg.ThreadTimestamp != itemMsg.Timestamp {
		parentID = encodeSlackID(channelID, itemMsg.ThreadTimestamp)
	}

	replySuffix := ""
	if itemMsg.ThreadTimestamp != "" && itemMsg.ThreadTimestamp != itemMsg.Timestamp {
		replySuffix = s.formatReplySuffix(ctx, channelID, itemMsg.ThreadTimestamp)
	}

	return parentID, replySuffix
}

func (s *Slack) handleItemMisc(ctx context.Context, userID, channelID string, itemMsg *slack.Message, action string) {
	msgID := encodeSlackID(channelID, itemMsg.Timestamp)
	parentID, replySuffix := s.resolveItemParent(ctx, channelID, itemMsg)
	msg := s.cleanupMessage(action+" ("+itemMsg.Text+")") + replySuffix

	s.handleActionMisc(userID, channelID, msgID, parentID, msg, "", slackTSMillis(itemMsg.Timestamp))
}

func formatPinnedSlackMsg(cleanup func(string) string, unicode bool, itemMsg *slack.Message, replySuffix, kind string) string {
	rawText, msghandled := formatSlackMsg(&itemMsg.Msg)
	if !msghandled {
		rawText = itemMsg.Text
	}

	if cleanup != nil {
		rawText = cleanup(rawText)
	}

	msg := utils.AppendMessageSuffix(rawText, replySuffix)

	return utils.AppendMessageSuffix(msg, slackActionSuffix(unicode, kind))
}

func (s *Slack) handleItemPin(ctx context.Context, userID, channelID string, itemMsg *slack.Message, kind string) {
	msgID := encodeSlackID(channelID, itemMsg.Timestamp)
	parentID, replySuffix := s.resolveItemParent(ctx, channelID, itemMsg)
	msg := formatPinnedSlackMsg(s.cleanupMessage, s.FormatterConfig().Unicode, itemMsg, replySuffix, kind)

	s.handleActionMisc(userID, channelID, msgID, parentID, msg, "post_edited", slackTSMillis(itemMsg.Timestamp))
}

func (s *Slack) handleActionMisc(userID, channelID, msgID, parentID, msg, eventType string, createAt int64) {
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

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, eventType, createAt)
}

func (s *Slack) handleMemberLeftChannel(ctx context.Context, rmsg *slack.MemberLeftChannelEvent) {
	s.sc.RemoveChannelMember(rmsg.Channel, rmsg.User)

	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Removed: []*bridge.UserInfo{
				s.GetUser(ctx, rmsg.User),
			},
			ChannelID: rmsg.Channel,
		},
	}

	s.sendEvent(event)
}

func (s *Slack) handleMemberJoinedChannel(ctx context.Context, rmsg *slack.MemberJoinedChannelEvent) {
	s.sc.AddChannelMember(rmsg.Channel, rmsg.User)

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
	s.sendEvent(event)
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

	s.sendEvent(event)
}

func (s *Slack) sendPublicMessage(ghost *bridge.UserInfo, msg, channelID, msgID, parentID, eventType string, createAt int64) {
	event := &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        msg,
			ChannelID:   channelID,
			Sender:      ghost,
			MessageType: s.channelMessageType(msg),
			MessageID:   msgID,
			ParentID:    parentID,
			Event:       eventType,
			CreateAt:    createAt,
		},
	}

	s.sendEvent(event)
}

func (s *Slack) attachmentFormattingOpts() (int, bool) {
	if s.cfg == nil {
		return 0, false
	}

	sc := s.cfg.Current().Slack

	return sc.MessageAttachmentShortFieldMaxLineLength, sc.MessageAttachmentOmitFieldTitles
}

func formatSlackAttachmentFields(fields []slack.AttachmentField, shortFieldMaxLen int, omitFieldTitles bool) []string {
	if len(fields) == 0 {
		return nil
	}

	if shortFieldMaxLen <= 0 {
		shortFieldMaxLen = 100
	}

	var (
		lines    []string
		shortBuf string
	)

	flushShort := func() {
		if shortBuf != "" {
			lines = append(lines, "> "+shortBuf)
			shortBuf = ""
		}
	}

	for _, f := range fields {
		entry := f.Value
		if !omitFieldTitles && f.Title != "" {
			if entry != "" {
				entry = f.Title + ": " + entry
			} else {
				entry = f.Title
			}
		}

		if entry == "" {
			continue
		}

		if !f.Short {
			flushShort()

			lines = append(lines, "> "+entry)

			continue
		}

		switch {
		case shortBuf == "":
			shortBuf = entry
		case len(shortBuf)+3+len(entry) <= shortFieldMaxLen:
			shortBuf += " | " + entry
		default:
			flushShort()

			shortBuf = entry
		}
	}

	flushShort()

	return lines
}

func formatSlackAttachmentHeader(attach slack.Attachment) []string {
	var lines []string

	if attach.AuthorName != "" {
		if attach.AuthorLink != "" {
			lines = append(lines, "> "+attach.AuthorName+" ("+attach.AuthorLink+")")
		} else {
			lines = append(lines, "> "+attach.AuthorName)
		}
	}

	if attach.Title != "" {
		if attach.TitleLink != "" {
			lines = append(lines, "> "+attach.Title+" ("+attach.TitleLink+")")
		} else {
			lines = append(lines, "> "+attach.Title)
		}
	}

	return lines
}

func formatSlackAttachment(attach slack.Attachment, shortFieldMaxLen int, omitFieldTitles bool) []string {
	var lines []string

	if attach.Pretext != "" {
		lines = append(lines, strings.Split(attach.Pretext, "\n")...)
	}

	lines = append(lines, formatSlackAttachmentHeader(attach)...)

	if attach.Text != "" {
		for i, row := range strings.Split(attach.Text, "\n") {
			lines = append(lines, "> "+row)

			if i > 4 {
				lines = append(lines, "> ...")
				break
			}
		}
	}

	if fieldLines := formatSlackAttachmentFields(attach.Fields, shortFieldMaxLen, omitFieldTitles); len(fieldLines) > 0 {
		lines = append(lines, fieldLines...)
	}

	if attach.ImageURL != "" {
		lines = append(lines, "> "+attach.ImageURL)
	}

	if len(lines) == 0 && attach.Fallback != "" {
		for row := range strings.SplitSeq(attach.Fallback, "\n") {
			lines = append(lines, "> "+row)
		}
	}

	return lines
}

func formatRichTextInline(elems []slack.RichTextSectionElement) string {
	var b strings.Builder

	for _, elem := range elems {
		switch se := elem.(type) {
		case *slack.RichTextSectionTextElement:
			b.WriteString(se.Text)
		case *slack.RichTextSectionLinkElement:
			if se.Text != "" && se.Text != se.URL {
				b.WriteString("<" + se.URL + "|" + se.Text + ">")
			} else {
				b.WriteString(se.URL)
			}
		case *slack.RichTextSectionUserElement:
			b.WriteString("<@" + se.UserID + ">")
		case *slack.RichTextSectionChannelElement:
			b.WriteString("<#" + se.ChannelID + ">")
		case *slack.RichTextSectionBroadcastElement:
			b.WriteString("<!" + se.Range + ">")
		case *slack.RichTextSectionEmojiElement:
			b.WriteString(":" + se.Name + ":")
		case *slack.RichTextSectionUserGroupElement:
			b.WriteString("<!subteam^" + se.UsergroupID + ">")
		}
	}

	return b.String()
}

func formatRichTextElements(elems []slack.RichTextElement) []string {
	var lines []string

	for _, elem := range elems {
		switch rt := elem.(type) {
		case *slack.RichTextSection:
			if text := formatRichTextInline(rt.Elements); text != "" {
				lines = append(lines, strings.Split(text, "\n")...)
			}
		case *slack.RichTextQuote:
			if text := formatRichTextInline(rt.Elements); text != "" {
				for row := range strings.SplitSeq(text, "\n") {
					lines = append(lines, "> "+row)
				}
			}
		case *slack.RichTextPreformatted:
			if text := formatRichTextInline(rt.Elements); text != "" {
				lines = append(lines, strings.Split(text, "\n")...)
			}
		case *slack.RichTextList:
			for _, item := range formatRichTextElements(rt.Elements) {
				lines = append(lines, "- "+item)
			}
		}
	}

	return lines
}

func formatSlackContextBlock(b *slack.ContextBlock) []string {
	var parts []string

	for _, elem := range b.ContextElements.Elements {
		switch el := elem.(type) {
		case *slack.TextBlockObject:
			if el != nil && el.Text != "" {
				parts = append(parts, el.Text)
			}
		case slack.TextBlockObject:
			if el.Text != "" {
				parts = append(parts, el.Text)
			}
		case *slack.ImageBlockElement:
			if el != nil && el.ImageURL != nil && *el.ImageURL != "" {
				parts = append(parts, *el.ImageURL)
			}
		}
	}

	if len(parts) == 0 {
		return nil
	}

	return strings.Split(strings.Join(parts, " "), "\n")
}

func formatSlackSectionBlock(b *slack.SectionBlock) []string {
	var lines []string

	if b.Text != nil && b.Text.Text != "" {
		lines = append(lines, strings.Split(b.Text.Text, "\n")...)
	}

	for _, f := range b.Fields {
		if f != nil && f.Text != "" {
			lines = append(lines, strings.Split(f.Text, "\n")...)
		}
	}

	return lines
}

func formatSlackBlocks(blocks slack.Blocks) []string {
	var lines []string

	for _, blk := range blocks.BlockSet {
		switch b := blk.(type) {
		case *slack.HeaderBlock:
			if b.Text != nil && b.Text.Text != "" {
				lines = append(lines, strings.Split(b.Text.Text, "\n")...)
			}
		case *slack.SectionBlock:
			lines = append(lines, formatSlackSectionBlock(b)...)
		case *slack.ContextBlock:
			lines = append(lines, formatSlackContextBlock(b)...)
		case *slack.RichTextBlock:
			lines = append(lines, formatRichTextElements(b.Elements)...)
		case *slack.MarkdownBlock:
			if b.Text != "" {
				lines = append(lines, strings.Split(b.Text, "\n")...)
			}
		case *slack.ImageBlock:
			if b.ImageURL != "" {
				lines = append(lines, b.ImageURL)
			}
		}
	}

	return lines
}

func formatSlackMsg(m *slack.Msg) (string, bool) {
	return formatSlackMsgOpts(m, 0, false)
}

func formatSlackMsgOpts(m *slack.Msg, shortFieldMaxLen int, omitFieldTitles bool) (string, bool) {
	if m == nil {
		return "", false
	}

	var msgs []string

	msghandled := false

	if m.Text != "" {
		msgs = append(msgs, strings.Split(m.Text, "\n")...)
		msghandled = true
	} else if blockLines := formatSlackBlocks(m.Blocks); len(blockLines) > 0 {
		msgs = append(msgs, blockLines...)
		msghandled = true
	}

	for _, attach := range m.Attachments {
		if attachLines := formatSlackAttachment(attach, shortFieldMaxLen, omitFieldTitles); len(attachLines) > 0 {
			msgs = append(msgs, attachLines...)
			msghandled = true
		}
	}

	if !msghandled {
		return "", false
	}

	return strings.Join(msgs, "\n"), true
}

func slackActionSuffix(unicode bool, kind string) string {
	switch kind {
	case subTypeMessageChanged:
		return utils.FormatActionSuffix(utils.SuffixEdited, unicode)
	case subTypeMessageDeleted:
		return utils.FormatActionSuffix(utils.SuffixDeleted, unicode)
	case subTypePinAdded:
		return utils.FormatActionSuffix(utils.SuffixPinned, unicode)
	case subTypePinRemoved:
		return utils.FormatActionSuffix(utils.SuffixUnpinned, unicode)
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

	return utils.AppendMessageSuffix(summary+replySuffix, slackActionSuffix(unicode, subTypeMessageDeleted))
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

	s.evictCachedPost(channelID, msgTS)

	if ghost != nil && ghost.Me && rc != nil && rc.Slack.DisableShowOwnModified {
		return
	}

	msg := formatDeletedSlackMsg(rc, origMsg, replySuffix)
	createAt := slackTSMillis(msgTS)

	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)
		s.sendDirectMessage(ghost, receiver, msg, channelID, msgID, parentID, createAt)

		return
	}

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, "post_deleted", createAt)
}

func isSlackLinkUnfurl(rmsg *slack.MessageEvent) bool {
	return rmsg.SubType == subTypeMessageChanged &&
		rmsg.SubMessage != nil &&
		rmsg.PreviousMessage != nil &&
		rmsg.SubMessage.Edited == nil &&
		rmsg.SubMessage.Text == rmsg.PreviousMessage.Text &&
		len(rmsg.SubMessage.Attachments) > len(rmsg.PreviousMessage.Attachments)
}

func formatSlackActionPostText(rmsg *slack.MessageEvent, isUnfurl bool, shortFieldMaxLen int, omitFieldTitles bool) (string, bool) {
	if isUnfurl {
		newAttachments := rmsg.SubMessage.Attachments[len(rmsg.PreviousMessage.Attachments):]

		return formatSlackMsgOpts(&slack.Msg{Attachments: newAttachments}, shortFieldMaxLen, omitFieldTitles)
	}

	if rmsg.SubType == subTypeMessageChanged && rmsg.SubMessage != nil {
		return formatSlackMsgOpts(rmsg.SubMessage, shortFieldMaxLen, omitFieldTitles)
	}

	return formatSlackMsgOpts(&rmsg.Msg, shortFieldMaxLen, omitFieldTitles)
}

func (s *Slack) handleSlackTopicEvent(rmsg *slack.MessageEvent) bool {
	if rmsg.SubType != eventChannelTopic && rmsg.SubType != eventGroupTopic {
		return false
	}

	topic := s.cleanupMessage(rmsg.Topic)
	s.sc.UpdateTopic(rmsg.Channel, topic)

	if !strings.HasPrefix(rmsg.Channel, "D") {
		s.sendEvent(&bridge.Event{
			Type: eventChannelTopic,
			Data: &bridge.ChannelTopicEvent{
				Text:      topic,
				ChannelID: rmsg.Channel,
				UserID:    rmsg.User,
			},
		})

		return true
	}

	rmsg.Text = "\x01ACTION updated topic to: " + topic + "\x01"

	return false
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

	if s.handleSlackTopicEvent(rmsg) {
		return
	}

	eventType := ""
	if (rmsg.SubType == eventChannelPurpose || rmsg.SubType == eventGroupPurpose) && !strings.HasPrefix(rmsg.Channel, "D") {
		eventType = eventChannelPurpose

		if rmsg.Purpose != "" {
			rmsg.Text = "set the channel description: " + rmsg.Purpose
		}
	}

	msgTS := rmsg.Timestamp
	parentTS := rmsg.ThreadTimestamp
	isUnfurl := isSlackLinkUnfurl(rmsg)

	if rmsg.SubType == subTypeMessageChanged {
		if rmsg.SubMessage == nil {
			return
		}

		if !isUnfurl {
			eventType = "post_edited"
		}

		msgTS = rmsg.SubMessage.Timestamp

		if rmsg.SubMessage.ThreadTimestamp != "" {
			parentTS = rmsg.SubMessage.ThreadTimestamp
		}
	}

	ghost, ok := s.resolveSlackPostGhost(rmsg)
	if !ok {
		return
	}

	shortFieldMaxLen, omitFieldTitles := s.attachmentFormattingOpts()
	if rmsg.SubType == subTypeMessageChanged && !isUnfurl && ghost.Me && s.cfg != nil && s.cfg.Current().Slack.DisableShowOwnModified {
		return
	}

	rawText, msghandled := formatSlackActionPostText(rmsg, isUnfurl, shortFieldMaxLen, omitFieldTitles)
	files := slackFilesToBridgeFiles(rmsg.Files)

	if !msghandled && len(files) == 0 {
		return
	}

	channelID := rmsg.Channel
	msgID := encodeSlackID(channelID, msgTS)

	parentID := ""
	rootTS := msgTS

	if parentTS != "" && parentTS != msgTS {
		parentID = encodeSlackID(channelID, parentTS)
		rootTS = parentTS
	}

	if msghandled {
		s.sendSlackActionPostMessage(ctx, ghost, rmsg, rawText, channelID, msgTS, rootTS, msgID, parentID, parentTS, eventType, isUnfurl)
	}

	if len(files) > 0 && rmsg.SubType != subTypeMessageChanged {
		s.emitSlackFileEvent(ghost, channelID, msgID, parentID, files)
	}
}

func (s *Slack) sendSlackActionPostMessage(
	ctx context.Context,
	ghost *bridge.UserInfo,
	rmsg *slack.MessageEvent,
	rawText, channelID, msgTS, rootTS, msgID, parentID, parentTS, eventType string,
	isUnfurl bool,
) {
	msg := s.cleanupMessage(rawText)

	switch rmsg.SubType {
	case eventChannelTopic, eventChannelPurpose:
		s.cacheThreadRoot(channelID, msgTS, rootTS)
	default:
		if !isUnfurl {
			s.cachePost(channelID, msgTS, rootTS, ghost, msg)
		}
	}

	if parentID != "" {
		msg = strings.TrimRight(msg, "\n") + s.formatReplySuffix(ctx, channelID, parentTS)
	}

	if rmsg.SubType == subTypeMessageChanged && !isUnfurl {
		msg = utils.AppendMessageSuffix(msg, slackActionSuffix(s.FormatterConfig().Unicode, subTypeMessageChanged))
	}

	createAt := slackTSMillis(msgTS)

	if strings.HasPrefix(channelID, "D") {
		receiver := s.resolveDMReceiver(ghost, channelID)
		s.sendDirectMessage(ghost, receiver, msg, channelID, msgID, parentID, createAt)

		return
	}

	s.sendPublicMessage(ghost, msg, channelID, msgID, parentID, eventType, createAt)
}

func (s *Slack) emitSlackFileEvent(ghost *bridge.UserInfo, channelID, msgID, parentID string, files []*bridge.File) {
	channelType := ""
	receiver := ghost

	if strings.HasPrefix(channelID, "D") {
		channelType = "D"
		receiver = s.resolveDMReceiver(ghost, channelID)
	}

	s.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Sender:      ghost,
			Receiver:    receiver,
			ChannelType: channelType,
			ChannelID:   channelID,
			MessageID:   msgID,
			ParentID:    parentID,
			Files:       files,
		},
	})
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

func slackMentionKeys(slackuser *slack.User, nick string) []string {
	if slackuser == nil {
		return nil
	}

	candidates := []string{
		nick,
		slackuser.Name,
		slackuser.Profile.DisplayName,
		cmp.Or(slackuser.RealName, slackuser.Profile.RealName),
		slackuser.ID,
	}

	var keys []string

	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c != "" && !slices.Contains(keys, c) {
			keys = append(keys, c)
		}
	}

	return keys
}

func buildSlackUserInfo(slackuser *slack.User, nick, teamID, directoryURL string, me bool) *bridge.UserInfo {
	roles := ""
	if slackuser.IsBot || slackuser.IsAppUser || slackuser.IsWorkflowBot {
		roles = "bot"
	}

	photo := cmp.Or(
		slackuser.Profile.ImageOriginal,
		slackuser.Profile.Image512,
		slackuser.Profile.Image192,
		slackuser.Profile.Image72,
		slackuser.Profile.Image48,
		slackuser.Profile.Image32,
		slackuser.Profile.Image24,
	)

	var mentionKeys []string
	if me {
		mentionKeys = slackMentionKeys(slackuser, nick)
	}

	return &bridge.UserInfo{
		Nick:        nick,
		User:        slackuser.ID,
		Real:        cmp.Or(slackuser.RealName, slackuser.Profile.RealName),
		Host:        "host",
		Roles:       roles,
		DisplayName: slackuser.Profile.DisplayName,
		Ghost:       true,
		Me:          me,
		Username:    cmp.Or(slackuser.Name, slackuser.Profile.RealName, slackuser.RealName),
		FirstName:   slackuser.Profile.FirstName,
		LastName:    slackuser.Profile.LastName,
		Email:       slackuser.Profile.Email,
		Directory:   directoryURL,
		Position:    slackuser.Profile.Title,
		Photo:       photo,
		TeamID:      teamID,
		MentionKeys: mentionKeys,
	}
}

func (s *Slack) createUser(slackuser *slack.User) *bridge.UserInfo {
	if slackuser == nil {
		return &bridge.UserInfo{}
	}

	rc := s.cfg.Current()
	nick := s.resolveSlackNick(slackuser, rc)
	me := slackuser.ID != "" && slackuser.ID == s.sc.UserID()
	directoryURL := utils.ExpandDirectoryURL(
		rc.Slack.Bridge.DirectoryURL,
		nick,
		slackuser.Name,
		slackuser.Profile.Email,
		slackuser.Profile.FirstName,
		slackuser.Profile.LastName,
	)

	info := buildSlackUserInfo(slackuser, nick, s.sc.TeamID(), directoryURL, me)
	if s.sc != nil && s.sc.TeamDomain() != "" {
		info.Host = s.sc.TeamDomain() + ".slack.com"
	}

	return info
}

func (s *Slack) Connected() bool {
	return s.connected.Load()
}

func (s *Slack) recordSent(ctx context.Context, channelID, msgID, threadTS, parentID, text string) {
	if s.msgLastSentCache == nil || msgID == "" {
		return
	}

	name := s.GetChannelName(ctx, channelID)
	if s.IsDMChannelName(name) {
		if receiver := s.GetDMUser(ctx, name); receiver != nil && receiver.Nick != "" {
			name = receiver.Nick
		}
	}

	if name == "" {
		name = channelID
	}

	var unicode bool
	if s.cfg != nil {
		unicode = s.cfg.Current().Slack.Formatter.Unicode
	}

	summary := utils.FormatAndShortenSummary(text, utils.SummaryOpts{
		DisableEmoji:    true,
		DisableMarkdown: true,
		MaxLength:       slackLastSentSummaryLen,
		UncountedPrefix: "@",
		Unicode:         unicode,
	})

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
		err := s.sc.DeleteMessage(ctx, channelID, ts)
		if err == nil {
			s.evictCachedPost(channelID, ts)
		}

		return err
	}

	text = slackActionText(text)

	err := s.sc.UpdateMessage(ctx, channelID, ts, text)
	if err == nil {
		if cp, ok := s.getCachedPostOnly(channelID, ts); ok {
			s.cachePost(channelID, ts, cp.rootTS, cp.sender, s.cleanupMessage(text))
		} else {
			s.evictCachedPost(channelID, ts)
		}
	}

	return err
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
	return s.GetPostsSince(ctx, channelID, since)
}

func (s *Slack) Ping(ctx context.Context, proto ...string) error {
	if s.sc == nil {
		return errors.New("client not initialized")
	}

	protocol := "ws"
	if len(proto) > 0 && proto[0] != "" {
		protocol = strings.ToLower(proto[0])
	}

	switch protocol {
	case "http", "rest":
		return s.sc.HttpPing(ctx)
	default:
		return s.sc.WsPing(ctx)
	}
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
	rc := s.cfg.Current()
	if !rc.EnableTyping || rc.DisableSendTyping || strings.HasPrefix(channelName, "&") {
		return
	}

	const typingDebounceSec = 4

	lastVal, ok := s.lastTypingSent.Load(channelName)
	if !ok {
		lastVal, _ = s.lastTypingSent.LoadOrStore(channelName, new(atomic.Int64))
	}

	lastPtr, ok := lastVal.(*atomic.Int64)
	if !ok {
		return
	}

	now := time.Now().Unix()
	last := lastPtr.Load()

	if now-last < typingDebounceSec || !lastPtr.CompareAndSwap(last, now) {
		return
	}

	var channelID string
	if pID, ok := strings.CutPrefix(channelName, "@@"); ok {
		channelID = s.GetPostChannelID(ctx, pID)
	} else if strings.HasPrefix(channelName, "#") {
		channelID = s.GetChannelID(ctx, channelName, "")
	} else if u := s.GetUserByUsername(ctx, channelName); u != nil && u.User != "" {
		channelID = s.GetDMChannelID(ctx, u.User)
	}

	if channelID != "" {
		s.sc.SendTyping(channelID)
	}
}
