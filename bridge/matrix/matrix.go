package matrix

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/davecgh/go-spew/spew"
	lru "github.com/hashicorp/golang-lru/v2"
	logger "github.com/sirupsen/logrus"
)

type Matrix struct {
	mc          *mautrix.Client
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	cfg         *config.Config

	connected atomic.Bool
	firstSync atomic.Bool

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	dmChannels map[id.RoomID][]id.UserID
	channels   map[id.RoomID]*Channel
	users      map[id.UserID]*User
	sync.RWMutex

	msgParentCache   *lru.Cache[id.EventID, string]
	msgLastSentCache *lru.Cache[string, string]
}

func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, *mautrix.Client, error) {
	m := &Matrix{
		credentials: cred,
		eventChan:   eventChan,
		cfg:         cfg,
		channels:    make(map[id.RoomID]*Channel),
		dmChannels:  make(map[id.RoomID][]id.UserID),
		users:       make(map[id.UserID]*User),
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

	mc, err := mautrix.NewClient(cred.Server, "", "")
	if err != nil {
		return nil, nil, err
	}

	if cred.Token != "" && cred.Login == "" {
		return nil, nil, errors.New("matrix: login with an access token is not supported, use LOGIN <server> <login> <pass>")
	}

	_, err = mc.Login(ctx, &mautrix.ReqLogin{
		Type: mautrix.AuthTypePassword,
		Identifier: mautrix.UserIdentifier{
			Type: mautrix.IdentifierTypeUser,
			User: cred.Login,
		},
		Password:         cred.Pass,
		StoreCredentials: true,
	})
	if err != nil {
		return nil, nil, err
	}

	m.mc = mc
	m.ctx, m.cancel = context.WithCancel(ctx)

	m.handleMatrix(onConnect)

	m.connected.Store(true)

	return m, mc, nil
}

func (m *Matrix) sendEvent(ev *bridge.Event) {
	select {
	case m.eventChan <- ev:
	case <-m.ctx.Done():
	}
}

func (m *Matrix) syncCallback(_ context.Context, resp *mautrix.RespSync, _ string) bool {
	logger.Tracef("syncCallback %s", spew.Sdump(resp))

	m.firstSync.Store(true)

	return true
}

func (m *Matrix) handleMatrix(onConnect func()) {
	syncer, ok := m.mc.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		logger.Error("matrix: unexpected syncer type")
		return
	}

	syncer.OnEventType(event.EventMessage, m.handleMessageEvent)
	syncer.OnEventType(event.EventReaction, m.handleReactionEvent)
	syncer.OnEventType(event.StateMember, m.handleMember)
	syncer.OnEventType(event.StateRoomName, m.handleRoomName)
	syncer.OnEventType(event.AccountDataDirectChats, m.handleDM)
	syncer.OnEventType(event.StateCanonicalAlias, m.handleCanonicalAlias)
	syncer.OnEvent(func(_ context.Context, ev *event.Event) {
		logger.Tracef("handleMatrix ev %s", spew.Sdump(ev))
	})
	syncer.OnSync(m.syncCallback)

	go func() {
		for m.ctx.Err() == nil {
			if err := m.mc.SyncWithContext(m.ctx); err != nil && m.ctx.Err() == nil {
				logger.Errorf("matrix: Sync() returned %s", err)

				select {
				case <-m.ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}
	}()

	go func() {
		// Wait for the initial sync to complete.
		for !m.firstSync.Load() {
			logger.Trace("syncing..")

			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}

		// The syncer callbacks run after OnSync, so wait for the number of events
		// on the buffered eventChan to remain stable before concluding the sync
		// is complete.
		current := len(m.eventChan)
		count := 0

		for count < 3 {
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(time.Second):
			}

			if current == len(m.eventChan) {
				count++
			} else {
				count = 0
			}

			current = len(m.eventChan)
		}

		logger.Trace("sync complete ", len(m.eventChan))

		onConnect()
	}()
}

func (m *Matrix) handleDM(_ context.Context, ev *event.Event) {
	directChats := ev.Content.AsDirectChats()
	if directChats == nil {
		return
	}

	m.Lock()
	defer m.Unlock()

	for userID, rooms := range *directChats {
		logger.Tracef("direct chat %#v", rooms)

		for _, roomID := range rooms {
			if _, ok := m.channels[roomID]; !ok {
				m.channels[roomID] = &Channel{
					ID:      roomID,
					Members: make(map[id.UserID]*User),
				}
			}

			u := &User{
				ID:                 userID,
				MemberEventContent: &event.MemberEventContent{},
			}

			m.users[userID] = u

			m.channels[roomID].Lock()
			m.channels[roomID].IsDirect = true

			if !containsUser(m.dmChannels[roomID], userID) {
				m.dmChannels[roomID] = append(m.dmChannels[roomID], userID)
			}

			if _, ok := m.channels[roomID].Members[userID]; !ok {
				m.channels[roomID].Members[userID] = u
			}

			m.channels[roomID].Unlock()
		}
	}
}

func containsUser(users []id.UserID, userID id.UserID) bool {
	for _, u := range users {
		if u == userID {
			return true
		}
	}

	return false
}

func (m *Matrix) getOrCreateChannelLocked(roomID id.RoomID) *Channel {
	ch, ok := m.channels[roomID]
	if !ok {
		ch = &Channel{
			ID:      roomID,
			Members: make(map[id.UserID]*User),
		}
		m.channels[roomID] = ch
	}

	return ch
}

func (m *Matrix) handleMember(_ context.Context, ev *event.Event) {
	member := ev.Content.AsMember()
	if member == nil || ev.StateKey == nil {
		return
	}

	m.Lock()
	defer m.Unlock()

	if _, ok := m.users[ev.Sender]; !ok {
		m.users[ev.Sender] = &User{
			ID:                 ev.Sender,
			MemberEventContent: member,
		}
	}

	if !member.IsDirect {
		return
	}

	logger.Trace("found direct member ", *ev.StateKey)

	userID := id.UserID(*ev.StateKey)
	ch := m.getOrCreateChannelLocked(ev.RoomID)

	m.users[userID] = &User{
		ID:                 userID,
		MemberEventContent: member,
	}

	ch.Lock()
	ch.IsDirect = true
	ch.Members[userID] = m.users[userID]
	ch.Unlock()

	if !containsUser(m.dmChannels[ev.RoomID], userID) {
		m.dmChannels[ev.RoomID] = append(m.dmChannels[ev.RoomID], userID)
	}
}

func (m *Matrix) handleRoomName(_ context.Context, ev *event.Event) {
	name := ev.Content.AsRoomName()
	if name == nil {
		return
	}

	m.Lock()
	defer m.Unlock()

	ch := m.getOrCreateChannelLocked(ev.RoomID)

	ch.Lock()
	defer ch.Unlock()

	// A canonical alias is preferred over the room name.
	if ch.Alias == "" {
		ch.Alias = id.RoomAlias("#" + strings.ReplaceAll(name.Name, " ", ""))
	}
}

func (m *Matrix) handleCanonicalAlias(_ context.Context, ev *event.Event) {
	logger.Trace("running handleCanonicalAlias for ", ev)

	alias := ev.Content.AsCanonicalAlias()
	if alias == nil {
		return
	}

	m.Lock()
	defer m.Unlock()

	ch := m.getOrCreateChannelLocked(ev.RoomID)

	ch.Lock()
	ch.Alias = alias.Alias
	ch.AltAliases = alias.AltAliases
	ch.Unlock()
}

func (m *Matrix) isDMRoom(roomID id.RoomID) bool {
	m.RLock()
	defer m.RUnlock()

	_, ok := m.dmChannels[roomID]

	return ok
}

func (m *Matrix) handleMessageEvent(ctx context.Context, ev *event.Event) {
	logger.Tracef("handleMessageEvent ev %s", spew.Sdump(ev))

	ghost := m.createUser(ev.Sender)
	if ghost == nil || ghost.Me {
		logger.Trace("handleMessageEvent ghost.Me")
		return
	}

	msgEventContent := ev.Content.AsMessage()
	if msgEventContent == nil {
		logger.Warnf("handleMessageEvent unsupported event type %s", ev.Type.String())
		return
	}

	text := msgEventContent.Body

	var parentID id.EventID
	if msgEventContent.RelatesTo != nil {
		parentID = msgEventContent.RelatesTo.EventID
	}

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies && parentID != "" {
		message, err := m.addParentMsg(ctx, ev.RoomID, parentID, text, mxCfg.ShortenRepliesTo, "@", mxCfg.Formatter.Unicode)
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v: %s", ev, err)
		}

		text = message
	}

	if m.isDMRoom(ev.RoomID) {
		m.sendEvent(&bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: ev.RoomID.String(),
				Sender:    ghost,
				Receiver:  m.GetMe(),
				MessageID: string(ev.ID),
				ParentID:  parentID.String(),
				CreateAt:  ev.Timestamp,
			},
		})

		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   ev.RoomID.String(),
			Sender:      ghost,
			ChannelType: "P",
			MessageID:   string(ev.ID),
			ParentID:    parentID.String(),
			CreateAt:    ev.Timestamp,
		},
	})
}

