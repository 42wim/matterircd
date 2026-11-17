package rocketchatclient

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
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
)

const (
	maxAPIRetries       = 5
	defaultRateWait     = 3 * time.Second
	maxRateLimitWait    = 60 * time.Second
	userListPageLimit   = 100
	maxRoomMemberPages  = 5
	roomMemberTimeout   = 15 * time.Second
	defaultHistoryCount = 20
	historyPageLimit    = 100
	maxHistoryPages     = 5
	defaultSearchCount  = 20
	meteorIDLen         = 17
	base62Alphabet      = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	roomActionLeave     = "leave"
)

// APIError represents an error payload returned by the Rocket.Chat REST API.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("rocketchat REST %d (%s): %s", e.StatusCode, e.Code, e.Message)
	case e.Message != "":
		return fmt.Sprintf("rocketchat REST %d: %s", e.StatusCode, e.Message)
	case e.Code != "":
		return fmt.Sprintf("rocketchat REST %d: %s", e.StatusCode, e.Code)
	default:
		return fmt.Sprintf("rocketchat REST %d", e.StatusCode)
	}
}

func normalizeServerURL(server string, noTLS bool) (string, string, error) {
	raw := strings.TrimSpace(server)
	if raw == "" {
		return "", "", errors.New("rocketchatclient: empty server URL")
	}

	if !strings.Contains(raw, "://") {
		if noTLS {
			raw = "http://" + raw
		} else {
			raw = "https://" + raw
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("rocketchatclient: invalid server URL %q: %w", server, err)
	}

	if parsed.Host == "" {
		return "", "", fmt.Errorf("rocketchatclient: missing host in server URL %q", server)
	}

	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawQuery = ""
	parsed.Fragment = ""

	httpURL := parsed.String()

	wsParsed := *parsed
	if wsParsed.Scheme == "http" {
		wsParsed.Scheme = "ws"
	} else {
		wsParsed.Scheme = "wss"
	}

	wsParsed.Path += "/websocket"

	return httpURL, wsParsed.String(), nil
}

func parseTokenCredential(raw string) (string, string, bool) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "token=")

	userID, authToken, ok := strings.Cut(trimmed, ":")
	if !ok || userID == "" || authToken == "" {
		return "", "", false
	}

	return userID, authToken, true
}

func (c *Client) authenticate(ctx context.Context) error {
	if c.cred.Token != "" {
		userID, authToken, ok := parseTokenCredential(c.cred.Token)
		if !ok {
			return errors.New("rocketchatclient: token must be formatted as <userId>:<authToken>")
		}

		c.authMu.Lock()
		c.userID = userID
		c.authToken = authToken
		c.tokenLogin = true
		c.authMu.Unlock()

		me, err := c.fetchMe(ctx)
		if err != nil {
			return fmt.Errorf("rocketchatclient: token verification failed: %w", err)
		}

		c.authMu.Lock()
		c.me = me
		c.authMu.Unlock()
		c.cache.upsertUser(me)

		return nil
	}

	if c.cred.Login == "" || c.cred.Pass == "" {
		return errors.New("rocketchatclient: login and password or token are required")
	}

	data, err := c.loginPassword(ctx, c.cred.Login, c.cred.Pass, c.cred.TwoFactorCode)
	if err != nil {
		return err
	}

	c.authMu.Lock()
	c.userID = data.UserID
	c.authToken = data.AuthToken
	c.tokenLogin = false

	if data.Me != nil {
		c.me = data.Me
	}

	c.authMu.Unlock()

	if data.Me != nil {
		c.cache.upsertUser(data.Me)
	} else {
		me, err := c.fetchMe(ctx)
		if err == nil {
			c.authMu.Lock()
			c.me = me
			c.authMu.Unlock()
			c.cache.upsertUser(me)
		}
	}

	return nil
}

type loginRequest struct {
	User     string `json:"user"`
	Password string `json:"password"`
	Code     string `json:"code,omitempty"`
}

type loginData struct {
	UserID    string `json:"userId"`
	AuthToken string `json:"authToken"`
	Me        *User  `json:"me"`
}

type loginResponse struct {
	Status string     `json:"status"`
	Data   *loginData `json:"data"`
}

func (c *Client) loginPassword(ctx context.Context, user, pass, code string) (*loginData, error) {
	req := loginRequest{
		User:     user,
		Password: pass,
		Code:     code,
	}

	var resp loginResponse

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/login", nil, req, &resp, false)
	if err != nil {
		return nil, err
	}

	if resp.Data == nil || resp.Data.UserID == "" || resp.Data.AuthToken == "" {
		return nil, errors.New("rocketchatclient: login response missing userId or authToken")
	}

	return resp.Data, nil
}

func (c *Client) fetchMe(ctx context.Context) (*User, error) {
	var user User

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/me", nil, nil, &user, true)
	if err != nil {
		return nil, err
	}

	if user.ID == "" {
		return nil, errors.New("rocketchatclient: /api/v1/me response missing _id")
	}

	return &user, nil
}

func (c *Client) logoutREST(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/logout", nil, nil, nil, true)
}

