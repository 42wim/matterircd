// Package matrixclient is a small Matrix client library used by matterircd
// (and usable by matterbridge), the equivalent of matterclient for Mattermost.
package matrixclient

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
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
	synced    atomic.Bool // set once the initial sync was processed

	cred Credentials

	// stateLock guards the debounce maps and is never held across API calls.
	stateLock  sync.Mutex
	lastViewed map[id.RoomID]viewedMark
	lastTyping map[id.RoomID]time.Time

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger

	channels   map[id.RoomID]*Channel
	dmChannels map[id.RoomID][]id.UserID
	directData map[id.UserID][]id.RoomID // last known m.direct account data
	users      map[id.UserID]*User
	presence   map[id.UserID]*Presence
	spaces     map[id.RoomID]*space

	invitations map[id.RoomID]Invitation
	// Accepted direct invites may not appear in m.direct, even after joining.
	acceptedDirect map[id.RoomID]id.UserID
	pendingDirect  map[id.RoomID]pendingAssoc // associations not yet seen in m.direct, see recordDirect
	leftRooms      map[id.RoomID]struct{}     // excludes departed rooms from selection, not DM classification

	// directWriteMu serializes the m.direct snapshot, write and cache update, as each write
	// replaces the whole account data. It is never held with the client lock over network I/O.
	directWriteMu sync.Mutex

	crypto       Crypto
	verification VerificationController

	decryptFailures bool // crypto reports terminal decryption failures

	defaultProvider string

	lastSync     atomic.Int64 // unix nanoseconds of the last processed sync response
	forceRelogin atomic.Bool

	nameResolved func(roomID id.RoomID, name string) // see WithRoomNameResolved
	// nameHealing tracks the room IDs with a running healing goroutine.
	nameHealing sync.Map
	nameLookups chan struct{} // bounds the concurrent name lookups of all healing goroutines
	// nameNotify serializes the room name callbacks with the name state they report,
	// so a late callback cannot overtake a newer one. It is taken before the client lock.
	nameNotify sync.Mutex
	nameDirty  map[id.RoomID]struct{} // rooms whose fallback name a shared profile change may have changed
}

// New logs in and starts syncing. Handler.Ready is called after the initial sync.
func New(ctx context.Context, cred Credentials, h Handler, opts ...Option) (*Client, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	rootLogger, rootAPILogger := newLoggers()

	mc, err := mautrix.NewClient(cred.Server, "", "")
	if err != nil {
		return nil, err
	}

	c := &Client{
		mc:      mc,
		handler: h,
		cred:    cred,

		defaultProvider: o.defaultProvider,

		rootLogger:    rootLogger,
		logger:        rootLogger.WithFields(logrus.Fields{"prefix": "matrixclient"}),
		rootAPILogger: rootAPILogger,
		apiLogger:     rootAPILogger.WithFields(logrus.Fields{"prefix": "matrixclient: API"}),

		channels:   make(map[id.RoomID]*Channel),
		dmChannels: make(map[id.RoomID][]id.UserID),
		directData: make(map[id.UserID][]id.RoomID),
		users:      make(map[id.UserID]*User),
		presence:   make(map[id.UserID]*Presence),
		spaces:     make(map[id.RoomID]*space),

		invitations:    make(map[id.RoomID]Invitation),
		acceptedDirect: make(map[id.RoomID]id.UserID),
		pendingDirect:  make(map[id.RoomID]pendingAssoc),
		leftRooms:      make(map[id.RoomID]struct{}),

		nameResolved: o.nameResolved,
		nameLookups:  make(chan struct{}, maxNameLookups),
		nameDirty:    make(map[id.RoomID]struct{}),

		lastViewed: make(map[id.RoomID]viewedMark),
		lastTyping: make(map[id.RoomID]time.Time),
	}
	ctx, c.cancel = context.WithCancel(ctx)
	c.ctx = ctx

	if o.userAgent != "" {
		mc.UserAgent = o.userAgent
	}

	mc.Client.Transport = newAPILogTransport(newTransport(&o), c.apiLogger)

	syncer, ok := mc.Syncer.(*mautrix.DefaultSyncer)
	if !ok {
		return nil, errors.New("matrixclient: unexpected syncer type")
	}

	c.register(syncer)

	// Ready is called from the sync goroutine, after the events of the first
	// sync response were processed, so handler calls stay serialized.
	mc.Syncer = &readySyncer{DefaultSyncer: syncer, ready: h.Ready, synced: c.onSynced, named: c.noteRoomNames, track: c.noteTimeline, leftInvites: c.removeLeftInvitations, decrypted: c.noteDecrypted, failed: c.onSyncFailed}

	err = c.login(ctx, o.crypto)
	if err != nil {
		c.cancel()

		return nil, err
	}

	c.applyDeviceName(ctx)

	c.connected.Store(true)

	go c.syncLoop()
	go c.maintainPresence()

	return c, nil
}

