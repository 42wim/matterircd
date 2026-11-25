package googlechat

import (
	"context"
	"strings"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/matterbridge/googlechatclient"
)

const (
	systemUser = "system"

	channelTypeDirect = "D"
	channelTypePublic = "O"
	channelTypeGroup  = "G"

	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"
	previewCharNonUnicode    = "|"
	previewCharUnicode       = "🮇"
	previewSpaceNonUnicode   = " "
	previewSpaceUnicode      = "\u00a0"
)

func (g *GoogleChat) handleGoogleChat() {
	for {
		select {
		case <-g.done:
			return
		case msg, ok := <-g.gc.MessageChan:
			if !ok {
				return
			}

			if msg == nil {
				continue
			}

			g.dispatchClientMessage(msg)
		}
	}
}

func (g *GoogleChat) dispatchClientMessage(msg *googlechatclient.Message) {
	switch msg.Type {
	case googlechatclient.EventPosted:
		g.handlePosted(msg)
	case googlechatclient.EventPostEdited:
		g.handlePostEdited(msg)
	case googlechatclient.EventPostDeleted:
		g.handlePostDeleted(msg)
	case googlechatclient.EventReactionAdded:
		g.handleReaction(msg, true)
	case googlechatclient.EventReactionRemoved:
		g.handleReaction(msg, false)
	case googlechatclient.EventTyping:
		g.handleTyping(msg)
	case googlechatclient.EventChannelTopic:
		g.handleTopicChange(msg)
	case googlechatclient.EventMemberAdded:
		g.handleMemberChange(msg, true)
	case googlechatclient.EventMemberRemoved:
		g.handleMemberChange(msg, false)
	case googlechatclient.EventConversationUpdate:
		g.handleConversationUpdate(msg)
	case googlechatclient.EventPresenceChange:
		g.handlePresenceChange(msg)
	case googlechatclient.EventConnected:
		g.connected.Store(true)
	case googlechatclient.EventDisconnected:
		logger.Debugf("WebChannel disconnected: %v", msg.Err)
	}
}

func (g *GoogleChat) sendEvent(ev *bridge.Event) {
	select {
	case g.eventChan <- ev:
	case <-g.done:
	}
}

func (g *GoogleChat) handlePosted(msg *googlechatclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	// Suppress echoes of messages sent by this IRC session.
	if g.gc.IsOwnMessageEcho(post.ID, post.ClientMessageID) {
		return
	}

	if post.ID != "" && g.replayedMsgCache.Contains(post.ID) {
		g.replayedMsgCache.Remove(post.ID)

		return
	}

	g.ensureConversationSynced(g.ctx, post.ConversationID)

	ev := g.chatMessageToEvent(g.ctx, post, "posted")
	if ev != nil {
		g.sendEvent(ev)
	}

	if len(post.Attachments) > 0 {
		g.emitFileEvent(post)
	}
}

func (g *GoogleChat) handlePostEdited(msg *googlechatclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	rc := g.cfg.Current()
	if rc.GoogleChat.DisableShowOwnModified && g.isOwnUserID(post.FromID) {
		return
	}

	ev := g.chatMessageToEvent(g.ctx, post, "post_edited")
	if ev != nil {
		g.sendEvent(ev)
	}
}

func (g *GoogleChat) handlePostDeleted(msg *googlechatclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	rc := g.cfg.Current()
	if rc.GoogleChat.DisableShowOwnModified && g.isOwnUserID(post.FromID) {
		return
	}

	encodedID := encodeGoogleChatID(post.ID)
	if post.ContentText == "" {
		if cached, ok := g.msgParentCache.Get(encodedID); ok && cached.ReplyMsg != "" {
			post.ContentText = cached.ReplyMsg
		}
	}

	g.msgParentCache.Remove(encodedID)

	ev := g.chatMessageToEvent(g.ctx, post, "post_deleted")
	if ev != nil {
		g.sendEvent(ev)
	}
}

