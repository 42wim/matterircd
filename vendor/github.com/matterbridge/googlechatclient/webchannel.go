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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jpillora/backoff"
	"github.com/sirupsen/logrus"
)

const (
	webChannelVersion        = "8"
	webChannelClientVersion  = "22"
	webChannelKeepaliveIntvl = 2 * time.Minute
	webChannelWatchdogIntvl  = 30 * time.Second
	webChannelSuspendDrift   = 15 * time.Second
	webChannelIdleTimeout    = 5 * time.Minute
	pollFallbackInterval     = 5 * time.Second
	maxWebChannelChunkSize   = 8 << 20 // 8 MiB
	maxEventBackoff          = 2 * time.Minute
	minEventBackoff          = 2 * time.Second
	streamGroupSubBatchSize  = 50
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
	sid        string
	aid        atomic.Int64
	rid        atomic.Int64
	ofs        atomic.Int64
	sessionID  string
	subMu      sync.Mutex
	subscribed map[string]struct{}
}

func (c *Client) setActiveSession(sess *webChannelSession) {
	c.streamMu.Lock()
	c.activeSess = sess
	c.streamMu.Unlock()
}

func (c *Client) currentActiveSession() *webChannelSession {
	c.streamMu.Lock()
	defer c.streamMu.Unlock()

	return c.activeSess
}

func (c *Client) subscribeGroups(ctx context.Context, sess *webChannelSession, refs []protoGroupRef) {
	if sess == nil || len(refs) == 0 {
		return
	}

	var pending []protoGroupRef

	sess.subMu.Lock()

	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}

		if _, ok := sess.subscribed[ref.ID]; ok {
			continue
		}

		sess.subscribed[ref.ID] = struct{}{}
		pending = append(pending, ref)
	}

	sess.subMu.Unlock()

	for chunk := range slices.Chunk(pending, streamGroupSubBatchSize) {
		kind := "subscribe"
		if c.apiTraceEnabled() {
			kind = fmt.Sprintf("subscribe(%d)", len(chunk))
		}

		subErr := c.sendStreamEvent(ctx, sess, kind, encodeStreamGroupSubscriptionRequest(chunk))
		if subErr != nil {
			c.logger.Warnf("WebChannel group subscription failed: %v", subErr)

			sess.subMu.Lock()

			for _, ref := range chunk {
				delete(sess.subscribed, ref.ID)
			}

			sess.subMu.Unlock()
		}
	}
}

func (c *Client) subscribeGroup(ctx context.Context, ref protoGroupRef) {
	if ref.ID == "" {
		return
	}

	sess := c.currentActiveSession()
	if sess == nil {
		return
	}

	c.subscribeGroups(ctx, sess, []protoGroupRef{ref})
}

// SubscribeConversation subscribes the active WebChannel stream to real-time
// group events (including typing indicators) for convID.
func (c *Client) SubscribeConversation(ctx context.Context, convID string) {
	if convID == "" {
		return
	}

	c.subscribeGroup(ctx, c.protoGroupRefFor(convID))
}

func (c *Client) runWebChannelSession(ctx context.Context, reconnecting bool) error {
	sessionCtx, cancel := context.WithCancel(ctx)
	c.setActiveStreamCancel(cancel)

	defer func() {
		cancel()
		c.setActiveSession(nil)
		c.setActiveStreamCancel(nil)
	}()

	if reconnecting {
		// Evict any dead half-open HTTP/2 connections left over from a network
		// drop or laptop suspend so registerWebChannel dials a fresh socket.
		c.closeIdleConnections()
		c.rotateCookiesIfStale(sessionCtx)
	}

	err := c.registerWebChannel(sessionCtx)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	sid, err := c.initWebChannelSID(sessionCtx)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	sess := &webChannelSession{
		sid:        sid,
		sessionID:  c.currentCSessionID(),
		subscribed: make(map[string]struct{}),
	}
	sess.rid.Store(1)

	err = c.sendStreamEvent(sessionCtx, sess, "initial_ping", encodeStreamPingRequest(true))
	if err != nil {
		return fmt.Errorf("initial ping: %w", err)
	}

	c.setActiveSession(sess)
	c.subscribeGroups(sessionCtx, sess, c.allGroupRefs())

	c.markStreamConnected(sessionCtx, sess, reconnecting)

	keepaliveErrCh := make(chan error, 1)

	go c.runWebChannelKeepalive(sessionCtx, cancel, sess, keepaliveErrCh)

	streamErr := c.streamWebChannelLoop(sessionCtx, sess, keepaliveErrCh)
	c.markStreamDisconnected(streamErr)

	return streamErr
}

