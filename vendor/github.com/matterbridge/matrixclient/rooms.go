package matrixclient

import (
	"context"
	"slices"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// CreateRoom creates a room and invites users. Public rooms get a published
// alias when name is set. The room shows up in Rooms once it is synced.
func (c *Client) CreateRoom(ctx context.Context, name, topic string, public bool, invite ...id.UserID) (id.RoomID, error) {
	req := &mautrix.ReqCreateRoom{
		Name:       name,
		Topic:      topic,
		Invite:     invite,
		Preset:     "private_chat",
		Visibility: "private",
	}

	if public {
		req.Preset = "public_chat"
		req.Visibility = "public"
	}

	resp, err := c.mc.CreateRoom(ctx, req)
	if err != nil {
		return "", err
	}

	return resp.RoomID, nil
}

// Room returns a snapshot of a single room, or false if unknown, departed or an invitation.
func (c *Client) Room(roomID id.RoomID) (RoomInfo, bool) {
	c.RLock()
	defer c.RUnlock()

	if _, invited := c.invitations[roomID]; invited {
		return RoomInfo{}, false
	}

	if _, left := c.leftRooms[roomID]; left {
		return RoomInfo{}, false
	}

	ch, ok := c.channels[roomID]
	if !ok {
		return RoomInfo{}, false
	}

	ch.Lock()

	display := c.displayNameLocked(roomID, ch)
	_, inferred := c.inferredDirectLocked(ch)
	unresolved, direct, members, last := ch.nameUnresolvedLocked(), ch.IsDirect || inferred, len(ch.Members), ch.LastActivity

	if !direct && c.creatingDirectRoomLocked(ch) {
		ch.Unlock()

		return RoomInfo{}, false
	}

	if !direct {
		ch.reportNameLocked(display)
	}

	ch.noteListedLocked(direct)
	ch.Unlock()

	if unresolved {
		c.logger.Debugf("room %s has no name or alias, listing it as %s", roomID, display)
		c.healRoomName(c.ctx, roomID, ch)
	}

	if direct && members <= 1 {
		return RoomInfo{}, false
	}

	return RoomInfo{
		ID:           roomID,
		Name:         display,
		IsDirect:     direct,
		LastActivity: last,
	}, true
}

// RoomUsers returns the users of a room. The cached members are used when
// known, otherwise the joined members are fetched from the homeserver.
func (c *Client) RoomUsers(ctx context.Context, roomID id.RoomID) ([]id.UserID, error) {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if ok {
		ch.RLock()

		ids := make([]id.UserID, 0, len(ch.Members))
		for userID := range ch.Members {
			ids = append(ids, userID)
		}

		ch.RUnlock()

		if len(ids) > 0 {
			return ids, nil
		}
	}

	return c.JoinedMembers(ctx, roomID)
}

// UsernamesInRoom returns the sorted display names of the users in a room,
// falling back to the localpart of the user ID.
func (c *Client) UsernamesInRoom(ctx context.Context, roomID id.RoomID) []string {
	ids, err := c.RoomUsers(ctx, roomID)
	if err != nil {
		c.logger.Errorf("failed to get users of %s (%s): %s", c.roomLabel(roomID), roomID, err)

		return nil
	}

	names := make([]string, 0, len(ids))

	for _, userID := range ids {
		name := c.DisplayName(userID)
		if name == "" {
			local, _, _ := strings.Cut(string(userID), ":")
			name = strings.TrimPrefix(local, "@")
		}

		names = append(names, name)
	}

	slices.Sort(names)

	return names
}
