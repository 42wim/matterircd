package matrixclient

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	maxFallbackNames = 3
	shortIDLength    = 8

	// Same retry policy as matterclient's channel name healing.
	nameHealInitialDelay = 5 * time.Second
	nameHealMaxDelay     = 5 * time.Minute
	nameHealMaxAttempts  = 12

	nameLookupTimeout = 30 * time.Second
	maxNameLookups    = 5
)

// sanitizeName drops what is not valid in an IRC channel name: whitespace, control
// characters such as CR, LF, NUL and BEL, and the ',' and ':' separators.
func sanitizeName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == ',' || r == ':' {
			return -1
		}

		return r
	}, s)
}

// displayNameLocked returns the channel name of a room: its canonical alias, its
// name, the members as Element names rooms without a name, or a short room ID.
// c must be read locked and ch locked.
func (c *Client) displayNameLocked(roomID id.RoomID, ch *Channel) string {
	switch {
	case ch.Alias != "":
		return c.roomName(ch.Alias)
	case ch.Name != "":
		return strings.Replace(ch.Name, ":", "/", 1)
	}

	name := c.memberNameLocked(ch)
	if name != "" {
		return name
	}

	return "#unnamed-" + shortRoomID(roomID)
}

// roomLabel returns the channel name of a room for logging, the room ID if it is unknown.
func (c *Client) roomLabel(roomID id.RoomID) string {
	c.RLock()
	defer c.RUnlock()

	ch, ok := c.channels[roomID]
	if !ok {
		return string(roomID)
	}

	ch.Lock()
	defer ch.Unlock()

	return c.displayNameLocked(roomID, ch)
}

// memberNameLocked names a room after its other members, preferring the heroes the
// server picked for it. It returns "" if there are none. c must be read locked
// and ch locked.
func (c *Client) memberNameLocked(ch *Channel) string {
	self := c.mc.UserID

	others := len(ch.Members)
	if _, ok := ch.Members[self]; ok {
		others--
	}

	var picked [maxFallbackNames]id.UserID

	n := 0

	if len(ch.heroes) > 0 {
		for _, userID := range ch.heroes {
			if userID != self && n < len(picked) {
				picked[n] = userID
				n++
			}
		}
	} else {
		// The smallest IDs, so the name does not depend on the map order.
		for userID := range ch.Members {
			if userID != self {
				n = insertSmallest(&picked, n, userID)
			}
		}
	}

	if n == 0 {
		return ""
	}

	var b strings.Builder

	b.WriteByte('#')

	for i, userID := range picked[:n] {
		if i > 0 {
			b.WriteByte('-')
		}

		b.WriteString(c.userLabelLocked(userID))
	}

	if others > n {
		b.WriteByte('+')
		b.WriteString(strconv.Itoa(others - n))
	}

	return b.String()
}

// insertSmallest keeps the n smallest IDs seen in picked, sorted, and returns the new n.
func insertSmallest(picked *[maxFallbackNames]id.UserID, n int, userID id.UserID) int {
	i := n

	if n == len(picked) {
		if userID >= picked[n-1] {
			return n
		}

		i = n - 1
	} else {
		n++
	}

	for ; i > 0 && picked[i-1] > userID; i-- {
		picked[i] = picked[i-1]
	}

	picked[i] = userID

	return n
}

// userLabelLocked is the display name of a user, or the localpart of its ID. c must be read locked.
func (c *Client) userLabelLocked(userID id.UserID) string {
	if u, ok := c.users[userID]; ok && u.MemberEventContent != nil {
		if label := sanitizeName(u.Displayname); label != "" {
			return label
		}
	}

	local, _, _ := strings.Cut(strings.TrimPrefix(string(userID), "@"), ":")

	return sanitizeName(local)
}

func shortRoomID(roomID id.RoomID) string {
	short, _, _ := strings.Cut(strings.TrimPrefix(string(roomID), "!"), ":")

	return short[:min(len(short), shortIDLength)]
}

// nameUnresolvedLocked reports whether the room has neither a name nor an alias and
// the server was not yet confirmed to have none, so its name may still be missing
// because the state was lost. ch must be locked.
func (ch *Channel) nameUnresolvedLocked() bool {
	return ch.Name == "" && ch.Alias == "" && !ch.IsDirect && !ch.nameChecked && !ch.nameGaveUp
}

// noteSelfMembershipLocked tracks our own membership. Only an actual change, not a
// profile update that repeats the join, starts a new epoch: it discards the lookups
// in flight and lets a room that was given up on be healed again. ch must be locked.
func (ch *Channel) noteSelfMembershipLocked(joined bool) bool {
	if ch.selfJoined == joined {
		return false
	}

	ch.selfJoined = joined
	ch.nameEpoch++

	if joined {
		ch.nameGaveUp = false
	}

	return true
}

