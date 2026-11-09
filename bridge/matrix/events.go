package matrix

import (
	"context"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
)

type suffixKind int

const (
	suffixEdited suffixKind = iota
	suffixDeleted
	suffixEncrypted
	suffixPinned
	suffixUnpinned
)

//nolint:gocyclo
func (m *Matrix) formatSuffix(kind suffixKind) string {
	useUnicode := m.cfg.Matrix().Formatter.Unicode

	switch {
	case kind == suffixEdited && useUnicode:
		return " \x1d(✏️edited)\x1d"
	case kind == suffixEdited:
		return " \x1d(edited)\x1d"
	case kind == suffixDeleted && useUnicode:
		return " \x1d(🗑️deleted)\x1d"
	case kind == suffixDeleted:
		return " \x1d(deleted)\x1d"
	case kind == suffixEncrypted && useUnicode:
		return " \x1d(🔒encrypted)\x1d"
	case kind == suffixEncrypted:
		return " \x1d(encrypted)\x1d"
	case kind == suffixPinned && useUnicode:
		return " \x1d(📌pinned)\x1d"
	case kind == suffixPinned:
		return " \x1d(pinned)\x1d"
	case kind == suffixUnpinned && useUnicode:
		return " \x1d(📌unpinned)\x1d"
	case kind == suffixUnpinned:
		return " \x1d(unpinned)\x1d"
	default:
		return ""
	}
}

// shortText returns the start of the first line, enough to recognize a message when it is
// deleted or pinned, without holding whole messages in memory.
func (m *Matrix) shortText(text string) string {
	// Replies start with the quoted parent as plaintext fallback.
	text = event.TrimReplyFallbackText(text)

	text, rest, multiline := strings.Cut(text, "\n")
	if text == "" {
		return ""
	}

	truncated := multiline && strings.TrimSpace(rest) != ""

	// A summary of ShortenRepliesTo characters needs at most 4 bytes per character.
	limit := msgTextMaxBytes
	if n := m.cfg.Matrix().ShortenRepliesTo; n > 0 {
		limit = min(n*4, msgTextMaxBytes)
	}

	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "")
		truncated = true
	}

	// Detach from the backing array of the whole message.
	text = strings.Clone(text)

	if truncated {
		if m.cfg.Matrix().Formatter.Unicode {
			text += "…"
		} else {
			text += "..."
		}
	}

	return text
}

// cacheText replaces the cached text of a message, for live messages and edits.
func (m *Matrix) cacheText(eventID id.EventID, text string) {
	text = m.shortText(text)
	if text == "" {
		m.msgTextCache.Remove(eventID)

		return
	}

	m.msgTextCache.Add(eventID, text)
}

// cacheTextIfAbsent keeps an existing entry, as fetched history may predate an edit.
// ContainsOrAdd is atomic, unlike a separate presence check.
func (m *Matrix) cacheTextIfAbsent(eventID id.EventID, text string) {
	text = m.shortText(text)
	if text != "" {
		m.msgTextCache.ContainsOrAdd(eventID, text)
	}
}

// allowPinFetch bounds the lookups for pins, which are delivered one by one from the sync
// goroutine, so that a bulk pin change cannot stall the sync on a slow homeserver.
func (m *Matrix) allowPinFetch() bool {
	m.pinFetchMu.Lock()
	defer m.pinFetchMu.Unlock()

	now := time.Now()
	if now.Sub(m.pinFetchStart) > pinFetchWindow {
		m.pinFetchStart = now
		m.pinFetchCount = 0
	}

	if m.pinFetchCount >= pinFetchMax {
		return false
	}

	m.pinFetchCount++

	return true
}

// cachedSummary returns the cached text of a message, shortened for display.
func (m *Matrix) cachedSummary(text string) string {
	if text == "" {
		return ""
	}

	return m.formatSummary(text, m.cfg.Matrix().ShortenRepliesTo, "")
}

// OnEdit implements matrixclient.EditHandler.
func (m *Matrix) OnEdit(e *matrixclient.Edit) {
	m.enqueueEvent(e)
}

func (m *Matrix) handleEdit(e *matrixclient.Edit, echo bool, peer id.UserID) {
	ghost := m.createUser(e.Sender)
	if ghost == nil {
		return
	}

	// Cached before the echo check, as edits sent from IRC must update the text too.
	m.editMu.Lock()
	m.cacheText(e.TargetID, e.Text)
	m.editEventIDs.Add(e.EventID, e.TargetID)
	m.latestEditIDs.Add(e.TargetID, e.EventID)
	m.editMu.Unlock()

	if ghost.Me && echo {
		return
	}

	// Replies to the edited message must show the new text.
	m.msgParentCache.Remove(e.TargetID)
	m.msgRoomCache.Add(e.TargetID, e.RoomID)

	m.sendText(ghost, e.RoomID, e.Direct, peer, e.TargetID, "", e.Text+m.formatSuffix(suffixEdited), "post_edited", m.messageType(e.Text), e.Timestamp)
}

