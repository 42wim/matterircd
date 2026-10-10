package irckit

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sorcix/irc"
	"github.com/stretchr/testify/assert"

	"github.com/42wim/matterircd/bridge"
)

func testDB(t *testing.T) *bolt.DB {
	t.Helper()

	db, err := bolt.Open(filepath.Join(t.TempDir(), "test.db"), 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatalf("unable to open test database: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// scriptedCommands is a Commands implementation that records what it runs.
type scriptedCommands struct {
	seen chan string
}

func (c *scriptedCommands) Add(Handler) {}

func (c *scriptedCommands) Run(_ Server, _ *User, msg *irc.Message) error {
	if msg.Command == "BOOM" {
		panic("bug in a command handler")
	}

	c.seen <- msg.Command

	return nil
}

// A panic in a command handler is confined to that one message.
func TestHandleSurvivesPanickingCommand(t *testing.T) {
	cmds := &scriptedCommands{seen: make(chan string, 4)}

	srv, ok := ServerConfig{Commands: cmds}.Server().(*server)
	if !ok {
		t.Fatal("not a *server")
	}

	u, _ := newTestUser(t, srv)

	go srv.handle(u)

	u.DecodeCh <- irc.ParseMessage("BOOM")

	u.DecodeCh <- irc.ParseMessage("PING 1")

	select {
	case cmd := <-cmds.seen:
		assert.Equal(t, irc.PING, cmd)
	case <-time.After(5 * time.Second):
		t.Fatal("the command after the panic was never handled")
	}

	close(u.DecodeCh)
}

// A client that sends more than the handshake tolerates, and keeps sending, must not
// leave Decode() blocked on a channel nobody reads anymore.
func TestConnectHandshakeFailureReleasesDecode(t *testing.T) {
	srv := NewServer("test")

	u, fc := newTestUser(t, srv)
	// not registered: neither a nick nor a user
	u.Nick, u.User = "", ""

	for range 3 * handshakeMsgTolerance {
		fc.feed("PING x")
	}

	err := srv.Connect(u)
	if !errors.Is(err, ErrHandshakeFailed) {
		t.Fatalf("Connect() = %v, want %v", err, ErrHandshakeFailed)
	}

	assert.True(t, fc.isClosed())

	// Decode() ends by quitting the user, which cancels its context.
	select {
	case <-u.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Decode() is stuck handing over a message nobody is going to read")
	}
}

// Logout hands an event to the event loop, and the loop is started with eventLoopMutex held.
func TestQuitDoesNotHoldEventLoopMutexWhileLoggingOut(t *testing.T) {
	srv := NewServer("test")
	u, _ := newTestUser(t, srv)

	fb := &fakeBridge{}
	fb.logoutHook = func(context.Context) {
		u.eventLoopMutex.Lock()
		u.eventLoopMutex.Unlock() //nolint:staticcheck
	}
	u.br = fb

	within(t, 5*time.Second, "Quit", func() { srv.Quit(u, "bye") })

	assert.EqualValues(t, 1, fb.logouts.Load())
	assert.Nil(t, u.br)

	// quitting twice is harmless
	srv.Quit(u, "bye again")
	assert.EqualValues(t, 1, fb.logouts.Load())
}

// The "logout" command used to log out twice, the second time nobody listened anymore.
func TestLogoutCommandLogsOutOnce(t *testing.T) {
	srv := NewServer("test")
	u, _ := newTestUser(t, srv)

	fb := &fakeBridge{}
	fb.logoutHook = func(context.Context) {
		u.eventLoopMutex.Lock()
		u.eventLoopMutex.Unlock() //nolint:staticcheck
	}
	u.br = fb

	within(t, 5*time.Second, "logout", func() { logout(u, serviceUser(), nil, testService) })

	assert.EqualValues(t, 1, fb.logouts.Load())
	assert.Nil(t, u.br)
	assert.False(t, u.inprogress.Load())

	// logging out when we're not logged in is not an error
	assert.NotPanics(t, func() { logout(u, serviceUser(), nil, testService) })
}

func TestLoginToUnknownProtocol(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))

	assert.NotPanics(t, func() {
		assert.Error(t, u.loginTo("nosuchprotocol"))
	})
	assert.Nil(t, u.br)
}

func TestCloseGhostUser(t *testing.T) {
	ghost := NewUser(nil)

	assert.NotPanics(t, func() {
		assert.NoError(t, ghost.Close())
	})
}

// runEventLoop runs the event loop of u in the background, the returned channel is
// closed when it ends.
func runEventLoop(u *User, events <-chan *bridge.Event) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		u.handleEventChan(events)
	}()

	return done
}

func TestEventLoopStopsOnLogoutEvent(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))
	events := make(chan *bridge.Event)

	done := runEventLoop(u, events)

	events <- &bridge.Event{Data: &bridge.LogoutEvent{}}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event loop is still running after a logout event")
	}
}

// Without this the event loop of a user that disconnected, while the bridge never
// delivered a logout event, would live (and keep the user alive) forever.
func TestEventLoopStopsWhenUserDisconnects(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))
	u.lastViewedAtDB = testDB(t)
	u.lastEventTime.Store(1234567)

	done := runEventLoop(u, make(chan *bridge.Event))

	u.cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event loop is still running after the user disconnected")
	}

	// the progress is saved on the way out
	assert.EqualValues(t, 1234567, u.getGlobalLastEventTime())
}

// One bad event (here: the bridge is gone, so u.br is nil) is not allowed to
// kill the process or to end the delivery of the events after it.
func TestEventLoopSurvivesPanickingEvent(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))
	events := make(chan *bridge.Event)

	done := runEventLoop(u, events)

	events <- &bridge.Event{Type: "channel_topic", Data: &bridge.ChannelTopicEvent{ChannelID: "c", UserID: "u", Text: "topic"}}

	events <- nil

	events <- &bridge.Event{Data: &bridge.LogoutEvent{}}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("event loop stopped handling events")
	}
}

func TestAddUsersToChannelsStopsWhenUserDisconnects(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// never gets a bridge
		u.addUsersToChannels()
	}()

	// let it get past the initial delay into the loop that waits for the bridge
	time.Sleep(700 * time.Millisecond)
	u.cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("addUsersToChannels keeps waiting for a bridge of a user that is gone")
	}
}

func TestIncomingLogLineRedactsCredentials(t *testing.T) {
	tests := []struct {
		line string
		want string
	}{
		{"PRIVMSG mattermost :login user secret", "<- PRIVMSG mattermost :login [redacted]"},
		{"PRIVMSG mattermost login user secret", "<- PRIVMSG mattermost :login [redacted]"},
		{"PRIVMSG slack :login token", "<- PRIVMSG slack :login [redacted]"},
		{"PRIVMSG mastodon :login", "<- PRIVMSG mastodon :login [redacted]"},
		{"PASS secret", "<- PASS [redacted]"},
		{"PRIVMSG #channel :hello", "<- PRIVMSG #channel :hello"},
		{"PRIVMSG mattermost :channel foo", "<- PRIVMSG mattermost :channel foo"},
		{"PRIVMSG :login", "<- PRIVMSG :login"},
	}

	for _, tt := range tests {
		got := incomingLogLine(irc.ParseMessage(tt.line))
		assert.Equal(t, tt.want, got, tt.line)
		assert.NotContains(t, got, "secret", tt.line)
	}
}
