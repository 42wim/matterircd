package googlechatclient

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

const (
	defaultHistoryLimit = 50
	maxHistoryLimit     = 200
	defaultReplyLimit   = 10
	typingDebounce      = 3 * time.Second
	maxSearchConvs      = 25
	searchMsgsPerConv   = 40
)

// IsOwnMessageEcho reports whether id or clientMsgID corresponds to a message
// recently sent by this client instance.
func (c *Client) IsOwnMessageEcho(id, clientMsgID string) bool {
	if id != "" && c.sentCache.Contains(id) {
		return true
	}

	if clientMsgID != "" && c.sentCache.Contains(clientMsgID) {
		return true
	}

	return false
}

// GetCachedPost returns a copy of a previously seen ChatMessage by ID, or nil.
func (c *Client) GetCachedPost(id string) *ChatMessage {
	if id == "" {
		return nil
	}

	msg, ok := c.postCache.Get(id)
	if !ok || msg == nil {
		return nil
	}

	return cloneChatMessage(msg)
}

func (c *Client) cachePost(msg *ChatMessage) {
	if msg == nil || msg.ID == "" {
		return
	}

	cp := cloneChatMessage(msg)
	c.postCache.Add(cp.ID, cp)

	if len(cp.Reactions) > 0 {
		c.reactionCache.Add(cp.ID, cloneReactionsMap(cp.Reactions))
	}
}

func (c *Client) recordSentIDs(id, localID string) {
	if id != "" {
		c.sentCache.Add(id, struct{}{})
	}

	if localID != "" {
		c.sentCache.Add(localID, struct{}{})
	}
}

func newLocalMessageID() string {
	var raw [8]byte

	_, _ = rand.Read(raw[:])

	return "gc-local-" + hex.EncodeToString(raw[:])
}

// SendMessage sends a top-level message to a Space or DM via create_topic.
func (c *Client) encodeQuoteRef(fallbackConvID string, quote *QuoteRef) []byte {
	if quote == nil || quote.MessageID == "" {
		return nil
	}

	convID := quote.ConversationID
	if convID == "" {
		convID = fallbackConvID
	}

	topicID := quote.TopicID
	if topicID == "" {
		topicID = quote.MessageID
	}

	return encodeQuotedMessagePayload(c.protoGroupRefFor(convID), topicID, quote.MessageID, quote.LastUpdateTimeUsec)
}

func (c *Client) SendMessage(ctx context.Context, convID, text string) (*ChatMessage, error) {
	return c.sendMessage(ctx, convID, text, nil)
}

// SendQuotedMessage sends a top-level message with a native Google Chat "Quote in reply" attachment.
func (c *Client) SendQuotedMessage(ctx context.Context, convID, text string, quote QuoteRef) (*ChatMessage, error) {
	return c.sendMessage(ctx, convID, text, &quote)
}

func (c *Client) sendMessage(ctx context.Context, convID, text string, quote *QuoteRef) (*ChatMessage, error) {
	if convID == "" {
		return nil, errors.New("googlechatclient: empty conversation ID")
	}

	ref := c.protoGroupRefFor(convID)
	localID := newLocalMessageID()
	c.recordSentIDs("", localID)

	bodyText, anns := c.buildOutgoingAnnotations(text)
	reqBytes := encodeCreateTopicRequest(ref, bodyText, localID, anns, c.encodeQuoteRef(convID, quote))

	respBytes, err := c.doProto(ctx, "create_topic", reqBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: create_topic: %w", err)
	}

	topic, err := decodeCreateTopicResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode create_topic: %w", err)
	}

	msg := c.extractSentMessageFromTopic(topic, convID, localID, bodyText)
	c.recordSentIDs(msg.ID, localID)
	c.cachePost(msg)
	c.touchConversation(convID, msg.CreateAt)

	return cloneChatMessage(msg), nil
}

func (c *Client) extractSentMessageFromTopic(topic *protoTopic, convID, localID, text string) *ChatMessage {
	if topic != nil && len(topic.Replies) > 0 && topic.Replies[0] != nil {
		return c.protoMessageToChatMessage(topic.Replies[0], convID, topic.TopicID)
	}

	topicID := ""
	if topic != nil {
		topicID = topic.TopicID
	}

	now := time.Now()

	return &ChatMessage{
		ID:                 topicID,
		TopicID:            topicID,
		ClientMessageID:    localID,
		ConversationID:     convID,
		FromID:             c.selfUserID(),
		ContentText:        text,
		CreateAt:           now.UnixMilli(),
		LastUpdateTimeUsec: now.UnixMicro(),
	}
}