func (c *Client) rotateCookiesIfStale(ctx context.Context) {
	c.authMu.RLock()
	updatedAt := c.tokenState.UpdatedAt
	c.authMu.RUnlock()

	if !updatedAt.IsZero() && time.Since(updatedAt.Round(0)) < cookieRotateInterval {
		return
	}

	err := c.RotateCookies(ctx)
	if err != nil && ctx.Err() == nil {
		c.logger.Debugf("Reconnect RotateCookies failed: %v", err)
	}
}

func (c *Client) markStreamConnected(sessionCtx context.Context, sess *webChannelSession, reconnecting bool) {
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

	if reconnecting {
		c.wg.Go(func() {
			c.resyncAfterReconnect(sessionCtx, sess)
		})

		return
	}

	if c.OnWsConnect != nil {
		c.wg.Go(func() {
			c.OnWsConnect()
		})
	}
}

// resyncAfterReconnect refreshes conversations (updating LastPostAt and
// discovering newly created DMs/Spaces) before invoking OnWsConnect, matching
// matterclient.resyncAfterWsReconnect so history replay does not skip channels
// with stale LastPostAt timestamps.
func (c *Client) resyncAfterReconnect(sessionCtx context.Context, sess *webChannelSession) {
	const maxAttempts = 3

	var err error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = c.UpdateChannels(sessionCtx)
		if err == nil {
			break
		}

		if sessionCtx.Err() != nil {
			return
		}

		c.logger.Warnf("WebChannel reconnect: updating channels failed %d/%d: %v", attempt, maxAttempts, err)

		if attempt == maxAttempts {
			break
		}

		select {
		case <-sessionCtx.Done():
			return
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}

	if err != nil {
		if sessionCtx.Err() != nil || c.currentActiveSession() != sess {
			return
		}

		c.logger.Warn("WebChannel reconnect: channel refresh failed, reconnecting stream")
		c.Reconnect()

		return
	}

	if sessionCtx.Err() != nil || c.currentActiveSession() != sess {
		return
	}

	c.subscribeGroups(sessionCtx, sess, c.allGroupRefs())

	if sessionCtx.Err() != nil || c.currentActiveSession() != sess {
		return
	}

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

	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateBody(body))
	}

	headerVal := resp.Header.Get("X-Http-Initial-Response")
	if sid := extractSIDFromInitPayload(headerVal); sid != "" {
		if c.apiTraceEnabled() {
			c.apiLogger.Trace("webchannel init: sid received (source=header)")
		}

		return sid, nil
	}

	sid, err := readSIDFromInitBody(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	if sid != "" {
		if c.apiTraceEnabled() {
			c.apiLogger.Trace("webchannel init: sid received (source=body)")
		}

		return sid, nil
	}

	return "", errors.New("missing SID in WebChannel init response")
}

// readSIDFromInitBody reads the WebChannel init response incrementally so we
// return as soon as the first frame yields a SID instead of blocking on
// io.ReadAll until Google closes the chunked stream (~60s).
func readSIDFromInitBody(r io.Reader) (string, error) {
	const maxInitFrames = 16

	reader := bufio.NewReaderSize(r, 8*1024)

	for range maxInitFrames {
		frame, eof, err := readInitFrame(reader)
		if err != nil {
			return "", err
		}

		if sid := extractSIDFromInitPayload(frame); sid != "" {
			return sid, nil
		}

		if eof {
			return "", nil
		}
	}

	return "", nil
}

