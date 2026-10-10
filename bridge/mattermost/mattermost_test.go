package mattermost

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matterbridge/matterclient"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
)

func init() {
	// New() normally creates the logger.
	l := logrus.New()
	l.SetOutput(io.Discard)
	logger = logrus.NewEntry(l)

	loggerOnce.Do(func() {})
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

// newTestBridge returns a bridge that is not connected to anything, with an event
// channel that nobody reads from (like the event loop after it has stopped).
func newTestBridge(t *testing.T) *Mattermost {
	t.Helper()

	m := &Mattermost{
		mc:        matterclient.New("user", "pass", "team", "localhost", ""),
		cfg:       testConfig(t),
		eventChan: make(chan *bridge.Event),
		done:      make(chan struct{}),
	}
	m.connected.Store(true)

	return m
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

func setLogoutEventTimeout(t *testing.T, d time.Duration) {
	t.Helper()

	old := logoutEventTimeout
	logoutEventTimeout = d

	t.Cleanup(func() { logoutEventTimeout = old })
}

// wsEvent decodes an event the way the websocket client does it.
func wsEvent(t *testing.T, raw string) *model.WebSocketEvent {
	t.Helper()

	ev, err := model.WebSocketEventFromJSON(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("unable to decode %q: %v", raw, err)
	}

	return ev
}

func TestLogoutDoesNotWaitForeverForTheEventLoop(t *testing.T) {
	setLogoutEventTimeout(t, 50*time.Millisecond)

	m := newTestBridge(t)

	// the event loop is gone, but the context of the user is still alive
	within(t, 2*time.Second, "Logout", func() {
		assert.NoError(t, m.Logout(context.Background()))
	})

	assert.False(t, m.Connected())
}

func TestLogoutDoesNotWaitWhenUserIsGone(t *testing.T) {
	// the timeout isn't what makes Logout return here
	setLogoutEventTimeout(t, time.Minute)

	m := newTestBridge(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	within(t, 2*time.Second, "Logout", func() {
		assert.NoError(t, m.Logout(ctx))
	})
}

func TestLogoutTellsTheEventLoop(t *testing.T) {
	m := newTestBridge(t)

	got := make(chan *bridge.Event, 1)

	go func() {
		got <- <-m.eventChan
	}()

	within(t, 2*time.Second, "Logout", func() {
		assert.NoError(t, m.Logout(context.Background()))
	})

	select {
	case ev := <-got:
		assert.Equal(t, "logout", ev.Type)
		assert.IsType(t, &bridge.LogoutEvent{}, ev.Data)
	case <-time.After(2 * time.Second):
		t.Fatal("the event loop didn't get the logout event")
	}
}

func TestLogoutOnlyRunsOnce(t *testing.T) {
	setLogoutEventTimeout(t, 50*time.Millisecond)

	m := newTestBridge(t)

	var unregistered atomic.Int32

	m.unregisterReloadHook = func() { unregistered.Add(1) }
	m.quitChan = []chan struct{}{make(chan struct{})}

	// the second logout (Quit after the logout command) must neither panic on
	// the channels that are closed already, nor wait for the event loop again.
	within(t, 2*time.Second, "Logout", func() {
		assert.NoError(t, m.Logout(context.Background()))
		assert.NoError(t, m.Logout(context.Background()))
	})

	assert.EqualValues(t, 1, unregistered.Load(), "the reload hook has to be removed, once")
}

func TestLogoutReleasesBlockedSenders(t *testing.T) {
	m := newTestBridge(t)

	// websocket workers that are in the middle of handing over an event
	var wg sync.WaitGroup

	for range 5 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			m.sendEvent(&bridge.Event{Type: "typing"})
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.NoError(t, m.Logout(ctx))

	within(t, 2*time.Second, "sendEvent", wg.Wait)
}

func TestSendEventDelivers(t *testing.T) {
	m := newTestBridge(t)

	go m.sendEvent(&bridge.Event{Type: "typing"})

	select {
	case ev := <-m.eventChan:
		assert.Equal(t, "typing", ev.Type)
	case <-time.After(2 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestLogoutContextSurvivesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	logoutCtx, logoutCancel := logoutContext(ctx, 3)
	defer logoutCancel()

	// the request that invalidates the session must still be possible ...
	assert.NoError(t, logoutCtx.Err())

	// ... but not forever.
	deadline, ok := logoutCtx.Deadline()
	assert.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(3*time.Second), deadline, time.Second)

	_, defCancel := logoutContext(ctx, 0)
	defer defCancel()
}

func TestWsWorkerStops(t *testing.T) {
	t.Run("on logout", func(t *testing.T) {
		m := newTestBridge(t)
		quit := make(chan struct{})

		stopped := make(chan struct{})

		go func() {
			defer close(stopped)

			m.handleWsMessage(context.Background(), quit, logger)
		}()

		close(quit)

		within(t, 2*time.Second, "worker", func() { <-stopped })
	})

	// e.g. a login that nobody logs out of anymore
	t.Run("on context done", func(t *testing.T) {
		m := newTestBridge(t)
		ctx, cancel := context.WithCancel(context.Background())

		stopped := make(chan struct{})

		go func() {
			defer close(stopped)

			m.handleWsMessage(ctx, make(chan struct{}), logger)
		}()

		cancel()

		within(t, 2*time.Second, "worker", func() { <-stopped })
	})
}

func TestWsWorkerSurvivesPanic(t *testing.T) {
	m := newTestBridge(t)
	// the config is gone, so handling a post panics
	m.cfg = nil

	post := wsEvent(t, `{"event":"posted","data":{"post":"{\"id\":\"p1\",\"user_id\":\"u1\",\"channel_id\":\"c1\"}"},"broadcast":{"channel_id":"c1"}}`)
	assert.Panics(t, func() { m.handleWsActionPost(context.Background(), post, logger) }, "the event has to make the handler panic")

	assert.NotPanics(t, func() {
		m.processWsMessage(context.Background(), &matterclient.Message{Raw: post}, logger)
	})

	// and the worker goes on with the next message
	quit := make(chan struct{})
	stopped := make(chan struct{})

	m.mc.MessageChan <- &matterclient.Message{Raw: post}
	m.mc.MessageChan <- &matterclient.Message{Raw: post}

	go func() {
		defer close(stopped)

		m.handleWsMessage(context.Background(), quit, logger)
	}()

	assert.Eventually(t, func() bool { return len(m.mc.MessageChan) == 0 }, 2*time.Second, 5*time.Millisecond)

	close(quit)

	within(t, 2*time.Second, "worker", func() { <-stopped })
}

func TestWsMessageWithoutEvent(t *testing.T) {
	m := newTestBridge(t)

	assert.NotPanics(t, func() {
		m.processWsMessage(context.Background(), nil, logger)
		m.processWsMessage(context.Background(), &matterclient.Message{}, logger)
	})
}

// The server doesn't always send a broadcast with an event.
func TestEventsWithoutBroadcast(t *testing.T) {
	m := newTestBridge(t)
	m.eventChan = make(chan *bridge.Event, 4)

	events := map[string]func(*model.WebSocketEvent){
		"user_added": func(ev *model.WebSocketEvent) {
			m.handleWsActionUserAdded(context.Background(), ev, logger)
		},
		"user_removed": func(ev *model.WebSocketEvent) {
			m.handleWsActionUserRemoved(context.Background(), ev, logger)
		},
		"typing": func(ev *model.WebSocketEvent) {
			m.handleTypingEvent(context.Background(), ev, logger)
		},
	}

	for name, handle := range events {
		t.Run(name, func(t *testing.T) {
			ev := wsEvent(t, `{"event":"`+name+`","data":{"user_id":"u1","remover_id":"u2"}}`)
			assert.Nil(t, ev.GetBroadcast())

			assert.NotPanics(t, func() { handle(ev) })
		})
	}

	// an event for no channel is of no use to anyone
	assert.Empty(t, m.eventChan)
}

func TestBroadcastOf(t *testing.T) {
	assert.NotNil(t, broadcastOf(wsEvent(t, `{"event":"typing"}`)))
	assert.Equal(t, "c1", broadcastOf(wsEvent(t, `{"event":"typing","broadcast":{"channel_id":"c1"}}`)).ChannelId)
}

func TestPostListWithMissingPost(t *testing.T) {
	m := newTestBridge(t)

	list := &model.PostList{
		Order: []string{"gone"},
		Posts: map[string]*model.Post{},
	}

	for _, eventType := range []string{"replay", "scrollback", "details"} {
		assert.NotPanics(t, func() {
			assert.Empty(t, m.postListToEvents(context.Background(), list, eventType, 0))
		}, eventType)
	}
}

// Workers handle config changes concurrently.
func TestBannerChangeIsSentOnce(t *testing.T) {
	m := newTestBridge(t)

	const workers = 20

	m.eventChan = make(chan *bridge.Event, workers)

	ev := wsEvent(t, `{"event":"config_changed","data":{"config":{"EnableBanner":true,"BannerText":"maintenance"}}}`)

	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			m.handleConfigChangedEvent(ev, logger)
		}()
	}

	wg.Wait()

	assert.Len(t, m.eventChan, 1, "the same banner must only be announced once")

	// a different banner is announced again, the same one again after it was cleared
	m.handleConfigChangedEvent(wsEvent(t, `{"event":"config_changed","data":{"config":{"EnableBanner":true,"BannerText":"over"}}}`), logger)
	m.handleConfigChangedEvent(wsEvent(t, `{"event":"config_changed","data":{"config":{"EnableBanner":false,"BannerText":""}}}`), logger)
	m.handleConfigChangedEvent(wsEvent(t, `{"event":"config_changed","data":{"config":{"EnableBanner":true,"BannerText":"over"}}}`), logger)

	assert.Len(t, m.eventChan, 3)
}

func TestBridgeLogLevel(t *testing.T) {
	level := func(debug, trace bool) logrus.Level {
		return bridgeLogLevel(&config.RuntimeConfig{GlobalConfig: config.GlobalConfig{Debug: debug, Trace: trace}})
	}

	assert.Equal(t, logrus.InfoLevel, level(false, false))
	assert.Equal(t, logrus.DebugLevel, level(true, false))
	assert.Equal(t, logrus.TraceLevel, level(true, true))
	assert.Equal(t, logrus.TraceLevel, level(false, true))
}

// testConfigHTTP returns a config that talks plain http to the server.
func testConfigHTTP(t *testing.T, clientTimeout string) *config.Config {
	t.Helper()

	t.Setenv("MATTERIRCD_MATTERMOST_INSECURE", "true")

	if clientTimeout != "" {
		t.Setenv("MATTERIRCD_CLIENTTIMEOUT", clientTimeout)
	}

	return testConfig(t)
}

func TestGetServerVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Version-Id", "10.5.0.1234")
	}))
	defer srv.Close()

	cfg := testConfigHTTP(t, "")
	cred := bridge.Credentials{Server: strings.TrimPrefix(srv.URL, "http://")}

	assert.Equal(t, "10.5.0.1234", getServerVersion(context.Background(), cfg, cred))
}

