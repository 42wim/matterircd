package matrix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/matterbridge/matrixclient"
	logger "github.com/sirupsen/logrus"
)

const (
	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"

	lastSentSummaryLen = 90
)

// markdownReplacer combines multi-line messages into a single line, making
// code blocks single code/monospace.
var markdownReplacer = strings.NewReplacer(
	"\n", " ",
	"```", "`",
	"~~~", "`",
)

type Matrix struct {
	mc        *matrixclient.Client
	eventChan chan *bridge.Event
	cfg       *config.Config
	onConnect func()

	connected atomic.Bool

	ctx context.Context //nolint:containedctx

	msgParentCache   *lru.Cache[id.EventID, string]
	msgLastSentCache *lru.Cache[string, string]
}

func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, *matrixclient.Client, error) {
	m := &Matrix{
		eventChan: eventChan,
		cfg:       cfg,
		onConnect: onConnect,
		ctx:       ctx,
	}
	m.msgParentCache, _ = lru.New[id.EventID, string](100)
	m.msgLastSentCache, _ = lru.New[string, string](10)

	setLogLevel := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			logger.SetLevel(logger.TraceLevel)
		case rc.Debug:
			logger.SetLevel(logger.DebugLevel)
		default:
			logger.SetLevel(logger.InfoLevel)
		}
	}
	setLogLevel(cfg.Current())
	cfg.RegisterReloadHook(setLogLevel)

	if cred.Token != "" && cred.Login == "" {
		return nil, nil, errors.New("matrix: login with an access token is not supported, use LOGIN <server> <login> <pass>")
	}

	mc, err := matrixclient.New(ctx, matrixclient.Credentials{
		Server: cred.Server,
		Login:  cred.Login,
		Pass:   cred.Pass,
	}, m)
	if err != nil {
		return nil, nil, err
	}

	m.mc = mc
	m.connected.Store(true)

	return m, mc, nil
}

// Ready implements matrixclient.Handler.
func (m *Matrix) Ready() {
	go m.onConnect()
}

func (m *Matrix) sendEvent(ev *bridge.Event) {
	select {
	case m.eventChan <- ev:
	case <-m.ctx.Done():
	}
}

// OnMessage implements matrixclient.Handler.
func (m *Matrix) OnMessage(msg *matrixclient.Message) {
	ghost := m.createUser(msg.Sender)
	if ghost == nil {
		return
	}

	text := msg.Text

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies && msg.ParentID != "" {
		message, err := m.addParentMsg(m.ctx, msg.RoomID, msg.ParentID, text, mxCfg.ShortenRepliesTo, "@")
		if err != nil {
			logger.Errorf("Unable to get parent post for %s: %s", msg.EventID, err)
		}

		text = message
	}

	if msg.Direct {
		m.sendEvent(&bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: msg.RoomID.String(),
				Sender:    ghost,
				Receiver:  m.GetMe(),
				MessageID: string(msg.EventID),
				ParentID:  msg.ParentID.String(),
				CreateAt:  msg.Timestamp,
			},
		})

		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   msg.RoomID.String(),
			Sender:      ghost,
			ChannelType: "P",
			MessageID:   string(msg.EventID),
			ParentID:    msg.ParentID.String(),
			CreateAt:    msg.Timestamp,
		},
	})
}

// OnReaction implements matrixclient.Handler.
func (m *Matrix) OnReaction(r *matrixclient.Reaction) {
	ghost := m.createUser(r.Sender)
	if ghost == nil {
		return
	}

	var text string

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies {
		message, err := m.addParentMsg(m.ctx, r.RoomID, r.ParentID, text, mxCfg.ShortenRepliesTo, "@")
		if err != nil {
			logger.Errorf("Unable to get parent post for %s: %s", r.ParentID, err)
		}

		text = message
	}

	channelType := ""
	if r.Direct {
		channelType = "D"
	}

	m.sendEvent(&bridge.Event{
		Type: "reaction_add",
		Data: &bridge.ReactionAddEvent{
			ChannelID:   r.RoomID.String(),
			MessageID:   string(r.ParentID),
			Sender:      ghost,
			Reaction:    r.Key,
			ChannelType: channelType,
			Message:     text,
			ParentID:    r.ParentID.String(),
		},
	})
}

func (m *Matrix) Invite(ctx context.Context, channelID, username string) error {
	return nil
}

func (m *Matrix) IsChannelMember(channelID string) bool {
	for _, r := range m.mc.Rooms() {
		if r.ID.String() == channelID {
			return true
		}
	}

	return m.mc.IsDirect(id.RoomID(channelID))
}

func (m *Matrix) Join(ctx context.Context, channelName string) (string, string, error) {
	roomID, err := m.mc.Join(ctx, channelName)
	if err != nil {
		return "", "", err
	}

	return roomID.String(), "", nil
}

func (m *Matrix) List(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (m *Matrix) Part(ctx context.Context, channelID string) error {
	return nil
}

func (m *Matrix) UpdateChannels(ctx context.Context) error {
	return nil
}

func (m *Matrix) Logout(ctx context.Context) error {
	m.connected.Store(false)
	m.mc.Logout()

	return nil
}

func (m *Matrix) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return m.MsgUserThread(ctx, userID, "", text)
}

