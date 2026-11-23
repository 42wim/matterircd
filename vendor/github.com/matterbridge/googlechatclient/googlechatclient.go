// Package googlechatclient is a small Google Chat client library used by
// matterircd (and usable by matterbridge), modeled on matterclient.
package googlechatclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/sirupsen/logrus"
)

const (
	defaultUserAgent   = "googlechatclient/0.0"
	defaultHTTPTimeout = 30 * time.Second
	maxAPIResponseSize = 16 << 20 // 16 MiB
	maxHTTPRetries     = 2
	postCacheSize      = 2000
	reactionCacheSize  = 2000
	sentCacheSize      = 256
	messageChanCap     = 100
)

// UsersCache holds cached users, conversations, memberships, and presence data.
// Its structure mirrors matterclient.UsersCache and msteamsclient.UsersCache so
// lock scopes stay separate from streaming and authentication state.
type UsersCache struct {
	mu                  sync.RWMutex
	users               map[string]*User
	conversations       map[string]*Conversation
	dmGroupIDs          map[string]struct{}
	channelMembers      map[string]map[string]struct{}
	channelAdmins       map[string]map[string]struct{}
	joinedChannels      map[string]struct{}
	channelLastViewedAt map[string]int64
	presences           map[string]*Presence
	lastUpdated         atomic.Int64
}

func newUsersCache() *UsersCache {
	return &UsersCache{
		users:               make(map[string]*User),
		conversations:       make(map[string]*Conversation),
		dmGroupIDs:          make(map[string]struct{}),
		channelMembers:      make(map[string]map[string]struct{}),
		channelAdmins:       make(map[string]map[string]struct{}),
		joinedChannels:      make(map[string]struct{}),
		channelLastViewedAt: make(map[string]int64),
		presences:           make(map[string]*Presence),
	}
}

// Client is a Google Chat client mirroring matterclient's API and lifecycle.
type Client struct {
	sync.RWMutex

	cred Credentials

	User        *User
	Users       *UsersCache
	MessageChan chan *Message

	// Lifecycle & persistence hooks (matching matterclient/msteamsclient callbacks).
	OnWsConnect           func()
	OnWsDisconnect        func(err error)
	OnChannelNameResolved func(channelID, name string)
	OnTokenUpdate         func(state TokenState)
	OnAuthPrompt          func(prompt AuthPrompt)

	ForceSyncOnReconnect bool
	CacheClearCutoff     time.Duration
	PollFallback         bool
	DisableStream        bool
	UserAgent            string
	Timeout              int

	//nolint:containedctx // Tied to the lifecycle of the persistent client session, matching matterclient.
	Ctx         context.Context
	loginCancel context.CancelFunc
	wg          sync.WaitGroup

	connected      atomic.Bool
	wsConnected    atomic.Bool
	lastWsActivity atomic.Int64
	apiReqSeq      atomic.Int64

	// Lock ordering: reauthMu -> authMu. Never hold authMu or Client.RWMutex across I/O.
	reauthMu   sync.Mutex
	lastReauth time.Time
	authMu     sync.RWMutex
	tokenState TokenState

	// streamMu guards the active WebChannel HTTP cancel func so Reconnect/Logout
	// can abort an in-flight long-poll stream without blocking.
	streamMu     sync.Mutex
	streamCancel context.CancelFunc

	httpClient   *http.Client
	streamClient *http.Client

	postCache     *lru.Cache[string, *ChatMessage]
	reactionCache *lru.Cache[string, map[string][]ReactionUser]
	sentCache     *lru.Cache[string, struct{}]

	typingLock     sync.Mutex
	lastTypingSent map[string]time.Time

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger
}

// New creates a new Google Chat client configured with cred.
func New(cred Credentials) *Client {
	rootLogger, rootAPILogger := newLoggers()
	logger := rootLogger.WithFields(logrus.Fields{"prefix": "googlechatclient"})
	apiLogger := rootAPILogger.WithFields(logrus.Fields{"prefix": "googlechatclient: API"})

	postCache, _ := lru.New[string, *ChatMessage](postCacheSize)
	reactionCache, _ := lru.New[string, map[string][]ReactionUser](reactionCacheSize)
	sentCache, _ := lru.New[string, struct{}](sentCacheSize)

	c := &Client{
		cred:           cred,
		Users:          newUsersCache(),
		MessageChan:    make(chan *Message, messageChanCap),
		UserAgent:      defaultUserAgent,
		postCache:      postCache,
		reactionCache:  reactionCache,
		sentCache:      sentCache,
		lastTypingSent: make(map[string]time.Time),
		rootLogger:     rootLogger,
		logger:         logger,
		rootAPILogger:  rootAPILogger,
		apiLogger:      apiLogger,
	}

	c.initHTTPClients()

	return c
}