// GetSubscriptions fetches the user's subscriptions, optionally filtered by updatedSince.
func (c *Client) GetSubscriptions(ctx context.Context, updatedSince *time.Time) (*SubscriptionsResponse, error) {
	var query url.Values
	if updatedSince != nil && !updatedSince.IsZero() {
		query = url.Values{}
		query.Set("updatedSince", updatedSince.UTC().Format(time.RFC3339Nano))
	}

	var resp SubscriptionsResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/subscriptions.get", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetRooms fetches rooms accessible to the user, optionally filtered by updatedSince.
func (c *Client) GetRooms(ctx context.Context, updatedSince *time.Time) (*RoomsResponse, error) {
	var query url.Values
	if updatedSince != nil && !updatedSince.IsZero() {
		query = url.Values{}
		query.Set("updatedSince", updatedSince.UTC().Format(time.RFC3339Nano))
	}

	var resp RoomsResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/rooms.get", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// ListUsers fetches a page of workspace users using the server's default projection.
func (c *Client) ListUsers(ctx context.Context, offset, count int) (*UsersListResponse, error) {
	query := url.Values{}
	query.Set("offset", strconv.Itoa(offset))
	query.Set("count", strconv.Itoa(count))

	var resp UsersListResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/users.list", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// CreateDM creates or opens a direct message room with username and updates the room cache.
func (c *Client) CreateDM(ctx context.Context, username string) (*Room, error) {
	if username == "" {
		return nil, errors.New("rocketchatclient: username is required")
	}

	req := struct {
		Username string `json:"username"`
	}{
		Username: username,
	}

	var resp struct {
		Room    *Room `json:"room"`
		Success bool  `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/im.create", nil, req, &resp, true)
	if err != nil {
		return nil, err
	}

	if resp.Room == nil || resp.Room.ID == "" {
		return nil, errors.New("rocketchatclient: im.create response missing room")
	}

	c.cache.upsertRoom(resp.Room)

	return cloneRoom(resp.Room), nil
}

// GetUserInfo fetches a single user by userID or username and updates the cache.
func (c *Client) GetUserInfo(ctx context.Context, userID, username string) (*User, error) {
	query := url.Values{}
	if userID != "" {
		query.Set("userId", userID)
	} else if username != "" {
		query.Set("username", username)
	}

	var resp struct {
		User    *User `json:"user"`
		Success bool  `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/users.info", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	if resp.User == nil {
		return nil, errors.New("rocketchatclient: users.info response missing user")
	}

	c.cache.upsertUser(resp.User)

	return resp.User, nil
}

// DownloadURL returns the authenticated download URL for an uploaded Rocket.Chat file.
func (c *Client) DownloadURL(fileID, fileName string) string {
	if fileID == "" {
		return ""
	}

	if fileName == "" {
		return c.httpURL + "/file-upload/" + url.PathEscape(fileID)
	}

	return c.httpURL + "/file-upload/" + url.PathEscape(fileID) + "/" + url.PathEscape(fileName)
}

// ResolveAttachmentURL normalizes a relative Rocket.Chat attachment path against the server base URL.
func (c *Client) ResolveAttachmentURL(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		return trimmed
	}

	if strings.HasPrefix(trimmed, "/") {
		return c.httpURL + trimmed
	}

	return c.httpURL + "/" + trimmed
}

func generateMeteorID() string {
	const maxValidByte = 248 // 62 * 4, avoids modulo bias

	var (
		out [meteorIDLen]byte
		buf [32]byte
	)

	idx := 0

	for idx < meteorIDLen {
		_, err := rand.Read(buf[:])
		if err != nil {
			// Fallback to timestamp-derived entropy only if crypto/rand fails.
			now := uint64(time.Now().UnixNano())
			for idx < meteorIDLen {
				out[idx] = base62Alphabet[now%62]
				now /= 62
				idx++
			}

			break
		}

		for _, b := range buf {
			if b >= maxValidByte {
				continue
			}

			out[idx] = base62Alphabet[b%62]
			idx++

			if idx == meteorIDLen {
				break
			}
		}
	}

	return string(out[:])
}

// SendMessage sends a message (or thread reply when threadID is non-empty) to roomID.
func (c *Client) SendMessage(ctx context.Context, roomID, text, threadID string) (string, error) {
	if roomID == "" {
		return "", errors.New("rocketchatclient: roomID is required")
	}

	msgID := generateMeteorID()
	sentKey := "send:" + msgID

	c.cache.markSent(sentKey)

	req := struct {
		Message struct {
			ID         string `json:"_id"`
			RoomID     string `json:"rid"`
			Text       string `json:"msg"`
			ThreadID   string `json:"tmid,omitempty"`
			ThreadShow bool   `json:"tshow,omitempty"`
		} `json:"message"`
	}{}
	req.Message.ID = msgID
	req.Message.RoomID = roomID
	req.Message.Text = text
	req.Message.ThreadID = threadID
	req.Message.ThreadShow = threadID != ""

	var resp struct {
		Message *Message `json:"message"`
		Success *bool    `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/chat.sendMessage", nil, req, &resp, true)
	if err != nil {
		c.cache.unmarkSent(sentKey)

		return "", err
	}

	if (resp.Success != nil && !*resp.Success) || resp.Message == nil {
		c.cache.unmarkSent(sentKey)

		return "", errors.New("rocketchatclient: chat.sendMessage did not return a created message")
	}

	c.logger.Debugf("chat.sendMessage: rid=%s threadID=%s reqID=%s respID=%s", roomID, threadID, msgID, resp.Message.ID)

	c.cache.cacheMessage(resp.Message)

	if resp.Message.ID != "" {
		return resp.Message.ID, nil
	}

	return msgID, nil
}

// UpdateMessage edits an existing message in roomID.
func (c *Client) UpdateMessage(ctx context.Context, roomID, msgID, text string) error {
	if roomID == "" || msgID == "" {
		return errors.New("rocketchatclient: roomID and msgID are required")
	}

	editKey := "edit:" + msgID + "\x00" + text
	c.cache.markSent(editKey)

	req := struct {
		RoomID string `json:"roomId"`
		MsgID  string `json:"msgId"`
		Text   string `json:"text"`
	}{
		RoomID: roomID,
		MsgID:  msgID,
		Text:   text,
	}

	var resp struct {
		Message *Message `json:"message"`
		Success bool     `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/chat.update", nil, req, &resp, true)
	if err != nil {
		c.cache.unmarkSent(editKey)

		return err
	}

	if resp.Message != nil {
		c.cache.cacheMessage(resp.Message)
	}

	return nil
}

// DeleteMessage deletes a message in roomID.
func (c *Client) DeleteMessage(ctx context.Context, roomID, msgID string) error {
	if roomID == "" || msgID == "" {
		return errors.New("rocketchatclient: roomID and msgID are required")
	}

	delKey := "del:" + msgID
	c.cache.markSent(delKey)

	req := struct {
		RoomID string `json:"roomId"`
		MsgID  string `json:"msgId"`
	}{
		RoomID: roomID,
		MsgID:  msgID,
	}

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/chat.delete", nil, req, nil, true)
	if err != nil {
		c.cache.unmarkSent(delKey)

		return err
	}

	c.cache.forgetMessage(msgID)

	return nil
}

// React adds or removes an emoji reaction on msgID.
func (c *Client) React(ctx context.Context, msgID, emoji string, shouldReact bool) error {
	if msgID == "" || emoji == "" {
		return errors.New("rocketchatclient: msgID and emoji are required")
	}

	normalized := strings.Trim(emoji, ":")
	if normalized == "" {
		return errors.New("rocketchatclient: emoji is required")
	}

	req := struct {
		MessageID   string `json:"messageId"`
		Emoji       string `json:"emoji"`
		ShouldReact bool   `json:"shouldReact"`
	}{
		MessageID:   msgID,
		Emoji:       ":" + normalized + ":",
		ShouldReact: shouldReact,
	}

	return c.doJSON(ctx, http.MethodPost, "/api/v1/chat.react", nil, req, nil, true)
}

// GetMessage fetches a single message by ID and updates the message cache.
func (c *Client) GetMessage(ctx context.Context, msgID string) (*Message, error) {
	if msgID == "" {
		return nil, errors.New("rocketchatclient: msgID is required")
	}

	query := url.Values{}
	query.Set("msgId", msgID)

	var resp struct {
		Message *Message `json:"message"`
		Success bool     `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/chat.getMessage", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	if resp.Message == nil {
		return nil, errors.New("rocketchatclient: chat.getMessage response missing message")
	}

	c.cache.cacheMessage(resp.Message)

	return resp.Message, nil
}

func (c *Client) roomTypeFor(roomID string) string {
	if room, ok := c.cache.room(roomID); ok && room.Type != "" {
		return room.Type
	}

	if sub, ok := c.cache.subscription(roomID); ok && sub.Type != "" {
		return sub.Type
	}

	return RoomTypeChannel
}

func roomEndpoint(roomType, suffix string) (string, error) {
	switch roomType {
	case RoomTypeChannel:
		return "/api/v1/channels." + suffix, nil
	case RoomTypePrivate:
		return "/api/v1/groups." + suffix, nil
	case RoomTypeDirect:
		return "/api/v1/im." + suffix, nil
	default:
		return "", fmt.Errorf("rocketchatclient: unsupported room type %q", roomType)
	}
}

// GetChannelInfo fetches public channel metadata by roomID or roomName and updates the cache.
func (c *Client) GetChannelInfo(ctx context.Context, roomID, roomName string) (*Room, error) {
	query := url.Values{}

	switch {
	case roomID != "":
		query.Set("roomId", roomID)
	case roomName != "":
		query.Set("roomName", strings.TrimPrefix(roomName, "#"))
	default:
		return nil, errors.New("rocketchatclient: roomID or roomName is required")
	}

	var resp struct {
		Channel *Room `json:"channel"`
		Success bool  `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/channels.info", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	if resp.Channel == nil || resp.Channel.ID == "" {
		return nil, errors.New("rocketchatclient: channels.info response missing channel")
	}

	c.cache.upsertRoom(resp.Channel)

	return cloneRoom(resp.Channel), nil
}

func (c *Client) resolveJoinTarget(ctx context.Context, roomID, cleanName string) (string, *Room, error) {
	if roomID != "" {
		if room, ok := c.cache.room(roomID); ok && room.Type != RoomTypeChannel {
			if _, subbed := c.cache.subscription(roomID); subbed {
				_ = c.SubscribeRoom(ctx, roomID)

				return roomID, room, nil
			}

			return "", nil, errors.New("rocketchatclient: cannot self-join private or direct room without invite")
		}

		return roomID, nil, nil
	}

	if cleanName == "" {
		return "", nil, errors.New("rocketchatclient: roomID or roomName is required")
	}

	info, err := c.GetChannelInfo(ctx, "", cleanName)
	if err != nil {
		return "", nil, err
	}

	return info.ID, nil, nil
}

// JoinChannel joins a public channel by roomID or roomName (or returns an already-joined private room).
func (c *Client) JoinChannel(ctx context.Context, roomID, roomName string) (*Room, error) {
	cleanName := strings.TrimPrefix(roomName, "#")

	targetID, alreadyJoined, err := c.resolveJoinTarget(ctx, roomID, cleanName)
	if err != nil {
		return nil, err
	}

	if alreadyJoined != nil {
		return alreadyJoined, nil
	}

	req := struct {
		RoomID string `json:"roomId"`
	}{
		RoomID: targetID,
	}

	var resp struct {
		Channel *Room `json:"channel"`
		Success bool  `json:"success"`
	}

	err = c.doJSON(ctx, http.MethodPost, "/api/v1/channels.join", nil, req, &resp, true)
	if err != nil {
		return nil, err
	}

	room := resp.Channel
	if room == nil {
		room, _ = c.cache.room(targetID)
	}

	if room == nil || room.ID == "" {
		room = &Room{ID: targetID, Type: RoomTypeChannel, Name: cleanName}
	}

	c.cache.upsertRoom(room)

	if _, subbed := c.cache.subscription(room.ID); !subbed {
		c.cache.upsertSubscription(&Subscription{
			RoomID: room.ID,
			Name:   room.Name,
			FName:  room.FName,
			Type:   room.Type,
			Open:   true,
		})
	}

	_ = c.SubscribeRoom(ctx, room.ID)

	return cloneRoom(room), nil
}

// LeaveRoom leaves a public channel, private group, or closes a direct room.
func (c *Client) LeaveRoom(ctx context.Context, roomID string) error {
	if roomID == "" {
		return errors.New("rocketchatclient: roomID is required")
	}

	roomType := c.roomTypeFor(roomID)

	suffix := roomActionLeave
	if roomType == RoomTypeDirect {
		suffix = "close"
	}

	endpoint, err := roomEndpoint(roomType, suffix)
	if err != nil {
		return err
	}

	req := struct {
		RoomID string `json:"roomId"`
	}{
		RoomID: roomID,
	}

	err = c.doJSON(ctx, http.MethodPost, endpoint, nil, req, nil, true)
	if err != nil {
		return err
	}

	_ = c.UnsubscribeRoom(ctx, roomID)
	c.cache.removeSubscription(roomID)

	return nil
}

// InviteUser invites userID to roomID.
func (c *Client) InviteUser(ctx context.Context, roomID, userID string) error {
	return c.roomUserAction(ctx, roomID, userID, "invite")
}

// KickUser removes userID from roomID.
func (c *Client) KickUser(ctx context.Context, roomID, userID string) error {
	return c.roomUserAction(ctx, roomID, userID, "kick")
}

func (c *Client) roomUserAction(ctx context.Context, roomID, userID, action string) error {
	if roomID == "" || userID == "" {
		return errors.New("rocketchatclient: roomID and userID are required")
	}

	roomType := c.roomTypeFor(roomID)
	if roomType == RoomTypeDirect {
		return fmt.Errorf("rocketchatclient: cannot %s in a direct room", action)
	}

	endpoint, err := roomEndpoint(roomType, action)
	if err != nil {
		return err
	}

	req := struct {
		RoomID string `json:"roomId"`
		UserID string `json:"userId"`
	}{
		RoomID: roomID,
		UserID: userID,
	}

	return c.doJSON(ctx, http.MethodPost, endpoint, nil, req, nil, true)
}

// SetTopic updates the topic of a public channel or private group.
func (c *Client) SetTopic(ctx context.Context, roomID, topic string) error {
	if roomID == "" {
		return errors.New("rocketchatclient: roomID is required")
	}

	roomType := c.roomTypeFor(roomID)
	if roomType == RoomTypeDirect {
		return errors.New("rocketchatclient: cannot set topic on a direct room")
	}

	endpoint, err := roomEndpoint(roomType, "setTopic")
	if err != nil {
		return err
	}

	req := struct {
		RoomID string `json:"roomId"`
		Topic  string `json:"topic"`
	}{
		RoomID: roomID,
		Topic:  topic,
	}

	err = c.doJSON(ctx, http.MethodPost, endpoint, nil, req, nil, true)
	if err != nil {
		return err
	}

	c.cache.updateRoomTopic(roomID, topic)

	return nil
}

// CreateRoom creates a public channel or private group and caches the resulting room.
func (c *Client) CreateRoom(ctx context.Context, name string, private bool) (*Room, error) {
	cleanName := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if cleanName == "" {
		return nil, errors.New("rocketchatclient: channel name is required")
	}

	endpoint := "/api/v1/channels.create"
	if private {
		endpoint = "/api/v1/groups.create"
	}

	req := struct {
		Name string `json:"name"`
	}{
		Name: cleanName,
	}

	var resp struct {
		Channel *Room `json:"channel"`
		Group   *Room `json:"group"`
		Success bool  `json:"success"`
	}

	err := c.doJSON(ctx, http.MethodPost, endpoint, nil, req, &resp, true)
	if err != nil {
		return nil, err
	}

	room := resp.Channel
	if room == nil {
		room = resp.Group
	}

	if room == nil || room.ID == "" {
		return nil, errors.New("rocketchatclient: create room response missing room")
	}

	c.cache.upsertRoom(room)
	c.cache.upsertSubscription(&Subscription{
		RoomID: room.ID,
		Name:   room.Name,
		FName:  room.FName,
		Type:   room.Type,
		Open:   true,
	})

	_ = c.SubscribeRoom(ctx, room.ID)

	return cloneRoom(room), nil
}

// RoomMembers returns cached members for roomID when known, otherwise fetching them via GetRoomMembers.
func (c *Client) RoomMembers(ctx context.Context, roomID string) ([]*User, error) {
	if roomID == "" {
		return nil, errors.New("rocketchatclient: roomID is required")
	}

	if members, ok := c.cache.roomMemberUsers(roomID); ok {
		return members, nil
	}

	return c.GetRoomMembers(ctx, roomID)
}

// GetRoomMembers fetches members (and room roles when permitted) for roomID.
func (c *Client) GetRoomMembers(ctx context.Context, roomID string) ([]*User, error) {
	if roomID == "" {
		return nil, errors.New("rocketchatclient: roomID is required")
	}

	roomType := c.roomTypeFor(roomID)

	endpoint, err := roomEndpoint(roomType, "members")
	if err != nil {
		return nil, err
	}

	memberCtx, cancel := context.WithTimeout(ctx, roomMemberTimeout)
	defer cancel()

	var members []*User

	offset := 0

	for range maxRoomMemberPages {
		query := url.Values{}
		query.Set("roomId", roomID)
		query.Set("offset", strconv.Itoa(offset))
		query.Set("count", strconv.Itoa(userListPageLimit))

		var resp MembersResponse

		err = c.doJSON(memberCtx, http.MethodGet, endpoint, query, nil, &resp, true)
		if err != nil {
			if len(members) > 0 {
				c.logger.Warnf("%s truncated at %d member(s) for %s: %s", endpoint, len(members), roomID, err)

				break
			}

			return nil, err
		}

		c.cache.upsertUsers(resp.Members)

		for i := range resp.Members {
			members = append(members, cloneUser(&resp.Members[i]))
		}

		offset += len(resp.Members)
		if len(resp.Members) == 0 || offset >= resp.Total {
			break
		}
	}

	c.cache.setRoomMembers(roomID, members)

	if roomType == RoomTypeChannel || roomType == RoomTypePrivate {
		c.fetchRoomRoles(memberCtx, roomID, roomType)
	}

	return members, nil
}

func (c *Client) fetchRoomRoles(ctx context.Context, roomID, roomType string) {
	endpoint, err := roomEndpoint(roomType, "roles")
	if err != nil {
		return
	}

	query := url.Values{}
	query.Set("roomId", roomID)

	var resp struct {
		Roles   []RoomRole `json:"roles"`
		Success bool       `json:"success"`
	}

	err = c.doJSON(ctx, http.MethodGet, endpoint, query, nil, &resp, true)
	if err != nil {
		c.logger.Debugf("%s for %s: %s", endpoint, roomID, err)

		return
	}

	c.cache.setRoomRoles(roomID, resp.Roles)
}

// SyncMessages fetches updated and deleted messages in roomID since lastUpdate.
func (c *Client) SyncMessages(ctx context.Context, roomID string, lastUpdate time.Time) (*SyncMessagesResponse, error) {
	if roomID == "" {
		return nil, errors.New("rocketchatclient: roomID is required")
	}

	query := url.Values{}
	query.Set("roomId", roomID)
	query.Set("lastUpdate", lastUpdate.UTC().Format(time.RFC3339Nano))

	var resp SyncMessagesResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/chat.syncMessages", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return &resp, nil
}

// GetHistory fetches messages for roomID in chronological (oldest-first) order.
// When oldest is non-zero, pagination walks back to oldest up to maxHistoryPages.
func (c *Client) GetHistory(ctx context.Context, roomID string, count int, oldest time.Time) ([]Message, error) {
	if roomID == "" {
		return nil, errors.New("rocketchatclient: roomID is required")
	}

	endpoint, err := roomEndpoint(c.roomTypeFor(roomID), "history")
	if err != nil {
		return nil, err
	}

	targetCount := count
	if targetCount <= 0 {
		targetCount = defaultHistoryCount
	}

	targetCount = min(targetCount, historyPageLimit*maxHistoryPages)

	pageSize := historyPageLimit
	if oldest.IsZero() && targetCount < pageSize {
		pageSize = targetCount
	}

	all, err := c.paginateHistory(ctx, endpoint, roomID, targetCount, pageSize, oldest)
	if err != nil {
		return nil, err
	}

	sortMessagesChronological(all)

	if oldest.IsZero() && len(all) > targetCount {
		all = all[len(all)-targetCount:]
	}

	return all, nil
}

func (c *Client) paginateHistory(ctx context.Context, endpoint, roomID string, targetCount, pageSize int, oldest time.Time) ([]Message, error) {
	seen := make(map[string]struct{})
	all := make([]Message, 0, min(targetCount, historyPageLimit))

	var latest time.Time

	for range maxHistoryPages {
		msgs, err := c.fetchHistoryPage(ctx, endpoint, roomID, pageSize, oldest, latest)
		if err != nil {
			return nil, err
		}

		if len(msgs) == 0 {
			break
		}

		minTS, added := c.collectUniqueMessages(&all, seen, msgs)
		if added == 0 || len(msgs) < pageSize || (oldest.IsZero() && len(all) >= targetCount) {
			break
		}

		if minTS.IsZero() || (!latest.IsZero() && !minTS.Before(latest)) {
			break
		}

		latest = minTS
	}

	return all, nil
}

func (c *Client) fetchHistoryPage(ctx context.Context, endpoint, roomID string, count int, oldest, latest time.Time) ([]Message, error) {
	query := url.Values{}
	query.Set("roomId", roomID)
	query.Set("count", strconv.Itoa(count))

	if !oldest.IsZero() {
		query.Set("oldest", oldest.UTC().Format(time.RFC3339Nano))
		query.Set("inclusive", "false")
	}

	if !latest.IsZero() {
		query.Set("latest", latest.UTC().Format(time.RFC3339Nano))
	}

	var resp MessagesResponse

	err := c.doJSON(ctx, http.MethodGet, endpoint, query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	return resp.Messages, nil
}

// GetThreadMessages returns the root message followed by its thread replies in chronological order.
func (c *Client) GetThreadMessages(ctx context.Context, msgID string) ([]Message, error) {
	if msgID == "" {
		return nil, errors.New("rocketchatclient: msgID is required")
	}

	tmid := msgID

	root, err := c.ResolveMessage(ctx, msgID)
	if err == nil && root != nil && root.ThreadID != "" {
		tmid = root.ThreadID

		parent, pErr := c.ResolveMessage(ctx, tmid)
		if pErr == nil && parent != nil {
			root = parent
		}
	}

	query := url.Values{}
	query.Set("tmid", tmid)
	query.Set("count", strconv.Itoa(historyPageLimit))

	var resp MessagesResponse

	err = c.doJSON(ctx, http.MethodGet, "/api/v1/chat.getThreadMessages", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(resp.Messages)+1)
	out := make([]Message, 0, len(resp.Messages)+1)

	if root != nil && root.ID != "" {
		c.collectUniqueMessages(&out, seen, []Message{*root})
	}

	c.collectUniqueMessages(&out, seen, resp.Messages)
	sortMessagesChronological(out)

	return out, nil
}

// SearchMessages searches messages within roomID in chronological order.
func (c *Client) SearchMessages(ctx context.Context, roomID, searchText string, count int) ([]Message, error) {
	if roomID == "" || strings.TrimSpace(searchText) == "" {
		return nil, errors.New("rocketchatclient: roomID and searchText are required")
	}

	if count <= 0 {
		count = defaultSearchCount
	}

	count = min(count, historyPageLimit)

	query := url.Values{}
	query.Set("roomId", roomID)
	query.Set("searchText", searchText)
	query.Set("count", strconv.Itoa(count))

	var resp MessagesResponse

	err := c.doJSON(ctx, http.MethodGet, "/api/v1/chat.search", query, nil, &resp, true)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]struct{}, len(resp.Messages))
	out := make([]Message, 0, len(resp.Messages))
	c.collectUniqueMessages(&out, seen, resp.Messages)
	sortMessagesChronological(out)

	return out, nil
}

// MarkRead marks roomID as read for the logged-in user and updates the cached lastSeen timestamp.
func (c *Client) MarkRead(ctx context.Context, roomID string) error {
	if roomID == "" {
		return errors.New("rocketchatclient: roomID is required")
	}

	req := struct {
		RoomID string `json:"rid"`
	}{
		RoomID: roomID,
	}

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/subscriptions.read", nil, req, nil, true)
	if err != nil {
		return err
	}

	c.cache.markRoomRead(roomID, time.Now())

	return nil
}

// SetStatus updates the authenticated user's presence status ("online", "away", "busy", "offline")
// and optional custom status message.
func (c *Client) SetStatus(ctx context.Context, status, message string) error {
	if status == "" {
		return errors.New("rocketchatclient: status is required")
	}

	req := struct {
		Status  string `json:"status"`
		Message string `json:"message,omitempty"`
	}{
		Status:  status,
		Message: message,
	}

	return c.doJSON(ctx, http.MethodPost, "/api/v1/users.setStatus", nil, req, nil, true)
}

// SetDisplayName updates the authenticated user's display name via users.updateOwnBasicInfo.
func (c *Client) SetDisplayName(ctx context.Context, name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errors.New("rocketchatclient: display name is required")
	}

	req := struct {
		Data struct {
			Name string `json:"name"`
		} `json:"data"`
	}{}
	req.Data.Name = trimmed

	err := c.doJSON(ctx, http.MethodPost, "/api/v1/users.updateOwnBasicInfo", nil, req, nil, true)
	if err != nil {
		return err
	}

	c.authMu.Lock()

	var updated *User

	if c.me != nil {
		c.me.Name = trimmed
		updated = cloneUser(c.me)
	}

	c.authMu.Unlock()

	if updated != nil {
		c.cache.upsertUser(updated)
	}

	return nil
}

// RefreshRoomsAndSubs refreshes cached rooms and subscriptions from the Rocket.Chat REST API.
func (c *Client) RefreshRoomsAndSubs(ctx context.Context) error {
	subsResp, err := c.GetSubscriptions(ctx, nil)
	if err != nil {
		return err
	}

	roomsResp, err := c.GetRooms(ctx, nil)
	if err != nil {
		return err
	}

	c.cache.hydrateRoomsAndSubs(roomsResp.Update, subsResp.Update)
	c.cache.invalidateRoomMembers()
	c.seedUsersFromRooms(roomsResp.Update)

	return nil
}

func (c *Client) seedMessageUsers(msg *Message) {
	if msg == nil {
		return
	}

	c.cache.seedUserRef(msg.User)

	for i := range msg.Mentions {
		c.cache.seedUserRef(msg.Mentions[i])
	}
}

func (c *Client) collectUniqueMessages(dst *[]Message, seen map[string]struct{}, msgs []Message) (time.Time, int) {
	var (
		minTS time.Time
		added int
	)

	for i := range msgs {
		m := &msgs[i]
		if m.ID == "" {
			continue
		}

		c.cache.cacheMessage(m)
		c.seedMessageUsers(m)

		if ts := m.Timestamp.Time; !ts.IsZero() && (minTS.IsZero() || ts.Before(minTS)) {
			minTS = ts
		}

		if _, exists := seen[m.ID]; exists {
			continue
		}

		seen[m.ID] = struct{}{}
		*dst = append(*dst, *m)
		added++
	}

	return minTS, added
}

func sortMessagesChronological(msgs []Message) {
	slices.SortStableFunc(msgs, func(a, b Message) int {
		if c := a.Timestamp.Time.Compare(b.Timestamp.Time); c != 0 {
			return c
		}

		return cmp.Compare(a.ID, b.ID)
	})
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, query url.Values, reqBody, out any, authenticated bool) error {
	var bodyBytes []byte

	if reqBody != nil {
		var err error

		bodyBytes, err = json.Marshal(reqBody)
		if err != nil {
			return err
		}
	}

	target := c.httpURL + endpoint
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	for attempt := range maxAPIRetries {
		retryWait, err := c.doJSONAttempt(ctx, method, endpoint, target, bodyBytes, out, authenticated, attempt)
		if err == nil {
			return nil
		}

		if retryWait <= 0 {
			return err
		}

		errSleep := sleepContext(ctx, retryWait)
		if errSleep != nil {
			return errSleep
		}
	}

	return fmt.Errorf("rocketchatclient: %s %s exceeded max retries", method, endpoint)
}

func (c *Client) buildJSONRequest(ctx context.Context, method, target string, bodyBytes []byte, authenticated bool) (*http.Request, error) {
	var bodyReader io.Reader
	if bodyBytes != nil {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, bodyReader)
	if err != nil {
		return nil, err
	}

	if bodyBytes != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.opts.userAgent != "" {
		req.Header.Set("User-Agent", c.opts.userAgent)
	}

	if authenticated {
		userID, authToken := c.authCredentials()
		req.Header.Set("X-User-Id", userID)
		req.Header.Set("X-Auth-Token", authToken)
	}

	return req, nil
}

func (c *Client) doJSONAttempt(ctx context.Context, method, endpoint, target string, bodyBytes []byte, out any, authenticated bool, attempt int) (time.Duration, error) {
	req, err := c.buildJSONRequest(ctx, method, target, bodyBytes, authenticated)
	if err != nil {
		return 0, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if attempt+1 < maxAPIRetries && method == http.MethodGet && ctx.Err() == nil {
			return time.Duration(attempt+1) * time.Second, err
		}

		return 0, err
	}

	respBytes, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if readErr != nil {
		return 0, readErr
	}

	return c.handleJSONResponse(method, endpoint, resp, respBytes, out, attempt)
}

func (c *Client) handleJSONResponse(method, endpoint string, resp *http.Response, respBytes []byte, out any, attempt int) (time.Duration, error) {
	if resp.StatusCode == http.StatusTooManyRequests && attempt+1 < maxAPIRetries {
		wait := rateLimitWait(resp.Header)
		c.logger.Warnf("%s %s rate-limited (429), retrying in %s", method, endpoint, wait.Round(time.Millisecond))

		return wait, fmt.Errorf("rate limited (%d)", resp.StatusCode)
	}

	if resp.StatusCode >= http.StatusInternalServerError && attempt+1 < maxAPIRetries && method == http.MethodGet {
		return time.Duration(attempt+1) * time.Second, fmt.Errorf("server error (%d)", resp.StatusCode)
	}

	if resp.StatusCode >= http.StatusBadRequest {
		return 0, parseAPIError(resp.StatusCode, respBytes)
	}

	if out == nil || hasEnvelopeErrorHint(respBytes) {
		err := checkEnvelopeError(resp.StatusCode, respBytes)
		if err != nil {
			return 0, err
		}
	}

	if out != nil && len(respBytes) > 0 {
		err := json.Unmarshal(respBytes, out)
		if err != nil {
			return 0, fmt.Errorf("rocketchatclient: decode %s %s: %w", method, endpoint, err)
		}
	}

	return 0, nil
}

var (
	jsonKeySuccess = []byte(`"success"`)
	jsonValFalse   = []byte(`false`)
	jsonKeyStatus  = []byte(`"status"`)
	jsonValError   = []byte(`"error"`)
)

func hasEnvelopeErrorHint(respBytes []byte) bool {
	return hasJSONKeyValue(respBytes, jsonKeySuccess, jsonValFalse) ||
		hasJSONKeyValue(respBytes, jsonKeyStatus, jsonValError)
}

func hasJSONKeyValue(data, key, value []byte) bool {
	search := data
	for len(search) > 0 {
		idx := bytes.Index(search, key)
		if idx < 0 {
			return false
		}

		rest := bytes.TrimLeft(search[idx+len(key):], " \t\r\n")
		if len(rest) > 0 && rest[0] == ':' {
			afterColon := bytes.TrimLeft(rest[1:], " \t\r\n")
			if bytes.HasPrefix(afterColon, value) {
				return true
			}
		}

		search = search[idx+len(key):]
	}

	return false
}

func checkEnvelopeError(statusCode int, respBytes []byte) error {
	if len(respBytes) == 0 {
		return nil
	}

	var envelope struct {
		Status    string          `json:"status"`
		Success   *bool           `json:"success"`
		Error     string          `json:"error"`
		ErrorType string          `json:"errorType"`
		Message   json.RawMessage `json:"message"`
	}

	err := json.Unmarshal(respBytes, &envelope)
	if err != nil {
		return nil //nolint:nilerr // non-JSON or alternate payloads are decoded by the caller
	}

	if envelope.Status != "error" && (envelope.Success == nil || *envelope.Success) {
		return nil
	}

	code := envelope.ErrorType
	if code == "" {
		code = envelope.Error
	}

	var msg string
	if len(envelope.Message) > 0 && envelope.Message[0] == '"' {
		_ = json.Unmarshal(envelope.Message, &msg)
	}

	if msg == "" && envelope.Error != "" && envelope.Error != code {
		msg = envelope.Error
	}

	return &APIError{
		StatusCode: statusCode,
		Code:       code,
		Message:    msg,
	}
}

func rateLimitWait(hdr http.Header) time.Duration {
	if wait, ok := parseResetHeader(hdr.Get("X-Ratelimit-Reset")); ok {
		return wait
	}

	if retryAfter := strings.TrimSpace(hdr.Get("Retry-After")); retryAfter != "" {
		secs, err := strconv.ParseInt(retryAfter, 10, 64)
		if err == nil && secs > 0 {
			return min(time.Duration(secs)*time.Second, maxRateLimitWait)
		}
	}

	return defaultRateWait
}

func parseResetHeader(raw string) (time.Duration, bool) {
	resetRaw := strings.TrimSpace(raw)
	if resetRaw == "" {
		return 0, false
	}

	resetVal, err := strconv.ParseInt(resetRaw, 10, 64)
	if err != nil || resetVal <= 0 {
		return 0, false
	}

	var resetTime time.Time
	if resetVal > 1_000_000_000_000 {
		resetTime = time.UnixMilli(resetVal)
	} else {
		resetTime = time.Unix(resetVal, 0)
	}

	wait := time.Until(resetTime)
	if wait <= 0 {
		return 100 * time.Millisecond, true
	}

	return min(wait, maxRateLimitWait), true
}

func parseAPIError(statusCode int, body []byte) error {
	if len(body) == 0 {
		return &APIError{StatusCode: statusCode}
	}

	var envelope struct {
		Error     string `json:"error"`
		ErrorType string `json:"errorType"`
		Message   string `json:"message"`
	}

	err := json.Unmarshal(body, &envelope)
	if err != nil {
		return &APIError{
			StatusCode: statusCode,
			Message:    strings.TrimSpace(string(body)),
		}
	}

	code := envelope.ErrorType
	if code == "" {
		code = envelope.Error
	}

	msg := envelope.Message
	if msg == "" && envelope.Error != "" && envelope.Error != code {
		msg = envelope.Error
	}

	return &APIError{
		StatusCode: statusCode,
		Code:       code,
		Message:    msg,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
