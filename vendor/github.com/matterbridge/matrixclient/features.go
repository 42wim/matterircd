package matrixclient

import (
	"context"
	"slices"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/sirupsen/logrus"
)

const typingTimeout = 5 * time.Second // server side expiry of a typing notification

// Status values returned by GetStatus, matching matterclient.
const (
	StatusOnline  = "online"
	StatusAway    = "away"
	StatusOffline = "offline"
)

// Presence is the last known presence of a user.
type Presence struct {
	Status         string
	StatusMsg      string
	LastActivityAt int64 // unix milliseconds, 0 if unknown
}

// SetLogLevel sets the log level of matrixclient.
func (c *Client) SetLogLevel(level string) {
	setLevel(c.rootLogger, level)
}

// SetLogAPICalls sets the log level of the Matrix API calls.
// Set to "warn" to log API requests, matching matterclient ("debug" also logs /sync).
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

// SendTyping sends a typing notification to roomID, at most once per typingDebounce per room.
func (c *Client) SendTyping(ctx context.Context, roomID id.RoomID) error {
	now := time.Now()

	c.stateLock.Lock()
	if now.Sub(c.lastTyping[roomID]) < typingDebounce {
		c.stateLock.Unlock()

		return nil
	}

	if len(c.lastTyping) > debounceMapLimit {
		for r, t := range c.lastTyping {
			if now.Sub(t) >= typingDebounce {
				delete(c.lastTyping, r)
			}
		}
	}

	c.lastTyping[roomID] = now
	c.stateLock.Unlock()

	for attempt := 0; ; attempt++ {
		_, err := c.mc.UserTyping(ctx, roomID, true, typingTimeout)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "SendTyping", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// UpdateLastViewed marks roomID as read up to eventID. Repeated calls for the
// same event, or within lastViewedDebounce of the previous one, are skipped.
func (c *Client) UpdateLastViewed(ctx context.Context, roomID id.RoomID, eventID id.EventID) error {
	now := time.Now()

	c.stateLock.Lock()

	last, ok := c.lastViewed[roomID]
	if ok && (last.eventID == eventID || now.Sub(last.at) < lastViewedDebounce) {
		c.stateLock.Unlock()

		return nil
	}

	if len(c.lastViewed) > debounceMapLimit {
		for r, m := range c.lastViewed {
			if now.Sub(m.at) >= lastViewedDebounce {
				delete(c.lastViewed, r)
			}
		}
	}

	c.lastViewed[roomID] = viewedMark{eventID: eventID, at: now}
	c.stateLock.Unlock()

	for attempt := 0; ; attempt++ {
		err := c.mc.SetReadMarkers(ctx, roomID, &mautrix.ReqSetReadMarkers{Read: eventID, FullyRead: eventID})
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "UpdateLastViewed", err, attempt, maxAPIRetries)
		if herr != nil {
			c.stateLock.Lock()
			delete(c.lastViewed, roomID)
			c.stateLock.Unlock()

			return herr
		}

		if !retry {
			c.stateLock.Lock()
			delete(c.lastViewed, roomID)
			c.stateLock.Unlock()

			return err
		}
	}
}

// Edit replaces the text of an event we sent.
func (c *Client) Edit(ctx context.Context, roomID id.RoomID, eventID id.EventID, text string) (id.EventID, error) {
	content := event.MessageEventContent{MsgType: event.MsgText, Body: text}
	content.SetEdit(eventID)

	for attempt := 0; ; attempt++ {
		resp, err := c.mc.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
		if err == nil {
			return resp.EventID, nil
		}

		retry, herr := c.HandleRetry(ctx, "Edit", err, attempt, maxAPIRetries)
		if herr != nil {
			return "", herr
		}

		if !retry {
			return "", err
		}
	}
}

// EditHTML is like Edit with an HTML formatted body next to the plain text.
func (c *Client) EditHTML(ctx context.Context, roomID id.RoomID, eventID id.EventID, text, html string) (id.EventID, error) {
	content := event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          text,
		Format:        event.FormatHTML,
		FormattedBody: html,
	}
	content.SetEdit(eventID)

	for attempt := 0; ; attempt++ {
		resp, err := c.mc.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
		if err == nil {
			return resp.EventID, nil
		}

		retry, herr := c.HandleRetry(ctx, "EditHTML", err, attempt, maxAPIRetries)
		if herr != nil {
			return "", herr
		}

		if !retry {
			return "", err
		}
	}
}

