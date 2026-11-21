package msteamsclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
)

const (
	trouterKeepaliveInterval = 45 * time.Second
	trouterStableDuration    = 30 * time.Second
	trouterReadTimeout       = 90 * time.Second
	trouterWriteTimeout      = 15 * time.Second
	trouterPollInterval      = 10 * time.Second
	trouterMaxConsecFailures = 3
)

type trouterAllocResponse struct {
	CCID          string            `json:"ccid"`
	SocketIO      string            `json:"socketio"`
	SURL          string            `json:"surl"`
	ConnectParams map[string]string `json:"connectparams"`
	TTL           int               `json:"ttl"`
}

type trouterEnvelope struct {
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	URL     string          `json:"url"`
	Headers map[string]any  `json:"headers"`
	Body    json.RawMessage `json:"body"`
}

type trouterBodyPayload struct {
	EventMessages []trouterEventMessage `json:"eventMessages"`
	ResourceType  string                `json:"resourceType"`
	Resource      json.RawMessage       `json:"resource"`
	Type          string                `json:"type"`
}

type trouterEventMessage struct {
	ID           int64           `json:"id"`
	Type         string          `json:"type"`
	ResourceType string          `json:"resourceType"`
	ResourceLink string          `json:"resourceLink"`
	Resource     json.RawMessage `json:"resource"`
	Time         string          `json:"time"`
}

type trouterConvUpdate struct {
	ID               string `json:"id"`
	ThreadProperties struct {
		Topic string `json:"topic"`
	} `json:"threadProperties"`
	Properties struct {
		ConsumptionHorizon string `json:"consumptionhorizon"`
	} `json:"properties"`
}

type trouterSession struct {
	alloc        *trouterAllocResponse
	epid         string
	endpointID   string
	wsConn       *websocket.Conn
	heartbeatSec int
	expiresAt    time.Time
}

// startTrouter spawns the background Trouter v4 supervisor goroutine.
func (c *Client) startTrouter(ctx context.Context) {
	c.wg.Go(func() {
		c.trouterSupervisor(ctx)
	})
}

func (c *Client) trouterSupervisor(ctx context.Context) {
	b := &backoff.Backoff{
		Min:    2 * time.Second,
		Max:    2 * time.Minute,
		Factor: 2,
		Jitter: true,
	}

	consecFailures := 0

	for {
		if ctx.Err() != nil {
			return
		}

		startedAt := time.Now()
		err := c.runTrouterSession(ctx)

		if ctx.Err() != nil {
			return
		}

		if time.Since(startedAt) >= trouterStableDuration {
			b.Reset()

			consecFailures = 0
		} else {
			consecFailures++
		}

		c.logger.Warnf("Trouter session ended: %v", err)

		if c.PollFallback && consecFailures >= trouterMaxConsecFailures {
			c.runPollWindow(ctx, b.Duration())

			continue
		}

		wait := b.Duration()
		c.logger.Debugf("Reconnecting Trouter in %s", wait.Round(time.Millisecond))

		if sleepContext(ctx, wait) != nil {
			return
		}
	}
}

func (c *Client) runTrouterSession(ctx context.Context) error {
	c.wsReconnectMu.Lock()

	sess, err := c.establishTrouter(ctx)
	if err != nil {
		c.wsReconnectMu.Unlock()

		return err
	}

	c.wsMu.Lock()
	c.wsConn = sess.wsConn
	c.trouterEPID = sess.endpointID
	c.trouterSURL = sess.alloc.SURL
	c.wsConnected.Store(true)
	c.lastWsActivity.Store(time.Now().Unix())
	c.wsMu.Unlock()

	c.wsReconnectMu.Unlock()

	defer func() {
		c.closeActiveWS()
	}()

	c.notifyWsConnected()

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	go c.trouterKeepaliveLoop(sessionCtx, sess)

	readErr := c.readTrouterLoop(sessionCtx, sess)

	c.wsConnected.Store(false)
	c.notifyWsDisconnected(readErr)

	return readErr
}

