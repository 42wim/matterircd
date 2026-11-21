package rocketchatclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	ddpHandshakeTimeout = 15 * time.Second
	ddpMethodTimeout    = 15 * time.Second
	ddpPingInterval     = 30 * time.Second
	ddpReadTimeout      = 75 * time.Second
	ddpWriteTimeout     = 10 * time.Second
	maxReconnectBackoff = time.Minute
	eventQueueSize      = 2048

	ddpFieldMsg        = "msg"
	streamRoomMessages = "stream-room-messages"
	streamNotifyUser   = "stream-notify-user"
	streamNotifyLogged = "stream-notify-logged"
	actionRemoved      = "removed"
)

type ddpEnvelope struct {
	Msg        string          `json:"msg"`
	ID         string          `json:"id,omitempty"`
	Session    string          `json:"session,omitempty"`
	Collection string          `json:"collection,omitempty"`
	Subs       []string        `json:"subs,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *ddpError       `json:"error,omitempty"`
	Fields     *ddpFields      `json:"fields,omitempty"`
	Reason     string          `json:"reason,omitempty"`
}

type ddpFields struct {
	EventName string            `json:"eventName"`
	Args      []json.RawMessage `json:"args"`
}

type ddpError struct {
	Error   any    `json:"error"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (e *ddpError) String() string {
	if e == nil {
		return ""
	}

	if e.Reason != "" {
		return e.Reason
	}

	if e.Message != "" {
		return e.Message
	}

	return fmt.Sprintf("%v", e.Error)
}

type ddpMethodResult struct {
	result json.RawMessage
	err    error
}

type ddpEventKind int

const (
	ddpEventReady ddpEventKind = iota
	ddpEventReconnect
	ddpEventChanged
)

type ddpEvent struct {
	kind       ddpEventKind
	downtime   time.Duration
	collection string
	fields     *ddpFields
}

type ddpConn struct {
	ws        *websocket.Conn
	writeCh   chan []byte
	pongCh    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	waitersMu     sync.Mutex
	connectedCh   chan struct{}
	connectErr    error
	methodWaiters map[string]chan ddpMethodResult
	subWaiters    map[string]chan error
	nextID        atomic.Uint64
}

func newDDPConn(ws *websocket.Conn) *ddpConn {
	return &ddpConn{
		ws:            ws,
		writeCh:       make(chan []byte, 64),
		pongCh:        make(chan []byte, 16),
		closed:        make(chan struct{}),
		connectedCh:   make(chan struct{}),
		methodWaiters: make(map[string]chan ddpMethodResult),
		subWaiters:    make(map[string]chan error),
	}
}

func (d *ddpConn) close() {
	d.closeOnce.Do(func() {
		close(d.closed)
		_ = d.ws.Close()

		d.waitersMu.Lock()

		for id, ch := range d.methodWaiters {
			delete(d.methodWaiters, id)

			ch <- ddpMethodResult{err: errors.New("rocketchatclient: websocket closed")}
		}

		for id, ch := range d.subWaiters {
			delete(d.subWaiters, id)

			ch <- errors.New("rocketchatclient: websocket closed")
		}

		d.waitersMu.Unlock()
	})
}

func (d *ddpConn) sendRaw(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	select {
	case d.writeCh <- data:
		return nil
	case <-d.closed:
		return errors.New("rocketchatclient: websocket closed")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *ddpConn) sendPong(id string) {
	msg := map[string]string{ddpFieldMsg: "pong"}
	if id != "" {
		msg["id"] = id
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return
	}

	select {
	case d.pongCh <- data:
	case <-d.closed:
	default:
	}
}

func (d *ddpConn) callMethod(ctx context.Context, method string, params ...any) (json.RawMessage, error) {
	id := strconv.FormatUint(d.nextID.Add(1), 10)
	ch := make(chan ddpMethodResult, 1)

	d.waitersMu.Lock()
	d.methodWaiters[id] = ch
	d.waitersMu.Unlock()

	cleanup := func() {
		d.waitersMu.Lock()
		delete(d.methodWaiters, id)
		d.waitersMu.Unlock()
	}

	req := map[string]any{
		ddpFieldMsg: "method",
		"method":    method,
		"id":        id,
		"params":    params,
	}

	err := d.sendRaw(ctx, req)
	if err != nil {
		cleanup()

		return nil, err
	}

	timer := time.NewTimer(ddpMethodTimeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		return res.result, res.err
	case <-d.closed:
		cleanup()

		return nil, errors.New("rocketchatclient: websocket closed during method call")
	case <-timer.C:
		cleanup()

		return nil, fmt.Errorf("rocketchatclient: DDP method %q timed out", method)
	case <-ctx.Done():
		cleanup()

		return nil, ctx.Err()
	}
}

func (d *ddpConn) subscribe(ctx context.Context, name string, params ...any) (string, error) {
	id := "sub-" + strconv.FormatUint(d.nextID.Add(1), 10)
	ch := make(chan error, 1)

	d.waitersMu.Lock()
	d.subWaiters[id] = ch
	d.waitersMu.Unlock()

	cleanup := func() {
		d.waitersMu.Lock()
		delete(d.subWaiters, id)
		d.waitersMu.Unlock()
	}

	req := map[string]any{
		ddpFieldMsg: "sub",
		"id":        id,
		"name":      name,
		"params":    params,
	}

	err := d.sendRaw(ctx, req)
	if err != nil {
		cleanup()

		return "", err
	}

	timer := time.NewTimer(ddpMethodTimeout)
	defer timer.Stop()

	select {
	case subErr := <-ch:
		if subErr != nil {
			return "", subErr
		}

		return id, nil
	case <-d.closed:
		cleanup()

		return "", errors.New("rocketchatclient: websocket closed during subscribe")
	case <-timer.C:
		cleanup()

		return "", fmt.Errorf("rocketchatclient: DDP subscribe %q timed out", name)
	case <-ctx.Done():
		cleanup()

		return "", ctx.Err()
	}
}

func (c *Client) writeLoop(ctx context.Context, conn *ddpConn) {
	defer conn.wg.Done()
	defer conn.close()

	pingTicker := time.NewTicker(ddpPingInterval)
	defer pingTicker.Stop()

	pingPayload := []byte(`{"msg":"ping"}`)

	writeFrame := func(data []byte) error {
		err := conn.ws.SetWriteDeadline(time.Now().Add(ddpWriteTimeout))
		if err != nil {
			return err
		}

		return conn.ws.WriteMessage(websocket.TextMessage, data)
	}

	for {
		// Prioritize pong responses so heartbeats cannot be delayed behind outbound frames.
		select {
		case data := <-conn.pongCh:
			err := writeFrame(data)
			if err != nil {
				c.logger.Warnf("ddp write pong: %s", err)

				return
			}

			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case <-conn.closed:
			return
		case data := <-conn.pongCh:
			err := writeFrame(data)
			if err != nil {
				c.logger.Warnf("ddp write pong: %s", err)

				return
			}
		case data := <-conn.writeCh:
			err := writeFrame(data)
			if err != nil {
				c.logger.Warnf("ddp write: %s", err)

				return
			}
		case <-pingTicker.C:
			err := writeFrame(pingPayload)
			if err != nil {
				c.logger.Warnf("ddp write ping: %s", err)

				return
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context, conn *ddpConn) {
	defer conn.wg.Done()
	defer conn.close()

	for {
		err := conn.ws.SetReadDeadline(time.Now().Add(ddpReadTimeout))
		if err != nil {
			return
		}

		_, data, err := conn.ws.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				c.logger.Warnf("ddp read: %s", err)
			}

			return
		}

		var env ddpEnvelope

		err = json.Unmarshal(data, &env)
		if err != nil {
			c.logger.Debugf("ddp unmarshal: %s", err)

			continue
		}

		c.handleDDPFrame(ctx, conn, &env)
	}
}

func (c *Client) handleDDPFrame(ctx context.Context, conn *ddpConn, env *ddpEnvelope) {
	switch env.Msg {
	case "ping":
		conn.sendPong(env.ID)
	case "pong":
	case "connected":
		conn.signalConnected(nil)
	case "failed":
		conn.signalConnected(fmt.Errorf("rocketchatclient: DDP protocol negotiation failed: %s", env.Reason))
	case "result":
		conn.resolveMethod(env)
	case "ready":
		conn.resolveReady(env.Subs)
	case "nosub":
		conn.resolveNosub(env)
	case "changed":
		c.enqueueChanged(ctx, conn, env)
	}
}

func (d *ddpConn) signalConnected(err error) {
	d.waitersMu.Lock()

	if err != nil {
		d.connectErr = err
	}

	select {
	case <-d.connectedCh:
	default:
		close(d.connectedCh)
	}

	d.waitersMu.Unlock()
}

func (d *ddpConn) resolveMethod(env *ddpEnvelope) {
	d.waitersMu.Lock()
	ch, ok := d.methodWaiters[env.ID]
	delete(d.methodWaiters, env.ID)
	d.waitersMu.Unlock()

	if !ok {
		return
	}

	var res ddpMethodResult
	if env.Error != nil {
		res.err = fmt.Errorf("rocketchatclient: DDP error: %s", env.Error.String())
	} else {
		res.result = env.Result
	}

	ch <- res
}

func (d *ddpConn) resolveReady(subs []string) {
	d.waitersMu.Lock()

	for _, subID := range subs {
		if ch, ok := d.subWaiters[subID]; ok {
			delete(d.subWaiters, subID)

			ch <- nil
		}
	}

	d.waitersMu.Unlock()
}

func (d *ddpConn) resolveNosub(env *ddpEnvelope) {
	d.waitersMu.Lock()
	ch, ok := d.subWaiters[env.ID]
	delete(d.subWaiters, env.ID)
	d.waitersMu.Unlock()

	if !ok {
		return
	}

	msg := "subscription rejected"
	if env.Error != nil {
		msg = env.Error.String()
	}

	ch <- fmt.Errorf("rocketchatclient: DDP nosub (%s): %s", env.ID, msg)
}

func (c *Client) enqueueChanged(ctx context.Context, conn *ddpConn, env *ddpEnvelope) {
	if env.Fields == nil {
		return
	}

	ev := ddpEvent{
		kind:       ddpEventChanged,
		collection: env.Collection,
		fields:     env.Fields,
	}

	select {
	case c.eventCh <- ev:
	case <-conn.closed:
	case <-ctx.Done():
	}
}

func (c *Client) runLoop() {
	defer c.wg.Done()

	backoff := time.Second

	for c.ctx.Err() == nil {
		wasConnected, err := c.connectOnce(c.ctx)
		if c.ctx.Err() != nil {
			return
		}

		if wasConnected {
			backoff = time.Second
		}

		if err != nil {
			c.logger.Warnf("ddp session ended: %s; reconnecting in %s", err, backoff.Round(time.Millisecond))
		}

		if c.shouldReauth(err) {
			authErr := c.authenticate(c.ctx)
			if authErr != nil {
				c.logger.Warnf("re-authentication failed: %s", authErr)
			}
		}

		wait := addJitter(backoff)

		errSleep := sleepContext(c.ctx, wait)
		if errSleep != nil {
			return
		}

		backoff = min(backoff*2, maxReconnectBackoff)
	}
}

func (c *Client) shouldReauth(err error) bool {
	if err == nil {
		return false
	}

	c.authMu.RLock()
	tokenLogin := c.tokenLogin
	c.authMu.RUnlock()

	if tokenLogin {
		return false
	}

	msg := strings.ToLower(err.Error())

	return strings.Contains(msg, "expired") || strings.Contains(msg, "invalid token") || strings.Contains(msg, "not-authorized")
}

func addJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}

	n, err := rand.Int(rand.Reader, big.NewInt(int64(base/2)+1))
	if err != nil {
		return base
	}

	return base + time.Duration(n.Int64())
}

func (c *Client) dialDDP(ctx context.Context) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: c.opts.timeout,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: c.cred.SkipTLSVerify || c.opts.skipTLSVerify, //nolint:gosec
		},
	}

	var headers http.Header
	if c.opts.userAgent != "" {
		headers = http.Header{}
		headers.Set("User-Agent", c.opts.userAgent)
	}

	ws, resp, err := dialer.DialContext(ctx, c.wsURL, headers)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	return ws, err
}

