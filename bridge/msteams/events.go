package msteams

import (
	"context"
	"strings"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/matterbridge/msteamsclient"
)

const (
	channelTypeDirect = "D"
	channelTypePublic = "O"
	channelTypeGroup  = "G"

	emotionLike      = "like"
	emotionHeart     = "heart"
	emotionLaugh     = "laugh"
	emotionSurprised = "surprised"
	emotionSad       = "sad"
	emotionAngry     = "angry"
)

func (m *MSTeams) handleMSTeams() {
	for {
		select {
		case <-m.done:
			return
		case msg, ok := <-m.mc.MessageChan:
			if !ok {
				return
			}

			if msg == nil {
				continue
			}

			m.dispatchClientMessage(msg)
		}
	}
}

func (m *MSTeams) dispatchClientMessage(msg *msteamsclient.Message) {
	switch msg.Type {
	case msteamsclient.EventPosted:
		m.handlePosted(msg)
	case msteamsclient.EventPostEdited:
		m.handlePostEdited(msg)
	case msteamsclient.EventPostDeleted:
		m.handlePostDeleted(msg)
	case msteamsclient.EventReactionAdded:
		m.handleReaction(msg, true)
	case msteamsclient.EventReactionRemoved:
		m.handleReaction(msg, false)
	case msteamsclient.EventTyping:
		m.handleTyping(msg)
	case msteamsclient.EventChannelTopic:
		m.handleTopicChange(msg)
	case msteamsclient.EventMemberAdded:
		m.handleMemberChange(msg, true)
	case msteamsclient.EventMemberRemoved:
		m.handleMemberChange(msg, false)
	case msteamsclient.EventConversationUpdate:
		m.handleConversationUpdate(msg)
	case msteamsclient.EventPresenceChange:
		m.handlePresenceChange(msg)
	case msteamsclient.EventConnected:
		m.connected.Store(true)
	case msteamsclient.EventDisconnected:
		logger.Debugf("Trouter disconnected: %v", msg.Err)
	}
}

func (m *MSTeams) sendEvent(ev *bridge.Event) {
	select {
	case m.eventChan <- ev:
	case <-m.done:
	}
}

func (m *MSTeams) handlePosted(msg *msteamsclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	// Suppress echoes of messages sent by this IRC session.
	if m.mc.IsOwnMessageEcho(post.ClientMessageID, post.ID) {
		return
	}

	m.ensureConversationSynced(m.ctx, post.ConversationID)

	ev := m.chatMessageToEvent(m.ctx, post, "posted")
	if ev != nil {
		m.sendEvent(ev)
	}

	if len(post.Attachments) > 0 {
		m.emitFileEvent(post)
	}
}

func (m *MSTeams) handlePostEdited(msg *msteamsclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	rc := m.cfg.Current()
	if rc.MSTeams.DisableShowOwnModified && m.isOwnMRI(post.FromMRI) {
		return
	}

	ev := m.chatMessageToEvent(m.ctx, post, "post_edited")
	if ev != nil {
		m.sendEvent(ev)
	}
}

func (m *MSTeams) handlePostDeleted(msg *msteamsclient.Message) {
	post := msg.Post
	if post == nil {
		return
	}

	rc := m.cfg.Current()
	if rc.MSTeams.DisableShowOwnModified && m.isOwnMRI(post.FromMRI) {
		return
	}

	encodedID := encodeTeamsID(post.ConversationID, post.ID)
	if post.ContentText == "" {
		if cached, ok := m.msgParentCache.Get(encodedID); ok && cached.ReplyMsg != "" {
			post.ContentText = cached.ReplyMsg
		}
	}

	m.msgParentCache.Remove(encodedID)

	ev := m.chatMessageToEvent(m.ctx, post, "post_deleted")
	if ev != nil {
		m.sendEvent(ev)
	}
}

