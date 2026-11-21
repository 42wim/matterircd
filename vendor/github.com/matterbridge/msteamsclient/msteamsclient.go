// Package msteamsclient is a small Microsoft Teams client library used by
// matterircd (and usable by matterbridge), modeled on matterclient.
package msteamsclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sirupsen/logrus"
)

const (
	defaultUserAgent   = "msteamsclient/0.0"
	defaultHTTPTimeout = 30 * time.Second
	maxAPIResponseSize = 16 << 20 // 16 MiB
	maxHTTPRetries     = 2
	postCacheSize      = 2000
	emotionCacheSize   = 2000
	sentCacheSize      = 256
	messageChanCap     = 100
)

// authKind selects which authentication headers an API call requires.
type authKind int

const (
	authSkypeToken authKind = iota
	authBearerScope
	authSkypeAndBearer
)

// UsersCache holds cached users, conversations, teams, and presence data.
// Its structure mirrors matterclient.UsersCache so lock scopes stay separate
// from WebSocket and authentication state.
type UsersCache struct {
	mu                  sync.RWMutex
	users               map[string]*User
	conversations       map[string]*Conversation
	teams               map[string]*Team
	channelMembers      map[string]map[string]struct{}
	joinedChannels      map[string]struct{}
	channelLastViewedAt map[string]int64
	presences           map[string]*Presence
	lastUpdated         atomic.Int64
}

func newUsersCache() *UsersCache {
	return &UsersCache{
		users:               make(map[string]*User),
		conversations:       make(map[string]*Conversation),
		teams:               make(map[string]*Team),
		channelMembers:      make(map[string]map[string]struct{}),
		joinedChannels:      make(map[string]struct{}),
		channelLastViewedAt: make(map[string]int64),
		presences:           make(map[string]*Presence),
	}
}

// Client is a Microsoft Teams client mirroring matterclient's API and lifecycle.
type Client struct {
	sync.RWMutex

	cred Credentials

	User        *User
	Users       *UsersCache
	MessageChan chan *Message

	// Lifecycle & persistence hooks (matching matterclient's callback style).
	OnWsConnect           func()
	OnWsDisconnect        func(err error)
	OnChannelNameResolved func(channelID, name string)
	OnTokenUpdate         func(state TokenState)
	OnAuthPrompt          func(prompt AuthPrompt)

	ForceSyncOnReconnect bool
	CacheClearCutoff     time.Duration
	PollFallback         bool
	DisableTrouter       bool
	UserAgent            string
	Timeout              int

	//nolint:containedctx // Tied to the lifecycle of the persistent client session, matching matterclient.
	Ctx         context.Context
	loginCancel context.CancelFunc
	wg          sync.WaitGroup

	connected      atomic.Bool
	wsConnected    atomic.Bool
	lastWsActivity atomic.Int64

	// Lock ordering: reauthMu -> authMu. Never hold authMu or Client.RWMutex across I/O.
	reauthMu    sync.Mutex
	authMu      sync.RWMutex
	tokenState  TokenState
	scopeTokens map[string]accessTokenEntry

	// wsReconnectMu serializes Trouter reconnects; wsMu guards wsConn and endpoint metadata.
	wsReconnectMu sync.Mutex
	wsMu          sync.RWMutex
	wsWriteMu     sync.Mutex
	wsConn        *websocket.Conn
	trouterEPID   string
	trouterSURL   string

	httpClient *http.Client

	postCache    *lru.Cache[string, *ChatMessage]
	emotionCache *lru.Cache[string, map[string][]ReactionUser]
	sentCache    *lru.Cache[string, struct{}]

	typingLock     sync.Mutex
	lastTypingSent map[string]time.Time

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger
}

// New creates a new Microsoft Teams client configured with cred.
func New(cred Credentials) *Client {
	rootLogger, rootAPILogger := newLoggers()
	logger := rootLogger.WithFields(logrus.Fields{"prefix": "msteamsclient"})
	apiLogger := rootAPILogger.WithFields(logrus.Fields{"prefix": "msteamsclient: API"})

	postCache, _ := lru.New[string, *ChatMessage](postCacheSize)
	emotionCache, _ := lru.New[string, map[string][]ReactionUser](emotionCacheSize)
	sentCache, _ := lru.New[string, struct{}](sentCacheSize)

	c := &Client{
		cred:           cred,
		Users:          newUsersCache(),
		MessageChan:    make(chan *Message, messageChanCap),
		UserAgent:      defaultUserAgent,
		scopeTokens:    make(map[string]accessTokenEntry),
		postCache:      postCache,
		emotionCache:   emotionCache,
		sentCache:      sentCache,
		lastTypingSent: make(map[string]time.Time),
		rootLogger:     rootLogger,
		logger:         logger,
		rootAPILogger:  rootAPILogger,
		apiLogger:      apiLogger,
	}

	c.initHTTPClient()

	return c
}

