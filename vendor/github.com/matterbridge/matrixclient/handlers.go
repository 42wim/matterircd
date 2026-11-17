package matrixclient

import (
	"context"
	"slices"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	redactedTypeTimeout = 5 * time.Second
	// redactedTypeBudget bounds all redaction lookups of one sync response, so a stalled
	// server cannot hold up the serialized callbacks and the next sync.
	redactedTypeBudget = 10 * time.Second
)

func (c *Client) beginLookups() {
	c.lookupUntil.Store(time.Now().Add(redactedTypeBudget).UnixNano())
}

func (c *Client) channelLocked(roomID id.RoomID) *Channel {
	ch, ok := c.channels[roomID]
	if !ok {
		ch = &Channel{ID: roomID, Members: make(map[id.UserID]*User)}
		c.channels[roomID] = ch
	}

	return ch
}

// addDirectLocked records a DM association, not room membership. Account data
// and saved invitations can refer to users who have already left the room.
func (c *Client) addDirectLocked(roomID id.RoomID, userID id.UserID) {
	ch := c.channelLocked(roomID)

	ch.Lock()
	ch.IsDirect = true
	ch.Unlock()

	if !slices.Contains(c.dmChannels[roomID], userID) {
		c.dmChannels[roomID] = append(c.dmChannels[roomID], userID)
	}
}

// handleDM rebuilds the direct room state, m.direct is a full snapshot.
func (c *Client) handleDM(_ context.Context, ev *event.Event) {
	chats := ev.Content.AsDirectChats()
	if chats == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	for roomID := range c.dmChannels {
		if ch, ok := c.channels[roomID]; ok {
			ch.Lock()
			ch.IsDirect = false
			ch.Unlock()
		}

		c.nameDirty[roomID] = struct{}{}
	}

	c.dmChannels = make(map[id.RoomID][]id.UserID)
	c.directData = make(map[id.UserID][]id.RoomID, len(*chats))

	for userID, rooms := range *chats {
		c.directData[userID] = slices.Clone(rooms)

		for _, roomID := range rooms {
			c.addDirectLocked(roomID, userID)
		}
	}

	for roomID, userID := range c.acceptedDirect {
		c.addDirectLocked(roomID, userID)
	}

	// Associations not persisted yet are not in the snapshot, but still hold locally.
	for roomID, p := range c.pendingDirect {
		if slices.Contains(c.directData[p.user], roomID) {
			delete(c.pendingDirect, roomID)

			continue
		}

		c.addDirectLocked(roomID, p.user)
	}

	for roomID := range c.dmChannels {
		c.nameDirty[roomID] = struct{}{}
	}
}

// inferrableLocked reports whether the room has the shape of an inferred direct message: invite-only,
// not world readable, two members, no name or aliases, all confirmed by the server. ch must be locked.
func (ch *Channel) inferrableLocked() bool {
	if ch.joinRule != event.JoinRuleInvite || ch.worldReadable || !ch.joinedSeen || !ch.invitedSeen || ch.joinedCount+ch.invitedCount != 2 {
		return false
	}

	return ch.Name == "" && ch.Alias == "" && len(ch.AltAliases) == 0 && ch.nameChecked
}

// inferredDirectLocked returns the peer of a room that clients such as Element show as a
// direct message although it is not in m.direct: it is invite-only and not world readable, so
// nobody else can join or read what is sent as a private message, it is not a space, has two
// members, no name and no alias (confirmed by the server), and the other member is known. c must
// be read locked and ch locked.
func (c *Client) inferredDirectLocked(ch *Channel) (id.UserID, bool) {
	if sp, isSpace := c.spaces[ch.ID]; isSpace && sp.isSpace {
		return "", false
	}

	if ch.IsDirect || !ch.inferrableLocked() {
		return "", false
	}

	var peer id.UserID

	for userID := range ch.Members {
		if userID == c.mc.UserID {
			continue
		}

		if peer != "" {
			return "", false
		}

		peer = userID
	}

	return peer, peer != ""
}

// memberInFlight reports whether roomID may be a DM that SendDirect is still creating, which
// the consumer must not learn of as a channel.
func (c *Client) memberInFlight(roomID id.RoomID) bool {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	return ok && c.creatingDirectRoom(ch)
}

func (c *Client) handleMember(_ context.Context, ev *event.Event) {
	member := ev.Content.AsMember()
	if member == nil || ev.StateKey == nil {
		return
	}

	targetID := id.UserID(*ev.StateKey)

	if ev.Mautrix.EventSource&event.SourceInvite != 0 {
		if targetID != c.mc.UserID || member.Membership != event.MembershipInvite {
			return
		}
	} else {
		c.applyMember(ev, member, targetID)
	}

	if h, ok := c.handler.(MemberHandler); ok && c.synced.Load() && !c.memberInFlight(ev.RoomID) {
		h.OnMember(&MemberChange{
			RoomID:     ev.RoomID,
			UserID:     targetID,
			Sender:     ev.Sender,
			Membership: member.Membership,
			Reason:     member.Reason,
		})
	}
}