func newLoggers() (*logrus.Logger, *logrus.Logger) {
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

	return rootLogger, rootAPILogger
}

// loginToken adopts cred.Token, looking up the user and device it belongs to.
func (c *Client) loginToken(ctx context.Context) error {
	c.mc.AccessToken = c.cred.Token

	resp, err := c.mc.Whoami(ctx)
	if err != nil {
		return err
	}

	// Whoami may omit the device ID, only then is the configured one trusted.
	if resp.DeviceID != "" && c.cred.DeviceID != "" && resp.DeviceID != c.cred.DeviceID {
		return errors.New("matrixclient: token belongs to device " + resp.DeviceID.String() + ", not the configured " + c.cred.DeviceID.String())
	}

	c.mc.UserID = resp.UserID
	c.mc.DeviceID = resp.DeviceID

	if c.mc.DeviceID == "" {
		c.mc.DeviceID = c.cred.DeviceID
	}

	return nil
}

func (c *Client) login(ctx context.Context, factory CryptoFactory) error {
	if c.cred.Token != "" {
		err := c.loginToken(ctx)
		if err != nil {
			return err
		}

		if factory == nil {
			return nil
		}
	}

	if factory == nil {
		_, err := c.mc.Login(ctx, loginReq(c.cred, ""))

		return err
	}

	// The crypto helper logs in itself to reuse the device stored with its keys.
	var req *mautrix.ReqLogin
	if c.cred.Token == "" {
		req = loginReq(c.cred, "")
	}

	crypto, err := factory(c.mc, req)
	if err != nil {
		return err
	}

	c.crypto = crypto

	if n, ok := crypto.(DecryptFailureNotifier); ok {
		n.OnDecryptFailure(c.noteDecryptFailure)

		c.decryptFailures = true
	}

	c.mc.Crypto = crypto

	err = crypto.Init(ctx)
	if err != nil {
		_ = crypto.Close()

		return err
	}

	if h, ok := c.handler.(VerificationHandler); ok {
		if p, supported := crypto.(VerificationProvider); supported {
			c.verification, err = p.InitVerification(ctx, h.OnVerification)
			if err != nil {
				_ = crypto.Close()

				return err
			}
		}
	}

	c.logger.Info("end-to-end encryption enabled")

	return nil
}

func (c *Client) register(s *mautrix.DefaultSyncer) {
	s.OnSync(c.syncInvitations)

	onState := func(t event.Type, handler mautrix.EventHandler) {
		s.OnEventType(t, func(ctx context.Context, ev *event.Event) {
			// Stripped invite state belongs to Invitations, not the joined room cache.
			if ev.Mautrix.EventSource&event.SourceInvite != 0 {
				return
			}

			handler(ctx, ev)
		})
	}

	s.OnEventType(event.EventMessage, c.handleMessage)
	s.OnEventType(event.EventReaction, c.handleReaction)
	s.OnEventType(event.StateMember, c.handleMember)
	onState(event.StateRoomName, c.handleRoomName)
	s.OnEventType(event.AccountDataDirectChats, c.handleDM)
	onState(event.StateCanonicalAlias, c.handleCanonicalAlias)
	s.OnEventType(event.EventRedaction, c.handleRedaction)
	s.OnEventType(event.EphemeralEventTyping, c.handleTyping)
	onState(event.StateTopic, c.handleTopic)
	onState(event.StateCreate, c.handleCreate)
	onState(event.StateEncryption, c.handleEncryption)
	onState(event.StateSpaceChild, c.handleSpaceChild)
	s.OnEventType(event.EphemeralEventPresence, c.handlePresence)
}

