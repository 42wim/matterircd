package googlechatclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	presenceBatchSize      = 50
	memberBatchSize        = 50
	presenceCacheTTL       = 2 * time.Minute
	freshStatusMinInterval = 30 * time.Second
	activeThreshold        = 20 * time.Minute
	maintainPresenceEvery  = time.Minute
	defaultDndHours        = 24

	statusOnline  = "online"
	statusAway    = "away"
	statusDND     = "dnd"
	statusOffline = "offline"
)

// CacheUser inserts or merges a user profile into the local cache.
func (c *Client) CacheUser(u *User) {
	if u == nil || u.ID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	existing := c.Users.users[u.ID]
	if existing == nil {
		cp := *u
		c.Users.users[u.ID] = &cp

		return
	}

	mergeUserFields(existing, u)
}

func mergeUserFields(dst, src *User) {
	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}

	if src.FirstName != "" {
		dst.FirstName = src.FirstName
	}

	if src.LastName != "" {
		dst.LastName = src.LastName
	}

	if src.Email != "" {
		dst.Email = src.Email
	}

	if src.AvatarURL != "" {
		dst.AvatarURL = src.AvatarURL
	}

	if src.OrganizationID != "" {
		dst.OrganizationID = src.OrganizationID
	}

	if src.OrganizationName != "" {
		dst.OrganizationName = src.OrganizationName
	}

	if src.IsBot {
		dst.IsBot = true
	}

	if src.IsExternal {
		dst.IsExternal = true
	}

	if src.IsConsumer {
		dst.IsConsumer = true
	}

	if src.Deleted {
		dst.Deleted = true
	}
}

// GetUser returns a copy of the cached user profile for id, or nil.
func (c *Client) GetUser(id string) *User {
	if id == "" {
		return nil
	}

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	u := c.Users.users[id]
	if u == nil {
		return nil
	}

	cp := *u

	return &cp
}

// GetUsers returns copies of all cached user profiles.
func (c *Client) GetUsers() []*User {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make([]*User, 0, len(c.Users.users))

	for _, u := range c.Users.users {
		if u != nil {
			cp := *u
			out = append(out, &cp)
		}
	}

	return out
}

// GetUserCount returns the number of cached user profiles.
func (c *Client) GetUserCount() int {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return len(c.Users.users)
}

// GetUserByEmail finds a cached user by case-insensitive email address.
func (c *Client) GetUserByEmail(email string) *User {
	target := strings.ToLower(strings.TrimSpace(email))
	if target == "" {
		return nil
	}

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, u := range c.Users.users {
		if u != nil && strings.EqualFold(u.Email, target) {
			cp := *u

			return &cp
		}
	}

	return nil
}

// ResolveUsers batch-fetches missing user profiles via get_members.
func (c *Client) ResolveUsers(ctx context.Context, ids []string) (map[string]*User, error) {
	result := make(map[string]*User, len(ids))

	var missing []string

	for _, id := range dedupStrings(ids) {
		if u := c.GetUser(id); u != nil && u.DisplayName != "" {
			result[id] = u

			continue
		}

		missing = append(missing, id)
	}

	for chunk := range slices.Chunk(missing, memberBatchSize) {
		respBytes, err := c.doProto(ctx, "get_members", encodeGetMembersRequest(chunk))
		if err != nil {
			return result, fmt.Errorf("googlechatclient: get_members: %w", err)
		}

		users, err := decodeGetMembersResponse(respBytes)
		if err != nil {
			return result, fmt.Errorf("googlechatclient: decode get_members: %w", err)
		}

		for _, pu := range users {
			u := protoUserToUser(pu)
			if u == nil {
				continue
			}

			c.CacheUser(u)
			result[u.ID] = c.GetUser(u.ID)
		}
	}

	return result, nil
}

