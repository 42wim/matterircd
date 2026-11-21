package msteamsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMessagesLimit = 50
	maxMessagesLimit     = 200
	typingDebounceWindow = 4 * time.Second

	msgFieldContent     = "content"
	msgFieldContentType = "contenttype"
	msgFieldMessageType = "messagetype"
	msgTypeRichTextHTML = "RichText/Html"
	contentTypeText     = "text"
)

type rawMessagesResponse struct {
	Messages []rawChatMessage `json:"messages"`
	Metadata struct {
		BackwardLink string `json:"backwardLink"`
	} `json:"_metadata"`
}

type rawChatMessage struct {
	ID                  string       `json:"id"`
	ClientMessageID     string       `json:"clientmessageid"`
	ConversationID      string       `json:"conversationid"`
	ConversationLink    string       `json:"conversationLink"`
	From                string       `json:"from"`
	IMDisplayName       string       `json:"imdisplayname"`
	MessageType         string       `json:"messagetype"`
	ContentType         string       `json:"contenttype"`
	Content             string       `json:"content"`
	ComposeTime         string       `json:"composetime"`
	OriginalArrivalTime string       `json:"originalarrivaltime"`
	Version             string       `json:"version"`
	Properties          rawMsgProps  `json:"properties"`
	Emotions            []rawEmotion `json:"emotions"`
}

type rawMsgProps struct {
	EditTime   string       `json:"edittime"`
	DeleteTime string       `json:"deletetime"`
	Mentions   string       `json:"mentions"`
	Files      string       `json:"files"`
	Emotions   []rawEmotion `json:"emotions"`
}

type rawEmotion struct {
	Key   string `json:"key"`
	Users []struct {
		MRI   string `json:"mri"`
		Time  int64  `json:"time"`
		Value string `json:"value"`
	} `json:"users"`
}

type rawMention struct {
	ID          int    `json:"id"`
	MentionText string `json:"mentionText"`
	Mentioned   struct {
		User struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			UserType    string `json:"userIdentityType"`
		} `json:"user"`
	} `json:"mentioned"`
}

type rawFileAttachment struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	FileName  string `json:"fileName"`
	FileType  string `json:"fileType"`
	ObjectURL string `json:"objectUrl"`
	FileInfo  struct {
		FileURL string `json:"fileUrl"`
	} `json:"fileInfo"`
}

type sendMessageResponse struct {
	OriginalArrivalTime int64 `json:"OriginalArrivalTime"`
}

type substrateSearchResponse struct {
	EntitySets []struct {
		ResultSets []struct {
			Results []struct {
				Source struct {
					ClientThreadID string `json:"ClientThreadId"`
					ThreadID       string `json:"ThreadId"`
					MessageID      string `json:"MessageId"`
					SenderName     string `json:"SenderName"`
					SenderMRI      string `json:"From"`
					Content        string `json:"Content"`
					DateTimeSent   string `json:"DateTimeSent"`
				} `json:"Source"`
			} `json:"Results"`
		} `json:"ResultSets"`
	} `json:"EntitySets"`
}

// SendMessage posts a message to conversationID and returns the created ChatMessage.
func (c *Client) SendMessage(ctx context.Context, conversationID, text string) (*ChatMessage, error) {
	return c.postMessage(ctx, conversationID, "", TextToHTML(text), text)
}

// SendMessageThread posts a reply to parentID inside conversationID.
// For Teams channels (@thread.tacv2), chatsvc uses ";messageid=<parentID>".
// For 1:1, group, and meeting chats, it embeds a Teams Reply blockquote.
func (c *Client) SendMessageThread(ctx context.Context, conversationID, parentID, text string) (*ChatMessage, error) {
	if parentID == "" {
		return c.SendMessage(ctx, conversationID, text)
	}

	bodyHTML := TextToHTML(text)

	if IsTeamsChannel(conversationID) {
		return c.postMessage(ctx, conversationID, parentID, bodyHTML, text)
	}

	parentMRI := ""
	parentAuthor := ""
	parentSnippet := ""

	if cached, ok := c.postCache.Get(parentID); ok && cached != nil {
		parentMRI = cached.FromMRI
		parentAuthor = cached.DisplayName
		parentSnippet = firstLineSnippet(cached.ContentText, 120)
	}

	quotedHTML := WrapReplyQuoteHTML(parentMRI, parentAuthor, parentID, parentSnippet, bodyHTML)

	return c.postMessage(ctx, conversationID, "", quotedHTML, text)
}