func (g *GoogleChat) handleReaction(msg *googlechatclient.Message, added bool) {
	rc := g.cfg.Current()
	if rc.GoogleChat.HideReactions || msg.Reaction == nil {
		return
	}

	if !rc.GoogleChat.ShowOwnReactions && g.isOwnUserID(msg.Reaction.UserID) {
		return
	}

	r := msg.Reaction

	sender := g.resolveUserInfo(g.ctx, r.UserID, "")
	if sender == nil {
		return
	}

	isDM := g.isDMConversation(r.ConversationID)
	encodedMsgID := encodeGoogleChatID(r.MessageID)
	g.rememberMessageMeta(r.MessageID, r.ConversationID, r.TopicID, isDM)

	parentID := ""
	if r.RootMessageID != "" {
		parentID = encodeGoogleChatID(r.RootMessageID)
		g.rememberMessageMeta(r.RootMessageID, r.ConversationID, r.RootMessageID, isDM)
	}

	parentUser, parentText := g.reactionParentContext(encodedMsgID, msg.Post, rc.GoogleChat.ShortenRepliesTo)

	replySuffix := ""

	if !rc.GoogleChat.HideReplies && parentText != "" {
		if parentUser != nil && parentUser.Nick != "" {
			replySuffix = " (re @" + parentUser.Nick + ": " + parentText + ")"
		} else {
			replySuffix = " (re: " + parentText + ")"
		}
	}

	evData := &bridge.ReactionAddEvent{
		Receiver:    g.dmReceiverFor(r.ConversationID, sender),
		Sender:      sender,
		ChannelID:   r.ConversationID,
		MessageID:   encodedMsgID,
		Reaction:    r.Key,
		ChannelType: g.bridgeChannelType(r.ConversationID),
		ParentUser:  parentUser,
		Message:     replySuffix,
		ParentID:    parentID,
	}

	if added {
		g.sendEvent(&bridge.Event{Type: "reaction_add", Data: evData})

		return
	}

	rem := bridge.ReactionRemoveEvent(*evData)
	g.sendEvent(&bridge.Event{Type: "reaction_remove", Data: &rem})
}

func (g *GoogleChat) reactionParentContext(encodedMsgID string, post *googlechatclient.ChatMessage, shorten int) (*bridge.UserInfo, string) {
	if cached, ok := g.msgParentCache.Get(encodedMsgID); ok {
		return cached.ParentUser, cached.ReplyMsg
	}

	if post == nil {
		rawID, ok := decodeGoogleChatID(encodedMsgID)
		if ok {
			post = g.gc.GetCachedPost(rawID)
		}
	}

	if post == nil {
		return nil, ""
	}

	return g.resolveUserInfo(g.ctx, post.FromID, post.DisplayName), g.formatSummary(post.ContentText, shorten)
}

func (g *GoogleChat) handleTyping(msg *googlechatclient.Message) {
	if !g.cfg.Current().EnableTyping || msg.Channel == "" || g.isOwnUserID(msg.UserID) {
		return
	}

	g.ensureConversationSynced(g.ctx, msg.Channel)

	sender := g.resolveUserInfo(g.ctx, msg.UserID, msg.Username)
	if sender == nil {
		return
	}

	parentID := ""
	if msg.Topic != "" {
		parentID = encodeGoogleChatID(msg.Topic)
	}

	logger.Tracef("Received typing from %s in %s (topic=%s)", sender.Nick, msg.Channel, msg.Topic)

	g.sendEvent(&bridge.Event{
		Type: "typing",
		Data: &bridge.TypingEvent{
			ChannelID:   msg.Channel,
			ChannelType: g.bridgeChannelType(msg.Channel),
			Receiver:    g.dmReceiverFor(msg.Channel, sender),
			Sender:      sender,
			ParentID:    parentID,
		},
	})
}

