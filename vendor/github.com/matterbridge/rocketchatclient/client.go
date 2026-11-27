// Package rocketchatclient is a small Rocket.Chat client library used by matterircd
// (and usable by matterbridge), modeled on matrixclient and matterclient.
package rocketchatclient

import (
	"context"
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
)

// Client is a logged-in Rocket.Chat client maintaining REST access, a DDP WebSocket stream,
// and an in-memory state cache.
type Client struct {
	cred Credentials
	opts options

	httpURL    string
	wsURL      string
	httpClient *http.Client

	handler Handler

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	authMu     sync.RWMutex
	userID     string
	authToken  string
	me         *User
	tokenLogin bool

	connected      atomic.Bool
	ready          atomic.Bool
	lastSync       atomic.Int64
	lastDisconnect atomic.Int64

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger

	cache            *stateCache
	eventCh          chan ddpEvent
	wg               sync.WaitGroup
	userPrefetchOnce sync.Once

	connMu         sync.Mutex
	activeConn     *ddpConn
	roomDeleteSubs map[string]string
	roomTypingSubs map[string]string
}

func resolveClientOptions(cred Credentials, opts []Option) options {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}

	if cred.SkipTLSVerify {
		o.skipTLSVerify = true
	}

	if cred.NoTLS {
		o.noTLS = true
	}

	return o
}

// New authenticates with Rocket.Chat, starts the DDP WebSocket loop, hydrates initial
// state, and invokes Handler.Ready once hydrated.
func New(ctx context.Context, cred Credentials, h Handler, opts ...Option) (*Client, error) {
	o := resolveClientOptions(cred, opts)

	httpURL, wsURL, err := normalizeServerURL(cred.Server, o.noTLS)
	if err != nil {
		return nil, err
	}

	cache, err := newStateCache(o.sentLRUSize, o.msgLRUSize, o.reactionLRUSize)
	if err != nil {
		return nil, err
	}

	rootLogger, rootAPILogger := newLoggers()
	clientCtx, cancel := context.WithCancel(ctx)

	c := &Client{
		cred:           cred,
		opts:           o,
		httpURL:        httpURL,
		wsURL:          wsURL,
		handler:        h,
		ctx:            clientCtx,
		cancel:         cancel,
		rootLogger:     rootLogger,
		logger:         rootLogger.WithFields(logrus.Fields{"prefix": "rocketchatclient"}),
		rootAPILogger:  rootAPILogger,
		apiLogger:      rootAPILogger.WithFields(logrus.Fields{"prefix": "rocketchatclient: API"}),
		cache:          cache,
		eventCh:        make(chan ddpEvent, eventQueueSize),
		roomDeleteSubs: make(map[string]string),
		roomTypingSubs: make(map[string]string),
	}

	c.httpClient = &http.Client{
		Timeout:   defaultHTTPClientTimeout,
		Transport: newAPILogTransport(newTransport(&o), c.apiLogger),
	}

	err = c.authenticate(clientCtx)
	if err != nil {
		cancel()

		return nil, err
	}

	c.wg.Add(2)

	go c.eventLoop(clientCtx)
	go c.runLoop()

	return c, nil
}

func newLoggers() (*logrus.Logger, *logrus.Logger) {
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	rootAPILogger := logrus.New()
	rootAPILogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	rootAPILogger.SetLevel(logrus.ErrorLevel)

	return rootLogger, rootAPILogger
}

// Logout stops the DDP loop, terminates background goroutines, and revokes password-created sessions.
func (c *Client) Logout() {
	c.connected.Store(false)

	c.authMu.RLock()
	tokenLogin := c.tokenLogin
	hasAuth := c.userID != "" && c.authToken != ""
	c.authMu.RUnlock()

	if !tokenLogin && hasAuth {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = c.logoutREST(logoutCtx)

		cancel()
	}

	c.cancel()
	c.wg.Wait()
}

// BaseURL returns the normalized HTTP base URL of the connected Rocket.Chat server.
func (c *Client) BaseURL() string {
	return c.httpURL
}

// Connected reports whether the DDP WebSocket connection is currently active.
func (c *Client) Connected() bool {
	return c.connected.Load()
}

// UserID returns the authenticated user's ID.
func (c *Client) UserID() string {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.userID
}

// Me returns a copy of the authenticated user's profile.
func (c *Client) Me() *User {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return cloneUser(c.me)
}

// SetLogLevel sets the log level of rocketchatclient.
func (c *Client) SetLogLevel(level string) {
	setLevel(c.rootLogger, level)
}

// SetLogAPICalls sets the log level of Rocket.Chat REST API call logging.
func (c *Client) SetLogAPICalls(level string) {
	setLevel(c.rootAPILogger, level)
}

func setLevel(l *logrus.Logger, level string) {
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		l.Warnf("invalid log level %q, keeping %s", level, l.GetLevel())

		return
	}

	l.SetLevel(lvl)
}

// User returns a cached user by ID.
func (c *Client) User(id string) (*User, bool) {
	return c.cache.user(id)
}

// UserByUsername returns a cached user by username.
func (c *Client) UserByUsername(username string) (*User, bool) {
	return c.cache.userByUsername(username)
}

// Users returns a snapshot of all cached users.
func (c *Client) Users() []*User {
	return c.cache.users()
}

// GetUserLastActivity returns the last observed activity timestamp for userID
// in Unix milliseconds, or 0 if unknown.
func (c *Client) GetUserLastActivity(userID string) int64 {
	return c.cache.userLastActivity(userID)
}

// GetUserLocalTime returns the current local time (HH:MM) for userID when their
// cached profile carries a valid utcOffset, or "" otherwise.
func (c *Client) GetUserLocalTime(userID string) string {
	u, ok := c.cache.user(userID)
	if !ok || !u.HasUTCOffset || u.UTCOffset < -12 || u.UTCOffset > 14 {
		return ""
	}

	offsetSec := int(math.Round(u.UTCOffset * 3600))
	loc := time.FixedZone("", offsetSec)

	return time.Now().In(loc).Format("15:04")
}

// Room returns a cached room by ID.
func (c *Client) Room(id string) (*Room, bool) {
	return c.cache.room(id)
}

// Rooms returns a snapshot of all cached rooms.
func (c *Client) Rooms() []*Room {
	return c.cache.rooms()
}

// Subscription returns a cached subscription by room ID.
func (c *Client) Subscription(roomID string) (*Subscription, bool) {
	return c.cache.subscription(roomID)
}

// Subscriptions returns a snapshot of all cached subscriptions.
func (c *Client) Subscriptions() []*Subscription {
	return c.cache.subscriptions()
}

// Invitations returns a snapshot of pending room invitations.
func (c *Client) Invitations() []Invitation {
	return c.cache.invitationsSnapshot()
}

// RoomUserRoles returns cached per-room roles (such as "owner" or "moderator") for userID.
func (c *Client) RoomUserRoles(roomID, userID string) []string {
	return c.cache.roomUserRoles(roomID, userID)
}

// CachedMessage returns a cached message by ID without making a REST call.
func (c *Client) CachedMessage(msgID string) (*Message, bool) {
	return c.cache.cachedMessage(msgID)
}

// ResolveMessage returns a message from the cache, falling back to GET /api/v1/chat.getMessage.
func (c *Client) ResolveMessage(ctx context.Context, msgID string) (*Message, error) {
	if msg, ok := c.cache.cachedMessage(msgID); ok {
		return msg, nil
	}

	return c.GetMessage(ctx, msgID)
}

func (c *Client) authCredentials() (string, string) {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.userID, c.authToken
}