func (c *Client) postMessage(ctx context.Context, conversationID, rootMessageID, htmlContent, plainText string) (*ChatMessage, error) {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return nil, errors.New("msteamsclient: not authenticated")
	}

	targetConv := conversationID
	if rootMessageID != "" {
		targetConv = conversationID + ";messageid=" + rootMessageID
	}

	clientMsgID := strconv.FormatInt(time.Now().UnixNano(), 10)
	c.recordSent(clientMsgID, "")

	myMRI, displayName := c.selfIdentity()

	payload := map[string]any{
		msgFieldContent:     htmlContent,
		msgFieldMessageType: msgTypeRichTextHTML,
		msgFieldContentType: contentTypeText,
		"clientmessageid":   clientMsgID,
		"imdisplayname":     displayName,
		"properties":        map[string]any{"importance": ""},
	}

	reqURL := fmt.Sprintf("%s/v1/users/ME/conversations/%s/messages", ep.ChatService, url.PathEscape(targetConv))

	var resp sendMessageResponse

	err := c.doJSON(ctx, http.MethodPost, reqURL, authSkypeToken, "", payload, &resp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: send message to %s: %w", conversationID, err)
	}

	return c.buildSentMessage(conversationID, rootMessageID, clientMsgID, myMRI, displayName, htmlContent, plainText, resp.OriginalArrivalTime), nil
}

func (c *Client) selfIdentity() (string, string) {
	if me := c.Me(); me != nil {
		return me.MRI, me.DisplayName
	}

	return "", ""
}

func (c *Client) buildSentMessage(convID, rootID, clientMsgID, myMRI, displayName, htmlContent, plainText string, arrivalTime int64) *ChatMessage {
	msgID := ""
	if arrivalTime > 0 {
		msgID = strconv.FormatInt(arrivalTime, 10)
		c.recordSent(clientMsgID, msgID)
	}

	createAt := arrivalTime
	if createAt == 0 {
		createAt = time.Now().UnixMilli()
	}

	msg := &ChatMessage{
		ID:              msgID,
		ClientMessageID: clientMsgID,
		ConversationID:  convID,
		RootMessageID:   rootID,
		FromMRI:         myMRI,
		DisplayName:     displayName,
		MessageType:     msgTypeRichTextHTML,
		ContentType:     contentTypeText,
		ContentHTML:     htmlContent,
		ContentText:     plainText,
		CreateAt:        createAt,
	}

	if msgID != "" {
		c.postCache.Add(msgID, msg)
	}

	return msg
}

// EditMessage replaces the content of messageID in conversationID.
func (c *Client) EditMessage(ctx context.Context, conversationID, messageID, text string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	htmlContent := TextToHTML(text)
	payload := map[string]any{
		msgFieldContent:     htmlContent,
		msgFieldMessageType: msgTypeRichTextHTML,
		msgFieldContentType: contentTypeText,
		"skypeeditedid":     messageID,
	}

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages/%s",
		ep.ChatService,
		url.PathEscape(conversationID),
		url.PathEscape(messageID),
	)

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: edit message %s: %w", messageID, err)
	}

	if cached, ok := c.postCache.Get(messageID); ok && cached != nil {
		cp := *cached
		cp.ContentHTML = htmlContent
		cp.ContentText = text
		cp.EditAt = time.Now().UnixMilli()
		c.postCache.Add(messageID, &cp)
	}

	return nil
}

// DeleteMessage deletes messageID in conversationID.
func (c *Client) DeleteMessage(ctx context.Context, conversationID, messageID string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages/%s",
		ep.ChatService,
		url.PathEscape(conversationID),
		url.PathEscape(messageID),
	)

	err := c.doJSON(ctx, http.MethodDelete, reqURL, authSkypeToken, "", nil, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: delete message %s: %w", messageID, err)
	}

	return nil
}

