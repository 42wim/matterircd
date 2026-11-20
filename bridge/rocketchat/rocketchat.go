package rocketchat

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/rocketchatclient"
	"github.com/sirupsen/logrus"
)

const defaultPostSizeLimit = 5000

var logger = newLogger()

func newLogger() *logrus.Entry {
	l := logrus.New()
	l.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	return l.WithFields(logrus.Fields{"prefix": "bridge/rocketchat"})
}

// RocketChat implements bridge.Bridger and rocketchatclient.Handler.
type RocketChat struct {
	rc        *rocketchatclient.Client
	eventChan chan *bridge.Event
	cfg       *config.Config
	onConnect func()

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	initDone chan struct{}
	readyCh  chan struct{}

	connected atomic.Bool
	ready     atomic.Bool

	unregisterReloadHook func()
}

// New authenticates with Rocket.Chat, starts the DDP event loop, and returns a bridge.Bridger.
func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, *rocketchatclient.Client, error) {
	rcCfg := cfg.RocketChat()

	server := cmp.Or(cred.Server, rcCfg.Server)
	if server == "" {
		return nil, nil, errors.New("rocketchat: server is required")
	}

	bridgeCtx, cancel := context.WithCancel(ctx)

	r := &RocketChat{
		eventChan: eventChan,
		cfg:       cfg,
		onConnect: onConnect,
		ctx:       bridgeCtx,
		cancel:    cancel,
		initDone:  make(chan struct{}),
		readyCh:   make(chan struct{}),
	}

	defer close(r.initDone)

	var opts []rocketchatclient.Option
	if config.UserAgent != "" {
		opts = append(opts, rocketchatclient.WithUserAgent(config.UserAgent))
	}

	rcCred := rocketchatclient.Credentials{
		Server:        server,
		Login:         cred.Login,
		Pass:          cred.Pass,
		Token:         cred.Token,
		TwoFactorCode: cred.MFAToken,
		NoTLS:         rcCfg.Insecure,
		SkipTLSVerify: rcCfg.SkipTLSVerify,
	}

	rc, err := rocketchatclient.New(bridgeCtx, rcCred, r, opts...)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	r.rc = rc

	setLogLevels := makeLogLevelSetter(rc)
	setLogLevels(cfg.Current())
	r.unregisterReloadHook = cfg.RegisterReloadHook(setLogLevels)
	r.connected.Store(true)

	return r, rc, nil
}

func makeLogLevelSetter(rc *rocketchatclient.Client) func(*config.RuntimeConfig) {
	return func(runtimeCfg *config.RuntimeConfig) {
		switch {
		case runtimeCfg.Trace:
			logger.Logger.SetLevel(logrus.TraceLevel)
		case runtimeCfg.Debug:
			logger.Logger.SetLevel(logrus.DebugLevel)
		default:
			logger.Logger.SetLevel(logrus.InfoLevel)
		}

		rc.SetLogLevel("info")

		if runtimeCfg.RocketChat.RocketchatclientLogLevel != "" {
			logger.Infof("enabling rocketchatclient logging: level: %s", runtimeCfg.RocketChat.RocketchatclientLogLevel)
			rc.SetLogLevel(strings.ToLower(runtimeCfg.RocketChat.RocketchatclientLogLevel))
		}

		rc.SetLogAPICalls("error")

		if runtimeCfg.Profiling {
			if runtimeCfg.Trace {
				rc.SetLogAPICalls("trace")
			} else if runtimeCfg.Debug {
				rc.SetLogAPICalls("warn")
			}
		}
	}
}

// Ready implements rocketchatclient.Handler.
func (r *RocketChat) Ready() {
	select {
	case <-r.initDone:
	case <-r.ctx.Done():
		return
	}

	if r.rc == nil {
		return
	}

	r.ready.Store(true)
	close(r.readyCh)

	if r.onConnect != nil {
		go r.onConnect()
	}
}

// OnReconnect implements rocketchatclient.ReconnectHandler.
func (r *RocketChat) OnReconnect(downtime time.Duration) {
	if !r.ready.Load() || r.ctx.Err() != nil || r.onConnect == nil {
		return
	}

	thresh := r.cfg.RocketChat().HeavySyncThreshold
	if thresh <= 0 {
		thresh = 15 * time.Minute
	}

	if downtime < thresh {
		return
	}

	logger.Infof("rocketchat sync recovered after %s, triggering channel synchronization", downtime.Round(time.Second))

	go r.onConnect()
}

