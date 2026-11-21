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
	ddpHandshakeTimeout  = 15 * time.Second
	ddpMethodTimeout     = 15 * time.Second
	ddpPingInterval      = 30 * time.Second
	ddpReadTimeout       = 75 * time.Second
	ddpWriteTimeout      = 10 * time.Second
	maxReconnectBackoff  = time.Minute
	eventQueueSize       = 2048
	maxInitialRoomSubs   = 100
	maxInitialUserPages  = 5
	userHydrateTimeout   = 10 * time.Second
	parentFetchTimeout   = 3 * time.Second
	reconnectSyncOverlap = 5 * time.Second
	reconnectSyncTimeout = 30 * time.Second

	ddpFieldMsg        = "msg"
	streamRoomMessages = "stream-room-messages"
	streamNotifyUser   = "stream-notify-user"
	streamNotifyLogged = "stream-notify-logged"
	streamNotifyRoom   = "stream-notify-room"
	actionRemoved      = "removed"
	memberActionJoin   = "join"
	memberActionLeave  = "leave"
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
	ddpEventSyncStart
	ddpEventSyncEnd
	ddpEventChanged
	ddpEventSyncedMsg
	ddpEventSyncedDelete
	ddpEventRoomChange
	ddpEventRoomRemove
	ddpEventMember
)

