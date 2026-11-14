package slack

import (
	"context"
	"testing"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
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
				AuthorName: "Monitoring",
				AuthorLink: "https://mon.example",
				Title:      "Alert",
				TitleLink:  "https://mon.example/a/1",
				Fields: []slack.AttachmentField{
					{Title: "Env", Value: "prod", Short: true},
					{Title: "Host", Value: "web-01", Short: true},
					{Title: "Status", Value: "degraded", Short: true},
					{Title: "Details", Value: "high latency", Short: false},
				},
				ImageURL: "https://mon.example/chart.png",
			},
		},
	}

	got, ok := formatSlackMsgOpts(msg, 25, false)
	assert.True(t, ok)
	assert.Equal(t,
		"> Monitoring (https://mon.example)\n> Alert (https://mon.example/a/1)\n> Env: prod | Host: web-01\n> Status: degraded\n> Details: high latency\n> https://mon.example/chart.png",
		got)

	gotOmit, ok := formatSlackMsgOpts(msg, 100, true)
	assert.True(t, ok)
	assert.Equal(t,
		"> Monitoring (https://mon.example)\n> Alert (https://mon.example/a/1)\n> prod | web-01 | degraded\n> high latency\n> https://mon.example/chart.png",
		gotOmit)

	fallbackOnly, ok := formatSlackMsgOpts(&slack.Msg{
		Attachments: []slack.Attachment{{Fallback: "Fallback summary"}},
	}, 0, false)
	assert.True(t, ok)
	assert.Equal(t, "> Fallback summary", fallbackOnly)
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