// Delete redacts an event.
func (c *Client) Delete(ctx context.Context, roomID id.RoomID, eventID id.EventID) error {
	for attempt := 0; ; attempt++ {
		_, err := c.mc.RedactEvent(ctx, roomID, eventID)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "Delete", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// React adds a reaction to an event and returns the reaction event ID.
func (c *Client) React(ctx context.Context, roomID id.RoomID, eventID id.EventID, key string) (id.EventID, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.mc.SendReaction(ctx, roomID, eventID, key)
		if err == nil {
			return resp.EventID, nil
		}

		retry, herr := c.HandleRetry(ctx, "React", err, attempt, maxAPIRetries)
		if herr != nil {
			return "", herr
		}

		if !retry {
			return "", err
		}
	}
}

// DeleteReaction removes a reaction, given the reaction event ID.
func (c *Client) DeleteReaction(ctx context.Context, roomID id.RoomID, reactionID id.EventID) error {
	return c.Delete(ctx, roomID, reactionID)
}

// Leave leaves a room or rejects a pending invitation.
func (c *Client) Leave(ctx context.Context, roomID id.RoomID) error {
	for attempt := 0; ; attempt++ {
		_, err := c.mc.LeaveRoom(ctx, roomID)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "Leave", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// Kick removes userID from a room.
func (c *Client) Kick(ctx context.Context, roomID id.RoomID, userID id.UserID, reason string) error {
	req := &mautrix.ReqKickUser{UserID: userID, Reason: reason}

	for attempt := 0; ; attempt++ {
		_, err := c.mc.KickUser(ctx, roomID, req)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "Kick", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// LastViewedAt returns the timestamp in milliseconds of the event our read
// marker (m.fully_read) points to, or 0 if the room has none.
func (c *Client) LastViewedAt(ctx context.Context, roomID id.RoomID) int64 {
	var marker event.FullyReadEventContent

	err := c.mc.GetRoomAccountData(ctx, roomID, event.AccountDataFullyRead.Type, &marker)
	if err != nil || marker.EventID == "" {
		return 0
	}

	ev, err := c.Event(ctx, roomID, marker.EventID)
	if err != nil {
		return 0
	}

	return ev.Timestamp
}

// Invite invites userID to a room.
func (c *Client) Invite(ctx context.Context, roomID id.RoomID, userID id.UserID) error {
	req := &mautrix.ReqInviteUser{UserID: userID}

	for attempt := 0; ; attempt++ {
		_, err := c.mc.InviteUser(ctx, roomID, req)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "Invite", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// Topic returns the cached topic of a room, or "".
func (c *Client) Topic(roomID id.RoomID) string {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return ""
	}

	ch.RLock()
	defer ch.RUnlock()

	return ch.Topic
}

// SetTopic changes the topic of a room.
func (c *Client) SetTopic(ctx context.Context, roomID id.RoomID, topic string) error {
	content := &event.TopicEventContent{Topic: topic}

	for attempt := 0; ; attempt++ {
		_, err := c.mc.SendStateEvent(ctx, roomID, event.StateTopic, "", content)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "SetTopic", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// SetDisplayName changes our own display name.
func (c *Client) SetDisplayName(ctx context.Context, name string) error {
	for attempt := 0; ; attempt++ {
		err := c.mc.SetDisplayName(ctx, name)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "SetDisplayName", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// SetStatus sets our own presence ("online", "away" or "offline") and status message.
func (c *Client) SetStatus(ctx context.Context, status, msg string) error {
	p := event.PresenceOnline

	switch status {
	case StatusAway:
		p = event.PresenceUnavailable
	case StatusOffline:
		p = event.PresenceOffline
	}

	req := mautrix.ReqPresence{Presence: p, StatusMsg: msg}

	for attempt := 0; ; attempt++ {
		err := c.mc.SetPresence(ctx, req)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "SetStatus", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// GetStatus returns the cached status of a user, "offline" when unknown.
func (c *Client) GetStatus(userID id.UserID) string {
	c.RLock()
	defer c.RUnlock()

	if p, ok := c.presence[userID]; ok {
		return p.Status
	}

	return StatusOffline
}

// GetStatuses returns the cached statuses of users with known presence.
func (c *Client) GetStatuses() map[id.UserID]string {
	c.RLock()
	defer c.RUnlock()

	statuses := make(map[id.UserID]string, len(c.presence))
	for userID, p := range c.presence {
		statuses[userID] = p.Status
	}

	return statuses
}

// GetUserCustomStatus returns the cached status message of a user.
func (c *Client) GetUserCustomStatus(userID id.UserID) string {
	c.RLock()
	defer c.RUnlock()

	if p, ok := c.presence[userID]; ok {
		return p.StatusMsg
	}

	return ""
}

// GetUserLastActivity returns when the user was last active in unix
// milliseconds, or 0 if unknown.
func (c *Client) GetUserLastActivity(userID id.UserID) int64 {
	c.RLock()
	defer c.RUnlock()

	if p, ok := c.presence[userID]; ok {
		return p.LastActivityAt
	}

	return 0
}

// UpdateStatus refreshes the cached presence of a user from the homeserver,
// reusing a recent fetch within freshStatusMinInterval and falling back to
// cached presence on error, matching matterclient.
func (c *Client) UpdateStatus(ctx context.Context, userID id.UserID) error {
	c.Lock()
	key := c.userIDKeyLocked(userID)
	_, hasCached := c.presence[key]
	lastREST := c.presenceFetched[key]

	if hasCached && lastREST > 0 && time.Since(time.Unix(lastREST, 0)) <= freshStatusMinInterval {
		c.Unlock()

		return nil
	}

	c.presenceFetched[key] = time.Now().Unix()
	c.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, inactiveFetchTimeout)
	defer cancel()

	resp, err := c.mc.GetPresence(fetchCtx, userID)
	if err != nil {
		if hasCached {
			return nil
		}

		c.Lock()
		delete(c.presenceFetched, key)
		c.Unlock()

		return err
	}

	c.setPresence(userID, resp.Presence, resp.StatusMsg, int64(resp.LastActiveAgo))

	return nil
}

// GetUserAvatarURL returns the HTTP URL of the avatar of a user, see PublicLink,
// or "" if the user has none.
func (c *Client) GetUserAvatarURL(ctx context.Context, userID id.UserID) string {
	uri, err := c.mc.GetAvatarURL(ctx, userID)
	if err != nil || uri.IsEmpty() {
		return ""
	}

	return c.PublicLink(uri.CUString())
}

// GetUserLocalTime returns the current time of a user formatted as HH:MM in
// their timezone (profile field m.tz), or "" if unknown.
func (c *Client) GetUserLocalTime(ctx context.Context, userID id.UserID) string {
	c.RLock()
	if u, ok := c.users[userID]; ok && u.tzFetchedAt > 0 && time.Since(time.Unix(u.tzFetchedAt, 0)) <= timezoneTTL {
		loc := u.tz
		c.RUnlock()

		if loc == nil {
			return ""
		}

		return time.Now().In(loc).Format("15:04")
	}
	c.RUnlock()

	fetchCtx, cancel := context.WithTimeout(ctx, inactiveFetchTimeout)
	defer cancel()

	// The homeserver returns {"m.tz": "..."}, not a bare string.
	var p struct {
		TZ string `json:"m.tz"`
	}

	err := c.mc.GetProfileField(fetchCtx, userID, "m.tz", &p)

	var loc *time.Location
	if err == nil && p.TZ != "" {
		loc = c.loadTimezone(p.TZ)
	}

	c.Lock()
	if u, ok := c.users[userID]; ok {
		u.tz = loc
		u.tzFetchedAt = time.Now().Unix()
	}
	c.Unlock()

	if loc == nil {
		return ""
	}

	return time.Now().In(loc).Format("15:04")
}

func (c *Client) loadTimezone(tz string) *time.Location {
	c.RLock()
	loc, ok := c.timezones[tz]
	c.RUnlock()

	if ok {
		return loc
	}

	loaded, err := time.LoadLocation(tz)
	if err != nil {
		return nil
	}

	c.Lock()
	if len(c.timezones) < maxSharedTimezones {
		c.timezones[tz] = loaded
	}
	c.Unlock()

	return loaded
}

func (c *Client) setPresence(userID id.UserID, p event.Presence, msg string, agoMS int64) {
	status := StatusOffline

	switch p {
	case event.PresenceOnline:
		status = StatusOnline
	case event.PresenceUnavailable:
		status = StatusAway
	}

	var last int64
	if agoMS > 0 {
		last = time.Now().UnixMilli() - agoMS
	}

	c.Lock()
	c.presence[c.userIDKeyLocked(userID)] = Presence{Status: status, StatusMsg: msg, LastActivityAt: last}
	c.Unlock()
}

func (c *Client) handlePresence(_ context.Context, ev *event.Event) {
	content := ev.Content.AsPresence()
	if content == nil || ev.Sender == "" {
		return
	}

	c.setPresence(ev.Sender, content.Presence, content.StatusMessage, content.LastActiveAgo)
}

func (c *Client) handlePinnedEvents(_ context.Context, ev *event.Event) {
	content := ev.Content.AsPinnedEvents()
	if content == nil || ev.StateKey == nil || *ev.StateKey != "" {
		return
	}

	c.Lock()
	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	prev := ch.pinned
	ch.pinned = content.Pinned
	ch.Unlock()
	c.Unlock()

	h, ok := c.handler.(PinHandler)
	if !ok || !c.synced.Load() {
		return
	}

	for _, eventID := range content.Pinned {
		if !slices.Contains(prev, eventID) {
			h.OnPin(&PinChange{RoomID: ev.RoomID, EventID: eventID, Sender: ev.Sender, Pinned: true})
		}
	}

	for _, eventID := range prev {
		if !slices.Contains(content.Pinned, eventID) {
			h.OnPin(&PinChange{RoomID: ev.RoomID, EventID: eventID, Sender: ev.Sender})
		}
	}
}

func (c *Client) handleTopic(_ context.Context, ev *event.Event) {
	topic := ev.Content.AsTopic()
	if topic == nil {
		return
	}

	c.Lock()
	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	ch.Topic = topic.Topic
	ch.Unlock()
	c.Unlock()

	if h, ok := c.handler.(TopicHandler); ok && c.synced.Load() {
		h.OnTopic(&TopicChange{RoomID: ev.RoomID, Sender: ev.Sender, Topic: topic.Topic})
	}
}