func (c *Client) connectOnce(parentCtx context.Context) (bool, error) {
	ws, err := c.dialDDP(parentCtx)
	if err != nil {
		return false, err
	}

	connCtx, connCancel := context.WithCancel(parentCtx)
	defer connCancel()

	conn := newDDPConn(ws)

	conn.wg.Add(2)

	go c.writeLoop(connCtx, conn)
	go c.readLoop(connCtx, conn)

	defer func() {
		conn.close()
		connCancel()
		conn.wg.Wait()
	}()

	err = c.handshakeAndLogin(connCtx, conn)
	if err != nil {
		return false, err
	}

	wasReady := c.ready.Load()
	if !wasReady {
		err = c.hydrateState(connCtx)
		if err != nil {
			return false, err
		}
	}

	err = c.subscribeCoreStreams(connCtx, conn)
	if err != nil {
		return false, err
	}

	c.connected.Store(true)

	prevDisconnect := c.lastDisconnect.Swap(0)
	c.lastSync.Store(time.Now().UnixNano())

	c.emitReadyOrReconnect(connCtx, conn, wasReady, prevDisconnect)

	select {
	case <-conn.closed:
		c.connected.Store(false)
		c.lastDisconnect.Store(time.Now().UnixNano())

		return true, errors.New("connection closed")
	case <-connCtx.Done():
		c.connected.Store(false)

		return true, connCtx.Err()
	}
}