func (m *Matrix) handleReactionEvent(ctx context.Context, ev *event.Event) {
	logger.Tracef("handleReactionEvent ev %s", spew.Sdump(ev))

	ghost := m.createUser(ev.Sender)
	if ghost == nil || ghost.Me {
		logger.Trace("handleReactionEvent ghost.Me")
		return
	}

	reactionEventContent := ev.Content.AsReaction()
	if reactionEventContent == nil {
		logger.Warnf("handleReactionEvent unsupported event type %s", ev.Type.String())
		return
	}

	reaction := reactionEventContent.RelatesTo.Key
	parentID := reactionEventContent.RelatesTo.EventID

	var text string

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies {
		message, err := m.addParentMsg(ctx, ev.RoomID, parentID, text, mxCfg.ShortenRepliesTo, "@", mxCfg.Formatter.Unicode)
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v: %s", ev, err)
		}

		text = message
	}

	channelType := ""
	if m.isDMRoom(ev.RoomID) {
		channelType = "D"
	}

	m.sendEvent(&bridge.Event{
		Type: "reaction_add",
		Data: &bridge.ReactionAddEvent{
			ChannelID:   ev.RoomID.String(),
			MessageID:   string(parentID),
			Sender:      ghost,
			Reaction:    reaction,
			ChannelType: channelType,
			Message:     text,
			ParentID:    parentID.String(),
		},
	})
}

