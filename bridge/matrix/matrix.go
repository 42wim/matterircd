package matrix

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/matrixclient"
	"github.com/sirupsen/logrus"
)

// logger is shared by all users, so creating it once avoids racing on a
// per-login replacement. It matches the format of the matterclient logging.
var logger = newLogger()

func newLogger() *logrus.Entry {
	l := logrus.New()
	l.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})

	return l.WithFields(logrus.Fields{"prefix": "bridge/matrix"})
}

const (
	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"

	lastSentSummaryLen = 90

	searchUsersLimit  = 50
	msgRoomCacheSize  = 2000
	reactionCacheSize = 100
	sentCacheSize     = 100
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
	ready     atomic.Bool

	ctx context.Context //nolint:containedctx

	msgParentCache   *lru.Cache[id.EventID, string]
	msgLastSentCache *lru.Cache[string, string]
	msgRoomCache     *lru.Cache[id.EventID, id.RoomID]
	reactionCache    *lru.Cache[string, id.EventID]

	lastEvent sync.Map // id.RoomID -> id.EventID, the newest event seen per room
	lastPost  sync.Map // id.RoomID -> int64, the timestamp (ms) of the newest message seen per room

	// sentMu is never held across network I/O. It guards sentCache and pendingSends,
	// which together recognize the echo of a message sent from IRC, also when the
	// echo is synced before the send call returned its event ID.
	sentMu       sync.Mutex
	sentCache    *lru.Cache[id.EventID, struct{}]
	pendingSends map[string]int // text of the sends in flight
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
	m.msgRoomCache, _ = lru.New[id.EventID, id.RoomID](msgRoomCacheSize)
	m.pendingSends = make(map[string]int)
	m.sentCache, _ = lru.New[id.EventID, struct{}](sentCacheSize)
	m.reactionCache, _ = lru.New[string, id.EventID](reactionCacheSize)

	setLogLevel := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			logger.Logger.SetLevel(logrus.TraceLevel)
		case rc.Debug:
			logger.Logger.SetLevel(logrus.DebugLevel)
		default:
			logger.Logger.SetLevel(logrus.InfoLevel)
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
	}, m, m.encryptionOptions(cred)...)
	if err != nil {
		return nil, nil, err
	}

	m.mc = mc
	m.connected.Store(true)

	return m, mc, nil
}

// Ready implements matrixclient.Handler.
func (m *Matrix) Ready() {
	m.ready.Store(true)

	go m.onConnect()
}

func (m *Matrix) sendEvent(ev *bridge.Event) {
	// The event loop only starts from onConnect, which Ready triggers. Blocking
	// the sync goroutine on the unbuffered channel before that would deadlock the
	// initial sync, so its events are dropped, history is replayed on join.
	if !m.ready.Load() {
		return
	}

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

	if ghost.Me {
		if m.isSentEcho(msg) {
			return
		}
	}

	m.msgRoomCache.Add(msg.EventID, msg.RoomID)
	m.lastEvent.Store(msg.RoomID, msg.EventID)

	prev, _ := m.lastPost.Load(msg.RoomID)
	if prevTS, _ := prev.(int64); msg.Timestamp > prevTS {
		m.lastPost.Store(msg.RoomID, msg.Timestamp)
	}

	text := msg.Text

	file := m.fileFromMessage(msg)
	if file != nil {
		m.sendFile(ghost, msg, file)

		if !hasCaption(msg) {
			return
		}
	}

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies && msg.ParentID != "" {
		message, err := m.addParentMsg(m.ctx, msg.RoomID, msg.ParentID, text, mxCfg.ShortenRepliesTo, "@")
		if err != nil {
			logger.Errorf("Unable to get parent post for %s: %s", msg.EventID, err)
		}

		text = message
	}

	m.sendText(ghost, msg.RoomID, msg.Direct, msg.EventID, msg.ParentID, text, "", msg.Timestamp)
}

