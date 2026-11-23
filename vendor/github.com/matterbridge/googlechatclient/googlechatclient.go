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
	defaultUserAgent     = "googlechatclient/0.0"
	defaultHTTPTimeout   = 30 * time.Second
	maxAPIResponseSize   = 16 << 20 // 16 MiB
	maxAPIRetries        = 10
	defaultRateLimitWait = 3 * time.Second
	maxRateLimitWaitSecs = 300
	cappedRateLimitWait  = 5 * time.Second
	postCacheSize        = 2000
	reactionCacheSize    = 2000
	sentCacheSize        = 256
	messageChanCap       = 100
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
	ResolveMention        func(token string) (userID, displayName string)

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
	aborted        atomic.Bool
	wsConnected    atomic.Bool
	lastWsActivity atomic.Int64
	apiReqSeq      atomic.Int64

	// Lock ordering: reauthMu -> authMu. Never hold authMu or Client.RWMutex across I/O.
	reauthMu   sync.Mutex
	lastReauth time.Time
	authMu     sync.RWMutex
	tokenState TokenState
	boqAT      string
	boqSID     string
	boqBL      string

	// streamMu guards the active WebChannel session and HTTP cancel func so
	// Reconnect/Logout and dynamic group subscriptions operate safely without blocking.
	streamMu     sync.Mutex
	streamCancel context.CancelFunc
	activeSess   *webChannelSession

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
	c.aborted.Store(false)
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

	c.wg.Go(func() {
		c.runCookieRotation(loginCtx)
	})

	if !c.DisableStream {
		c.startWebChannel(loginCtx)
	}

	return nil
}

// Logout terminates background goroutines and marks the client disconnected.
func (c *Client) Logout(_ context.Context) error {
	c.aborted.Store(true)
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

// IsAborted reports whether Logout has been called or ctx has been canceled.
func (c *Client) IsAborted(ctx context.Context) bool {
	if c.aborted.Load() {
		return true
	}

	return ctx != nil && ctx.Err() != nil
}

// HandleRatelimit sleeps for the duration requested by an HTTP 429 response,
// checking Retry-After first and falling back to X-RateLimit-Reset (mirroring matterclient).
func (c *Client) HandleRatelimit(ctx context.Context, name string, resp *http.Response) error {
	if resp == nil {
		return fmt.Errorf("googlechatclient: got a nil response from %s", name)
	}

	if resp.StatusCode != http.StatusTooManyRequests {
		return fmt.Errorf("googlechatclient: status code error: %d", resp.StatusCode)
	}

	waitSecs, hasHeader := parseRateLimitWaitSeconds(resp.Header)
	if !hasHeader {
		_, err := c.sleepWithContext(ctx, defaultRateLimitWait)

		return err
	}

	if waitSecs < 1 {
		waitSecs = 1
	}

	wait := time.Duration(waitSecs) * time.Second
	if waitSecs > maxRateLimitWaitSecs {
		c.logger.Warnf("Rate limit reset time (%d seconds) looks suspiciously long; capping sleep to %d seconds",
			waitSecs, int(cappedRateLimitWait/time.Second))
		wait = cappedRateLimitWait
	} else {
		c.logger.Warnf("Ratelimited on %s for %d seconds", name, waitSecs)
	}

	_, err := c.sleepWithContext(ctx, wait)

	return err
}

// HandleRetry decides whether a failed API call should be retried, sleeping
// before returning true (mirroring matterclient.HandleRetry).
func (c *Client) HandleRetry(ctx context.Context, name string, err error, current, maxLimit int, resp *http.Response) (bool, error) {
	if c.IsAborted(ctx) {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}

		return false, context.Canceled
	}

	if current >= maxLimit {
		return false, nil
	}

	sleepDuration := time.Duration(current+1) * time.Second

	if resp == nil {
		c.logger.Warnf("Network error on %s (resp is nil, err: %v), backing off: %s (attempt %d/%d)",
			name, err, sleepDuration, current+1, maxLimit)

		return c.sleepWithContext(ctx, sleepDuration)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		rlErr := c.HandleRatelimit(ctx, name, resp)
		if rlErr != nil && c.IsAborted(ctx) {
			return false, rlErr
		}

		return true, nil

	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return c.handleUnauthorizedRetry(ctx, name, current, resp.StatusCode), nil

	case resp.StatusCode >= http.StatusInternalServerError:
		c.logger.Warnf("Transient error %d on %s, backing off: %s (attempt %d/%d)",
			resp.StatusCode, name, sleepDuration, current+1, maxLimit)

		return c.sleepWithContext(ctx, sleepDuration)

	case resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusInternalServerError:
		// Client errors (4xx) should not be retried unless 429 or 401/403 above.
		return false, nil

	default:
		return c.sleepWithContext(ctx, sleepDuration)
	}
}