func (c *Client) establishTrouter(ctx context.Context) (*trouterSession, error) {
	epid := randomHexID(16)

	alloc, err := c.allocateTrouter(ctx, epid)
	if err != nil {
		return nil, err
	}

	wsConn, heartbeatSec, err := c.dialTrouterWS(ctx, alloc, epid)
	if err != nil {
		return nil, err
	}

	endpointID, err := c.registerChatsvcEndpoint(ctx, alloc.SURL, epid)
	if err != nil {
		_ = wsConn.Close()

		return nil, err
	}

	ttl := time.Duration(alloc.TTL) * time.Second
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}

	return &trouterSession{
		alloc:        alloc,
		epid:         epid,
		endpointID:   endpointID,
		wsConn:       wsConn,
		heartbeatSec: heartbeatSec,
		expiresAt:    time.Now().Add(ttl - 5*time.Minute),
	}, nil
}

func (c *Client) allocateTrouter(ctx context.Context, epid string) (*trouterAllocResponse, error) {
	ep := c.Endpoints()
	if ep.Trouter == "" {
		return nil, errors.New("msteamsclient: trouter endpoint unavailable")
	}

	token, err := c.EnsureSkypeToken(ctx)
	if err != nil {
		return nil, err
	}

	allocURL := fmt.Sprintf("%s/v4/a?epid=%s", ep.Trouter, url.QueryEscape(epid))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, allocURL, http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Skypetoken", token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: trouter /v4/a request failed: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAuthResponseSize))
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: read trouter /v4/a response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_, _ = c.ForceRefreshSkypeToken(ctx)

		return nil, errors.New("msteamsclient: trouter /v4/a returned HTTP 401")
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("msteamsclient: trouter /v4/a returned HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	var alloc trouterAllocResponse

	err = json.Unmarshal(body, &alloc)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: decode trouter /v4/a response: %w", err)
	}

	if alloc.SocketIO == "" || alloc.SURL == "" {
		return nil, errors.New("msteamsclient: incomplete trouter allocation response")
	}

	return &alloc, nil
}

func (c *Client) dialTrouterWS(ctx context.Context, alloc *trouterAllocResponse, epid string) (*websocket.Conn, int, error) {
	token, err := c.EnsureSkypeToken(ctx)
	if err != nil {
		return nil, 0, err
	}

	base := strings.TrimRight(alloc.SocketIO, "/")
	q := buildTrouterQuery(alloc, epid)

	handshakeURL := base + "/socket.io/1/?" + q.Encode()

	sid, heartbeatSec, err := c.negotiateSocketIO(ctx, handshakeURL, token)
	if err != nil {
		return nil, 0, err
	}

	wsBase := strings.Replace(base, "https://", "wss://", 1)
	wsBase = strings.Replace(wsBase, "http://", "ws://", 1)
	wsURL := fmt.Sprintf("%s/socket.io/1/websocket/%s?%s", wsBase, url.PathEscape(sid), q.Encode())

	dialer := websocket.Dialer{
		HandshakeTimeout: 20 * time.Second,
	}

	if c.cred.SkipTLSVerify {
		dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	}

	headers := http.Header{
		"X-Skypetoken": {token},
		"User-Agent":   {c.userAgent()},
	}

	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		return nil, 0, fmt.Errorf("msteamsclient: trouter websocket dial failed: %w", err)
	}

	return conn, heartbeatSec, nil
}

func buildTrouterQuery(alloc *trouterAllocResponse, epid string) url.Values {
	q := url.Values{}
	q.Set("v", "v4")
	q.Set("epid", epid)
	q.Set("ccid", alloc.CCID)
	q.Set("con_num", epid+"_1")
	q.Set("tc", `{"cv":"2026.1.0","ua":"TeamsWeb","hr":"","v":"1.0"}`)

	for k, v := range alloc.ConnectParams {
		q.Set(k, v)
	}

	return q
}