func readInitFrame(reader *bufio.Reader) (string, bool, error) {
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, err
	}

	eof := errors.Is(err, io.EOF)
	trimmed := strings.TrimSpace(line)

	if trimmed == "" {
		return "", eof, nil
	}

	chunkLen, parseErr := strconv.Atoi(trimmed)
	if parseErr == nil && chunkLen > 0 && chunkLen <= maxWebChannelChunkSize {
		buf := make([]byte, chunkLen)

		_, readErr := io.ReadFull(reader, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return "", false, readErr
		}

		return string(buf), readErr != nil, nil
	}

	return trimmed, eof, nil
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

func (c *Client) sendStreamEvent(ctx context.Context, sess *webChannelSession, kind string, protoBytes []byte) error {
	aid := sess.aid.Load()

	params := url.Values{}
	params.Set("VER", webChannelVersion)
	params.Set("RID", strconv.FormatInt(sess.rid.Add(1), 10))
	params.Set("SID", sess.sid)
	params.Set("AID", strconv.FormatInt(aid, 10))
	params.Set("CI", "0")
	params.Set("t", "1")

	if sess.sessionID != "" {
		params.Set("csessionid", sess.sessionID)
	}

	if c.apiTraceEnabled() {
		c.apiLogger.Tracef("webchannel send: %s (aid=%d, bytes=%d)", kind, aid, len(protoBytes))
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

func (c *Client) runWebChannelKeepalive(ctx context.Context, cancel context.CancelFunc, sess *webChannelSession, errCh chan<- error) {
	pingTicker := time.NewTicker(webChannelKeepaliveIntvl)
	defer pingTicker.Stop()

	watchdogTicker := time.NewTicker(webChannelWatchdogIntvl)
	defer watchdogTicker.Stop()

	// Strip monotonic reading via .Round(0) so wall-clock jumps across laptop
	// suspend/resume are detected immediately on the first tick after wake.
	prevWall := time.Now().Round(0)

	abort := func(err error) {
		select {
		case errCh <- err:
		default:
		}

		cancel()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-pingTicker.C:
			pingErr := c.sendStreamEvent(ctx, sess, "ping", encodeStreamPingRequest(true))
			if pingErr != nil {
				abort(fmt.Errorf("keepalive ping: %w", pingErr))

				return
			}

			_, _ = c.doProto(ctx, "heartbeat", encodeHeartbeatRequest(true))
		case <-watchdogTicker.C:
			nowWall := time.Now().Round(0)
			wallDelta := nowWall.Sub(prevWall)
			prevWall = nowWall

			if wallDelta > webChannelWatchdogIntvl+webChannelSuspendDrift {
				abort(fmt.Errorf("WebChannel wall-clock jump detected (%s, system suspend/resume)", wallDelta.Round(time.Second)))

				return
			}

			lastNano := c.lastWsActivity.Load()
			if lastNano > 0 && time.Since(time.Unix(0, lastNano)) > webChannelIdleTimeout {
				abort(errors.New("WebChannel watchdog idle timeout"))

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

type webChannelPollStats struct {
	noop   int
	stop   int
	msg    int
	edit   int
	del    int
	react  int
	typing int
	status int
	group  int
	member int
	read   int
	world  int
	other  int
}

func (c *Client) apiTraceEnabled() bool {
	return c.apiLogger != nil && c.apiLogger.Logger.IsLevelEnabled(logrus.TraceLevel)
}

func (c *Client) logWebChannelPollSummary(duration time.Duration, aid int64, stats *webChannelPollStats) {
	if stats == nil || !c.apiTraceEnabled() {
		return
	}

	c.apiLogger.Tracef(
		"webchannel poll (%s, aid=%d): noop=%d msg=%d edit=%d del=%d react=%d typing=%d status=%d group=%d member=%d read=%d world=%d other=%d",
		duration.Round(time.Millisecond), aid,
		stats.noop, stats.msg, stats.edit, stats.del, stats.react,
		stats.typing, stats.status, stats.group, stats.member,
		stats.read, stats.world, stats.other,
	)
}

func (c *Client) readWebChannelLongPoll(ctx context.Context, sess *webChannelSession) error {
	params := url.Values{}
	params.Set("VER", webChannelVersion)
	params.Set("RID", "rpc")
	params.Set("SID", sess.sid)
	params.Set("AID", strconv.FormatInt(sess.aid.Load(), 10))
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

	pollStart := time.Now()

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

	var stats webChannelPollStats

	err = c.consumeWebChannelChunks(ctx, sess, resp.Body, &stats)
	c.logWebChannelPollSummary(time.Since(pollStart), sess.aid.Load(), &stats)

	return err
}

func (c *Client) consumeWebChannelChunks(ctx context.Context, sess *webChannelSession, r io.Reader, stats *webChannelPollStats) error {
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
				c.processWebChannelChunk(sess, []byte(trimmed), stats)
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
		c.processWebChannelChunk(sess, buf, stats)
	}

	return ctx.Err()
}

func (c *Client) processWebChannelChunk(sess *webChannelSession, raw []byte, stats *webChannelPollStats) {
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
		if json.Unmarshal(frame[0], &aid) == nil {
			for cur := sess.aid.Load(); aid > cur; cur = sess.aid.Load() {
				if sess.aid.CompareAndSwap(cur, aid) {
					break
				}
			}
		}

		c.processWebChannelFramePayload(frame[1], stats)
	}
}

func (c *Client) processWebChannelFramePayload(payload json.RawMessage, stats *webChannelPollStats) {
	// Control arrays look like ["noop"] or ["stop"].
	var envelopes []struct {
		Data string `json:"data"`
	}

	err := json.Unmarshal(payload, &envelopes)
	if err != nil {
		c.recordWebChannelControl(payload, stats)

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

		c.handleProtoEvent(ev, stats)
	}
}

func (c *Client) recordWebChannelControl(payload json.RawMessage, stats *webChannelPollStats) {
	var ctrl []string

	err := json.Unmarshal(payload, &ctrl)
	if err != nil || len(ctrl) == 0 {
		return
	}

	switch ctrl[0] {
	case "noop":
		if stats != nil {
			stats.noop++
		}
	case "stop":
		if stats != nil {
			stats.stop++
		}

		if c.apiTraceEnabled() {
			c.apiLogger.Trace("webchannel recv: control stop")
		}
	default:
		if stats != nil {
			stats.other++
		}

		if c.apiTraceEnabled() {
			c.apiLogger.Tracef("webchannel recv: control %s", ctrl[0])
		}
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

func (c *Client) handleProtoEvent(ev *protoEvent, stats *webChannelPollStats) {
	if ev == nil {
		return
	}

	c.rememberGroupRef(ev.Group)

	if ev.WorldItem != nil {
		if stats != nil {
			stats.world++
		}

		c.upsertWorldItem(ev.WorldItem)
	}

	for i := range ev.Bodies {
		c.handleProtoEventBody(ev, &ev.Bodies[i], stats)
	}
}

func (c *Client) handleProtoEventBody(ev *protoEvent, body *protoEventBody, stats *webChannelPollStats) {
	evType := body.EventType
	if evType == 0 {
		evType = ev.Type
	}

	recordProtoEventBody(evType, stats)

	if c.apiTraceEnabled() {
		c.traceProtoEventBody(ev, body, evType)
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

func recordProtoEventBody(evType int32, stats *webChannelPollStats) {
	if stats == nil {
		return
	}

	switch evType {
	case protoEventMessagePosted, protoEventTopicCreated:
		stats.msg++
	case protoEventMessageUpdated:
		stats.edit++
	case protoEventMessageDeleted:
		stats.del++
	case protoEventMessageReacted, protoEventBatchReactions:
		stats.react++
	case protoEventTypingChanged:
		stats.typing++
	case protoEventGroupUpdated:
		stats.group++
	case protoEventMembershipChanged:
		stats.member++
	case protoEventUserStatusUpdated:
		stats.status++
	case protoEventGroupViewed, protoEventTopicViewed, protoEventGroupReadState, protoEventReadReceiptChanged, protoEventMarkAsUnread:
		stats.read++
	case protoEventGroupNoOp:
		stats.noop++
	default:
		stats.other++
	}
}

func (c *Client) traceProtoEventBody(ev *protoEvent, body *protoEventBody, evType int32) {
	if c.traceProtoMessageEvent(ev, body, evType) {
		return
	}

	switch evType {
	case protoEventTypingChanged:
		c.apiLogger.Tracef("webchannel recv: typing group=%s user=%s state=%s",
			firstNonEmpty(body.TypingGroup.ID, ev.Group.ID),
			firstNonEmpty(body.TypingUserID, ev.UserID),
			typingStateLabel(body.TypingState),
		)
	case protoEventGroupUpdated:
		groupID := ev.Group.ID
		if body.GroupUpdated != nil && body.GroupUpdated.Group.ID != "" {
			groupID = body.GroupUpdated.Group.ID
		}

		c.apiLogger.Tracef("webchannel recv: group_updated group=%s op=%d", groupID, body.GroupUpdateOp)
	case protoEventMembershipChanged:
		if body.Membership != nil {
			c.apiLogger.Tracef("webchannel recv: membership_changed group=%s user=%s state=%d",
				firstNonEmpty(body.Membership.Group.ID, ev.Group.ID), body.Membership.UserID, body.Membership.State)
		}
	case protoEventUserStatusUpdated:
		if body.UserStatus != nil {
			c.apiLogger.Tracef("webchannel recv: user_status user=%s dnd=%v presence_shared=%v custom=%v",
				body.UserStatus.UserID, body.UserStatus.DND, body.UserStatus.PresenceShared,
				body.UserStatus.CustomText != "" || body.UserStatus.CustomEmoji != "")
		}
	case protoEventGroupViewed, protoEventGroupReadState:
		groupID := firstNonEmpty(body.GroupViewed.ID, firstNonEmpty(body.ReadStateGroup.ID, ev.Group.ID))
		c.apiLogger.Tracef("webchannel recv: read_state group=%s type=%d", groupID, evType)
	default:
		if label := auxiliaryEventLabel(evType); label != "" {
			c.apiLogger.Tracef("webchannel recv: %s group=%s", label, ev.Group.ID)
		} else {
			c.apiLogger.Tracef("webchannel recv: unknown event_type=%d group=%s", evType, ev.Group.ID)
		}
	}
}

var auxiliaryEventLabels = map[int32]string{
	protoEventTopicViewed:               "topic_viewed",
	protoEventTopicMuteChanged:          "topic_mute_changed",
	protoEventUserSettingsChanged:       "user_settings_changed",
	protoEventGroupStarred:              "group_starred",
	protoEventGroupUnreadTopicCount:     "unread_topic_count",
	protoEventGroupHideChanged:          "group_hide_changed",
	protoEventGroupNotificationSettings: "group_notification_settings",
	protoEventRetentionSettingsUpdated:  "retention_settings_updated",
	protoEventMessageSmartReplies:       "smart_replies",
	protoEventGroupDeleted:              "group_deleted",
	protoEventSessionReady:              "session_ready",
	protoEventGroupSortTimestampChanged: "group_sort_timestamp",
	protoEventReadReceiptChanged:        "read_receipt_changed",
	protoEventMarkAsUnread:              "mark_as_unread",
	protoEventGroupNoOp:                 "group_noop",
}

func auxiliaryEventLabel(evType int32) string {
	return auxiliaryEventLabels[evType]
}

func (c *Client) traceProtoMessageEvent(ev *protoEvent, body *protoEventBody, evType int32) bool {
	switch evType {
	case protoEventMessagePosted, protoEventMessageUpdated:
		kind := "message_posted"
		if evType == protoEventMessageUpdated {
			kind = "message_updated"
		}

		if body.MessagePosted != nil {
			creatorID := ""
			if body.MessagePosted.Creator != nil {
				creatorID = body.MessagePosted.Creator.ID
			}

			c.apiLogger.Tracef("webchannel recv: %s group=%s msg=%s user=%s system=%v edited=%v",
				kind, firstNonEmpty(body.MessagePosted.Group.ID, ev.Group.ID),
				body.MessagePosted.MessageID, creatorID,
				body.MessagePosted.MessageType == protoMsgTypeSystem,
				body.MessagePosted.EditTimeUsec > 0)
		}
	case protoEventTopicCreated:
		if body.TopicCreated != nil {
			c.apiLogger.Tracef("webchannel recv: topic_created group=%s topic=%s replies=%d system=%v",
				firstNonEmpty(body.TopicCreated.Group.ID, ev.Group.ID),
				body.TopicCreated.TopicID, len(body.TopicCreated.Replies), body.TopicCreated.IsSystem)
		}
	case protoEventMessageDeleted:
		c.apiLogger.Tracef("webchannel recv: message_deleted group=%s msg=%s",
			firstNonEmpty(body.DeletedGroup.ID, ev.Group.ID), body.DeletedMsgID)
	case protoEventMessageReacted:
		c.apiLogger.Tracef("webchannel recv: reaction group=%s msg=%s user=%s opt=%d",
			firstNonEmpty(body.ReactGroup.ID, ev.Group.ID), body.ReactMsgID,
			firstNonEmpty(body.ReactUserID, ev.UserID), body.ReactOption)
	case protoEventBatchReactions:
		c.apiLogger.Tracef("webchannel recv: batch_reactions group=%s msg=%s summaries=%d",
			firstNonEmpty(body.BatchGroup.ID, ev.Group.ID), body.BatchMsgID, len(body.BatchSummaries))
	default:
		return false
	}

	return true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

func typingStateLabel(state int32) string {
	switch state {
	case protoTypingStarted:
		return "started"
	case protoTypingStopped:
		return "stopped"
	default:
		return strconv.Itoa(int(state))
	}
}

func (c *Client) handleMessageEventBody(ev *protoEvent, body *protoEventBody, isEdit bool) {
	var (
		prevTopic    string
		hasPrevTopic bool
	)

	if body.MessageGroup != nil {
		if prev := c.GetChannel(body.MessageGroup.Group.ID); prev != nil {
			prevTopic = prev.Topic
			hasPrevTopic = true
		}

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

	if isEdit && c.isNonEditMessageUpdate(msg) {
		c.cachePost(msg)

		return
	}

	c.emitIncomingChatMessage(msg, prevTopic, hasPrevTopic, isEdit)
}

// isNonEditMessageUpdate reports whether a MESSAGE_UPDATED event is a
// server-side metadata/link-preview unfurl rather than a user edit.
func (c *Client) isNonEditMessageUpdate(msg *ChatMessage) bool {
	prev := c.GetCachedPost(msg.ID)
	sameContent := prev == nil || (prev.ContentText == msg.ContentText && len(prev.Attachments) == len(msg.Attachments))

	if msg.EditAt == 0 {
		return sameContent
	}

	return prev != nil && prev.EditAt == msg.EditAt && sameContent
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

		if body.TopicCreated.IsSystem {
			msg.IsSystem = true
		}

		c.emitIncomingChatMessage(msg, "", false, false)
	}
}

func (c *Client) emitIncomingChatMessage(msg *ChatMessage, prevTopic string, hasPrevTopic, isEdit bool) {
	if msg.TopicChanged {
		newTopic := strings.TrimSpace(msg.NewTopic)
		changed := c.setConversationTopic(msg.ConversationID, newTopic)

		if hasPrevTopic {
			changed = prevTopic != newTopic
		}

		if changed {
			c.emitMessage(&Message{
				Type:      EventChannelTopic,
				Channel:   msg.ConversationID,
				UserID:    msg.FromID,
				Topic:     newTopic,
				Timestamp: timeFromMillis(msg.CreateAt),
			})
		}

		if msg.ContentText == "" || msg.ContentText == "Space Updated" {
			return
		}

		msg.TopicChanged = false
	}

	if !msg.IsSystem && msg.FromID != "" {
		c.recordUserActivity(msg.FromID)
	}

	c.cachePost(msg)
	c.touchConversation(msg.ConversationID, msg.CreateAt)

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

	c.recordUserActivity(userID)

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
	if body.TypingState != protoTypingStarted && body.TypingState != protoTypingStopped {
		return
	}

	c.rememberGroupRef(body.TypingGroup)

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

	if body.TypingState == protoTypingStarted {
		c.recordUserActivity(userID)
	}

	evType := EventTyping
	if body.TypingState == protoTypingStopped {
		evType = EventClearTyping
	}

	c.emitMessage(&Message{
		Type:      evType,
		Channel:   convID,
		Topic:     body.TypingTopicID,
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
	c.markStreamConnected(ctx, nil, false)

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