func (m *Matrix) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	logger.Debugf("sending message '%s' '%s' '%s'", userID, parentID, text)

	eventID, err := m.mc.SendDirect(ctx, id.UserID(userID), id.EventID(parentID), text)
	if err != nil {
		logger.Error("msguserthread sending message: error ", err)
		return "", err
	}

	return m.recordSent(ctx, "", eventID, text), nil
}

func (m *Matrix) recordSent(ctx context.Context, channelID string, eventID id.EventID, text string) string {
	name := channelID
	if channelID != "" {
		name = m.GetChannelName(ctx, channelID)
	}

	summary := m.formatSummary(text, lastSentSummaryLen, "@")
	m.msgLastSentCache.Add(eventID.String(), name+": "+summary)

	return eventID.String()
}

func (m *Matrix) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return m.MsgChannelThread(ctx, channelID, "", text)
}

func (m *Matrix) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	logger.Debugf("msgchannelthread: sending message thread '%s' '%s' '%s'", channelID, parentID, text)

	eventID, err := m.mc.Send(ctx, id.RoomID(channelID), id.EventID(parentID), text)
	if err != nil {
		return "", err
	}

	return m.recordSent(ctx, channelID, eventID, text), nil
}

func (m *Matrix) ModifyPost(ctx context.Context, msgID, text string) error {
	return errors.New("not implemented yet")
}

func (m *Matrix) Topic(ctx context.Context, channelID string) string {
	return ""
}

func (m *Matrix) SetTopic(ctx context.Context, channelID, text string) error {
	return nil
}

func (m *Matrix) StatusUser(ctx context.Context, userID string) (string, error) {
	return "", nil
}

func (m *Matrix) StatusUsers(ctx context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (m *Matrix) IsOnline(status string) bool {
	return status == "online"
}

func (m *Matrix) Protocol() string {
	return "matrix"
}

func (m *Matrix) Kick(ctx context.Context, channelID, username string) error {
	return nil
}

func (m *Matrix) SetStatus(ctx context.Context, status string) error {
	return nil
}

func (m *Matrix) Nick(ctx context.Context, name string) error {
	return nil
}

func (m *Matrix) GetChannelName(ctx context.Context, channelID string) string {
	for _, channel := range m.GetChannels() {
		if channel.ID == channelID {
			return channel.Name
		}
	}

	return channelID
}

func (m *Matrix) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	var users []*bridge.UserInfo

	ids, err := m.mc.JoinedMembers(ctx, id.RoomID(channelID))
	if err != nil {
		return nil, err
	}

	logger.Debugf("GetChannelUsers %s %d", channelID, len(ids))

	for _, userID := range ids {
		if u := m.createUser(userID); u != nil {
			users = append(users, u)
		}
	}

	return users, nil
}

func (m *Matrix) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	userIDs := m.mc.KnownUsers()

	logger.Debugf("GetUsers %d", len(userIDs))

	for _, userID := range userIDs {
		if u := m.createUser(userID); u != nil {
			users = append(users, u)
		}
	}

	return users
}

func (m *Matrix) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	for _, room := range m.mc.Rooms() {
		channels = append(channels, &bridge.ChannelInfo{
			Name:    room.Name,
			ID:      room.ID.String(),
			DM:      room.IsDirect,
			Private: false,
		})

		logger.Debugf("GetChannels %s (%s)", room.Name, room.ID.String())
	}

	return channels
}

func (m *Matrix) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	for _, channel := range m.GetChannels() {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (m *Matrix) GetChannelCount() int {
	return len(m.GetChannels())
}

func (m *Matrix) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return m.createUser(id.UserID(userID))
}

func (m *Matrix) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	return []string{}, nil
}

func (m *Matrix) GetUserCount() int {
	return len(m.mc.KnownUsers())
}

func (m *Matrix) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.UserID())
}

func (m *Matrix) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (m *Matrix) createUser(userID id.UserID) *bridge.UserInfo {
	localpart, homeserver, err := userID.Parse()
	if err != nil {
		return nil
	}

	displayName := m.mc.DisplayName(userID)
	if displayName == "" {
		displayName = localpart + "@" + homeserver
	}

	// Drop the homeserver/host part if it's a user on the same homeserver as us.
	nick := localpart + "@" + homeserver

	_, myHomeserver, err := m.mc.UserID().Parse()
	if err == nil && homeserver == myHomeserver {
		nick = localpart
	}

	return &bridge.UserInfo{
		Nick:     nick,
		User:     "~" + localpart,
		Real:     displayName,
		Host:     homeserver,
		Ghost:    true,
		Me:       userID == m.mc.UserID(),
		Username: nick,
	}
}

