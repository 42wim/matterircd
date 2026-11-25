package googlechatclient

import (
	"context"
	"crypto/sha1" //nolint:gosec // G505: SHA-1 is required by Google's SAPISIDHASH web auth header format.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	chatOriginURL        = "https://chat.google.com"
	mailOriginURL        = "https://mail.google.com"
	rotateCookiesURL     = "https://accounts.google.com/RotateCookies"
	rotateCookiesBody    = `[0, "-0000000000000000000"]`
	defaultBrowserUA     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
	maxAuthHTMLSize      = 8 << 20 // 8 MiB
	wizGlobalDataStart   = ">window.WIZ_global_data = "
	wizGlobalDataEnd     = ";</script>"
	moleWorldHSParam     = `["h_hs",null,null,[1,0],null,null,"gmail.pinto-server_20230730.06_p0",1,null,[15,38,36,35,26,30,41,18,24,11,21,14,6],null,null,"3Mu86PSulM4.en..es5",0,null,null,[0]]`
	reauthCooldown       = 10 * time.Second
	cookieRotateInterval = 9 * time.Minute
)

// ParseCookies parses browser cookies from a JSON TokenState, a browser DevTools
// JSON cookie array ([{"name":...,"value":...}]), a flat JSON object
// ({"SID":...}), or a raw "Cookie:" header string ("SID=...; HSID=...").
func ParseCookies(raw string) map[string]string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}

	if strings.HasPrefix(s, "{") {
		if cookies := parseJSONObjectCookies(s); len(cookies) > 0 {
			return cookies
		}
	}

	if strings.HasPrefix(s, "[") {
		if cookies := parseJSONArrayCookies(s); len(cookies) > 0 {
			return cookies
		}
	}

	return parseRawCookieHeader(s)
}

func parseJSONObjectCookies(raw string) map[string]string {
	var state TokenState

	err := json.Unmarshal([]byte(raw), &state)
	if err == nil && len(state.Cookies) > 0 {
		return cloneStringMap(state.Cookies)
	}

	var flat map[string]any

	err = json.Unmarshal([]byte(raw), &flat)
	if err != nil {
		return nil
	}

	out := make(map[string]string, len(flat))

	for k, v := range flat {
		str, ok := v.(string)
		if ok && strings.TrimSpace(k) != "" && strings.TrimSpace(str) != "" {
			out[strings.TrimSpace(k)] = strings.TrimSpace(str)
		}
	}

	return out
}

func parseJSONArrayCookies(raw string) map[string]string {
	var entries []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
		Path  string `json:"path"`
	}

	err := json.Unmarshal([]byte(raw), &entries)
	if err != nil {
		return nil
	}

	out := make(map[string]string, len(entries))

	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		val := strings.TrimSpace(e.Value)

		if name == "" || val == "" {
			continue
		}

		// Prefer root-path cookies (especially COMPASS on "/") when multiple paths exist.
		if _, exists := out[name]; exists && e.Path != "/" {
			continue
		}

		out[name] = val
	}

	return out
}

func parseRawCookieHeader(raw string) map[string]string {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(s), "cookie:") {
		s = strings.TrimSpace(s[len("cookie:"):])
	}

	out := make(map[string]string)

	for part := range strings.SplitSeq(s, ";") {
		name, val, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}

		name = strings.TrimSpace(name)
		val = strings.TrimSpace(val)

		if name != "" && val != "" {
			out[name] = val
		}
	}

	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

// Authenticate validates supplied cookies, fetches a fresh XSRF token from
// chat.google.com, identifies the signed-in Google account, and persists the
// updated session snapshot via OnTokenUpdate.
func (c *Client) Authenticate(ctx context.Context) error {
	c.loadInitialTokenState()

	if !c.hasCoreCookies() {
		if c.OnAuthPrompt != nil {
			c.OnAuthPrompt(AuthPrompt{
				Message: "Google Chat requires browser session cookies (SID, HSID, SSID, OSID, SAPISID, COMPASS) from chat.google.com in TokenSaveFile or via LOGIN cookie <cookies>.",
			})
		}

		return errors.New("googlechatclient: missing required browser cookies (SID, HSID, SSID, OSID)")
	}

	err := c.refreshXSRFWithRecovery(ctx)
	if err != nil {
		return err
	}

	err = c.fetchSelfIdentity(ctx)
	if err != nil {
		return err
	}

	err = c.validateAccountType()
	if err != nil {
		return err
	}

	c.notifyTokenUpdate(c.TokenState())

	return nil
}