func (m *Matrix) sendFile(ghost *bridge.UserInfo, msg *matrixclient.Message, file *bridge.File) {
	channelType := ""
	if msg.Direct {
		channelType = "D"
	}

	receiver := m.dmReceiver(ghost, msg.RoomID, msg.Direct)
	if receiver == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Receiver:    receiver,
			Sender:      ghost,
			ChannelID:   msg.RoomID.String(),
			ChannelType: channelType,
			Files:       []*bridge.File{file},
			MessageID:   msg.EventID.String(),
			ParentID:    msg.ParentID.String(),
		},
	})
}

// OnReaction implements matrixclient.Handler.
func (m *Matrix) OnReaction(r *matrixclient.Reaction) {
	m.msgRoomCache.Add(r.ParentID, r.RoomID)

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

	receiver := m.dmReceiver(ghost, r.RoomID, r.Direct)
	if receiver == nil {
		return
	}

	// The reacted message is the MessageID, a ParentID would show it as a reply to itself.
	m.sendEvent(&bridge.Event{
		Type: "reaction_add",
		Data: &bridge.ReactionAddEvent{
			ChannelID:   r.RoomID.String(),
			MessageID:   string(r.ParentID),
			Receiver:    receiver,
			Sender:      ghost,
			Reaction:    r.Key,
			ChannelType: channelType,
			Message:     text,
		},
	})
}

func (m *Matrix) Invite(ctx context.Context, channelID, username string) error {
	userID, ok := m.resolveUserID(username)
	if !ok {
		return errors.New("unknown user " + username)
	}

	return m.mc.Invite(ctx, id.RoomID(channelID), userID)
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
	// "space/room" joins a room of a space we know of.
	if spaceName, name, ok := strings.Cut(channelName, "/"); ok && !strings.HasPrefix(channelName, "#") {
		if spaceID := m.mc.TeamByName(spaceName); spaceID != "" {
			channelID := m.GetChannelID(ctx, name, spaceID.String())
			if channelID == "" {
				return "", "", errors.New("room " + name + " not found in " + spaceName)
			}

			return channelID, spaceID.String(), nil
		}
	}

	// CmdJoin strips the leading '#' of an alias like "#room:server".
	target := channelName
	if !strings.HasPrefix(target, "!") && !strings.HasPrefix(target, "#") {
		if strings.Contains(target, ":") {
			target = "#" + target
		} else if channelID := m.GetChannelID(ctx, target, ""); channelID != "" {
			target = channelID
		}
	}

	roomID, err := m.mc.Join(ctx, target)
	if err != nil {
		return "", "", err
	}

	return roomID.String(), "", nil
}

func (m *Matrix) List(ctx context.Context) (map[string]string, error) {
	rooms := make(map[string]string)

	for _, room := range m.mc.Rooms() {
		if room.IsDirect {
			continue
		}

		roomName := strings.TrimPrefix(room.Name, "#")

		name := "#" + roomName
		if teams := m.mc.RoomTeams(room.ID); len(teams) > 0 {
			name = m.GetTeamName(ctx, teams[0].String()) + "/" + roomName
		}

		rooms[name] = strings.ReplaceAll(m.mc.Topic(room.ID), "\n", " | ")
	}

	return rooms, nil
}

func (m *Matrix) Part(ctx context.Context, channelID string) error {
	return m.mc.Leave(ctx, id.RoomID(channelID))
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

	uid, ok := m.resolveUserID(userID)
	if !ok {
		return "", errors.New("unknown user " + userID)
	}

	m.beginSend(text)

	eventID, err := m.mc.SendDirect(ctx, uid, id.EventID(parentID), text)
	m.endSend(text, eventID, err)

	if err != nil {
		logger.Error("msguserthread sending message: error ", err)
		return "", err
	}

	if roomID := m.mc.DirectRoom(uid); roomID != "" {
		m.msgRoomCache.Add(eventID, roomID)
	}

	return m.recordSent(ctx, "", eventID, text), nil
}

func (m *Matrix) beginSend(text string) {
	m.sentMu.Lock()
	m.pendingSends[text]++
	m.sentMu.Unlock()
}

func (m *Matrix) endSend(text string, eventID id.EventID, err error) {
	m.sentMu.Lock()
	defer m.sentMu.Unlock()

	if m.pendingSends[text] <= 1 {
		delete(m.pendingSends, text)
	} else {
		m.pendingSends[text]--
	}

	if err == nil {
		m.sentCache.Add(eventID, struct{}{})
	}
}