func (c *Client) applyMember(ev *event.Event, member *event.MemberEventContent, targetID id.UserID) {
	c.Lock()
	defer c.Unlock()

	u, ok := c.users[targetID]
	if !ok {
		u = &User{ID: targetID}
		c.users[targetID] = u
	}

	// The profile is shared by the rooms of the user, so they all may be named differently now.
	if c.synced.Load() && (u.MemberEventContent == nil || u.Displayname != member.Displayname) {
		for roomID, other := range c.channels {
			other.RLock()

			var in bool
			if other.Name == "" && other.Alias == "" {
				_, in = other.Members[targetID]
				// A hero names the room even when it is not among the members.
				in = in || slices.Contains(other.heroes, targetID)
			}

			other.RUnlock()

			if in {
				c.nameDirty[roomID] = struct{}{}
			}
		}
	}

	u.MemberEventContent = member

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()

	if targetID == c.mc.UserID {
		if ch.noteSelfMembershipLocked(member.Membership == event.MembershipJoin) {
			c.cancelNameHealing(ev.RoomID)
		}
	}

	switch member.Membership {
	case event.MembershipJoin, event.MembershipInvite:
		ch.Members[targetID] = u
	default:
		delete(ch.Members, targetID)
	}
	ch.Unlock()

	// Our own member event also carries is_direct, but the peer is the other user.
	if member.IsDirect && targetID != c.mc.UserID {
		c.addDirectLocked(ev.RoomID, targetID)
	}
}

func (c *Client) handleRoomName(_ context.Context, ev *event.Event) {
	name := ev.Content.AsRoomName()
	if name == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	defer ch.Unlock()

	ch.nameRev++
	ch.setNameLocked(name.Name)
}

// setNameLocked sets the room name, ch must be locked.
func (ch *Channel) setNameLocked(name string) {
	ch.RawName = name

	ch.Name = ""
	if name != "" {
		ch.Name = "#" + strings.ReplaceAll(name, " ", "")
	}
}

func (c *Client) handleJoinRules(_ context.Context, ev *event.Event) {
	rules := ev.Content.AsJoinRules()
	if rules == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	ch.joinRule = rules.JoinRule
	ch.Unlock()
}

func (c *Client) handleHistoryVisibility(_ context.Context, ev *event.Event) {
	vis := ev.Content.AsHistoryVisibility()
	if vis == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	ch.worldReadable = vis.HistoryVisibility == event.HistoryVisibilityWorldReadable
	ch.Unlock()
}

func (c *Client) handleCanonicalAlias(_ context.Context, ev *event.Event) {
	alias := ev.Content.AsCanonicalAlias()
	if alias == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	ch.aliasRev++
	ch.Alias, ch.AltAliases = alias.Alias, alias.AltAliases
	ch.Unlock()
}

const (
	// pruneThreshold is the minimum Channel.eventSeq size that triggers a prune.
	pruneThreshold = 1024
	// pendingTTL is how long a position is kept for an undelivered message when
	// the Crypto cannot report terminal decryption failures, see DecryptFailureNotifier.
	pendingTTL = time.Hour
)

// noteTimeline records the sync timeline position of the messages in res before
// they are dispatched. Encrypted messages with a late room key are delivered
// out of order, so their position can only be taken from the sync response.
func (c *Client) noteTimeline(res *mautrix.RespSync) {
	c.Lock()
	defer c.Unlock()

	for roomID, room := range res.Rooms.Join {
		if room != nil { //nolint:nestif
			c.noteTimelineLocked(roomID, room.Timeline.Events)

			// An omitted field keeps the heroes, an empty array clears them.
			if room.Summary.Heroes != nil {
				ch := c.channelLocked(roomID)

				ch.Lock()
				ch.heroes = slices.Clone(room.Summary.Heroes)
				ch.Unlock()
			}

			if room.Summary.JoinedMemberCount != nil || room.Summary.InvitedMemberCount != nil {
				ch := c.channelLocked(roomID)

				ch.Lock()

				if n := room.Summary.JoinedMemberCount; n != nil {
					ch.joinedCount, ch.joinedSeen = *n, true
				}

				if n := room.Summary.InvitedMemberCount; n != nil {
					ch.invitedCount, ch.invitedSeen = *n, true
				}

				ch.Unlock()
			}
		}
	}

	for roomID, room := range res.Rooms.Leave {
		if room != nil {
			c.noteTimelineLocked(roomID, room.Timeline.Events)
		}
	}
}