func (g *GoogleChat) handleTopicChange(msg *googlechatclient.Message) {
	if msg.Channel == "" {
		return
	}

	g.rebuildNames()

	g.sendEvent(&bridge.Event{
		Type: "channel_topic",
		Data: &bridge.ChannelTopicEvent{
			Text:      msg.Topic,
			ChannelID: msg.Channel,
			UserID:    msg.UserID,
		},
	})
}

func (g *GoogleChat) handleMemberChange(msg *googlechatclient.Message, added bool) {
	if msg.Channel == "" || len(msg.Members) == 0 || g.isDMConversation(msg.Channel) {
		return
	}

	g.ensureConversationSynced(g.ctx, msg.Channel)

	if g.isDMConversation(msg.Channel) {
		return
	}

	actor := g.resolveUserInfo(g.ctx, msg.UserID, "")
	users := make([]*bridge.UserInfo, 0, len(msg.Members))

	for _, uid := range msg.Members {
		if info := g.resolveUserInfo(g.ctx, uid, ""); info != nil {
			users = append(users, info)
		}
	}

	if len(users) == 0 {
		return
	}

	g.rebuildNames()

	ts := msg.Timestamp.UnixMilli()
	if added {
		g.sendEvent(&bridge.Event{
			Type: "channel_add",
			Data: &bridge.ChannelAddEvent{
				Adder:     actor,
				Added:     users,
				ChannelID: msg.Channel,
				CreateAt:  ts,
			},
		})

		return
	}

	g.sendEvent(&bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover:   actor,
			Removed:   users,
			ChannelID: msg.Channel,
			CreateAt:  ts,
		},
	})
}

func (g *GoogleChat) handleConversationUpdate(msg *googlechatclient.Message) {
	if msg.Channel == "" {
		return
	}

	g.rebuildNames()

	conv := g.gc.GetChannel(msg.Channel)
	if conv == nil {
		return
	}

	g.sendEvent(&bridge.Event{
		Type: "channel_update",
		Data: &bridge.ChannelUpdateEvent{
			ChannelID:   conv.ID,
			Name:        g.ircChannelName(conv.ID),
			DisplayName: conv.DisplayName,
		},
	})
}

func (g *GoogleChat) handlePresenceChange(msg *googlechatclient.Message) {
	if msg.UserID == "" {
		return
	}

	status := msg.Text
	if status == "" && msg.Presence != nil {
		base := googlechatclient.MapAvailabilityToStatus(msg.Presence)

		note := googlechatclient.FormatStatusNote(msg.Presence)
		if note != "" {
			status = base + ": " + note
		} else {
			status = base
		}
	}

	if status == "" {
		return
	}

	g.sendEvent(&bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: msg.UserID,
			Status: status,
		},
	})
}

func (g *GoogleChat) chatMessageToEvent(ctx context.Context, post *googlechatclient.ChatMessage, eventKind string) *bridge.Event {
	if post == nil || post.ConversationID == "" {
		return nil
	}

	isDM := g.isDMConversation(post.ConversationID)

	if post.TopicChanged && !isDM && (post.ContentText == "" || post.ContentText == "Space Updated") {
		return &bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				ChannelID: post.ConversationID,
				Text:      post.NewTopic,
				UserID:    post.FromID,
			},
		}
	}

	if post.IsSystem {
		return g.systemMessageToEvent(ctx, post, isDM, eventKind)
	}

	sender := g.resolveUserInfo(ctx, post.FromID, post.DisplayName)
	if sender == nil {
		return nil
	}

	encodedID := encodeGoogleChatID(post.ID)
	g.rememberMessageMeta(post.ID, post.ConversationID, post.TopicID, isDM)

	rc := g.cfg.Current()
	parentID := g.resolveIncomingParentID(ctx, post, isDM, rc.GoogleChat.ShortenRepliesTo)
	text := g.formatIncomingText(ctx, post, eventKind, parentID, rc)

	files := attachmentsToBridgeFiles(post.Attachments)
	if text == "" && len(files) == 0 {
		return nil
	}

	if eventKind != "post_deleted" {
		g.cachePostContext(encodedID, parentID, sender, post.ContentText, rc.GoogleChat.ShortenRepliesTo)
	}

	return g.buildMessageBridgeEvent(post, sender, files, text, encodedID, parentID, eventKind)
}

