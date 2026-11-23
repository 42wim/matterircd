package googlechatclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jpillora/backoff"
)

const (
	webChannelVersion        = "8"
	webChannelClientVersion  = "22"
	webChannelKeepaliveIntvl = 2 * time.Minute
	webChannelWatchdogIntvl  = 1 * time.Minute
	webChannelIdleTimeout    = 5 * time.Minute
	pollFallbackInterval     = 5 * time.Second
	maxWebChannelChunkSize   = 8 << 20 // 8 MiB
	maxEventBackoff          = 2 * time.Minute
	minEventBackoff          = 2 * time.Second
)

func (c *Client) startWebChannel(ctx context.Context) {
	c.wg.Go(func() {
		if c.PollFallback {
			c.runPollFallback(ctx)

			return
		}

		c.webChannelLoop(ctx)
	})
}

func (c *Client) setActiveStreamCancel(cancel context.CancelFunc) {
	c.streamMu.Lock()
	c.streamCancel = cancel
	c.streamMu.Unlock()
}

func (c *Client) cancelActiveStream() {
	c.streamMu.Lock()
	cancel := c.streamCancel
	c.streamCancel = nil
	c.streamMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (c *Client) webChannelLoop(ctx context.Context) {
	b := &backoff.Backoff{
		Min:    minEventBackoff,
		Max:    maxEventBackoff,
		Factor: 2,
		Jitter: true,
	}

	reconnecting := false

	for ctx.Err() == nil {
		sessionStart := time.Now()
		err := c.runWebChannelSession(ctx, reconnecting)

		if ctx.Err() != nil {
			return
		}

		reconnecting = true

		if time.Since(sessionStart) > 30*time.Second {
			b.Reset()
		}

		wait := b.Duration()
		c.logger.Warnf("WebChannel session ended (%v); reconnecting in %s", err, wait)

		sleepErr := sleepContext(ctx, wait)
		if sleepErr != nil {
			return
		}
	}
}

type webChannelSession struct {
	sid       string
	aid       int64
	rid       atomic.Int64
	ofs       atomic.Int64
	sessionID string
}

func (c *Client) runWebChannelSession(ctx context.Context, reconnecting bool) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	c.setActiveStreamCancel(cancel)

	defer func() {
		cancel()
		c.setActiveStreamCancel(nil)
	}()

	err := c.registerWebChannel(sessionCtx)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	sid, err := c.initWebChannelSID(sessionCtx)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	sess := &webChannelSession{
		sid:       sid,
		sessionID: c.currentCSessionID(),
	}
	sess.rid.Store(1)

	err = c.sendStreamEvent(sessionCtx, sess, encodeStreamPingRequest(true))
	if err != nil {
		return fmt.Errorf("initial ping: %w", err)
	}

	if refs := c.allGroupRefs(); len(refs) > 0 {
		subErr := c.sendStreamEvent(sessionCtx, sess, encodeStreamGroupSubscriptionRequest(refs))
		if subErr != nil {
			c.logger.Warnf("WebChannel group subscription failed: %v", subErr)
		}
	}

	c.markStreamConnected(reconnecting)

	if reconnecting && c.ForceSyncOnReconnect {
		syncErr := c.UpdateChannels(sessionCtx)
		if syncErr != nil {
			c.logger.Warnf("Channel resync on reconnect failed: %v", syncErr)
		}
	}

	keepaliveErrCh := make(chan error, 1)

	go c.runWebChannelKeepalive(sessionCtx, sess, keepaliveErrCh)

	streamErr := c.streamWebChannelLoop(sessionCtx, sess, keepaliveErrCh)
	c.markStreamDisconnected(streamErr)

	return streamErr
}

func (c *Client) markStreamConnected(reconnecting bool) {
	c.lastWsActivity.Store(time.Now().UnixNano())
	c.wsConnected.Store(true)

	if reconnecting {
		c.logger.Info("Google Chat WebChannel reconnected")
	} else {
		c.logger.Info("Google Chat WebChannel connected")
	}

	c.emitMessage(&Message{
		Type:      EventConnected,
		Timestamp: time.Now(),
	})

	if c.OnWsConnect != nil {
		c.OnWsConnect()
	}
}

func (c *Client) markStreamDisconnected(err error) {
	if !c.wsConnected.Swap(false) {
		return
	}

	c.emitMessage(&Message{
		Type:      EventDisconnected,
		Timestamp: time.Now(),
		Err:       err,
	})

	if c.OnWsDisconnect != nil {
		c.OnWsDisconnect(err)
	}
}