func (c *Client) initHTTPClients() {
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

	loggedTransport := newAPILogTransport(transport, c.apiLogger)

	c.httpClient = &http.Client{
		Timeout:   timeout,
		Transport: loggedTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// WebChannel long-poll GET streams stay open indefinitely until server rollover or cancel.
	c.streamClient = &http.Client{
		Timeout:   0,
		Transport: loggedTransport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// SetLogAPICalls sets the log level for Google Chat API call logging.
// Accepted levels match matterclient: 'trace', 'debug', 'info', 'warn', 'error', 'fatal', 'panic'.
func (c *Client) SetLogAPICalls(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		c.logger.Warnf("Failed to parse specified log-level '%s': %#v", level, err)
	} else {
		c.rootAPILogger.SetLevel(l)
	}
}

// SetLogLevel sets the log level for googlechatclient internal logging.
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

// WsConnected reports whether the real-time WebChannel stream is currently active.
func (c *Client) WsConnected() bool {
	return c.wsConnected.Load()
}

// Reconnect aborts the active WebChannel stream so the supervisor immediately
// establishes a fresh session.
func (c *Client) Reconnect() {
	c.cancelActiveStream()
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

// TokenState returns a snapshot of the current session cookies and XSRF state.
func (c *Client) TokenState() TokenState {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	cp := c.tokenState
	cp.Cookies = cloneStringMap(c.tokenState.Cookies)

	return cp
}

// Login authenticates against Google Chat, syncs Spaces/DMs/users, and starts
// the background WebChannel real-time event loop.
func (c *Client) Login(ctx context.Context) error {
	c.initHTTPClients()

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

	err = c.UpdateChannels(loginCtx)
	if err != nil {
		c.logger.Warnf("Initial channel sync failed: %v", err)
	}

	c.connected.Store(true)

	if !c.DisableStream {
		c.startWebChannel(loginCtx)
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

	c.cancelActiveStream()
	c.wg.Wait()

	return nil
}

// Ping verifies that the Google Chat backend is reachable with the current session.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.doProto(ctx, "get_self_user_status", encodeGetSelfUserStatusRequest())

	return err
}

func (c *Client) doProto(ctx context.Context, endpoint string, reqBytes []byte) ([]byte, error) {
	seq := c.apiReqSeq.Add(1)
	rawURL := fmt.Sprintf("%s/api/%s?c=%d&rt=b&alt=proto", c.chatBaseURL(), endpoint, seq)

	for attempt := range maxHTTPRetries + 1 {
		respBytes, status, retryAfter, err := c.doProtoOnce(ctx, rawURL, reqBytes)

		retry, retryErr := c.shouldRetryHTTP(ctx, endpoint, attempt, status, retryAfter, err)
		if retryErr != nil {
			return nil, retryErr
		}

		if retry {
			continue
		}

		if status >= http.StatusBadRequest {
			return nil, fmt.Errorf("googlechatclient: POST /api/%s returned HTTP %d: %s", endpoint, status, truncateBody(respBytes))
		}

		return respBytes, nil
	}

	return nil, fmt.Errorf("googlechatclient: /api/%s exceeded retry limit", endpoint)
}

func (c *Client) doProtoOnce(ctx context.Context, targetURL string, reqBytes []byte) ([]byte, int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, 0, 0, err
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Goog-Encode-Response-If-Executable", "base64")
	c.applyAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if readErr != nil {
		return nil, resp.StatusCode, 0, readErr
	}

	if resp.StatusCode < http.StatusBadRequest && strings.EqualFold(resp.Header.Get("X-Goog-Safety-Encoding"), "base64") {
		decoded, decErr := decodeBase64Flexible(body)
		if decErr == nil {
			body = decoded
		}
	}

	return body, resp.StatusCode, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

func (c *Client) shouldRetryHTTP(ctx context.Context, endpoint string, attempt, status int, retryAfter time.Duration, reqErr error) (bool, error) {
	if reqErr != nil {
		if ctx.Err() != nil || attempt >= maxHTTPRetries {
			return false, reqErr
		}

		backoff := time.Duration(attempt+1) * time.Second
		c.logger.Warnf("Network error on /api/%s (%v), backing off %s (attempt %d/%d)",
			endpoint, reqErr, backoff, attempt+1, maxHTTPRetries)

		return true, sleepContext(ctx, backoff)
	}

	switch {
	case (status == http.StatusUnauthorized || status == http.StatusForbidden) && attempt == 0:
		refreshErr := c.handleUnauthorized(ctx)
		if refreshErr != nil {
			return false, fmt.Errorf("googlechatclient: HTTP %d on /api/%s (XSRF refresh failed: %w)", status, endpoint, refreshErr)
		}

		return true, nil
	case status == http.StatusTooManyRequests && attempt < maxHTTPRetries:
		c.logger.Warnf("Rate limited (429) on /api/%s, waiting %s (attempt %d/%d)",
			endpoint, retryAfter, attempt+1, maxHTTPRetries)

		return true, sleepContext(ctx, retryAfter)
	case status >= http.StatusInternalServerError && attempt < maxHTTPRetries:
		backoff := time.Duration(attempt+1) * time.Second
		c.logger.Warnf("Transient HTTP %d on /api/%s, backing off %s (attempt %d/%d)",
			status, endpoint, backoff, attempt+1, maxHTTPRetries)

		return true, sleepContext(ctx, backoff)
	default:
		return false, nil
	}
}

func decodeBase64Flexible(raw []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	out, err := base64.StdEncoding.DecodeString(string(trimmed))
	if err == nil {
		return out, nil
	}

	return base64.RawStdEncoding.DecodeString(string(trimmed))
}

func parseRetryAfter(raw string) time.Duration {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 2 * time.Second
	}

	secs, err := strconv.Atoi(s)
	if err == nil && secs > 0 {
		return min(time.Duration(secs)*time.Second, 30*time.Second)
	}

	t, err := http.ParseTime(s)
	if err == nil {
		d := time.Until(t)
		if d > 0 {
			return min(d, 30*time.Second)
		}
	}

	return 2 * time.Second
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func truncateBody(b []byte) string {
	const maxLen = 240

	s := strings.TrimSpace(string(b))
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}

	return s
}