// SearchUsers searches cached users by display name, email, or user ID.
func (c *Client) SearchUsers(_ context.Context, query string, limit int) ([]*User, error) {
	needle := strings.ToLower(strings.TrimSpace(query))
	if needle == "" {
		return nil, nil
	}

	if limit <= 0 {
		limit = 25
	}

	users := c.GetUsers()

	var out []*User

	for _, u := range users {
		if strings.Contains(strings.ToLower(u.DisplayName), needle) ||
			strings.Contains(strings.ToLower(u.Email), needle) ||
			strings.Contains(strings.ToLower(u.ID), needle) {
			out = append(out, u)

			if len(out) >= limit {
				break
			}
		}
	}

	return out, nil
}

// GetPresence returns the cached presence for userID if fresh, otherwise fetches it.
func (c *Client) GetPresence(ctx context.Context, userID string) (*Presence, error) {
	if userID == "" {
		return nil, errors.New("googlechatclient: empty user ID")
	}

	c.Users.mu.RLock()
	cached := c.Users.presences[userID]
	c.Users.mu.RUnlock()

	if cached != nil && time.Since(time.UnixMilli(cached.UpdatedAt)) < presenceCacheTTL {
		cp := *cached

		return &cp, nil
	}

	return c.GetPresenceFresh(ctx, userID)
}

// GetPresenceFresh queries get_user_presence directly unless the user's cached
// presence was fetched within freshStatusMinInterval (30s, matching matterclient).
func (c *Client) GetPresenceFresh(ctx context.Context, userID string) (*Presence, error) {
	if userID == "" {
		return nil, errors.New("googlechatclient: empty user ID")
	}

	c.Users.mu.RLock()
	cached := c.Users.presences[userID]
	c.Users.mu.RUnlock()

	if cached != nil && time.Since(time.UnixMilli(cached.UpdatedAt)) < freshStatusMinInterval {
		cp := *cached

		return &cp, nil
	}

	if userID == c.selfUserID() {
		p, err := c.fetchSelfPresence(ctx)
		if err == nil && p != nil {
			return p, nil
		}
	}

	res, err := c.GetPresences(ctx, []string{userID})
	if err != nil {
		return nil, err
	}

	p := res[userID]
	if p == nil {
		return &Presence{
			UserID:       userID,
			Availability: statusOffline,
			UpdatedAt:    time.Now().UnixMilli(),
		}, nil
	}

	return p, nil
}

// GetPresences batch-queries user presence via get_user_presence and enriches
// unknown results via PeopleStack GetAssistiveFeatures (and recent activity).
func (c *Client) GetPresences(ctx context.Context, userIDs []string) (map[string]*Presence, error) {
	unique := dedupStrings(userIDs)
	out := make(map[string]*Presence, len(unique))
	myID := c.selfUserID()

	var unknownIDs []string

	for chunk := range slices.Chunk(unique, presenceBatchSize) {
		respBytes, err := c.doProto(ctx, "get_user_presence", encodeGetUserPresenceRequest(chunk))
		if err != nil {
			return out, fmt.Errorf("googlechatclient: get_user_presence: %w", err)
		}

		items, err := decodeGetUserPresenceResponse(respBytes)
		if err != nil {
			return out, fmt.Errorf("googlechatclient: decode get_user_presence: %w", err)
		}

		for _, pp := range items {
			p := c.protoPresenceToPresence(pp)
			if p == nil {
				continue
			}

			if p.UserID != myID && p.Availability == statusOffline && !p.DND {
				unknownIDs = append(unknownIDs, p.UserID)
			}

			c.cachePresence(p)
			cp := *p
			out[p.UserID] = &cp
		}
	}

	c.enrichUnknownPresences(ctx, unknownIDs, out)

	return out, nil
}