// OnMessage implements rocketchatclient.Handler.
func (r *RocketChat) OnMessage(msg *rocketchatclient.Message) {
	r.dispatchMessage(msg, "")
}

// OnEdit implements rocketchatclient.Handler.
func (r *RocketChat) OnEdit(msg *rocketchatclient.Message) {
	r.dispatchMessage(msg, "edited")
}

// OnDelete implements rocketchatclient.Handler.
func (r *RocketChat) OnDelete(roomID, msgID string) {
	logger.Debugf("onDelete room=%s msg=%s", roomID, msgID)
}

// OnReaction implements rocketchatclient.Handler.
func (r *RocketChat) OnReaction(ev *rocketchatclient.ReactionEvent) {
	logger.Debugf("onReaction room=%s msg=%s emoji=%s added=%v", ev.RoomID, ev.MessageID, ev.Emoji, ev.Added)
}

// OnRoomChange implements rocketchatclient.Handler.
func (r *RocketChat) OnRoomChange(room *rocketchatclient.Room) {
	if !r.ready.Load() || room == nil || room.ID == "" {
		return
	}

	if r.cfg.RocketChat().ShowTopic && room.Topic != "" {
		userID := ""
		if room.Owner != nil {
			userID = room.Owner.ID
		}

		r.sendEvent(&bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				ChannelID: room.ID,
				Text:      room.Topic,
				UserID:    userID,
			},
		})
	}
}

// OnPresence implements rocketchatclient.Handler.
func (r *RocketChat) OnPresence(userID, status string) {
	if !r.ready.Load() || userID == "" {
		return
	}

	r.sendEvent(&bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: userID,
			Status: status,
		},
	})
}

func (r *RocketChat) dispatchMessage(msg *rocketchatclient.Message, eventType string) {
	if !r.ready.Load() || msg == nil || msg.RoomID == "" {
		return
	}

	sender := r.senderInfo(msg.User)
	createAt := msg.Timestamp.Time.UnixMilli()

	room, ok := r.rc.Room(msg.RoomID)
	if ok && isOneToOneDM(room) {
		peer := r.dmPeer(room)
		if peer == nil {
			peer = sender
		}

		r.sendEvent(&bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      msg.Text,
				ChannelID: msg.RoomID,
				Receiver:  peer,
				Sender:    sender,
				MessageID: msg.ID,
				Event:     eventType,
				ParentID:  msg.ThreadID,
				CreateAt:  createAt,
			},
		})

		return
	}

	r.sendEvent(&bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        msg.Text,
			ChannelID:   msg.RoomID,
			Sender:      sender,
			ChannelType: channelType(room),
			MessageID:   msg.ID,
			Event:       eventType,
			ParentID:    msg.ThreadID,
			CreateAt:    createAt,
		},
	})
}

func (r *RocketChat) sendEvent(ev *bridge.Event) {
	if !r.ready.Load() {
		return
	}

	select {
	case r.eventChan <- ev:
	case <-r.ctx.Done():
	}
}

func (r *RocketChat) senderInfo(ref rocketchatclient.UserRef) *bridge.UserInfo {
	if ref.ID != "" {
		if u := r.GetUser(r.ctx, ref.ID); u != nil {
			return u
		}
	}

	nick := cmp.Or(ref.Username, ref.ID)
	realName := cmp.Or(ref.Name, nick)

	return &bridge.UserInfo{
		Nick:      nick,
		User:      ref.ID,
		Real:      realName,
		Username:  ref.Username,
		FirstName: ref.Name,
		Ghost:     ref.ID != r.rc.UserID(),
		Me:        ref.ID == r.rc.UserID(),
	}
}

func (r *RocketChat) toBridgeUser(u *rocketchatclient.User) *bridge.UserInfo {
	if u == nil {
		return nil
	}

	nick := cmp.Or(u.Username, u.ID)
	realName := cmp.Or(u.Name, nick)
	meID := r.rc.UserID()

	return &bridge.UserInfo{
		Nick:        nick,
		User:        u.ID,
		Real:        realName,
		Roles:       strings.Join(u.Roles, ","),
		DisplayName: u.Name,
		Ghost:       u.ID != meID,
		Me:          u.ID == meID,
		Username:    u.Username,
		FirstName:   u.Name,
	}
}

