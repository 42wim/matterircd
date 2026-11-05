package matrix

import (
	"context"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/matterbridge/matrixclient"
)

const (
	editedSuffix  = " \x1d(edited)\x1d"
	deletedText   = "\x1d(deleted)\x1d"
	encryptedNote = " (encrypted)"
)

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

	m.sendText(ghost, e.RoomID, e.Direct, e.TargetID, "", e.Text+editedSuffix, "post_edited", m.messageType(e.Text), e.Timestamp)
}

// OnRedaction implements matrixclient.RedactionHandler.
func (m *Matrix) OnRedaction(r *matrixclient.Redaction) {
	// The cache only holds messages, which skips redacted reactions.
	if _, ok := m.msgRoomCache.Get(r.TargetID); !ok {
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

	m.sendText(ghost, r.RoomID, r.Direct, r.TargetID, "", deletedText, "post_deleted", "", 0)
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
// callbacks, so queueing in call order and keeping only the newest name per room
// stops a stale name from being applied last. It never blocks.
// A drop or restore is kept when a plain rename of the room is queued after it.
func (m *Matrix) queueRoomUpdate(roomID id.RoomID, name, displayName string, kind matrixclient.RoomRenameKind) {
	name = strings.TrimPrefix(name, "#")
	if displayName == "" {
		displayName = name
	}

	m.roomUpdateMu.Lock()

	if m.roomUpdates == nil {
		m.roomUpdates = make(map[id.RoomID]*roomUpdate)
	}

	queued, ok := m.roomUpdates[roomID]
	if !ok {
		m.roomUpdateOrder = append(m.roomUpdateOrder, roomID)
	} else if kind == matrixclient.RoomRenamed {
		kind = queued.kind
	}

	m.roomUpdates[roomID] = &roomUpdate{
		kind: kind,
		ev: &bridge.ChannelUpdateEvent{
			ChannelID:   roomID.String(),
			Name:        name,
			DisplayName: displayName,
		},
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

			if len(m.roomUpdateOrder) == 0 {
				m.roomUpdateMu.Unlock()

				break
			}

			roomID := m.roomUpdateOrder[0]
			m.roomUpdateOrder = m.roomUpdateOrder[1:]
			update := m.roomUpdates[roomID]

			delete(m.roomUpdates, roomID)
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
		note = encryptedNote
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