// AddReaction adds an emotion/reaction key (e.g. "like", "heart", "laugh", or
// a Unicode emoji) to messageID in conversationID.
func (c *Client) AddReaction(ctx context.Context, conversationID, messageID, key string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages/%s/properties?name=emotions",
		ep.ChatService,
		url.PathEscape(conversationID),
		url.PathEscape(messageID),
	)

	payload := map[string]any{
		"emotions": map[string]string{
			"key":   key,
			"value": strconv.FormatInt(time.Now().UnixMilli(), 10),
		},
	}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: add reaction %q on %s: %w", key, messageID, err)
	}

	return nil
}

// RemoveReaction removes an emotion/reaction key from messageID in conversationID.
func (c *Client) RemoveReaction(ctx context.Context, conversationID, messageID, key string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages/%s/properties?name=emotions",
		ep.ChatService,
		url.PathEscape(conversationID),
		url.PathEscape(messageID),
	)

	payload := map[string]any{
		"emotions": map[string]string{
			"key": key,
		},
	}

	err := c.doJSON(ctx, http.MethodDelete, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: remove reaction %q on %s: %w", key, messageID, err)
	}

	return nil
}

// GetMessages fetches up to limit recent messages in conversationID, returned
// in chronological (oldest-first) order.
func (c *Client) GetMessages(ctx context.Context, conversationID string, limit int) ([]*ChatMessage, error) {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return nil, errors.New("msteamsclient: not authenticated")
	}

	if limit <= 0 {
		limit = defaultMessagesLimit
	}

	pageSize := min(limit, maxMessagesLimit)
	nextURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages?pageSize=%d&view=msnp24Equivalent%%7CsupportsMessageProperties",
		ep.ChatService,
		url.PathEscape(conversationID),
		pageSize,
	)

	var collected []*ChatMessage

	for len(collected) < limit && nextURL != "" {
		var resp rawMessagesResponse

		err := c.doJSON(ctx, http.MethodGet, nextURL, authSkypeToken, "", nil, &resp)
		if err != nil {
			if len(collected) > 0 {
				break
			}

			return nil, fmt.Errorf("msteamsclient: get messages for %s: %w", conversationID, err)
		}

		if len(resp.Messages) == 0 {
			break
		}

		collected = c.appendParsedMessages(collected, resp.Messages, conversationID, limit)

		if resp.Metadata.BackwardLink == "" || resp.Metadata.BackwardLink == nextURL {
			break
		}

		nextURL = resp.Metadata.BackwardLink
	}

	sortMessagesChronologically(collected)

	return collected, nil
}

func (c *Client) appendParsedMessages(dst []*ChatMessage, raw []rawChatMessage, fallbackConvID string, limit int) []*ChatMessage {
	for i := range raw {
		msg := c.parseRawChatMessage(&raw[i], fallbackConvID)
		if msg == nil {
			continue
		}

		dst = append(dst, msg)
		if len(dst) >= limit {
			break
		}
	}

	return dst
}

func sortMessagesChronologically(msgs []*ChatMessage) {
	slices.SortFunc(msgs, func(a, b *ChatMessage) int {
		if a.CreateAt < b.CreateAt {
			return -1
		}

		if a.CreateAt > b.CreateAt {
			return 1
		}

		return strings.Compare(a.ID, b.ID)
	})
}

// GetMessagesSince returns messages in conversationID created strictly after
// since (epoch milliseconds), in chronological order. Returns nil on API error
// so callers do not advance read state past missed history.
func (c *Client) GetMessagesSince(ctx context.Context, conversationID string, since int64) ([]*ChatMessage, error) {
	msgs, err := c.GetMessages(ctx, conversationID, maxMessagesLimit)
	if err != nil {
		return nil, err
	}

	out := make([]*ChatMessage, 0, len(msgs))
	for _, msg := range msgs {
		if msg.CreateAt > since {
			out = append(out, msg)
		}
	}

	return out, nil
}