func (c *Client) initHTTPClient() {
	timeout := defaultHTTPTimeout
	if c.Timeout > 0 {
		timeout = time.Duration(c.Timeout) * time.Second
	}

	baseTransport, ok := http.DefaultTransport.(*http.Transport)

	var transport *http.Transport
	if ok {
		transport = baseTransport.Clone()
	} else {
		transport = &http.Transport{}
	}

	if c.cred.SkipTLSVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	c.httpClient = &http.Client{
		Timeout:   timeout,
		Transport: newAPILogTransport(transport, c.apiLogger),
	}
}

// SetLogAPICalls sets the log level for Teams API call logging.
// Accepted levels match matterclient: 'trace', 'debug', 'info', 'warn', 'error', 'fatal', 'panic'.
func (c *Client) SetLogAPICalls(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		c.logger.Warnf("Failed to parse specified log-level '%s': %#v", level, err)
	} else {
		c.rootAPILogger.SetLevel(l)
	}
}

// SetLogLevel sets the log level for msteamsclient internal logging.
func (c *Client) SetLogLevel(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		c.logger.Warnf("Failed to parse specified log-level '%s': %#v", level, err)
	} else {
		c.rootLogger.SetLevel(l)
	}
}

// Connected reports whether the client currently has an active session.
func (c *Client) Connected() bool {
	return c.connected.Load()
}

// WsConnected reports whether the real-time Trouter WebSocket is currently open.
func (c *Client) WsConnected() bool {
	return c.wsConnected.Load()
}

// Reconnect closes the active Trouter WebSocket so the supervisor immediately
// establishes a fresh session.
func (c *Client) Reconnect() {
	c.closeActiveWS()
}

// Me returns a copy of the currently authenticated user profile, or nil before login.
func (c *Client) Me() *User {
	c.RLock()
	defer c.RUnlock()

	if c.User == nil {
		return nil
	}

	cp := *c.User

	return &cp
}

// TokenState returns a snapshot of the current token and regional endpoint state.
func (c *Client) TokenState() TokenState {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.tokenState
}

// Endpoints returns the discovered regional Microsoft Teams endpoints.
func (c *Client) Endpoints() RegionEndpoints {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.tokenState.Endpoints
}

// Login authenticates the client against Microsoft Teams and performs initial
// channel/conversation synchronization. Phase 3 adds the Trouter real-time loop.
func (c *Client) Login(ctx context.Context) error {
	c.initHTTPClient()

	loginCtx, cancel := context.WithCancel(ctx)

	c.Lock()
	c.Ctx = loginCtx
	c.loginCancel = cancel
	c.Unlock()

	err := c.Authenticate(loginCtx)
	if err != nil {
		cancel()

		return err
	}

	if me := c.Me(); me != nil {
		c.CacheUser(me)
	}

	err = c.UpdateChannels(loginCtx)
	if err != nil {
		c.logger.Warnf("Initial channel sync failed: %v", err)
	}

	c.connected.Store(true)

	if !c.DisableTrouter {
		c.startTrouter(loginCtx)
	}

	return nil
}

// Logout terminates background goroutines and marks the client disconnected.
func (c *Client) Logout(_ context.Context) error {
	c.connected.Store(false)

	c.Lock()
	cancel := c.loginCancel
	c.loginCancel = nil
	c.Unlock()

	if cancel != nil {
		cancel()
	}

	c.closeActiveWS()
	c.wg.Wait()

	return nil
}

// Ping verifies that the chatsvc backend is reachable with the current token.
func (c *Client) Ping(ctx context.Context) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	pingURL := ep.ChatService + "/v1/users/ME/conversations?pageSize=1&view=msnp24Equivalent"

	return c.doJSON(ctx, http.MethodGet, pingURL, authSkypeToken, "", nil, nil)
}

func (c *Client) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}

	return defaultUserAgent
}

