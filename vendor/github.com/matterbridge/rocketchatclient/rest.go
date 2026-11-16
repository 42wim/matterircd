package rocketchatclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxAPIRetries     = 5
	defaultRateWait   = 3 * time.Second
	maxRateLimitWait  = 60 * time.Second
	userListPageLimit = 100
)

// APIError represents an error payload returned by the Rocket.Chat REST API.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("rocketchat REST %d (%s): %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("rocketchat REST %d: %s", e.StatusCode, e.Message)
	case e.Code != "":
		return fmt.Sprintf("rocketchat REST %d: %s", e.StatusCode, e.Code)
	default:
		return fmt.Sprintf("rocketchat REST %d", e.StatusCode)
	}
}

func normalizeServerURL(server string, noTLS bool) (string, string, error) {
	raw := strings.TrimSpace(server)
	if raw == "" {
		return "", "", errors.New("rocketchatclient: empty server URL")
	}

	if !strings.Contains(raw, "://") {
		if noTLS {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("rocketchatclient: invalid server URL %q: %w", server, err)
	}

	if parsed.Host == "" {
		return "", "", fmt.Errorf("rocketchatclient: missing host in server URL %q", server)
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""

	httpURL := parsed.String()

	wsParsed := *parsed
	if wsParsed.Scheme == "http" {
		wsParsed.Scheme = "ws"
	} else {
		wsParsed.Scheme = "wss"
	}

	wsParsed.Path += "/websocket"

	return httpURL, wsParsed.String(), nil
}

func parseTokenCredential(raw string) (string, string, bool) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "token=")

	userID, authToken, ok := strings.Cut(trimmed, ":")
	if !ok || userID == "" || authToken == "" {
		return "", "", false
	}

	return userID, authToken, true
}

func (c *Client) authenticate(ctx context.Context) error {
	if c.cred.Token != "" {
		userID, authToken, ok := parseTokenCredential(c.cred.Token)
		if !ok {
			return errors.New("rocketchatclient: token must be formatted as <userId>:<authToken>")
		}

		c.authMu.Lock()
		c.userID = userID
		c.authToken = authToken
		c.tokenLogin = true
		c.authMu.Unlock()

		me, err := c.fetchMe(ctx)
		if err != nil {
			return fmt.Errorf("rocketchatclient: token verification failed: %w", err)
		}

		c.authMu.Lock()
		c.me = me
		c.authMu.Unlock()
		c.cache.upsertUser(me)

		return nil
	}

	if c.cred.Login == "" || c.cred.Pass == "" {
		return errors.New("rocketchatclient: login and password or token are required")
	}

	data, err := c.loginPassword(ctx, c.cred.Login, c.cred.Pass, c.cred.TwoFactorCode)
	if err != nil {
		return err
	}

	c.authMu.Lock()
	c.userID = data.UserID
	c.authToken = data.AuthToken
	c.tokenLogin = false

	if data.Me != nil {
		c.me = data.Me
	}

	c.authMu.Unlock()

	if data.Me != nil {
		c.cache.upsertUser(data.Me)
	} else {
		me, err := c.fetchMe(ctx)
		if err == nil {
			c.authMu.Lock()
			c.me = me
			c.authMu.Unlock()
			c.cache.upsertUser(me)
		}
	}

	return nil
}

type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}

type loginData struct {
	UserID    string `json:"userId"`
	AuthToken string `json:"authToken"`
	Me        *User  `json:"me"`
}

type loginResponse struct {
	Status string     `json:"status"`
	Data   *loginData `json:"data"`
}

func (c *Client) loginPassword(ctx context.Context, user, pass, code string) (*loginData, error) {
	req := loginRequest{
		User:     user,
		Password: pass,
		Code:     code,
	}

	var resp loginResponse

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/login", nil, req, &resp, false)
	if err != nil {
		return nil, err
	}

	if resp.Data == nil || resp.Data.UserID == "" || resp.Data.AuthToken == "" {
		return nil, errors.New("rocketchatclient: login response missing userId or authToken")
	}

	return resp.Data, nil
}

