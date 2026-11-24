package msteamsclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	profileBatchSize         = 50
	presenceBatchMax         = 100
	presenceCacheTTL         = 60 * time.Second
	freshPresenceMinInterval = 30 * time.Second

	statusOnline  = "online"
	statusAway    = "away"
	statusDND     = "dnd"
	statusOffline = "offline"
)

type rawShortProfileResponse struct {
	Value []rawUserProfile `json:"value"`
}

type rawUserProfile struct {
	MRI               string `json:"mri"`
	ObjectID          string `json:"objectId"`
	DisplayName       string `json:"displayName"`
	GivenName         string `json:"givenName"`
	Surname           string `json:"surname"`
	Email             string `json:"email"`
	UserPrincipalName string `json:"userPrincipalName"`
	JobTitle          string `json:"jobTitle"`
	Department        string `json:"department"`
	OfficeLocation    string `json:"officeLocation"`
	City              string `json:"city"`
	Country           string `json:"country"`
	TimeZone          string `json:"timeZone"`
	TenantName        string `json:"tenantName"`
	Type              string `json:"type"`
	IsExternal        bool   `json:"isExternal"`
}

type rawPresenceEntry struct {
	MRI      string `json:"mri"`
	Presence struct {
		Availability   string `json:"availability"`
		Activity       string `json:"activity"`
		LastActiveTime string `json:"lastActiveTime"`
		Note           struct {
			Message string `json:"message"`
			Expiry  string `json:"expiry"`
		} `json:"note"`
		StatusMessage struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			ExpiryDateTime struct {
				DateTime string `json:"dateTime"`
			} `json:"expiryDateTime"`
		} `json:"statusMessage"`
	} `json:"presence"`
}

func (e *rawPresenceEntry) statusNote() (string, int64) {
	var note string
	if e.Presence.Note.Message != "" {
		note = HTMLToText(e.Presence.Note.Message)
	} else {
		note = HTMLToText(e.Presence.StatusMessage.Message.Content)
	}

	if note == "" {
		return "", 0
	}

	exp := parsePresenceExpiry(e.Presence.Note.Expiry)
	if exp == 0 {
		exp = parsePresenceExpiry(e.Presence.StatusMessage.ExpiryDateTime.DateTime)
	}

	if exp > 0 && time.Now().Unix() >= exp {
		return "", 0
	}

	return note, exp
}

func parsePresenceExpiry(raw string) int64 {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05"} {
		t, err := time.Parse(layout, s)
		if err == nil {
			// Teams uses year 9999 as a sentinel for "never expires".
			if t.Year() <= 1970 || t.Year() >= 9999 {
				return 0
			}

			return t.Unix()
		}
	}

	return 0
}

func (e *rawPresenceEntry) lastActiveAt() int64 {
	raw := strings.TrimSpace(e.Presence.LastActiveTime)
	if raw == "" {
		return 0
	}

	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		t, err := time.Parse(layout, raw)
		if err == nil && t.Year() > 1970 {
			return t.Unix()
		}
	}

	return 0
}

// CacheUser merges u into UsersCache without overwriting richer fields with empty ones.
func (c *Client) CacheUser(u *User) {
	if u == nil || u.MRI == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	existing, ok := c.Users.users[u.MRI]
	if !ok {
		cp := *u
		c.Users.users[u.MRI] = &cp

		return
	}

	mergeUserFields(existing, u)
}

func mergeUserFields(dst, src *User) {
	if src.ObjectID != "" {
		dst.ObjectID = src.ObjectID
	}

	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}

	if src.GivenName != "" {
		dst.GivenName = src.GivenName
	}

	if src.Surname != "" {
		dst.Surname = src.Surname
	}

	if src.Email != "" {
		dst.Email = src.Email
	}

	if src.UPN != "" {
		dst.UPN = src.UPN
	}

	if src.JobTitle != "" {
		dst.JobTitle = src.JobTitle
	}

	if src.Department != "" {
		dst.Department = src.Department
	}

	if src.Location != "" {
		dst.Location = src.Location
	}

	if src.TimeZone != "" {
		dst.TimeZone = src.TimeZone
	}

	if src.TenantID != "" {
		dst.TenantID = src.TenantID
	}

	if src.IsBot {
		dst.IsBot = true
	}

	if src.IsExternal {
		dst.IsExternal = true
	}
}

// GetUser returns a cached copy of the user with MRI, or nil.
func (c *Client) GetUser(mri string) *User {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	u, ok := c.Users.users[mri]
	if !ok {
		return nil
	}

	cp := *u

	return &cp
}

