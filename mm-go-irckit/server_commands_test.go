package irckit

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/sirupsen/logrus"
	"github.com/sorcix/irc"
	"github.com/stretchr/testify/assert"
	bolt "go.etcd.io/bbolt"
)

type modifyPostCall struct {
	msgID string
	text  string
}

type fakeModifyBridge struct {
	bridge.Bridger
	calls   []modifyPostCall
	deleted map[string]bool
}

func (f *fakeModifyBridge) Protocol() string { return "mattermost" }

func (f *fakeModifyBridge) ModifyPost(_ context.Context, msgID, text string) error {
	f.calls = append(f.calls, modifyPostCall{msgID, text})
	if f.deleted[msgID] {
		// Mattermost answers like this when patching a deleted post.
		return errors.New("You do not have the appropriate permissions.")
	}
	if text == "" {
		f.deleted[msgID] = true
	}

	return nil
}

func newModifyTestUser(t *testing.T) (*User, *fakeModifyBridge, <-chan string) {
	t.Helper()

	if logger == nil {
		SetLogger(logrus.NewEntry(logrus.New()))
	}

	db, err := bolt.Open(filepath.Join(t.TempDir(), "test.db"), 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	server, client := net.Pipe()
	t.Cleanup(func() { server.Close(); client.Close() })

	lines := make(chan string, 16)
	go func() {
		scanner := bufio.NewScanner(client)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	u := NewUserNet(server)
	u.Nick = "me"
	u.User = "me"
	u.lastViewedAtDB = db
	u.msgLast = make(map[string][2]string)
	u.msgMapIndex = make(map[string]map[int]string)

	br := &fakeModifyBridge{deleted: make(map[string]bool)}
	u.br = br

	return u, br, lines
}

func assertNotice(t *testing.T, lines <-chan string, want string) {
	t.Helper()

	select {
	case line := <-lines:
		assert.True(t, strings.Contains(line, want), line)
	case <-time.After(2 * time.Second):
		t.Fatalf("no notice containing %q sent to the user", want)
	}
}

func TestParseModifyMsgLastAfterDelete(t *testing.T) {
	u, br, lines := newModifyTestUser(t)
	const channelID = "channel"
	u.msgLast[channelID] = [2]string{"post1", ""}

	// s/!!/ deletes the last message.
	assert.True(t, parseModifyMsg(u, &irc.Message{Trailing: "s/!!/"}, channelID))
	assert.Equal(t, []modifyPostCall{{"post1", ""}}, br.calls)
	_, ok := u.msgLast[channelID]
	assert.False(t, ok, "deleted message must not stay the last message")

	// s/!!/my edit must not be sent as a literal message, the user is told instead.
	assert.True(t, parseModifyMsg(u, &irc.Message{Trailing: "s/!!/my edit"}, channelID))
	assert.Len(t, br.calls, 1)
	assertNotice(t, lines, "could not be modified")
}

func TestParseModifyMsgLastError(t *testing.T) {
	u, br, lines := newModifyTestUser(t)
	const channelID = "channel"
	u.msgLast[channelID] = [2]string{"post1", ""}
	br.deleted["post1"] = true

	// A permissions error for s// must not put the text through as a message.
	assert.True(t, parseModifyMsg(u, &irc.Message{Trailing: "s//my edit"}, channelID))
	assertNotice(t, lines, "could not be modified")
}

func TestParseModifyMsgExplicitIDPermissionsFallsThrough(t *testing.T) {
	u, br, _ := newModifyTestUser(t)
	const channelID = "channel"
	u.msgMapIndex[channelID] = map[int]string{0xadd: "post1"}
	br.deleted["post1"] = true

	// "s/add/..." could be plain text, keep putting it through as a message.
	assert.False(t, parseModifyMsg(u, &irc.Message{Trailing: "s/add/ more"}, channelID))
	_, ok := u.msgLast[channelID]
	assert.False(t, ok)
}

func TestParseModifyMsgExplicitIDEdit(t *testing.T) {
	u, br, _ := newModifyTestUser(t)
	const channelID = "channel"
	u.msgMapIndex[channelID] = map[int]string{0x004: "post4"}

	assert.True(t, parseModifyMsg(u, &irc.Message{Trailing: "s/004/fixed"}, channelID))
	assert.Equal(t, []modifyPostCall{{"post4", "fixed"}}, br.calls)
	assert.Equal(t, [2]string{"post4", ""}, u.msgLast[channelID])
}
