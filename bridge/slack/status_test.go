package slack

import (
	"context"
	"testing"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/matterbridge/slackclient"
	"github.com/slack-go/slack"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

func TestIsOnline(t *testing.T) {
	s := &Slack{}

	assert.True(t, s.IsOnline("online"))
	assert.True(t, s.IsOnline("active"))
	assert.True(t, s.IsOnline("online (Last activity just now, local time: 14:30)"))
	assert.True(t, s.IsOnline("active (Last activity just now)"))
	assert.False(t, s.IsOnline("online (Last activity just now): In a meeting"))
	assert.False(t, s.IsOnline("online: In a meeting"))
	assert.False(t, s.IsOnline("away"))
	assert.False(t, s.IsOnline("away (Last online 1 hr. ago)"))
	assert.False(t, s.IsOnline(""))
}

func TestNormalizeSlackPresence(t *testing.T) {
	assert.Equal(t, "online", normalizeSlackPresence("active"))
	assert.Equal(t, "away", normalizeSlackPresence("away"))
	assert.Empty(t, normalizeSlackPresence(""))
}

func TestFormatSlackUserStatus(t *testing.T) {
	now := time.Now().Unix()

	assert.Equal(t, "online", formatSlackUserStatus("online", "", "", 0))
	assert.Equal(t, "online (Last activity just now, local time: 14:30)",
		formatSlackUserStatus("online", "14:30", "", now-5))
	assert.Equal(t, "online (Last activity just now, local time: 14:30): :calendar: In a meeting",
		formatSlackUserStatus("online", "14:30", ":calendar: In a meeting", now-5))

	// Away users omit Last online if activity was < 20 minutes ago.
	assert.Equal(t, "away (local time: 09:15)",
		formatSlackUserStatus("away", "09:15", "", now-10*60))

	// Away users include Last online if activity was >= 20 minutes ago.
	assert.Equal(t, "away (Last online 1 hr. ago, local time: 09:15): Lunch",
		formatSlackUserStatus("away", "09:15", "Lunch", now-3600))
}

func TestSlackClientStatusHelpers(t *testing.T) {
	sc := slackclient.New(slackclient.Credentials{})
	future := int(time.Now().Add(time.Hour).Unix())
	past := int(time.Now().Add(-time.Hour).Unix())

	sc.UpdateUser(slack.User{
		ID: "U1",
		TZ: "UTC",
		Profile: slack.UserProfile{
			StatusEmoji:      ":coffee:",
			StatusText:       "Coffee break",
			StatusExpiration: future,
		},
	})
	sc.UpdateUser(slack.User{
		ID:       "U2",
		TZLabel:  "AEST",
		TZOffset: 10 * 3600,
		Profile: slack.UserProfile{
			StatusEmoji:      ":palm_tree:",
			StatusText:       "Expired",
			StatusExpiration: past,
		},
	})
	sc.UpdatePresence("U1", "active")
	sc.UpdatePresence("U2", "away")

	assert.Contains(t, sc.GetUserCustomStatus("U1"), ":coffee: Coffee break (Until ")
	assert.Empty(t, sc.GetUserCustomStatus("U2"))

	assert.Equal(t, time.Now().UTC().Format("15:04"), sc.GetUserLocalTime("U1"))
	assert.Equal(t, time.Now().In(time.FixedZone("AEST", 10*3600)).Format("15:04"), sc.GetUserLocalTime("U2"))

	statuses := sc.GetStatuses()
	assert.Contains(t, statuses["U1"], "online: :coffee: Coffee break (Until ")
	assert.Equal(t, "away", statuses["U2"])

	up, err := sc.GetUserPresence(context.Background(), "U1")
	if err != nil {
		t.Fatalf("GetUserPresence failed: %v", err)
	}

	if up == nil {
		t.Fatal("expected non-nil UserPresence")
	}

	assert.Equal(t, "active", up.Presence)
	assert.True(t, up.Online)
	assert.NotZero(t, up.LastActivity)
}

func TestBuildSlackUserInfo(t *testing.T) {
	u := &slack.User{
		ID:       "U123",
		RealName: "Jane Doe",
		IsBot:    true,
		Profile: slack.UserProfile{
			DisplayName: "jane",
			Email:       "jane@example.com",
			Title:       "Staff Engineer",
			Image192:    "https://example.com/192.png",
			Image512:    "https://example.com/512.png",
		},
	}

	info := buildSlackUserInfo(u, "jane", "T1", "https://dir.example/people/jane", false)
	assert.Equal(t, "U123", info.User)
	assert.Equal(t, "bot", info.Roles)
	assert.Equal(t, "jane@example.com", info.Email)
	assert.Equal(t, "https://dir.example/people/jane", info.Directory)
	assert.Equal(t, "Staff Engineer", info.Position)
	assert.Equal(t, "https://example.com/512.png", info.Photo)
}

func TestSlackClientCountsAndLookups(t *testing.T) {
	sc := slackclient.New(slackclient.Credentials{})
	assert.Equal(t, 0, sc.GetUserCount())
	assert.Equal(t, 0, sc.GetChannelCount())

	err := sc.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error from Ping when disconnected")
	}

	err = sc.WsPing(context.Background())
	if err == nil {
		t.Fatal("expected error from WsPing when disconnected")
	}

	s := &Slack{sc: sc}

	err = s.Ping(context.Background(), "http")
	if err == nil {
		t.Fatal("expected error from Slack.Ping(http) when disconnected")
	}

	err = s.Ping(context.Background(), "ws")
	if err == nil {
		t.Fatal("expected error from Slack.Ping(ws) when disconnected")
	}

	sc.UpdateUser(slack.User{
		ID:   "U10",
		Name: "carol",
		Profile: slack.UserProfile{
			DisplayName: "CarolW",
		},
	})
	assert.Equal(t, 1, sc.GetUserCount())

	u := sc.GetUserByUsername("CAROL")
	if assert.NotNil(t, u) {
		assert.Equal(t, "U10", u.ID)
	}

	uByDisplay := sc.GetUserByUsername("carolw")
	if assert.NotNil(t, uByDisplay) {
		assert.Equal(t, "U10", uByDisplay.ID)
	}

	assert.Nil(t, sc.GetUserByUsername("nonexistent"))

	sc.UpdateChannel("C10", "general")
	assert.Equal(t, 1, sc.GetChannelCount())
	assert.Equal(t, "#general", sc.GetChannelName("C10"))

	sc.RemoveChannel("C10")
	assert.Equal(t, 0, sc.GetChannelCount())
}