// GetUsers returns copies of all cached users.
func (c *Client) GetUsers() []*User {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make([]*User, 0, len(c.Users.users))
	for _, u := range c.Users.users {
		cp := *u
		out = append(out, &cp)
	}

	return out
}

// GetUserCount returns the number of cached users.
func (c *Client) GetUserCount() int {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return len(c.Users.users)
}

// GetUserByEmail looks up a cached user by email or UPN (case-insensitive).
func (c *Client) GetUserByEmail(email string) *User {
	if email == "" {
		return nil
	}

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, u := range c.Users.users {
		if strings.EqualFold(u.Email, email) || strings.EqualFold(u.UPN, email) {
			cp := *u

			return &cp
		}
	}

	return nil
}

// ResolveUsers ensures profiles for mris are cached by batch-fetching missing
// entries from Middle-Tier fetchShortProfile.
func (c *Client) ResolveUsers(ctx context.Context, mris []string) (map[string]*User, error) {
	result, missing := c.cachedUsersAndMissing(mris)
	if len(missing) == 0 {
		return result, nil
	}

	ep := c.Endpoints()
	if ep.MiddleTier == "" {
		return result, nil
	}

	if c.effectiveAccountType() == AccountPersonal && c.currentRefreshToken() == "" {
		return result, nil
	}

	profileURL := ep.MiddleTier + "/users/fetchShortProfile?isMailAddress=false&enableGuest=true&includeIBBarredUsers=true&skypeTeamsInfo=true"
	scope := c.middleTierScope()

	for start := 0; start < len(missing); start += profileBatchSize {
		end := min(start+profileBatchSize, len(missing))
		chunk := missing[start:end]

		var resp rawShortProfileResponse

		err := c.doJSON(ctx, http.MethodPost, profileURL, authSkypeAndBearer, scope, chunk, &resp)
		if err != nil {
			c.logger.Debugf("fetchShortProfile failed for %d users: %v", len(chunk), err)

			continue
		}

		for i := range resp.Value {
			u := parseRawUserProfile(&resp.Value[i])
			if u == nil {
				continue
			}

			c.CacheUser(u)
			result[u.MRI] = u
		}
	}

	return result, nil
}

func (c *Client) cachedUsersAndMissing(mris []string) (map[string]*User, []string) {
	result := make(map[string]*User, len(mris))
	missing := make([]string, 0)
	isWork := c.effectiveAccountType() != AccountPersonal

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, mri := range mris {
		if mri == "" {
			continue
		}

		if u, ok := c.Users.users[mri]; ok && u.DisplayName != "" {
			cp := *u
			result[mri] = &cp

			continue
		}

		// Work/School Middle-Tier fetchShortProfile rejects consumer MSA MRIs
		// (8:live:...) with HTTP 400 InvalidUserId, which fails the entire batch.
		if isWork && strings.HasPrefix(mri, "8:live:") {
			continue
		}

		if !slices.Contains(missing, mri) {
			missing = append(missing, mri)
		}
	}

	return result, missing
}

// SearchUsers searches the Middle-Tier searchV2 endpoint and supplements with
// matches from UsersCache.
func (c *Client) SearchUsers(ctx context.Context, query string, limit int) ([]*User, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	if limit <= 0 {
		limit = 50
	}

	seen := make(map[string]struct{})
	out := c.searchUsersMiddleTier(ctx, query, limit, seen)

	if len(out) >= limit {
		return out, nil
	}

	return c.appendCachedUserMatches(out, seen, strings.ToLower(query), limit), nil
}

func (c *Client) searchUsersMiddleTier(ctx context.Context, query string, limit int, seen map[string]struct{}) []*User {
	ep := c.Endpoints()
	if ep.MiddleTier == "" {
		return nil
	}

	searchURL := ep.MiddleTier + "/users/searchV2?includeDLs=true&includeBots=true&enableGuest=true&source=newChat&skypeTeamsInfo=true"

	var resp rawShortProfileResponse

	err := c.doJSON(ctx, http.MethodPost, searchURL, authSkypeAndBearer, c.middleTierScope(), query, &resp)
	if err != nil {
		c.logger.Debugf("Middle-Tier user search failed (%v); falling back to cached directory", err)

		return nil
	}

	out := make([]*User, 0, min(len(resp.Value), limit))

	for i := range resp.Value {
		u := parseRawUserProfile(&resp.Value[i])
		if u == nil {
			continue
		}

		c.CacheUser(u)

		if _, dup := seen[u.MRI]; !dup {
			seen[u.MRI] = struct{}{}
			out = append(out, u)
		}

		if len(out) >= limit {
			break
		}
	}

	return out
}