func (c *Client) loadInitialTokenState() {
	c.authMu.Lock()
	defer c.authMu.Unlock()

	if c.tokenState.Cookies == nil {
		c.tokenState.Cookies = make(map[string]string)
	}

	if c.cred.TokenState != nil {
		st := *c.cred.TokenState
		st.Cookies = cloneStringMap(c.cred.TokenState.Cookies)

		if st.Cookies == nil {
			st.Cookies = make(map[string]string)
		}

		c.tokenState = st
	}

	if c.cred.AuthUser > 0 {
		c.tokenState.AuthUser = c.cred.AuthUser
	}

	if c.cred.AccountType != "" && c.cred.AccountType != AccountAuto {
		c.tokenState.AccountType = c.cred.AccountType
	}

	if c.cred.RawCookies != "" {
		maps.Copy(c.tokenState.Cookies, ParseCookies(c.cred.RawCookies))
	}

	maps.Copy(c.tokenState.Cookies, c.cred.Cookies)

	if csid := extractCSessionID(c.tokenState.Cookies["COMPASS"]); csid != "" {
		c.tokenState.CSessionID = csid
	}
}

func (c *Client) hasCoreCookies() bool {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	cookies := c.tokenState.Cookies
	if len(cookies) == 0 {
		return false
	}

	for _, req := range []string{"SID", "HSID", "SSID", "OSID"} {
		if cookies[req] == "" {
			return false
		}
	}

	return true
}

func (c *Client) authUserIndex() int {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	if c.tokenState.AuthUser > 0 {
		return c.tokenState.AuthUser
	}

	return c.cred.AuthUser
}

func (c *Client) chatBaseURL() string {
	idx := c.authUserIndex()
	if idx > 0 {
		return fmt.Sprintf("%s/u/%d", chatOriginURL, idx)
	}

	return chatOriginURL
}

func (c *Client) refreshXSRFToken(ctx context.Context) error {
	params := url.Values{}
	params.Set("origin", mailOriginURL)
	params.Set("shell", "9")
	params.Set("hl", "en")
	params.Set("wfi", "gtn-roster-iframe-id")
	params.Set("hs", moleWorldHSParam)

	targetURL := c.chatBaseURL() + "/mole/world?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("googlechatclient: build xsrf request: %w", err)
	}

	req.Header.Set("Referer", mailOriginURL+"/")
	req.Header.Set("User-Agent", defaultBrowserUA)
	c.applyCookieAndAuthUserHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("googlechatclient: fetch /mole/world: %w", err)
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	if resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		loc := resp.Header.Get("Location")

		u, parseErr := url.Parse(loc)
		if parseErr == nil && u.Host != "" {
			c.logger.Debugf("/mole/world redirected (%d) to %s%s", resp.StatusCode, u.Host, u.Path)
		}

		if strings.Contains(loc, "accounts.google.com") {
			return errors.New("googlechatclient: authentication failed (cookies expired or redirected to Google sign-in)")
		}

		return fmt.Errorf("googlechatclient: unexpected redirect (%d) from /mole/world", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthHTMLSize))
	if err != nil {
		return fmt.Errorf("googlechatclient: read /mole/world response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("googlechatclient: /mole/world returned HTTP %d", resp.StatusCode)
	}

	return c.parseWizGlobalData(string(body))
}