// isSentEcho reports whether our own message was sent from IRC. A message with
// the text of a send in flight is taken as its echo, as the event ID isn't known yet.
func (m *Matrix) isSentEcho(msg *matrixclient.Message) bool {
	m.sentMu.Lock()
	defer m.sentMu.Unlock()

	if _, ok := m.sentCache.Get(msg.EventID); ok {
		return true
	}

	return m.pendingSends[msg.Text] > 0
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

	m.beginSend(text)

	eventID, err := m.mc.Send(ctx, id.RoomID(channelID), id.EventID(parentID), text)
	m.endSend(text, eventID, err)

	if err != nil {
		return "", err
	}

	m.msgRoomCache.Add(eventID, id.RoomID(channelID))

	return m.recordSent(ctx, channelID, eventID, text), nil
}

func (m *Matrix) ModifyPost(ctx context.Context, msgID, text string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	m.msgParentCache.Remove(id.EventID(msgID))

	if text == "" {
		return m.mc.Delete(ctx, roomID, id.EventID(msgID))
	}

	_, err := m.mc.Edit(ctx, roomID, id.EventID(msgID), text)

	return err
}

func (m *Matrix) Topic(ctx context.Context, channelID string) string {
	return m.mc.Topic(id.RoomID(channelID))
}

func (m *Matrix) SetTopic(ctx context.Context, channelID, text string) error {
	return m.mc.SetTopic(ctx, id.RoomID(channelID), text)
}

func (m *Matrix) StatusUser(ctx context.Context, userID string) (string, error) {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return "", nil
	}

	err := m.mc.UpdateStatus(ctx, uid)
	if err != nil {
		return "", err
	}

	status := m.mc.GetStatus(uid)
	if custom := m.mc.GetUserCustomStatus(uid); custom != "" {
		status += ": " + custom
	}

	if localTime := m.mc.GetUserLocalTime(ctx, uid); localTime != "" {
		status += " (local time: " + localTime + ")"
	}

	return status, nil
}

func (m *Matrix) HasCustomStatus(userID string) bool {
	uid, ok := m.resolveUserID(userID)

	return ok && m.mc.GetUserCustomStatus(uid) != ""
}

func (m *Matrix) StatusUsers(ctx context.Context) (map[string]string, error) {
	statuses := make(map[string]string)

	for _, userID := range m.mc.KnownUsers() {
		statuses[userID.String()] = m.mc.GetStatus(userID)
	}

	return statuses, nil
}

func (m *Matrix) IsOnline(status string) bool {
	return status == "online"
}

func (m *Matrix) Protocol() string {
	return "matrix"
}

func (m *Matrix) Kick(ctx context.Context, channelID, username string) error {
	return errors.New("kick is not supported by matrixclient yet")
}

func (m *Matrix) SetStatus(ctx context.Context, status string) error {
	return m.mc.SetStatus(ctx, status, "")
}

func (m *Matrix) Nick(ctx context.Context, name string) error {
	return m.mc.SetDisplayName(ctx, name)
}

func (m *Matrix) GetChannelName(ctx context.Context, channelID string) string {
	for _, channel := range m.GetChannels() {
		if channel.ID == channelID {
			if channel.DM {
				return channel.Name
			}

			return "#" + channel.Name
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
		teamID := ""
		if teams := m.mc.RoomTeams(room.ID); len(teams) > 0 && !room.IsDirect {
			teamID = teams[0].String()
		}

		ts, _ := m.lastPost.Load(room.ID)
		lastPostAt, _ := ts.(int64)

		channels = append(channels, &bridge.ChannelInfo{
			Name:       strings.TrimPrefix(room.Name, "#"),
			ID:         room.ID.String(),
			TeamID:     teamID,
			DM:         room.IsDirect,
			Private:    false,
			LastPostAt: lastPostAt,
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
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return nil
	}

	return m.createUser(uid)
}

func (m *Matrix) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return []string{}, nil
	}

	var names []string

	for _, room := range m.mc.Rooms() {
		if room.IsDirect {
			continue
		}

		members, err := m.mc.RoomUsers(ctx, room.ID)
		if err == nil && slices.Contains(members, uid) {
			names = append(names, room.Name)
		}
	}

	return names, nil
}

func (m *Matrix) GetUserCount() int {
	return len(m.mc.KnownUsers())
}

func (m *Matrix) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.UserID())
}

