package rocketchatclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testFieldSuccess = "success"
	testFieldMsgs    = "messages"
	testFieldID      = "_id"
	testFieldRID     = "rid"
	testUserBob      = "bob"
	testUserAlice    = "alice"
	testRoomGeneral  = "general"
	testRoomID       = "R1"
	testRootID       = "rootMsg1234567890"
	testReplID       = "replMsg1234567890"
)

type recordingHandler struct {
	mu        sync.Mutex
	ready     int
	messages  []*Message
	edits     []*Message
	deletes   []string
	reactions []*ReactionEvent
	rooms     []*Room
	members   []*MemberEvent
	presences []string
	typings   []string
	invites   []*Invitation
}

func (h *recordingHandler) Ready() {
	h.mu.Lock()
	h.ready++
	h.mu.Unlock()
}

func (h *recordingHandler) OnMessage(msg *Message) {
	h.mu.Lock()
	h.messages = append(h.messages, msg)
	h.mu.Unlock()
}

func (h *recordingHandler) OnEdit(msg *Message) {
	h.mu.Lock()
	h.edits = append(h.edits, msg)
	h.mu.Unlock()
}

func (h *recordingHandler) OnDelete(roomID, msgID string) {
	h.mu.Lock()
	h.deletes = append(h.deletes, roomID+":"+msgID)
	h.mu.Unlock()
}

func (h *recordingHandler) OnReaction(ev *ReactionEvent) {
	h.mu.Lock()
	h.reactions = append(h.reactions, ev)
	h.mu.Unlock()
}

func (h *recordingHandler) OnRoomChange(room *Room) {
	h.mu.Lock()
	h.rooms = append(h.rooms, room)
	h.mu.Unlock()
}

func (h *recordingHandler) OnPresence(userID, status string) {
	h.mu.Lock()
	h.presences = append(h.presences, userID+":"+status)
	h.mu.Unlock()
}

func (h *recordingHandler) OnMember(ev *MemberEvent) {
	h.mu.Lock()
	h.members = append(h.members, ev)
	h.mu.Unlock()
}

func (h *recordingHandler) OnTyping(roomID, username string) {
	h.mu.Lock()
	h.typings = append(h.typings, roomID+":"+username)
	h.mu.Unlock()
}

func (h *recordingHandler) OnInvite(inv *Invitation) {
	h.mu.Lock()
	h.invites = append(h.invites, inv)
	h.mu.Unlock()
}

func newUnitClient(t *testing.T, baseURL string, h Handler) *Client {
	t.Helper()

	cache, err := newStateCache(256, 256, 256)
	if err != nil {
		t.Fatal(err)
	}

	rootLogger, rootAPILogger := newLoggers()
	rootLogger.SetLevel(0)
	rootAPILogger.SetLevel(0)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	return &Client{
		httpURL:        baseURL,
		httpClient:     &http.Client{Timeout: 5 * time.Second},
		handler:        h,
		ctx:            ctx,
		cancel:         cancel,
		userID:         "uMe",
		authToken:      "tokMe",
		me:             &User{ID: "uMe", Username: "me", Name: "Me User", Active: true},
		rootLogger:     rootLogger,
		logger:         rootLogger.WithField("test", true),
		rootAPILogger:  rootAPILogger,
		apiLogger:      rootAPILogger.WithField("test", true),
		cache:          cache,
		eventCh:        make(chan ddpEvent, eventQueueSize),
		roomDeleteSubs: make(map[string]string),
		roomTypingSubs: make(map[string]string),
	}
}

func writeJSONResponse(t *testing.T, w http.ResponseWriter, status int, payload any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	err := json.NewEncoder(w).Encode(payload)
	if err != nil {
		t.Fatal(err)
	}
}

func testUserMap(id, username, name string) map[string]any {
	m := map[string]any{testFieldID: id, "username": username}
	if name != "" {
		m["name"] = name
	}

	return m
}