func (c *Client) currentCSessionID() string {
	c.authMu.RLock()
	defer c.authMu.RUnlock()

	return c.tokenState.CSessionID
}

func (c *Client) registerWebChannel(ctx context.Context) error {
	targetURL := c.chatBaseURL() + "/webchannel/register"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, http.NoBody)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-protobuf")
	c.applyAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		refreshErr := c.handleUnauthorized(ctx)
		if refreshErr != nil {
			return fmt.Errorf("HTTP %d (refresh failed: %w)", resp.StatusCode, refreshErr)
		}

		return fmt.Errorf("HTTP %d (refreshed XSRF, retrying)", resp.StatusCode)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) initWebChannelSID(ctx context.Context) (string, error) {
	params := url.Values{}
	params.Set("VER", webChannelVersion)
	params.Set("RID", "0")
	params.Set("CVER", webChannelClientVersion)
	params.Set("TYPE", "init")
	params.Set("$req", "count=0")
	params.Set("SID", "null")
	params.Set("t", "1")

	if csid := c.currentCSessionID(); csid != "" {
		params.Set("csessionid", csid)
	}

	targetURL := c.chatBaseURL() + "/webchannel/events_encoded?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, http.NoBody)
	if err != nil {
		return "", err
	}

	c.applyAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	headerVal := resp.Header.Get("X-Http-Initial-Response")
	if sid := extractSIDFromInitPayload(headerVal); sid != "" {
		return sid, nil
	}

	if sid := extractSIDFromInitPayload(string(body)); sid != "" {
		return sid, nil
	}

	return "", errors.New("missing SID in WebChannel init response")
}

// extractSIDFromInitPayload parses Google's WebChannel init JSON array:
// [[0,["c","<SID>","",8,12,30000]]]
func extractSIDFromInitPayload(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	// Skip optional decimal length prefix line if parsed from body instead of header.
	if idx := strings.IndexByte(trimmed, '['); idx > 0 {
		trimmed = trimmed[idx:]
	}

	var outer [][]json.RawMessage

	err := json.Unmarshal([]byte(trimmed), &outer)
	if err != nil {
		return ""
	}

	for _, entry := range outer {
		if len(entry) < 2 {
			continue
		}

		var ctrl []any

		ctrlErr := json.Unmarshal(entry[1], &ctrl)
		if ctrlErr != nil || len(ctrl) < 2 {
			continue
		}

		kind, _ := ctrl[0].(string)
		sid, _ := ctrl[1].(string)

		if kind == "c" && sid != "" {
			return sid
		}
	}

	return ""
}

func (c *Client) sendStreamEvent(ctx context.Context, sess *webChannelSession, protoBytes []byte) error {
	params := url.Values{}
	params.Set("VER", webChannelVersion)
	params.Set("RID", strconv.FormatInt(sess.rid.Add(1), 10))
	params.Set("SID", sess.sid)
	params.Set("AID", strconv.FormatInt(sess.aid, 10))
	params.Set("CI", "0")
	params.Set("t", "1")

	if sess.sessionID != "" {
		params.Set("csessionid", sess.sessionID)
	}

	payloadJSON, err := json.Marshal(struct {
		Data string `json:"data"`
	}{
		Data: base64.StdEncoding.EncodeToString(protoBytes),
	})
	if err != nil {
		return err
	}

	form := url.Values{}
	form.Set("count", "1")
	form.Set("ofs", strconv.FormatInt(sess.ofs.Add(1)-1, 10))
	form.Set("req0___data__", string(payloadJSON))

	targetURL := c.chatBaseURL() + "/webchannel/events_encoded?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	c.applyAuthHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return nil
}

func (c *Client) runWebChannelKeepalive(ctx context.Context, sess *webChannelSession, errCh chan<- error) {
	pingTicker := time.NewTicker(webChannelKeepaliveIntvl)
	defer pingTicker.Stop()

	watchdogTicker := time.NewTicker(webChannelWatchdogIntvl)
	defer watchdogTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			pingErr := c.sendStreamEvent(ctx, sess, encodeStreamPingRequest(true))
			if pingErr != nil {
				select {
				case errCh <- fmt.Errorf("keepalive ping: %w", pingErr):
				default:
				}

				return
			}

			_, _ = c.doProto(ctx, "heartbeat", encodeHeartbeatRequest(true))
		case <-watchdogTicker.C:
			lastNano := c.lastWsActivity.Load()
			if lastNano > 0 && time.Since(time.Unix(0, lastNano)) > webChannelIdleTimeout {
				select {
				case errCh <- errors.New("WebChannel watchdog idle timeout"):
				default:
				}

				return
			}
		}
	}
}