// noteListedLocked records that the room was handed out as a channel. While a callback is
// being delivered the flags stay, as the callback may still drop the channel that was
// just handed out, and the comparison after it must restore it. ch must be locked.
func (ch *Channel) noteListedLocked(direct bool) {
	if ch.delivering {
		// A DM confirmed during delivery skipped reportNameLocked, the loop still has to compare again.
		ch.renameAgain = true

		return
	}

	if direct {
		return
	}

	ch.listedAsChannel, ch.droppedAsDirect = true, false
}

// reportNameLocked records display as the name the consumer holds. While a callback is
// being delivered the baseline stays, as the callback carries an older name, and the
// delivery loop of notifyRoomName compares again once the callback returned. ch must be locked.
func (ch *Channel) reportNameLocked(display string) {
	if ch.delivering {
		ch.renameAgain = true

		return
	}

	ch.reportedName = display
}

// ResolveRoomName returns the channel name of roomID, as Rooms lists it. A room
// known only by its ID is first looked up on the server, bounded by ctx, so a
// caller can pass a short deadline before creating the channel. It returns ""
// for an unknown room.
func (c *Client) ResolveRoomName(ctx context.Context, roomID id.RoomID) string {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return ""
	}

	ch.RLock()
	unresolved := ch.nameUnresolvedLocked()
	ch.RUnlock()

	if unresolved {
		err := c.RefreshRoomName(ctx, roomID)
		if err != nil {
			c.logger.Debugf("resolving name of %s: %s", roomID, err)
			// The healer outlives the short deadline of the caller, so it follows the client instead.
			c.healRoomName(c.ctx, roomID, ch) //nolint:contextcheck
		}
	}

	c.RLock()
	defer c.RUnlock()

	ch.Lock()
	defer ch.Unlock()

	display := c.displayNameLocked(roomID, ch)
	_, inferred := c.inferredDirectLocked(ch)
	direct := ch.IsDirect || inferred

	// The name of a DM is not a channel name, the baseline stays what the consumer holds.
	if !direct {
		ch.reportNameLocked(display)
	}

	ch.noteListedLocked(direct)

	return display
}

// notifyRoomName delivers a RoomRename, or with healed the WithRoomNameResolved
// callback, if the channel name of roomID differs from the one last reported through
// Rooms, ResolveRoomName or a callback. A room never reported is skipped in both
// cases, the consumer does not know it yet and gets the current name when it asks.
// Deliveries are serialized by nameNotify so callbacks arrive in the order of the
// names. No other lock is held during a callback, so it may call client methods.
func (c *Client) notifyRoomName(roomID id.RoomID, healed bool) {
	h, isHandler := c.handler.(RoomNameHandler)

	// Drops and restores of a channel always go through RoomNameHandler, also when healed.
	transitions := isHandler && c.synced.Load()
	names := transitions

	if healed {
		names = c.nameResolved != nil
	}

	if !names && !transitions {
		return
	}

	c.nameNotify.Lock()
	defer c.nameNotify.Unlock()

	for {
		rename, changed, transition := c.claimRoomName(roomID, names, transitions)
		if !changed {
			return
		}

		if healed && !transition {
			c.nameResolved(roomID, rename.Name)
		} else {
			h.OnRoomName(rename)
		}

		if !c.finishRoomName(roomID) {
			return
		}
	}
}

// departedLocked reports whether roomID is an invitation or a room we left, which Rooms excludes. c must be read locked.
func (c *Client) departedLocked(roomID id.RoomID) bool {
	_, invited := c.invitations[roomID]
	_, left := c.leftRooms[roomID]

	return invited || left
}

// claimRoomName moves the reported name of a room to its current name and marks the
// delivery of the callback as running, see reportNameLocked.
func (c *Client) claimRoomName(roomID id.RoomID, names, transitions bool) (*RoomRename, bool, bool) {
	c.RLock()
	defer c.RUnlock()

	ch, ok := c.channels[roomID]
	if !ok {
		return nil, false, false
	}

	ch.Lock()
	defer ch.Unlock()

	old := ch.reportedName
	name := c.displayNameLocked(roomID, ch)

	_, inferred := c.inferredDirectLocked(ch)
	direct := ch.IsDirect || inferred

	// A room handed out as a channel that turned into a DM is reported once, whatever its name,
	// so the consumer can drop the channel. Later changes of a DM name are not reported.
	if transitions && direct && ch.listedAsChannel {
		ch.listedAsChannel, ch.droppedAsDirect = false, true

		return ch.startDeliveryLocked(roomID, old, name, RoomDropped), true, true
	}

	// The reverse transition: a room reported as a DM is a channel again, so the consumer restores it.
	if transitions && !direct && ch.droppedAsDirect && !c.departedLocked(roomID) {
		ch.listedAsChannel, ch.droppedAsDirect = true, false

		return ch.startDeliveryLocked(roomID, old, name, RoomRestored), true, true
	}

	// A room reported as a DM is not held by the consumer, so its renames are not reported
	// until it is restored, which a departed room never is.
	if !names || name == old || old == "" || direct || ch.droppedAsDirect {
		return nil, false, false
	}

	return ch.startDeliveryLocked(roomID, old, name, RoomRenamed), true, false
}