// OnRedaction implements matrixclient.RedactionHandler.
func (m *Matrix) OnRedaction(r *matrixclient.Redaction) {
	m.enqueueEvent(r)
}

func (m *Matrix) handleRedaction(r *matrixclient.Redaction, echo bool, peer id.UserID) {
	// Redactions of other events, such as topics or pins, are not deleted messages.
	if r.TargetType.Type != "" && r.TargetType != event.EventMessage && r.TargetType != event.EventEncrypted {
		return
	}

	// An edit is a message event too, but redacting it leaves the original message in place.
	// IsEdit is best effort, as homeservers usually prune the relation from a redacted event,
	// so the edits seen by OnEdit are tracked as well.
	m.editMu.Lock()

	target, tracked := m.editEventIDs.Get(r.TargetID)
	if tracked || r.IsEdit {
		// Only the text of the latest edit is cached, an older edit being removed leaves it valid.
		if latest, _ := m.latestEditIDs.Get(target); tracked && latest == r.TargetID {
			m.msgTextCache.Remove(target)
			m.latestEditIDs.Remove(target)
		}

		m.editEventIDs.Remove(r.TargetID)
		m.editMu.Unlock()

		return
	}

	m.editMu.Unlock()

	text, hasText := m.msgTextCache.Get(r.TargetID)
	// Evicted before any early return, so echoes of our own deletions do not leave the text behind.
	m.msgTextCache.Remove(r.TargetID)

	_, inRoomCache := m.msgRoomCache.Get(r.TargetID)

	// An unknown type, as the lookup failed, must not report a removed reaction as a deleted message.
	if r.TargetType.Type == "" && !inRoomCache && !hasText {
		return
	}

	ghost := m.createUser(r.Sender)
	if ghost == nil {
		return
	}

	if ghost.Me && echo {
		m.sentRedactions.Remove(r.TargetID)

		return
	}

	m.msgParentCache.Remove(r.TargetID)

	m.sendText(ghost, r.RoomID, r.Direct, peer, r.TargetID, "", m.cachedSummary(text)+m.formatSuffix(suffixDeleted), "post_deleted", m.messageType(text), 0)
}

// fetchText looks up and caches the text of a message missing from the cache. It returns
// an empty string if the lookup fails, so that callers fall back to showing no text.
func (m *Matrix) fetchText(roomID id.RoomID, eventID id.EventID) string {
	if !m.allowPinFetch() {
		return ""
	}

	ctx, cancel := context.WithTimeout(m.ctx, fetchTextTimeout)
	defer cancel()

	ev, err := m.mc.Event(ctx, roomID, eventID)
	if err != nil {
		logger.Debugf("Unable to get post %s for pin: %s", eventID, err)

		return ""
	}

	// Encrypted content is not decrypted by Event, so there is no body to show.
	body, _ := ev.Content.Raw["body"].(string)
	m.cacheTextIfAbsent(eventID, body)

	text, _ := m.msgTextCache.Get(eventID)

	return text
}

// OnPin implements matrixclient.PinHandler.
func (m *Matrix) OnPin(p *matrixclient.PinChange) {
	m.enqueueEvent(p)
}

func (m *Matrix) handlePin(p *matrixclient.PinChange, direct bool, peer id.UserID) {
	ghost := m.createUser(p.Sender)
	if ghost == nil {
		return
	}

	text, ok := m.msgTextCache.Get(p.EventID)
	if !ok {
		text = m.fetchText(p.RoomID, p.EventID)
	}

	kind := suffixUnpinned
	if p.Pinned {
		kind = suffixPinned
	}

	m.msgRoomCache.Add(p.EventID, p.RoomID)
	m.sendText(ghost, p.RoomID, direct, peer, p.EventID, "", m.cachedSummary(text)+m.formatSuffix(kind), "post_edited", m.messageType(text), 0)
}

// OnTyping implements matrixclient.TypingHandler.
func (m *Matrix) OnTyping(t *matrixclient.Typing) {
	m.enqueueEvent(t)
}