func (c *Client) shouldTrackTimeline(ev *event.Event, live bool) bool {
	if ev == nil || ev.ID == "" {
		return false
	}

	switch ev.Type {
	case event.EventMessage:
		return true
	case event.EventEncrypted:
		// Without crypto an encrypted event is never delivered, so it would stay pending.
		return c.crypto != nil
	case event.EventReaction, event.EventRedaction:
		return live
	default:
		return false
	}
}

func (c *Client) noteTimelineLocked(roomID id.RoomID, events []*event.Event) {
	var ch *Channel

	now := time.Now()
	live := c.synced.Load()

	for _, ev := range events {
		if !c.shouldTrackTimeline(ev, live) {
			continue
		}

		if ch == nil {
			ch = c.channelLocked(roomID)
			ch.Lock()

			if ch.eventSeq == nil {
				ch.eventSeq = make(map[id.EventID]pendingSeq)
			}
		}

		if prev, dup := ch.eventSeq[ev.ID]; dup {
			ch.removePendingLocked(prev.seq)
		}

		ch.nextSeq++
		p := pendingSeq{seq: ch.nextSeq, at: now}
		ch.eventSeq[ev.ID] = p

		if live {
			ch.pending = append(ch.pending, p)
		}
	}

	if ch == nil {
		return
	}

	if len(ch.held) == 0 {
		ch.expirePendingLocked(now, false)
	}

	if len(ch.eventSeq) > max(ch.pruneAt, pruneThreshold) {
		pruneEventSeq(ch, !c.decryptFailures)
	}

	ch.Unlock()
}

// pruneEventSeq drops the positions that cannot advance LastEventID. Pending
// positions are kept until delivery or a terminal decryption failure, only
// without failure reports they expire after pendingTTL to bound memory.
// The caller holds ch's lock.
func pruneEventSeq(ch *Channel, expire bool) {
	now := time.Now()

	for evID, p := range ch.eventSeq {
		if p.seq <= ch.lastSeq || (expire && now.Sub(p.at) > pendingTTL) {
			delete(ch.eventSeq, evID)
			ch.removePendingLocked(p.seq)
		}
	}

	// Avoid rescanning on every sync while a large tail stays pending.
	ch.pruneAt = 2 * len(ch.eventSeq)
}

// noteDecryptFailure drops the position of a message the Crypto gave up on, as
// it is never delivered.
// The failure itself is logged by the caller's handler.
func (c *Client) noteDecryptFailure(ev *event.Event, _ error) {
	c.resolvePending(ev.RoomID, ev.ID)
}

// noteDecrypted drops the position of a decrypted event whose type is not
// dispatched by ordered timeline delivery.
func (c *Client) noteDecrypted(ev *event.Event) {
	if !ev.Mautrix.WasEncrypted {
		return
	}

	switch ev.Type {
	case event.EventMessage, event.EventReaction, event.EventRedaction:
	default:
		c.resolvePending(ev.RoomID, ev.ID)
	}
}

// noteActivity records the newest message of a room, including
// those of the initial sync which are not delivered to the handler. It returns the
// timeline position of the message, 0 if it is unknown, for noteDelivered.
func (c *Client) noteActivity(roomID id.RoomID, eventID id.EventID, ts int64) uint64 {
	ch := c.ensureChannel(roomID)

	ch.Lock()
	seq := ch.consumeSeqLocked(eventID)
	ch.noteActivityLocked(eventID, ts, seq)
	ch.Unlock()

	return seq
}

// noteDelivered records that the handler call for a message has returned, or that
// the message needs none, as for the initial sync. Like noteActivity it keeps the
// event with the newest timeline position.
func (c *Client) noteDelivered(roomID id.RoomID, eventID id.EventID, seq uint64) {
	if seq == 0 {
		return
	}

	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return
	}

	ch.Lock()
	ch.noteDeliveredLocked(eventID, seq)
	ch.Unlock()
}

func (c *Client) handleMessage(_ context.Context, ev *event.Event) {
	content := ev.Content.AsMessage()
	if content == nil {
		c.resolvePending(ev.RoomID, ev.ID)

		return
	}

	if c.handleEditMessage(ev, content) {
		return
	}

	if ev.Sender != c.mc.UserID {
		c.noteSender(ev.Sender, content.MsgType)
	}

	if !c.synced.Load() {
		c.noteDelivered(ev.RoomID, ev.ID, c.noteActivity(ev.RoomID, ev.ID, ev.Timestamp))

		return
	}

	msg := &Message{
		RoomID:    ev.RoomID,
		EventID:   ev.ID,
		Sender:    ev.Sender,
		Text:      content.Body,
		Direct:    c.IsDirect(ev.RoomID),
		Timestamp: ev.Timestamp,
		MsgType:   content.MsgType,
		URL:       content.URL,
		FileName:  content.GetFileName(),
		Info:      content.Info,
		File:      content.File,
	}
	if content.RelatesTo != nil {
		msg.ParentID = content.RelatesTo.EventID
	}

	c.deliverOrdered(ev.RoomID, ev.ID, ev.Timestamp, true, msg)
}