// startDeliveryLocked moves the reported name to name and marks the delivery of a callback
// as running. ch must be locked.
func (ch *Channel) startDeliveryLocked(roomID id.RoomID, old, name string, kind RoomRenameKind) *RoomRename {
	ch.reportedName = name
	ch.delivering = true
	ch.renameAgain = false

	return &RoomRename{Kind: kind, RoomID: roomID, OldName: old, Name: name, DisplayName: ch.RawName}
}

// finishRoomName ends the delivery of a callback and reports whether the room
// was reported meanwhile, in which case its name has to be compared again.
func (c *Client) finishRoomName(roomID id.RoomID) bool {
	c.RLock()
	defer c.RUnlock()

	ch, ok := c.channels[roomID]
	if !ok {
		return false
	}

	ch.Lock()
	defer ch.Unlock()

	again := ch.renameAgain
	ch.delivering, ch.renameAgain = false, false

	return again
}

// noteRoomNames reports the channel name changes of the rooms in res, once all
// its events were applied so a burst of state events yields a single change, and
// heals the rooms that are still without a name.
func (c *Client) noteRoomNames(res *mautrix.RespSync) {
	_, notify := c.handler.(RoomNameHandler)

	// Rooms outside res can be affected as well, by a profile change in another room.
	c.Lock()
	rooms := make(map[id.RoomID]struct{}, len(res.Rooms.Join)+len(c.nameDirty))

	for roomID := range res.Rooms.Join {
		rooms[roomID] = struct{}{}
	}

	for roomID := range c.nameDirty {
		rooms[roomID] = struct{}{}
	}

	clear(c.nameDirty)
	c.Unlock()

	for roomID := range rooms {
		if notify {
			c.notifyRoomName(roomID, false)
		}

		c.RLock()
		ch, ok := c.channels[roomID]
		c.RUnlock()

		if !ok {
			continue
		}

		ch.RLock()
		unresolved := ch.nameUnresolvedLocked()
		ch.RUnlock()

		if unresolved {
			c.healRoomName(c.ctx, roomID, ch)
		}
	}
}

// healRoomName starts, at most once per room, a background retry with exponential
// backoff that refetches the name and alias of a room that has neither, for
// example because the state was lost during a failed sync. It stops on success,
// when the room is gone, after nameHealMaxAttempts, or when the client stops.
// The WithRoomNameResolved callback is called once the name changed from the one reported.
func (c *Client) healRoomName(parent context.Context, roomID id.RoomID, ch *Channel) {
	ctx, cancel := context.WithCancel(parent)

	// Only the room lock is taken, as callers may hold the client lock and a nested read lock can deadlock.
	// Membership changes cancel healers under the room write lock, so registering and reading
	// the epoch under the read lock keeps a cancellation paired with the epoch it belongs to.
	ch.RLock()

	_, running := c.nameHealing.LoadOrStore(roomID, cancel)
	epoch := ch.nameEpoch

	ch.RUnlock()

	if running {
		cancel()

		return
	}

	go func() {
		defer func() {
			cancel()
			c.nameHealing.Delete(roomID)
			c.restartStaleHealing(parent, roomID, ch, epoch)
		}()

		delay := nameHealInitialDelay

		for attempt := 1; attempt <= nameHealMaxAttempts; attempt++ {
			resolved, final := c.attemptRoomName(ctx, roomID, epoch)
			if final {
				// A room confirmed to have no name may turn out to be a DM. That is reported through
				// RoomNameHandler, so it does not depend on the optional WithRoomNameResolved callback.
				if ctx.Err() == nil {
					if resolved {
						c.notifyRoomName(roomID, true)
					}

					c.notifyRoomName(roomID, false)
				}

				return
			}

			if resolved {
				c.logger.Infof("room name for %s resolved after %d attempt(s)", roomID, attempt)
				c.notifyRoomName(roomID, true)

				return
			}

			c.logger.Debugf("room name for %s still unresolved (attempt %d/%d), retrying in %s",
				roomID, attempt, nameHealMaxAttempts, delay)

			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}

			delay = min(delay*2, nameHealMaxDelay)
		}

		c.logger.Warnf("giving up resolving room name for %s", roomID)
		c.giveUpRoomName(roomID, epoch)
	}()
}