func (c *Client) emitReadyOrReconnect(ctx context.Context, conn *ddpConn, wasReady bool, prevDisconnect int64) {
	if c.ready.CompareAndSwap(false, true) {
		select {
		case c.eventCh <- ddpEvent{kind: ddpEventReady}:
		case <-conn.closed:
		case <-ctx.Done():
		}

		return
	}

	if !wasReady {
		return
	}

	var downtime time.Duration
	if prevDisconnect > 0 {
		downtime = time.Since(time.Unix(0, prevDisconnect))
	}

	select {
	case c.eventCh <- ddpEvent{kind: ddpEventReconnect, downtime: downtime}:
	case <-conn.closed:
	case <-ctx.Done():
	}
}

func (c *Client) handshakeAndLogin(ctx context.Context, conn *ddpConn) error {
	connectFrame := map[string]any{
		ddpFieldMsg: "connect",
		"version":   "1",
		"support":   []string{"1"},
	}

	err := conn.sendRaw(ctx, connectFrame)
	if err != nil {
		return err
	}

	timer := time.NewTimer(ddpHandshakeTimeout)
	defer timer.Stop()

	select {
	case <-conn.connectedCh:
		conn.waitersMu.Lock()
		connectErr := conn.connectErr
		conn.waitersMu.Unlock()

		if connectErr != nil {
			return connectErr
		}
	case <-conn.closed:
		return errors.New("rocketchatclient: websocket closed before DDP connected")
	case <-timer.C:
		return errors.New("rocketchatclient: DDP handshake timed out")
	case <-ctx.Done():
		return ctx.Err()
	}

	_, authToken := c.authCredentials()

	_, err = conn.callMethod(ctx, "login", map[string]string{"resume": authToken})
	if err != nil {
		return fmt.Errorf("rocketchatclient: DDP login failed: %w", err)
	}

	return nil
}