func (c *Client) enrichUnknownPresences(ctx context.Context, unknownIDs []string, out map[string]*Presence) {
	if len(unknownIDs) == 0 {
		return
	}

	assistive, err := c.fetchAssistivePresences(ctx, unknownIDs)
	if err != nil {
		c.logger.Debugf("GetAssistiveFeatures presence lookup failed: %v", err)
	}

	for _, uid := range unknownIDs {
		p := out[uid]
		if p == nil {
			continue
		}

		if ap := assistive[uid]; ap != nil {
			switch ap.Presence {
			case assistivePresenceActive:
				p.Availability = statusOnline
				p.ActiveUntil = time.Now().Add(presenceCacheTTL).UnixMilli()
			case assistivePresenceAway:
				p.Availability = statusAway
			}

			if ap.DND {
				p.DND = true
			}
		}

		if p.Availability == statusOffline && !p.DND && c.isRecentlyActive(uid) {
			p.Availability = statusOnline
		}

		c.cachePresence(p)
		cp := *p
		out[uid] = &cp
	}
}

func (c *Client) recordUserActivity(userID string) {
	if userID == "" || userID == c.selfUserID() {
		return
	}

	now := time.Now().UnixMilli()

	var changed *Presence

	c.Users.mu.Lock()
	c.Users.lastActivity[userID] = now

	p := c.Users.presences[userID]
	switch {
	case p == nil:
		np := &Presence{
			UserID:         userID,
			Availability:   statusOnline,
			LastActivityAt: now,
			UpdatedAt:      now,
		}
		c.Users.presences[userID] = np

		cp := *np
		changed = &cp
	case !p.DND && p.Availability != statusOnline:
		p.Availability = statusOnline
		p.LastActivityAt = now
		p.UpdatedAt = now

		cp := *p
		changed = &cp
	default:
		p.LastActivityAt = now
	}

	c.Users.mu.Unlock()

	if changed != nil {
		c.emitMessage(&Message{
			Type:      EventPresenceChange,
			UserID:    userID,
			Presence:  changed,
			Timestamp: time.UnixMilli(now),
		})
	}
}

func (c *Client) isRecentlyActive(userID string) bool {
	if userID == "" {
		return false
	}

	c.Users.mu.RLock()
	last := c.Users.lastActivity[userID]
	c.Users.mu.RUnlock()

	return last > 0 && time.Since(time.UnixMilli(last)) < activeThreshold
}

// maintainPresence expires activity-based online presence after activeThreshold
// and prunes stale entries for unknown users, matching matrixclient.
func (c *Client) maintainPresence(ctx context.Context) {
	ticker := time.NewTicker(maintainPresenceEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.expirePresence(time.Now())
		}
	}
}

func (c *Client) expirePresence(now time.Time) {
	nowMs := now.UnixMilli()
	cutoff := now.Add(-activeThreshold).UnixMilli()
	myID := c.selfUserID()

	var expired []*Presence

	c.Users.mu.Lock()

	for userID := range c.Users.lastActivity {
		if _, ok := c.Users.users[userID]; !ok {
			delete(c.Users.lastActivity, userID)
		}
	}

	for userID, p := range c.Users.presences {
		if userID == myID {
			continue
		}

		if _, ok := c.Users.users[userID]; !ok {
			delete(c.Users.presences, userID)

			continue
		}

		if p == nil || p.Availability != statusOnline || p.DND {
			continue
		}

		if p.LastActivityAt > 0 && p.LastActivityAt < cutoff && (p.ActiveUntil == 0 || p.ActiveUntil < nowMs) {
			p.Availability = statusOffline
			p.UpdatedAt = nowMs

			cp := *p
			expired = append(expired, &cp)
		}
	}

	c.Users.mu.Unlock()

	for _, p := range expired {
		c.emitMessage(&Message{
			Type:      EventPresenceChange,
			UserID:    p.UserID,
			Presence:  p,
			Timestamp: now,
		})
	}
}

func (c *Client) cachePresence(p *Presence) {
	if p == nil || p.UserID == "" {
		return
	}

	c.Users.mu.Lock()
	cp := *p

	if cp.LastActivityAt == 0 {
		cp.LastActivityAt = c.Users.lastActivity[cp.UserID]
	}

	c.Users.presences[cp.UserID] = &cp
	c.Users.mu.Unlock()
}