func (c *Client) parseWizGlobalData(htmlBody string) error {
	_, payload, ok := strings.Cut(htmlBody, wizGlobalDataStart)
	if !ok {
		return errors.New("googlechatclient: missing WIZ_global_data in /mole/world response (cookies may be invalid)")
	}

	rawJSON, _, ok := strings.Cut(payload, wizGlobalDataEnd)
	if !ok {
		return errors.New("googlechatclient: unterminated WIZ_global_data in /mole/world response")
	}

	var wiz map[string]any

	err := json.Unmarshal([]byte(rawJSON), &wiz)
	if err != nil {
		return fmt.Errorf("googlechatclient: decode WIZ_global_data: %w", err)
	}

	if uiType, _ := wiz["qwAQke"].(string); uiType == "AccountsSignInUi" {
		return errors.New("googlechatclient: authentication failed (cookies expired)")
	}

	xsrf, _ := wiz["SMqcke"].(string)
	if xsrf == "" {
		return errors.New("googlechatclient: missing XSRF token in WIZ_global_data")
	}

	c.authMu.Lock()
	c.tokenState.XSRFToken = xsrf
	c.applyWizTokensLocked(wiz, htmlBody)
	c.tokenState.UpdatedAt = time.Now().UTC()
	c.authMu.Unlock()

	return nil
}

func (c *Client) applyWizTokensLocked(wiz map[string]any, htmlBody string) {
	if boqAT, _ := wiz["SNlM0e"].(string); boqAT != "" {
		c.boqAT = boqAT
	}

	if boqSID, _ := wiz["FdrFJe"].(string); boqSID != "" {
		c.boqSID = boqSID
	}

	if boqBL, _ := wiz["cfb2h"].(string); boqBL != "" {
		c.boqBL = boqBL
	}

	if keys := extractGoogleAPIKeys(wiz, htmlBody); len(keys) > 0 {
		c.apiKeys = keys
		if c.apiKey != "" && !slices.Contains(keys, c.apiKey) {
			c.apiKey = ""
		}
	}

	if sapisid, _ := wiz["WZsZ1e"].(string); sapisid != "" {
		if c.tokenState.Cookies == nil {
			c.tokenState.Cookies = make(map[string]string)
		}

		c.tokenState.Cookies["SAPISID"] = sapisid
	}

	if email, _ := wiz["oPEP7c"].(string); email != "" && c.tokenState.Email == "" {
		c.tokenState.Email = email
	}
}

func extractGoogleAPIKeys(wiz map[string]any, htmlBody string) []string {
	seen := make(map[string]struct{})

	var keys []string

	wizNames := make([]string, 0, len(wiz))
	for k := range wiz {
		wizNames = append(wizNames, k)
	}

	slices.Sort(wizNames)

	for _, name := range wizNames {
		if s, ok := wiz[name].(string); ok && isGoogleAPIKey(s) {
			if _, dup := seen[s]; !dup {
				seen[s] = struct{}{}
				keys = append(keys, s)
			}
		}
	}

	const prefix = "AIzaSy"

	for pos := 0; pos < len(htmlBody); {
		rel := strings.Index(htmlBody[pos:], prefix)
		if rel < 0 {
			break
		}

		start := pos + rel
		end := start + len(prefix)

		for end < len(htmlBody) && end-start < 45 && isAPIKeyChar(htmlBody[end]) {
			end++
		}

		if cand := htmlBody[start:end]; isGoogleAPIKey(cand) {
			if _, dup := seen[cand]; !dup {
				seen[cand] = struct{}{}
				keys = append(keys, cand)
			}
		}

		pos = end
	}

	return keys
}

func isAPIKeyChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-'
}

func isGoogleAPIKey(s string) bool {
	return strings.HasPrefix(s, "AIzaSy") && len(s) >= 35 && len(s) <= 45
}

func (c *Client) fetchSelfIdentity(ctx context.Context) error {
	respBytes, err := c.doProto(ctx, "get_self_user_status", encodeGetSelfUserStatusRequest())
	if err != nil {
		return fmt.Errorf("googlechatclient: get_self_user_status: %w", err)
	}

	st, err := decodeGetSelfUserStatusResponse(respBytes)
	if err != nil {
		return fmt.Errorf("googlechatclient: decode self user status: %w", err)
	}

	if st == nil || st.UserID == "" {
		return errors.New("googlechatclient: get_self_user_status returned empty user ID")
	}

	me := &User{
		ID: st.UserID,
	}

	resolved, resolveErr := c.ResolveUsers(ctx, []string{st.UserID})
	if resolveErr == nil {
		if full := resolved[st.UserID]; full != nil {
			me = full
		}
	}

	c.authMu.Lock()

	if me.Email == "" && c.tokenState.Email != "" {
		me.Email = c.tokenState.Email
	}

	c.tokenState.UserID = me.ID
	c.tokenState.DisplayName = me.DisplayName
	c.tokenState.Email = me.Email
	c.tokenState.OrganizationID = me.OrganizationID
	c.tokenState.AccountType = detectAccountType(me)
	c.tokenState.UpdatedAt = time.Now().UTC()
	c.authMu.Unlock()

	c.Lock()
	cp := *me
	c.User = &cp
	c.Unlock()

	c.CacheUser(me)
	c.applyUserStatusUpdate(st)

	return nil
}