func (c *Client) fetchMe(ctx context.Context) (*User, error) {
	var user User

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/me", nil, nil, &user, true)
	if err != nil {
		return nil, err
	}

	if user.ID == "" {
		return nil, errors.New("rocketchatclient: /api/v1/me response missing _id")
	}

	return &user, nil
}

func (c *Client) logoutREST(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/logout", nil, nil, nil, true)
}

// GetSubscriptions fetches the user's subscriptions, optionally filtered by updatedSince.
func (c *Client) GetSubscriptions(ctx context.Context, updatedSince *time.Time) (*SubscriptionsResponse, error) {
	var query url.Values
	if updatedSince != nil && !updatedSince.IsZero() {
		query = url.Values{}
		query.Set("updatedSince", updatedSince.UTC().Format(time.RFC3339Nano))
	}

	var resp SubscriptionsResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/subscriptions.get", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetRooms fetches rooms accessible to the user, optionally filtered by updatedSince.
func (c *Client) GetRooms(ctx context.Context, updatedSince *time.Time) (*RoomsResponse, error) {
	var query url.Values
	if updatedSince != nil && !updatedSince.IsZero() {
		query = url.Values{}
		query.Set("updatedSince", updatedSince.UTC().Format(time.RFC3339Nano))
	}

	var resp RoomsResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/rooms.get", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// ListUsers fetches a page of workspace users.
func (c *Client) ListUsers(ctx context.Context, offset, count int) (*UsersListResponse, error) {
	query := url.Values{}
	query.Set("offset", strconv.Itoa(offset))
	query.Set("count", strconv.Itoa(count))

	var resp UsersListResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/users.list", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetUserInfo fetches a single user by userID or username and updates the cache.
func (c *Client) GetUserInfo(ctx context.Context, userID, username string) (*User, error) {
	query := url.Values{}
	if userID != "" {
		query.Set("userId", userID)
	} else if username != "" {
		query.Set("username", username)
	}

	var resp struct {
		User    *User `json:"user"`
		Success bool  `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/users.info", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	if resp.User == nil {
		return nil, errors.New("rocketchatclient: users.info response missing user")
	}

	c.cache.upsertUser(resp.User)

	return resp.User, nil
}

// DownloadURL returns the authenticated download URL for an uploaded Rocket.Chat file.
func (c *Client) DownloadURL(fileID, fileName string) string {
	if fileID == "" {
		return ""
	}

	if fileName == "" {
		return c.httpURL + "/file-upload/" + url.PathEscape(fileID)
	}

	return c.httpURL + "/file-upload/" + url.PathEscape(fileID) + "/" + url.PathEscape(fileName)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, query url.Values, reqBody, out any, authenticated bool) error {
	var bodyBytes []byte

	if reqBody != nil {
		var err error

		bodyBytes, err = json.Marshal(reqBody)
		if err != nil {
			return err
		}
	}

	target := c.httpURL + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	for attempt := range maxAPIRetries {
		retryWait, err := c.doJSONAttempt(ctx, method, endpoint, target, bodyBytes, out, authenticated, attempt)
		if err == nil {
			return nil
		}

		if retryWait <= 0 {
			return err
		}

		errSleep := sleepContext(ctx, retryWait)
		if errSleep != nil {
			return errSleep
		}
	}

	return fmt.Errorf("rocketchatclient: %s %s exceeded max retries", method, endpoint)
}

func (c *Client) buildJSONRequest(ctx context.Context, method, target string, bodyBytes []byte, authenticated bool) (*http.Request, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, err
	}

	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.opts.userAgent != "" {
		req.Header.Set("User-Agent", c.opts.userAgent)
	}

	if authenticated {
		userID, authToken := c.authCredentials()
		req.Header.Set("X-User-Id", userID)
		req.Header.Set("X-Auth-Token", authToken)
	}

	return req, nil
}

func (c *Client) doJSONAttempt(ctx context.Context, method, endpoint, target string, bodyBytes []byte, out any, authenticated bool, attempt int) (time.Duration, error) {
	req, err := c.buildJSONRequest(ctx, method, target, bodyBytes, authenticated)
	if err != nil {
		return 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if attempt+1 < maxAPIRetries && method == http.MethodGet && ctx.Err() == nil {
			return time.Duration(attempt+1) * time.Second, err
		}

		return 0, err
	}

	respBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if readErr != nil {
		return 0, readErr
	}

	return c.handleJSONResponse(method, endpoint, resp, respBytes, out, attempt)
}

func (c *Client) handleJSONResponse(method, endpoint string, resp *http.Response, respBytes []byte, out any, attempt int) (time.Duration, error) {
	if resp.StatusCode == http.StatusTooManyRequests && attempt+1 < maxAPIRetries {
		wait := rateLimitWait(resp.Header)
		c.logger.Warnf("%s %s rate-limited (429), retrying in %s", method, endpoint, wait.Round(time.Millisecond))

		return wait, fmt.Errorf("rate limited (%d)", resp.StatusCode)
	}

	if resp.StatusCode >= http.StatusInternalServerError && attempt+1 < maxAPIRetries && method == http.MethodGet {
		return time.Duration(attempt+1) * time.Second, fmt.Errorf("server error (%d)", resp.StatusCode)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return 0, parseAPIError(resp.StatusCode, respBytes)
	}

	err := checkEnvelopeError(resp.StatusCode, respBytes)
	if err != nil {
		return 0, err
	}

	if out != nil && len(respBytes) > 0 {
		err = json.Unmarshal(respBytes, out)
		if err != nil {
			return 0, fmt.Errorf("rocketchatclient: decode %s %s: %w", method, endpoint, err)
		}
	}

	return 0, nil
}

func checkEnvelopeError(statusCode int, respBytes []byte) error {
	if len(respBytes) == 0 {
		return nil
	}

	var envelope struct {
		Status    string `json:"status"`
		Success   *bool  `json:"success"`
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Message   string `json:"message"`
	}

	err := json.Unmarshal(respBytes, &envelope)
	if err != nil {
		return nil //nolint:nilerr // non-JSON or alternate payloads are decoded by the caller
	}

	if envelope.Status != "error" && (envelope.Success == nil || *envelope.Success) {
		return nil
	}

	code := envelope.ErrorType
	if code == "" {
		code = envelope.Error
	}

	msg := envelope.Message
	if msg == "" && envelope.Error != "" && envelope.Error != code {
		msg = envelope.Error
	}

	return &APIError{
		StatusCode: statusCode,
		Code:       code,
		Message:    msg,
	}
}

func rateLimitWait(hdr http.Header) time.Duration {
	if wait, ok := parseResetHeader(hdr.Get("X-Ratelimit-Reset")); ok {
		return wait
	}

	if retryAfter := strings.TrimSpace(hdr.Get("Retry-After")); retryAfter != "" {
		secs, err := strconv.ParseInt(retryAfter, 10, 64)
		if err == nil && secs > 0 {
			return min(time.Duration(secs)*time.Second, maxRateLimitWait)
		}
	}

	return defaultRateWait
}

func parseResetHeader(raw string) (time.Duration, bool) {
	resetRaw := strings.TrimSpace(raw)
	if resetRaw == "" {
		return 0, false
	}

	resetVal, err := strconv.ParseInt(resetRaw, 10, 64)
	if err != nil || resetVal <= 0 {
		return 0, false
	}

	var resetTime time.Time
	if resetVal > 1_000_000_000_000 {
		resetTime = time.UnixMilli(resetVal)
	} else {
		resetTime = time.Unix(resetVal, 0)
	}

	wait := time.Until(resetTime)
	if wait <= 0 {
		return 100 * time.Millisecond, true
	}

	return min(wait, maxRateLimitWait), true
}

func parseAPIError(statusCode int, body []byte) error {
	if len(body) == 0 {
		return &APIError{StatusCode: statusCode}
	}

	var envelope struct {
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Message   string `json:"message"`
	}

	err := json.Unmarshal(body, &envelope)
	if err != nil {
		return &APIError{
			StatusCode: statusCode,
			Message:    strings.TrimSpace(string(body)),
		}
	}

	code := envelope.ErrorType
	if code == "" {
		code = envelope.Error
	}

	msg := envelope.Message
	if msg == "" && envelope.Error != "" && envelope.Error != code {
		msg = envelope.Error
	}

	return &APIError{
		StatusCode: statusCode,
		Code:       code,
		Message:    msg,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