// SendMessageThread sends an inline thread reply within topicID via create_message.
func (c *Client) SendMessageThread(ctx context.Context, convID, topicID, text string) (*ChatMessage, error) {
	return c.sendMessageThread(ctx, convID, topicID, text, nil)
}

// SendQuotedMessageThread sends an inline thread reply with a native Google Chat "Quote in reply" attachment.
func (c *Client) SendQuotedMessageThread(ctx context.Context, convID, topicID, text string, quote QuoteRef) (*ChatMessage, error) {
	return c.sendMessageThread(ctx, convID, topicID, text, &quote)
}

func (c *Client) sendMessageThread(ctx context.Context, convID, topicID, text string, quote *QuoteRef) (*ChatMessage, error) {
	if convID == "" || topicID == "" {
		return nil, errors.New("googlechatclient: conversation ID and topic ID are required")
	}

	ref := c.protoGroupRefFor(convID)
	localID := newLocalMessageID()
	c.recordSentIDs("", localID)

	bodyText, anns := c.buildOutgoingAnnotations(text)
	reqBytes := encodeCreateMessageRequest(ref, topicID, bodyText, localID, anns, c.encodeQuoteRef(convID, quote))

	respBytes, err := c.doProto(ctx, "create_message", reqBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: create_message: %w", err)
	}

	pm, err := decodeSingleMessageResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode create_message: %w", err)
	}

	msg := c.protoMessageToChatMessage(pm, convID, topicID)
	if msg == nil {
		now := time.Now()

		msg = &ChatMessage{
			TopicID:            topicID,
			ClientMessageID:    localID,
			ConversationID:     convID,
			RootMessageID:      topicID,
			FromID:             c.selfUserID(),
			ContentText:        bodyText,
			CreateAt:           now.UnixMilli(),
			LastUpdateTimeUsec: now.UnixMicro(),
		}
	}

	c.recordSentIDs(msg.ID, localID)
	c.cachePost(msg)
	c.touchConversation(convID, msg.CreateAt)

	return cloneChatMessage(msg), nil
}

// EditMessage edits an existing message via edit_message.
func (c *Client) EditMessage(ctx context.Context, convID, topicID, msgID, text string) (*ChatMessage, error) {
	if convID == "" || msgID == "" {
		return nil, errors.New("googlechatclient: conversation ID and message ID are required")
	}

	if topicID == "" {
		topicID = c.resolveTopicIDForMessage(msgID)
	}

	ref := c.protoGroupRefFor(convID)
	bodyText, anns := c.buildOutgoingAnnotations(text)
	reqBytes := encodeEditMessageRequest(ref, topicID, msgID, bodyText, anns)

	respBytes, err := c.doProto(ctx, "edit_message", reqBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: edit_message: %w", err)
	}

	pm, err := decodeSingleMessageResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode edit_message: %w", err)
	}

	msg := c.protoMessageToChatMessage(pm, convID, topicID)
	if msg == nil {
		msg = &ChatMessage{
			ID:             msgID,
			TopicID:        topicID,
			ConversationID: convID,
			FromID:         c.selfUserID(),
			ContentText:    bodyText,
			EditAt:         time.Now().UnixMilli(),
		}
	}

	c.cachePost(msg)

	return cloneChatMessage(msg), nil
}

// DeleteMessage deletes an existing message via delete_message.
func (c *Client) DeleteMessage(ctx context.Context, convID, topicID, msgID string) error {
	if convID == "" || msgID == "" {
		return errors.New("googlechatclient: conversation ID and message ID are required")
	}

	if topicID == "" {
		topicID = c.resolveTopicIDForMessage(msgID)
	}

	ref := c.protoGroupRefFor(convID)

	_, err := c.doProto(ctx, "delete_message", encodeDeleteMessageRequest(ref, topicID, msgID))
	if err != nil {
		return fmt.Errorf("googlechatclient: delete_message: %w", err)
	}

	return nil
}

// AddReaction adds a Unicode emoji reaction to a message via update_reaction.
func (c *Client) AddReaction(ctx context.Context, convID, topicID, msgID, emojiUnicode string) error {
	return c.updateReaction(ctx, convID, topicID, msgID, emojiUnicode, true)
}

// RemoveReaction removes a Unicode emoji reaction from a message via update_reaction.
func (c *Client) RemoveReaction(ctx context.Context, convID, topicID, msgID, emojiUnicode string) error {
	return c.updateReaction(ctx, convID, topicID, msgID, emojiUnicode, false)
}