func (g *GoogleChat) systemMessageToEvent(
	ctx context.Context,
	post *googlechatclient.ChatMessage,
	isDM bool,
	eventKind string,
) *bridge.Event {
	text := strings.TrimSpace(post.ContentText)
	if text == "" {
		return nil
	}

	var sender *bridge.UserInfo
	if isDM {
		sender = g.resolveUserInfo(ctx, post.FromID, post.DisplayName)
	} else {
		sender = &bridge.UserInfo{Nick: systemUser}
	}

	if sender == nil {
		return nil
	}

	return g.buildMessageBridgeEvent(post, sender, nil, text, "", "", eventKind)
}

func (g *GoogleChat) buildMessageBridgeEvent(
	post *googlechatclient.ChatMessage,
	sender *bridge.UserInfo,
	files []*bridge.File,
	text, encodedID, parentID, eventKind string,
) *bridge.Event {
	if g.isDMConversation(post.ConversationID) {
		return &bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: post.ConversationID,
				Receiver:  g.dmReceiverFor(post.ConversationID, sender),
				Sender:    sender,
				Files:     files,
				MessageID: encodedID,
				Event:     eventKind,
				ParentID:  parentID,
				CreateAt:  post.CreateAt,
			},
		}
	}

	return &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   post.ConversationID,
			Sender:      sender,
			ChannelType: g.bridgeChannelType(post.ConversationID),
			Files:       files,
			MessageID:   encodedID,
			Event:       eventKind,
			ParentID:    parentID,
			CreateAt:    post.CreateAt,
		},
	}
}

func (g *GoogleChat) resolveIncomingParentID(
	ctx context.Context,
	post *googlechatclient.ChatMessage,
	isDM bool,
	shorten int,
) string {
	quotedEncoded := ""
	if post.QuotedMessage != nil && post.QuotedMessage.ID != "" {
		quotedEncoded = g.cacheQuotedContext(ctx, post, isDM, shorten)
	}

	if post.RootMessageID != "" {
		parentID := encodeGoogleChatID(post.RootMessageID)
		g.rememberMessageMeta(post.RootMessageID, post.ConversationID, post.RootMessageID, isDM)

		return parentID
	}

	if post.QuotedMessage != nil && post.QuotedMessage.IsForwarded {
		return ""
	}

	return quotedEncoded
}

func (g *GoogleChat) cacheQuotedContext(
	ctx context.Context,
	post *googlechatclient.ChatMessage,
	isDM bool,
	shorten int,
) string {
	q := post.QuotedMessage
	encoded := encodeGoogleChatID(q.ID)

	topicID := q.TopicID
	if topicID == "" {
		topicID = q.ID
	}

	sourceConvID := q.ConversationID
	sourceIsDM := isDM

	if sourceConvID == "" {
		sourceConvID = post.ConversationID
	} else if sourceConvID != post.ConversationID {
		sourceIsDM = g.isDMConversation(sourceConvID)
	}

	g.rememberMessageMeta(q.ID, sourceConvID, topicID, sourceIsDM)

	if !q.IsForwarded && q.Text != "" && !g.msgParentCache.Contains(encoded) {
		quotedUser := g.resolveUserInfo(ctx, q.FromID, q.DisplayName)
		g.cachePostContext(encoded, "", quotedUser, q.Text, shorten)
	}

	return encoded
}

