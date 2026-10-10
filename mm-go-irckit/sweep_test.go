package irckit

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/sorcix/irc"
	"github.com/stretchr/testify/assert"

	"github.com/42wim/matterircd/bridge"
)

func newGhost(name string) *User {
	g := NewUser(nil)
	g.UserInfo = &bridge.UserInfo{Nick: name, User: name, Host: "ghost", Ghost: true}

	return g
}

// newRealUser returns a user with a connection (fakeConn) and a distinct name.
func newRealUser(t *testing.T, srv Server, name string) (*User, *fakeConn) {
	t.Helper()

	u, fc := newTestUser(t, srv)
	u.Nick = name
	u.User = name

	return u, fc
}

// newAliasedChannel returns a channel that the server knows by its id and its name,
// like the ones that Server.Channel returns.
func newAliasedChannel(s *server, id, name string) Channel {
	ch := NewChannel(s, id, name, testService, nil)

	s.Lock()
	s.channels[id] = ch
	s.channels[name] = ch
	s.Unlock()

	return ch
}

func newTestServer(t *testing.T) *server {
	t.Helper()

	s, ok := NewServer("test").(*server)
	if !ok {
		t.Fatal("not a *server")
	}

	return s
}

func mustJoin(t *testing.T, ch Channel, users ...*User) {
	t.Helper()

	for _, u := range users {
		noError(t, ch.Join(u), "join "+u.Nick)
	}
}

func isCanceled(u *User) bool {
	select {
	case <-u.ctx.Done():
		return true
	default:
		return false
	}
}

func hasChannel(u *User, ch Channel) bool {
	u.RLock()
	defer u.RUnlock()

	_, ok := u.channels[ch]

	return ok
}

func commandsOf(msgs []*irc.Message, command string) []*irc.Message {
	var found []*irc.Message

	for _, m := range msgs {
		if m.Command == command {
			found = append(found, m)
		}
	}

	return found
}

// Only real users get to see a part, ghosts have nobody to tell.
func TestPartNotifiesRealUsers(t *testing.T) {
	srv := NewServer("test")
	ch := srv.Channel("#room")

	alice, aliceConn := newRealUser(t, srv, "alice")
	bob, bobConn := newRealUser(t, srv, "bob")
	ghost := newGhost("ghost")

	mustJoin(t, ch, alice, bob, ghost)
	assert.Equal(t, 3, ch.Len())

	ch.Part(alice, "bye")

	assert.Equal(t, 2, ch.Len())
	assert.False(t, ch.HasUser(alice))
	assert.False(t, hasChannel(alice, ch))

	for i, conn := range []*fakeConn{aliceConn, bobConn} {
		parts := commandsOf(conn.sent(), irc.PART)
		if assert.Len(t, parts, 1, "connection %d", i) {
			assert.Equal(t, alice.Nick, parts[0].Name, "connection %d", i)
			assert.Equal(t, "bye", parts[0].Trailing, "connection %d", i)
		}
	}

	// a ghost leaving is seen by the real users as well
	ch.Part(ghost, "")

	assert.Len(t, commandsOf(bobConn.sent(), irc.PART), 2)
	assert.Len(t, commandsOf(aliceConn.sent(), irc.PART), 1, "alice left already")
}

// What is said in a channel is for the real users, ghosts don't have a connection.
func TestChannelMessagesReachRealUsers(t *testing.T) {
	srv := NewServer("test")
	ch := srv.Channel("#room")

	alice, aliceConn := newRealUser(t, srv, "alice")
	bob, bobConn := newRealUser(t, srv, "bob")
	ghost := newGhost("ghost")

	mustJoin(t, ch, alice, bob, ghost)

	// what came with joining isn't interesting here
	conns := []*fakeConn{aliceConn, bobConn}
	before := make([]int, len(conns))

	for i, conn := range conns {
		before[i] = len(conn.sent())
	}

	ch.Topic(ghost, "a new topic")
	ch.SpoofMessage("someone", "hello\nworld")
	ch.SpoofNotice("someone", "notice")
	ch.Message(alice, "from alice")

	for i, conn := range conns {
		sent := conn.sent()[before[i]:]

		assert.Len(t, commandsOf(sent, irc.TOPIC), 1, "connection %d", i)
		assert.Len(t, commandsOf(sent, irc.NOTICE), 1, "connection %d", i)

		var texts []string

		for _, m := range commandsOf(sent, irc.PRIVMSG) {
			texts = append(texts, m.Trailing)
		}

		assert.Equal(t, []string{"hello", "world", "from alice"}, texts, "connection %d", i)
	}
}