func isOneToOneDM(room *rocketchatclient.Room) bool {
	return room != nil && room.Type == rocketchatclient.RoomTypeDirect && len(room.UIDs) == 2
}

func channelType(room *rocketchatclient.Room) string {
	if room == nil {
		return "O"
	}

	switch room.Type {
	case rocketchatclient.RoomTypeDirect:
		return "D"
	case rocketchatclient.RoomTypePrivate:
		return "P"
	default:
		return "O"
	}
}

func (r *RocketChat) roomName(room *rocketchatclient.Room) string {
	if room == nil {
		return ""
	}

	if isOneToOneDM(room) {
		return room.ID
	}

	if room.Type == rocketchatclient.RoomTypeDirect {
		return r.groupDMName(room)
	}

	return cmp.Or(room.Name, room.FName, room.ID)
}

func (r *RocketChat) groupDMName(room *rocketchatclient.Room) string {
	if name := cmp.Or(room.Name, room.FName); name != "" {
		return name
	}

	meID := r.rc.UserID()
	names := make([]string, 0, len(room.UIDs))

	for _, uid := range room.UIDs {
		if uid == meID {
			continue
		}

		if u, ok := r.rc.User(uid); ok && u.Username != "" {
			names = append(names, u.Username)
		} else {
			names = append(names, uid)
		}
	}

	if len(names) > 0 {
		slices.Sort(names)

		return "dm-" + strings.Join(names, "-")
	}

	return room.ID
}

func (r *RocketChat) toChannelInfo(room *rocketchatclient.Room) *bridge.ChannelInfo {
	if room == nil {
		return nil
	}

	var lastPost int64
	if !room.LastMessage.Time.IsZero() {
		lastPost = room.LastMessage.Time.UnixMilli()
	}

	return &bridge.ChannelInfo{
		Name:        r.roomName(room),
		ID:          room.ID,
		DM:          room.Type == rocketchatclient.RoomTypeDirect,
		Private:     room.Type == rocketchatclient.RoomTypePrivate,
		DisplayName: cmp.Or(room.FName, room.Name),
		LastPostAt:  lastPost,
	}
}

func (r *RocketChat) dmPeer(room *rocketchatclient.Room) *bridge.UserInfo {
	if !isOneToOneDM(room) {
		return nil
	}

	meID := r.rc.UserID()
	for _, uid := range room.UIDs {
		if uid != meID {
			return r.GetUser(r.ctx, uid)
		}
	}

	return nil
}

func (r *RocketChat) Protocol() string {
	return "rocketchat"
}

func (r *RocketChat) Connected() bool {
	return r.connected.Load() && r.rc != nil && r.rc.Connected()
}

func (r *RocketChat) Logout(ctx context.Context) error {
	r.connected.Store(false)

	if r.unregisterReloadHook != nil {
		r.unregisterReloadHook()
	}

	if r.rc != nil {
		r.rc.Logout()
	}

	r.cancel()

	return nil
}

func (r *RocketChat) Ping(ctx context.Context, proto ...string) error {
	if !r.Connected() {
		return errors.New("rocketchat: not connected")
	}

	return nil
}

func (r *RocketChat) GetMe() *bridge.UserInfo {
	if u := r.rc.Me(); u != nil {
		return r.toBridgeUser(u)
	}

	return &bridge.UserInfo{
		User: r.rc.UserID(),
		Me:   true,
	}
}

func (r *RocketChat) GetUsers() []*bridge.UserInfo {
	rcUsers := r.rc.Users()
	out := make([]*bridge.UserInfo, 0, len(rcUsers))

	for _, u := range rcUsers {
		out = append(out, r.toBridgeUser(u))
	}

	return out
}

func (r *RocketChat) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	if userID == "" {
		return nil
	}

	if u, ok := r.rc.User(userID); ok {
		return r.toBridgeUser(u)
	}

	u, err := r.rc.GetUserInfo(ctx, userID, "")
	if err != nil {
		return nil
	}

	return r.toBridgeUser(u)
}

func (r *RocketChat) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	if username == "" {
		return nil
	}

	if u, ok := r.rc.UserByUsername(username); ok {
		return r.toBridgeUser(u)
	}

	u, err := r.rc.GetUserInfo(ctx, "", username)
	if err != nil {
		return nil
	}

	return r.toBridgeUser(u)
}

func (r *RocketChat) GetUserCount() int {
	return len(r.rc.Users())
}