func (m *Matrix) Invite(ctx context.Context, channelID, username string) error {
	return nil
}

func (m *Matrix) IsChannelMember(channelID string) bool {
	m.RLock()
	defer m.RUnlock()

	_, ok := m.channels[id.RoomID(channelID)]

	return ok
}

func (m *Matrix) Join(ctx context.Context, channelName string) (string, string, error) {
	resp, err := m.mc.JoinRoom(ctx, channelName, nil)
	if err != nil {
		return "", "", err
	}

	return resp.RoomID.String(), "", nil
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
	m.cancel()

	return nil
}

func (m *Matrix) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return m.MsgUserThread(ctx, userID, "", text)
}

func (m *Matrix) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	logger.Debugf("sending message '%s' '%s' '%s'", userID, parentID, text)

	invites := []id.UserID{id.UserID(userID)}

	var roomID id.RoomID

	m.RLock()

	for rID, users := range m.dmChannels {
		if len(users) == 1 && users[0] == id.UserID(userID) {
			roomID = rID
			break
		}
	}

	m.RUnlock()

	if roomID == "" {
		resp, err := m.mc.CreateRoom(ctx, &mautrix.ReqCreateRoom{
			Preset:   "trusted_private_chat",
			Invite:   invites,
			IsDirect: true,
		})
		if err != nil {
			logger.Error("msguserthread sending message: error ", err)
			return "", err
		}

		m.Lock()
		m.dmChannels[resp.RoomID] = invites
		m.Unlock()

		roomID = resp.RoomID
	}

	return m.MsgChannelThread(ctx, roomID.String(), parentID, text)
}

func (m *Matrix) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return m.MsgChannelThread(ctx, channelID, "", text)
}

func (m *Matrix) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	logger.Debugf("msgchannelthread: sending message thread '%s' '%s' '%s'", channelID, parentID, text)

	content := event.MessageEventContent{
		MsgType: event.MsgText,
		Body:    text,
	}

	if parentID != "" {
		content.RelatesTo = &event.RelatesTo{
			Type:    event.RelThread,
			EventID: id.EventID(parentID),
		}
	}

	resp, err := m.mc.SendMessageEvent(ctx, id.RoomID(channelID), event.EventMessage, &content)
	if err != nil {
		return "", err
	}

	logger.Trace("msgchannelthread: resp ", resp)

	m.msgLastSentCache.Add(resp.EventID.String(), fmt.Sprintf("%s: %s", m.GetChannelName(ctx, channelID), text))

	return resp.EventID.String(), nil
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
	return "matrix" //nolint:goconst
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

	resp, err := m.mc.JoinedMembers(ctx, id.RoomID(channelID))
	if err != nil {
		return nil, err
	}

	logger.Debugf("GetChannelUsers %s %d", channelID, len(resp.Joined))

	for userID := range resp.Joined {
		if u := m.createUser(userID); u != nil {
			users = append(users, u)
		}
	}

	return users, nil
}