// Nothing is left behind when the last real user is gone: the channels that only
// have ghosts left are evicted and forgotten, channels with somebody in it are not.
func TestSweepEvictsChannelsWithoutRealUsers(t *testing.T) {
	srv := newTestServer(t)

	alice, _ := newRealUser(t, srv, "alice")
	g1, g2, g3 := newGhost("g1"), newGhost("g2"), newGhost("g3")

	srv.BatchAdd([]*User{g1, g2, g3})
	assert.True(t, srv.Add(alice))

	shared := newAliasedChannel(srv, "id-shared", "#shared")
	abandoned := newAliasedChannel(srv, "id-abandoned", "#abandoned")
	empty := newAliasedChannel(srv, "id-empty", "#empty")

	mustJoin(t, shared, alice, g1, g2)
	mustJoin(t, abandoned, g1, g3)

	assert.True(t, hasChannel(g1, abandoned))
	assert.True(t, hasChannel(g3, abandoned))

	srv.sweep(newGhost("quitter"))

	// channels with a real user stay as they were
	_, found := srv.HasChannel("id-shared")
	assert.True(t, found)

	_, found = srv.HasChannel("#shared")
	assert.True(t, found)

	assert.Equal(t, 3, shared.Len())
	assert.True(t, hasChannel(g1, shared))

	// ghost only channels, and empty ones, are gone under all their names
	for _, key := range []string{"id-abandoned", "#abandoned", "id-empty", "#empty"} {
		_, found = srv.HasChannel(key)
		assert.False(t, found, key)
	}

	assert.Equal(t, 0, abandoned.Len())
	assert.Equal(t, 0, empty.Len())
	assert.False(t, hasChannel(g1, abandoned))
	assert.False(t, hasChannel(g3, abandoned))

	// the ghost that is nowhere anymore is released, the other two still are in use
	assert.Len(t, srv.users, 3, "alice, g1 and g2")
	assert.True(t, isCanceled(g3), "the context of an orphaned ghost has to be released")
	assert.False(t, isCanceled(g1))

	// all of it a second time changes nothing
	srv.sweep(newGhost("quitter"))
	assert.Equal(t, 3, shared.Len())
}

// The server forgets everything about a user that quits.
func TestQuitEvictsEverythingWhenTheLastRealUserLeaves(t *testing.T) {
	srv := newTestServer(t)

	alice, _ := newRealUser(t, srv, "alice")
	assert.True(t, srv.Add(alice))

	ghosts := make([]*User, 0, 5)
	for i := range 5 {
		ghosts = append(ghosts, newGhost(fmt.Sprintf("ghost%d", i)))
	}

	srv.BatchAdd(ghosts)

	chans := make([]Channel, 0, 2)

	for _, name := range []string{"#one", "#two"} {
		ch := newAliasedChannel(srv, "id-"+name, name)
		chans = append(chans, ch)

		mustJoin(t, ch, alice)
		mustJoin(t, ch, ghosts...)
	}

	srv.Quit(alice, "bye")

	for _, ch := range chans {
		assert.Equal(t, 0, ch.Len(), ch.String())

		_, found := srv.HasChannel(ch.ID())
		assert.False(t, found, ch.String())
	}

	assert.Equal(t, 0, srv.ChannelCount())

	for _, g := range ghosts {
		assert.Empty(t, g.Channels())
	}

	assert.Equal(t, 0, srv.UserCount(), "ghosts that are nowhere anymore are dropped")
}

// Leaving a channel makes the (ghost) users that are left in it leave as well.
func TestCmdPartEvictsGhosts(t *testing.T) {
	srv := NewServer("test")

	alice, aliceConn := newRealUser(t, srv, "alice")
	alice.br = &fakeBridge{}

	ghosts := []*User{newGhost("g1"), newGhost("g2")}

	ch := srv.Channel("#room")
	mustJoin(t, ch, alice)
	mustJoin(t, ch, ghosts...)

	noError(t, CmdPart(srv, alice, &irc.Message{Command: irc.PART, Params: []string{"#room"}}), "PART")

	assert.Equal(t, 0, ch.Len())
	assert.Len(t, commandsOf(aliceConn.sent(), irc.PART), 1)

	for _, g := range ghosts {
		assert.False(t, hasChannel(g, ch))
	}
}

// Evicting the ghosts of a channel must not cost an allocation (a copy of all the other
// ghosts) for each ghost, which makes it quadratic: with a few thousand users in a channel
// that's gigabytes of garbage every time the last user leaves.
func TestEvictingGhostsDoesNotAllocatePerGhost(t *testing.T) {
	const ghosts = 2000

	srv := NewServer("test")
	ch := srv.Channel("#big")

	members := make([]*User, 0, ghosts)
	for i := range ghosts {
		members = append(members, newGhost(fmt.Sprintf("ghost%d", i)))
	}

	noError(t, ch.BatchJoin(members), "BatchJoin")
	assert.Equal(t, ghosts, ch.Len())

	var before, after runtime.MemStats

	runtime.GC()
	runtime.ReadMemStats(&before)

	assert.True(t, evictIfOnlyGhosts(ch))

	runtime.ReadMemStats(&after)

	assert.Equal(t, 0, ch.Len())
	assert.Less(t, after.Mallocs-before.Mallocs, uint64(ghosts/10), "allocations while evicting %d ghosts", ghosts)
	assert.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20), "bytes allocated while evicting %d ghosts", ghosts)
}

func TestEvictIfOnlyGhostsKeepsRealUsers(t *testing.T) {
	srv := NewServer("test")
	ch := srv.Channel("#room")

	alice, _ := newRealUser(t, srv, "alice")
	ghost := newGhost("ghost")

	mustJoin(t, ch, alice, ghost)

	assert.False(t, evictIfOnlyGhosts(ch))
	assert.Equal(t, 2, ch.Len())
	assert.True(t, hasChannel(ghost, ch))

	ch.Part(alice, "")
	assert.True(t, evictIfOnlyGhosts(ch))
	assert.Equal(t, 0, ch.Len())
	assert.False(t, hasChannel(ghost, ch))
}