func (m *Matrix) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	userID, ok := m.resolveUserID(username)
	if !ok {
		return nil
	}

	return m.createUser(userID)
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

	// The IRC user is the key of a ghost. It always has the homeserver, as '/' is
	// legal in a localpart and would otherwise collide with a remote user.
	ident := "~" + localpart + "/" + homeserver

	_, myHomeserver, err := m.mc.UserID().Parse()
	if err == nil && homeserver == myHomeserver {
		nick = localpart
	}

	return &bridge.UserInfo{
		Nick:     nick,
		User:     ident,
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
	for _, team := range m.mc.Teams() {
		if team.ID.String() == teamID {
			return team.Name
		}
	}

	return ""
}

// GetLastViewedAt is always 0, Matrix read markers cannot be fetched with matrixclient.
func (m *Matrix) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (m *Matrix) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return m.historySince(ctx, channelID, since, "scrollback")
}

func (m *Matrix) historySince(ctx context.Context, channelID string, since int64, eventType string) []*bridge.Event {
	msgs, err := m.mc.GetMessagesSince(ctx, id.RoomID(channelID), since)
	if err != nil {
		logger.Errorf("matrix: fetching messages of %s: %s", channelID, err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, eventType)
}

func (m *Matrix) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	msgs, err := m.mc.GetMessages(ctx, id.RoomID(channelID), limit)
	if err != nil {
		logger.Errorf("matrix: fetching messages of %s: %s", channelID, err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "scrollback")
}

func (m *Matrix) GetPostChannelID(ctx context.Context, postID string) string {
	roomID, ok := m.roomOf(postID)
	if !ok {
		return ""
	}

	return roomID.String()
}

func (m *Matrix) GetPostSizeLimit() int {
	// Matrix limits event PDUs to 65,536 bytes (64 KiB). Accounting for worst-case
	// JSON escaping (\u003c) and Megolm base64 ciphertext expansion (~8x), a conservative
	// 4,000 raw byte cap guarantees encrypted room posts stay well within homeserver limits.
	return 4000
}

func (m *Matrix) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	roomID, ok := m.roomOf(postID)
	if !ok {
		return []*bridge.Event{}
	}

	msgs, err := m.mc.GetThread(ctx, roomID, id.EventID(postID))
	if err != nil {
		logger.Errorf("matrix: fetching thread %s: %s", postID, err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "details")
}

func (m *Matrix) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return m.historySince(ctx, channelID, since, "replay")
}

func (m *Matrix) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	msgs, err := m.mc.SearchMessages(ctx, search)
	if err != nil {
		logger.Errorf("matrix: searching messages: %s", err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "search")
}

func (m *Matrix) UpdateLastViewed(ctx context.Context, channelID string) {
	roomID := id.RoomID(channelID)

	eventID, ok := m.lastEvent.Load(roomID)
	if !ok {
		return
	}

	evID, _ := eventID.(id.EventID)

	sendLogErr("marking "+channelID+" as read", m.mc.UpdateLastViewed(ctx, roomID, evID))
}

func (m *Matrix) UpdateLastViewedUser(ctx context.Context, userID string) error {
	roomID := m.dmRoom(userID)
	if roomID == "" {
		return nil
	}

	m.UpdateLastViewed(ctx, roomID.String())

	return nil
}

// GetFilesInfo takes mxc:// URIs as file IDs.
func (m *Matrix) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	files := make([]*bridge.File, 0, len(fileIDs))

	for _, fileID := range fileIDs {
		link := m.mc.PublicLink(id.ContentURIString(fileID))
		if link != "" {
			files = append(files, &bridge.File{Name: fileID, URL: link})
		}
	}

	return files
}

func (m *Matrix) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	found, err := m.mc.SearchUsers(ctx, query, searchUsersLimit)
	if err != nil {
		return nil, err
	}

	users := make([]*bridge.UserInfo, 0, len(found))

	for _, u := range found {
		if info := m.createUser(u.ID); info != nil {
			users = append(users, info)
		}
	}

	return users, nil
}