func (c *Client) negotiateSocketIO(ctx context.Context, handshakeURL, token string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, handshakeURL, http.NoBody)
	if err != nil {
		return "", 0, err
	}

	req.Header.Set("X-Skypetoken", token)
	req.Header.Set("User-Agent", c.userAgent())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("msteamsclient: socket.io handshake failed: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return "", 0, fmt.Errorf("msteamsclient: read socket.io handshake: %w", err)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return "", 0, fmt.Errorf("msteamsclient: socket.io handshake returned HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	raw := strings.TrimSpace(string(body))
	parts := strings.Split(raw, ":")

	if len(parts) < 2 || parts[0] == "" {
		return "", 0, fmt.Errorf("msteamsclient: unexpected socket.io handshake response %q", raw)
	}

	heartbeatSec := 30

	n, parseErr := strconv.Atoi(parts[1])
	if parseErr == nil && n > 5 {
		heartbeatSec = n
	}

	return parts[0], heartbeatSec, nil
}

func (c *Client) registerChatsvcEndpoint(ctx context.Context, surl, epid string) (string, error) {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return "", errors.New("msteamsclient: chatService endpoint unavailable")
	}

	endpointID := "{" + epid + "}"
	reqURL := fmt.Sprintf("%s/v1/users/ME/endpoints/%s", ep.ChatService, url.PathEscape(endpointID))

	payload := map[string]any{
		"id":               endpointID,
		"trouterUri":       surl,
		"endpointFeatures": "Agent,MessageProperties,CustomUserProperties,NotificationStream,SupportsMessageProperties",
		"subscriptions": []map[string]any{
			{
				"channelType": "HttpLongPoll",
				"interestedResources": []string{
					"/v1/users/ME/conversations/ALL/properties",
					"/v1/users/ME/conversations/ALL/messages",
					"/v1/threads/ALL",
				},
				"template": "raw",
			},
		},
	}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		// Fall back to POST /v1/users/ME/endpoints on tenants expecting creation semantics.
		postURL := ep.ChatService + "/v1/users/ME/endpoints"

		err = c.doJSON(ctx, http.MethodPost, postURL, authSkypeToken, "", payload, nil)
		if err != nil {
			return "", fmt.Errorf("msteamsclient: register chatsvc endpoint: %w", err)
		}
	}

	_ = c.sendEndpointActive(ctx, endpointID)

	return endpointID, nil
}

func (c *Client) sendEndpointActive(ctx context.Context, endpointID string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" || endpointID == "" {
		return nil
	}

	reqURL := fmt.Sprintf("%s/v1/users/ME/endpoints/%s/active", ep.ChatService, url.PathEscape(endpointID))
	payload := map[string]any{"timeout": 12}

	return c.doJSON(ctx, http.MethodPost, reqURL, authSkypeToken, "", payload, nil)
}

func (c *Client) trouterKeepaliveLoop(ctx context.Context, sess *trouterSession) {
	activeTicker := time.NewTicker(trouterKeepaliveInterval)
	defer activeTicker.Stop()

	hbInterval := max(time.Duration(sess.heartbeatSec/2)*time.Second, 15*time.Second)

	hbTicker := time.NewTicker(hbInterval)
	defer hbTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-hbTicker.C:
			if time.Now().After(sess.expiresAt) {
				c.logger.Debug("Trouter session TTL approaching expiry; reconnecting")
				c.closeActiveWS()

				return
			}

			err := c.writeWSFrame(sess.wsConn, "2::")
			if err != nil {
				c.logger.Debugf("Trouter heartbeat write failed: %v", err)
				c.closeActiveWS()

				return
			}
		case <-activeTicker.C:
			err := c.sendEndpointActive(ctx, sess.endpointID)
			if err != nil {
				c.logger.Debugf("Endpoint /active keepalive failed: %v", err)
			}
		}
	}
}

func (c *Client) readTrouterLoop(ctx context.Context, sess *trouterSession) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		_ = sess.wsConn.SetReadDeadline(time.Now().Add(trouterReadTimeout))

		_, data, err := sess.wsConn.ReadMessage()
		if err != nil {
			return err
		}

		c.lastWsActivity.Store(time.Now().Unix())

		frameErr := c.handleSocketIOFrame(sess, string(data))
		if frameErr != nil {
			return frameErr
		}
	}
}