// MapAvailabilityToStatus converts a Google Chat Presence into a matterircd-compatible
// status string ("online", "dnd", "away", "offline").
func MapAvailabilityToStatus(p *Presence) string {
	if p == nil {
		return statusOffline
	}

	if p.DND {
		return statusDND
	}

	switch strings.ToLower(p.Availability) {
	case statusOnline, "active":
		return statusOnline
	case statusAway, "inactive":
		return statusAway
	case statusDND:
		return statusDND
	default:
		return statusOffline
	}
}

// SetPresence updates the signed-in user's availability or DND state in Google Chat.
func (c *Client) SetPresence(ctx context.Context, status string) error {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case statusOnline, "available", "active", "":
		_, err := c.doProto(ctx, "set_dnd_duration", encodeSetDndDurationRequest(false, 0))
		if err != nil {
			return fmt.Errorf("googlechatclient: clear DND: %w", err)
		}

		_, err = c.doProto(ctx, "set_presence_shared", encodeSetPresenceSharedRequest(true))
		if err != nil {
			return fmt.Errorf("googlechatclient: set_presence_shared(true): %w", err)
		}

		c.updateSelfPresenceCache(statusOnline, false)

		return nil
	case statusDND, "busy", "do_not_disturb":
		durUsec := (defaultDndHours * time.Hour).Microseconds()

		_, err := c.doProto(ctx, "set_dnd_duration", encodeSetDndDurationRequest(true, durUsec))
		if err != nil {
			return fmt.Errorf("googlechatclient: set_dnd_duration: %w", err)
		}

		c.updateSelfPresenceCache(statusDND, true)

		return nil
	case statusAway, statusOffline, "invisible":
		_, err := c.doProto(ctx, "set_presence_shared", encodeSetPresenceSharedRequest(false))
		if err != nil {
			return fmt.Errorf("googlechatclient: set_presence_shared(false): %w", err)
		}

		c.updateSelfPresenceCache(statusAway, false)

		return nil
	default:
		return fmt.Errorf("googlechatclient: unsupported status %q", status)
	}
}

// SetStatusNote sets or clears the signed-in user's Google Chat custom status.
func (c *Client) SetStatusNote(ctx context.Context, emoji, message string, expiry time.Time) error {
	cleanEmoji := strings.TrimSpace(emoji)
	cleanMessage := strings.TrimSpace(message)
	reqBytes := encodeSetCustomStatusRequest(cleanEmoji, cleanMessage, expiry)

	_, err := c.doProto(ctx, "set_custom_status", reqBytes)
	if err != nil {
		return fmt.Errorf("googlechatclient: set_custom_status: %w", err)
	}

	c.updateSelfStatusNoteCache(cleanEmoji, cleanMessage, expiry)

	return nil
}

func (c *Client) updateSelfStatusNoteCache(emoji, message string, expiry time.Time) {
	myID := c.selfUserID()
	if myID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	var p Presence
	if existing := c.Users.presences[myID]; existing != nil {
		p = *existing
	} else {
		p = Presence{
			UserID:       myID,
			Availability: statusOnline,
		}
	}

	p.CustomEmoji = emoji
	p.CustomStatus = message

	if !expiry.IsZero() && (emoji != "" || message != "") {
		p.CustomStatusExpiresAt = expiry.UnixMilli()
	} else {
		p.CustomStatusExpiresAt = 0
	}

	p.UpdatedAt = time.Now().UnixMilli()

	cp := p
	c.Users.presences[myID] = &cp
}

// GetStatus returns the normalized status string for userID, including any
// active custom status text in the form "<base>: <note>".
func (c *Client) GetStatus(ctx context.Context, userID string) string {
	p, err := c.GetPresence(ctx, userID)
	if err != nil || p == nil {
		return statusOffline
	}

	return formatStatusWithNote(p)
}

// GetStatuses batch-fetches normalized status strings for userIDs.
func (c *Client) GetStatuses(ctx context.Context, userIDs []string) map[string]string {
	presences, err := c.GetPresences(ctx, userIDs)
	if err != nil {
		c.logger.Debugf("Batch presence lookup failed: %v", err)
	}

	out := make(map[string]string, len(userIDs))

	for _, id := range userIDs {
		if p := presences[id]; p != nil {
			out[id] = formatStatusWithNote(p)
		} else {
			out[id] = statusOffline
		}
	}

	return out
}