func (g *GoogleChat) formatIncomingText(
	ctx context.Context,
	post *googlechatclient.ChatMessage,
	eventKind, parentID string,
	rc *config.RuntimeConfig,
) string {
	text := post.ContentText
	useUnicode := rc.GoogleChat.Formatter.Unicode

	if post.QuotedMessage != nil && post.QuotedMessage.IsForwarded {
		text = g.appendForwardedPreview(ctx, text, post.QuotedMessage, rc)
	} else if quoted, ok := g.prependQuotedMessage(ctx, text, post.QuotedMessage); ok {
		text = quoted
	} else if !rc.GoogleChat.HideReplies {
		text = g.appendReplyContext(text, parentID)
	}

	switch eventKind {
	case "post_edited":
		text = utils.AppendMessageSuffix(text, utils.FormatActionSuffix(utils.SuffixEdited, useUnicode))
	case "post_deleted":
		text = utils.AppendMessageSuffix(text, utils.FormatActionSuffix(utils.SuffixDeleted, useUnicode))
	}

	return text
}

func (g *GoogleChat) appendForwardedPreview(
	ctx context.Context,
	baseText string,
	q *googlechatclient.QuotedMessage,
	rc *config.RuntimeConfig,
) string {
	if q == nil {
		return baseText
	}

	body := strings.TrimRight(q.Text, " \t\r\n")
	if body == "" && q.Deleted {
		body = "(deleted)"
	}

	if body == "" {
		return baseText
	}

	author := g.forwardedAuthorName(ctx, q)
	channel := g.forwardedChannelName(q)

	var b strings.Builder

	b.Grow(len(baseText) + len(body) + 128)
	b.WriteString(baseText)

	if b.Len() > 0 {
		b.WriteByte('\n')
	}

	b.WriteString(utils.AttachmentMarkerStart)
	b.WriteByte('\n')
	g.renderForwardedPreview(&b, author, channel, body, rc)
	b.WriteByte('\n')
	b.WriteString(utils.AttachmentMarkerEnd)

	return b.String()
}

func (g *GoogleChat) forwardedAuthorName(ctx context.Context, q *googlechatclient.QuotedMessage) string {
	if u := g.resolveUserInfo(ctx, q.FromID, q.DisplayName); u != nil && u.Nick != "" {
		return u.Nick
	}

	if q.DisplayName != "" {
		return q.DisplayName
	}

	return "unknown"
}

func (g *GoogleChat) forwardedChannelName(q *googlechatclient.QuotedMessage) string {
	if q.ConversationID == "" || g.isDMConversation(q.ConversationID) {
		return ""
	}

	if name := g.ircChannelName(q.ConversationID); name != "" {
		return "#" + name
	}

	return ""
}

func forwardedPreviewStyle(fc config.FormatterConfig) (string, utils.ProcessMessageOpts) {
	prefixChar := previewCharNonUnicode
	spaceChar := previewSpaceNonUnicode
	blockquoteChar := blockquoteCharNonUnicode
	codeBlockPrefix := fc.CodeBlockPrefix

	if fc.Unicode {
		prefixChar = previewCharUnicode
		spaceChar = previewSpaceUnicode
		blockquoteChar = blockquoteCharUnicode

		if strings.ContainsAny(codeBlockPrefix, "┃🮇▎") {
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "┃", "│", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "🮇", "▕", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "▎", "▏", 1)
		}
	}

	if fc.MarkdownBlockQuoteChar != "" {
		blockquoteChar = fc.MarkdownBlockQuoteChar
	}

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       fc.DisableEmoji,
		CustomEmoji:        fc.CustomEmoji,
		DisableMarkdown:    fc.DisableMarkdown,
		SyntaxHighlighting: fc.SyntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: fc.CodeBlockSeparator,
		BlockquoteChar:     blockquoteChar,
		InlineCodeChar:     fc.MarkdownInlineCode,
		Unicode:            fc.Unicode,
		PreserveNewLines:   fc.PreserveNewLines,
	}

	return prefixChar + spaceChar, opts
}

