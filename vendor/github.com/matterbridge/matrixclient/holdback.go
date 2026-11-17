package matrixclient

import (
	"cmp"
	"slices"
	"time"

	"maunium.net/go/mautrix/id"
)

const (
	// holdbackTimeout bounds how long newer events in a room wait behind a
	// still-undecrypted event. mautrix waits 3s before requesting keys and 22s
	// afterwards, so 5s covers normal key-fetch roundtrips without stalling a
	// room for 25s when the sender device is offline.
	holdbackTimeout = 5 * time.Second

	// maxHoldback caps queued events per room so a stalled event cannot cause
	// unbounded growth during a message burst.
	maxHoldback = 256
)

func (c *Client) closing() bool {
	return c.ctx != nil && c.ctx.Err() != nil
}

func (c *Client) ensureChannel(roomID id.RoomID) *Channel {
	c.RLock()
	ch, ok := c.channels[roomID]
	c.RUnlock()

	if ok {
		return ch
	}

	c.Lock()
	ch = c.channelLocked(roomID)
	c.Unlock()

	return ch
}

func (c *Client) stopHoldbacks() {
	c.RLock()
	defer c.RUnlock()

	for _, ch := range c.channels {
		ch.Lock()
		ch.stopHoldbackTimerLocked()
		ch.pending = nil
		ch.clearHeldLocked()
		ch.Unlock()
	}
}

func (ch *Channel) removePendingLocked(seq uint64) {
	if seq == 0 || len(ch.pending) == 0 {
		return
	}

	// Normal sync dispatch resolves the head of ch.pending in order.
	if ch.pending[0].seq == seq {
		ch.pending = ch.pending[1:]
		if len(ch.pending) == 0 {
			ch.pending = nil
		}

		return
	}

	idx, ok := slices.BinarySearchFunc(ch.pending, seq, func(p pendingSeq, target uint64) int {
		return cmp.Compare(p.seq, target)
	})
	if ok {
		ch.pending = slices.Delete(ch.pending, idx, idx+1)
	}
}

func (ch *Channel) consumeSeqLocked(eventID id.EventID) uint64 {
	if eventID == "" {
		return 0
	}

	p, ok := ch.eventSeq[eventID]
	if !ok {
		return 0
	}

	delete(ch.eventSeq, eventID)
	ch.removePendingLocked(p.seq)

	return p.seq
}

func (ch *Channel) expirePendingLocked(now time.Time, evictHead bool) {
	for len(ch.pending) > 0 {
		if now.Sub(ch.pending[0].at) >= holdbackTimeout {
			ch.pending = ch.pending[1:]

			continue
		}

		if evictHead && len(ch.held) > 0 && ch.pending[0].seq < ch.held[0].seq {
			ch.pending = ch.pending[1:]

			continue
		}

		break
	}

	if len(ch.pending) == 0 {
		ch.pending = nil
	}
}

func (ch *Channel) isBlockedLocked(seq uint64) bool {
	return seq > 0 && len(ch.pending) > 0 && ch.pending[0].seq < seq
}

func (ch *Channel) canDeliverNowLocked(seq uint64) bool {
	if ch.deliveringEvents || ch.isBlockedLocked(seq) {
		return false
	}

	return len(ch.held) == 0 || (seq > 0 && seq < ch.held[0].seq)
}

func (ch *Channel) insertHeldLocked(item heldEvent) {
	if len(ch.held) == 0 || item.seq == 0 {
		ch.held = append(ch.held, item)

		return
	}

	last := ch.held[len(ch.held)-1].seq
	if last > 0 && last <= item.seq {
		ch.held = append(ch.held, item)

		return
	}

	idx, _ := slices.BinarySearchFunc(ch.held, item.seq, func(h heldEvent, target uint64) int {
		if h.seq == 0 {
			return 1
		}

		return cmp.Compare(h.seq, target)
	})
	ch.held = slices.Insert(ch.held, idx, item)
}

func (ch *Channel) clearHeldLocked() {
	clear(ch.held)
	ch.held = nil
}

func (ch *Channel) stopHoldbackTimerLocked() {
	ch.holdbackGen++
	ch.holdbackDeadline = time.Time{}

	if ch.holdbackTimer != nil {
		ch.holdbackTimer.Stop()
		ch.holdbackTimer = nil
	}
}

func (ch *Channel) noteActivityLocked(eventID id.EventID, ts int64, seq uint64) {
	ch.LastActivity = max(ch.LastActivity, ts)

	// The timeline position, not the arrival or decryption order, decides the
	// newest event, as origin_server_ts is not monotonic either.
	if seq > ch.lastSeq {
		ch.lastSeq, ch.LastEventID = seq, eventID
	}
}

func (ch *Channel) noteDeliveredLocked(eventID id.EventID, seq uint64) {
	if seq > ch.deliveredSeq {
		ch.deliveredSeq, ch.DeliveredEventID = seq, eventID
	}
}

func (c *Client) updateHoldbackTimerLocked(ch *Channel, roomID id.RoomID, now time.Time) {
	if len(ch.held) == 0 || !ch.isBlockedLocked(ch.held[0].seq) || c.closing() {
		ch.stopHoldbackTimerLocked()

		return
	}

	deadline := ch.pending[0].at.Add(holdbackTimeout)
	if ch.holdbackTimer != nil && deadline.Equal(ch.holdbackDeadline) {
		return
	}

	ch.stopHoldbackTimerLocked()

	delay := max(deadline.Sub(now), time.Millisecond)
	gen := ch.holdbackGen

	ch.holdbackDeadline = deadline
	ch.holdbackTimer = time.AfterFunc(delay, func() {
		c.onHoldbackTimeout(roomID, gen)
	})
}

