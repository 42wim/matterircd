package matrixclient

import (
	"testing"
	"time"
)

type testReconnectHandler struct {
	reconnects []time.Duration
}

func (h *testReconnectHandler) Ready()                      {}
func (h *testReconnectHandler) OnMessage(*Message)          {}
func (h *testReconnectHandler) OnReaction(*Reaction)        {}
func (h *testReconnectHandler) OnReconnect(d time.Duration) { h.reconnects = append(h.reconnects, d) }

func TestOnSyncedReconnect(t *testing.T) {
	h := &testReconnectHandler{}
	c := &Client{handler: h}
	c.connected.Store(true)

	// Initial sync should not notify ReconnectHandler.
	reconnected, downtime := c.onSynced()
	if reconnected || downtime != 0 {
		t.Fatalf("initial sync returned (%v, %v), want (false, 0)", reconnected, downtime)
	}

	// Healthy subsequent sync should not notify ReconnectHandler.
	reconnected, downtime = c.onSynced()
	if reconnected || downtime != 0 {
		t.Fatalf("healthy sync returned (%v, %v), want (false, 0)", reconnected, downtime)
	}

	// Simulate a disconnection that started 20 minutes after the last successful sync.
	c.lastSync.Store(time.Now().Add(-20 * time.Minute).UnixNano())
	c.connected.Store(false)

	reconnected, downtime = c.onSynced()
	if !reconnected {
		t.Fatal("expected reconnected=true after disconnected sync")
	}

	if downtime < 19*time.Minute || downtime > 21*time.Minute {
		t.Fatalf("unexpected downtime %v, want ~20m", downtime)
	}

	c.notifyReconnect(downtime)

	if len(h.reconnects) != 1 || h.reconnects[0] != downtime {
		t.Fatalf("got reconnects %v, want [%v]", h.reconnects, downtime)
	}
}
