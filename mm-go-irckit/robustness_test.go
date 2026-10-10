package irckit

import (
	"strings"
	"testing"
	"time"

	"github.com/sorcix/irc"
	"github.com/stretchr/testify/assert"

	"github.com/42wim/matterircd/bridge"
)

func serviceUser(nick string) *User {
	return &User{UserInfo: &bridge.UserInfo{Nick: nick, User: nick, Host: "service", Ghost: true}}
}

// A message to the login service that only consists of quotes parses into zero
// commands without an error. That used to index commands[0] and kill the daemon.
func TestHandleServiceBotEmptyCommand(t *testing.T) {
	for _, msg := range []string{`""`, `''`, `"" ""`} {
		u, fc := newTestUser(t, NewServer("test"))

		assert.NotPanics(t, func() {
			u.handleServiceBot("mattermost", serviceUser("mattermost"), msg)
		}, msg)

		// we get the usage hint instead of silence
		out := fc.sent()
		if assert.NotEmpty(t, out, msg) {
			assert.Contains(t, out[0].Trailing, "possible commands", msg)
		}
	}
}

func TestHandleServiceBotUnbalancedQuotes(t *testing.T) {
	u, fc := newTestUser(t, NewServer("test"))

	u.handleServiceBot("mattermost", serviceUser("mattermost"), `login "foo`)

	out := fc.sent()
	if assert.Len(t, out, 1) {
		assert.Contains(t, out[0].Trailing, "improperly formatted")
	}
}

// A panic in a handler (e.g. the bridge going away halfway through a command)
// must not take the whole process down.
func TestHandleServiceBotRecoversFromPanic(t *testing.T) {
	u, _ := newTestUser(t, NewServer("test"))
	// "lastsent" needs a bridge, this one panics on every call.
	u.br = &fakeBridge{}

	assert.NotPanics(t, func() {
		u.handleServiceBot("mattermost", serviceUser("mattermost"), "lastsent")
	})
}

// KICK was registered with MinParams 1 but reads Params[1].
func TestKickRequiresChannelAndNick(t *testing.T) {
	srv := NewServer("test")
	cmds := DefaultCommands()

	u, fc := newTestUser(t, srv)
	u.br = &fakeBridge{}

	var err error
	assert.NotPanics(t, func() {
		err = cmds.Run(srv, u, &irc.Message{Command: irc.KICK, Params: []string{"#channel"}})
	})
	assert.NoError(t, err)

	out := fc.sent()
	if assert.Len(t, out, 1) {
		assert.Equal(t, irc.ERR_NEEDMOREPARAMS, out[0].Command)
	}
}

// A PRIVMSG without a target (":text") has no Params. The paste buffer compared
// Params[0] of every pair of consecutive messages.
func TestDecodePrivmsgWithoutParams(t *testing.T) {
	u, fc := newTestUser(t, nil)
	fc.feed("PRIVMSG a :x", "PRIVMSG :y", "PRIVMSG a :z")

	go u.Decode()

	var got []string
	for range 3 {
		m := recvMsg(t, u.DecodeCh)
		got = append(got, m.Command+" "+strings.Join(m.Params, ",")+" "+m.Trailing)
	}

	assert.Equal(t, []string{"PRIVMSG a x", "PRIVMSG  y", "PRIVMSG a z"}, got)

	fc.Close()

	// Decode closes DecodeCh when the connection goes away
	select {
	case _, ok := <-u.DecodeCh:
		assert.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("DecodeCh was not closed")
	}
}

// Consecutive PRIVMSGs to the same target are still pasted together.
func TestDecodePasteBufferJoinsLines(t *testing.T) {
	u, fc := newTestUser(t, nil)
	u.br = &fakeBridge{}
	fc.feed("PRIVMSG a :one", "PRIVMSG a :two", "PRIVMSG b :three")

	go u.Decode()

	first := recvMsg(t, u.DecodeCh)
	assert.Equal(t, []string{"a"}, first.Params)
	assert.Equal(t, "one\ntwo", first.Trailing)

	second := recvMsg(t, u.DecodeCh)
	assert.Equal(t, []string{"b"}, second.Params)
	assert.Equal(t, "three", second.Trailing)
}

func TestStringInRegexp(t *testing.T) {
	assert.True(t, stringInRegexp("foobar", []string{"^nope", "^foo"}))
	assert.False(t, stringInRegexp("foobar", []string{"^nope"}))
	assert.False(t, stringInRegexp("foobar", nil))

	// Patterns come from the config file and aren't validated: an invalid one
	// must not take the daemon down, it just never matches.
	assert.NotPanics(t, func() {
		assert.False(t, stringInRegexp("foobar", []string{"(unclosed"}))
		assert.True(t, stringInRegexp("foobar", []string{"(unclosed", "bar$"}))
	})
}

func TestStringInRegexpCompilesOnce(t *testing.T) {
	const pattern = "^compiled-once-[0-9]+$"

	first := compileCached(pattern)
	if assert.NotNil(t, first) {
		assert.Same(t, first, compileCached(pattern))
	}

	assert.Nil(t, compileCached("(also invalid"))
	assert.Nil(t, compileCached("(also invalid"))
}

func setLastViewedDelay(t *testing.T, d time.Duration) {
	t.Helper()

	old := lastViewedMaxDelay
	lastViewedMaxDelay = d
	t.Cleanup(func() { lastViewedMaxDelay = old })
}

func TestUpdateLastViewedSendsUpdate(t *testing.T) {
	setLastViewedDelay(t, 20*time.Millisecond)

	u, _ := newTestUser(t, nil)
	fb := &fakeBridge{lastViewed: make(chan string, 2)}
	u.br = fb

	u.updateLastViewed("chan1")
	// repeated calls are rate limited
	u.updateLastViewed("chan1")

	select {
	case id := <-fb.lastViewed:
		assert.Equal(t, "chan1", id)
	case <-time.After(2 * time.Second):
		t.Fatal("update was never sent")
	}

	select {
	case id := <-fb.lastViewed:
		t.Fatalf("unexpected second update for %s", id)
	case <-time.After(100 * time.Millisecond):
	}
}

// updateLastViewed sleeps for a random time before telling the server. When the
// user logged out (br == nil) in the meantime that used to be a nil dereference.
func TestUpdateLastViewedAfterLogout(t *testing.T) {
	setLastViewedDelay(t, 20*time.Millisecond)

	u, _ := newTestUser(t, nil)
	fb := &fakeBridge{lastViewed: make(chan string, 1)}
	u.br = fb

	u.updateLastViewed("chan1")

	// the user logs out while the delayed update is pending
	u.eventLoopMutex.Lock()
	u.br = nil
	u.eventLoopMutex.Unlock()

	select {
	case id := <-fb.lastViewed:
		t.Fatalf("update for %s was sent after logging out", id)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestUpdateLastViewedWithoutBridge(t *testing.T) {
	setLastViewedDelay(t, 10*time.Millisecond)

	u, _ := newTestUser(t, nil)

	assert.NotPanics(t, func() {
		u.updateLastViewed("chan1")
		time.Sleep(100 * time.Millisecond)
	})
}

func TestUpdateLastViewedStopsOnDisconnect(t *testing.T) {
	setLastViewedDelay(t, time.Hour)

	u, _ := newTestUser(t, nil)
	fb := &fakeBridge{lastViewed: make(chan string, 1)}
	u.br = fb

	u.updateLastViewed("chan1")
	u.cancel()

	select {
	case <-fb.lastViewed:
		t.Fatal("update was sent after the user disconnected")
	case <-time.After(100 * time.Millisecond):
	}
}