func detectAccountType(u *User) AccountType {
	if u == nil {
		return AccountPersonal
	}

	if u.IsConsumer {
		return AccountPersonal
	}

	if u.OrganizationID != "" {
		return AccountWork
	}

	lower := strings.ToLower(strings.TrimSpace(u.Email))
	if strings.HasSuffix(lower, "@gmail.com") || strings.HasSuffix(lower, "@googlemail.com") {
		return AccountPersonal
	}

	if lower != "" {
		return AccountWork
	}

	return AccountPersonal
}

func (c *Client) validateAccountType() error {
	desired := c.cred.AccountType
	if desired == "" || desired == AccountAuto {
		return nil
	}

	c.authMu.RLock()
	actual := c.tokenState.AccountType
	c.authMu.RUnlock()

	if actual != "" && actual != desired {
		return fmt.Errorf("googlechatclient: signed-in account is %s, expected %s", actual, desired)
	}

	return nil
}

func (c *Client) handleUnauthorized(ctx context.Context) error {
	c.reauthMu.Lock()
	defer c.reauthMu.Unlock()

	if !c.lastReauth.IsZero() && time.Since(c.lastReauth) < reauthCooldown {
		return nil
	}

	rotErr := c.RotateCookies(ctx)
	if rotErr != nil {
		c.logger.Debugf("RotateCookies during reauth failed: %v", rotErr)
	}

	err := c.refreshXSRFWithRecovery(ctx)
	if err != nil {
		return err
	}

	c.lastReauth = time.Now()
	c.notifyTokenUpdate(c.TokenState())

	return nil
}

func (c *Client) refreshXSRFWithRecovery(ctx context.Context) error {
	err := c.refreshXSRFToken(ctx)
	if err == nil {
		return nil
	}

	// Attempt a cookie rotation in case __Secure-1PSIDTS/__Secure-3PSIDTS expired
	// while the underlying session cookies are still valid.
	rotErr := c.RotateCookies(ctx)
	if rotErr != nil {
		c.logger.Debugf("RotateCookies during auth recovery failed: %v", rotErr)

		return err
	}

	return c.refreshXSRFToken(ctx)
}

func (c *Client) applyAuthHeaders(req *http.Request) {
	c.applyCookieAndAuthUserHeaders(req)

	c.authMu.RLock()
	xsrf := c.tokenState.XSRFToken

	sapisid := c.tokenState.Cookies["SAPISID"]
	if sapisid == "" {
		sapisid = c.tokenState.Cookies["__Secure-3PAPISID"]
	}

	c.authMu.RUnlock()

	if xsrf != "" {
		req.Header.Set("X-Framework-Xsrf-Token", xsrf)
	}

	if sapisid != "" {
		req.Header.Set("Authorization", sapisidAuthHeader(sapisid, chatOriginURL, time.Now()))
	}

	req.Header.Set("Origin", chatOriginURL)
	req.Header.Set("Referer", chatOriginURL+"/")
	req.Header.Set("User-Agent", defaultBrowserUA)
}

func (c *Client) applyCookieAndAuthUserHeaders(req *http.Request) {
	c.authMu.RLock()
	cookieHeader := formatCookieHeader(c.tokenState.Cookies, c.tokenState.CSessionID)
	authUser := c.tokenState.AuthUser
	c.authMu.RUnlock()

	if cookieHeader != "" {
		req.Header.Set("Cookie", cookieHeader)
	}

	if authUser > 0 {
		req.Header.Set("X-Goog-Authuser", strconv.Itoa(authUser))
	}
}