func (r *RocketChat) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	rcUsers := r.rc.Users()
	out := make([]*bridge.UserInfo, 0, len(rcUsers))

	for _, u := range rcUsers {
		if q == "" || strings.Contains(strings.ToLower(u.Username), q) || strings.Contains(strings.ToLower(u.Name), q) {
			out = append(out, r.toBridgeUser(u))
		}
	}

	return out, nil
}

func (r *RocketChat) GetChannels() []*bridge.ChannelInfo {
	rooms := r.rc.Rooms()
	out := make([]*bridge.ChannelInfo, 0, len(rooms))

	for _, room := range rooms {
		if _, subbed := r.rc.Subscription(room.ID); !subbed {
			continue
		}

		out = append(out, r.toChannelInfo(room))
	}

	return out
}

func (r *RocketChat) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return nil, errors.New("channel not found")
	}

	return r.toChannelInfo(room), nil
}

func (r *RocketChat) GetChannelCount() int {
	return len(r.rc.Subscriptions())
}

func (r *RocketChat) GetChannelName(ctx context.Context, channelID string) string {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return ""
	}

	return r.roomName(room)
}

func (r *RocketChat) GetChannelID(ctx context.Context, name, teamID string) string {
	clean := strings.TrimPrefix(name, "#")

	for _, room := range r.rc.Rooms() {
		if room.ID == clean || r.roomName(room) == clean {
			return room.ID
		}
	}

	return ""
}

func (r *RocketChat) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return nil, errors.New("channel not found")
	}

	out := make([]*bridge.UserInfo, 0, len(room.UIDs))
	for _, uid := range room.UIDs {
		if u := r.GetUser(ctx, uid); u != nil {
			out = append(out, u)
		}
	}

	return out, nil
}

func (r *RocketChat) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	var channels []string

	meID := r.rc.UserID()

	for _, room := range r.rc.Rooms() {
		if room.Type == rocketchatclient.RoomTypeDirect {
			continue
		}

		if userID == meID {
			if _, ok := r.rc.Subscription(room.ID); ok {
				channels = append(channels, "#"+r.roomName(room))
			}

			continue
		}

		if slices.Contains(room.UIDs, userID) {
			channels = append(channels, "#"+r.roomName(room))
		}
	}

	return channels, nil
}

func (r *RocketChat) IsChannelMember(channelID string) bool {
	_, ok := r.rc.Subscription(channelID)

	return ok
}

func (r *RocketChat) List(ctx context.Context) (map[string]string, error) {
	rooms := r.rc.Rooms()
	out := make(map[string]string, len(rooms))

	for _, room := range rooms {
		if room.Type == rocketchatclient.RoomTypeDirect {
			continue
		}

		out["#"+r.roomName(room)] = room.Topic
	}

	return out, nil
}

func (r *RocketChat) Topic(ctx context.Context, channelID string) string {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return ""
	}

	return room.Topic
}

func (r *RocketChat) GetDMChannelID(ctx context.Context, userID string) string {
	meID := r.rc.UserID()

	for _, room := range r.rc.Rooms() {
		if !isOneToOneDM(room) {
			continue
		}

		if slices.Contains(room.UIDs, userID) && slices.Contains(room.UIDs, meID) {
			return room.ID
		}
	}

	return ""
}

func (r *RocketChat) GetDMChannelName(userID1 string, userID2 string) string {
	meID := r.rc.UserID()

	target := userID2
	if target == meID {
		target = userID1
	}

	return r.GetDMChannelID(r.ctx, target)
}

func (r *RocketChat) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return nil
	}

	return r.dmPeer(room)
}

func (r *RocketChat) GetDMUserIDs(channelName string) (string, string, bool) {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return "", "", false
	}

	peer := r.dmPeer(room)
	if peer == nil {
		return "", "", false
	}

	return r.rc.UserID(), peer.User, true
}

func (r *RocketChat) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return "", false
	}

	peer := r.dmPeer(room)
	if peer == nil {
		return "", false
	}

	return peer.User, true
}

func (r *RocketChat) IsDMChannelName(channelName string) bool {
	room, ok := r.rc.Room(channelName)

	return ok && isOneToOneDM(room)
}

func (r *RocketChat) StatusUser(ctx context.Context, userID string) (string, error) {
	u, ok := r.rc.User(userID)
	if !ok || u.Status == "" {
		return rocketchatclient.StatusOffline, nil
	}

	if u.StatusText != "" {
		return u.Status + ": " + u.StatusText, nil
	}

	return u.Status, nil
}