// GetStatusNote returns the active custom status note for userID, or "".
func (c *Client) GetStatusNote(ctx context.Context, userID string) string {
	p, err := c.GetPresence(ctx, userID)
	if err != nil || p == nil {
		return ""
	}

	return FormatStatusNote(p)
}

// GetUserLastActivity returns the last observed activity timestamp for userID
// in Unix milliseconds, or 0 if no activity has been observed.
func (c *Client) GetUserLastActivity(userID string) int64 {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return c.Users.lastActivity[userID]
}

// FormatStatusNote formats a Presence's non-expired custom emoji + status text,
// appending "(Until ...)" when an expiry timestamp is set (matching matterclient/msteamsclient).
func FormatStatusNote(p *Presence) string {
	if p == nil {
		return ""
	}

	now := time.Now()
	if p.CustomStatusExpiresAt > 0 && now.UnixMilli() >= p.CustomStatusExpiresAt {
		return ""
	}

	emoji := strings.TrimSpace(p.CustomEmoji)
	text := strings.TrimSpace(p.CustomStatus)

	var note string

	switch {
	case emoji != "" && text != "":
		note = emoji + " " + text
	case text != "":
		note = text
	default:
		note = emoji
	}

	if note == "" {
		return ""
	}

	if p.CustomStatusExpiresAt > 0 {
		return note + " (Until " + formatStatusExpiry(now, time.UnixMilli(p.CustomStatusExpiresAt)) + ")"
	}

	return note
}

func formatStatusExpiry(now, expiry time.Time) string {
	loc := now.Location()
	expLocal := expiry.In(loc)
	timeStr := expLocal.Format("15:04")

	nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	expDate := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day(), 0, 0, 0, 0, loc)
	daysDiff := int(expDate.Sub(nowDate).Hours() / 24)

	switch daysDiff {
	case 0:
		return "Today at " + timeStr
	case 1:
		return "Tomorrow at " + timeStr
	default:
		return expLocal.Format("Mon, 02 Jan 15:04")
	}
}

func formatStatusWithNote(p *Presence) string {
	base := MapAvailabilityToStatus(p)

	note := FormatStatusNote(p)
	if note != "" {
		return fmt.Sprintf("%s: %s", base, note)
	}

	return base
}

// GetUserAvatarURL returns the cached avatar URL for userID, or "".
func (c *Client) GetUserAvatarURL(userID string) string {
	u := c.GetUser(userID)
	if u == nil {
		return ""
	}

	return u.AvatarURL
}

func protoUserToUser(pu *protoUser) *User {
	if pu == nil || pu.ID == "" {
		return nil
	}

	name := strings.TrimSpace(pu.Name)
	if name == "" {
		name = strings.TrimSpace(pu.FirstName + " " + pu.LastName)
	}

	email := strings.TrimSpace(pu.Email)

	return &User{
		ID:             pu.ID,
		DisplayName:    name,
		FirstName:      strings.TrimSpace(pu.FirstName),
		LastName:       strings.TrimSpace(pu.LastName),
		Email:          email,
		AvatarURL:      strings.TrimSpace(pu.AvatarURL),
		OrganizationID: strings.TrimSpace(pu.OrganizationID),
		IsBot:          pu.IsBot,
		IsConsumer:     pu.IsConsumer,
		Deleted:        pu.Deleted || isDeletedUserPlaceholder(name, email),
	}
}

func isDeletedUserPlaceholder(displayName, email string) bool {
	return email == "" && strings.EqualFold(displayName, "Deleted User")
}