func (c *Client) subscribeCoreStreams(ctx context.Context, conn *ddpConn) error {
	userID, _ := c.authCredentials()

	subs := []struct {
		name   string
		params []any
	}{
		{name: streamRoomMessages, params: []any{"__my_messages__", false}},
		{name: streamNotifyUser, params: []any{userID + "/rooms-changed", false}},
		{name: streamNotifyUser, params: []any{userID + "/subscriptions-changed", false}},
		{name: streamNotifyLogged, params: []any{"user-status", false}},
		{name: streamNotifyLogged, params: []any{"Users:NameChanged", false}},
	}

	for _, s := range subs {
		_, err := conn.subscribe(ctx, s.name, s.params...)
		if err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) hydrateState(ctx context.Context) error {
	subsResp, err := c.GetSubscriptions(ctx, nil)
	if err != nil {
		return fmt.Errorf("rocketchatclient: subscriptions.get: %w", err)
	}

	roomsResp, err := c.GetRooms(ctx, nil)
	if err != nil {
		return fmt.Errorf("rocketchatclient: rooms.get: %w", err)
	}

	c.cache.hydrateRoomsAndSubs(roomsResp.Update, subsResp.Update)

	for offset := 0; ; {
		usersResp, err := c.ListUsers(ctx, offset, userListPageLimit)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusUnauthorized) {
				c.logger.Warnf("users.list restricted (%s); continuing with lazy user lookups", apiErr)

				break
			}

			return fmt.Errorf("rocketchatclient: users.list: %w", err)
		}

		c.cache.upsertUsers(usersResp.Users)

		if len(usersResp.Users) == 0 || offset+len(usersResp.Users) >= usersResp.Total {
			break
		}

		offset += len(usersResp.Users)
	}

	return nil
}