func (c *Client) appendCachedUserMatches(out []*User, seen map[string]struct{}, lower string, limit int) []*User {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, u := range c.Users.users {
		if _, dup := seen[u.MRI]; dup {
			continue
		}

		if strings.Contains(strings.ToLower(u.DisplayName), lower) ||
			strings.Contains(strings.ToLower(u.Email), lower) ||
			strings.Contains(strings.ToLower(u.UPN), lower) {
			cp := *u
			seen[u.MRI] = struct{}{}

			out = append(out, &cp)
			if len(out) >= limit {
				break
			}
		}
	}

	return out
}

// GetPresence returns the presence for a single user MRI, using the TTL cache.
func (c *Client) GetPresence(ctx context.Context, mri string) (*Presence, error) {
	res, err := c.GetPresences(ctx, []string{mri})
	if err != nil {
		return nil, err
	}

	return res[mri], nil
}

// GetPresenceFresh fetches the current presence for mri from the Teams
// presence service (rate-limited per user to freshPresenceMinInterval,
// mirroring matterclient.GetStatusFresh), falling back to any cached entry on
// error.
func (c *Client) GetPresenceFresh(ctx context.Context, mri string) *Presence {
	if mri == "" {
		return nil
	}

	now := time.Now().Unix()

	c.Users.mu.RLock()

	cached, hasCached := c.Users.presences[mri]
	if hasCached && now-cached.UpdatedAt < int64(freshPresenceMinInterval.Seconds()) {
		cp := *cached

		c.Users.mu.RUnlock()

		return &cp
	}

	c.Users.mu.RUnlock()

	ep := c.Endpoints()
	if ep.Presence != "" {
		result := make(map[string]*Presence, 1)
		c.fetchPresenceBatch(ctx, ep.Presence+"/v1/presence/getpresence/", []string{mri}, now, result)

		if p, ok := result[mri]; ok {
			return p
		}
	}

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	if p, ok := c.Users.presences[mri]; ok {
		cp := *p

		return &cp
	}

	return nil
}

// GetPresences returns presences for mris, batch-fetching expired entries from
// the Teams presence service.
func (c *Client) GetPresences(ctx context.Context, mris []string) (map[string]*Presence, error) {
	now := time.Now().Unix()
	result, stale := c.cachedPresencesAndStale(mris, now)

	if len(stale) == 0 {
		return result, nil
	}

	ep := c.Endpoints()
	if ep.Presence == "" {
		return result, nil
	}

	presenceURL := ep.Presence + "/v1/presence/getpresence/"

	for start := 0; start < len(stale); start += presenceBatchMax {
		end := min(start+presenceBatchMax, len(stale))
		c.fetchPresenceBatch(ctx, presenceURL, stale[start:end], now, result)
	}

	return result, nil
}

func (c *Client) cachedPresencesAndStale(mris []string, now int64) (map[string]*Presence, []string) {
	ttlSec := int64(presenceCacheTTL.Seconds())
	result := make(map[string]*Presence, len(mris))
	stale := make([]string, 0)

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, mri := range mris {
		if mri == "" {
			continue
		}

		if p, ok := c.Users.presences[mri]; ok && now-p.UpdatedAt < ttlSec {
			cp := *p
			result[mri] = &cp

			continue
		}

		if !slices.Contains(stale, mri) {
			stale = append(stale, mri)
		}
	}

	return result, stale
}