func (c *Client) streamWebChannelLoop(ctx context.Context, sess *webChannelSession, keepaliveErrCh <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-keepaliveErrCh:
			return err
		default:
		}

		err := c.readWebChannelLongPoll(ctx, sess)
		if err != nil {
			select {
			case kErr := <-keepaliveErrCh:
				return kErr
			default:
				return err
			}
		}
	}
}

func (c *Client) readWebChannelLongPoll(ctx context.Context, sess *webChannelSession) error {
	params := url.Values{}
	params.Set("VER", webChannelVersion)
	params.Set("RID", "rpc")
	params.Set("SID", sess.sid)
	params.Set("AID", strconv.FormatInt(sess.aid, 10))
	params.Set("CI", "0")
	params.Set("t", "1")
	params.Set("TYPE", "xmlhttp")

	if sess.sessionID != "" {
		params.Set("csessionid", sess.sessionID)
	}

	targetURL := c.chatBaseURL() + "/webchannel/events_encoded?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, http.NoBody)
	if err != nil {
		return err
	}

	c.applyAuthHeaders(req)

	resp, err := c.streamClient.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	c.absorbResponseCookies(resp.Cookies())

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("long-poll HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	return c.consumeWebChannelChunks(ctx, sess, resp.Body)
}

func (c *Client) consumeWebChannelChunks(ctx context.Context, sess *webChannelSession, r io.Reader) error {
	reader := bufio.NewReaderSize(r, 64*1024)

	for ctx.Err() == nil {
		line, err := reader.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return err
		}

		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		chunkLen, parseErr := strconv.Atoi(trimmed)
		if parseErr != nil || chunkLen <= 0 || chunkLen > maxWebChannelChunkSize {
			// Some Google WebChannel responses emit raw JSON without a length prefix.
			if strings.HasPrefix(trimmed, "[") {
				c.processWebChannelChunk(sess, []byte(trimmed))
			}

			continue
		}

		buf := make([]byte, chunkLen)

		_, readErr := io.ReadFull(reader, buf)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil
			}

			return readErr
		}

		c.lastWsActivity.Store(time.Now().UnixNano())
		c.processWebChannelChunk(sess, buf)
	}

	return ctx.Err()
}

func (c *Client) processWebChannelChunk(sess *webChannelSession, raw []byte) {
	var frames [][]json.RawMessage

	err := json.Unmarshal(bytes.TrimSpace(raw), &frames)
	if err != nil {
		c.logger.Debugf("Ignoring unparseable WebChannel frame: %v", err)

		return
	}

	for _, frame := range frames {
		if len(frame) < 2 {
			continue
		}

		var aid int64
		if json.Unmarshal(frame[0], &aid) == nil && aid > sess.aid {
			sess.aid = aid
		}

		c.processWebChannelFramePayload(frame[1])
	}
}

func (c *Client) processWebChannelFramePayload(payload json.RawMessage) {
	// Control arrays look like ["noop"] or ["stop"].
	var envelopes []struct {
		Data string `json:"data"`
	}

	err := json.Unmarshal(payload, &envelopes)
	if err != nil {
		return
	}

	for _, env := range envelopes {
		if env.Data == "" {
			continue
		}

		rawProto, decErr := decodeBase64Flexible([]byte(env.Data))
		if decErr != nil || len(rawProto) == 0 {
			continue
		}

		ev, protoErr := decodeStreamEventsResponse(rawProto)
		if protoErr != nil || ev == nil {
			continue
		}

		c.handleProtoEvent(ev)
	}
}

func (c *Client) rememberGroupRef(ref protoGroupRef) {
	if !ref.IsDM || ref.ID == "" {
		return
	}

	c.Users.mu.Lock()
	c.Users.dmGroupIDs[ref.ID] = struct{}{}
	c.Users.mu.Unlock()
}

func (c *Client) handleProtoEvent(ev *protoEvent) {
	if ev == nil {
		return
	}

	c.rememberGroupRef(ev.Group)

	if ev.WorldItem != nil {
		c.upsertWorldItem(ev.WorldItem)
	}

	for i := range ev.Bodies {
		c.handleProtoEventBody(ev, &ev.Bodies[i])
	}
}