// doJSON performs an authenticated HTTP request with JSON payload/response,
// honoring HTTP 429 Retry-After and single-flight 401 token refresh.
func (c *Client) doJSON(ctx context.Context, method, rawURL string, kind authKind, scope string, reqBody, out any) error {
	var bodyBytes []byte

	if reqBody != nil {
		var err error

		bodyBytes, err = json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("msteamsclient: marshal request body: %w", err)
		}
	}

	for attempt := range maxHTTPRetries + 1 {
		respBytes, status, retryAfter, err := c.doRawOnce(ctx, method, rawURL, kind, scope, bodyBytes)
		if err != nil {
			return err
		}

		if status == http.StatusUnauthorized && attempt == 0 {
			refreshErr := c.handleUnauthorized(ctx, kind, scope)
			if refreshErr == nil {
				continue
			}

			return fmt.Errorf("msteamsclient: HTTP 401 unauthorized (refresh failed: %w)", refreshErr)
		}

		if status == http.StatusTooManyRequests && attempt < maxHTTPRetries {
			waitErr := sleepContext(ctx, retryAfter)
			if waitErr != nil {
				return waitErr
			}

			continue
		}

		if status >= http.StatusBadRequest {
			return fmt.Errorf("msteamsclient: %s %s returned HTTP %d: %s", method, sanitizeURLForErr(rawURL), status, truncateBody(respBytes))
		}

		if out != nil && len(respBytes) > 0 {
			err = json.Unmarshal(respBytes, out)
			if err != nil {
				return fmt.Errorf("msteamsclient: decode JSON response from %s: %w", sanitizeURLForErr(rawURL), err)
			}
		}

		return nil
	}

	return errors.New("msteamsclient: request exceeded retry limit")
}

func (c *Client) doRawOnce(ctx context.Context, method, rawURL string, kind authKind, scope string, bodyBytes []byte) ([]byte, int, time.Duration, error) {
	var bodyReader io.Reader
	if len(bodyBytes) > 0 {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, 0, 0, err
	}

	if len(bodyBytes) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	err = c.applyAuthHeaders(ctx, req, kind, scope)
	if err != nil {
		return nil, 0, 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}

	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if err != nil {
		return nil, resp.StatusCode, 0, err
	}

	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))

	return respBytes, resp.StatusCode, retryAfter, nil
}

func (c *Client) applyAuthHeaders(ctx context.Context, req *http.Request, kind authKind, scope string) error {
	switch kind {
	case authSkypeToken:
		token, err := c.EnsureSkypeToken(ctx)
		if err != nil {
			return err
		}

		req.Header.Set("Authentication", "skypetoken="+token)

		if c.effectiveAccountType() == AccountPersonal {
			req.Header.Set("X-Skypetoken", token)
		}

		return nil
	case authBearerScope:
		token, err := c.AccessToken(ctx, scope)
		if err != nil {
			return err
		}

		req.Header.Set("Authorization", "Bearer "+token)

		return nil
	case authSkypeAndBearer:
		skypeToken, err := c.EnsureSkypeToken(ctx)
		if err != nil {
			return err
		}

		req.Header.Set("X-Skypetoken", skypeToken)

		if c.effectiveAccountType() == AccountPersonal {
			req.Header.Set("X-Ms-Client-Type", "web")
			req.Header.Set("X-Ms-Client-Consumer-Type", "teams4life")
		}

		if scope != "" {
			bearer, bearerErr := c.AccessToken(ctx, scope)
			if bearerErr == nil && bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
		}

		return nil
	default:
		return fmt.Errorf("msteamsclient: unsupported auth kind %d", kind)
	}
}

func (c *Client) handleUnauthorized(ctx context.Context, kind authKind, scope string) error {
	switch kind {
	case authSkypeToken:
		_, err := c.ForceRefreshSkypeToken(ctx)

		return err
	case authBearerScope:
		c.authMu.Lock()
		delete(c.scopeTokens, scope)
		c.authMu.Unlock()

		_, err := c.AccessToken(ctx, scope)

		return err
	case authSkypeAndBearer:
		if scope != "" {
			c.authMu.Lock()
			delete(c.scopeTokens, scope)
			c.authMu.Unlock()
		}

		_, err := c.ForceRefreshSkypeToken(ctx)

		return err
	default:
		return errors.New("msteamsclient: unknown auth kind")
	}
}

func parseRetryAfter(header string) time.Duration {
	if header == "" {
		return 2 * time.Second
	}

	secs, err := strconv.Atoi(strings.TrimSpace(header))
	if err == nil && secs > 0 {
		return min(time.Duration(secs)*time.Second, 60*time.Second)
	}

	return 2 * time.Second
}

func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func sanitizeURLForErr(rawURL string) string {
	before, _, found := strings.Cut(rawURL, "?")
	if found {
		return before
	}

	return rawURL
}
