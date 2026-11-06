package matrixclient

import (
	"context"
	"slices"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Matrix spaces are the closest thing to Mattermost teams: a space is a room
// that groups other rooms, which are its "channels".
type space struct {
	isSpace  bool
	children map[id.RoomID]struct{}
}

func (c *Client) spaceLocked(roomID id.RoomID) *space {
	sp, ok := c.spaces[roomID]
	if !ok {
		sp = &space{children: make(map[id.RoomID]struct{})}
		c.spaces[roomID] = sp
	}

	return sp
}

func (c *Client) handleCreate(_ context.Context, ev *event.Event) {
	create := ev.Content.AsCreate()
	if create == nil || create.Type != event.RoomTypeSpace {
		return
	}

	c.Lock()
	c.spaceLocked(ev.RoomID).isSpace = true
	c.Unlock()
}

func (c *Client) handleSpaceChild(_ context.Context, ev *event.Event) {
	child := ev.Content.AsSpaceChild()
	if child == nil || ev.StateKey == nil || *ev.StateKey == "" {
		return
	}

	childID := id.RoomID(*ev.StateKey)

	c.Lock()
	defer c.Unlock()

	// An empty via means the child was removed from the space.
	if len(child.Via) == 0 {
		if sp, ok := c.spaces[ev.RoomID]; ok {
			delete(sp.children, childID)
		}

		return
	}

	c.spaceLocked(ev.RoomID).children[childID] = struct{}{}
}

func (c *Client) handleEncryption(_ context.Context, ev *event.Event) {
	c.Lock()
	defer c.Unlock()

	ch := c.channelLocked(ev.RoomID)

	ch.Lock()
	first := !ch.Encrypted
	ch.Encrypted = true

	if first && c.crypto == nil {
		// c is already locked, so roomLabel would deadlock.
		c.logger.Warnf("room %s (%s) is encrypted but end-to-end encryption is not enabled, its messages are unreadable", c.displayNameLocked(ev.RoomID, ch), ev.RoomID)
	}

	ch.Unlock()
}

// IsEncrypted reports whether roomID is end-to-end encrypted.
func (c *Client) IsEncrypted(roomID id.RoomID) bool {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return false
	}

	ch.RLock()
	defer ch.RUnlock()

	return ch.Encrypted
}

// Teams returns the spaces we know of, the Matrix equivalent of Mattermost teams.
func (c *Client) Teams() []RoomInfo {
	c.RLock()
	defer c.RUnlock()

	var teams []RoomInfo

	for roomID, sp := range c.spaces {
		if !sp.isSpace {
			continue
		}

		name := string(roomID)

		if ch, ok := c.channels[roomID]; ok {
			ch.RLock()

			if ch.RawName != "" {
				name = ch.RawName
			}

			ch.RUnlock()
		}

		teams = append(teams, RoomInfo{ID: roomID, Name: name})
	}

	return teams
}

// TeamByName returns the space with the given name (case insensitive), or "".
func (c *Client) TeamByName(name string) id.RoomID {
	for _, t := range c.Teams() {
		if strings.EqualFold(t.Name, name) {
			return t.ID
		}
	}

	return ""
}

// TeamChannels returns the rooms in a space.
func (c *Client) TeamChannels(spaceID id.RoomID) []id.RoomID {
	c.RLock()
	defer c.RUnlock()

	sp, ok := c.spaces[spaceID]
	if !ok {
		return nil
	}

	rooms := make([]id.RoomID, 0, len(sp.children))
	for roomID := range sp.children {
		rooms = append(rooms, roomID)
	}

	slices.Sort(rooms)

	return rooms
}

// RoomTeams returns the spaces roomID is in.
func (c *Client) RoomTeams(roomID id.RoomID) []id.RoomID {
	c.RLock()
	defer c.RUnlock()

	var teams []id.RoomID

	for spaceID, sp := range c.spaces {
		if _, ok := sp.children[roomID]; ok {
			teams = append(teams, spaceID)
		}
	}

	slices.Sort(teams)

	return teams
}

// CreateTeam creates a space.
func (c *Client) CreateTeam(ctx context.Context, name, topic string, public bool) (id.RoomID, error) {
	req := &mautrix.ReqCreateRoom{
		Name:            name,
		Topic:           topic,
		Preset:          "private_chat",
		Visibility:      "private",
		CreationContent: map[string]any{"type": event.RoomTypeSpace},
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

// AddToTeam adds a room to a space.
func (c *Client) AddToTeam(ctx context.Context, spaceID, roomID id.RoomID) error {
	_, server, ok := strings.Cut(string(roomID), ":")
	if !ok {
		server = c.mc.UserID.Homeserver()
	}

	_, err := c.mc.SendStateEvent(ctx, spaceID, event.StateSpaceChild, string(roomID), &event.SpaceChildEventContent{Via: []string{server}})

	return err
}

// RemoveFromTeam removes a room from a space.
func (c *Client) RemoveFromTeam(ctx context.Context, spaceID, roomID id.RoomID) error {
	_, err := c.mc.SendStateEvent(ctx, spaceID, event.StateSpaceChild, string(roomID), &event.SpaceChildEventContent{})

	return err
}

// noteSender records whether a user looks like a bot. Matrix has no bot flag,
// the convention is that bots only send m.notice messages, so a user is
// reported as a bot until it sends anything else.
func (c *Client) noteSender(userID id.UserID, msgType event.MessageType) {
	c.RLock()

	u, ok := c.users[userID]
	known := ok && (u.human || (msgType == event.MsgNotice && u.Bot))

	c.RUnlock()

	if known {
		return
	}

	c.Lock()
	defer c.Unlock()

	u, ok = c.users[userID]
	if !ok {
		u = &User{ID: userID}
		c.users[userID] = u
	}

	if msgType == event.MsgNotice {
		u.Bot = !u.human
	} else {
		u.human = true
		u.Bot = false
	}
}

// IsBot reports whether the user only ever sent notices, the Matrix bot convention.
func (c *Client) IsBot(userID id.UserID) bool {
	c.RLock()
	defer c.RUnlock()

	u, ok := c.users[userID]

	return ok && u.Bot
}