func (c *Client) fetchPresenceBatch(ctx context.Context, presenceURL string, chunk []string, now int64, dst map[string]*Presence) {
	reqBody := make([]map[string]string, 0, len(chunk))
	for _, mri := range chunk {
		reqBody = append(reqBody, map[string]string{"mri": mri})
	}

	var resp []rawPresenceEntry

	err := c.doJSON(ctx, http.MethodPost, presenceURL, authSkypeAndBearer, c.presenceScope(), reqBody, &resp)
	if err != nil {
		c.logger.Debugf("Presence batch fetch failed for %d users: %v", len(chunk), err)

		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	for i := range resp {
		entry := &resp[i]
		if entry.MRI == "" {
			continue
		}

		note, noteExpiry := entry.statusNote()

		p := &Presence{
			MRI:                 entry.MRI,
			Availability:        entry.Presence.Availability,
			Activity:            entry.Presence.Activity,
			StatusNote:          note,
			StatusNoteExpiresAt: noteExpiry,
			LastActiveAt:        entry.lastActiveAt(),
			UpdatedAt:           now,
		}

		c.Users.presences[p.MRI] = p
		cp := *p
		dst[p.MRI] = &cp
	}
}

// SetPresence sets the authenticated user's Teams availability.
// Accepts matterircd status names ("online", "away", "dnd", "offline") as well
// as native Teams availability values ("Available", "Away", "Busy", "DoNotDisturb", "BeRightBack", "Offline").
func (c *Client) SetPresence(ctx context.Context, status string) error {
	ep := c.Endpoints()
	if ep.Presence == "" {
		return errors.New("msteamsclient: presence endpoint unavailable")
	}

	availability := normalizeTeamsAvailability(status)
	reqURL := ep.Presence + "/v1/me/forceavailability/"
	payload := map[string]string{"availability": availability}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeAndBearer, c.presenceScope(), payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: set presence %q: %w", availability, err)
	}

	if me := c.Me(); me != nil && me.MRI != "" {
		c.Users.mu.Lock()

		note := ""
		noteExpiry := int64(0)

		if prev, ok := c.Users.presences[me.MRI]; ok {
			note = prev.StatusNote
			noteExpiry = prev.StatusNoteExpiresAt
		}

		c.Users.presences[me.MRI] = &Presence{
			MRI:                 me.MRI,
			Availability:        availability,
			StatusNote:          note,
			StatusNoteExpiresAt: noteExpiry,
			UpdatedAt:           time.Now().Unix(),
		}
		c.Users.mu.Unlock()
	}

	return nil
}

// SetStatusNote sets or clears the authenticated user's Teams custom status note.
func (c *Client) SetStatusNote(ctx context.Context, message string, expiry time.Time) error {
	ep := c.Endpoints()
	if ep.Presence == "" {
		return errors.New("msteamsclient: presence endpoint unavailable")
	}

	payload := map[string]any{
		"message": message,
	}

	if !expiry.IsZero() {
		payload["expiry"] = expiry.UTC().Format(time.RFC3339)
	}

	reqURL := ep.Presence + "/v1/me/publishnote"

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeAndBearer, c.presenceScope(), payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: set status note: %w", err)
	}

	if me := c.Me(); me != nil && me.MRI != "" {
		var expUnix int64
		if !expiry.IsZero() && expiry.Year() < 9999 {
			expUnix = expiry.Unix()
		}

		c.Users.mu.Lock()

		if prev, ok := c.Users.presences[me.MRI]; ok {
			prev.StatusNote = message
			prev.StatusNoteExpiresAt = expUnix
			prev.UpdatedAt = time.Now().Unix()
		} else {
			c.Users.presences[me.MRI] = &Presence{
				MRI:                 me.MRI,
				StatusNote:          message,
				StatusNoteExpiresAt: expUnix,
				UpdatedAt:           time.Now().Unix(),
			}
		}

		c.Users.mu.Unlock()
	}

	return nil
}

// GetStatusNote returns the active (non-expired) cached custom status note for mri, or "".
func (c *Client) GetStatusNote(mri string) string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	p, ok := c.Users.presences[mri]
	if !ok || p.StatusNote == "" {
		return ""
	}

	if p.StatusNoteExpiresAt > 0 && time.Now().Unix() >= p.StatusNoteExpiresAt {
		return ""
	}

	return p.StatusNote
}

// FormatStatusNote returns the active custom status note for mri, appending
// " (Until ...)" when an expiry timestamp is set (matching matterclient).
func (c *Client) FormatStatusNote(mri string) string {
	c.Users.mu.RLock()

	p, ok := c.Users.presences[mri]
	if !ok || p.StatusNote == "" {
		c.Users.mu.RUnlock()

		return ""
	}

	note := p.StatusNote
	expUnix := p.StatusNoteExpiresAt

	tz := ""
	if u, hasUser := c.Users.users[mri]; hasUser {
		tz = strings.TrimSpace(u.TimeZone)
	}

	c.Users.mu.RUnlock()

	if expUnix == 0 {
		return note
	}

	now := time.Now()
	if now.Unix() >= expUnix {
		return ""
	}

	return note + " (Until " + formatStatusExpiry(now, time.Unix(expUnix, 0), tz) + ")"
}

func formatStatusExpiry(now, expiry time.Time, userTZ string) string {
	loc := now.Location()
	expLocal := expiry.In(loc)
	timeStr := expLocal.Format("15:04")

	nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	expDate := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day(), 0, 0, 0, 0, loc)
	daysDiff := int(expDate.Sub(nowDate).Hours() / 24)

	var dateStr string

	switch daysDiff {
	case 0:
		dateStr = "Today at " + timeStr
	case 1:
		dateStr = "Tomorrow at " + timeStr
	default:
		dateStr = expLocal.Format("Mon, 02 Jan 15:04")
	}

	if userTZ != "" {
		userLoc, err := time.LoadLocation(userTZ)
		if err == nil {
			userTimeStr := expiry.In(userLoc).Format("15:04")
			if userTimeStr != timeStr {
				dateStr += ", local time: " + userTimeStr
			}
		}
	}

	return dateStr
}