func (m *Matrix) GetChannelID(ctx context.Context, name, teamID string) string {
	name = strings.TrimPrefix(name, "#")

	for _, channel := range m.GetChannels() {
		if channel.ID != name && channel.Name != name {
			continue
		}

		if teamID == "" || channel.ID == name || m.inTeam(channel.ID, teamID) {
			return channel.ID
		}
	}

	// A user resolves to the direct room with them.
	if roomID := m.dmRoom(name); roomID != "" {
		return roomID.String()
	}

	return ""
}

func (m *Matrix) inTeam(channelID, teamID string) bool {
	return slices.Contains(m.mc.RoomTeams(id.RoomID(channelID)), id.RoomID(teamID))
}

func (m *Matrix) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	roomID, err := m.mc.CreateRoom(ctx, strings.TrimPrefix(channelName, "#"), "", channelType == "O")
	if err != nil {
		return nil, err
	}

	return &bridge.ChannelInfo{Name: channelName, ID: roomID.String(), Private: channelType != "O"}, nil
}

func (m *Matrix) GetDMChannelID(ctx context.Context, userID string) string {
	return m.dmRoom(userID).String()
}

// GetDMChannelName returns the room ID, which GetChannelID resolves again.
func (m *Matrix) GetDMChannelName(userID1 string, userID2 string) string {
	return m.dmRoom(userID2).String()
}

func (m *Matrix) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	users := m.mc.DirectUsers(id.RoomID(channelName))
	if len(users) != 1 {
		return nil
	}

	return m.createUser(users[0])
}

func (m *Matrix) GetDMUserIDs(channelName string) (string, string, bool) {
	other := m.dmPeer(id.RoomID(channelName))
	if other == nil {
		return "", "", false
	}

	return m.GetMe().User, other.User, true
}

func (m *Matrix) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	other := m.dmPeer(id.RoomID(channelName))
	if other == nil {
		return "", false
	}

	return other.User, true
}

func (m *Matrix) IsDMChannelName(channelName string) bool {
	return m.mc.IsDirect(id.RoomID(channelName))
}

func (m *Matrix) Connected() bool {
	return m.connected.Load()
}

func (m *Matrix) reactionKey(emoji string) string {
	emoji = strings.Trim(emoji, ":")

	if unicode, ok := utils.EmojiFromAlias(emoji, m.cfg.Matrix().Formatter.CustomEmoji); ok {
		return unicode
	}

	return emoji
}

func (m *Matrix) AddReaction(ctx context.Context, msgID, emoji string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	key := m.reactionKey(emoji)

	reactionID, err := m.mc.React(ctx, roomID, id.EventID(msgID), key)
	if err != nil {
		return err
	}

	m.reactionCache.Add(msgID+"\x00"+key, reactionID)

	return nil
}

func (m *Matrix) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	cacheKey := msgID + "\x00" + m.reactionKey(emoji)

	reactionID, ok := m.reactionCache.Get(cacheKey)
	if !ok {
		return errors.New("reaction was not added by this session")
	}

	err := m.mc.DeleteReaction(ctx, roomID, reactionID)
	if err != nil {
		return err
	}

	m.reactionCache.Remove(cacheKey)

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
	return m.mc.Ping(ctx)
}

func (m *Matrix) NormalizePostID(msgID string) string {
	if strings.HasPrefix(msgID, "$") {
		return msgID
	}

	return ""
}

func (m *Matrix) SendTyping(ctx context.Context, channelName string) {
	rc := m.cfg.Current()
	if !rc.EnableTyping || rc.DisableSendTyping || strings.HasPrefix(channelName, "&") {
		return
	}

	var roomID id.RoomID

	if postID, ok := strings.CutPrefix(channelName, "@@"); ok {
		roomID, _ = m.roomOf(postID)
	} else if roomID = id.RoomID(m.GetChannelID(ctx, channelName, "")); roomID == "" {
		if userID, found := m.resolveUserID(channelName); found {
			roomID = m.mc.DirectRoom(userID)
		}
	}

	if roomID == "" {
		return
	}

	sendLogErr("sending typing to "+roomID.String(), m.mc.SendTyping(ctx, roomID))
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
