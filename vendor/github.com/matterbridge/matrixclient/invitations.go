package matrixclient

import (
	"context"
	"errors"
	"maps"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Invitations returns a snapshot of pending invitations, including direct rooms.
// Read it once the consumer is running to display invitations received offline.
// Joining accepts an invitation; Leave rejects it. A failed request preserves it,
// and a successful request is reflected when the membership is synced.
func (c *Client) Invitations() []Invitation {
	c.RLock()
	defer c.RUnlock()

	invites := make([]Invitation, 0, len(c.invitations))
	for _, invite := range c.invitations {
		invites = append(invites, invite)
	}

	return invites
}

// syncInvitations updates membership before dispatch, so callbacks and room
// listings never treat a pending invitation as a joined room through m.direct.
func (c *Client) syncInvitations(_ context.Context, res *mautrix.RespSync, _ string) bool {
	c.Lock()

	for roomID := range res.Rooms.Join {
		delete(c.leftRooms, roomID)

		if invite, ok := c.invitations[roomID]; ok && invite.Direct {
			c.acceptedDirect[roomID] = invite.Sender
			c.addDirectLocked(roomID, invite.Sender)
		}

		for _, userID := range c.dmChannels[roomID] {
			c.addDirectLocked(roomID, userID)
		}

		delete(c.invitations, roomID)
	}

	for roomID := range res.Rooms.Leave {
		c.leftRooms[roomID] = struct{}{}
	}

	c.Unlock()

	for roomID, room := range res.Rooms.Invite {
		invite := c.parseInvitation(roomID, room.State.Events)
		if invite.Sender == "" {
			c.logger.Warnf("invitation for %s has no valid self membership event", roomID)

			continue
		}

		c.Lock()
		previous, exists := c.invitations[roomID]
		c.invitations[roomID] = invite
		c.Unlock()

		if h, ok := c.handler.(InviteHandler); ok && c.synced.Load() && (!exists || previous != invite) {
			h.OnInvite(&invite)
		}
	}

	return true
}

// persistAcceptedDirect records a joined direct invitation in m.direct, which
// the homeserver does not do, so the DM association survives a restart.
func (c *Client) persistAcceptedDirect(ctx context.Context, roomID id.RoomID) {
	c.Lock()

	userID, ok := c.acceptedDirect[roomID]
	if !ok {
		invite, invited := c.invitations[roomID]
		if !invited || !invite.Direct {
			c.Unlock()

			return
		}

		userID = invite.Sender
		c.acceptedDirect[roomID] = userID
	}

	c.Unlock()

	c.recordDirect(ctx, userID, roomID)
}

// recordDirect persists roomID as a direct room of userID. The homeserver does not set
// m.direct, so it is written here to survive a restart. As each write replaces the whole
// value, every pending association is merged into it. An association stays pending until
// handleDM observes it in a synced snapshot, so a delayed older snapshot cannot make a
// later write erase it, and a failed write is retried by later calls.
func (c *Client) recordDirect(ctx context.Context, userID id.UserID, roomID id.RoomID) {
	c.directWriteMu.Lock()
	defer c.directWriteMu.Unlock()

	c.Lock()
	c.pendingDirect[roomID] = pendingAssoc{user: userID}
	direct := maps.Clone(c.directData)

	if direct == nil {
		direct = make(map[id.UserID][]id.RoomID)
	}

	unwritten := make([]id.RoomID, 0, len(c.pendingDirect))

	for pendingRoom, p := range c.pendingDirect {
		// The room being recorded was just joined or created, so its marker may be stale until the next sync.
		if _, left := c.leftRooms[pendingRoom]; left && pendingRoom != roomID {
			delete(c.pendingDirect, pendingRoom)

			continue
		}

		if slices.Contains(direct[p.user], pendingRoom) {
			delete(c.pendingDirect, pendingRoom)

			continue
		}

		direct[p.user] = append(slices.Clone(direct[p.user]), pendingRoom)

		if !p.written {
			unwritten = append(unwritten, pendingRoom)
		}
	}
	c.Unlock()

	if len(unwritten) == 0 {
		return
	}

	err := c.mc.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct)
	if err != nil {
		c.logger.Errorf("failed to persist m.direct for %s, it is retried later: %s", roomID, err)

		return
	}

	c.Lock()
	for _, pendingRoom := range unwritten {
		if p, ok := c.pendingDirect[pendingRoom]; ok {
			p.written = true
			c.pendingDirect[pendingRoom] = p
		}
	}
	c.Unlock()
}

// retryDirect retries a failed m.direct write for roomID, if there is one.
func (c *Client) retryDirect(ctx context.Context, roomID id.RoomID) {
	c.RLock()
	p, ok := c.pendingDirect[roomID]
	c.RUnlock()

	if ok && !p.written {
		c.recordDirect(ctx, p.user, roomID)
	}
}

// Leave state can create cache entries while it is dispatched. Remove them
// afterwards so a rejected or revoked invitation never becomes a listed room.
func (c *Client) removeLeftInvitations(res *mautrix.RespSync) {
	c.Lock()
	defer c.Unlock()

	for roomID := range res.Rooms.Leave {
		if _, invited := c.invitations[roomID]; invited {
			delete(c.invitations, roomID)
			delete(c.channels, roomID)
		}
	}
}

func (c *Client) parseInvitation(roomID id.RoomID, events []*event.Event) Invitation {
	invite := Invitation{RoomID: roomID}

	for _, ev := range events {
		ev.Type.Class = event.StateEventType

		if ev.Type != event.StateMember && ev.Type != event.StateRoomName && ev.Type != event.StateCanonicalAlias {
			continue
		}

		err := ev.Content.ParseRaw(ev.Type)
		if err != nil && !errors.Is(err, event.ErrContentAlreadyParsed) {
			c.logger.Warnf("failed to parse %s in invitation for %s: %s", ev.Type, roomID, err)

			continue
		}

		switch ev.Type {
		case event.StateMember:
			if ev.StateKey == nil || *ev.StateKey != string(c.mc.UserID) {
				continue
			}

			member := ev.Content.AsMember()
			if member.Membership == event.MembershipInvite {
				invite.Sender = ev.Sender
				invite.Reason = member.Reason
				invite.Direct = member.IsDirect
			}
		case event.StateRoomName:
			invite.Name = ev.Content.AsRoomName().Name
		case event.StateCanonicalAlias:
			invite.Alias = ev.Content.AsCanonicalAlias().Alias
		}
	}

	return invite
}