type ddpEvent struct {
	kind       ddpEventKind
	downtime   time.Duration
	collection string
	fields     *ddpFields
	msg        *Message
	roomID     string
	msgID      string
	room       *Room
	member     *MemberEvent
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

func (d *ddpConn) callMethod(ctx context.Context, method string, params ...any) error {
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

		return err
	}

	timer := time.NewTimer(ddpMethodTimeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		return res.err
	case <-d.closed:
		cleanup()

		return errors.New("rocketchatclient: websocket closed during method call")
	case <-timer.C:
		cleanup()

		return fmt.Errorf("rocketchatclient: DDP method %q timed out", method)
	case <-ctx.Done():
		cleanup()

		return ctx.Err()
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

func (d *ddpConn) unsubscribe(ctx context.Context, subID string) error {
	if subID == "" {
		return nil
	}

	req := map[string]any{
		ddpFieldMsg: "unsub",
		"id":        subID,
	}

	return d.sendRaw(ctx, req)
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
		c.clearActiveConn(conn)
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

	endReconnectSync := c.prepareReconnectSync(connCtx, wasReady)
	defer endReconnectSync()

	err = c.subscribeCoreStreams(connCtx, conn)
	if err != nil {
		return false, err
	}

	c.setActiveConn(conn)
	c.subscribeInitialRooms(connCtx)

	c.connected.Store(true)
	c.finishSessionSync(connCtx, conn, wasReady, endReconnectSync)

	return c.waitConnectionClose(connCtx, conn)
}

func (c *Client) finishSessionSync(connCtx context.Context, conn *ddpConn, wasReady bool, endReconnectSync func()) {
	prevSyncNS := c.lastSync.Load()
	prevDisconnect := c.lastDisconnect.Swap(0)

	if wasReady && prevSyncNS > 0 {
		c.syncAfterReconnect(connCtx, conn, time.Unix(0, prevSyncNS))
	}

	c.lastSync.Store(time.Now().UnixNano())
	endReconnectSync()

	c.emitReadyOrReconnect(connCtx, conn, wasReady, prevDisconnect)
}

func (c *Client) prepareReconnectSync(connCtx context.Context, wasReady bool) func() {
	if !wasReady {
		return func() {}
	}

	c.enqueueSyncMarker(connCtx, ddpEventSyncStart)

	var done atomic.Bool

	return func() {
		if done.CompareAndSwap(false, true) {
			c.enqueueSyncMarker(c.ctx, ddpEventSyncEnd)
		}
	}
}

func (c *Client) enqueueSyncMarker(ctx context.Context, kind ddpEventKind) {
	select {
	case c.eventCh <- ddpEvent{kind: kind}:
	case <-ctx.Done():
	}
}

func (c *Client) syncAfterReconnect(ctx context.Context, conn *ddpConn, prevSync time.Time) {
	c.cache.invalidateRoomMembers()

	since := prevSync.Add(-reconnectSyncOverlap)

	syncCtx, cancel := context.WithTimeout(ctx, reconnectSyncTimeout)
	defer cancel()

	subsResp, err := c.GetSubscriptions(syncCtx, &since)
	if err == nil {
		c.applyReconnectSubscriptions(syncCtx, conn, subsResp)
	} else {
		c.logger.Warnf("reconnect subscriptions.get: %s", err)
	}

	roomsResp, err := c.GetRooms(syncCtx, &since)
	if err == nil {
		c.applyReconnectRooms(syncCtx, conn, roomsResp)
	} else {
		c.logger.Warnf("reconnect rooms.get: %s", err)
	}

	c.syncOpenRoomsMessages(syncCtx, conn, since)
}

func (c *Client) applyReconnectSubscriptions(ctx context.Context, conn *ddpConn, resp *SubscriptionsResponse) {
	meID := c.UserID()

	for i := range resp.Remove {
		sub := &resp.Remove[i]
		if sub.RoomID == "" {
			continue
		}

		c.cache.removeSubscription(sub.RoomID)
		_ = c.UnsubscribeRoom(ctx, sub.RoomID)

		c.enqueueEvent(ctx, conn, ddpEvent{
			kind: ddpEventMember,
			member: &MemberEvent{
				RoomID: sub.RoomID,
				UserID: meID,
				Action: memberActionLeave,
			},
		})
	}

	for i := range resp.Update {
		sub := &resp.Update[i]
		if sub.RoomID == "" {
			continue
		}

		existed := c.cache.upsertSubscription(sub)
		if sub.Open {
			c.ensureRoomSubscribed(sub.RoomID)
		}

		if !existed {
			c.enqueueEvent(ctx, conn, ddpEvent{
				kind: ddpEventMember,
				member: &MemberEvent{
					RoomID: sub.RoomID,
					UserID: meID,
					Action: memberActionJoin,
				},
			})
		}
	}
}

func (c *Client) applyReconnectRooms(ctx context.Context, conn *ddpConn, resp *RoomsResponse) {
	for i := range resp.Remove {
		if resp.Remove[i].ID != "" {
			c.cache.removeRoom(resp.Remove[i].ID)
			c.enqueueEvent(ctx, conn, ddpEvent{
				kind:   ddpEventRoomRemove,
				roomID: resp.Remove[i].ID,
			})
		}
	}

	for i := range resp.Update {
		room := &resp.Update[i]
		if room.ID == "" {
			continue
		}

		c.cache.upsertRoom(room)
		c.enqueueEvent(ctx, conn, ddpEvent{
			kind: ddpEventRoomChange,
			room: cloneRoom(room),
		})
	}
}

func (c *Client) syncOpenRoomsMessages(ctx context.Context, conn *ddpConn, since time.Time) {
	subs := c.Subscriptions()
	count := 0

	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}

		if !sub.Open || sub.RoomID == "" {
			continue
		}

		syncResp, err := c.SyncMessages(ctx, sub.RoomID, since)
		if err != nil {
			c.logger.Debugf("reconnect chat.syncMessages %s: %s", sub.RoomID, err)

			continue
		}

		sortMessagesChronological(syncResp.Result.Updated)

		for i := range syncResp.Result.Updated {
			m := syncResp.Result.Updated[i]
			c.enqueueEvent(ctx, conn, ddpEvent{
				kind: ddpEventSyncedMsg,
				msg:  &m,
			})
		}

		for i := range syncResp.Result.Deleted {
			del := &syncResp.Result.Deleted[i]
			if del.ID == "" {
				continue
			}

			c.enqueueEvent(ctx, conn, ddpEvent{
				kind:   ddpEventSyncedDelete,
				roomID: sub.RoomID,
				msgID:  del.ID,
			})
		}

		count++
		if count >= maxInitialRoomSubs {
			break
		}
	}
}

func (c *Client) enqueueEvent(ctx context.Context, conn *ddpConn, ev ddpEvent) {
	select {
	case c.eventCh <- ev:
	case <-conn.closed:
	case <-ctx.Done():
	}
}