func (c *Client) handleEditMessage(ev *event.Event, content *event.MessageEventContent) bool {
	targetID := content.RelatesTo.GetReplaceID()
	if targetID == "" {
		return false
	}

	if _, ok := c.handler.(EditHandler); !ok {
		return false
	}

	if !c.synced.Load() {
		c.noteDelivered(ev.RoomID, ev.ID, c.noteActivity(ev.RoomID, ev.ID, ev.Timestamp))

		return true
	}

	text := content.Body
	if content.NewContent != nil {
		text = content.NewContent.Body
	}

	c.deliverOrdered(ev.RoomID, ev.ID, ev.Timestamp, true, &Edit{
		RoomID:    ev.RoomID,
		EventID:   ev.ID,
		TargetID:  targetID,
		Sender:    ev.Sender,
		Text:      text,
		Direct:    c.IsDirect(ev.RoomID),
		Timestamp: ev.Timestamp,
	})

	return true
}

func (c *Client) handleReaction(_ context.Context, ev *event.Event) {
	if !c.synced.Load() {
		c.resolvePending(ev.RoomID, ev.ID)

		return
	}

	content := ev.Content.AsReaction()
	if content == nil || content.RelatesTo.EventID == "" {
		c.resolvePending(ev.RoomID, ev.ID)

		return
	}

	c.deliverOrdered(ev.RoomID, ev.ID, 0, false, &Reaction{
		RoomID:   ev.RoomID,
		EventID:  ev.ID,
		ParentID: content.RelatesTo.EventID,
		Sender:   ev.Sender,
		Key:      content.RelatesTo.Key,
		Direct:   c.IsDirect(ev.RoomID),
	})
}

func (c *Client) handleRedaction(ctx context.Context, ev *event.Event) {
	_, ok := c.handler.(RedactionHandler)
	if !ok || !c.synced.Load() {
		c.resolvePending(ev.RoomID, ev.ID)

		return
	}

	targetID := ev.Redacts
	if content := ev.Content.AsRedaction(); content != nil && content.Redacts != "" {
		targetID = content.Redacts
	}

	if targetID == "" {
		c.resolvePending(ev.RoomID, ev.ID)

		return
	}

	targetType, isEdit := c.redactedTarget(ctx, ev.RoomID, targetID)

	c.deliverOrdered(ev.RoomID, ev.ID, 0, false, &Redaction{
		RoomID:     ev.RoomID,
		TargetID:   targetID,
		Sender:     ev.Sender,
		Direct:     c.IsDirect(ev.RoomID),
		TargetType: targetType,
		IsEdit:     isEdit,
	})
}

// redactedTarget returns the type of a redacted event, empty if it cannot be fetched
// or the lookup budget of the sync response is used up, and whether it was an edit.
// Redaction keeps the event type, so it tells a message from a reaction even for
// events this client never saw. The m.replace relation is only reported when the
// homeserver still returns it on the redacted event, most prune it with the rest
// of the content, so isEdit false does not prove the target was not an edit.
func (c *Client) redactedTarget(ctx context.Context, roomID id.RoomID, eventID id.EventID) (event.Type, bool) {
	remaining := time.Until(time.Unix(0, c.lookupUntil.Load()))
	if remaining <= 0 {
		return event.Type{}, false
	}

	ctx, cancel := context.WithTimeout(ctx, min(redactedTypeTimeout, remaining))
	defer cancel()

	target, err := c.mc.GetEvent(ctx, roomID, eventID)
	if err != nil || target == nil {
		return event.Type{}, false
	}

	rel, _ := target.Content.Raw["m.relates_to"].(map[string]any)
	relType, _ := rel["rel_type"].(string)

	return target.Type, event.RelationType(relType) == event.RelReplace
}

func (c *Client) handleTyping(_ context.Context, ev *event.Event) {
	h, ok := c.handler.(TypingHandler)
	if !ok || !c.synced.Load() {
		return
	}

	content := ev.Content.AsTyping()
	if content == nil {
		return
	}

	users := make([]id.UserID, 0, len(content.UserIDs))

	for _, userID := range content.UserIDs {
		if userID != c.mc.UserID {
			users = append(users, userID)
		}
	}

	h.OnTyping(&Typing{RoomID: ev.RoomID, Users: users, Direct: c.IsDirect(ev.RoomID)})
}