func (r *RocketChat) StatusUsers(ctx context.Context) (map[string]string, error) {
	rcUsers := r.rc.Users()
	out := make(map[string]string, len(rcUsers))

	for _, u := range rcUsers {
		if u.Status != "" {
			out[u.ID] = u.Status
		}
	}

	return out, nil
}

func (r *RocketChat) IsOnline(status string) bool {
	return status == rocketchatclient.StatusOnline || strings.HasPrefix(status, rocketchatclient.StatusOnline+": ")
}

func (r *RocketChat) HasCustomStatus(userID string) bool {
	u, ok := r.rc.User(userID)

	return ok && u.StatusText != ""
}

func (r *RocketChat) Config() any {
	return r.cfg.RocketChat()
}

func (r *RocketChat) BridgeConfig() *config.BridgeConfig {
	return &r.cfg.RocketChat().Bridge
}

func (r *RocketChat) FormatterConfig() *config.FormatterConfig {
	return &r.cfg.RocketChat().Formatter
}

func (r *RocketChat) GetPostSizeLimit() int {
	return defaultPostSizeLimit
}

func (r *RocketChat) GetLastSentMsgs() []string {
	return nil
}

func (r *RocketChat) NormalizePostID(msgID string) string {
	return strings.TrimSpace(msgID)
}

func (r *RocketChat) GetTeamName(ctx context.Context, teamID string) string {
	return ""
}

func (r *RocketChat) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (r *RocketChat) UpdateChannels(ctx context.Context) error {
	return nil
}

func (r *RocketChat) UpdateLastViewed(ctx context.Context, channelID string) {}

func (r *RocketChat) UpdateLastViewedUser(ctx context.Context, userID string) error {
	return nil
}

func (r *RocketChat) SendTyping(ctx context.Context, channelName string) {}

func (r *RocketChat) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	return nil
}

func (r *RocketChat) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return nil
}

func (r *RocketChat) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return nil
}

func (r *RocketChat) GetPostChannelID(ctx context.Context, postID string) string {
	return ""
}

func (r *RocketChat) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	return nil
}

func (r *RocketChat) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return nil
}

func (r *RocketChat) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return nil
}

func (r *RocketChat) Invite(ctx context.Context, channelID, username string) error {
	return errors.New("rocketchat: Invite not implemented yet (Phase 3)")
}

func (r *RocketChat) Join(ctx context.Context, channelName string) (string, string, error) {
	return "", "", errors.New("rocketchat: Join not implemented yet (Phase 3)")
}

func (r *RocketChat) Part(ctx context.Context, channel string) error {
	return errors.New("rocketchat: Part not implemented yet (Phase 3)")
}

func (r *RocketChat) SetTopic(ctx context.Context, channelID, text string) error {
	return errors.New("rocketchat: SetTopic not implemented yet (Phase 3)")
}

func (r *RocketChat) Kick(ctx context.Context, channelID, username string) error {
	return errors.New("rocketchat: Kick not implemented yet (Phase 3)")
}

func (r *RocketChat) Nick(ctx context.Context, name string) error {
	return errors.New("rocketchat: Nick not implemented yet")
}

func (r *RocketChat) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return "", errors.New("rocketchat: MsgUser not implemented yet (Phase 2)")
}

func (r *RocketChat) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	return "", errors.New("rocketchat: MsgUserThread not implemented yet (Phase 2)")
}

func (r *RocketChat) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return "", errors.New("rocketchat: MsgChannel not implemented yet (Phase 2)")
}

func (r *RocketChat) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	return "", errors.New("rocketchat: MsgChannelThread not implemented yet (Phase 2)")
}

func (r *RocketChat) ModifyPost(ctx context.Context, msgID, text string) error {
	return errors.New("rocketchat: ModifyPost not implemented yet (Phase 2)")
}

func (r *RocketChat) AddReaction(ctx context.Context, msgID, emoji string) error {
	return errors.New("rocketchat: AddReaction not implemented yet (Phase 2)")
}

func (r *RocketChat) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	return errors.New("rocketchat: RemoveReaction not implemented yet (Phase 2)")
}

func (r *RocketChat) SetStatus(ctx context.Context, status string) error {
	return errors.New("rocketchat: SetStatus not implemented yet (Phase 3)")
}

func (r *RocketChat) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	return nil, errors.New("rocketchat: CreateChannel not implemented yet (Phase 3)")
}