func (c *Client) waitConnectionClose(ctx context.Context, conn *ddpConn) (bool, error) {
	select {
	case <-conn.closed:
		c.connected.Store(false)
		c.lastDisconnect.Store(time.Now().UnixNano())

		return true, errors.New("connection closed")
	case <-ctx.Done():
		c.connected.Store(false)

		return true, ctx.Err()
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

	err = conn.callMethod(ctx, "login", map[string]string{"resume": authToken})
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
	c.seedUsersFromRooms(roomsResp.Update)

	userCtx, cancel := context.WithTimeout(ctx, userHydrateTimeout)
	defer cancel()

	loaded := 0

	for page := range maxInitialUserPages {
		usersResp, err := c.ListUsers(userCtx, loaded, userListPageLimit)
		if err != nil {
			c.logger.Warnf("users.list stopped after %d user(s) (%s); continuing with lazy user lookups", loaded, err)

			break
		}

		c.cache.upsertUsers(usersResp.Users)
		loaded += len(usersResp.Users)

		if len(usersResp.Users) == 0 || loaded >= usersResp.Total {
			break
		}

		if page+1 == maxInitialUserPages && loaded < usersResp.Total {
			c.logger.Infof("users.list capped at %d/%d users during startup; remaining users will resolve lazily", loaded, usersResp.Total)
		}
	}

	return nil
}

func (c *Client) seedUsersFromRooms(rooms []Room) {
	for i := range rooms {
		r := &rooms[i]
		if len(r.UIDs) == 0 || len(r.UIDs) != len(r.Usernames) {
			continue
		}

		for idx, uid := range r.UIDs {
			uname := r.Usernames[idx]
			if uid == "" || uname == "" {
				continue
			}

			if _, ok := c.cache.user(uid); !ok {
				c.cache.upsertUser(&User{
					ID:       uid,
					Username: uname,
					Active:   true,
				})
			}
		}
	}
}

func (c *Client) eventLoop(ctx context.Context) {
	defer c.wg.Done()

	consumerReady := false
	syncBuffering := false

	var deferred []ddpEvent

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-c.eventCh:
			c.handleLoopEvent(ctx, &ev, &consumerReady, &syncBuffering, &deferred)
		}
	}
}

func (c *Client) handleLoopEvent(ctx context.Context, ev *ddpEvent, consumerReady, syncBuffering *bool, deferred *[]ddpEvent) {
	switch ev.kind {
	case ddpEventSyncStart:
		*syncBuffering = true

		return
	case ddpEventSyncEnd:
		*syncBuffering = false

		c.flushDeferredDDP(ctx, *consumerReady, deferred)

		return
	case ddpEventChanged:
		if *syncBuffering {
			if len(*deferred) >= eventQueueSize {
				c.flushDeferredDDP(ctx, *consumerReady, deferred)
			}

			*deferred = append(*deferred, *ev)

			return
		}
	}

	c.dispatchEvent(ctx, ev, consumerReady)
}

func (c *Client) flushDeferredDDP(ctx context.Context, emit bool, deferred *[]ddpEvent) {
	for i := range *deferred {
		d := &(*deferred)[i]
		c.processDDPChanged(ctx, d.collection, d.fields, emit)
	}

	clear(*deferred)
	*deferred = nil
}

func (c *Client) dispatchEvent(ctx context.Context, ev *ddpEvent, consumerReady *bool) {
	switch ev.kind {
	case ddpEventReady:
		*consumerReady = true

		if c.handler != nil {
			c.handler.Ready()
		}

		return
	case ddpEventChanged:
		c.processDDPChanged(ctx, ev.collection, ev.fields, *consumerReady)

		return
	}

	if !*consumerReady {
		return
	}

	c.dispatchLiveEvent(ctx, ev)
}

func (c *Client) dispatchLiveEvent(ctx context.Context, ev *ddpEvent) {
	switch ev.kind {
	case ddpEventReconnect:
		if rh, ok := c.handler.(ReconnectHandler); ok && c.handler != nil {
			rh.OnReconnect(ev.downtime)
		}
	case ddpEventSyncedMsg:
		if ev.msg != nil {
			c.processIncomingMessage(ctx, ev.msg)
		}
	case ddpEventSyncedDelete:
		c.processDeletedMessage(ev.roomID, ev.msgID)
	case ddpEventRoomChange:
		if c.handler != nil && ev.room != nil {
			c.handler.OnRoomChange(ev.room)
		}
	case ddpEventRoomRemove:
		c.emitRoomRemove(ev.roomID)
	case ddpEventMember:
		if ev.member != nil {
			c.emitMember(ev.member)
		}
	}
}

func (c *Client) emitRoomRemove(roomID string) {
	if c.handler == nil || roomID == "" {
		return
	}

	if rrh, ok := c.handler.(RoomRemoveHandler); ok {
		rrh.OnRoomRemove(roomID)
	}
}

func (c *Client) emitMember(ev *MemberEvent) {
	if ev == nil {
		return
	}

	switch ev.Action {
	case memberActionJoin:
		if ev.UserID != "" {
			c.cache.seedUserRef(UserRef{ID: ev.UserID, Username: ev.Username})
			c.cache.addRoomMember(ev.RoomID, ev.UserID)
		}
	case memberActionLeave:
		c.cache.removeRoomMember(ev.RoomID, ev.UserID)
	}

	if c.handler == nil {
		return
	}

	if mh, ok := c.handler.(MemberHandler); ok {
		mh.OnMember(ev)
	}
}