func (c *Client) handleSocketIOFrame(sess *trouterSession, frame string) error {
	switch {
	case frame == "0::" || strings.HasPrefix(frame, "0::"):
		return errors.New("msteamsclient: trouter server requested disconnect")
	case frame == "1::" || strings.HasPrefix(frame, "1::"):
		c.logger.Debug("Trouter Socket.IO connected")

		return nil
	case frame == "2::" || frame == "2":
		reply := "2::"
		if frame == "2" {
			reply = "3"
		}

		return c.writeWSFrame(sess.wsConn, reply)
	case strings.HasPrefix(frame, "3:::"):
		return c.handleTrouterMessageFrame(sess, frame[4:])
	case strings.HasPrefix(frame, "5:::"):
		c.handleSocketIOEvent(frame[4:])

		return nil
	case strings.HasPrefix(frame, "42"):
		c.handleSocketIOEvent(frame[2:])

		return nil
	default:
		return nil
	}
}

func (c *Client) handleTrouterMessageFrame(sess *trouterSession, payload string) error {
	var env trouterEnvelope

	err := json.Unmarshal([]byte(payload), &env)
	if err != nil {
		c.logger.Tracef("Ignoring non-JSON trouter frame: %v", err)

		return nil
	}

	if env.ID != 0 {
		ack := fmt.Sprintf(`3:::{"id":%d,"status":200,"headers":{},"body":""}`, env.ID)

		writeErr := c.writeWSFrame(sess.wsConn, ack)
		if writeErr != nil {
			return writeErr
		}
	}

	if len(env.Body) == 0 {
		return nil
	}

	c.dispatchTrouterBody(env.Body)

	return nil
}

func (c *Client) handleSocketIOEvent(raw string) {
	if strings.Contains(raw, "trouter.message_loss") {
		c.logger.Warn("Trouter signaled message_loss; triggering reconnect resync")
		c.closeActiveWS()
	}
}

func (c *Client) dispatchTrouterBody(rawBody json.RawMessage) {
	bodyBytes := rawBody

	// Trouter frequently encodes the inner body as a JSON-escaped string.
	if len(rawBody) > 0 && rawBody[0] == '"' {
		var unquoted string

		err := json.Unmarshal(rawBody, &unquoted)
		if err != nil {
			return
		}

		bodyBytes = []byte(unquoted)
	}

	var payload trouterBodyPayload

	err := json.Unmarshal(bodyBytes, &payload)
	if err != nil {
		return
	}

	if len(payload.EventMessages) > 0 {
		for i := range payload.EventMessages {
			c.handleTrouterEvent(&payload.EventMessages[i])
		}

		return
	}

	if payload.ResourceType != "" && len(payload.Resource) > 0 {
		c.handleTrouterEvent(&trouterEventMessage{
			Type:         payload.Type,
			ResourceType: payload.ResourceType,
			Resource:     payload.Resource,
		})
	}
}

func (c *Client) handleTrouterEvent(ev *trouterEventMessage) {
	switch ev.ResourceType {
	case "NewMessage", "MessageUpdate":
		c.handleTrouterChatEvent(ev.ResourceType, ev.Resource)
	case "ConversationUpdate", "ThreadUpdate":
		c.handleTrouterConversationUpdate(ev.Resource)
	case "UserPresence":
		c.handleTrouterPresenceEvent(ev.Resource)
	default:
		c.logger.Tracef("Ignoring unhandled trouter resourceType=%q", ev.ResourceType)
	}
}