func (c *Client) deliverOrdered(roomID id.RoomID, eventID id.EventID, ts int64, trackActivity bool, payload any) {
	var ch *Channel

	if trackActivity {
		ch = c.ensureChannel(roomID)
	} else {
		c.RLock()
		ch = c.channels[roomID]
		c.RUnlock()
	}

	if ch == nil {
		c.invokeEventHandler(payload)

		return
	}

	ch.Lock()

	seq := ch.consumeSeqLocked(eventID)
	now := time.Now()
	ch.expirePendingLocked(now, false)

	if ch.canDeliverNowLocked(seq) {
		ch.deliveringEvents = true

		if trackActivity {
			ch.noteActivityLocked(eventID, ts, seq)
		}

		ch.Unlock()

		c.deliverOneAndDrain(ch, roomID, eventID, seq, trackActivity, payload)

		return
	}

	ch.insertHeldLocked(heldEvent{
		seq:           seq,
		eventID:       eventID,
		ts:            ts,
		trackActivity: trackActivity,
		payload:       payload,
	})

	if len(ch.held) > maxHoldback {
		ch.expirePendingLocked(now, true)
	}

	if !ch.deliveringEvents && len(ch.held) > 0 && !ch.isBlockedLocked(ch.held[0].seq) {
		ch.deliveringEvents = true
		c.drainHeldLocked(ch, roomID)

		return
	}

	c.updateHoldbackTimerLocked(ch, roomID, now)
	ch.Unlock()
}

func (c *Client) deliverOneAndDrain(ch *Channel, roomID id.RoomID, eventID id.EventID, seq uint64, trackActivity bool, payload any) {
	if !c.closing() {
		c.invokeEventHandler(payload)
	}

	ch.Lock()

	if trackActivity && seq > 0 {
		ch.noteDeliveredLocked(eventID, seq)
	}

	c.drainHeldLocked(ch, roomID)
}

// drainHeldLocked delivers ready events from ch.held in sequence order until the
// queue is empty or its head is blocked behind an earlier pending event.
// Caller must hold ch.Lock() with ch.deliveringEvents == true; drainHeldLocked
// always releases ch.Lock() before returning.
func (c *Client) drainHeldLocked(ch *Channel, roomID id.RoomID) {
	for {
		now := time.Now()
		ch.expirePendingLocked(now, false)

		if c.closing() {
			ch.stopHoldbackTimerLocked()
			ch.clearHeldLocked()
			ch.deliveringEvents = false
			ch.Unlock()

			return
		}

		if len(ch.held) == 0 || ch.isBlockedLocked(ch.held[0].seq) {
			c.updateHoldbackTimerLocked(ch, roomID, now)
			ch.deliveringEvents = false
			ch.Unlock()

			return
		}

		next := ch.held[0]
		ch.held[0] = heldEvent{}

		ch.held = ch.held[1:]
		if len(ch.held) == 0 {
			ch.held = nil
		}

		if next.trackActivity {
			ch.noteActivityLocked(next.eventID, next.ts, next.seq)
		}

		ch.Unlock()

		c.invokeEventHandler(next.payload)

		ch.Lock()

		if next.trackActivity && next.seq > 0 {
			ch.noteDeliveredLocked(next.eventID, next.seq)
		}
	}
}

func (c *Client) resolvePending(roomID id.RoomID, eventID id.EventID) {
	if eventID == "" {
		return
	}

	c.RLock()
	ch := c.channels[roomID]
	c.RUnlock()

	if ch == nil {
		return
	}

	ch.Lock()
	ch.consumeSeqLocked(eventID)

	if ch.deliveringEvents || len(ch.held) == 0 {
		ch.Unlock()

		return
	}

	now := time.Now()
	ch.expirePendingLocked(now, false)

	if ch.isBlockedLocked(ch.held[0].seq) {
		c.updateHoldbackTimerLocked(ch, roomID, now)
		ch.Unlock()

		return
	}

	ch.deliveringEvents = true
	c.drainHeldLocked(ch, roomID)
}

func (c *Client) onHoldbackTimeout(roomID id.RoomID, gen uint64) {
	if c.closing() {
		return
	}

	c.RLock()
	ch := c.channels[roomID]
	c.RUnlock()

	if ch == nil {
		return
	}

	ch.Lock()

	if ch.holdbackGen != gen {
		ch.Unlock()

		return
	}

	ch.holdbackTimer = nil
	ch.holdbackDeadline = time.Time{}

	now := time.Now()
	ch.expirePendingLocked(now, false)

	if ch.deliveringEvents || len(ch.held) == 0 || ch.isBlockedLocked(ch.held[0].seq) {
		c.updateHoldbackTimerLocked(ch, roomID, now)
		ch.Unlock()

		return
	}

	ch.deliveringEvents = true
	c.drainHeldLocked(ch, roomID)
}

func (c *Client) invokeEventHandler(payload any) {
	switch e := payload.(type) {
	case *Message:
		c.handler.OnMessage(e)
	case *Edit:
		if h, ok := c.handler.(EditHandler); ok {
			h.OnEdit(e)
		}
	case *Reaction:
		c.handler.OnReaction(e)
	case *Redaction:
		if h, ok := c.handler.(RedactionHandler); ok {
			h.OnRedaction(e)
		}
	}
}