func TestSlackParityFeatures(t *testing.T) {
	cfg, err := config.Load("", pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	assert.Equal(t, 15*time.Minute, cfg.Slack().JoinReplayDuration)

	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateUser(slack.User{
		ID:       "U1",
		Name:     "dana",
		RealName: "Dana Smith",
		TZ:       "UTC",
		Profile: slack.UserProfile{
			DisplayName: "DanaS",
			FirstName:   "Dana",
			LastName:    "Smith",
			Email:       "dana@example.com",
		},
	})

	// Partial UserProfileChangedEvent / UserStatusChangedEvent preserves existing top-level fields.
	sc.UpdateUser(slack.User{
		ID: "U1",
		Profile: slack.UserProfile{
			DisplayName: "DanaUpdated",
			StatusText:  "Working",
		},
	})

	merged := sc.GetUser("U1")
	if assert.NotNil(t, merged) {
		assert.Equal(t, "dana", merged.Name)
		assert.Equal(t, "UTC", merged.TZ)
		assert.Equal(t, "DanaUpdated", merged.Profile.DisplayName)
	}

	selfInfo := buildSlackUserInfo(merged, "DanaUpdated", "T1", "", true)
	assert.Equal(t, "dana", selfInfo.Username)
	assert.Contains(t, selfInfo.MentionKeys, "DanaUpdated")
	assert.Contains(t, selfInfo.MentionKeys, "U1")

	dirURL := utils.ExpandDirectoryURL(
		"https://dir.example/u/{username}?nick={nick}&email={email}&first={firstname}&last={lastname}",
		"DanaNew", "dana", "dana@example.com", "Dana", "Smith",
	)
	assert.Equal(t, "https://dir.example/u/dana?nick=DanaNew&email=dana%40example.com&first=Dana&last=Smith", dirURL)
}

func TestSlackDMAndHistoricalEvents(t *testing.T) {
	cfg, err := config.Load("", pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateUser(slack.User{ID: "U1", Name: "erin"})
	sc.CacheDMChannel("U1", "D12345678")

	evCh := make(chan *bridge.Event, 4)

	s, err := newTestSlackBridge(cfg, sc, evCh)
	if err != nil {
		t.Fatalf("newTestSlackBridge: %v", err)
	}

	otherID, ok := s.GetDMOtherUserID("D12345678", "UME")
	assert.True(t, ok)
	assert.Equal(t, "U1", otherID)

	dmUser := s.GetDMUser(context.Background(), "D12345678")
	if assert.NotNil(t, dmUser) {
		assert.Equal(t, "U1", dmUser.User)
		assert.Equal(t, "erin", dmUser.Nick)
	}

	rootEv := s.slackMsgToEvent(context.Background(), "C1", slack.Msg{
		User:      "U1",
		Timestamp: "1700000100.000100",
		Text:      "Root post <!here>",
	})
	if assert.NotNil(t, rootEv) {
		chEv, ok := rootEv.Data.(*bridge.ChannelMessageEvent)
		assert.True(t, ok)
		assert.Equal(t, "notice", chEv.MessageType)
	}

	replyEv := s.slackMsgToEvent(context.Background(), "C1", slack.Msg{
		User:            "U1",
		Timestamp:       "1700000101.000200",
		ThreadTimestamp: "1700000100.000100",
		Text:            "Reply body",
	})
	if assert.NotNil(t, replyEv) {
		chEv, ok := replyEv.Data.(*bridge.ChannelMessageEvent)
		assert.True(t, ok)
		assert.Equal(t, "Reply body (re @erin: Root post @here)", chEv.Text)
	}

	assert.Equal(t, "1700000100.000100",
		threadTSFromPermalink("https://team.slack.com/archives/C1/p1700000101000200?thread_ts=1700000100.000100&cid=C1"))
}

func TestSlackUserProfileAndStatusEvents(t *testing.T) {
	cfg, err := config.Load("", pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateUser(slack.User{ID: "U1", Name: "erin"})

	evCh := make(chan *bridge.Event, 4)

	s, err := newTestSlackBridge(cfg, sc, evCh)
	if err != nil {
		t.Fatalf("newTestSlackBridge: %v", err)
	}

	s.handleSlackEvent(context.Background(), slack.RTMEvent{
		Data: &slack.UserProfileChangedEvent{
			User: slack.User{ID: "U1", Profile: slack.UserProfile{DisplayName: "ErinNew"}},
		},
	})

	updEv := <-evCh
	assert.Equal(t, "user_update", updEv.Type)

	s.handleSlackEvent(context.Background(), slack.RTMEvent{
		Data: &slack.UserStatusChangedEvent{
			User: slack.User{ID: "U1", Profile: slack.UserProfile{StatusText: ""}},
		},
	})

	updEv2 := <-evCh
	assert.Equal(t, "user_update", updEv2.Type)

	statusEv := <-evCh
	assert.Equal(t, "status_change", statusEv.Type)

	// GetUserByUsername resolves dot-sanitized DisplayName/RealName and userByNick mappings.
	s.userByNick["Custom.Nick"] = "U1"

	byCustom := s.GetUserByUsername(context.Background(), "custom.nick")
	if assert.NotNil(t, byCustom) {
		assert.Equal(t, "U1", byCustom.User)
	}

	// Logout is idempotent and unregisters reload hooks.
	unregistered := 0
	s.unregisterReloadHook = func() { unregistered++ }
	_ = s.Logout(context.Background())
	_ = s.Logout(context.Background())

	assert.Equal(t, 1, unregistered)
}

func TestSlackBlocksListAndGetChannel(t *testing.T) {
	cfg, err := config.Load("", pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatalf("config.Load failed: %v", err)
	}

	sc := slackclient.New(slackclient.Credentials{})
	sc.UpdateChannel("C1", "general")
	sc.SetChannelJoined("C1", true)
	sc.UpdateTopic("C1", "line1\nline2")
	sc.UpdateChannel("D1", "D1")
	sc.SetChannelJoined("D1", true)

	evCh := make(chan *bridge.Event, 4)

	s, err := newTestSlackBridge(cfg, sc, evCh)
	if err != nil {
		t.Fatalf("newTestSlackBridge: %v", err)
	}

	// List skips IM/MPIM conversations.
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	assert.Equal(t, map[string]string{"#general": "line1 | line2"}, list)

	// GetChannel resolves #channel-name and rejects &service channels.
	chInfo, err := s.GetChannel(context.Background(), "#general")
	if err != nil {
		t.Fatalf("GetChannel(#general): %v", err)
	}

	assert.Equal(t, "C1", chInfo.ID)

	_, err = s.GetChannel(context.Background(), "&users")
	if err == nil {
		t.Fatal("expected error for &users")
	}

	// Block Kit fallback renders SectionBlock and RichTextBlock when Text is empty.
	headerTxt := slack.NewTextBlockObject(slack.PlainTextType, "Deploy Alert", false, false)
	secTxt := slack.NewTextBlockObject(slack.MarkdownType, "Service *api* deployed", false, false)
	rtBlock := slack.NewRichTextBlock("b1",
		slack.NewRichTextSection(
			slack.NewRichTextSectionTextElement("Triggered by ", nil),
			slack.NewRichTextSectionUserElement("U1", nil),
		),
	)

	msg := &slack.Msg{
		Blocks: slack.Blocks{
			BlockSet: []slack.Block{
				slack.NewHeaderBlock(headerTxt),
				slack.NewSectionBlock(secTxt, nil, nil),
				rtBlock,
			},
		},
	}

	rendered, ok := formatSlackMsg(msg)
	assert.True(t, ok)
	assert.Equal(t, "Deploy Alert\nService *api* deployed\nTriggered by <@U1>", rendered)
}

func newTestSlackBridge(cfg *config.Config, sc *slackclient.Client, evCh chan *bridge.Event) (*Slack, error) {
	rootCache, err := lru.New[string, string](64)
	if err != nil {
		return nil, err
	}

	parentCache, err := lru.New[string, slackCachedPost](64)
	if err != nil {
		return nil, err
	}

	return &Slack{
		cfg:                cfg,
		sc:                 sc,
		eventChan:          evCh,
		userByNick:         make(map[string]string),
		msgThreadRootCache: rootCache,
		msgParentCache:     parentCache,
	}, nil
}