func (c *Client) syncLoop() {
	backoff := time.Second

	for c.ctx.Err() == nil {
		if c.forceRelogin.Swap(false) && c.cred.Token == "" {
			err := c.relogin(c.ctx)
			if err != nil {
				c.apiLogger.Errorf("reconnect: %s", err)
			}
		}

		err := c.mc.SyncWithContext(c.ctx)
		if err == nil || c.ctx.Err() != nil {
			continue
		}

		// A sync that was healthy before failing starts over with a short backoff.
		if c.connected.Swap(false) {
			backoff = time.Second
		}

		c.apiLogger.Errorf("sync: %s", err)

		if errors.Is(err, mautrix.MUnknownToken) {
			err = c.relogin(c.ctx)
			if err != nil {
				c.apiLogger.Errorf("relogin: %s", err)
			}
		}

		select {
		case <-c.ctx.Done():
		case <-time.After(backoff):
		}

		backoff = min(backoff*2, maxSyncBackoff)
	}
}

// readySyncer calls ready once, after the first sync response was processed.
type readySyncer struct {
	*mautrix.DefaultSyncer

	once      sync.Once
	ready     func()
	synced    func()
	named     func(*mautrix.RespSync)
	track     func(*mautrix.RespSync)
	decrypted func(*event.Event)
	failed    func(err error, wait time.Duration)

	leftInvites func(*mautrix.RespSync)

	failures int // consecutive failed syncs, only used from the sync goroutine
}

func (r *readySyncer) ProcessResponse(ctx context.Context, res *mautrix.RespSync, since string) error {
	r.track(res)

	err := r.DefaultSyncer.ProcessResponse(ctx, res, since)
	if err == nil {
		r.failures = 0
		r.leftInvites(res)
		r.synced()
		r.named(res)
		r.once.Do(r.ready)
	}

	return err
}

// OnFailedSync retries quickly at first, then backs off exponentially with
// jitter, instead of the 10 second fixed wait of the default syncer. The sync
// resumes from the last next_batch token, so no events are missed.
func (r *readySyncer) OnFailedSync(res *mautrix.RespSync, err error) (time.Duration, error) {
	if errors.Is(err, mautrix.MUnknownToken) {
		return 0, err
	}

	wait := min(time.Second<<min(r.failures, 5), maxSyncRetryWait)

	jitter, randomErr := rand.Int(rand.Reader, big.NewInt(int64(wait/2)+1))
	if randomErr == nil {
		wait += time.Duration(jitter.Int64())
	}
	r.failures++

	r.failed(err, wait)

	return wait, nil
}

// Dispatch sees the events the crypto helper dispatches after decrypting them.
func (r *readySyncer) Dispatch(ctx context.Context, ev *event.Event) {
	r.decrypted(ev)
	r.DefaultSyncer.Dispatch(ctx, ev)
}

// Logout stops syncing.
func (c *Client) Logout() {
	c.connected.Store(false)
	c.cancel()

	if c.crypto != nil {
		_ = c.crypto.Close()
	}
}

// Connected reports whether the client is logged in.
func (c *Client) Connected() bool { return c.connected.Load() }

// Encrypted reports whether end-to-end encryption is enabled.
func (c *Client) Encrypted() bool { return c.crypto != nil }