func (m *MSTeams) handleReaction(msg *msteamsclient.Message, added bool) {
	rc := m.cfg.Current()
	if rc.MSTeams.HideReactions || msg.Reaction == nil {
		return
	}

	if !rc.MSTeams.ShowOwnReactions && m.isOwnMRI(msg.Reaction.UserMRI) {
		return
	}

	r := msg.Reaction

	sender := m.resolveUserInfo(m.ctx, r.UserMRI, "")
	if sender == nil {
		return
	}

	encodedMsgID := encodeTeamsID(r.ConversationID, r.MessageID)
	m.rememberMessageConv(encodedMsgID, r.ConversationID)

	parentID := ""
	if r.RootMessageID != "" {
		parentID = encodeTeamsID(r.ConversationID, r.RootMessageID)
		m.rememberMessageConv(parentID, r.ConversationID)
	}

	parentUser, parentText := m.reactionParentContext(encodedMsgID, msg.Post, rc.MSTeams.ShortenRepliesTo)

	replySuffix := ""

	if !rc.MSTeams.HideReplies && parentText != "" {
		if parentUser != nil && parentUser.Nick != "" {
			replySuffix = " (re @" + parentUser.Nick + ": " + parentText + ")"
		} else {
			replySuffix = " (re: " + parentText + ")"
		}
	}

	evData := &bridge.ReactionAddEvent{
		Receiver:    m.dmReceiverFor(r.ConversationID, sender),
		Sender:      sender,
		ChannelID:   r.ConversationID,
		MessageID:   encodedMsgID,
		Reaction:    teamsEmotionToIRC(r.Key),
		ChannelType: m.bridgeChannelType(r.ConversationID),
		ParentUser:  parentUser,
		Message:     replySuffix,
		ParentID:    parentID,
	}

	if added {
		m.sendEvent(&bridge.Event{Type: "reaction_add", Data: evData})

		return
	}

	rem := bridge.ReactionRemoveEvent(*evData)
	m.sendEvent(&bridge.Event{Type: "reaction_remove", Data: &rem})
}

func (m *MSTeams) reactionParentContext(encodedMsgID string, post *msteamsclient.ChatMessage, shorten int) (*bridge.UserInfo, string) {
	if cached, ok := m.msgParentCache.Get(encodedMsgID); ok {
		return cached.ParentUser, cached.ReplyMsg
	}

	if post == nil {
		return nil, ""
	}

	return m.resolveUserInfo(m.ctx, post.FromMRI, post.DisplayName), m.formatSummary(post.ContentText, shorten)
}

func (m *MSTeams) handleTyping(msg *msteamsclient.Message) {
	if !m.cfg.Current().EnableTyping || msg.Channel == "" || m.isOwnMRI(msg.UserID) {
		return
	}

	sender := m.resolveUserInfo(m.ctx, msg.UserID, msg.Username)
	if sender == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "typing",
		Data: &bridge.TypingEvent{
			ChannelID:   msg.Channel,
			ChannelType: m.bridgeChannelType(msg.Channel),
			Receiver:    m.dmReceiverFor(msg.Channel, sender),
			Sender:      sender,
		},
	})
}

func (m *MSTeams) handleTopicChange(msg *msteamsclient.Message) {
	if msg.Channel == "" {
		return
	}

	m.rebuildNames()

	m.sendEvent(&bridge.Event{
		Type: "channel_topic",
		Data: &bridge.ChannelTopicEvent{
			Text:      msg.Topic,
			ChannelID: msg.Channel,
			UserID:    msg.UserID,
		},
	})
}

func (m *MSTeams) handleMemberChange(msg *msteamsclient.Message, added bool) {
	if msg.Channel == "" || len(msg.Members) == 0 || m.isDMConversation(msg.Channel) {
		return
	}

	m.ensureConversationSynced(m.ctx, msg.Channel)

	actor := m.resolveUserInfo(m.ctx, msg.UserID, "")
	users := make([]*bridge.UserInfo, 0, len(msg.Members))

	for _, mri := range msg.Members {
		if info := m.resolveUserInfo(m.ctx, mri, ""); info != nil {
			users = append(users, info)
		}
	}

	if len(users) == 0 {
		return
	}

	m.rebuildNames()

	ts := msg.Timestamp.UnixMilli()
	if added {
		m.sendEvent(&bridge.Event{
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

	m.sendEvent(&bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover:   actor,
			Removed:   users,
			ChannelID: msg.Channel,
			CreateAt:  ts,
		},
	})
}

func (m *MSTeams) handleConversationUpdate(msg *msteamsclient.Message) {
	if msg.Channel == "" {
		return
	}

	m.rebuildNames()

	conv := m.mc.GetChannel(msg.Channel)
	if conv == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "channel_update",
		Data: &bridge.ChannelUpdateEvent{
			ChannelID:   conv.ID,
			Name:        m.ircChannelName(conv.ID),
			DisplayName: conv.DisplayName,
		},
	})
}

