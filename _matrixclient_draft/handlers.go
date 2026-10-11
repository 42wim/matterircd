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

func (c *Client) addDirectLocked(roomID id.RoomID, userID id.UserID, member *event.MemberEventContent) {
	u := &User{ID: userID, MemberEventContent: member}
	c.users[userID] = u

	ch := c.channelLocked(roomID)

	ch.Lock()
	ch.IsDirect = true
	ch.Members[userID] = u
	ch.Unlock()

	if !slices.Contains(c.dmChannels[roomID], userID) {
		c.dmChannels[roomID] = append(c.dmChannels[roomID], userID)
	}
}

func (c *Client) handleDM(_ context.Context, ev *event.Event) {
	chats := ev.Content.AsDirectChats()
	if chats == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	for userID, rooms := range *chats {
		for _, roomID := range rooms {
			c.addDirectLocked(roomID, userID, &event.MemberEventContent{})
		}
	}
}

func (c *Client) handleMember(_ context.Context, ev *event.Event) {
	member := ev.Content.AsMember()
	if member == nil || ev.StateKey == nil {
		return
	}

	c.Lock()
	defer c.Unlock()

	if _, ok := c.users[ev.Sender]; !ok {
		c.users[ev.Sender] = &User{ID: ev.Sender, MemberEventContent: member}
	}

	if member.IsDirect {
		c.addDirectLocked(ev.RoomID, id.UserID(*ev.StateKey), member)
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

	// A canonical alias is preferred over the room name.
	if ch.Alias == "" {
		ch.Alias = id.RoomAlias("#" + strings.ReplaceAll(name.Name, " ", ""))
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
	if content == nil {
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