func TestFormatSlackAttachmentFields(t *testing.T) {
	msg := &slack.Msg{
		Attachments: []slack.Attachment{
			{
				Title: "Alert",
				Fields: []slack.AttachmentField{
					{Title: "Env", Value: "prod", Short: true},
					{Title: "Host", Value: "web-01", Short: true},
					{Title: "Status", Value: "degraded", Short: true},
					{Title: "Details", Value: "high latency", Short: false},
				},
			},
		},
	}

	got, ok := formatSlackMsgOpts(msg, 25, false)
	assert.True(t, ok)
	assert.Equal(t, "> Env: prod | Host: web-01\n> Status: degraded\n> Details: high latency", got)

	gotOmit, ok := formatSlackMsgOpts(msg, 100, true)
	assert.True(t, ok)
	assert.Equal(t, "> prod | web-01 | degraded\n> high latency", gotOmit)
}

func TestSlackClientChannelMembershipCaches(t *testing.T) {
	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateUser(slack.User{ID: "U1", Name: "member1"})
	sc.UpdateUser(slack.User{ID: "U2", Name: "member2"})

	assert.False(t, sc.IsChannelMember("C1"))

	sc.SetChannelJoined("C1", true)
	assert.True(t, sc.IsChannelMember("C1"))

	sc.SetChannelJoined("C1", false)
	assert.False(t, sc.IsChannelMember("C1"))

	sc.UpdateChannel("C1", "general")
	sc.AddChannelMember("C1", "U1")
	sc.RemoveChannelMember("C1", "U1")

	sc.RemoveChannel("C1")
	assert.False(t, sc.IsChannelMember("C1"))
}

func TestSlackChannelToInfoAndRenameEvent(t *testing.T) {
	pub := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Name: "general",
			Conversation: slack.Conversation{
				ID:     "C1",
				Latest: &slack.Message{Msg: slack.Msg{Timestamp: "1700000000.000000"}},
			},
		},
	}

	pubInfo := slackChannelToInfo("T1", &pub)
	assert.Equal(t, "general", pubInfo.Name)
	assert.False(t, pubInfo.Private)
	assert.False(t, pubInfo.DM)
	assert.Equal(t, int64(1700000000000), pubInfo.LastPostAt)

	priv := slack.Channel{
		GroupConversation: slack.GroupConversation{
			Name: "secret",
			Conversation: slack.Conversation{
				ID:        "G1",
				IsPrivate: true,
			},
		},
	}

	privInfo := slackChannelToInfo("T1", &priv)
	assert.True(t, privInfo.Private)

	sc := slackclient.New(slackclient.Credentials{})
	evCh := make(chan *bridge.Event, 1)
	s := &Slack{sc: sc, eventChan: evCh}

	s.emitChannelUpdate("C1", "#renamed-chan")

	ev := <-evCh
	assert.Equal(t, "channel_update", ev.Type)

	upd, ok := ev.Data.(*bridge.ChannelUpdateEvent)
	assert.True(t, ok)
	assert.Equal(t, "C1", upd.ChannelID)
	assert.Equal(t, "renamed-chan", upd.Name)
	assert.Equal(t, "#renamed-chan", sc.GetChannelName("C1"))
}