func (m *Matrix) handleTyping(t *matrixclient.Typing) {
	channelType := ""
	if t.Direct {
		channelType = "D"
	}

	for _, userID := range t.Users {
		ghost := m.createUser(userID)
		if ghost == nil {
			continue
		}

		m.sendEvent(&bridge.Event{
			Type: "typing",
			Data: &bridge.TypingEvent{
				ChannelID:   t.RoomID.String(),
				ChannelType: channelType,
				Receiver:    m.GetMe(),
				Sender:      ghost,
			},
		})
	}
}

// OnTopic implements matrixclient.TopicHandler.
func (m *Matrix) OnTopic(t *matrixclient.TopicChange) {
	m.enqueueEvent(t)
}

func (m *Matrix) handleTopic(t *matrixclient.TopicChange) {
	ghost := m.createUser(t.Sender)
	if ghost == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_topic",
		Data: &bridge.ChannelTopicEvent{
			Text:      t.Topic,
			ChannelID: t.RoomID.String(),
			UserID:    ghost.User,
		},
	})
}

// OnRoomName implements matrixclient.RoomNameHandler.
func (m *Matrix) OnRoomName(r *matrixclient.RoomRename) {
	// Queued with the messages, not with roomUpdateLoop, so that a drop does not
	// unlink the channel before the messages synced ahead of it are delivered.
	m.enqueueEvent(newRoomUpdate(r.RoomID, r.Name, r.DisplayName, r.Kind))
}

// onRoomNameResolved is the matrixclient.WithRoomNameResolved callback, which
// must not block.
func (m *Matrix) onRoomNameResolved(roomID id.RoomID, name string) {
	m.nameResolveTried.Delete(roomID)
	m.queueRoomUpdate(roomID, name, "", matrixclient.RoomRenamed)
}

func newRoomUpdate(roomID id.RoomID, name, displayName string, kind matrixclient.RoomRenameKind) *roomUpdate {
	name = strings.TrimPrefix(name, "#")
	if displayName == "" {
		displayName = name
	}

	return &roomUpdate{
		kind: kind,
		ev: &bridge.ChannelUpdateEvent{
			ChannelID:   roomID.String(),
			Name:        name,
			DisplayName: displayName,
		},
	}
}

// queueRoomUpdate hands a rename to roomUpdateLoop. matrixclient serializes its
// callbacks, so queueing in call order and keeping only the newest name of
// consecutive renames of a room stops a stale name from being applied last.
// Drops and restores are never merged, the IRC side has to see each transition
// in order to part, unlink and recreate the channel. It never blocks.
func (m *Matrix) queueRoomUpdate(roomID id.RoomID, name, displayName string, kind matrixclient.RoomRenameKind) {
	update := newRoomUpdate(roomID, name, displayName, kind)

	m.roomUpdateMu.Lock()

	merged := false

	if kind == matrixclient.RoomRenamed {
		for i, queued := range slices.Backward(m.roomUpdates) {
			if queued.ev.ChannelID != update.ev.ChannelID {
				continue
			}

			if queued.kind == matrixclient.RoomRenamed {
				m.roomUpdates[i] = update
				merged = true
			}

			break
		}
	}

	if !merged {
		m.roomUpdates = append(m.roomUpdates, update)
	}

	m.roomUpdateMu.Unlock()

	select {
	case m.roomUpdateWake <- struct{}{}:
	default:
	}
}

// roomUpdateLoop sends the queued renames until ctx is done.
func (m *Matrix) roomUpdateLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.roomUpdateWake:
		}

		for {
			m.roomUpdateMu.Lock()

			if len(m.roomUpdates) == 0 {
				m.roomUpdateMu.Unlock()

				break
			}

			update := m.roomUpdates[0]
			m.roomUpdates[0] = nil
			m.roomUpdates = m.roomUpdates[1:]
			m.roomUpdateMu.Unlock()

			m.sendRoomUpdate(ctx, update)
		}
	}
}

// sendRoomUpdate dispatches on the kind captured by matrixclient, not on the live
// DM state, which may have changed again since the update was queued.
func (m *Matrix) sendRoomUpdate(ctx context.Context, update *roomUpdate) {
	ev := update.ev

	switch update.kind {
	case matrixclient.RoomDropped:
		m.sendEventCtx(ctx, &bridge.Event{
			Type: "channel_delete",
			Data: &bridge.ChannelDeleteEvent{ChannelID: ev.ChannelID},
		})
	case matrixclient.RoomRestored:
		// Created with the name of the transition even if the room is a DM again by now: the
		// messages synced in between are for this channel, and the drop queued behind this
		// one removes it again. The live name is a raw room ID then.
		name := ""
		if ev.Name != "" {
			name = "#" + ev.Name
		}

		info, err := m.GetChannel(ctx, ev.ChannelID)

		switch {
		case err != nil:
		case !info.DM:
			name = "#" + info.Name
		case name == "":
			return
		}

		m.sendEventCtx(ctx, &bridge.Event{
			Type: "channel_create",
			Data: &bridge.ChannelCreateEvent{ChannelID: ev.ChannelID, Name: name},
		})
	case matrixclient.RoomRenamed:
		// The name may have been given a suffix to keep it unique.
		info, err := m.GetChannel(ctx, ev.ChannelID)
		if err == nil && !info.DM {
			ev.Name = info.Name
		}

		m.sendEventCtx(ctx, &bridge.Event{Type: "channel_update", Data: ev})
	}
}

