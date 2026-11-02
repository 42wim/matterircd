// Package matrixclient is a small Matrix client library used by matterircd
// (and usable by matterbridge), the equivalent of matterclient for Mattermost.
package matrixclient

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
)

// Client is a logged in Matrix client keeping track of rooms and users.
type Client struct {
	sync.RWMutex

	mc      *mautrix.Client
	handler Handler

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	connected atomic.Bool

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger

	channels   map[id.RoomID]*Channel
	dmChannels map[id.RoomID][]id.UserID
	directData map[id.UserID][]id.RoomID // last known m.direct account data
	users      map[id.UserID]*User
}

// New logs in and starts syncing. Handler.Ready is called after the initial sync.
func New(ctx context.Context, cred Credentials, h Handler) (*Client, error) {
	// Logger for the rest of matrixclient
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Logger for Matrix API calls
	rootAPILogger := logrus.New()
	rootAPILogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Default to higher than "warn" to not log anything
	rootAPILogger.SetLevel(logrus.ErrorLevel)

	mc, err := mautrix.NewClient(cred.Server, "", "")
	if err != nil {
		return nil, err
	}

	_, err = mc.Login(ctx, &mautrix.ReqLogin{
		Type:             mautrix.AuthTypePassword,
		Identifier:       mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cred.Login},
		Password:         cred.Pass,
		StoreCredentials: true,
	})
	if err != nil {
		return nil, err
	}

	c := &Client{
		mc:      mc,
		handler: h,

		rootLogger:    rootLogger,
		logger:        rootLogger.WithFields(logrus.Fields{"prefix": "matrixclient"}),
		rootAPILogger: rootAPILogger,
		apiLogger:     rootAPILogger.WithFields(logrus.Fields{"prefix": "matrixclient: API"}),

		channels:   make(map[id.RoomID]*Channel),
		dmChannels: make(map[id.RoomID][]id.UserID),
		directData: make(map[id.UserID][]id.RoomID),
		users:      make(map[id.UserID]*User),
	}
	c.ctx, c.cancel = context.WithCancel(ctx)

	syncer, ok := mc.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return nil, errors.New("matrixclient: unexpected syncer type")
	}

	c.register(syncer)

	// Ready is called from the sync goroutine, after the events of the first
	// sync response were processed, so handler calls stay serialized.
	mc.Syncer = &readySyncer{DefaultSyncer: syncer, ready: h.Ready}

	go c.syncLoop()

	c.connected.Store(true)

	return c, nil
}

func (c *Client) register(s *mautrix.DefaultSyncer) {
	s.OnEventType(event.EventMessage, c.handleMessage)
	s.OnEventType(event.EventReaction, c.handleReaction)
	s.OnEventType(event.StateMember, c.handleMember)
	s.OnEventType(event.StateRoomName, c.handleRoomName)
	s.OnEventType(event.AccountDataDirectChats, c.handleDM)
	s.OnEventType(event.StateCanonicalAlias, c.handleCanonicalAlias)
}