func (c *Client) handleTrouterChatEvent(resourceType string, resource json.RawMessage) {
	var raw rawChatMessage

	err := json.Unmarshal(resource, &raw)
	if err != nil {
		return
	}

	convID, _ := splitConversationAndRoot(raw.ConversationID, raw.ConversationLink, "")
	fromMRI := extractMRIFromURL(raw.From)

	switch raw.MessageType {
	case "Control/Typing":
		c.emitMessage(&Message{
			Type:      EventTyping,
			Channel:   convID,
			UserID:    fromMRI,
			Username:  raw.IMDisplayName,
			Timestamp: time.Now(),
		})

		return
	case "Control/ClearTyping", "Control/LiveState":
		c.emitMessage(&Message{
			Type:      EventClearTyping,
			Channel:   convID,
			UserID:    fromMRI,
			Username:  raw.IMDisplayName,
			Timestamp: time.Now(),
		})

		return
	case "ThreadActivity/TopicUpdate":
		c.handleTopicActivity(convID, fromMRI, raw.Content)

		return
	case "ThreadActivity/AddMember":
		c.handleMemberActivity(convID, fromMRI, raw.Content, true)

		return
	case "ThreadActivity/DeleteMember":
		c.handleMemberActivity(convID, fromMRI, raw.Content, false)

		return
	}

	if strings.HasPrefix(raw.MessageType, "Control/") || strings.HasPrefix(raw.MessageType, "ThreadActivity/") {
		return
	}

	c.dispatchChatMessageEvent(resourceType, &raw)
}

func (c *Client) dispatchChatMessageEvent(resourceType string, raw *rawChatMessage) {
	prevEmotions, hadEmotions := c.emotionCache.Get(raw.ID)
	prevPost, hadPost := c.postCache.Get(raw.ID)

	msg := c.parseRawChatMessage(raw, "")
	if msg == nil {
		return
	}

	teamID := ""
	if ch := c.GetChannel(msg.ConversationID); ch != nil {
		teamID = ch.TeamID
	}

	ts := time.UnixMilli(msg.CreateAt)
	if msg.CreateAt == 0 {
		ts = time.Now()
	}

	if msg.DeleteAt > 0 {
		c.emitChatEvent(EventPostDeleted, teamID, msg, time.UnixMilli(msg.DeleteAt))

		return
	}

	if resourceType == "MessageUpdate" {
		emittedReactions := c.emitEmotionDiffs(msg, teamID, prevEmotions, hadEmotions)
		if emittedReactions && (!hadPost || (prevPost.ContentHTML == msg.ContentHTML && prevPost.EditAt == msg.EditAt)) {
			return
		}

		if msg.EditAt > 0 || (hadPost && prevPost.ContentHTML != msg.ContentHTML) {
			c.emitChatEvent(EventPostEdited, teamID, msg, ts)

			return
		}
	}

	c.emitChatEvent(EventPosted, teamID, msg, ts)
}

func (c *Client) emitChatEvent(evType, teamID string, msg *ChatMessage, ts time.Time) {
	c.emitMessage(&Message{
		Type:      evType,
		Channel:   msg.ConversationID,
		Team:      teamID,
		UserID:    msg.FromMRI,
		Username:  msg.DisplayName,
		Text:      msg.ContentText,
		Post:      msg,
		Timestamp: ts,
	})
}

func (c *Client) emitEmotionDiffs(msg *ChatMessage, teamID string, prev map[string][]ReactionUser, hadPrev bool) bool {
	if !hadPrev {
		prev = nil
	}

	emitted := false

	for key, currUsers := range msg.Emotions {
		prevUsers := prev[key]
		for _, u := range currUsers {
			if !hasReactionUser(prevUsers, u.MRI) {
				emitted = true

				c.emitReactionEvent(EventReactionAdded, teamID, msg, key, u.MRI, true, u.Time)
			}
		}
	}

	for key, prevUsers := range prev {
		currUsers := msg.Emotions[key]
		for _, u := range prevUsers {
			if !hasReactionUser(currUsers, u.MRI) {
				emitted = true

				c.emitReactionEvent(EventReactionRemoved, teamID, msg, key, u.MRI, false, time.Now().UnixMilli())
			}
		}
	}

	if msg.ID != "" {
		c.emotionCache.Add(msg.ID, msg.Emotions)
	}

	return emitted
}