func (c *Client) fetchSelfPresence(ctx context.Context) (*Presence, error) {
	respBytes, err := c.doProto(ctx, "get_self_user_status", encodeGetSelfUserStatusRequest())
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: get_self_user_status: %w", err)
	}

	st, err := decodeGetSelfUserStatusResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode get_self_user_status: %w", err)
	}

	if st == nil {
		return nil, errors.New("googlechatclient: empty self user status")
	}

	if st.UserID == "" {
		st.UserID = c.selfUserID()
	}

	return c.applyUserStatusUpdate(st), nil
}

func (c *Client) updateSelfPresenceCache(avail string, dnd bool) {
	myID := c.selfUserID()
	if myID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	var p Presence
	if existing := c.Users.presences[myID]; existing != nil {
		p = *existing
	} else {
		p = Presence{UserID: myID}
	}

	p.Availability = avail
	p.DND = dnd
	p.UpdatedAt = time.Now().UnixMilli()

	cp := p
	c.Users.presences[myID] = &cp
}

func (c *Client) protoPresenceToPresence(pp *protoUserPresence) *Presence {
	if pp == nil || pp.UserID == "" {
		return nil
	}

	avail := statusOffline

	switch pp.Presence {
	case protoPresenceActive:
		avail = statusOnline
	case protoPresenceInactive, protoPresenceSharingDisabled:
		avail = statusAway
	}

	if avail == statusOffline && pp.ActiveUntilUsec > time.Now().UnixMicro() {
		avail = statusOnline
	}

	p := &Presence{
		UserID:       pp.UserID,
		Availability: avail,
		DND:          pp.DND,
		ActiveUntil:  usecToMillis(pp.ActiveUntilUsec),
		UpdatedAt:    time.Now().UnixMilli(),
	}

	if pp.Status != nil {
		applyUserStatusToPresence(p, pp.Status)

		if pp.Status.HasPresenceShared && !pp.Status.PresenceShared {
			p.Availability = statusAway
		} else if pp.UserID == c.selfUserID() && pp.Status.PresenceShared &&
			(pp.Presence == 0 || pp.Presence == protoPresenceUnknown) {
			p.Availability = statusOnline
		}
	}

	c.traceDecodedPresence(pp, p.Availability)

	return p
}

func (c *Client) traceDecodedPresence(pp *protoUserPresence, avail string) {
	var hasShared, shared bool
	if pp.Status != nil {
		hasShared = pp.Status.HasPresenceShared
		shared = pp.Status.PresenceShared
	}

	c.logger.Tracef("Presence decoded for %s: raw_presence=%d dnd=%v active_until=%d has_shared=%v shared=%v -> %s",
		pp.UserID, pp.Presence, pp.DND, pp.ActiveUntilUsec, hasShared, shared, avail)
}

func applyUserStatusToPresence(p *Presence, st *protoUserStatus) {
	if st == nil {
		return
	}

	if st.DND {
		p.DND = true
	}

	p.CustomEmoji = strings.TrimSpace(st.CustomEmoji)
	p.CustomStatus = strings.TrimSpace(st.CustomText)
	p.CustomStatusExpiresAt = usecToMillis(st.CustomExpiryUsec)
}

func (c *Client) applyUserStatusUpdate(st *protoUserStatus) *Presence {
	if st == nil || st.UserID == "" {
		return nil
	}

	isSelf := st.UserID == c.selfUserID()

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	existing := c.Users.presences[st.UserID]

	var p Presence
	if existing != nil {
		p = *existing
	} else {
		p = Presence{
			UserID:       st.UserID,
			Availability: statusOnline,
		}
	}

	if st.HasPresenceShared || isSelf {
		if st.PresenceShared {
			p.Availability = statusOnline
		} else {
			p.Availability = statusAway
		}
	}

	p.DND = st.DND
	p.CustomEmoji = strings.TrimSpace(st.CustomEmoji)
	p.CustomStatus = strings.TrimSpace(st.CustomText)
	p.CustomStatusExpiresAt = usecToMillis(st.CustomExpiryUsec)
	p.UpdatedAt = time.Now().UnixMilli()

	cp := p
	c.Users.presences[st.UserID] = &cp

	return &p
}
