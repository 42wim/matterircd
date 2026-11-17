package mattermost

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/matterbridge/matterclient"
	"github.com/mattermost/mattermost/server/public/model"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

const testChannelID = "#test"

func newTestMattermost(t *testing.T) *Mattermost {
	t.Helper()

	loggerOnce.Do(func() {
		logger = logrus.NewEntry(logrus.New())
	})

	cfgPath := filepath.Join(t.TempDir(), "matterircd.toml")

	cfg, err := config.Load(cfgPath, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatal(err)
	}

	return &Mattermost{
		mc: &matterclient.Client{
			User: &model.User{Username: "testuser", Nickname: "testuser"},
		},
		cfg: cfg,
	}
}

func newTestPost(id string, createAt int64) *model.Post {
	return &model.Post{
		Id:        id,
		ChannelId: testChannelID,
		UserId:    "user1",
		CreateAt:  createAt,
		Message:   "msg-" + id,
	}
}

func testMessageIDs(events []*bridge.Event) []string {
	ids := make([]string, 0, len(events))
	for _, ev := range events {
		data, ok := ev.Data.(*bridge.ChannelMessageEvent)
		if ok {
			ids = append(ids, data.MessageID)
		}
	}

	return ids
}

func TestPostListToEventsThreadRootReordering(t *testing.T) {
	m := newTestMattermost(t)

	const rootID = "root"

	postList := &model.PostList{
		// Mattermost API can place a thread root post at index 0 even when older.
		Order: []string{rootID, "p3", "p2"},
		Posts: map[string]*model.Post{
			rootID: newTestPost(rootID, 100),
			"p2":   newTestPost("p2", 200),
			"p3":   newTestPost("p3", 300),
		},
	}

	events := m.postListToEvents(context.Background(), postList, "replay", 50)
	assert.Equal(t, []string{rootID, "p2", "p3"}, testMessageIDs(events))
}

func TestPostListToEventsStableOnCreateAtTies(t *testing.T) {
	m := newTestMattermost(t)

	const count = 20

	order := make([]string, 0, count)
	posts := make(map[string]*model.Post, count)
	want := make([]string, 0, count)

	// Order is newest-first; slices.Backward processes from the tail forward.
	for i := count - 1; i >= 0; i-- {
		id := fmt.Sprintf("p%02d", i)
		order = append(order, id)
		posts[id] = newTestPost(id, 500)
	}

	for i := range count {
		want = append(want, fmt.Sprintf("p%02d", i))
	}

	postList := &model.PostList{
		Order: order,
		Posts: posts,
	}

	events := m.postListToEvents(context.Background(), postList, "scrollback", 0)
	assert.Equal(t, want, testMessageIDs(events))
}

func TestPostListToEventsNilAndMissingPosts(t *testing.T) {
	m := newTestMattermost(t)

	const (
		id100 = "p100"
		id200 = "p200"
		id300 = "p300"
	)

	postList := &model.PostList{
		Order: []string{id100, "missing", "explicit_nil", id300, id200},
		Posts: map[string]*model.Post{
			id100:          newTestPost(id100, 100),
			"explicit_nil": nil,
			id200:          newTestPost(id200, 200),
			id300:          newTestPost(id300, 300),
		},
	}

	events := m.postListToEvents(context.Background(), postList, "replay", 50)
	assert.Equal(t, []string{id100, id200, id300}, testMessageIDs(events))

	assert.Nil(t, m.postListToEvents(context.Background(), nil, "replay", 0))
	assert.Empty(t, m.postListToEvents(context.Background(), &model.PostList{Order: []string{"missing"}}, "replay", 0))
}