func (c *Client) emitReactionEvent(evType, teamID string, msg *ChatMessage, key, userMRI string, added bool, ts int64) {
	c.emitMessage(&Message{
		Type:    evType,
		Channel: msg.ConversationID,
		Team:    teamID,
		UserID:  userMRI,
		Post:    msg,
		Reaction: &ReactionEvent{
			ConversationID: msg.ConversationID,
			MessageID:      msg.ID,
			RootMessageID:  msg.RootMessageID,
			UserMRI:        userMRI,
			Key:            key,
			Added:          added,
			Timestamp:      ts,
		},
		Timestamp: time.Now(),
	})
}

func hasReactionUser(users []ReactionUser, mri string) bool {
	for _, u := range users {
		if u.MRI == mri {
			return true
		}
	}

	return false
}

func (c *Client) handleTopicActivity(convID, fromMRI, content string) {
	topic := extractXMLTagValue(content, "value")

	c.Users.mu.Lock()
	if conv, ok := c.Users.conversations[convID]; ok {
		conv.Topic = topic
		conv.DisplayName = topic
	}
	c.Users.mu.Unlock()

	c.emitMessage(&Message{
		Type:      EventChannelTopic,
		Channel:   convID,
		UserID:    fromMRI,
		Topic:     topic,
		Text:      topic,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleMemberActivity(convID, fromMRI, content string, added bool) {
	targets := extractAllXMLTagValues(content, "target")
	if len(targets) == 0 {
		return
	}

	c.Users.mu.Lock()

	set, ok := c.Users.channelMembers[convID]
	if !ok && added {
		set = make(map[string]struct{}, len(targets))
		c.Users.channelMembers[convID] = set
	}

	for _, mri := range targets {
		if added {
			set[mri] = struct{}{}
		} else if ok {
			delete(set, mri)
		}
	}

	if conv, convOK := c.Users.conversations[convID]; convOK {
		if added {
			for _, mri := range targets {
				if !slices.Contains(conv.Members, mri) {
					conv.Members = append(conv.Members, mri)
				}
			}
		} else {
			conv.Members = slices.DeleteFunc(conv.Members, func(m string) bool {
				return slices.Contains(targets, m)
			})
		}
	}

	c.Users.mu.Unlock()

	evType := EventMemberRemoved
	if added {
		evType = EventMemberAdded
	}

	c.emitMessage(&Message{
		Type:      evType,
		Channel:   convID,
		UserID:    fromMRI,
		Members:   targets,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleTrouterConversationUpdate(resource json.RawMessage) {
	var upd trouterConvUpdate

	err := json.Unmarshal(resource, &upd)
	if err != nil || upd.ID == "" {
		return
	}

	c.Users.mu.Lock()
	if conv, ok := c.Users.conversations[upd.ID]; ok {
		if upd.ThreadProperties.Topic != "" {
			conv.Topic = upd.ThreadProperties.Topic
			conv.DisplayName = upd.ThreadProperties.Topic
		}

		if horizon := parseConsumptionHorizon(upd.Properties.ConsumptionHorizon); horizon > 0 {
			conv.LastViewedAt = horizon
			c.Users.channelLastViewedAt[upd.ID] = horizon
		}
	}
	c.Users.mu.Unlock()

	c.emitMessage(&Message{
		Type:      EventConversationUpdate,
		Channel:   upd.ID,
		Topic:     upd.ThreadProperties.Topic,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleTrouterPresenceEvent(resource json.RawMessage) {
	var entry rawPresenceEntry

	err := json.Unmarshal(resource, &entry)
	if err != nil || entry.MRI == "" {
		return
	}

	p := &Presence{
		MRI:          entry.MRI,
		Availability: entry.Presence.Availability,
		Activity:     entry.Presence.Activity,
		StatusNote:   HTMLToText(entry.Presence.StatusMessage.Message.Content),
		UpdatedAt:    time.Now().Unix(),
	}

	c.Users.mu.Lock()
	c.Users.presences[p.MRI] = p
	c.Users.mu.Unlock()

	c.emitMessage(&Message{
		Type:      EventPresenceChange,
		UserID:    p.MRI,
		Text:      MapAvailabilityToStatus(p.Availability),
		Presence:  p,
		Timestamp: time.Now(),
	})
}

func (c *Client) runPollWindow(ctx context.Context, window time.Duration) {
	deadline := time.Now().Add(window)
	ticker := time.NewTicker(trouterPollInterval)

	defer ticker.Stop()

	for time.Now().Before(deadline) {
		c.pollConversationsDelta(ctx)

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Client) pollConversationsDelta(ctx context.Context) {
	prevLastPost := make(map[string]int64)
	for _, ch := range c.GetChannels() {
		prevLastPost[ch.ID] = ch.LastPostAt
	}

	err := c.UpdateChannels(ctx)
	if err != nil {
		c.logger.Debugf("Poll fallback UpdateChannels failed: %v", err)

		return
	}

	for _, ch := range c.GetChannels() {
		prev := prevLastPost[ch.ID]
		if ch.LastPostAt <= prev || prev == 0 {
			continue
		}

		msgs, fetchErr := c.GetMessagesSince(ctx, ch.ID, prev)
		if fetchErr != nil {
			continue
		}

		for _, msg := range msgs {
			c.emitMessage(&Message{
				Type:      EventPosted,
				Channel:   msg.ConversationID,
				Team:      ch.TeamID,
				UserID:    msg.FromMRI,
				Username:  msg.DisplayName,
				Text:      msg.ContentText,
				Post:      msg,
				Timestamp: time.UnixMilli(msg.CreateAt),
			})
		}
	}
}

func (c *Client) writeWSFrame(conn *websocket.Conn, frame string) error {
	c.wsWriteMu.Lock()
	defer c.wsWriteMu.Unlock()

	_ = conn.SetWriteDeadline(time.Now().Add(trouterWriteTimeout))

	return conn.WriteMessage(websocket.TextMessage, []byte(frame))
}

func (c *Client) closeActiveWS() {
	c.wsMu.Lock()
	conn := c.wsConn
	c.wsConn = nil
	c.wsConnected.Store(false)
	c.wsMu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
}

func (c *Client) notifyWsConnected() {
	c.emitMessage(&Message{
		Type:      EventConnected,
		Timestamp: time.Now(),
	})

	if c.OnWsConnect != nil {
		c.OnWsConnect()
	}
}

func (c *Client) notifyWsDisconnected(err error) {
	c.emitMessage(&Message{
		Type:      EventDisconnected,
		Err:       err,
		Timestamp: time.Now(),
	})

	if c.OnWsDisconnect != nil {
		c.OnWsDisconnect(err)
	}
}

func (c *Client) emitMessage(msg *Message) {
	select {
	case c.MessageChan <- msg:
	default:
		if msg.Type != EventTyping && msg.Type != EventClearTyping {
			c.logger.Warnf("MessageChan full, dropping %s event for %s", msg.Type, msg.Channel)
		}
	}
}

func extractXMLTagValue(xmlStr, tag string) string {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"

	_, after, ok := strings.Cut(xmlStr, openTag)
	if !ok {
		return ""
	}

	val, _, ok := strings.Cut(after, closeTag)
	if !ok {
		return ""
	}

	return strings.TrimSpace(val)
}

func extractAllXMLTagValues(xmlStr, tag string) []string {
	openTag := "<" + tag + ">"
	closeTag := "</" + tag + ">"

	var out []string

	rest := xmlStr
	for {
		_, after, ok := strings.Cut(rest, openTag)
		if !ok {
			break
		}

		val, tail, ok := strings.Cut(after, closeTag)
		if !ok {
			break
		}

		val = strings.TrimSpace(val)
		if val != "" {
			out = append(out, val)
		}

		rest = tail
	}

	return out
}

func randomHexID(numBytes int) string {
	buf := make([]byte, numBytes)
	_, _ = rand.Read(buf)

	return hex.EncodeToString(buf)
}