func (c *Client) handleProtoEventBody(ev *protoEvent, body *protoEventBody) {
	evType := body.EventType
	if evType == 0 {
		evType = ev.Type
	}

	switch evType {
	case protoEventMessagePosted, protoEventMessageUpdated:
		c.handleMessageEventBody(ev, body, evType == protoEventMessageUpdated)
	case protoEventTopicCreated:
		c.handleTopicCreatedBody(ev, body)
	case protoEventMessageDeleted:
		c.handleMessageDeletedBody(ev, body)
	case protoEventMessageReacted:
		c.handleReactionBody(ev, body)
	case protoEventBatchReactions:
		c.handleBatchReactionsBody(ev, body)
	case protoEventTypingChanged:
		c.handleTypingBody(ev, body)
	case protoEventGroupUpdated:
		c.handleGroupUpdatedBody(body)
	case protoEventMembershipChanged:
		c.handleMembershipChangedBody(ev, body)
	case protoEventUserStatusUpdated:
		c.handleUserStatusBody(body)
	case protoEventGroupViewed, protoEventGroupReadState:
		c.handleReadStateBody(ev, body)
	}
}

func (c *Client) handleMessageEventBody(ev *protoEvent, body *protoEventBody, isEdit bool) {
	if body.MessageGroup != nil {
		c.upsertConversationFromProto(body.MessageGroup)
	}

	if body.MessagePosted == nil {
		return
	}

	c.rememberGroupRef(body.MessagePosted.Group)

	msg := c.protoMessageToChatMessage(body.MessagePosted, ev.Group.ID, "")
	if msg == nil {
		return
	}

	if isEdit && msg.EditAt == 0 {
		msg.EditAt = time.Now().UnixMilli()
	}

	c.cachePost(msg)

	evType := EventPosted
	if isEdit || msg.EditAt > 0 {
		evType = EventPostEdited
	}

	c.emitMessage(&Message{
		Type:      evType,
		Channel:   msg.ConversationID,
		UserID:    msg.FromID,
		Username:  msg.DisplayName,
		Text:      msg.ContentText,
		Post:      msg,
		Timestamp: timeFromMillis(msg.CreateAt),
	})
}

func (c *Client) handleTopicCreatedBody(ev *protoEvent, body *protoEventBody) {
	if body.TopicCreated == nil {
		return
	}

	c.rememberGroupRef(body.TopicCreated.Group)

	convID := body.TopicCreated.Group.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	for _, pm := range body.TopicCreated.Replies {
		msg := c.protoMessageToChatMessage(pm, convID, body.TopicCreated.TopicID)
		if msg == nil {
			continue
		}

		c.cachePost(msg)
		c.emitMessage(&Message{
			Type:      EventPosted,
			Channel:   msg.ConversationID,
			UserID:    msg.FromID,
			Username:  msg.DisplayName,
			Text:      msg.ContentText,
			Post:      msg,
			Timestamp: timeFromMillis(msg.CreateAt),
		})
	}
}

func (c *Client) handleMessageDeletedBody(ev *protoEvent, body *protoEventBody) {
	convID := body.DeletedGroup.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	msgID := body.DeletedMsgID
	if msgID == "" {
		return
	}

	userID := ev.UserID
	if cached := c.GetCachedPost(msgID); cached != nil && userID == "" {
		userID = cached.FromID
	}

	c.emitMessage(&Message{
		Type:    EventPostDeleted,
		Channel: convID,
		UserID:  userID,
		Post: &ChatMessage{
			ID:             msgID,
			TopicID:        body.DeletedTopicID,
			ConversationID: convID,
			FromID:         userID,
			DeleteAt:       time.Now().UnixMilli(),
		},
		Timestamp: time.Now(),
	})
}

func (c *Client) handleReactionBody(ev *protoEvent, body *protoEventBody) {
	convID := body.ReactGroup.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	if body.ReactMsgID == "" || body.ReactEmoji == "" {
		return
	}

	added := body.ReactOption != protoReactionRemove

	evType := EventReactionAdded
	if !added {
		evType = EventReactionRemoved
	}

	userID := body.ReactUserID
	if userID == "" {
		userID = ev.UserID
	}

	ts := usecToMillis(body.ReactTimeUsec)
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}

	rootID := ""
	if body.ReactTopicID != "" && body.ReactTopicID != body.ReactMsgID {
		rootID = body.ReactTopicID
	}

	c.emitMessage(&Message{
		Type:    evType,
		Channel: convID,
		UserID:  userID,
		Reaction: &ReactionEvent{
			ConversationID: convID,
			TopicID:        body.ReactTopicID,
			MessageID:      body.ReactMsgID,
			RootMessageID:  rootID,
			UserID:         userID,
			Key:            body.ReactEmoji,
			Added:          added,
			Timestamp:      ts,
		},
		Timestamp: timeFromMillis(ts),
	})
}