func (c *Client) eventLoop() {
	defer c.wg.Done()

	consumerReady := false

	for {
		select {
		case <-c.ctx.Done():
			return
		case ev := <-c.eventCh:
			switch ev.kind {
			case ddpEventReady:
				consumerReady = true

				if c.handler != nil {
					c.handler.Ready()
				}
			case ddpEventReconnect:
				if consumerReady && c.handler != nil {
					if rh, ok := c.handler.(ReconnectHandler); ok {
						rh.OnReconnect(ev.downtime)
					}
				}
			case ddpEventChanged:
				c.processDDPChanged(ev.collection, ev.fields, consumerReady)
			}
		}
	}
}

func (c *Client) processDDPChanged(collection string, fields *ddpFields, emit bool) {
	if fields == nil || len(fields.Args) == 0 {
		return
	}

	switch collection {
	case streamNotifyUser:
		c.handleNotifyUser(fields, emit)
	case streamNotifyLogged:
		c.handleNotifyLogged(fields, emit)
	case streamRoomMessages:
		c.handleStreamRoomMessage(fields, emit)
	}
}

func (c *Client) handleNotifyUser(fields *ddpFields, emit bool) {
	if len(fields.Args) < 2 {
		return
	}

	var action string

	err := json.Unmarshal(fields.Args[0], &action)
	if err != nil {
		return
	}

	switch {
	case strings.HasSuffix(fields.EventName, "/rooms-changed"):
		var room Room

		err = json.Unmarshal(fields.Args[1], &room)
		if err != nil || room.ID == "" {
			return
		}

		if action == actionRemoved {
			c.cache.removeRoom(room.ID)
		} else {
			c.cache.upsertRoom(&room)
		}

		if emit && c.handler != nil && action != actionRemoved {
			c.handler.OnRoomChange(&room)
		}
	case strings.HasSuffix(fields.EventName, "/subscriptions-changed"):
		var sub Subscription

		err = json.Unmarshal(fields.Args[1], &sub)
		if err != nil || sub.RoomID == "" {
			return
		}

		if action == actionRemoved {
			c.cache.removeSubscription(sub.RoomID)
		} else {
			c.cache.upsertSubscription(&sub)
		}
	}
}

func (c *Client) handleNotifyLogged(fields *ddpFields, emit bool) {
	switch fields.EventName {
	case "user-status":
		var tuple []json.RawMessage

		err := json.Unmarshal(fields.Args[0], &tuple)
		if err != nil || len(tuple) < 3 {
			return
		}

		var (
			userID     string
			username   string
			statusCode int
			statusText string
		)

		err = json.Unmarshal(tuple[0], &userID)
		if err != nil || userID == "" {
			return
		}

		_ = json.Unmarshal(tuple[1], &username)

		err = json.Unmarshal(tuple[2], &statusCode)
		if err != nil {
			return
		}

		if len(tuple) >= 4 {
			_ = json.Unmarshal(tuple[3], &statusText)
		}

		status := mapStatusCode(statusCode)
		_, changed := c.cache.updateUserPresence(userID, username, status, statusText)

		if emit && changed && c.handler != nil {
			c.handler.OnPresence(userID, status)
		}
	case "Users:NameChanged":
		var u struct {
			ID       string `json:"_id"`
			Name     string `json:"name"`
			Username string `json:"username"`
		}

		err := json.Unmarshal(fields.Args[0], &u)
		if err != nil || u.ID == "" {
			return
		}

		c.cache.updateUserName(u.ID, u.Name, u.Username)
	}
}

func (c *Client) handleStreamRoomMessage(fields *ddpFields, emit bool) {
	if !emit || c.handler == nil {
		return
	}

	var msg Message

	err := json.Unmarshal(fields.Args[0], &msg)
	if err != nil || msg.ID == "" {
		return
	}

	c.cache.msgCache.Add(msg.ID, &msg)

	if msg.EditedAt != nil && !msg.EditedAt.Time.IsZero() {
		c.handler.OnEdit(&msg)

		return
	}

	c.handler.OnMessage(&msg)
}

func mapStatusCode(code int) string {
	switch code {
	case 1:
		return StatusOnline
	case 2:
		return StatusAway
	case 3:
		return StatusBusy
	default:
		return StatusOffline
	}
}