// cancelNameHealing stops the healing of a room whose membership changed, so that the
// next sync or listing can start one for the new epoch instead of finding a stale one running.
func (c *Client) cancelNameHealing(roomID id.RoomID) {
	v, ok := c.nameHealing.Load(roomID)
	if !ok {
		return
	}

	cancel, ok := v.(context.CancelFunc)
	if ok {
		cancel()
	}
}

// restartStaleHealing starts healing again when the one that just ended belonged to a
// previous membership epoch and the room still has no name. Its guard entry blocked
// the starts requested in the meantime.
func (c *Client) restartStaleHealing(parent context.Context, roomID id.RoomID, ch *Channel, epoch uint64) {
	if parent.Err() != nil {
		return
	}

	ch.RLock()
	stale := ch.nameEpoch != epoch && ch.selfJoined && ch.nameUnresolvedLocked()
	ch.RUnlock()

	if stale {
		c.healRoomName(parent, roomID, ch)
	}
}

// giveUpRoomName stops the sync and listing from starting healing for a room again,
// until we join it again. Without it a forbidden room, or one that ran out of
// attempts, would restart with a fresh retry budget on every listing or sync.
func (c *Client) giveUpRoomName(roomID id.RoomID, epoch uint64) {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return
	}

	ch.Lock()
	defer ch.Unlock()

	// After we joined or left again, the result belongs to the previous membership.
	if ch.nameEpoch == epoch {
		ch.nameGaveUp = true
	}
}

// attemptRoomName makes one lookup. It returns whether the room now has a name or
// alias, and whether retrying is pointless: the room is gone, the server confirmed
// it has neither (like a 404 on a Mattermost channel), or the client stopped.
func (c *Client) attemptRoomName(ctx context.Context, roomID id.RoomID, epoch uint64) (bool, bool) {
	err := c.RefreshRoomName(ctx, roomID)

	switch {
	case ctx.Err() != nil || errors.Is(err, mautrix.MUnknownToken):
		// Not about the room, a later sync or listing may try again.
		return false, true
	case errors.Is(err, mautrix.MForbidden):
		c.giveUpRoomName(roomID, epoch)

		return false, true
	}

	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return false, true
	}

	ch.RLock()
	defer ch.RUnlock()

	return ch.Name != "" || ch.Alias != "", ch.nameChecked
}

// RefreshRoomName fetches the name and canonical alias of roomID from the server
// and applies the ones the room does not have yet. A room the server reports as
// having neither, by a 404 or by empty content after they were cleared, is marked
// as such and not healed again, later changes arrive through the sync. Unlike
// ResolveRoomName it does not report anything.
func (c *Client) RefreshRoomName(ctx context.Context, roomID id.RoomID) error {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if !ok {
		return nil
	}

	ch.RLock()
	nameRev, aliasRev, epoch := ch.nameRev, ch.aliasRev, ch.nameEpoch
	ch.RUnlock()

	select {
	case c.nameLookups <- struct{}{}:
		defer func() { <-c.nameLookups }()
	case <-ctx.Done():
		return ctx.Err()
	}

	ctx, cancel := context.WithTimeout(ctx, nameLookupTimeout)
	defer cancel()

	var (
		name  event.RoomNameEventContent
		alias event.CanonicalAliasEventContent
	)

	nameErr := c.mc.StateEvent(ctx, roomID, event.StateRoomName, "", &name)
	aliasErr := c.mc.StateEvent(ctx, roomID, event.StateCanonicalAlias, "", &alias)

	nameAbsent := absentState(nameErr, name.Name == "")
	aliasAbsent := absentState(aliasErr, alias.Alias == "")

	ch.Lock()
	defer ch.Unlock()

	// The result belongs to a previous membership if we joined or left meanwhile.
	if ch.nameEpoch != epoch {
		return errors.Join(lookupError(nameErr), lookupError(aliasErr))
	}

	// A sync event that arrived in the meantime, a clear included, is newer than the fetched state.
	if ch.Name == "" && name.Name != "" && ch.nameRev == nameRev {
		ch.setNameLocked(name.Name)
	}

	if ch.Alias == "" && (alias.Alias != "" || len(alias.AltAliases) > 0) && ch.aliasRev == aliasRev {
		ch.Alias, ch.AltAliases = alias.Alias, alias.AltAliases
	}

	ch.nameChecked = nameAbsent && aliasAbsent && ch.nameRev == nameRev && ch.aliasRev == aliasRev

	return errors.Join(lookupError(nameErr), lookupError(aliasErr))
}

// absentState reports whether a state lookup confirmed the state is not set: a 404, or
// successful empty content, which is what remains after the state was cleared.
func absentState(err error, empty bool) bool {
	return errors.Is(err, mautrix.MNotFound) || err == nil && empty
}

// lookupError returns err unless it is a 404, which means the state is not set.
func lookupError(err error) error {
	if errors.Is(err, mautrix.MNotFound) {
		return nil
	}

	return err
}