func (c *Client) processDDPChanged(ctx context.Context, collection string, fields *ddpFields, emit bool) {
	if fields == nil || len(fields.Args) == 0 {
		return
	}

	switch collection {
	case streamNotifyUser:
		c.handleNotifyUser(fields, emit)
	case streamNotifyLogged:
		c.handleNotifyLogged(fields, emit)
	case streamNotifyRoom:
		c.handleNotifyRoom(fields, emit)
	case streamRoomMessages:
		c.handleStreamRoomMessage(ctx, fields, emit)
	}
}

func (c *Client) setActiveConn(conn *ddpConn) {
	c.connMu.Lock()
	c.activeConn = conn
	clear(c.roomDeleteSubs)
	clear(c.roomTypingSubs)
	c.connMu.Unlock()
}

func (c *Client) clearActiveConn(conn *ddpConn) {
	c.connMu.Lock()
	if c.activeConn == conn {
		c.activeConn = nil
		clear(c.roomDeleteSubs)
		clear(c.roomTypingSubs)
	}

	c.connMu.Unlock()
}

func (c *Client) subscribeInitialRooms(ctx context.Context) {
	subs := c.Subscriptions()
	count := 0

	for _, sub := range subs {
		if !sub.Open || sub.RoomID == "" {
			continue
		}

		err := c.SubscribeRoom(ctx, sub.RoomID)
		if err != nil {
			c.logger.Debugf("subscribe deleteMessage for room %s: %s", sub.RoomID, err)

			continue
		}

		count++
		if count >= maxInitialRoomSubs {
			break
		}
	}
}

// SubscribeRoom subscribes to per-room notifications (such as deleteMessage) for roomID.
func (c *Client) SubscribeRoom(ctx context.Context, roomID string) error {
	if roomID == "" {
		return nil
	}

	c.connMu.Lock()
	conn := c.activeConn
	_, exists := c.roomDeleteSubs[roomID]
	c.connMu.Unlock()

	if conn == nil || exists {
		return nil
	}

	subID, err := conn.subscribe(ctx, streamNotifyRoom, roomID+"/deleteMessage", false)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	if c.activeConn != conn {
		c.connMu.Unlock()

		return nil
	}

	if _, dup := c.roomDeleteSubs[roomID]; dup {
		c.connMu.Unlock()

		_ = conn.unsubscribe(ctx, subID)

		return nil
	}

	c.roomDeleteSubs[roomID] = subID
	c.connMu.Unlock()

	typingSubID, typingErr := conn.subscribe(ctx, streamNotifyRoom, roomID+"/typing", false)
	if typingErr == nil && typingSubID != "" {
		c.connMu.Lock()
		if c.activeConn == conn {
			if _, dup := c.roomTypingSubs[roomID]; !dup {
				c.roomTypingSubs[roomID] = typingSubID
				typingSubID = ""
			}
		}
		c.connMu.Unlock()

		if typingSubID != "" {
			_ = conn.unsubscribe(ctx, typingSubID)
		}
	}

	return nil
}