func (c *Client) handleBatchReactionsBody(ev *protoEvent, body *protoEventBody) {
	if body.BatchMsgID == "" || len(body.BatchSummaries) == 0 {
		return
	}

	convID := body.BatchGroup.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	ts := usecToMillis(body.BatchTimeUsec)
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}

	emittedExplicit := c.emitExplicitBatchReactionDeltas(convID, body, ts)
	if emittedExplicit {
		return
	}

	next := buildBatchReactionMap(body.BatchSummaries, ts)
	prev, _ := c.reactionCache.Get(body.BatchMsgID)
	c.reactionCache.Add(body.BatchMsgID, cloneReactionsMap(next))

	added, removed := mergeReactionsDiff(prev, next)

	c.emitReactionMapEvents(convID, body.BatchTopicID, body.BatchMsgID, added, true, ts)
	c.emitReactionMapEvents(convID, body.BatchTopicID, body.BatchMsgID, removed, false, ts)
}

func (c *Client) emitExplicitBatchReactionDeltas(convID string, body *protoEventBody, ts int64) bool {
	emitted := false

	for _, s := range body.BatchSummaries {
		if s.Emoji == "" {
			continue
		}

		for _, uid := range s.UsersAdded {
			emitted = true

			c.emitSingleReaction(convID, body.BatchTopicID, body.BatchMsgID, uid, s.Emoji, true, ts)
		}

		for _, uid := range s.UsersRemoved {
			emitted = true

			c.emitSingleReaction(convID, body.BatchTopicID, body.BatchMsgID, uid, s.Emoji, false, ts)
		}
	}

	return emitted
}

func buildBatchReactionMap(summaries []protoReactionSummary, ts int64) map[string][]ReactionUser {
	out := make(map[string][]ReactionUser, len(summaries))

	for _, s := range summaries {
		if s.Emoji == "" {
			continue
		}

		users := make([]ReactionUser, 0, len(s.Reactors))
		for _, uid := range s.Reactors {
			users = append(users, ReactionUser{UserID: uid, Time: ts})
		}

		out[s.Emoji] = users
	}

	return out
}

func (c *Client) emitReactionMapEvents(convID, topicID, msgID string, changes map[string][]string, added bool, ts int64) {
	for emoji, users := range changes {
		for _, uid := range users {
			c.emitSingleReaction(convID, topicID, msgID, uid, emoji, added, ts)
		}
	}
}

func (c *Client) emitSingleReaction(convID, topicID, msgID, userID, emoji string, added bool, ts int64) {
	evType := EventReactionAdded
	if !added {
		evType = EventReactionRemoved
	}

	rootID := ""
	if topicID != "" && topicID != msgID {
		rootID = topicID
	}

	c.emitMessage(&Message{
		Type:    evType,
		Channel: convID,
		UserID:  userID,
		Reaction: &ReactionEvent{
			ConversationID: convID,
			TopicID:        topicID,
			MessageID:      msgID,
			RootMessageID:  rootID,
			UserID:         userID,
			Key:            emoji,
			Added:          added,
			Timestamp:      ts,
		},
		Timestamp: timeFromMillis(ts),
	})
}