// GetThreadMessages returns the root post and replies for a thread.
func (c *Client) GetThreadMessages(ctx context.Context, conversationID, rootMessageID string, limit int) ([]*ChatMessage, error) {
	if limit <= 0 {
		limit = defaultMessagesLimit
	}

	if IsTeamsChannel(conversationID) {
		threadConv := conversationID + ";messageid=" + rootMessageID

		return c.GetMessages(ctx, threadConv, limit)
	}

	msgs, err := c.GetMessages(ctx, conversationID, max(limit, 100))
	if err != nil {
		return nil, err
	}

	out := make([]*ChatMessage, 0)

	for _, msg := range msgs {
		if msg.ID == rootMessageID || msg.RootMessageID == rootMessageID {
			out = append(out, msg)
		}
	}

	return out, nil
}

// GetMessage returns a single message by ID, checking postCache first.
func (c *Client) GetMessage(ctx context.Context, conversationID, messageID string) (*ChatMessage, error) {
	if cached, ok := c.postCache.Get(messageID); ok && cached != nil {
		cp := *cached

		return &cp, nil
	}

	ep := c.Endpoints()
	if ep.ChatService == "" {
		return nil, errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/messages/%s?view=msnp24Equivalent%%7CsupportsMessageProperties",
		ep.ChatService,
		url.PathEscape(conversationID),
		url.PathEscape(messageID),
	)

	var raw rawChatMessage

	err := c.doJSON(ctx, http.MethodGet, reqURL, authSkypeToken, "", nil, &raw)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: get message %s: %w", messageID, err)
	}

	msg := c.parseRawChatMessage(&raw, conversationID)
	if msg == nil {
		return nil, errors.New("msteamsclient: empty message returned")
	}

	return msg, nil
}

// MarkConversationRead updates the user's consumption horizon for conversationID.
func (c *Client) MarkConversationRead(ctx context.Context, conversationID string, timestamp int64) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	if timestamp <= 0 {
		timestamp = time.Now().UnixMilli()
	}

	ts := strconv.FormatInt(timestamp, 10)
	horizon := ts + ";" + ts + ";" + ts

	reqURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations/%s/properties?name=consumptionhorizon",
		ep.ChatService,
		url.PathEscape(conversationID),
	)

	payload := map[string]string{
		"consumptionhorizon": horizon,
	}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: mark read %s: %w", conversationID, err)
	}

	c.Users.mu.Lock()

	c.Users.channelLastViewedAt[conversationID] = timestamp
	if conv, ok := c.Users.conversations[conversationID]; ok {
		conv.LastViewedAt = timestamp
	}

	c.Users.mu.Unlock()

	return nil
}

// SendTyping sends a debounced Control/Typing indicator to conversationID.
func (c *Client) SendTyping(ctx context.Context, conversationID string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	now := time.Now()

	c.typingLock.Lock()
	if last, ok := c.lastTypingSent[conversationID]; ok && now.Sub(last) < typingDebounceWindow {
		c.typingLock.Unlock()

		return nil
	}

	c.lastTypingSent[conversationID] = now
	c.typingLock.Unlock()

	reqURL := fmt.Sprintf("%s/v1/users/ME/conversations/%s/messages", ep.ChatService, url.PathEscape(conversationID))
	payload := map[string]any{
		msgFieldContent:     "",
		msgFieldMessageType: "Control/Typing",
		msgFieldContentType: "Application/Message",
	}

	return c.doJSON(ctx, http.MethodPost, reqURL, authSkypeToken, "", payload, nil)
}

// SearchMessages queries Microsoft Substrate for messages matching query.
func (c *Client) SearchMessages(ctx context.Context, query string, limit int) ([]*ChatMessage, error) {
	ep := c.Endpoints()
	if ep.Substrate == "" {
		return nil, errors.New("msteamsclient: substrate endpoint unavailable")
	}

	if limit <= 0 {
		limit = 25
	}

	payload := map[string]any{
		"EntityRequests": []map[string]any{
			{
				"EntityType": "Message",
				"Query":      map[string]string{"QueryString": query},
				"From":       0,
				"Size":       limit,
			},
		},
	}

	var resp substrateSearchResponse

	err := c.doJSON(ctx, http.MethodPost, ep.Substrate+"/search/api/v2/query", authBearerScope, ScopeSubstrate, payload, &resp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: search messages: %w", err)
	}

	return flattenSubstrateSearch(&resp), nil
}