func (c *Client) updateReaction(ctx context.Context, convID, topicID, msgID, emojiUnicode string, add bool) error {
	if convID == "" || msgID == "" || emojiUnicode == "" {
		return errors.New("googlechatclient: conversation ID, message ID, and emoji are required")
	}

	if topicID == "" {
		topicID = c.resolveTopicIDForMessage(msgID)
	}

	ref := c.protoGroupRefFor(convID)
	reqBytes := encodeUpdateReactionRequest(ref, topicID, msgID, emojiUnicode, add)

	_, err := c.doProto(ctx, "update_reaction", reqBytes)
	if err != nil {
		return fmt.Errorf("googlechatclient: update_reaction: %w", err)
	}

	return nil
}

// SendTyping sends a debounced typing indicator to convID (and optional topicID).
func (c *Client) SendTyping(ctx context.Context, convID, topicID string) error {
	if convID == "" {
		return nil
	}

	key := convID + "\x00" + topicID
	now := time.Now()

	c.typingLock.Lock()

	if last, ok := c.lastTypingSent[key]; ok && now.Sub(last) < typingDebounce {
		c.typingLock.Unlock()

		return nil
	}

	c.lastTypingSent[key] = now
	c.typingLock.Unlock()

	ref := c.protoGroupRefFor(convID)

	_, err := c.doProto(ctx, "set_typing_state", encodeSetTypingStateRequest(ref, topicID, true))
	if err != nil {
		return fmt.Errorf("googlechatclient: set_typing_state: %w", err)
	}

	return nil
}

// GetMessages fetches recent messages from convID (oldest first) via list_topics.
func (c *Client) GetMessages(ctx context.Context, convID string, limit int) ([]*ChatMessage, error) {
	if convID == "" {
		return nil, errors.New("googlechatclient: empty conversation ID")
	}

	limit = clampHistoryLimit(limit)
	ref := c.protoGroupRefFor(convID)

	respBytes, err := c.doProto(ctx, "list_topics", encodeListTopicsRequest(ref, clampedInt32(limit), defaultReplyLimit))
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: list_topics: %w", err)
	}

	topics, err := decodeListTopicsResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode list_topics: %w", err)
	}

	msgs := c.flattenTopics(topics, convID)
	if len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}

	return msgs, nil
}

// GetMessagesSince fetches messages in convID created after since (oldest first).
// It tries catch_up_group first and falls back to list_topics filtered by timestamp.
func (c *Client) GetMessagesSince(ctx context.Context, convID string, since time.Time, limit int) ([]*ChatMessage, error) {
	if convID == "" {
		return nil, errors.New("googlechatclient: empty conversation ID")
	}

	limit = clampHistoryLimit(limit)
	sinceMs := since.UnixMilli()

	msgs, err := c.catchUpMessagesSince(ctx, convID, since, limit)
	if err == nil && len(msgs) > 0 {
		return msgs, nil
	}

	all, err := c.GetMessages(ctx, convID, limit)
	if err != nil {
		return nil, err
	}

	var filtered []*ChatMessage

	for _, m := range all {
		if m.CreateAt > sinceMs {
			filtered = append(filtered, m)
		}
	}

	return filtered, nil
}

func (c *Client) catchUpMessagesSince(ctx context.Context, convID string, since time.Time, limit int) ([]*ChatMessage, error) {
	ref := c.protoGroupRefFor(convID)
	// Advance past the sub-millisecond tail of `since` (matching slackclient) so
	// messages whose microsecond fraction was truncated into millisecond cursors
	// are not returned a second time on reconnect/replay.
	reqBytes := encodeCatchUpGroupRequest(ref, since.UnixMicro()+999, clampedInt32(limit))

	respBytes, err := c.doProto(ctx, "catch_up_group", reqBytes)
	if err != nil {
		return nil, err
	}

	events, err := decodeCatchUpResponse(respBytes)
	if err != nil {
		return nil, err
	}

	sinceMs := since.UnixMilli()
	byID := make(map[string]*ChatMessage)
	deleted := make(map[string]struct{})

	for _, ev := range events {
		c.applyCatchUpEvent(ev, convID, sinceMs, byID, deleted)
	}

	out := make([]*ChatMessage, 0, len(byID))

	for id, msg := range byID {
		if _, isDel := deleted[id]; isDel {
			continue
		}

		c.cachePost(msg)
		out = append(out, msg)
	}

	sortMessagesByTime(out)

	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	return out, nil
}