func (c *Client) handleTypingBody(ev *protoEvent, body *protoEventBody) {
	convID := body.TypingGroup.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	userID := body.TypingUserID
	if userID == "" {
		userID = ev.UserID
	}

	if convID == "" || userID == "" || userID == c.selfUserID() {
		return
	}

	evType := EventTyping
	if body.TypingState == protoTypingStopped {
		evType = EventClearTyping
	}

	c.emitMessage(&Message{
		Type:      evType,
		Channel:   convID,
		UserID:    userID,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleGroupUpdatedBody(body *protoEventBody) {
	if body.GroupUpdated == nil || body.GroupUpdated.Group.ID == "" {
		return
	}

	prev := c.GetChannel(body.GroupUpdated.Group.ID)
	updated := c.upsertConversationFromProto(body.GroupUpdated)

	if updated == nil {
		return
	}

	if prev != nil && updated.Topic != prev.Topic {
		c.emitMessage(&Message{
			Type:      EventChannelTopic,
			Channel:   updated.ID,
			Topic:     updated.Topic,
			Timestamp: time.Now(),
		})
	}

	c.emitMessage(&Message{
		Type:      EventConversationUpdate,
		Channel:   updated.ID,
		Timestamp: time.Now(),
	})

	if c.OnChannelNameResolved != nil && updated.DisplayName != "" {
		c.OnChannelNameResolved(updated.ID, updated.DisplayName)
	}
}

func (c *Client) handleMembershipChangedBody(ev *protoEvent, body *protoEventBody) {
	if body.Membership == nil || body.Membership.UserID == "" {
		return
	}

	c.rememberGroupRef(body.Membership.Group)

	convID := body.Membership.Group.ID
	if convID == "" {
		convID = ev.Group.ID
	}

	if convID == "" {
		return
	}

	var evType string

	switch body.Membership.State {
	case protoMembershipJoined:
		isAdmin := body.Membership.Role == protoRoleOwner || body.Membership.Role == protoRoleManager
		c.addChannelMemberLocal(convID, body.Membership.UserID, isAdmin)

		evType = EventMemberAdded
	case protoMembershipNotAMember:
		c.removeChannelMemberLocal(convID, body.Membership.UserID)

		evType = EventMemberRemoved
	default:
		return
	}

	c.emitMessage(&Message{
		Type:      evType,
		Channel:   convID,
		UserID:    body.Membership.UserID,
		Members:   []string{body.Membership.UserID},
		Timestamp: time.Now(),
	})
}

func (c *Client) handleUserStatusBody(body *protoEventBody) {
	p := c.applyUserStatusUpdate(body.UserStatus)
	if p == nil {
		return
	}

	c.emitMessage(&Message{
		Type:      EventPresenceChange,
		UserID:    p.UserID,
		Presence:  p,
		Timestamp: time.Now(),
	})
}

func (c *Client) handleReadStateBody(ev *protoEvent, body *protoEventBody) {
	convID := body.GroupViewed.ID
	if convID == "" {
		convID = body.ReadStateGroup.ID
	}

	if convID == "" {
		convID = ev.Group.ID
	}

	usec := max(body.ViewTimeUsec, body.ReadTimeUsec)

	c.updateChannelLastViewed(convID, usecToMillis(usec))
}

func (c *Client) runPollFallback(ctx context.Context) {
	c.markStreamConnected(false)

	defer c.markStreamDisconnected(ctx.Err())

	lastSeen := make(map[string]time.Time)
	start := time.Now()

	for _, conv := range c.GetChannels() {
		lastSeen[conv.ID] = start
	}

	ticker := time.NewTicker(pollFallbackInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollFallbackTick(ctx, lastSeen)
		}
	}
}

func (c *Client) pollFallbackTick(ctx context.Context, lastSeen map[string]time.Time) {
	c.lastWsActivity.Store(time.Now().UnixNano())

	for _, conv := range c.GetChannels() {
		if ctx.Err() != nil {
			return
		}

		if conv.IsHidden {
			continue
		}

		since, ok := lastSeen[conv.ID]
		if !ok {
			since = time.Now()
			lastSeen[conv.ID] = since

			continue
		}

		msgs, err := c.GetMessagesSince(ctx, conv.ID, since, 20)
		if err != nil {
			continue
		}

		for _, msg := range msgs {
			msgTime := timeFromMillis(msg.CreateAt)
			if !msgTime.After(since) {
				continue
			}

			if msgTime.After(lastSeen[conv.ID]) {
				lastSeen[conv.ID] = msgTime
			}

			c.emitMessage(&Message{
				Type:      EventPosted,
				Channel:   msg.ConversationID,
				UserID:    msg.FromID,
				Username:  msg.DisplayName,
				Text:      msg.ContentText,
				Post:      msg,
				Timestamp: msgTime,
			})
		}
	}
}

func (c *Client) emitMessage(msg *Message) {
	if msg == nil {
		return
	}

	select {
	case c.MessageChan <- msg:
	default:
		c.logger.Warnf("MessageChan full; dropping %s event", msg.Type)
	}
}

func timeFromMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Now()
	}

	return time.UnixMilli(ms)
}