func (g *GoogleChat) renderForwardedPreview(
	b *strings.Builder,
	author, channel, body string,
	rc *config.RuntimeConfig,
) {
	prefix, opts := forwardedPreviewStyle(rc.GoogleChat.Formatter)

	b.WriteString(prefix)
	b.WriteString("\x02@")
	b.WriteString(author)
	b.WriteString("\x02 wrote")

	if channel != "" {
		b.WriteString(" in \x1d")
		b.WriteString(channel)
		b.WriteByte('\x1d')
	}

	b.WriteByte(':')

	trimmedBQ := strings.TrimSpace(opts.BlockquoteChar)
	trimmedCB := strings.TrimSpace(opts.CodeBlockPrefix)

	var pendingLines []string

	utils.ProcessMessageText(body, opts, func(line string) {
		line = strings.TrimRight(line, " \t\r")
		trimmed := strings.TrimSpace(line)

		if trimmed == "" || trimmed == trimmedCB || trimmed == trimmedBQ {
			pendingLines = append(pendingLines, line)

			return
		}

		for _, pending := range pendingLines {
			b.WriteByte('\n')
			b.WriteString(prefix)
			b.WriteString(pending)
		}

		pendingLines = pendingLines[:0]

		b.WriteByte('\n')
		b.WriteString(prefix)
		b.WriteString(line)
	})
}

func (g *GoogleChat) prependQuotedMessage(
	ctx context.Context,
	baseText string,
	q *googlechatclient.QuotedMessage,
) (string, bool) {
	if q == nil {
		return baseText, false
	}

	body := strings.ReplaceAll(q.Text, "\r\n", "\n")
	body = strings.Trim(body, "\r\n")

	if body == "" && q.Deleted {
		body = "(deleted)"
	}

	if body == "" {
		return baseText, false
	}

	var author string
	if u := g.resolveUserInfo(ctx, q.FromID, q.DisplayName); u != nil && u.Nick != "" {
		author = "@" + u.Nick
	} else {
		author = strings.TrimSpace(q.DisplayName)
	}

	lines := strings.Split(body, "\n")

	var b strings.Builder

	b.Grow(len(author) + len(body) + len(lines)*2 + len(baseText) + 24)

	if author != "" {
		b.WriteString("> **")
		b.WriteString(author)
		b.WriteString("** wrote:\n")
	}

	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}

		if strings.TrimSpace(line) == "" {
			b.WriteByte('>')
		} else {
			b.WriteString("> ")
			b.WriteString(line)
		}
	}

	if baseText != "" {
		b.WriteByte('\n')
		b.WriteString(baseText)
	}

	return b.String(), true
}

func (g *GoogleChat) appendReplyContext(text, parentID string) string {
	if parentID != "" {
		if parent, ok := g.msgParentCache.Get(parentID); ok && parent.ParentUser != nil && parent.ReplyMsg != "" {
			return text + " (re @" + parent.ParentUser.Nick + ": " + parent.ReplyMsg + ")"
		}
	}

	return text
}

func (g *GoogleChat) cachePostContext(encodedID, parentID string, sender *bridge.UserInfo, plainText string, shorten int) {
	if encodedID == "" || plainText == "" {
		return
	}

	rootID := parentID
	if rootID == "" {
		rootID = encodedID
	}

	g.msgParentCache.Add(encodedID, CachedPost{
		RootID:     rootID,
		ParentUser: sender,
		ReplyMsg:   g.formatSummary(plainText, shorten),
	})
}

func (g *GoogleChat) emitFileEvent(post *googlechatclient.ChatMessage) {
	files := attachmentsToBridgeFiles(post.Attachments)
	if len(files) == 0 {
		return
	}

	sender := g.resolveUserInfo(g.ctx, post.FromID, post.DisplayName)
	if sender == nil {
		return
	}

	encodedID := encodeGoogleChatID(post.ID)

	parentID := ""
	if post.RootMessageID != "" {
		parentID = encodeGoogleChatID(post.RootMessageID)
	}

	g.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Receiver:    g.dmReceiverFor(post.ConversationID, sender),
			Sender:      sender,
			ChannelID:   post.ConversationID,
			ChannelType: g.bridgeChannelType(post.ConversationID),
			Files:       files,
			MessageID:   encodedID,
			ParentID:    parentID,
			CreateAt:    post.CreateAt,
		},
	})
}