func flattenSubstrateSearch(resp *substrateSearchResponse) []*ChatMessage {
	out := make([]*ChatMessage, 0)

	for _, es := range resp.EntitySets {
		for _, rs := range es.ResultSets {
			for _, r := range rs.Results {
				src := r.Source

				convID := src.ClientThreadID
				if convID == "" {
					convID = src.ThreadID
				}

				if convID == "" || src.MessageID == "" {
					continue
				}

				out = append(out, &ChatMessage{
					ID:             src.MessageID,
					ConversationID: convID,
					FromMRI:        extractMRIFromURL(src.SenderMRI),
					DisplayName:    src.SenderName,
					ContentHTML:    src.Content,
					ContentText:    HTMLToText(src.Content),
					CreateAt:       parseTeamsTimeMillis(src.DateTimeSent, ""),
				})
			}
		}
	}

	return out
}

// IsOwnMessageEcho reports whether a message was recently sent by this client.
func (c *Client) IsOwnMessageEcho(clientMsgID, serverMsgID string) bool {
	if clientMsgID != "" && c.sentCache.Contains(clientMsgID) {
		return true
	}

	if serverMsgID != "" && c.sentCache.Contains(serverMsgID) {
		return true
	}

	return false
}

func (c *Client) recordSent(clientMsgID, serverMsgID string) {
	if clientMsgID != "" {
		c.sentCache.Add(clientMsgID, struct{}{})
	}

	if serverMsgID != "" {
		c.sentCache.Add(serverMsgID, struct{}{})
	}
}

// parseRawChatMessage normalizes a raw chatsvc/Trouter message payload.
func (c *Client) parseRawChatMessage(raw *rawChatMessage, fallbackConvID string) *ChatMessage {
	if raw == nil || strings.HasPrefix(raw.MessageType, "Control/") || strings.HasPrefix(raw.MessageType, "ThreadActivity/") {
		return nil
	}

	convID, rootID := splitConversationAndRoot(raw.ConversationID, raw.ConversationLink, fallbackConvID)
	if convID == "" {
		return nil
	}

	if rootID == "" && raw.Content != "" {
		rootID = ExtractReplyParentID(raw.Content)
	}

	// A channel root post may carry ;messageid=<its own ID>; clear self-parent.
	if rootID == raw.ID {
		rootID = ""
	}

	fromMRI := extractMRIFromURL(raw.From)
	if fromMRI != "" && raw.IMDisplayName != "" {
		c.CacheUser(&User{MRI: fromMRI, DisplayName: raw.IMDisplayName})
	}

	text := raw.Content
	if strings.EqualFold(raw.MessageType, msgTypeRichTextHTML) || strings.ContainsRune(raw.Content, '<') {
		text = HTMLToText(raw.Content)
	}

	emotions := parseRawEmotions(raw.Properties.Emotions, raw.Emotions)

	msg := &ChatMessage{
		ID:              raw.ID,
		ClientMessageID: raw.ClientMessageID,
		ConversationID:  convID,
		RootMessageID:   rootID,
		FromMRI:         fromMRI,
		DisplayName:     raw.IMDisplayName,
		MessageType:     raw.MessageType,
		ContentType:     raw.ContentType,
		ContentHTML:     raw.Content,
		ContentText:     text,
		CreateAt:        parseTeamsTimeMillis(raw.ComposeTime, raw.OriginalArrivalTime),
		EditAt:          parseInt64Millis(raw.Properties.EditTime),
		DeleteAt:        parseInt64Millis(raw.Properties.DeleteTime),
		Version:         raw.Version,
		Mentions:        parseRawMentions(raw.Properties.Mentions),
		Attachments:     parseRawFiles(raw.Properties.Files),
		Emotions:        emotions,
	}

	c.cacheParsedMessage(msg)

	return msg
}

func (c *Client) cacheParsedMessage(msg *ChatMessage) {
	if msg.ID == "" {
		return
	}

	c.postCache.Add(msg.ID, msg)

	if len(msg.Emotions) > 0 {
		c.emotionCache.Add(msg.ID, msg.Emotions)
	}
}