func (m *Matrix) addParentMsg(ctx context.Context, roomID id.RoomID, parentID id.EventID, msg string, newLen int, uncounted string) (string, error) {
	// Search and use cached reply if it exists.
	replyMessage, ok := m.msgParentCache.Get(parentID)
	if ok {
		logger.Debugf("Found saved reply for parent post %s, using:%s", parentID, replyMessage)

		return strings.TrimRight(msg, "\n") + replyMessage, nil
	}

	// None found, so we'll need to create one and save it for future uses.
	replyMessage, err := m.createParentReply(ctx, roomID, parentID, newLen, uncounted)
	if err != nil {
		return msg, err
	}

	logger.Debugf("Created reply for parent post %s:%s", parentID.String(), replyMessage)
	m.msgParentCache.Add(parentID, replyMessage)

	return strings.TrimRight(msg, "\n") + replyMessage, nil
}

func (m *Matrix) createParentReply(ctx context.Context, roomID id.RoomID, parentID id.EventID, newLen int, uncounted string) (string, error) {
	resp, err := m.mc.Event(ctx, roomID, parentID)

	if err != nil {
		return "", err
	}

	body, _ := resp.Content.Raw["body"].(string)

	nick := resp.Sender.String()
	if parentUser := m.GetUser(ctx, resp.Sender.String()); parentUser != nil {
		nick = parentUser.Nick
	}

	return " (re @" + nick + ": " + m.formatSummary(body, newLen, uncounted) + ")", nil
}

func (m *Matrix) GetTeamName(ctx context.Context, teamID string) string {
	return ""
}

func (m *Matrix) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (m *Matrix) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Matrix) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Matrix) GetPostChannelID(ctx context.Context, postID string) string {
	return ""
}

func (m *Matrix) GetPostSizeLimit() int {
	return 0
}

func (m *Matrix) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Matrix) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Matrix) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Matrix) UpdateLastViewed(ctx context.Context, channelID string) {
}

func (m *Matrix) UpdateLastViewedUser(ctx context.Context, userID string) error {
	return nil
}

func (m *Matrix) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	return []*bridge.File{}
}

func (m *Matrix) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (m *Matrix) GetChannelID(ctx context.Context, name, teamID string) string {
	for _, channel := range m.GetChannels() {
		if channel.Name == name {
			return channel.ID
		}
	}

	return ""
}

func (m *Matrix) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	return nil, errors.New("not implemented yet")
}

func (m *Matrix) GetDMChannelID(ctx context.Context, userID string) string {
	return m.mc.DirectRoom(id.UserID(userID)).String()
}

func (m *Matrix) GetDMChannelName(userID1 string, userID2 string) string {
	return ""
}

func (m *Matrix) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	users := m.mc.DirectUsers(id.RoomID(channelName))
	if len(users) != 1 {
		return nil
	}

	return m.createUser(users[0])
}

func (m *Matrix) GetDMUserIDs(channelName string) (string, string, bool) {
	return "", "", false
}

func (m *Matrix) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	return "", false
}

func (m *Matrix) IsDMChannelName(channelName string) bool {
	return m.mc.IsDirect(id.RoomID(channelName))
}

func (m *Matrix) Connected() bool {
	return m.connected.Load()
}

func (m *Matrix) AddReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (m *Matrix) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (m *Matrix) GetLastSentMsgs() []string {
	data := make([]string, 0)

	for _, k := range m.msgLastSentCache.Keys() {
		if v, ok := m.msgLastSentCache.Get(k); ok {
			data = append(data, fmt.Sprintf("[@@%s] %s", k, v))
		}
	}

	return data
}

func (m *Matrix) Config() any {
	return &m.cfg.Current().Matrix
}

func (m *Matrix) BridgeConfig() *config.BridgeConfig {
	return &m.cfg.Current().Matrix.Bridge
}

func (m *Matrix) FormatterConfig() *config.FormatterConfig {
	return &m.cfg.Current().Matrix.Formatter
}

func (m *Matrix) Ping(ctx context.Context, proto ...string) error {
	return nil
}

func (m *Matrix) NormalizePostID(msgID string) string {
	return msgID
}

func (m *Matrix) SendTyping(ctx context.Context, channelName string) {
}

// formatSummary renders msg like the Mattermost bridge does for replies: markdown
// to IRC formatting, emoji aliases replaced, then shortened to maxLen.
func (m *Matrix) formatSummary(msg string, maxLen int, uncounted string) string {
	// Single config snapshot so a reload cannot change settings mid-format.
	fc := &m.cfg.Matrix().Formatter
	unicode := fc.Unicode

	if !fc.DisableMarkdown {
		msg = markdownReplacer.Replace(msg)

		blockquoteChar := blockquoteCharNonUnicode
		if unicode {
			blockquoteChar = blockquoteCharUnicode
		}

		msg = utils.Markdown2irc(msg, blockquoteChar, fc.MarkdownInlineCode)
	} else {
		msg = strings.ReplaceAll(msg, "\n", " ")
	}

	if !fc.DisableEmoji {
		msg = utils.EmojiReplaceAliases(msg, fc.CustomEmoji)
	}

	return utils.FormatAndShortenSummary(msg, utils.SummaryOpts{
		DisableMarkdown: true,
		DisableEmoji:    true,
		MaxLength:       maxLen,
		UncountedPrefix: uncounted,
		Unicode:         unicode,
	})
}