func historyTestHandler(t *testing.T) http.HandlerFunc {
	t1 := "2026-11-16T01:00:00Z"
	t2 := "2026-11-16T02:00:00Z"

	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/channels.history":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				testFieldSuccess: true,
				testFieldMsgs: []map[string]any{
					{testFieldID: testReplID, testFieldRID: testRoomID, ddpFieldMsg: "second", "ts": t2, "u": testUserMap("u2", testUserBob, "Bob")},
					{testFieldID: testRootID, testFieldRID: testRoomID, ddpFieldMsg: "first", "ts": t1, "u": testUserMap("u1", testUserAlice, "Alice"), "mentions": []map[string]any{testUserMap("u3", "carol", "Carol")}},
				},
			})
		case "/api/v1/chat.getMessage":
			msg := map[string]any{testFieldID: testRootID, testFieldRID: testRoomID, ddpFieldMsg: "root", "ts": t1, "u": testUserMap("u1", testUserAlice, "")}
			if r.URL.Query().Get("msgId") == testReplID {
				msg = map[string]any{testFieldID: testReplID, testFieldRID: testRoomID, "tmid": testRootID, ddpFieldMsg: "reply", "ts": t2, "u": testUserMap("u2", testUserBob, "")}
			}

			writeJSONResponse(t, w, http.StatusOK, map[string]any{testFieldSuccess: true, "message": msg})
		case "/api/v1/chat.getThreadMessages":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				testFieldSuccess: true,
				testFieldMsgs:    []map[string]any{{testFieldID: testReplID, testFieldRID: testRoomID, "tmid": testRootID, ddpFieldMsg: "reply", "ts": t2, "u": testUserMap("u2", testUserBob, "")}},
			})
		case "/api/v1/chat.search":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				testFieldSuccess: true,
				testFieldMsgs: []map[string]any{
					{testFieldID: testReplID, testFieldRID: testRoomID, ddpFieldMsg: "second match", "ts": t2, "u": testUserMap("u2", testUserBob, "")},
					{testFieldID: testRootID, testFieldRID: testRoomID, ddpFieldMsg: "first match", "ts": t1, "u": testUserMap("u1", testUserAlice, "")},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestGetHistoryAndThreadAndSearch(t *testing.T) {
	srv := httptest.NewServer(historyTestHandler(t))
	defer srv.Close()

	c := newUnitClient(t, srv.URL, nil)
	c.cache.upsertRoom(&Room{ID: testRoomID, Type: RoomTypeChannel, Name: testRoomGeneral})

	msgs, err := c.GetHistory(t.Context(), testRoomID, 10, time.Time{})
	if err != nil || len(msgs) != 2 || msgs[0].ID != testRootID || msgs[1].ID != testReplID {
		t.Fatalf("unexpected GetHistory result: err=%v msgs=%+v", err, msgs)
	}

	if u, ok := c.User("u3"); !ok || u.Username != "carol" {
		t.Fatalf("expected carol seeded from mentions, got ok=%v u=%+v", ok, u)
	}

	c.cache.forgetMessage(testReplID)
	c.cache.forgetMessage(testRootID)

	thread, err := c.GetThreadMessages(t.Context(), testReplID)
	if err != nil || len(thread) != 2 || thread[0].ID != testRootID || thread[1].ID != testReplID {
		t.Fatalf("unexpected GetThreadMessages result: err=%v thread=%+v", err, thread)
	}

	found, err := c.SearchMessages(t.Context(), testRoomID, "match", 10)
	if err != nil || len(found) != 2 || found[0].ID != testRootID || found[1].ID != testReplID {
		t.Fatalf("unexpected SearchMessages result: err=%v found=%+v", err, found)
	}
}

func statusRolesTestHandler(t *testing.T, attempts *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/subscriptions.read", "/api/v1/users.setStatus", "/api/v1/users.updateOwnBasicInfo":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{testFieldSuccess: true})
		case "/api/v1/channels.roles":
			if attempts.Add(1) == 1 {
				w.Header().Set("Retry-After", "0")
				writeJSONResponse(t, w, http.StatusTooManyRequests, map[string]any{testFieldSuccess: false, "error": "rate"})

				return
			}

			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				testFieldSuccess: true,
				"roles":          []map[string]any{{testFieldID: "rr1", testFieldRID: testRoomID, "u": testUserMap("u1", testUserAlice, ""), "roles": []string{"owner"}}},
			})
		case "/api/v1/channels.members":
			writeJSONResponse(t, w, http.StatusOK, map[string]any{
				testFieldSuccess: true,
				"total":          1,
				"members":        []map[string]any{{testFieldID: "u1", "username": testUserAlice, "active": true}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestMarkReadAndStatus(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(statusRolesTestHandler(t, &attempts))
	defer srv.Close()

	c := newUnitClient(t, srv.URL, nil)
	c.cache.upsertRoom(&Room{ID: testRoomID, Type: RoomTypeChannel, Name: testRoomGeneral})
	c.cache.upsertSubscription(&Subscription{
		RoomID:   testRoomID,
		Name:     testRoomGeneral,
		Type:     RoomTypeChannel,
		Open:     true,
		Alert:    true,
		Unread:   4,
		LastSeen: Timestamp{Time: time.Unix(1000, 0).UTC()},
	})

	err := c.MarkRead(t.Context(), testRoomID)
	if err != nil {
		t.Fatalf("MarkRead failed: %v", err)
	}

	sub, ok := c.Subscription(testRoomID)
	if !ok || sub.Alert || sub.Unread != 0 || sub.LastSeen.Time.Unix() <= 1000 {
		t.Fatalf("expected subscription LastSeen updated, got %+v", sub)
	}

	err = c.SetStatus(t.Context(), StatusAway, "")
	if err != nil {
		t.Fatalf("SetStatus failed: %v", err)
	}

	err = c.SetDisplayName(t.Context(), "Renamed Me")
	if err != nil {
		t.Fatalf("SetDisplayName failed: %v", err)
	}

	if me := c.Me(); me == nil || me.Name != "Renamed Me" {
		t.Fatalf("expected Me().Name updated, got %+v", me)
	}
}

func TestRoomMembersRolesAndURLs(t *testing.T) {
	var attempts atomic.Int32

	srv := httptest.NewServer(statusRolesTestHandler(t, &attempts))
	defer srv.Close()

	c := newUnitClient(t, srv.URL, nil)
	c.cache.upsertRoom(&Room{ID: testRoomID, Type: RoomTypeChannel, Name: testRoomGeneral})

	members, err := c.GetRoomMembers(t.Context(), testRoomID)
	if err != nil {
		t.Fatalf("GetRoomMembers failed: %v", err)
	}

	if len(members) != 1 || members[0].Username != testUserAlice {
		t.Fatalf("unexpected GetRoomMembers: %+v", members)
	}

	if roles := c.RoomUserRoles(testRoomID, "u1"); len(roles) != 1 || roles[0] != "owner" {
		t.Fatalf("expected owner role after 429 retry, got %v", roles)
	}

	if got := c.ResolveAttachmentURL("/file-upload/f1/pic.png"); got != srv.URL+"/file-upload/f1/pic.png" {
		t.Fatalf("unexpected ResolveAttachmentURL: %s", got)
	}
}

func TestDDPMessageAndNotificationDispatch(t *testing.T) {
	h := &recordingHandler{}
	c := newUnitClient(t, "https://chat.example.com", h)

	meMsg := &Message{
		ID:     "meMsg123456789012",
		RoomID: testRoomID,
		Type:   "me",
		Text:   "waves",
		User:   UserRef{ID: "u1", Username: testUserAlice},
	}
	c.processIncomingMessage(t.Context(), meMsg)

	if len(h.messages) != 1 || h.messages[0].Type != "me" {
		t.Fatalf("expected /me message delivered to OnMessage, got %+v", h.messages)
	}

	c.cache.markSent("send:selfMsg1234567890")
	c.processIncomingMessage(t.Context(), &Message{ID: "selfMsg1234567890", RoomID: testRoomID, Text: "echo"})

	if len(h.messages) != 1 {
		t.Fatalf("expected self-echo dropped, got %d messages", len(h.messages))
	}

	c.handleNotifyRoom(&ddpFields{
		EventName: testRoomID + "/typing",
		Args:      []json.RawMessage{json.RawMessage(`"alice"`), json.RawMessage(`true`)},
	}, true)

	if len(h.typings) != 1 || h.typings[0] != "R1:alice" {
		t.Fatalf("expected typing event R1:alice, got %v", h.typings)
	}

	const thumbsUp = ":+1:"

	msgWithReact := &Message{
		ID:        "reactMsg123456789",
		RoomID:    testRoomID,
		Text:      "hi",
		User:      UserRef{ID: "u1", Username: testUserAlice},
		Reactions: map[string]ReactionSet{thumbsUp: {Usernames: []string{testUserAlice}}},
	}
	c.processIncomingMessage(t.Context(), msgWithReact)
	msgWithReact.Reactions = map[string]ReactionSet{thumbsUp: {Usernames: []string{testUserAlice, testUserBob}}}
	c.processIncomingMessage(t.Context(), msgWithReact)
	msgWithReact.Reactions = map[string]ReactionSet{thumbsUp: {Usernames: []string{testUserBob}}}
	c.processIncomingMessage(t.Context(), msgWithReact)

	if len(h.reactions) != 2 || !h.reactions[0].Added || h.reactions[0].Username != testUserBob || h.reactions[1].Added || h.reactions[1].Username != testUserAlice {
		t.Fatalf("unexpected reaction diff events: %+v", h.reactions)
	}
}

func TestSubscriptionSeedsRoomAndSuppressesSelfSystemJoin(t *testing.T) {
	h := &recordingHandler{}
	c := newUnitClient(t, "https://chat.example.com", h)

	// Subscription event arriving before rooms-changed must immediately make Room() available.
	c.handleSubscriptionsChanged("inserted", json.RawMessage(`{"rid":"NEWROOM1234567890","name":"sandbox","t":"c","open":true}`), true)

	room, ok := c.Room("NEWROOM1234567890")
	if !ok || room.Name != "sandbox" || room.Type != RoomTypeChannel {
		t.Fatalf("expected seeded room sandbox from subscription, got ok=%v room=%+v", ok, room)
	}

	if len(h.members) != 1 || h.members[0].Action != memberActionJoin {
		t.Fatalf("expected 1 join MemberEvent from subscription, got %+v", h.members)
	}

	// Subsequent self "uj" system message must not emit a duplicate self join.
	c.handleSystemMessage(t.Context(), &Message{
		ID:     "ujMsg123456789012",
		RoomID: "NEWROOM1234567890",
		Type:   "uj",
		User:   UserRef{ID: c.UserID(), Username: "me"},
	})

	if len(h.members) != 1 {
		t.Fatalf("expected duplicate self uj suppressed, got %d MemberEvents", len(h.members))
	}
}