func (m *Matrix) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	m.RLock()
	userIDs := make([]id.UserID, 0, len(m.users))
	for userID := range m.users {
		userIDs = append(userIDs, userID)
	}
	m.RUnlock()

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

	m.RLock()
	defer m.RUnlock()

	for roomID, channel := range m.channels {
		channel.RLock()

		alias := channel.Alias
		if channel.IsDirect && alias == "" {
			alias = id.RoomAlias(roomID.String())
		}

		isDirect := channel.IsDirect
		members := len(channel.Members)

		channel.RUnlock()

		// if we only have 1 user this is a DM, not a real channel
		if isDirect && members == 1 {
			continue
		}

		channels = append(channels, &bridge.ChannelInfo{
			Name:    strings.Replace(alias.String(), ":", "/", 1),
			ID:      roomID.String(),
			DM:      isDirect,
			Private: false,
		})

		logger.Debugf("GetChannels %s (%s)", alias.String(), roomID.String())
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
	m.RLock()
	defer m.RUnlock()

	return len(m.users)
}

func (m *Matrix) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.UserID)
}

func (m *Matrix) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (m *Matrix) createUser(userID id.UserID) *bridge.UserInfo {
	localpart, homeserver, err := userID.Parse()
	if err != nil {
		return nil
	}

	m.RLock()
	displayName := localpart + "@" + homeserver
	if user, ok := m.users[userID]; ok && user.MemberEventContent != nil && user.Displayname != "" {
		displayName = user.Displayname
	}
	m.RUnlock()

	// Drop the homeserver/host part if it's a user on the same homeserver as us.
	nick := localpart + "@" + homeserver
	if _, myHomeserver, err2 := m.mc.UserID.Parse(); err2 == nil && homeserver == myHomeserver {
		nick = localpart
	}

	return &bridge.UserInfo{
		Nick:     nick,
		User:     "~" + localpart,
		Real:     displayName,
		Host:     homeserver,
		Ghost:    true,
		Me:       userID == m.mc.UserID,
		Username: nick,
	}
}

func (m *Matrix) addParentMsg(ctx context.Context, roomID id.RoomID, parentID id.EventID, msg string, newLen int, uncounted string, unicode bool) (string, error) {
	// Search and use cached reply if it exists.
	// None found, so we'll need to create one and save it for future uses.
	replyMessage, ok := m.msgParentCache.Get(parentID)
	if !ok {
		resp, err := m.mc.GetEvent(ctx, roomID, parentID)
		// Retry once on failure.
		if err != nil {
			resp, err = m.mc.GetEvent(ctx, roomID, parentID)
		}

		if err != nil {
			return msg, err
		}

		body := ""
		if val, ok2 := resp.Content.Raw["body"].(string); ok2 {
			body = val
		}

		nick := resp.Sender.String()
		if parentUser := m.GetUser(ctx, resp.Sender.String()); parentUser != nil {
			nick = parentUser.Nick
		}

		replyMessage = fmt.Sprintf(" (re @%s: %s)", nick, maybeShorten(body, newLen, uncounted, unicode))
		logger.Debugf("Created reply for parent post %s:%s", parentID.String(), replyMessage)

		m.msgParentCache.Add(parentID, replyMessage)
	} else {
		logger.Debugf("Found saved reply for parent post %s, using:%s", parentID, replyMessage)
	}

	return strings.TrimRight(msg, "\n") + replyMessage, nil
}

// maybeShorten returns a prefix of msg that is approximately newLen
// characters long, followed by "...".  Words that start with uncounted
// are included in the result but are not reckoned against newLen.
func maybeShorten(msg string, newLen int, uncounted string, unicode bool) string {
	if newLen == 0 || len(msg) < newLen {
		return msg
	}

	ellipsis := "..."
	if unicode {
		ellipsis = "…"
	}

	newMsg := ""

	for _, word := range strings.Split(strings.ReplaceAll(msg, "\n", " "), " ") {
		if newMsg == "" {
			newMsg = word
			continue
		}

		if len(newMsg) < newLen {
			skipped := false

			if uncounted != "" && strings.HasPrefix(word, uncounted) {
				newLen += len(word) + 1
				skipped = true
			}

			// Truncate very long words, but only if they were not skipped, on the
			// assumption that such words are important enough to be preserved whole.
			if !skipped && len(word) > newLen {
				word = fmt.Sprintf("%s[%s]", word[0:(newLen*2/3)], ellipsis)
			}

			newMsg = fmt.Sprintf("%s %s", newMsg, word)

			continue
		}

		break
	}

	return fmt.Sprintf("%s %s", newMsg, ellipsis)
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
	m.RLock()
	defer m.RUnlock()

	for roomID, users := range m.dmChannels {
		if len(users) == 1 && users[0] == id.UserID(userID) {
			return roomID.String()
		}
	}

	return ""
}

func (m *Matrix) GetDMChannelName(userID1 string, userID2 string) string {
	return ""
}

func (m *Matrix) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	m.RLock()
	users := m.dmChannels[id.RoomID(channelName)]
	m.RUnlock()

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
	return m.isDMRoom(id.RoomID(channelName))
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