// RotateCookies calls accounts.google.com/RotateCookies to refresh short-lived
// session security cookies (__Secure-1PSIDTS / __Secure-3PSIDTS) and persists
// any updated cookies via OnTokenUpdate.
func (c *Client) RotateCookies(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rotateCookiesURL, strings.NewReader(rotateCookiesBody))
	if err != nil {
		return fmt.Errorf("googlechatclient: build RotateCookies request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", chatOriginURL)
	req.Header.Set("Referer", chatOriginURL+"/")
	req.Header.Set("User-Agent", defaultBrowserUA)
	c.applyCookieAndAuthUserHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("googlechatclient: RotateCookies: %w", err)
	}

	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	c.absorbResponseCookies(resp.Cookies())

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("googlechatclient: RotateCookies returned HTTP %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) runCookieRotation(ctx context.Context) {
	ticker := time.NewTicker(cookieRotateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err := c.RotateCookies(ctx)
			if err != nil && ctx.Err() == nil {
				c.logger.Debugf("Background RotateCookies failed: %v", err)
			}
		}
	}
}

func (c *Client) absorbResponseCookies(cookies []*http.Cookie) {
	if len(cookies) == 0 {
		return
	}

	now := time.Now()
	changed := false

	c.authMu.Lock()

	if c.tokenState.Cookies == nil {
		c.tokenState.Cookies = make(map[string]string)
	}

	for _, ck := range cookies {
		if isStaleCookie(ck, now) {
			continue
		}

		if c.tokenState.Cookies[ck.Name] != ck.Value {
			c.tokenState.Cookies[ck.Name] = ck.Value
			changed = true
		}

		if ck.Name == "COMPASS" {
			if csid := extractCSessionID(ck.Value); csid != "" && csid != c.tokenState.CSessionID {
				c.tokenState.CSessionID = csid
				changed = true
			}
		}
	}

	if changed {
		c.tokenState.UpdatedAt = now.UTC()
	}

	c.authMu.Unlock()

	if changed {
		c.notifyTokenUpdate(c.TokenState())
	}
}

func isStaleCookie(ck *http.Cookie, now time.Time) bool {
	if ck == nil || ck.Name == "" || ck.Value == "" {
		return true
	}

	// Ignore deletion/expired Set-Cookie directives so valid session tokens
	// are never overwritten with empty/expired markers.
	return ck.MaxAge < 0 || strings.EqualFold(ck.Value, "deleted") || (!ck.Expires.IsZero() && ck.Expires.Before(now))
}

func formatCookieHeader(cookies map[string]string, csessionid string) string {
	if len(cookies) == 0 && csessionid == "" {
		return ""
	}

	var b strings.Builder

	hasCompass := false

	for k, v := range cookies {
		if k == "" || v == "" {
			continue
		}

		if k == "COMPASS" {
			hasCompass = true
		}

		if b.Len() > 0 {
			b.WriteString("; ")
		}

		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
	}

	if !hasCompass && csessionid != "" {
		if b.Len() > 0 {
			b.WriteString("; ")
		}

		b.WriteString("COMPASS=dynamite=")
		b.WriteString(csessionid)
	}

	return b.String()
}

func extractCSessionID(compassVal string) string {
	val := strings.TrimSpace(compassVal)
	if after, ok := strings.CutPrefix(val, "dynamite="); ok {
		return after
	}

	if after, ok := strings.CutPrefix(val, "dynamite-ui="); ok {
		return after
	}

	return ""
}

func sapisidAuthHeader(sapisid, origin string, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	//nolint:gosec // G401: SHA-1 digest is defined by Google's SAPISIDHASH protocol.
	sum := sha1.Sum([]byte(ts + " " + sapisid + " " + origin))
	hash := ts + "_" + hex.EncodeToString(sum[:])

	return "SAPISIDHASH " + hash + " SAPISID1PHASH " + hash + " SAPISID3PHASH " + hash
}

func (c *Client) notifyTokenUpdate(state TokenState) {
	if c.OnTokenUpdate == nil {
		return
	}

	cp := state
	cp.Cookies = cloneStringMap(state.Cookies)
	c.OnTokenUpdate(cp)
}