func (c *Client) applyCatchUpEvent(
	ev *protoEvent,
	convID string,
	sinceMs int64,
	byID map[string]*ChatMessage,
	deleted map[string]struct{},
) {
	if ev == nil {
		return
	}

	for _, body := range ev.Bodies {
		if body.DeletedMsgID != "" {
			deleted[body.DeletedMsgID] = struct{}{}
			delete(byID, body.DeletedMsgID)
		}

		if body.MessagePosted != nil {
			c.upsertCatchUpMessage(body.MessagePosted, convID, "", sinceMs, byID, deleted)
		}

		if body.TopicCreated != nil {
			for _, pm := range body.TopicCreated.Replies {
				c.upsertCatchUpMessage(pm, convID, body.TopicCreated.TopicID, sinceMs, byID, deleted)
			}
		}
	}
}

func (c *Client) upsertCatchUpMessage(
	pm *protoMessage,
	convID, topicID string,
	sinceMs int64,
	byID map[string]*ChatMessage,
	deleted map[string]struct{},
) {
	msg := c.protoMessageToChatMessage(pm, convID, topicID)
	if msg == nil || msg.ID == "" {
		return
	}

	if msg.DeleteAt > 0 {
		deleted[msg.ID] = struct{}{}
		delete(byID, msg.ID)

		return
	}

	if msg.CreateAt <= sinceMs {
		return
	}

	if _, isDel := deleted[msg.ID]; isDel {
		return
	}

	byID[msg.ID] = msg
}

// GetThreadMessages fetches all messages inside an inline thread via list_messages.
func (c *Client) GetThreadMessages(ctx context.Context, convID, topicID string, limit int) ([]*ChatMessage, error) {
	if convID == "" || topicID == "" {
		return nil, errors.New("googlechatclient: conversation ID and topic ID are required")
	}

	limit = clampHistoryLimit(limit)
	ref := c.protoGroupRefFor(convID)

	respBytes, err := c.doProto(ctx, "list_messages", encodeListMessagesRequest(ref, topicID, clampedInt32(limit)))
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: list_messages: %w", err)
	}

	pms, err := decodeListMessagesResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode list_messages: %w", err)
	}

	out := make([]*ChatMessage, 0, len(pms))

	for _, pm := range pms {
		msg := c.protoMessageToChatMessage(pm, convID, topicID)
		if msg == nil {
			continue
		}

		c.cachePost(msg)
		out = append(out, msg)
	}

	sortMessagesByTime(out)

	return out, nil
}

