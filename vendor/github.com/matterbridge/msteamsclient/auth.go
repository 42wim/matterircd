package msteamsclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// DefaultPublicClientID is the Microsoft Teams public client ID supporting
	// OAuth2 device-code and refresh-token flows for Work/School (Entra ID) accounts.
	DefaultPublicClientID = "1fec8e78-bce4-4aaf-ab1b-5451cc387264"

	// DefaultPersonalClientID is the Microsoft Teams public client ID enabled
	// for Personal (MSA / Consumer Teams) accounts.
	DefaultPersonalClientID = "8ec6bc83-69c8-4392-8f08-b3c986009232"

	// msaTenantID is the fixed Microsoft Account (Personal/Consumer) tenant GUID
	// present in the "tid" claim of consumer Entra/MSA JWTs.
	msaTenantID = "9188040d-6c67-4c5b-b112-36a304b66dad"

	loginBaseURL        = "https://login.microsoftonline.com"
	workAuthsvcURL      = "https://teams.microsoft.com/api/authsvc/v1.0/authz"
	consumerAuthsvcURL  = "https://teams.live.com/api/auth/v1.0/authz/consumer"
	defaultRegion       = "amer"
	tokenRefreshMargin  = 2 * time.Minute
	minPollInterval     = 5 * time.Second
	defaultAuthTTL      = 86400 * time.Second
	maxAuthResponseSize = 1 << 20 // 1 MiB
	formKeyClientID     = "client_id"

	// ScopeSpacesWork and related OAuth2 scopes for Microsoft Teams backend services.
	ScopeSpacesWork     = "https://api.spaces.skype.com/.default offline_access openid profile"
	ScopeSpacesPersonal = "https://api.spaces.skype.com/user_impersonation offline_access openid profile"
	ScopeChatsvcAgg     = "https://chatsvcagg.teams.microsoft.com/.default offline_access"
	ScopePresence       = "https://presence.teams.microsoft.com/.default offline_access"
	ScopeSubstrate      = "https://substrate.office.com/.default offline_access"
	ScopeGraphWork      = "https://graph.microsoft.com/.default offline_access"
	ScopeGraphPersonal  = "User.Read offline_access openid profile"
)

type accessTokenEntry struct {
	token     string
	expiresAt time.Time
}

type oauthDeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	Message         string `json:"message"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

type oauthTokenResponse struct {
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	ExpiresIn    int    `json:"expires_in"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Error        string `json:"error"`
	ErrorDesc    string `json:"error_description"`
}

type authsvcResponse struct {
	Tokens struct {
		SkypeToken string `json:"skypeToken"`
		ExpiresIn  int    `json:"expiresIn"`
	} `json:"tokens"`
	Region     string            `json:"region"`
	RegionGtms map[string]string `json:"regionGtms"`
}

