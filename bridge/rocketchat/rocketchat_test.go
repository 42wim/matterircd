package rocketchat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/gorilla/websocket"
	"github.com/matterbridge/rocketchatclient"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
)

const (
	testFieldMsg      = "msg"
	testFieldID       = "_id"
	testFieldRID      = "rid"
	testFieldUsername = "username"
	testFieldName     = "name"
	testFieldSuccess  = "success"
	testFieldActive   = "active"
	testFieldRoles    = "roles"
	testUserMeID      = "uMe"
	testUserMeNick    = "hloeung"
	testUserMeName    = "Haw Loeung"
	testUserAliceID   = "uAlice"
	testUserAlice     = "alice"
	testGenRoomID     = "GEN12345678901234"
	testDMRoomID      = "DM123456789012345"
	testTS1           = "2026-11-16T01:00:00Z"
	testReportPDF     = "report.pdf"
)

func loadTestConfig(t *testing.T, tomlContent string) *config.Config {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "matterircd.toml")

	err := os.WriteFile(cfgPath, []byte(tomlContent), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := config.Load(cfgPath, pflag.NewFlagSet("test", pflag.ContinueOnError))
	if err != nil {
		t.Fatal(err)
	}

	return cfg
}

func writeJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")

	err := json.NewEncoder(w).Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
}

func serveMockDDP(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	for {
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			return
		}

		var frame struct {
			Msg  string   `json:"msg"`
			ID   string   `json:"id"`
			Subs []string `json:"subs"`
		}

		if json.Unmarshal(raw, &frame) != nil {
			continue
		}

		switch frame.Msg {
		case "connect":
			_ = conn.WriteJSON(map[string]any{testFieldMsg: "connected", "session": "s1"})
		case "method":
			_ = conn.WriteJSON(map[string]any{testFieldMsg: "result", "id": frame.ID, "result": map[string]any{"id": testUserMeID}})
		case "sub":
			_ = conn.WriteJSON(map[string]any{testFieldMsg: "ready", "subs": []string{frame.ID}})
		case "ping":
			_ = conn.WriteJSON(map[string]any{testFieldMsg: "pong", "id": frame.ID})
		}
	}
}

func writeHydrateResponse(t *testing.T, w http.ResponseWriter, path string) bool {
	t.Helper()

	switch path {
	case "/api/v1/me":
		writeJSON(t, w, map[string]any{
			testFieldID:       testUserMeID,
			testFieldUsername: testUserMeNick,
			testFieldName:     testUserMeName,
			"emails":          []map[string]any{{"address": "haw@example.com", "verified": true}},
			testFieldActive:   true,
			testFieldSuccess:  true,
		})
	case "/api/v1/subscriptions.get":
		writeJSON(t, w, map[string]any{
			testFieldSuccess: true,
			"update": []map[string]any{
				{testFieldID: "sub1", testFieldRID: testGenRoomID, testFieldName: "general", "t": "c", "open": true, "ls": testTS1, "_updatedAt": testTS1},
				{testFieldID: "subDM", testFieldRID: testDMRoomID, testFieldName: testUserAlice, "t": "d", "open": true, "ls": testTS1, "_updatedAt": testTS1},
			},
		})
	case "/api/v1/rooms.get":
		writeJSON(t, w, map[string]any{
			testFieldSuccess: true,
			"update": []map[string]any{
				{testFieldID: testGenRoomID, "t": "c", testFieldName: "general", "topic": "Initial topic"},
				{testFieldID: testDMRoomID, "t": "d", "uids": []string{testUserMeID, testUserAliceID}, "usernames": []string{testUserMeNick, testUserAlice}},
			},
		})
	case "/api/v1/users.list":
		writeJSON(t, w, map[string]any{
			testFieldSuccess: true,
			"total":          2,
			"users": []map[string]any{
				{testFieldID: testUserMeID, testFieldUsername: testUserMeNick, testFieldName: testUserMeName, testFieldActive: true, testFieldRoles: []string{"admin"}},
				{testFieldID: testUserAliceID, testFieldUsername: testUserAlice, testFieldName: "AliceDev", testFieldActive: true, "type": "bot"},
			},
		})
	default:
		return false
	}

	return true
}

func newTestBridge(t *testing.T, tomlContent string, extra http.HandlerFunc) (*RocketChat, chan *bridge.Event, string) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/websocket" {
			serveMockDDP(w, r)

			return
		}

		if writeHydrateResponse(t, w, r.URL.Path) {
			return
		}

		if extra != nil {
			extra(w, r)

			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := loadTestConfig(t, tomlContent)
	evCh := make(chan *bridge.Event, 64)
	ready := make(chan struct{})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	br, _, err := New(ctx, cfg, bridge.Credentials{Server: srv.URL, Token: testUserMeID + ":secretToken"}, evCh, func() {
		close(ready)
	})
	if err != nil {
		t.Fatal(err)
	}

	rc, ok := br.(*RocketChat)
	if !ok {
		t.Fatalf("expected *RocketChat, got %T", br)
	}

	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for RocketChat Ready callback")
	}

	t.Cleanup(func() {
		_ = rc.Logout(t.Context())
	})

	return rc, evCh, srv.URL
}