// GetUserLocalTime returns the formatted local time (e.g. "15:04 AEDT") for
// mri if their profile carries a valid IANA TimeZone, or "" otherwise.
func (c *Client) GetUserLocalTime(mri string) string {
	c.Users.mu.RLock()

	tz := ""
	if u, ok := c.Users.users[mri]; ok {
		tz = strings.TrimSpace(u.TimeZone)
	}

	c.Users.mu.RUnlock()

	if tz == "" {
		return ""
	}

	loc, err := time.LoadLocation(tz)
	if err != nil {
		return ""
	}

	return time.Now().In(loc).Format("15:04 MST")
}

// GetStatus returns the cached canonical status ("online", "away", "dnd", "offline") for mri.
func (c *Client) GetStatus(mri string) string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	if p, ok := c.Users.presences[mri]; ok {
		return MapAvailabilityToStatus(p.Availability)
	}

	return statusOffline
}

// GetStatuses returns a map of cached user MRI -> canonical status.
func (c *Client) GetStatuses() map[string]string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make(map[string]string, len(c.Users.presences))
	for mri, p := range c.Users.presences {
		out[mri] = MapAvailabilityToStatus(p.Availability)
	}

	return out
}

// MapAvailabilityToStatus translates Teams presence availability strings into
// matterircd's canonical status strings.
func MapAvailabilityToStatus(availability string) string {
	switch strings.ToLower(strings.TrimSpace(availability)) {
	case "available", "availableidle":
		return statusOnline
	case statusAway, "berightback":
		return statusAway
	case "busy", "busyidle", "donotdisturb", "inacal", "incall", "inaconferencecall", "presenting", "focusing":
		return statusDND
	default:
		return statusOffline
	}
}

func normalizeTeamsAvailability(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case statusOnline, "available":
		return "Available"
	case statusAway:
		return "Away"
	case "brb", "berightback":
		return "BeRightBack"
	case "busy":
		return "Busy"
	case statusDND, "donotdisturb":
		return "DoNotDisturb"
	case statusOffline:
		return "Offline"
	default:
		return "Available"
	}
}

func (c *Client) middleTierScope() string {
	if c.effectiveAccountType() == AccountPersonal {
		return ScopeSpacesPersonal
	}

	return ScopeSpacesWork
}

func (c *Client) presenceScope() string {
	if c.effectiveAccountType() == AccountPersonal {
		return ScopeSpacesPersonal
	}

	return ScopePresence
}

func parseRawUserProfile(rp *rawUserProfile) *User {
	if rp == nil || rp.MRI == "" {
		return nil
	}

	email := rp.Email
	if email == "" {
		email = rp.UserPrincipalName
	}

	name := strings.TrimSpace(rp.DisplayName)
	if name == "" {
		name = strings.TrimSpace(rp.GivenName + " " + rp.Surname)
	}

	location := strings.TrimSpace(rp.OfficeLocation)
	if location == "" {
		parts := make([]string, 0, 2)
		if c := strings.TrimSpace(rp.City); c != "" {
			parts = append(parts, c)
		}

		if c := strings.TrimSpace(rp.Country); c != "" {
			parts = append(parts, c)
		}

		location = strings.Join(parts, ", ")
	}

	return &User{
		MRI:         rp.MRI,
		ObjectID:    rp.ObjectID,
		DisplayName: name,
		GivenName:   rp.GivenName,
		Surname:     rp.Surname,
		Email:       email,
		UPN:         rp.UserPrincipalName,
		JobTitle:    rp.JobTitle,
		Department:  rp.Department,
		Location:    location,
		TimeZone:    strings.TrimSpace(rp.TimeZone),
		TenantID:    rp.TenantName,
		IsBot:       strings.HasPrefix(rp.MRI, "28:") || strings.EqualFold(rp.Type, "Bot"),
		IsExternal:  rp.IsExternal,
	}
}

// GetUserAvatarURL returns the Middle-Tier profile picture URL for userMRI when available.
func (c *Client) GetUserAvatarURL(userMRI string) string {
	ep := c.Endpoints()
	if ep.MiddleTier == "" || userMRI == "" {
		return ""
	}

	return fmt.Sprintf("%s/users/%s/profilepicturev2", ep.MiddleTier, url.PathEscape(userMRI))
}