type jwtClaims struct {
	TenantID          string `json:"tid"`
	ObjectID          string `json:"oid"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	Email             string `json:"email"`
	SkypeID           string `json:"skypeid"`
	Region            string `json:"reg"`
	Exp               int64  `json:"exp"`
}

// Authenticate establishes a valid Teams session (skypetoken + regional
// endpoints) using whichever credentials were supplied: saved TokenState,
// direct skypetoken, refresh token, or interactive OAuth2 Device Code flow.
func (c *Client) Authenticate(ctx context.Context) error {
	c.loadInitialTokenState()

	if c.hasUsableSkypeToken() {
		c.logger.Debug("Using cached skypetoken")

		return nil
	}

	if rawToken := strings.TrimSpace(c.cred.SkypeToken); rawToken != "" && c.currentRefreshToken() == "" {
		return c.initFromSkypeToken(rawToken)
	}

	c.authMu.RLock()
	savedSkypeToken := c.tokenState.SkypeToken
	c.authMu.RUnlock()

	if savedSkypeToken != "" && c.currentRefreshToken() == "" {
		return c.initFromSkypeToken(savedSkypeToken)
	}

	if c.currentRefreshToken() != "" {
		err := c.refreshSession(ctx)
		if err == nil {
			return nil
		}

		c.logger.Warnf("Refresh token login failed, falling back if interactive: %v", err)

		if c.OnAuthPrompt == nil {
			return err
		}
	}

	return c.authenticateDeviceCode(ctx)
}

// EnsureSkypeToken returns a valid skypetoken, refreshing it single-flight
// when near expiry.
func (c *Client) EnsureSkypeToken(ctx context.Context) (string, error) {
	c.authMu.RLock()
	token := c.tokenState.SkypeToken
	expiry := c.tokenState.SkypeTokenExpiry
	c.authMu.RUnlock()

	if token != "" && (expiry.IsZero() || time.Now().Add(tokenRefreshMargin).Before(expiry)) {
		return token, nil
	}

	c.reauthMu.Lock()
	defer c.reauthMu.Unlock()

	// Re-check under reauthMu in case another goroutine already refreshed.
	c.authMu.RLock()
	token = c.tokenState.SkypeToken
	expiry = c.tokenState.SkypeTokenExpiry
	refresh := c.tokenState.RefreshToken
	c.authMu.RUnlock()

	if token != "" && (expiry.IsZero() || time.Now().Add(tokenRefreshMargin).Before(expiry)) {
		return token, nil
	}

	if refresh == "" {
		if token != "" {
			return token, nil
		}

		return "", errors.New("msteamsclient: skypetoken expired and no refresh token is available")
	}

	err := c.refreshSession(ctx)
	if err != nil {
		return "", err
	}

	c.authMu.RLock()
	token = c.tokenState.SkypeToken
	c.authMu.RUnlock()

	return token, nil
}

// ForceRefreshSkypeToken unconditionally re-exchanges the refresh token for a
// fresh skypetoken after a 401 Unauthorized response.
func (c *Client) ForceRefreshSkypeToken(ctx context.Context) (string, error) {
	c.reauthMu.Lock()
	defer c.reauthMu.Unlock()

	if c.currentRefreshToken() == "" {
		return "", errors.New("msteamsclient: cannot refresh expired skypetoken without a refresh token")
	}

	err := c.refreshSession(ctx)
	if err != nil {
		return "", err
	}

	c.authMu.RLock()
	token := c.tokenState.SkypeToken
	c.authMu.RUnlock()

	return token, nil
}

// AccessToken returns a cached or freshly minted OAuth2 bearer access token
// for the requested resource scope.
func (c *Client) AccessToken(ctx context.Context, scope string) (string, error) {
	c.authMu.RLock()
	entry, ok := c.scopeTokens[scope]
	c.authMu.RUnlock()

	if ok && entry.token != "" && time.Now().Add(tokenRefreshMargin).Before(entry.expiresAt) {
		return entry.token, nil
	}

	c.reauthMu.Lock()
	defer c.reauthMu.Unlock()

	c.authMu.RLock()
	entry, ok = c.scopeTokens[scope]
	refresh := c.tokenState.RefreshToken
	c.authMu.RUnlock()

	if ok && entry.token != "" && time.Now().Add(tokenRefreshMargin).Before(entry.expiresAt) {
		return entry.token, nil
	}

	if refresh == "" {
		return "", fmt.Errorf("msteamsclient: no refresh token available to acquire scope %q", scope)
	}

	resp, err := c.exchangeRefreshToken(ctx, refresh, scope)
	if err != nil {
		return "", err
	}

	c.storeOAuthResponse(scope, resp)

	return resp.AccessToken, nil
}

func (c *Client) loadInitialTokenState() {
	c.authMu.Lock()
	defer c.authMu.Unlock()

	if c.cred.TokenState != nil {
		c.tokenState = *c.cred.TokenState
	}

	if c.cred.AccountType != "" && c.cred.AccountType != AccountAuto {
		c.tokenState.AccountType = c.cred.AccountType
	} else if c.tokenState.AccountType == "" {
		c.tokenState.AccountType = AccountAuto
	}

	if c.cred.TenantID != "" {
		c.tokenState.TenantID = c.cred.TenantID
	}

	if c.cred.ClientID != "" {
		c.tokenState.ClientID = c.cred.ClientID
	}

	if c.cred.RefreshToken != "" {
		c.tokenState.RefreshToken = strings.TrimSpace(c.cred.RefreshToken)
	}

	if c.cred.SkypeToken != "" {
		c.tokenState.SkypeToken = stripSkypeTokenPrefix(c.cred.SkypeToken)
	}

	if c.cred.Region != "" {
		c.tokenState.Region = c.cred.Region
	}

	if c.cred.ChatServiceURL != "" {
		c.tokenState.Endpoints.ChatService = strings.TrimRight(c.cred.ChatServiceURL, "/")
	}

	switch c.tokenState.AccountType {
	case AccountPersonal:
		if strings.HasPrefix(c.tokenState.Endpoints.ChatService, "https://teams.live.com/api/chatsvc/") {
			c.tokenState.Endpoints.ChatService = "https://msgapi.teams.live.com"
		}

		if c.tokenState.Endpoints.Trouter == "https://go.trouter.teams.live.com" {
			c.tokenState.Endpoints.Trouter = "https://go.trouter.skype.com"
		}
	case AccountWork:
		c.migrateWorkEndpointsLocked()
	}

	c.syncUserFromStateLocked()
}

func (c *Client) migrateWorkEndpointsLocked() {
	reg := c.tokenState.Region
	if reg == "" {
		reg = defaultRegion
	}

	if strings.Contains(c.tokenState.Endpoints.ChatsvcAgg, "/templates/api") {
		c.tokenState.Endpoints.ChatsvcAgg = "https://teams.microsoft.com/api/csa/" + reg
	}

	if mt := c.tokenState.Endpoints.MiddleTier; mt != "" && !strings.HasSuffix(mt, "/beta") && !strings.HasSuffix(mt, "/v1") {
		c.tokenState.Endpoints.MiddleTier = mt + "/beta"
	}
}

func (c *Client) hasUsableSkypeToken() bool {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	if c.tokenState.SkypeToken == "" || c.tokenState.Endpoints.ChatService == "" {
		return false
	}

	if c.tokenState.SkypeTokenExpiry.IsZero() {
		return true
	}

	return time.Now().Add(tokenRefreshMargin).Before(c.tokenState.SkypeTokenExpiry)
}

func (c *Client) currentRefreshToken() string {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.tokenState.RefreshToken
}

func (c *Client) effectiveAccountType() AccountType {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	if c.tokenState.AccountType != "" {
		return c.tokenState.AccountType
	}

	return AccountAuto
}

func (c *Client) effectiveClientID() string {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	if c.tokenState.ClientID != "" {
		return c.tokenState.ClientID
	}

	if c.tokenState.AccountType == AccountPersonal {
		return DefaultPersonalClientID
	}

	return DefaultPublicClientID
}

func (c *Client) oauthAuthority() string {
	c.authMu.RLock()
	acctType := c.tokenState.AccountType
	tenantID := c.tokenState.TenantID
	c.authMu.RUnlock()

	switch {
	case tenantID != "":
		return loginBaseURL + "/" + url.PathEscape(tenantID) + "/oauth2/v2.0"
	case acctType == AccountPersonal:
		return loginBaseURL + "/consumers/oauth2/v2.0"
	case acctType == AccountWork:
		return loginBaseURL + "/organizations/oauth2/v2.0"
	default:
		return loginBaseURL + "/common/oauth2/v2.0"
	}
}

func (c *Client) primarySpacesScope() string {
	if c.effectiveAccountType() == AccountPersonal {
		return ScopeSpacesPersonal
	}

	return ScopeSpacesWork
}

func (c *Client) authenticateDeviceCode(ctx context.Context) error {
	scope := c.primarySpacesScope()

	dcResp, err := c.startDeviceCode(ctx, scope)
	if err != nil && scope == ScopeSpacesPersonal {
		// Retry with .default if the consumer authority rejects user_impersonation.
		scope = ScopeSpacesWork

		dcResp, err = c.startDeviceCode(ctx, scope)
	}

	if err != nil {
		return err
	}

	if c.OnAuthPrompt != nil {
		c.OnAuthPrompt(AuthPrompt{
			VerificationURI: dcResp.VerificationURI,
			UserCode:        dcResp.UserCode,
			Message:         dcResp.Message,
			ExpiresIn:       dcResp.ExpiresIn,
		})
	} else {
		c.logger.Infof("Microsoft Teams login required: open %s and enter code %s", dcResp.VerificationURI, dcResp.UserCode)
	}

	tokResp, err := c.pollDeviceCode(ctx, dcResp)
	if err != nil {
		return err
	}

	c.storeOAuthResponse(scope, tokResp)

	return c.exchangeSkypeToken(ctx, tokResp.AccessToken)
}

func (c *Client) startDeviceCode(ctx context.Context, scope string) (*oauthDeviceCodeResponse, error) {
	form := url.Values{
		formKeyClientID: {c.effectiveClientID()},
		"scope":         {scope},
	}

	endpoint := c.oauthAuthority() + "/devicecode"

	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: device code request failed: %w", err)
	}

	var dcResp oauthDeviceCodeResponse

	err = json.Unmarshal(body, &dcResp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: decode device code response (status %d): %w", status, err)
	}

	if status >= http.StatusBadRequest || dcResp.Error != "" {
		return nil, fmt.Errorf("msteamsclient: device code error %s: %s", dcResp.Error, dcResp.ErrorDesc)
	}

	if dcResp.DeviceCode == "" || dcResp.UserCode == "" {
		return nil, errors.New("msteamsclient: incomplete device code response from Microsoft")
	}

	return &dcResp, nil
}

func (c *Client) pollDeviceCode(ctx context.Context, dc *oauthDeviceCodeResponse) (*oauthTokenResponse, error) {
	interval := max(time.Duration(dc.Interval)*time.Second, minPollInterval)

	expiresIn := dc.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 900
	}

	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	endpoint := c.oauthAuthority() + "/token"

	form := url.Values{
		"grant_type":    {"urn:ietf:params:oauth:grant-type:device_code"},
		formKeyClientID: {c.effectiveClientID()},
		"device_code":   {dc.DeviceCode},
	}

	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}

		if time.Now().After(deadline) {
			return nil, errors.New("msteamsclient: device code login timed out")
		}

		tokResp, slowDown, err := c.pollDeviceCodeOnce(ctx, endpoint, form)
		if err != nil {
			return nil, err
		}

		if tokResp != nil {
			return tokResp, nil
		}

		if slowDown {
			interval += 5 * time.Second
		}

		timer.Reset(interval)
	}
}

func (c *Client) pollDeviceCodeOnce(ctx context.Context, endpoint string, form url.Values) (*oauthTokenResponse, bool, error) {
	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return nil, false, fmt.Errorf("msteamsclient: device code poll failed: %w", err)
	}

	var tokResp oauthTokenResponse

	err = json.Unmarshal(body, &tokResp)
	if err != nil {
		return nil, false, fmt.Errorf("msteamsclient: decode token response (status %d): %w", status, err)
	}

	switch tokResp.Error {
	case "":
		if tokResp.AccessToken == "" {
			return nil, false, errors.New("msteamsclient: empty access token returned from Microsoft")
		}

		return &tokResp, false, nil
	case "authorization_pending":
		return nil, false, nil
	case "slow_down":
		return nil, true, nil
	case "expired_token":
		return nil, false, errors.New("msteamsclient: device code expired before sign-in completed")
	case "authorization_declined":
		return nil, false, errors.New("msteamsclient: sign-in was declined by user")
	default:
		return nil, false, fmt.Errorf("msteamsclient: oauth token error %s: %s", tokResp.Error, tokResp.ErrorDesc)
	}
}

func (c *Client) refreshSession(ctx context.Context) error {
	refresh := c.currentRefreshToken()
	if refresh == "" {
		return errors.New("msteamsclient: refresh token is empty")
	}

	scope := c.primarySpacesScope()

	tokResp, err := c.exchangeRefreshToken(ctx, refresh, scope)
	if err != nil && scope == ScopeSpacesPersonal {
		scope = ScopeSpacesWork

		tokResp, err = c.exchangeRefreshToken(ctx, refresh, scope)
	}

	if err != nil {
		return err
	}

	c.storeOAuthResponse(scope, tokResp)

	return c.exchangeSkypeToken(ctx, tokResp.AccessToken)
}

func (c *Client) exchangeRefreshToken(ctx context.Context, refreshToken, scope string) (*oauthTokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		formKeyClientID: {c.effectiveClientID()},
		"refresh_token": {refreshToken},
		"scope":         {scope},
	}

	endpoint := c.oauthAuthority() + "/token"

	body, status, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: refresh token request failed: %w", err)
	}

	var tokResp oauthTokenResponse

	err = json.Unmarshal(body, &tokResp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: decode refresh token response (status %d): %w", status, err)
	}

	if status >= http.StatusBadRequest || tokResp.Error != "" {
		return nil, fmt.Errorf("msteamsclient: refresh token error %s: %s", tokResp.Error, tokResp.ErrorDesc)
	}

	if tokResp.AccessToken == "" {
		return nil, errors.New("msteamsclient: refresh returned an empty access token")
	}

	return &tokResp, nil
}

func (c *Client) storeOAuthResponse(scope string, resp *oauthTokenResponse) {
	ttl := time.Duration(resp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}

	claims := parseJWTClaims(resp.AccessToken)
	if resp.IDToken != "" {
		idClaims := parseJWTClaims(resp.IDToken)
		mergeJWTClaims(&claims, idClaims)
	}

	c.authMu.Lock()

	c.scopeTokens[scope] = accessTokenEntry{
		token:     resp.AccessToken,
		expiresAt: time.Now().Add(ttl),
	}

	rotated := false

	if resp.RefreshToken != "" && resp.RefreshToken != c.tokenState.RefreshToken {
		c.tokenState.RefreshToken = resp.RefreshToken
		rotated = true
	}

	c.applyJWTClaimsLocked(claims)
	stateCopy := c.tokenState

	c.authMu.Unlock()

	if rotated {
		c.notifyTokenUpdate(stateCopy)
	}
}

func (c *Client) exchangeSkypeToken(ctx context.Context, spacesToken string) error {
	acctType := c.effectiveAccountType()

	switch acctType {
	case AccountPersonal:
		return c.exchangeConsumerSkypeToken(ctx, spacesToken)
	case AccountWork:
		return c.exchangeWorkSkypeToken(ctx, spacesToken)
	default:
		err := c.exchangeWorkSkypeToken(ctx, spacesToken)
		if err == nil {
			return nil
		}

		c.logger.Debugf("Work authsvc failed under auto account detection (%v); trying consumer authz", err)

		consumerErr := c.exchangeConsumerSkypeToken(ctx, spacesToken)
		if consumerErr != nil {
			return fmt.Errorf("msteamsclient: authsvc failed (work: %v; personal: %w)", err, consumerErr)
		}

		return nil
	}
}

func (c *Client) exchangeWorkSkypeToken(ctx context.Context, spacesToken string) error {
	resp, err := c.postAuthsvc(ctx, workAuthsvcURL, spacesToken)
	if err != nil {
		return err
	}

	c.applyAuthsvcResult(AccountWork, resp)

	return nil
}

func (c *Client) exchangeConsumerSkypeToken(ctx context.Context, spacesToken string) error {
	resp, err := c.postAuthsvc(ctx, consumerAuthsvcURL, spacesToken)
	if err != nil {
		return err
	}

	c.applyAuthsvcResult(AccountPersonal, resp)

	return nil
}

func (c *Client) postAuthsvc(ctx context.Context, endpoint, bearerToken string) (*authsvcResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: authsvc request failed: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseSize))
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: read authsvc response: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("msteamsclient: authsvc returned HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	var parsed authsvcResponse

	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: decode authsvc response: %w", err)
	}

	if parsed.Tokens.SkypeToken == "" {
		return nil, errors.New("msteamsclient: authsvc response did not contain a skypetoken")
	}

	return &parsed, nil
}

func (c *Client) applyAuthsvcResult(acctType AccountType, resp *authsvcResponse) {
	skypeClaims := parseJWTClaims(resp.Tokens.SkypeToken)

	expiry := time.Now().Add(defaultAuthTTL)
	if resp.Tokens.ExpiresIn > 0 {
		expiry = time.Now().Add(time.Duration(resp.Tokens.ExpiresIn) * time.Second)
	} else if skypeClaims.Exp > 0 {
		expiry = time.Unix(skypeClaims.Exp, 0)
	}

	endpoints := buildRegionEndpoints(acctType, resp.Region, resp.RegionGtms)

	c.authMu.Lock()

	c.tokenState.AccountType = acctType
	c.tokenState.SkypeToken = resp.Tokens.SkypeToken
	c.tokenState.SkypeTokenExpiry = expiry
	c.tokenState.Region = endpoints.Region
	c.tokenState.Endpoints = endpoints

	if c.cred.ChatServiceURL != "" {
		c.tokenState.Endpoints.ChatService = strings.TrimRight(c.cred.ChatServiceURL, "/")
	}

	c.applyJWTClaimsLocked(skypeClaims)
	c.syncUserFromStateLocked()
	stateCopy := c.tokenState

	c.authMu.Unlock()

	c.notifyTokenUpdate(stateCopy)
}

func (c *Client) initFromSkypeToken(rawToken string) error {
	token := stripSkypeTokenPrefix(rawToken)
	if token == "" {
		return errors.New("msteamsclient: skypetoken is empty")
	}

	claims := parseJWTClaims(token)

	acctType := c.effectiveAccountType()
	if acctType == AccountAuto {
		switch {
		case strings.HasPrefix(claims.SkypeID, "live:"):
			acctType = AccountPersonal
		case strings.HasPrefix(claims.SkypeID, "orgid:"):
			acctType = AccountWork
		default:
			acctType = AccountWork
		}
	}

	region := c.cred.Region
	if region == "" {
		region = claims.Region
	}

	endpoints := buildRegionEndpoints(acctType, region, nil)
	if c.cred.ChatServiceURL != "" {
		endpoints.ChatService = strings.TrimRight(c.cred.ChatServiceURL, "/")
	}

	var expiry time.Time
	if claims.Exp > 0 {
		expiry = time.Unix(claims.Exp, 0)
		if time.Now().After(expiry) {
			return fmt.Errorf("msteamsclient: provided skypetoken expired at %s", expiry.Format(time.RFC3339))
		}
	}

	c.authMu.Lock()

	c.tokenState.AccountType = acctType
	c.tokenState.SkypeToken = token
	c.tokenState.SkypeTokenExpiry = expiry
	c.tokenState.Region = endpoints.Region
	c.tokenState.Endpoints = endpoints

	c.applyJWTClaimsLocked(claims)
	c.syncUserFromStateLocked()
	stateCopy := c.tokenState

	c.authMu.Unlock()

	c.notifyTokenUpdate(stateCopy)

	return nil
}

func (c *Client) applyJWTClaimsLocked(claims jwtClaims) {
	if claims.TenantID != "" {
		c.tokenState.TenantID = claims.TenantID

		if c.tokenState.AccountType == "" || c.tokenState.AccountType == AccountAuto {
			if strings.EqualFold(claims.TenantID, msaTenantID) {
				c.tokenState.AccountType = AccountPersonal
			} else {
				c.tokenState.AccountType = AccountWork
			}
		}
	}

	switch {
	case claims.SkypeID != "":
		if strings.HasPrefix(claims.SkypeID, "8:") {
			c.tokenState.UserMRI = claims.SkypeID
		} else {
			c.tokenState.UserMRI = "8:" + claims.SkypeID
		}
	case claims.ObjectID != "" && c.tokenState.UserMRI == "":
		if c.tokenState.AccountType == AccountPersonal {
			c.tokenState.UserMRI = "8:live:" + claims.ObjectID
		} else {
			c.tokenState.UserMRI = "8:orgid:" + claims.ObjectID
		}
	}

	if claims.Name != "" {
		c.tokenState.DisplayName = claims.Name
	}

	switch {
	case claims.Email != "":
		c.tokenState.Email = claims.Email
	case claims.UPN != "":
		c.tokenState.Email = claims.UPN
	case claims.PreferredUsername != "" && c.tokenState.Email == "":
		c.tokenState.Email = claims.PreferredUsername
	}
}

func (c *Client) syncUserFromStateLocked() {
	if c.tokenState.UserMRI == "" {
		return
	}

	user := &User{
		MRI:         c.tokenState.UserMRI,
		DisplayName: c.tokenState.DisplayName,
		Email:       c.tokenState.Email,
		UPN:         c.tokenState.Email,
		TenantID:    c.tokenState.TenantID,
	}

	if _, after, ok := strings.Cut(c.tokenState.UserMRI, "8:orgid:"); ok {
		user.ObjectID = after
	}

	c.Lock()
	c.User = user
	c.Unlock()
}

func (c *Client) notifyTokenUpdate(state TokenState) {
	if c.OnTokenUpdate != nil {
		c.OnTokenUpdate(state)
	}
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseSize))
	if err != nil {
		return nil, resp.StatusCode, err
	}

	return body, resp.StatusCode, nil
}

func buildRegionEndpoints(acctType AccountType, region string, gtms map[string]string) RegionEndpoints {
	reg := strings.ToLower(strings.TrimSpace(region))
	if reg == "" {
		reg = strings.ToLower(strings.TrimSpace(lookupGTM(gtms, "region")))
	}

	if reg == "" {
		reg = defaultRegion
	}

	ep := RegionEndpoints{
		Region:         reg,
		ChatService:    trimURL(lookupGTM(gtms, "chatService", "chatservice")),
		ChatServiceAfd: trimURL(lookupGTM(gtms, "chatServiceAfd", "chatserviceafd")),
		ChatsvcAgg:     trimURL(lookupGTM(gtms, "chatsvcAgg", "chatsvcagg")),
		MiddleTier:     trimURL(lookupGTM(gtms, "middleTier", "middletier")),
		Trouter:        trimURL(lookupGTM(gtms, "trouter", "trouterurl", "trouterRegistrar")),
		Presence:       trimURL(lookupGTM(gtms, "presence", "unifiedPresence")),
		AMS:            trimURL(lookupGTM(gtms, "ams", "amsv2", "amdS2S")),
		Substrate:      trimURL(lookupGTM(gtms, "substrate")),
	}

	if acctType == AccountPersonal {
		applyConsumerEndpointDefaults(&ep, reg)
	} else {
		applyWorkEndpointDefaults(&ep, reg)
	}

	return ep
}

func applyWorkEndpointDefaults(ep *RegionEndpoints, reg string) {
	if ep.ChatService == "" {
		ep.ChatService = "https://" + reg + ".ng.msg.teams.microsoft.com"
	}

	if ep.ChatsvcAgg == "" {
		ep.ChatsvcAgg = "https://teams.microsoft.com/api/csa/" + reg
	}

	if ep.MiddleTier == "" {
		ep.MiddleTier = "https://teams.microsoft.com/api/mt/part/" + reg + "-01/beta"
	} else if !strings.HasSuffix(ep.MiddleTier, "/beta") && !strings.HasSuffix(ep.MiddleTier, "/v1") {
		ep.MiddleTier += "/beta"
	}

	if ep.Trouter == "" {
		ep.Trouter = "https://go.trouter.teams.microsoft.com"
	}

	if ep.Presence == "" {
		ep.Presence = "https://presence.teams.microsoft.com"
	}

	if ep.AMS == "" {
		ep.AMS = "https://us-api.asm.skype.com"
	}

	if ep.Substrate == "" {
		ep.Substrate = "https://substrate.office.com"
	}
}

func applyConsumerEndpointDefaults(ep *RegionEndpoints, reg string) {
	if ep.ChatService == "" {
		ep.ChatService = "https://msgapi.teams.live.com"
	}

	if ep.ChatsvcAgg == "" {
		ep.ChatsvcAgg = "https://teams.live.com/api/csa/" + reg
	}

	if ep.MiddleTier == "" {
		ep.MiddleTier = "https://teams.live.com/api/mt/beta"
	}

	if ep.Trouter == "" {
		ep.Trouter = "https://go.trouter.skype.com"
	}

	if ep.Presence == "" {
		ep.Presence = "https://presence.teams.live.com"
	}

	if ep.AMS == "" {
		ep.AMS = "https://api.asm.skype.com"
	}

	if ep.Substrate == "" {
		ep.Substrate = "https://substrate.office.com"
	}
}

func lookupGTM(gtms map[string]string, keys ...string) string {
	if len(gtms) == 0 {
		return ""
	}

	for _, want := range keys {
		if v, ok := gtms[want]; ok && v != "" {
			return v
		}
	}

	for _, want := range keys {
		for k, v := range gtms {
			if strings.EqualFold(k, want) && v != "" {
				return v
			}
		}
	}

	return ""
}

func trimURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func stripSkypeTokenPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "skypetoken=")
	s = strings.TrimPrefix(s, "skypetoken_asm=")

	return strings.TrimSpace(s)
}

func parseJWTClaims(token string) jwtClaims {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return jwtClaims{}
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwtClaims{}
	}

	var claims jwtClaims

	err = json.Unmarshal(payload, &claims)
	if err != nil {
		return jwtClaims{}
	}

	return claims
}

func mergeJWTClaims(dst *jwtClaims, src jwtClaims) {
	if dst.TenantID == "" {
		dst.TenantID = src.TenantID
	}

	if dst.ObjectID == "" {
		dst.ObjectID = src.ObjectID
	}

	if dst.Name == "" {
		dst.Name = src.Name
	}

	if dst.PreferredUsername == "" {
		dst.PreferredUsername = src.PreferredUsername
	}

	if dst.UPN == "" {
		dst.UPN = src.UPN
	}

	if dst.Email == "" {
		dst.Email = src.Email
	}
}

func truncateBody(body []byte) string {
	const maxLen = 256

	s := strings.TrimSpace(string(body))
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}

	return s
}
