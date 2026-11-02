package matrixclient

import (
	"context"
	"slices"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func (c *Client) channelLocked(roomID id.RoomID) *Channel {
	ch, ok := c.channels[roomID]
	if !ok {
		ch = &Channel{ID: roomID, Members: make(map[id.UserID]*User)}
		c.channels[roomID] = ch
	}

	return ch
}

// addDirectLocked marks roomID as direct with userID. A nil member keeps any
// cached profile of the user.
func (c *Client) addDirectLocked(roomID id.RoomID, userID id.UserID, member *event.MemberEventContent) {
	u, ok := c.users[userID]
	if !ok {
		u = &User{ID: userID}
		c.users[userID] = u
	}

	if member != nil {
		u.MemberEventContent = member
	}

	ch := c.channelLocked(roomID)

	ch.Lock()
	ch.IsDirect = true
	ch.Members[userID] = u
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
	}

	c.dmChannels = make(map[id.RoomID][]id.UserID)
	c.directData = make(map[id.UserID][]id.RoomID, len(*chats))

	for userID, rooms := range *chats {
		c.directData[userID] = slices.Clone(rooms)

		for _, roomID := range rooms {
			c.addDirectLocked(roomID, userID, nil)
		}
	}
}

func (c *Client) handleMember(_ context.Context, ev *event.Event) {
	member := ev.Content.AsMember()
	if member == nil || ev.StateKey == nil {
		return
	}

	targetID := id.UserID(*ev.StateKey)

	c.Lock()
	defer c.Unlock()

	u, ok := c.users[targetID]
	if !ok {
		u = &User{ID: targetID}
		c.users[targetID] = u
	}

	u.MemberEventContent = member

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	switch member.Membership {
	case event.MembershipJoin, event.MembershipInvite:
		ch.Members[targetID] = u
	default:
		delete(ch.Members, targetID)
	}
	ch.Unlock()

	if member.IsDirect {
		c.addDirectLocked(ev.RoomID, targetID, member)
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

	ch.Name = ""
	if name.Name != "" {
		ch.Name = "#" + strings.ReplaceAll(name.Name, " ", "")
	}
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
	ch.Alias, ch.AltAliases = alias.Alias, alias.AltAliases
	ch.Unlock()
}

func (c *Client) handleMessage(_ context.Context, ev *event.Event) {
	if ev.Sender == c.mc.UserID {
		return
	}

	content := ev.Content.AsMessage()
	if content == nil {
		return
	}

	msg := &Message{
		RoomID:    ev.RoomID,
		EventID:   ev.ID,
		Sender:    ev.Sender,
		Text:      content.Body,
		Direct:    c.IsDirect(ev.RoomID),
		Timestamp: ev.Timestamp,
	}
	if content.RelatesTo != nil {
		msg.ParentID = content.RelatesTo.EventID
	}

	c.handler.OnMessage(msg)
}

func (c *Client) handleReaction(_ context.Context, ev *event.Event) {
	if ev.Sender == c.mc.UserID {
		return
	}

	content := ev.Content.AsReaction()
	if content == nil || content.RelatesTo.EventID == "" {
		return
	}

	c.handler.OnReaction(&Reaction{
		RoomID:   ev.RoomID,
		ParentID: content.RelatesTo.EventID,
		Sender:   ev.Sender,
		Key:      content.RelatesTo.Key,
		Direct:   c.IsDirect(ev.RoomID),
	})
}