func TestSlackDisconnectReconnectEvents(t *testing.T) {
	var (
		gotErr      error
		reconnected = make(chan struct{}, 1)
	)

	s := &Slack{
		onWsError: func(err error) {
			gotErr = err
		},
		onConnect: func() {
			reconnected <- struct{}{}
		},
	}

	// Intentional disconnect (e.g. logout) must not trigger reconnect notice.
	s.handleSlackEvent(context.Background(), slack.RTMEvent{
		Data: &slack.DisconnectedEvent{Intentional: true},
	})
	assert.False(t, s.wasDisconnected.Load())

	if gotErr != nil {
		t.Fatalf("unexpected error on intentional disconnect: %v", gotErr)
	}

	// Unintentional disconnect notifies onWsError and marks wasDisconnected.
	cause := context.DeadlineExceeded
	s.handleSlackEvent(context.Background(), slack.RTMEvent{
		Data: &slack.DisconnectedEvent{Intentional: false, Cause: cause},
	})
	assert.True(t, s.wasDisconnected.Load())
	assert.Equal(t, cause, gotErr)

	// Subsequent ConnectedEvent triggers onConnect/reconnect handling.
	s.handleSlackEvent(context.Background(), slack.RTMEvent{
		Data: &slack.ConnectedEvent{},
	})
	<-reconnected
	assert.False(t, s.wasDisconnected.Load())
}

func TestSlackFileAttachments(t *testing.T) {
	cfg, err := config.Load("", pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateUser(slack.User{ID: "U1", Name: "member1"})

	evCh := make(chan *bridge.Event, 2)
	s := &Slack{cfg: cfg, sc: sc, eventChan: evCh, userByNick: make(map[string]string)}

	msg := slack.Msg{
		Username:  "sender",
		Timestamp: "1700000000.000100",
		Files: []slack.File{
			{
				ID:                 "F1",
				Name:               "report.pdf",
				Size:               2048,
				URLPrivateDownload: "https://files.slack.com/files-pri/T1-F1/download/report.pdf",
			},
		},
	}

	ev := s.slackMsgToEvent(context.Background(), "C1", msg)
	if ev == nil {
		t.Fatal("expected non-nil event for file-only message")
	}

	chMsg, ok := ev.Data.(*bridge.ChannelMessageEvent)
	assert.True(t, ok)
	assert.Empty(t, chMsg.Text)
	assert.Len(t, chMsg.Files, 1)
	assert.Equal(t, "report.pdf", chMsg.Files[0].Name)
	assert.Equal(t, int64(2048), chMsg.Files[0].Size)
	assert.Equal(t, "https://files.slack.com/files-pri/T1-F1/download/report.pdf", chMsg.Files[0].URL)

	formatted, ok := formatSlackMsg(&slack.Msg{
		Text:  "See attached",
		Files: msg.Files,
	})
	assert.True(t, ok)
	assert.Equal(t, "See attached", formatted)

	sender := &bridge.UserInfo{Nick: "sender", Ghost: true}
	s.emitSlackFileEvent(sender, "C1", "msg1", "parent1", chMsg.Files)

	second := <-evCh
	assert.Equal(t, "file_event", second.Type)

	fe, ok := second.Data.(*bridge.FileEvent)
	assert.True(t, ok)
	assert.Equal(t, sender, fe.Sender)
	assert.Equal(t, "C1", fe.ChannelID)
	assert.Equal(t, "msg1", fe.MessageID)
	assert.Equal(t, "parent1", fe.ParentID)
	assert.Len(t, fe.Files, 1)
	assert.Equal(t, "report.pdf", fe.Files[0].Name)
	assert.Equal(t, "https://files.slack.com/files-pri/T1-F1/download/report.pdf", fe.Files[0].URL)
}