// DeviceID is the ID of our device, as listed under the sessions of other clients.
// It is empty when end-to-end encryption is disabled.
func (c *Client) DeviceID() id.DeviceID {
	if c.crypto == nil {
		return ""
	}

	return c.mc.DeviceID
}

// Fingerprint is the fingerprint of our device's Ed25519 key, for manual verification.
// It is empty when end-to-end encryption is disabled.
func (c *Client) Fingerprint() string {
	if p, ok := c.crypto.(FingerprintProvider); ok {
		return p.Fingerprint()
	}

	return ""
}

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

// DirectRoom returns the direct message room with userID, or "", excluding invitations and departed rooms.
func (c *Client) DirectRoom(userID id.UserID) id.RoomID {
	c.RLock()
	defer c.RUnlock()

	for roomID, users := range c.dmChannels {
		if _, invited := c.invitations[roomID]; invited {
			continue
		}

		if _, left := c.leftRooms[roomID]; left {
			continue
		}

		if len(users) == 1 && users[0] == userID {
			return roomID
		}
	}

	return ""
}

// LastEventID returns the newest message event seen in a room, "" if none.
func (c *Client) LastEventID(roomID id.RoomID) id.EventID {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return ""
	}

	ch.RLock()
	defer ch.RUnlock()

	return ch.LastEventID
}

// RoomInfo is a snapshot of a room.
type RoomInfo struct {
	ID       id.RoomID
	Name     string // alias, room name, member names or short ID, with ':' replaced by '/'
	IsDirect bool

	LastActivity int64 // timestamp in milliseconds of the newest message seen, 0 if none
}

// Rooms returns a snapshot of the known rooms (excluding invitations, departed rooms and 1:1 direct rooms).
func (c *Client) Rooms() []RoomInfo {
	c.RLock()
	defer c.RUnlock()

	rooms := make([]RoomInfo, 0, len(c.channels))

	for roomID, ch := range c.channels {
		if _, invited := c.invitations[roomID]; invited {
			continue
		}

		if _, left := c.leftRooms[roomID]; left {
			continue
		}

		ch.Lock()

		display := c.displayNameLocked(roomID, ch)
		ch.reportNameLocked(display)

		unresolved, direct, members, last := ch.nameUnresolvedLocked(), ch.IsDirect, len(ch.Members), ch.LastActivity

		ch.Unlock()

		if unresolved {
			c.logger.Debugf("room %s has no name or alias, listing it as %s", roomID, display)
			c.healRoomName(c.ctx, roomID, ch)
		}

		// DM associations carry no members, so a direct room may not have any cached yet.
		if direct && members <= 1 {
			continue
		}

		rooms = append(rooms, RoomInfo{
			ID:       roomID,
			Name:     display,
			IsDirect: direct,

			LastActivity: last,
		})
	}

	return rooms
}

// roomName converts an alias to a channel name, dropping the default provider.
func (c *Client) roomName(alias id.RoomAlias) string {
	if c.defaultProvider != "" {
		if name, ok := strings.CutSuffix(string(alias), ":"+c.defaultProvider); ok {
			return name
		}
	}

	return strings.Replace(string(alias), ":", "/", 1)
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

// Join joins a room by ID or alias, accepting a pending invitation if there is one.
func (c *Client) Join(ctx context.Context, roomIDOrAlias string) (id.RoomID, error) {
	if c.defaultProvider != "" && strings.HasPrefix(roomIDOrAlias, "#") && !strings.Contains(roomIDOrAlias, ":") {
		roomIDOrAlias += ":" + c.defaultProvider
	}

	resp, err := c.mc.JoinRoom(ctx, roomIDOrAlias, nil)
	if err != nil {
		return "", err
	}

	c.persistAcceptedDirect(ctx, resp.RoomID)

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

		c.recordDirect(ctx, userID, roomID)

		c.Lock()
		c.addDirectLocked(roomID, userID)
		c.Unlock()
	} else {
		c.retryDirect(ctx, roomID)
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