func (g *GoogleChat) formatSummary(text string, maxLen int) string {
	rc := g.cfg.Current()

	bq := "|"

	switch {
	case rc.GoogleChat.Formatter.DisableMarkdownBlockQuote || rc.GoogleChat.Formatter.DisableMarkdown:
		bq = utils.BlockQuoteCharDefault
	case rc.GoogleChat.Formatter.MarkdownBlockQuoteChar != "":
		bq = rc.GoogleChat.Formatter.MarkdownBlockQuoteChar
	case rc.GoogleChat.Formatter.Unicode:
		bq = "▕"
	}

	return utils.FormatAndShortenSummary(text, utils.SummaryOpts{
		MaxLength:       maxLen,
		DisableMarkdown: rc.GoogleChat.Formatter.DisableMarkdown,
		DisableEmoji:    rc.GoogleChat.Formatter.DisableEmoji,
		BlockquoteChar:  bq,
		InlineCodeChar:  rc.GoogleChat.Formatter.MarkdownInlineCode,
		CustomEmoji:     rc.GoogleChat.Formatter.CustomEmoji,
		Unicode:         rc.GoogleChat.Formatter.Unicode,
	})
}

func attachmentsToBridgeFiles(atts []googlechatclient.Attachment) []*bridge.File {
	if len(atts) == 0 {
		return nil
	}

	out := make([]*bridge.File, 0, len(atts))

	for _, a := range atts {
		if a.URL == "" && a.Name == "" {
			continue
		}

		out = append(out, &bridge.File{
			Name: a.Name,
			Size: a.Size,
			URL:  a.URL,
		})
	}

	return out
}

func ircEmojiToGoogleChatEmoji(emoji string, customEmoji map[string]string) string {
	clean := strings.Trim(strings.TrimSpace(emoji), ":")
	if unicodeEmoji, ok := utils.EmojiFromAlias(clean, customEmoji); ok && unicodeEmoji != "" {
		return unicodeEmoji
	}

	return clean
}

func (g *GoogleChat) bridgeChannelType(convID string) string {
	conv := g.gc.GetChannel(convID)
	if conv == nil {
		return channelTypePublic
	}

	switch conv.Type {
	case googlechatclient.ConversationDM, googlechatclient.ConversationSelfNote:
		return channelTypeDirect
	case googlechatclient.ConversationGroupDM:
		return channelTypeGroup
	default:
		return channelTypePublic
	}
}

func (g *GoogleChat) ensureConversationSynced(ctx context.Context, convID string) {
	if convID == "" || g.gc.GetChannel(convID) != nil {
		return
	}

	_, err := g.gc.SyncConversation(ctx, convID)
	if err != nil {
		logger.Debugf("ensureConversationSynced(%s) failed: %v", convID, err)

		return
	}

	g.rebuildNames()
}

func (g *GoogleChat) isDMConversation(convID string) bool {
	conv := g.gc.GetChannel(convID)
	if conv == nil {
		return false
	}

	return conv.Type == googlechatclient.ConversationDM || conv.Type == googlechatclient.ConversationSelfNote
}

func (g *GoogleChat) dmReceiverFor(convID string, sender *bridge.UserInfo) *bridge.UserInfo {
	if !g.isDMConversation(convID) {
		return nil
	}

	me := g.GetMe()
	if me == nil {
		return nil
	}

	if sender != nil && sender.User != me.User {
		return me
	}

	otherID, ok := g.GetDMOtherUserID(convID, me.User)
	if !ok || otherID == "" {
		return me
	}

	if other := g.resolveUserInfo(g.ctx, otherID, ""); other != nil {
		return other
	}

	return me
}