// UnsubscribeRoom removes the per-room deleteMessage and typing subscriptions for roomID.
func (c *Client) UnsubscribeRoom(ctx context.Context, roomID string) error {
	if roomID == "" {
		return nil
	}

	c.connMu.Lock()
	conn := c.activeConn
	subID, exists := c.roomDeleteSubs[roomID]
	delete(c.roomDeleteSubs, roomID)
	typingSubID, typingExists := c.roomTypingSubs[roomID]
	delete(c.roomTypingSubs, roomID)
	c.connMu.Unlock()

	if conn == nil {
		return nil
	}

	var firstErr error

	if exists && subID != "" {
		firstErr = conn.unsubscribe(ctx, subID)
	}

	if typingExists && typingSubID != "" {
		err := conn.unsubscribe(ctx, typingSubID)
		if firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

// SendTyping publishes a transient typing notification for the logged-in user in roomID.
func (c *Client) SendTyping(ctx context.Context, roomID string) error {
	if roomID == "" {
		return nil
	}

	me := c.Me()
	if me == nil || me.Username == "" {
		return nil
	}

	c.connMu.Lock()
	conn := c.activeConn
	c.connMu.Unlock()

	if conn == nil {
		return nil
	}

	return conn.callMethod(ctx, streamNotifyRoom, roomID+"/typing", me.Username, true)
}

func (c *Client) ensureRoomSubscribed(roomID string) {
	if roomID == "" {
		return
	}

	c.connMu.Lock()
	conn := c.activeConn
	_, exists := c.roomDeleteSubs[roomID]
	c.connMu.Unlock()

	if conn == nil || exists {
		return
	}

	// Subscribe asynchronously so eventLoop never blocks waiting on readLoop for "ready".
	go func() {
		_ = c.SubscribeRoom(c.ctx, roomID)
	}()
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
		c.handleRoomsChanged(action, fields.Args[1], emit)
	case strings.HasSuffix(fields.EventName, "/subscriptions-changed"):
		c.handleSubscriptionsChanged(action, fields.Args[1], emit)
	}
}

func (c *Client) handleRoomsChanged(action string, raw json.RawMessage, emit bool) {
	var room Room

	err := json.Unmarshal(raw, &room)
	if err != nil || room.ID == "" {
		return
	}

	if action == actionRemoved {
		c.cache.removeRoom(room.ID)

		if emit {
			c.emitRoomRemove(room.ID)
		}

		return
	}

	c.cache.upsertRoom(&room)

	if emit && c.handler != nil {
		c.handler.OnRoomChange(&room)
	}
}

func (c *Client) handleSubscriptionsChanged(action string, raw json.RawMessage, emit bool) {
	var sub Subscription

	err := json.Unmarshal(raw, &sub)
	if err != nil || sub.RoomID == "" {
		return
	}

	if action == actionRemoved {
		c.cache.removeSubscription(sub.RoomID)
		_ = c.UnsubscribeRoom(c.ctx, sub.RoomID)

		if emit {
			c.emitMember(&MemberEvent{
				RoomID: sub.RoomID,
				UserID: c.UserID(),
				Action: memberActionLeave,
			})
		}

		return
	}

	existed := c.cache.upsertSubscription(&sub)
	if sub.Open {
		c.ensureRoomSubscribed(sub.RoomID)
	}

	if emit && !existed {
		c.emitMember(&MemberEvent{
			RoomID: sub.RoomID,
			UserID: c.UserID(),
			Action: memberActionJoin,
		})
	}
}

func (c *Client) handleNotifyRoom(fields *ddpFields, emit bool) {
	if roomID, ok := strings.CutSuffix(fields.EventName, "/typing"); ok {
		c.handleRoomTyping(roomID, fields.Args, emit)

		return
	}

	if !strings.HasSuffix(fields.EventName, "/deleteMessage") {
		return
	}

	roomID := strings.TrimSuffix(fields.EventName, "/deleteMessage")
	if roomID == "" {
		return
	}

	var deleted struct {
		ID string `json:"_id"`
	}

	err := json.Unmarshal(fields.Args[0], &deleted)
	if err != nil || deleted.ID == "" {
		return
	}

	if !emit {
		c.cache.forgetMessage(deleted.ID)

		return
	}

	c.processDeletedMessage(roomID, deleted.ID)
}

func (c *Client) handleRoomTyping(roomID string, args []json.RawMessage, emit bool) {
	if !emit || roomID == "" || len(args) < 2 || c.handler == nil {
		return
	}

	th, ok := c.handler.(TypingHandler)
	if !ok {
		return
	}

	var (
		username string
		typing   bool
	)

	err := json.Unmarshal(args[0], &username)
	if err != nil || username == "" {
		return
	}

	err = json.Unmarshal(args[1], &typing)
	if err != nil || !typing {
		return
	}

	th.OnTyping(roomID, username)
}

func (c *Client) processDeletedMessage(roomID, msgID string) {
	if roomID == "" || msgID == "" {
		return
	}

	c.cache.forgetMessage(msgID)

	if c.cache.consumeSent("del:" + msgID) {
		return
	}

	if c.handler != nil {
		c.handler.OnDelete(roomID, msgID)
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

func (c *Client) handleStreamRoomMessage(ctx context.Context, fields *ddpFields, emit bool) {
	if !emit || c.handler == nil {
		return
	}

	var msg Message

	err := json.Unmarshal(fields.Args[0], &msg)
	if err != nil || msg.ID == "" {
		return
	}

	c.ensureRoomSubscribed(msg.RoomID)
	c.lastSync.Store(time.Now().UnixNano())
	c.processIncomingMessage(ctx, &msg)
}

func (c *Client) processIncomingMessage(ctx context.Context, msg *Message) {
	if c.handler == nil || msg == nil || msg.ID == "" {
		return
	}

	if msg.Type != "" && msg.Type != "me" {
		c.handleSystemMessage(ctx, msg)

		return
	}

	c.seedMessageUsers(msg)

	prevMeta, seen := c.cache.recordSeenMessage(msg)
	c.cache.cacheMessage(msg)

	isEdit := msg.EditedAt != nil && !msg.EditedAt.Time.IsZero()

	var editTS int64
	if isEdit {
		editTS = msg.EditedAt.Time.UnixNano()
	}

	if !seen {
		c.handleUnseenStreamMessage(ctx, msg, isEdit)

		return
	}

	reactionEvents := c.cache.diffReactions(msg.RoomID, msg.ID, msg.Reactions)
	for i := range reactionEvents {
		c.handler.OnReaction(&reactionEvents[i])
	}

	if !isEdit || (editTS == prevMeta.editedAt && msg.Text == prevMeta.text) {
		return
	}

	if c.cache.consumeSent("edit:" + msg.ID + "\x00" + msg.Text) {
		return
	}

	c.prefetchThreadParent(ctx, msg.ThreadID)
	c.handler.OnEdit(msg)
}

func (c *Client) handleSystemMessage(ctx context.Context, msg *Message) {
	switch msg.Type {
	case "uj":
		if msg.User.ID == c.UserID() {
			c.cache.seedUserRef(msg.User)
			c.cache.addRoomMember(msg.RoomID, msg.User.ID)
			c.ensureRoomSubscribed(msg.RoomID)

			return
		}

		c.emitMember(&MemberEvent{
			RoomID:   msg.RoomID,
			UserID:   msg.User.ID,
			Username: msg.User.Username,
			Action:   memberActionJoin,
		})
	case "ul":
		if msg.User.ID == c.UserID() {
			c.cache.removeRoomMember(msg.RoomID, msg.User.ID)

			return
		}

		c.emitMember(&MemberEvent{
			RoomID:   msg.RoomID,
			UserID:   msg.User.ID,
			Username: msg.User.Username,
			Action:   memberActionLeave,
		})
	case "au":
		c.emitTargetedMember(ctx, msg, memberActionJoin)
	case "ru":
		c.emitTargetedMember(ctx, msg, memberActionLeave)
	case "room_changed_topic":
		if room, ok := c.cache.updateRoomTopic(msg.RoomID, msg.Text); ok && c.handler != nil {
			c.handler.OnRoomChange(room)
		}
	case "r":
		if room, ok := c.cache.updateRoomName(msg.RoomID, msg.Text); ok && c.handler != nil {
			c.handler.OnRoomChange(room)
		}
	}
}

func (c *Client) emitTargetedMember(ctx context.Context, msg *Message, action string) {
	username := strings.TrimSpace(msg.Text)
	if username == "" {
		return
	}

	targetID := ""

	if u, ok := c.cache.userByUsername(username); ok {
		targetID = u.ID
	} else {
		lookupCtx, cancel := context.WithTimeout(ctx, parentFetchTimeout)
		u, err := c.GetUserInfo(lookupCtx, "", username)

		cancel()

		if err == nil && u != nil {
			targetID = u.ID
		}
	}

	c.emitMember(&MemberEvent{
		RoomID:   msg.RoomID,
		UserID:   targetID,
		Username: username,
		Sender:   msg.User.ID,
		Action:   action,
	})
}

func (c *Client) handleUnseenStreamMessage(ctx context.Context, msg *Message, isEdit bool) {
	c.cache.seedReactions(msg.ID, msg.Reactions)

	if !isEdit {
		if c.cache.consumeSent("send:" + msg.ID) {
			return
		}

		c.prefetchThreadParent(ctx, msg.ThreadID)
		c.handler.OnMessage(msg)

		return
	}

	if c.cache.consumeSent("edit:" + msg.ID + "\x00" + msg.Text) {
		return
	}

	c.prefetchThreadParent(ctx, msg.ThreadID)
	c.handler.OnEdit(msg)
}

func (c *Client) prefetchThreadParent(ctx context.Context, threadID string) {
	if threadID == "" {
		return
	}

	if _, ok := c.cache.cachedMessage(threadID); ok {
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, parentFetchTimeout)
	defer cancel()

	_, err := c.GetMessage(fetchCtx, threadID)
	if err != nil {
		c.logger.Debugf("prefetch thread parent %s: %s", threadID, err)
	}
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