func (c *Client) handleUnauthorizedRetry(ctx context.Context, name string, current, status int) bool {
	if current > 0 || c.IsAborted(ctx) {
		return false
	}

	c.logger.Warnf("HTTP %d on %s, attempting session cookie/XSRF refresh", status, name)

	refreshErr := c.handleUnauthorized(ctx)
	if refreshErr != nil {
		c.logger.Errorf("Session refresh failed on %s: %v", name, refreshErr)

		return false
	}

	return true
}

func (c *Client) doProto(ctx context.Context, endpoint string, reqBytes []byte) ([]byte, error) {
	seq := c.apiReqSeq.Add(1)
	rawURL := fmt.Sprintf("%s/api/%s?c=%d&rt=b&alt=proto", c.chatBaseURL(), endpoint, seq)
	opName := "/api/" + endpoint

	for attempt := 0; ; attempt++ {
		respBytes, status, header, err := c.doProtoOnce(ctx, rawURL, reqBytes)
		if err == nil && status > 0 && status < http.StatusBadRequest {
			return respBytes, nil
		}

		var resp *http.Response
		if status > 0 {
			resp = &http.Response{
				StatusCode: status,
				Header:     header,
			}
		}

		shouldRetry, retryErr := c.HandleRetry(ctx, opName, err, attempt, maxAPIRetries, resp)
		if retryErr != nil {
			return nil, retryErr
		}

		if shouldRetry {
			continue
		}

		if err != nil {
			return nil, err
		}

		if status > 0 {
			return nil, fmt.Errorf("googlechatclient: POST %s returned HTTP %d: %s", opName, status, truncateBody(respBytes))
		}

		return nil, fmt.Errorf("googlechatclient: POST %s failed with nil response", opName)
	}
}

func (c *Client) doProtoOnce(ctx context.Context, targetURL string, reqBytes []byte) ([]byte, int, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBytes))
	if err != nil {
		return nil, 0, nil, err
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("X-Goog-Encode-Response-If-Executable", "base64")
	c.applyAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize))
	if readErr != nil {
		return nil, resp.StatusCode, resp.Header, readErr
	}

	if resp.StatusCode < http.StatusBadRequest && strings.EqualFold(resp.Header.Get("X-Goog-Safety-Encoding"), "base64") {
		decoded, decErr := decodeBase64Flexible(body)
		if decErr == nil {
			body = decoded
		}
	}

	return body, resp.StatusCode, resp.Header, nil
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

func parseRateLimitWaitSeconds(h http.Header) (int64, bool) {
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		secs, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			return secs, true
		}

		t, parseErr := http.ParseTime(raw)
		if parseErr == nil {
			return int64(time.Until(t).Seconds()), true
		}
	}

	if raw := strings.TrimSpace(h.Get("X-RateLimit-Reset")); raw != "" { //nolint:canonicalheader
		val, err := strconv.ParseInt(raw, 10, 64)
		if err == nil {
			if val > 1000000 {
				return val - time.Now().Unix(), true
			}

			return val, true
		}
	}

	return 0, false
}

func (c *Client) sleepWithContext(ctx context.Context, d time.Duration) (bool, error) {
	if c.IsAborted(ctx) {
		if ctx != nil && ctx.Err() != nil {
			return false, ctx.Err()
		}

		return false, context.Canceled
	}

	err := sleepContext(ctx, d)
	if err != nil {
		return false, err
	}

	if c.IsAborted(ctx) {
		return false, context.Canceled
	}

	return true, nil
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		if ctx != nil {
			return ctx.Err()
		}

		return nil
	}

	t := time.NewTimer(d)
	defer t.Stop()

	if ctx == nil {
		<-t.C

		return nil
	}

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