func TestRocketChatUserInfoRolesDirectoryAndDM(t *testing.T) {
	tomlCfg := "[rocketchat]\nDirectoryURL = \"https://dir.example.com/u/{username}?nick={nick}\"\nPreferNickname = true\n"

	rc, _, baseURL := newTestBridge(t, tomlCfg, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/channels.members":
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				"total":          2,
				"members": []map[string]any{
					{testFieldID: testUserMeID, testFieldUsername: testUserMeNick, testFieldName: "Haw", testFieldRoles: []string{"admin"}},
					{testFieldID: testUserAliceID, testFieldUsername: testUserAlice, testFieldName: "AliceDev", "type": "bot"},
				},
			})
		case "/api/v1/channels.roles":
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				testFieldRoles: []map[string]any{
					{testFieldID: "rr1", testFieldRID: testGenRoomID, "u": map[string]any{testFieldID: testUserAliceID, testFieldUsername: testUserAlice}, testFieldRoles: []string{"moderator"}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	me := rc.GetMe()
	if !assert.NotNil(t, me) {
		return
	}

	assert.Equal(t, []string{testUserMeNick, testUserMeName, testUserMeID}, me.MentionKeys)
	assert.Equal(t, "haw@example.com", me.Email)
	assert.Equal(t, baseURL+"/avatar/"+testUserMeNick, me.Photo)
	assert.Equal(t, "https://dir.example.com/u/hloeung?nick=hloeung", me.Directory)

	alice := rc.GetUser(t.Context(), testUserAliceID)
	if !assert.NotNil(t, alice) {
		return
	}

	assert.Equal(t, "AliceDev", alice.Nick)
	assert.Contains(t, alice.Roles, "bot")

	photo, secure := rc.UserDetails(t.Context(), testUserAliceID)
	assert.Equal(t, baseURL+"/avatar/"+testUserAlice, photo)
	assert.False(t, secure)

	members, err := rc.GetChannelUsers(t.Context(), testGenRoomID)
	if err != nil {
		t.Fatal(err)
	}

	assert.Len(t, members, 2)
	assert.Equal(t, "@", rc.ChannelUserPrefix(testGenRoomID, testUserMeID))
	assert.Equal(t, "+", rc.ChannelUserPrefix(testGenRoomID, testUserAliceID))

	assert.Equal(t, testGenRoomID, rc.GetChannelID(t.Context(), "#general", ""))
	assert.Equal(t, testDMRoomID, rc.GetChannelID(t.Context(), testUserAlice, ""))
	assert.Empty(t, rc.GetChannelID(t.Context(), "#"+testUserAlice, ""))
}

func TestRocketChatMentionsAndRichAttachments(t *testing.T) {
	rc, _, _ := newTestBridge(t, "[rocketchat]\nDisableDefaultMentions = true\n", nil)

	linkMsg := &rocketchatclient.Message{
		ID:        "linkMsg1234567890",
		RoomID:    testGenRoomID,
		Text:      "hey <@uAlice> and @all check https://example.com",
		Timestamp: rocketchatclient.Timestamp{Time: time.UnixMilli(1700000000000)},
		User:      rocketchatclient.UserRef{ID: testUserAliceID, Username: testUserAlice},
		Mentions:  []rocketchatclient.UserRef{{ID: testUserAliceID, Username: testUserAlice}, {ID: "all", Username: "all"}},
		Attachments: []rocketchatclient.Attachment{
			{Title: "Example", TitleLink: "https://example.com", ImageURL: "https://example.com/og.png"},
			{AuthorName: testUserAlice, Text: "quoted text", MessageLink: "https://rocket.example.com/channel/general?msg=m1", Color: "good"},
		},
	}

	assert.Empty(t, rc.extractFiles(linkMsg))

	text, msgType := rc.formatIncomingText(linkMsg, false)
	assert.Empty(t, msgType)
	assert.Contains(t, text, "@"+testUserAlice)
	assert.NotContains(t, text, "@all")
	assert.Contains(t, text, utils.AttachmentMarkerStart)
	assert.Contains(t, text, "quoted text")
}

func TestRocketChatFileUploads(t *testing.T) {
	rc, evCh, _ := newTestBridge(t, "", nil)

	fileMsg := &rocketchatclient.Message{
		ID:        "fileMsg1234567890",
		RoomID:    testGenRoomID,
		Timestamp: rocketchatclient.Timestamp{Time: time.UnixMilli(1700000001000)},
		User:      rocketchatclient.UserRef{ID: testUserAliceID, Username: testUserAlice},
		File:      &rocketchatclient.File{ID: "f123", Name: testReportPDF},
		Attachments: []rocketchatclient.Attachment{
			{Title: testReportPDF, TitleLink: "/file-upload/f123/report.pdf", Description: "Quarterly report", ImageSize: 4096},
		},
	}
	rc.OnMessage(fileMsg)

	var seenText, seenFile bool

	for range 2 {
		ev := <-evCh
		switch e := ev.Data.(type) {
		case *bridge.ChannelMessageEvent:
			seenText = e.Text == "Quarterly report"
		case *bridge.FileEvent:
			seenFile = len(e.Files) == 1 && e.Files[0].Name == testReportPDF && strings.Contains(e.Files[0].URL, "/file-upload/f123/report.pdf")
		}
	}

	assert.True(t, seenText)
	assert.True(t, seenFile)
}

func TestRocketChatThreadedActions(t *testing.T) {
	rc, _, _ := newTestBridge(t, "", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/chat.sendMessage":
			var body struct {
				Message struct {
					Text string `json:"msg"`
					TMID string `json:"tmid"`
				} `json:"message"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				"message":        map[string]any{testFieldID: "sentMsg1234567890", testFieldRID: testGenRoomID, testFieldMsg: body.Message.Text, "tmid": body.Message.TMID},
			})
		case "/api/v1/chat.getMessage":
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				"message":        map[string]any{testFieldID: "childReply1234567", testFieldRID: testGenRoomID, "tmid": "rootThread1234567", testFieldMsg: "child"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	msgID, err := rc.MsgChannelThread(t.Context(), testGenRoomID, "childReply1234567", "\x01ACTION waves\x01")
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, "sentMsg1234567890", msgID)

	sentCached, ok := rc.rc.CachedMessage("sentMsg1234567890")
	if assert.True(t, ok) {
		assert.Equal(t, "_waves_", sentCached.Text)
		assert.Equal(t, "rootThread1234567", sentCached.ThreadID)
	}
}

func historySearchHandler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/channels.history":
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				"messages": []map[string]any{
					{testFieldID: "oldBoundary123456", testFieldRID: testGenRoomID, testFieldMsg: "old", "ts": testTS1, "u": map[string]any{testFieldID: testUserAliceID, testFieldUsername: testUserAlice}},
					{testFieldID: "newReplay12345678", testFieldRID: testGenRoomID, testFieldMsg: "new @all", "ts": "2026-11-16T02:00:00Z", "u": map[string]any{testFieldID: testUserAliceID, testFieldUsername: testUserAlice}},
				},
			})
		case "/api/v1/subscriptions.read", "/api/v1/users.setStatus", "/api/v1/users.updateOwnBasicInfo":
			writeJSON(t, w, map[string]any{testFieldSuccess: true})
		case "/api/v1/chat.search":
			writeJSON(t, w, map[string]any{
				testFieldSuccess: true,
				"messages": []map[string]any{
					{testFieldID: "searchHit12345678", testFieldRID: testGenRoomID, testFieldMsg: "needle", "ts": "2026-11-16T03:00:00Z", "u": map[string]any{testFieldID: testUserAliceID, testFieldUsername: testUserAlice}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestRocketChatReplayAndSearch(t *testing.T) {
	rc, _, baseURL := newTestBridge(t, "", historySearchHandler(t))

	since := rc.GetLastViewedAt(t.Context(), testGenRoomID)
	assert.Equal(t, time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC).UnixMilli(), since)

	replay := rc.GetReplayEvents(t.Context(), testGenRoomID, since)
	if !assert.Len(t, replay, 1) {
		return
	}

	chEv, ok := replay[0].Data.(*bridge.ChannelMessageEvent)
	if assert.True(t, ok) {
		assert.Equal(t, "newReplay12345678", chEv.MessageID)
		assert.Equal(t, "notice", chEv.MessageType)
	}

	err := rc.UpdateLastViewedUser(t.Context(), testUserAliceID)
	if err != nil {
		t.Fatal(err)
	}

	assert.Greater(t, rc.GetLastViewedAt(t.Context(), testDMRoomID), since)

	results := rc.SearchPosts(t.Context(), "#general needle")
	if assert.Len(t, results, 1) {
		assert.Equal(t, baseURL+"/channel/general?msg=searchHit12345678", rc.PostURL(t.Context(), "searchHit12345678", results))
	}
}

func TestRocketChatStatusNickInviteAndTyping(t *testing.T) {
	rc, evCh, _ := newTestBridge(t, "", historySearchHandler(t))

	err := rc.SetStatus(t.Context(), "away")
	if err != nil {
		t.Fatal(err)
	}

	err = rc.Nick(t.Context(), "Haw")
	if err != nil {
		t.Fatal(err)
	}

	rc.OnInvite(&rocketchatclient.Invitation{RoomID: "PRIV1234567890123", RoomName: "secret", Sender: testUserAlice})

	invEv := <-evCh
	if inv, okInv := invEv.Data.(*bridge.ChannelInviteEvent); assert.True(t, okInv) {
		assert.Equal(t, "PRIV1234567890123", inv.ChannelID)
		assert.Equal(t, "secret", inv.Name)
	}

	rc.OnTyping(testGenRoomID, testUserMeNick)
	rc.OnTyping(testDMRoomID, testUserAlice)

	typEv := <-evCh
	if typ, okTyp := typEv.Data.(*bridge.TypingEvent); assert.True(t, okTyp) {
		assert.Equal(t, "D", typ.ChannelType)
		assert.Equal(t, testUserAliceID, typ.Sender.User)
	}
}