func (m *MSTeams) handlePresenceChange(msg *msteamsclient.Message) {
	if msg.UserID == "" || msg.Text == "" {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: msg.UserID,
			Status: msg.Text,
		},
	})
}

func (m *MSTeams) chatMessageToEvent(ctx context.Context, post *msteamsclient.ChatMessage, eventKind string) *bridge.Event {
	if post == nil || post.ConversationID == "" {
		return nil
	}

	sender := m.resolveUserInfo(ctx, post.FromMRI, post.DisplayName)
	if sender == nil {
		return nil
	}

	encodedID := encodeTeamsID(post.ConversationID, post.ID)
	m.rememberMessageConv(encodedID, post.ConversationID)

	parentID := ""
	if post.RootMessageID != "" {
		parentID = encodeTeamsID(post.ConversationID, post.RootMessageID)
		m.rememberMessageConv(parentID, post.ConversationID)
	}

	rc := m.cfg.Current()
	text := m.formatIncomingText(post, eventKind, parentID, rc)

	files := attachmentsToBridgeFiles(post.Attachments)
	if text == "" && len(files) == 0 {
		return nil
	}

	if eventKind != "post_deleted" {
		m.cachePostContext(encodedID, parentID, sender, post.ContentText, rc.MSTeams.ShortenRepliesTo)
	}

	return m.buildMessageBridgeEvent(post, sender, files, text, encodedID, parentID, eventKind)
}

func (m *MSTeams) buildMessageBridgeEvent(post *msteamsclient.ChatMessage, sender *bridge.UserInfo, files []*bridge.File, text, encodedID, parentID, eventKind string) *bridge.Event {
	if m.isDMConversation(post.ConversationID) {
		return &bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: post.ConversationID,
				Receiver:  m.dmReceiverFor(post.ConversationID, sender),
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
			ChannelType: m.bridgeChannelType(post.ConversationID),
			Files:       files,
			MessageID:   encodedID,
			Event:       eventKind,
			ParentID:    parentID,
			CreateAt:    post.CreateAt,
		},
	}
}

func (m *MSTeams) formatIncomingText(post *msteamsclient.ChatMessage, eventKind, parentID string, rc *config.RuntimeConfig) string {
	text := post.ContentText
	useUnicode := rc.MSTeams.Formatter.Unicode

	if parentID != "" && !rc.MSTeams.HideReplies {
		if parent, ok := m.msgParentCache.Get(parentID); ok && parent.ParentUser != nil && parent.ReplyMsg != "" {
			text = text + " (re @" + parent.ParentUser.Nick + ": " + parent.ReplyMsg + ")"
		}
	}

	switch eventKind {
	case "post_edited":
		text = utils.AppendMessageSuffix(text, utils.FormatActionSuffix(utils.SuffixEdited, useUnicode))
	case "post_deleted":
		text = utils.AppendMessageSuffix(text, utils.FormatActionSuffix(utils.SuffixDeleted, useUnicode))
	}

	return text
}

func (m *MSTeams) cachePostContext(encodedID, parentID string, sender *bridge.UserInfo, plainText string, shorten int) {
	if encodedID == "" || plainText == "" {
		return
	}

	rootID := parentID
	if rootID == "" {
		rootID = encodedID
	}

	m.msgParentCache.Add(encodedID, CachedPost{
		RootID:     rootID,
		ParentUser: sender,
		ReplyMsg:   m.formatSummary(plainText, shorten),
	})
}

func (m *MSTeams) emitFileEvent(post *msteamsclient.ChatMessage) {
	files := attachmentsToBridgeFiles(post.Attachments)
	if len(files) == 0 {
		return
	}

	sender := m.resolveUserInfo(m.ctx, post.FromMRI, post.DisplayName)
	if sender == nil {
		return
	}

	encodedID := encodeTeamsID(post.ConversationID, post.ID)

	parentID := ""
	if post.RootMessageID != "" {
		parentID = encodeTeamsID(post.ConversationID, post.RootMessageID)
	}

	m.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Receiver:    m.dmReceiverFor(post.ConversationID, sender),
			Sender:      sender,
			ChannelID:   post.ConversationID,
			ChannelType: m.bridgeChannelType(post.ConversationID),
			Files:       files,
			MessageID:   encodedID,
			ParentID:    parentID,
			CreateAt:    post.CreateAt,
		},
	})
}