func (c *Client) syncLoop() {
	for c.ctx.Err() == nil {
		err := c.mc.SyncWithContext(c.ctx)
		if err == nil || c.ctx.Err() != nil {
			continue
		}

		c.apiLogger.Errorf("sync: %s", err)

		select {
		case <-c.ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// readySyncer calls ready once, after the first sync response was processed.
type readySyncer struct {
	*mautrix.DefaultSyncer

	once  sync.Once
	ready func()
}

func (r *readySyncer) ProcessResponse(ctx context.Context, res *mautrix.RespSync, since string) error {
	err := r.DefaultSyncer.ProcessResponse(ctx, res, since)
	if err == nil {
		r.once.Do(r.ready)
	}

	return err
}

// Logout stops syncing.
func (c *Client) Logout() {
	c.connected.Store(false)
	c.cancel()
}

// Connected reports whether the client is logged in.
func (c *Client) Connected() bool { return c.connected.Load() }

// UserID is our own user ID.
func (c *Client) UserID() id.UserID { return c.mc.UserID }

// IsDirect reports whether roomID is a direct message room.
func (c *Client) IsDirect(roomID id.RoomID) bool {
	c.RLock()
	defer c.RUnlock()

	_, ok := c.dmChannels[roomID]

	return ok
}

// DirectUsers returns the users of a direct message room.
func (c *Client) DirectUsers(roomID id.RoomID) []id.UserID {
	c.RLock()
	defer c.RUnlock()

	return slices.Clone(c.dmChannels[roomID])
}

// DirectRoom returns the direct message room with userID, or "".
func (c *Client) DirectRoom(userID id.UserID) id.RoomID {
	c.RLock()
	defer c.RUnlock()

	for roomID, users := range c.dmChannels {
		if len(users) == 1 && users[0] == userID {
			return roomID
		}
	}

	return ""
}

// RoomInfo is a snapshot of a room.
type RoomInfo struct {
	ID       id.RoomID
	Name     string // alias with ':' replaced by '/'
	IsDirect bool
}

// Rooms returns a snapshot of the known rooms (excluding 1:1 direct rooms).
func (c *Client) Rooms() []RoomInfo {
	c.RLock()
	defer c.RUnlock()

	rooms := make([]RoomInfo, 0, len(c.channels))

	for roomID, ch := range c.channels {
		ch.RLock()

		name, alias, direct, members := ch.Name, ch.Alias, ch.IsDirect, len(ch.Members)

		ch.RUnlock()

		// A canonical alias is preferred over the room name.
		if alias == "" {
			alias = id.RoomAlias(name)
		}

		if alias == "" {
			alias = id.RoomAlias(roomID)
		}

		if direct && members == 1 {
			continue
		}

		rooms = append(rooms, RoomInfo{
			ID:       roomID,
			Name:     strings.Replace(string(alias), ":", "/", 1),
			IsDirect: direct,
		})
	}

	return rooms
}

// DisplayName returns the known display name of a user, or "".
func (c *Client) DisplayName(userID id.UserID) string {
	c.RLock()
	defer c.RUnlock()

	if u, ok := c.users[userID]; ok && u.MemberEventContent != nil {
		return u.Displayname
	}

	return ""
}

// KnownUsers returns the IDs of all known users.
func (c *Client) KnownUsers() []id.UserID {
	c.RLock()
	defer c.RUnlock()

	ids := make([]id.UserID, 0, len(c.users))
	for userID := range c.users {
		ids = append(ids, userID)
	}

	return ids
}

// JoinedMembers returns the joined members of a room.
func (c *Client) JoinedMembers(ctx context.Context, roomID id.RoomID) ([]id.UserID, error) {
	resp, err := c.mc.JoinedMembers(ctx, roomID)
	if err != nil {
		return nil, err
	}

	ids := make([]id.UserID, 0, len(resp.Joined))
	for userID := range resp.Joined {
		ids = append(ids, userID)
	}

	return ids, nil
}

// Join joins a room by ID or alias.
func (c *Client) Join(ctx context.Context, roomIDOrAlias string) (id.RoomID, error) {
	resp, err := c.mc.JoinRoom(ctx, roomIDOrAlias, nil)
	if err != nil {
		return "", err
	}

	return resp.RoomID, nil
}

// Send sends a text message, as a thread reply when parentID is set.
func (c *Client) Send(ctx context.Context, roomID id.RoomID, parentID id.EventID, text string) (id.EventID, error) {
	content := event.MessageEventContent{MsgType: event.MsgText, Body: text}
	if parentID != "" {
		content.RelatesTo = &event.RelatesTo{Type: event.RelThread, EventID: parentID}
	}

	resp, err := c.mc.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	if err != nil {
		return "", err
	}

	return resp.EventID, nil
}

// SendDirect sends a message to userID, creating the direct room if needed.
func (c *Client) SendDirect(ctx context.Context, userID id.UserID, parentID id.EventID, text string) (id.EventID, error) {
	roomID := c.DirectRoom(userID)
	if roomID == "" {
		resp, err := c.mc.CreateRoom(ctx, &mautrix.ReqCreateRoom{
			Preset:   "trusted_private_chat",
			Invite:   []id.UserID{userID},
			IsDirect: true,
		})
		if err != nil {
			return "", err
		}

		roomID = resp.RoomID

		c.Lock()
		direct := maps.Clone(c.directData)
		c.Unlock()

		if direct == nil {
			direct = make(map[id.UserID][]id.RoomID)
		}

		direct[userID] = append(slices.Clone(direct[userID]), roomID)

		// m.direct is not set by the homeserver, persist it ourselves.
		err = c.mc.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct)
		if err != nil {
			c.logger.Errorf("failed to persist m.direct for %s: %s", roomID, err)
		}

		c.Lock()
		c.directData[userID] = direct[userID]
		c.addDirectLocked(roomID, userID, nil)
		c.Unlock()
	}

	return c.Send(ctx, roomID, parentID, text)
}

// Event fetches a single event, retrying once.
func (c *Client) Event(ctx context.Context, roomID id.RoomID, eventID id.EventID) (*event.Event, error) {
	ev, err := c.mc.GetEvent(ctx, roomID, eventID)
	if err != nil {
		ev, err = c.mc.GetEvent(ctx, roomID, eventID)
	}

	return ev, err
}