func TestGetServerVersionServerNeverAnswers(t *testing.T) {
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	cred := bridge.Credentials{Server: strings.TrimPrefix(srv.URL, "http://")}

	t.Run("client timeout", func(t *testing.T) {
		cfg := testConfigHTTP(t, "1")

		start := time.Now()

		within(t, 5*time.Second, "getServerVersion", func() {
			assert.Empty(t, getServerVersion(context.Background(), cfg, cred))
		})

		assert.Less(t, time.Since(start), 4*time.Second)
	})

	t.Run("user is gone", func(t *testing.T) {
		cfg := testConfigHTTP(t, "")

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		// well before the default timeout of the client
		within(t, 2*time.Second, "getServerVersion", func() {
			assert.Empty(t, getServerVersion(ctx, cfg, cred))
		})
	})
}

// The very first login has no logger yet, a failing version check must not panic on that.
func TestNewWithUnreachableServer(t *testing.T) {
	oldLogger := logger

	logger = nil
	loggerOnce = sync.Once{}

	t.Cleanup(func() {
		logger = oldLogger
		loggerOnce = sync.Once{}
		loggerOnce.Do(func() {})
	})

	// nothing listens on this port
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	addr := l.Addr().String()
	l.Close()

	cfg := testConfigHTTP(t, "")

	within(t, 5*time.Second, "New", func() {
		var br bridge.Bridger

		assert.NotPanics(t, func() {
			br, _, err = New(context.Background(), cfg, bridge.Credentials{Server: addr}, make(chan *bridge.Event), nil)
		})

		assert.Error(t, err)
		assert.Nil(t, br)
	})

	assert.NotNil(t, logger)
}