// OnInvite implements matrixclient.InviteHandler.
func (m *Matrix) OnInvite(invite *matrixclient.Invitation) {
	m.enqueueEvent(invite)
}

// sendInvite reports an invitation to the IRC layer. Joining the room by its
// ID accepts it, so the ID is always sent, also when a name or alias is known.
func (m *Matrix) sendInvite(invite *matrixclient.Invitation) {
	inviter := m.createUser(invite.Sender)
	if inviter == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_invite",
		Data: &bridge.ChannelInviteEvent{
			Inviter:   inviter,
			ChannelID: invite.RoomID.String(),
			Name:      invite.Name,
			Alias:     invite.Alias.String(),
			Reason:    invite.Reason,
			Direct:    invite.Direct,
		},
	})
}

// OnMember implements matrixclient.MemberHandler.
func (m *Matrix) OnMember(c *matrixclient.MemberChange) {
	if c.UserID == m.mc.UserID() && c.Membership != event.MembershipJoin {
		m.nameResolveTried.Delete(c.RoomID)
	}

	// Decided here, as the room may have turned into a channel or a DM by the time the
	// event is handled.
	if m.mc.IsDirect(c.RoomID) {
		return
	}

	m.enqueueEvent(c)
}

func (m *Matrix) handleMember(c *matrixclient.MemberChange) {
	target := m.createUser(c.UserID)
	actor := m.createUser(c.Sender)

	if target == nil || actor == nil {
		return
	}

	switch c.Membership {
	case event.MembershipJoin:
		m.sendEvent(&bridge.Event{
			Type: "channel_add",
			Data: &bridge.ChannelAddEvent{
				Adder:     actor,
				Added:     []*bridge.UserInfo{target},
				ChannelID: c.RoomID.String(),
			},
		})
	case event.MembershipLeave, event.MembershipBan:
		m.sendEvent(&bridge.Event{
			Type: "channel_remove",
			Data: &bridge.ChannelRemoveEvent{
				Remover:   actor,
				Removed:   []*bridge.UserInfo{target},
				ChannelID: c.RoomID.String(),
				Text:      c.Reason,
			},
		})
	}
}

// sendText emits a direct or channel message event. DMs are always shown as
// coming from the other side, as handled by the IRC layer.
func (m *Matrix) sendText(ghost *bridge.UserInfo, roomID id.RoomID, direct bool, peer id.UserID, eventID, parentID id.EventID, text, eventType, messageType string, ts int64) {
	if direct {
		receiver := m.dmReceiver(ghost, true, peer)
		if receiver == nil {
			return
		}

		m.sendEvent(&bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: roomID.String(),
				Sender:    ghost,
				Receiver:  receiver,
				MessageID: eventID.String(),
				Event:     eventType,
				ParentID:  parentID.String(),
				CreateAt:  ts,
			},
		})

		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   roomID.String(),
			Sender:      ghost,
			ChannelType: "P",
			MessageType: messageType,
			MessageID:   eventID.String(),
			Event:       eventType,
			ParentID:    parentID.String(),
			CreateAt:    ts,
		},
	})
}

// fileFromMessage returns the attachment of a media message, nil for text.
func (m *Matrix) fileFromMessage(msg *matrixclient.Message) *bridge.File {
	mxc := msg.URL
	note := ""

	if mxc == "" && msg.File != nil {
		mxc = msg.File.URL
		note = m.formatSuffix(suffixEncrypted)
	}

	if mxc == "" {
		return nil
	}

	name := msg.FileName
	if name == "" {
		name = msg.Text
	}

	f := &bridge.File{Name: name + note, URL: m.mc.PublicLink(mxc)}
	if msg.Info != nil {
		f.Size = int64(msg.Info.Size)
	}

	return f
}

// hasCaption reports whether the text of a media message is more than the file name.
func hasCaption(msg *matrixclient.Message) bool {
	return msg.FileName != "" && msg.FileName != msg.Text
}