// SearchMessages searches Google Chat messages using the server-side
// batchexecute search RPC, falling back to scanning recent messages across
// active Spaces and DMs if the server search fails.
func (c *Client) SearchMessages(ctx context.Context, query string, limit int) ([]*ChatMessage, error) {
	trimmed := strings.TrimSpace(query)
	if trimmed == "" {
		return nil, nil
	}

	limit = clampHistoryLimit(limit)

	msgs, err := c.searchMessagesServer(ctx, trimmed, limit)
	if err == nil {
		return msgs, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	c.logger.Debugf("Server-side search failed, falling back to local scan: %v", err)

	return c.searchMessagesLocal(ctx, strings.ToLower(trimmed), limit)
}

func (c *Client) searchMessagesLocal(ctx context.Context, needle string, limit int) ([]*ChatMessage, error) {
	convs := c.GetChannels()

	slices.SortFunc(convs, func(a, b *Conversation) int {
		return cmp.Compare(b.LastPostAt, a.LastPostAt)
	})

	if len(convs) > maxSearchConvs {
		convs = convs[:maxSearchConvs]
	}

	var out []*ChatMessage

	for _, conv := range convs {
		err := ctx.Err()
		if err != nil {
			return out, err
		}

		if conv.IsHidden {
			continue
		}

		msgs, fetchErr := c.GetMessages(ctx, conv.ID, searchMsgsPerConv)
		if fetchErr != nil {
			continue
		}

		for _, m := range msgs {
			if strings.Contains(strings.ToLower(m.ContentText), needle) {
				out = append(out, m)
			}
		}

		if len(out) >= limit {
			break
		}
	}

	sortMessagesByTime(out)

	if len(out) > limit {
		out = out[len(out)-limit:]
	}

	return out, nil
}

func (c *Client) flattenTopics(topics []*protoTopic, convID string) []*ChatMessage {
	var out []*ChatMessage

	for _, t := range topics {
		if t == nil {
			continue
		}

		for _, pm := range t.Replies {
			msg := c.protoMessageToChatMessage(pm, convID, t.TopicID)
			if msg == nil {
				continue
			}

			if t.IsSystem {
				msg.IsSystem = true
			}

			c.cachePost(msg)
			out = append(out, msg)
		}
	}

	sortMessagesByTime(out)

	return out
}

func (c *Client) protoMessageToChatMessage(pm *protoMessage, fallbackConvID, fallbackTopicID string) *ChatMessage {
	if pm == nil {
		return nil
	}

	convID := pm.Group.ID
	if convID == "" {
		convID = fallbackConvID
	}

	topicID := pm.TopicID
	if topicID == "" {
		topicID = fallbackTopicID
	}

	msgID := pm.MessageID
	if msgID == "" {
		msgID = topicID
	}

	mentions, attachments, text := c.extractMentionsAndAttachments(pm)
	fromID, displayName := c.extractMessageCreator(pm)
	isSystem, topicChanged, newTopic, initiator, newName := c.extractSystemMetadata(pm)

	if fromID == "" && initiator != "" {
		fromID = initiator
	}

	if newName != "" && (text == "" || text == "Space Updated") {
		text = "Space renamed to: " + newName
	}

	// In Google Chat inline threads, a top-level post has MessageID == TopicID,
	// whereas an inline reply has MessageID != TopicID (or IsInlineReply == true).
	rootID := ""
	if topicID != "" && msgID != "" && topicID != msgID {
		rootID = topicID
	}

	msg := &ChatMessage{
		ID:                 msgID,
		TopicID:            topicID,
		ClientMessageID:    pm.LocalID,
		ConversationID:     convID,
		RootMessageID:      rootID,
		FromID:             fromID,
		DisplayName:        displayName,
		ContentText:        text,
		CreateAt:           usecToMillis(pm.CreateTimeUsec),
		EditAt:             usecToMillis(pm.EditTimeUsec),
		DeleteAt:           usecToMillis(pm.DeleteTimeUsec),
		LastUpdateTimeUsec: max(pm.UpdateTimeUsec, pm.EditTimeUsec, pm.CreateTimeUsec),
		IsSystem:           isSystem,
		TopicChanged:       topicChanged,
		NewTopic:           newTopic,
		QuotedMessage:      c.protoQuotedToQuotedMessage(pm.Quoted, convID, text),
		Mentions:           mentions,
		Attachments:        attachments,
		Reactions:          c.protoReactionsToMap(pm.Reactions),
	}

	return msg
}

func (c *Client) extractSystemMetadata(pm *protoMessage) (bool, bool, string, string, string) {
	isSystem := pm.MessageType == protoMsgTypeSystem

	var (
		topicChanged bool
		newTopic     string
		initiator    string
		newName      string
	)

	for _, ann := range pm.Annotations {
		if ann.RoomUpdated {
			isSystem = true

			if ann.RoomInitiator != "" {
				initiator = ann.RoomInitiator
			}

			if ann.RoomDescUpdated {
				topicChanged = true
				newTopic = ann.RoomNewDesc
			}

			if ann.RoomNewName != "" {
				newName = ann.RoomNewName
			}

			if len(ann.RoomUnknownFields) > 0 {
				c.logger.Debugf("RoomUpdatedMetadata extra fields on message %s: %v", pm.MessageID, ann.RoomUnknownFields)
			}
		}

		if ann.MemberChanged || ann.MemberAction != 0 {
			isSystem = true
		}
	}

	return isSystem, topicChanged, newTopic, initiator, newName
}

func (c *Client) protoQuotedToQuotedMessage(pq *protoQuotedMessage, destConvID, destText string) *QuotedMessage {
	if pq == nil {
		return nil
	}

	c.rememberGroupRef(pq.Group)

	if len(pq.UnknownFields) > 0 {
		c.logger.Debugf("QuotedMessageMetadata extra fields on message %s: %v", pq.MessageID, pq.UnknownFields)
	}

	fromID, displayName := c.extractQuotedCreator(pq)

	quotedText := pq.TextBody
	if quotedText != "" && len(pq.Annotations) > 0 {
		quotedText = c.formatAnnotatedText(quotedText, pq.Annotations)
	}

	if pq.MessageID == "" && quotedText == "" && !pq.Deleted {
		return nil
	}

	isCrossConv := pq.Group.ID != "" && destConvID != "" && pq.Group.ID != destConvID
	isForwarded := isCrossConv || strings.TrimSpace(destText) == ""

	return &QuotedMessage{
		ID:             pq.MessageID,
		ConversationID: pq.Group.ID,
		TopicID:        pq.TopicID,
		FromID:         fromID,
		DisplayName:    displayName,
		Text:           quotedText,
		Deleted:        pq.Deleted,
		IsForwarded:    isForwarded,
	}
}

func (c *Client) extractQuotedCreator(pq *protoQuotedMessage) (string, string) {
	if pq.Creator == nil {
		return pq.CreatorID, ""
	}

	if u := protoUserToUser(pq.Creator); u != nil {
		c.CacheUser(u)

		if pq.CreatorID != "" {
			return pq.CreatorID, u.DisplayName
		}

		return u.ID, u.DisplayName
	}

	fromID := pq.CreatorID
	if fromID == "" {
		fromID = pq.Creator.ID
	}

	return fromID, pq.Creator.Name
}

func (c *Client) extractMessageCreator(pm *protoMessage) (string, string) {
	if pm.Creator == nil {
		return "", ""
	}

	if u := protoUserToUser(pm.Creator); u != nil {
		c.CacheUser(u)

		return u.ID, u.DisplayName
	}

	return pm.Creator.ID, pm.Creator.Name
}

func (c *Client) protoReactionsToMap(reactions []protoReaction) map[string][]ReactionUser {
	if len(reactions) == 0 {
		return nil
	}

	myID := c.selfUserID()
	out := make(map[string][]ReactionUser, len(reactions))

	for _, r := range reactions {
		if r.Emoji == "" || r.Count <= 0 {
			continue
		}

		var users []ReactionUser
		if r.Own && myID != "" {
			users = append(users, ReactionUser{UserID: myID, Time: usecToMillis(r.Created)})
		}

		out[r.Emoji] = users
	}

	return out
}

func (c *Client) resolveTopicIDForMessage(msgID string) string {
	if cached := c.GetCachedPost(msgID); cached != nil && cached.TopicID != "" {
		return cached.TopicID
	}

	return msgID
}

func clampHistoryLimit(limit int) int {
	if limit <= 0 {
		return defaultHistoryLimit
	}

	if limit > maxHistoryLimit {
		return maxHistoryLimit
	}

	return limit
}

func clampedInt32(limit int) int32 {
	return int32(clampHistoryLimit(limit)) //nolint:gosec // bounded <= maxHistoryLimit
}

func sortMessagesByTime(msgs []*ChatMessage) {
	slices.SortFunc(msgs, func(a, b *ChatMessage) int {
		if c := cmp.Compare(a.CreateAt, b.CreateAt); c != 0 {
			return c
		}

		return cmp.Compare(a.ID, b.ID)
	})
}

func cloneChatMessage(in *ChatMessage) *ChatMessage {
	if in == nil {
		return nil
	}

	cp := *in
	if in.QuotedMessage != nil {
		qcp := *in.QuotedMessage
		cp.QuotedMessage = &qcp
	}

	cp.Mentions = slices.Clone(in.Mentions)
	cp.Attachments = slices.Clone(in.Attachments)
	cp.Reactions = cloneReactionsMap(in.Reactions)

	return &cp
}

func cloneReactionsMap(in map[string][]ReactionUser) map[string][]ReactionUser {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string][]ReactionUser, len(in))

	for k, v := range in {
		out[k] = slices.Clone(v)
	}

	return out
}

func mergeReactionsDiff(prev, next map[string][]ReactionUser) (map[string][]string, map[string][]string) {
	added := make(map[string][]string)
	removed := make(map[string][]string)

	allKeys := make(map[string]struct{}, len(prev)+len(next))

	for k := range maps.Keys(prev) {
		allKeys[k] = struct{}{}
	}

	for k := range maps.Keys(next) {
		allKeys[k] = struct{}{}
	}

	for emoji := range allKeys {
		prevSet := reactionUserSet(prev[emoji])
		nextSet := reactionUserSet(next[emoji])

		for uid := range nextSet {
			if _, ok := prevSet[uid]; !ok {
				added[emoji] = append(added[emoji], uid)
			}
		}

		for uid := range prevSet {
			if _, ok := nextSet[uid]; !ok {
				removed[emoji] = append(removed[emoji], uid)
			}
		}
	}

	return added, removed
}

func reactionUserSet(list []ReactionUser) map[string]struct{} {
	out := make(map[string]struct{}, len(list))

	for _, ru := range list {
		if ru.UserID != "" {
			out[ru.UserID] = struct{}{}
		}
	}

	return out
}
