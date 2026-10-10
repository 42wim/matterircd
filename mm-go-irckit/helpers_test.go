package irckit

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/sorcix/irc"
	"github.com/spf13/pflag"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
)

func init() {
	// main() normally installs the logger.
	l := logrus.New()
	l.SetOutput(io.Discard)
	logger = logrus.NewEntry(l)
}

// fakeConn is an in-memory Conn. Decode returns the scripted messages and then
// blocks until Close (like a socket that is still open), Encode records what
// the server sent to the client.
type fakeConn struct {
	in     chan *irc.Message
	closed chan struct{}
	once   sync.Once

	mu  sync.Mutex
	out []*irc.Message
}

func newFakeConn() *fakeConn {
	return &fakeConn{
		in:     make(chan *irc.Message, 64),
		closed: make(chan struct{}),
	}
}

func (c *fakeConn) feed(lines ...string) {
	for _, line := range lines {
		c.in <- irc.ParseMessage(line)
	}
}

func (c *fakeConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeConn) Encode(m *irc.Message) error {
	c.mu.Lock()
	c.out = append(c.out, m)
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) Decode() (*irc.Message, error) {
	select {
	case m := <-c.in:
		return m, nil
	default:
	}

	select {
	case m := <-c.in:
		return m, nil
	case <-c.closed:
		return nil, io.EOF
	}
}

func (c *fakeConn) ResolveHost() string { return "127.0.0.1" }

func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

func (c *fakeConn) sent() []*irc.Message {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]*irc.Message(nil), c.out...)
}

func (c *fakeConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// fakeBridge is a bridge.Bridger that only implements what the tests need,
// calling anything else panics (nil embedded interface).
type fakeBridge struct {
	bridge.Bridger

	logouts     atomic.Int32
	logoutHook  func(ctx context.Context)
	lastViewed  chan string
	postSizeMax int
}

func (b *fakeBridge) Logout(ctx context.Context) error {
	b.logouts.Add(1)
	if b.logoutHook != nil {
		b.logoutHook(ctx)
	}

	return nil
}

func (b *fakeBridge) Protocol() string { return "mattermost" }

func (b *fakeBridge) Connected() bool { return true }

func (b *fakeBridge) GetPostSizeLimit() int {
	if b.postSizeMax == 0 {
		return 16383
	}

	return b.postSizeMax
}

func (b *fakeBridge) UpdateLastViewed(_ context.Context, channelID string) {
	if b.lastViewed != nil {
		b.lastViewed <- channelID
	}
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()

	// A config file that doesn't exist yields the default configuration.
	cfg, err := config.Load(filepath.Join(t.TempDir(), "matterircd.toml"), pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("unable to load default config: %v", err)
	}

	return cfg
}

// newTestUser returns a user (without a bridge) that talks over a fakeConn.
func newTestUser(t *testing.T, srv Server) (*User, *fakeConn) {
	t.Helper()

	fc := newFakeConn()
	u := NewUser(fc)
	u.cfg = testConfig(t)
	u.Srv = srv
	u.Nick = "tester"
	u.User = "tester"
	u.updateCounter = map[string]time.Time{}
	u.eventChan = make(chan *bridge.Event, 16)

	t.Cleanup(func() {
		u.cancel()
		fc.Close()
	})

	return u, fc
}

// recvMsg reads one message from ch, failing the test if nothing arrives in time.
func recvMsg(t *testing.T, ch <-chan *irc.Message) *irc.Message {
	t.Helper()

	select {
	case m, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, expected a message")
		}

		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a message")
	}

	return nil
}

// within fails the test unless fn returns within d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()

	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s", what, d)
	}
}