// msgToEvent converts a fetched message for scrollback, replay, threads and search.
func (m *Matrix) msgToEvent(msg *matrixclient.Message, eventType string) *bridge.Event {
	ghost := m.createUser(msg.Sender)
	if ghost == nil {
		return nil
	}

	m.msgRoomCache.Add(msg.EventID, msg.RoomID)
	m.cacheTextIfAbsent(msg.EventID, msg.Text)

	text := msg.Text

	var files []*bridge.File

	if f := m.fileFromMessage(msg); f != nil {
		files = append(files, f)

		if !hasCaption(msg) {
			text = ""
		}
	}

	if msg.Direct {
		d := &bridge.DirectMessageEvent{
			Text:      text,
			ChannelID: msg.RoomID.String(),
			Files:     files,
			MessageID: msg.EventID.String(),
			Event:     eventType,
			ParentID:  msg.ParentID.String(),
			CreateAt:  msg.Timestamp,
		}

		d.Sender = ghost
		d.Receiver = m.GetMe()

		if ghost.Me {
			d.Receiver = m.dmPeer(msg.RoomID)
		}

		if d.Receiver == nil {
			return nil
		}

		return &bridge.Event{Type: "direct_message", Data: d}
	}

	return &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   msg.RoomID.String(),
			Sender:      ghost,
			ChannelType: "P",
			MessageType: m.messageType(text),
			Files:       files,
			MessageID:   msg.EventID.String(),
			Event:       eventType,
			ParentID:    msg.ParentID.String(),
			CreateAt:    msg.Timestamp,
		},
	}
}

func (m *Matrix) dmPeer(roomID id.RoomID) *bridge.UserInfo {
	users := m.mc.DirectUsers(roomID)
	if len(users) != 1 {
		return nil
	}

	return m.createUser(users[0])
}

// dmReceiver returns the receiver of a message: the peer for our own messages
// in a DM, as they are sent from another client, otherwise ourselves. The peer is
// the one synced with the event, see queuedEvent.
func (m *Matrix) dmReceiver(ghost *bridge.UserInfo, direct bool, peer id.UserID) *bridge.UserInfo {
	if direct && ghost.Me {
		if peer == "" {
			return nil
		}

		return m.createUser(peer)
	}

	return m.GetMe()
}

func (m *Matrix) msgsToEvents(msgs []*matrixclient.Message, eventType string) []*bridge.Event {
	events := make([]*bridge.Event, 0, len(msgs))

	for _, msg := range msgs {
		if ev := m.msgToEvent(msg, eventType); ev != nil {
			events = append(events, ev)
		}
	}

	return events
}

// roomOf resolves the room of an event ID seen earlier.
func (m *Matrix) roomOf(eventID string) (id.RoomID, bool) {
	evID := id.EventID(eventID)
	if roomID, ok := m.msgRoomCache.Get(evID); ok && roomID != "" {
		return roomID, true
	}

	for _, r := range m.mc.Rooms() {
		ev, err := m.mc.Event(m.ctx, r.ID, evID)
		if err == nil && ev != nil {
			m.msgRoomCache.Add(evID, r.ID)

			return r.ID, true
		}
	}

	return "", false
}

// resolveUserID maps a Matrix ID, the "~localpart" IRC user of a ghost, a nick
// or a display name to a user ID.
func (m *Matrix) resolveUserID(name string) (id.UserID, bool) {
	if strings.HasPrefix(name, "@") && strings.Contains(name, ":") {
		return id.UserID(name), true
	}

	lookupKey := strings.ToLower(name)
	if cachedID, ok := m.userResolveCache.Get(lookupKey); ok {
		return cachedID, true
	}

	isIdent := strings.HasPrefix(name, "~")

	for _, userID := range m.mc.KnownUsers() {
		u := m.createUser(userID)
		if u == nil {
			continue
		}

		if isIdent && strings.EqualFold(u.User, name) {
			m.userResolveCache.Add(lookupKey, userID)

			return userID, true
		}

		if !isIdent && (strings.EqualFold(u.Nick, name) || strings.EqualFold(u.Real, name)) {
			m.userResolveCache.Add(lookupKey, userID)

			return userID, true
		}
	}

	return "", false
}

// dmRoom returns the direct room with a user, "" if there is none.
func (m *Matrix) dmRoom(userID string) id.RoomID {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return ""
	}

	return m.mc.DirectRoom(uid)
}

// sendLogErr logs a failed best-effort call.
func sendLogErr(what string, err error) {
	if err != nil {
		logger.Errorf("matrix: %s: %s", what, err)
	}
}
