package matrix

import (
	"context"
	"slices"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
)

func (m *Matrix) formatSuffix(isEdited, isDeleted, isEncrypted bool) string {
	useUnicode := m.cfg.Matrix().Formatter.Unicode

	switch {
	case isEdited && useUnicode:
		return " \x1d(✏️edited)\x1d"
	case isEdited:
		return " \x1d(edited)\x1d"
	case isDeleted && useUnicode:
		return "\x1d(🗑️deleted)\x1d"
	case isDeleted:
		return "\x1d(deleted)\x1d"
	case isEncrypted && useUnicode:
		return " \x1d(🔒encrypted)\x1d"
	case isEncrypted:
		return " \x1d(encrypted)\x1d"
	default:
		return ""
	}
}

func (m *Matrix) formatPinSuffix(pinned bool) string {
	useUnicode := m.cfg.Matrix().Formatter.Unicode

	switch {
	case pinned && useUnicode:
		return "\x1d(📌pinned)\x1d"
	case pinned:
		return "\x1d(pinned)\x1d"
	case useUnicode:
		return "\x1d(📌unpinned)\x1d"
	default:
		return "\x1d(unpinned)\x1d"
	}
}

// cacheText remembers text so the message can be shown again when it is deleted.
func (m *Matrix) cacheText(eventID id.EventID, text string) {
	if text != "" {
		m.msgTextCache.Add(eventID, text)
	}
}

// OnEdit implements matrixclient.EditHandler.
func (m *Matrix) OnEdit(e *matrixclient.Edit) {
	ghost := m.createUser(e.Sender)
	if ghost == nil {
		return
	}

	if ghost.Me && m.isSentEcho(e.EventID, e.Text) {
		return
	}

	// Replies to the edited message must show the new text.
	m.msgParentCache.Remove(e.TargetID)
	m.msgRoomCache.Add(e.TargetID, e.RoomID)
	m.cacheText(e.TargetID, e.Text)

	m.sendText(ghost, e.RoomID, e.Direct, e.TargetID, "", e.Text+m.formatSuffix(true, false, false), "post_edited", m.messageType(e.Text), e.Timestamp)
}

// OnRedaction implements matrixclient.RedactionHandler.
func (m *Matrix) OnRedaction(r *matrixclient.Redaction) {
	if r.TargetType == event.EventReaction {
		return
	}

	text, hasText := m.msgTextCache.Get(r.TargetID)
	_, inRoomCache := m.msgRoomCache.Get(r.TargetID)

	// An unknown type, as the lookup failed, must not report a removed reaction as a deleted message.
	if r.TargetType.Type == "" && !inRoomCache && !hasText {
		return
	}

	ghost := m.createUser(r.Sender)
	if ghost == nil {
		return
	}

	if ghost.Me && m.sentRedactions.Remove(r.TargetID) {
		return
	}

	m.msgParentCache.Remove(r.TargetID)
	m.msgTextCache.Remove(r.TargetID)

	suffix := m.formatSuffix(false, true, false)
	if text != "" {
		suffix = text + " " + suffix
	}

	m.sendText(ghost, r.RoomID, r.Direct, r.TargetID, "", suffix, "post_deleted", m.messageType(text), 0)
}

// OnPin implements matrixclient.PinHandler.
func (m *Matrix) OnPin(p *matrixclient.PinChange) {
	ghost := m.createUser(p.Sender)
	if ghost == nil {
		return
	}

	text, _ := m.msgTextCache.Get(p.EventID)
	suffix := m.formatPinSuffix(p.Pinned)

	if text != "" {
		suffix = text + " " + suffix
	}

	m.msgRoomCache.Add(p.EventID, p.RoomID)
	m.sendText(ghost, p.RoomID, m.mc.IsDirect(p.RoomID), p.EventID, "", suffix, "post_edited", m.messageType(text), 0)
}

// OnTyping implements matrixclient.TypingHandler.
func (m *Matrix) OnTyping(t *matrixclient.Typing) {
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
	m.queueRoomUpdate(r.RoomID, r.Name, r.DisplayName, r.Kind)
}

// onRoomNameResolved is the matrixclient.WithRoomNameResolved callback, which
// must not block.
func (m *Matrix) onRoomNameResolved(roomID id.RoomID, name string) {
	m.nameResolveTried.Delete(roomID)
	m.queueRoomUpdate(roomID, name, "", matrixclient.RoomRenamed)
}

// queueRoomUpdate hands a rename to roomUpdateLoop. matrixclient serializes its
// callbacks, so queueing in call order and keeping only the newest name of
// consecutive renames of a room stops a stale name from being applied last.
// Drops and restores are never merged, the IRC side has to see each transition
// in order to part, unlink and recreate the channel. It never blocks.
func (m *Matrix) queueRoomUpdate(roomID id.RoomID, name, displayName string, kind matrixclient.RoomRenameKind) {
	name = strings.TrimPrefix(name, "#")
	if displayName == "" {
		displayName = name
	}

	m.roomUpdateMu.Lock()

	update := &roomUpdate{
		kind: kind,
		ev: &bridge.ChannelUpdateEvent{
			ChannelID:   roomID.String(),
			Name:        name,
			DisplayName: displayName,
		},
	}

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
		// A later transition may have made it a DM again, whose channel name is the raw room ID.
		// That drop is queued behind this one, so the channel is not created in between.
		info, err := m.GetChannel(ctx, ev.ChannelID)
		if err == nil && info.DM {
			return
		}

		m.sendEventCtx(ctx, &bridge.Event{
			Type: "channel_create",
			Data: &bridge.ChannelCreateEvent{ChannelID: ev.ChannelID},
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
	m.sendInvite(invite)
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

	if m.mc.IsDirect(c.RoomID) {
		return
	}

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
func (m *Matrix) sendText(ghost *bridge.UserInfo, roomID id.RoomID, direct bool, eventID, parentID id.EventID, text, eventType, messageType string, ts int64) {
	if direct {
		receiver := m.dmReceiver(ghost, roomID, true)
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
		note = m.formatSuffix(false, false, true)
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
	m.cacheText(msg.EventID, msg.Text)

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
// in a DM, as they are sent from another client, otherwise ourselves.
func (m *Matrix) dmReceiver(ghost *bridge.UserInfo, roomID id.RoomID, direct bool) *bridge.UserInfo {
	if direct && ghost.Me {
		return m.dmPeer(roomID)
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
	return m.msgRoomCache.Get(id.EventID(eventID))
}

// resolveUserID maps a Matrix ID, the "~localpart" IRC user of a ghost, a nick
// or a display name to a user ID.
func (m *Matrix) resolveUserID(name string) (id.UserID, bool) {
	if strings.HasPrefix(name, "@") && strings.Contains(name, ":") {
		return id.UserID(name), true
	}

	isIdent := strings.HasPrefix(name, "~")

	for _, userID := range m.mc.KnownUsers() {
		u := m.createUser(userID)
		if u == nil {
			continue
		}

		if isIdent && strings.EqualFold(u.User, name) {
			return userID, true
		}

		if !isIdent && (strings.EqualFold(u.Nick, name) || strings.EqualFold(u.Real, name)) {
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