func (m *MSTeams) formatSummary(text string, maxLen int) string {
	rc := m.cfg.Current()

	bq := "|"

	switch {
	case rc.MSTeams.Formatter.DisableMarkdownBlockQuote || rc.MSTeams.Formatter.DisableMarkdown:
		bq = utils.BlockQuoteCharDefault
	case rc.MSTeams.Formatter.MarkdownBlockQuoteChar != "":
		bq = rc.MSTeams.Formatter.MarkdownBlockQuoteChar
	case rc.MSTeams.Formatter.Unicode:
		bq = "▕"
	}

	return utils.FormatAndShortenSummary(text, utils.SummaryOpts{
		MaxLength:       maxLen,
		DisableMarkdown: rc.MSTeams.Formatter.DisableMarkdown,
		DisableEmoji:    rc.MSTeams.Formatter.DisableEmoji,
		BlockquoteChar:  bq,
		InlineCodeChar:  rc.MSTeams.Formatter.MarkdownInlineCode,
		CustomEmoji:     rc.MSTeams.Formatter.CustomEmoji,
		Unicode:         rc.MSTeams.Formatter.Unicode,
	})
}

func attachmentsToBridgeFiles(atts []msteamsclient.Attachment) []*bridge.File {
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

func teamsEmotionToIRC(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case emotionLike:
		return "thumbsup"
	case emotionHeart:
		return emotionHeart
	case emotionLaugh:
		return "laughing"
	case emotionSurprised:
		return "open_mouth"
	case emotionSad:
		return "cry"
	case emotionAngry:
		return emotionAngry
	case "hearteyes":
		return "heart_eyes"
	case "thumbsdown":
		return "-1"
	case "clap":
		return "clap"
	case "100":
		return "100"
	default:
		return key
	}
}

func ircEmojiToTeamsEmotion(emoji string, customEmoji map[string]string) string {
	clean := strings.Trim(strings.TrimSpace(emoji), ":")
	switch strings.ToLower(clean) {
	case "+1", "thumbsup", emotionLike:
		return emotionLike
	case emotionHeart, "red_heart":
		return emotionHeart
	case "laughing", emotionLaugh, "joy", "rofl":
		return emotionLaugh
	case "open_mouth", emotionSurprised, "astonished":
		return emotionSurprised
	case "cry", emotionSad, "sob":
		return emotionSad
	case emotionAngry, "rage":
		return emotionAngry
	default:
		if unicodeEmoji, ok := utils.EmojiFromAlias(clean, customEmoji); ok && unicodeEmoji != "" {
			return unicodeEmoji
		}

		return clean
	}
}

func (m *MSTeams) bridgeChannelType(convID string) string {
	conv := m.mc.GetChannel(convID)
	if conv == nil {
		return channelTypePublic
	}

	switch conv.Type {
	case msteamsclient.ConversationDM, msteamsclient.ConversationSelfNote:
		return channelTypeDirect
	case msteamsclient.ConversationGroup, msteamsclient.ConversationMeeting:
		return channelTypeGroup
	default:
		return channelTypePublic
	}
}

func (m *MSTeams) ensureConversationSynced(ctx context.Context, convID string) {
	if convID == "" || m.isDMConversation(convID) || m.mc.GetChannel(convID) != nil {
		return
	}

	err := m.UpdateChannels(ctx)
	if err != nil {
		logger.Debugf("ensureConversationSynced(%s) failed: %v", convID, err)
	}
}

func (m *MSTeams) isDMConversation(convID string) bool {
	if convID == "48:notes" || strings.HasSuffix(convID, "@unq.gbl.spaces") {
		return true
	}

	conv := m.mc.GetChannel(convID)
	if conv == nil {
		return false
	}

	return conv.Type == msteamsclient.ConversationDM || conv.Type == msteamsclient.ConversationSelfNote
}

func (m *MSTeams) dmReceiverFor(convID string, sender *bridge.UserInfo) *bridge.UserInfo {
	if !m.isDMConversation(convID) {
		return nil
	}

	me := m.GetMe()
	if me == nil {
		return nil
	}

	if sender != nil && sender.User != me.User {
		return me
	}

	otherMRI, ok := m.GetDMOtherUserID(convID, me.User)
	if !ok || otherMRI == "" {
		return me
	}

	if other := m.resolveUserInfo(m.ctx, otherMRI, ""); other != nil {
		return other
	}

	return me
}