func splitConversationAndRoot(convID, convLink, fallback string) (string, string) {
	raw := convID
	if raw == "" && convLink != "" {
		if idx := strings.LastIndex(convLink, "/conversations/"); idx >= 0 {
			rest := convLink[idx+len("/conversations/"):]
			rest, _, _ = strings.Cut(rest, "/")
			rest, _, _ = strings.Cut(rest, "?")

			unescaped, err := url.PathUnescape(rest)
			if err == nil {
				raw = unescaped
			} else {
				raw = rest
			}
		}
	}

	if raw == "" {
		raw = fallback
	}

	baseConv, rootID := splitMessageIDParam(raw)
	if rootID == "" && convLink != "" {
		if _, linkRoot := splitMessageIDParam(convLink); linkRoot != "" {
			rootID = linkRoot
		}
	}

	return baseConv, rootID
}

func splitMessageIDParam(s string) (string, string) {
	before, after, found := strings.Cut(s, ";messageid=")
	if !found {
		return s, ""
	}

	rootID, _, _ := strings.Cut(after, "/")
	rootID, _, _ = strings.Cut(rootID, "?")
	rootID, _, _ = strings.Cut(rootID, ";")

	return before, rootID
}

func extractMRIFromURL(raw string) string {
	if raw == "" {
		return ""
	}

	if idx := strings.LastIndex(raw, "/contacts/"); idx >= 0 {
		mri := raw[idx+len("/contacts/"):]
		mri, _, _ = strings.Cut(mri, "?")

		unescaped, err := url.PathUnescape(mri)
		if err == nil {
			return unescaped
		}

		return mri
	}

	return raw
}

func parseTeamsTimeMillis(primary, fallback string) int64 {
	for _, s := range []string{primary, fallback} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}

		ms, err := strconv.ParseInt(s, 10, 64)
		if err == nil && ms > 0 {
			return ms
		}

		t, err := time.Parse(time.RFC3339Nano, s)
		if err == nil {
			return t.UnixMilli()
		}

		t, err = time.Parse(time.RFC3339, s)
		if err == nil {
			return t.UnixMilli()
		}
	}

	return 0
}

func parseInt64Millis(raw string) int64 {
	if raw == "" {
		return 0
	}

	ms, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0
	}

	return ms
}

func parseRawEmotions(propEmotions, topEmotions []rawEmotion) map[string][]ReactionUser {
	src := propEmotions
	if len(src) == 0 {
		src = topEmotions
	}

	if len(src) == 0 {
		return nil
	}

	out := make(map[string][]ReactionUser, len(src))
	for _, em := range src {
		if em.Key == "" || len(em.Users) == 0 {
			continue
		}

		users := make([]ReactionUser, 0, len(em.Users))
		for _, u := range em.Users {
			mri := extractMRIFromURL(u.MRI)
			if mri == "" {
				continue
			}

			users = append(users, ReactionUser{
				MRI:  mri,
				Time: u.Time,
			})
		}

		if len(users) > 0 {
			out[em.Key] = users
		}
	}

	return out
}

func parseRawMentions(raw string) []Mention {
	if raw == "" {
		return nil
	}

	var parsed []rawMention

	err := json.Unmarshal([]byte(raw), &parsed)
	if err != nil || len(parsed) == 0 {
		return nil
	}

	out := make([]Mention, 0, len(parsed))
	for _, m := range parsed {
		if m.Mentioned.User.ID == "" && m.MentionText == "" {
			continue
		}

		out = append(out, Mention{
			MRI:         m.Mentioned.User.ID,
			DisplayName: m.MentionText,
			Type:        m.Mentioned.User.UserType,
		})
	}

	return out
}

func parseRawFiles(raw string) []Attachment {
	if raw == "" {
		return nil
	}

	var parsed []rawFileAttachment

	err := json.Unmarshal([]byte(raw), &parsed)
	if err != nil || len(parsed) == 0 {
		return nil
	}

	out := make([]Attachment, 0, len(parsed))
	for _, f := range parsed {
		link := f.ObjectURL
		if link == "" {
			link = f.FileInfo.FileURL
		}

		name := f.Title
		if name == "" {
			name = f.FileName
		}

		if link == "" && name == "" {
			continue
		}

		out = append(out, Attachment{
			ID:          f.ID,
			Name:        name,
			ContentType: f.FileType,
			URL:         link,
		})
	}

	return out
}

func firstLineSnippet(text string, maxLen int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if len(line) > maxLen {
		return line[:maxLen] + "..."
	}

	return line
}
