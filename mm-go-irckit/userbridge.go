package irckit

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mastodon"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/42wim/matterircd/bridge/slack"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	"github.com/davecgh/go-spew/spew"
	"github.com/sirupsen/logrus"
	"github.com/sorcix/irc"
)

const systemUser = "system"

type UserBridge struct {
	Srv         Server
	Credentials bridge.Credentials
	br          bridge.Bridger
	inprogress  atomic.Bool
	eventChan   chan *bridge.Event
	away        atomic.Bool

	eventLoopMutex   sync.Mutex
	eventLoopStarted bool

	lastViewedAtDB *bolt.DB

	lastEventTime atomic.Int64

	msgCounterMutex sync.RWMutex
	msgCounter      map[string]int

	msgLastMutex sync.RWMutex
	msgLast      map[string][2]string

	msgMapMutex      sync.RWMutex
	msgMap           map[string]map[string]int
	msgMapIndexMutex sync.RWMutex
	msgMapIndex      map[string]map[int]string

	requestedChannels atomic.Pointer[map[string]struct{}]

	updateCounterMutex sync.Mutex
	updateCounter      map[string]time.Time
}

func NewUserBridge(c net.Conn, srv Server, cfg *config.Config, db *bolt.DB) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})

	u.Srv = srv
	u.cfg = cfg
	u.lastViewedAtDB = db
	u.msgLast = make(map[string][2]string)
	u.msgMap = make(map[string]map[string]int)
	u.msgMapIndex = make(map[string]map[int]string)
	u.msgCounter = make(map[string]int)
	u.updateCounter = make(map[string]time.Time)
	u.eventChan = make(chan *bridge.Event, 1000)

	initialMap := make(map[string]struct{})
	u.requestedChannels.Store(&initialMap)

	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	u.createService("mastodon", "loginservice")
	u.createService("matterircd", "systemservice")
	return u
}

func (u *User) handleEventChan() {
	// Flush to disk every 2 minutes to prevent I/O bottlenecks
	flushTicker := time.NewTicker(2 * time.Minute)
	defer flushTicker.Stop()

	for {
		select {
		case event, ok := <-u.eventChan:
			if !ok {
				return
			}

			// Update our in-memory global timestamp on every event
			u.lastEventTime.Store(time.Now().UnixMilli())

			if logger.Level.String() == "trace" {
				logger.Tracef("eventchan %s", spew.Sdump(event))
			}

			// Process the event
			switch e := event.Data.(type) {
			case *bridge.BannerChangeEvent:
				u.handleBannerChangeEvent(e)
			case *bridge.ChannelMessageEvent:
				u.handleChannelMessageEvent(e)
			case *bridge.DirectMessageEvent:
				u.handleDirectMessageEvent(e)
			case *bridge.ChannelTopicEvent:
				u.handleChannelTopicEvent(e)
			case *bridge.FileEvent:
				u.handleFileEvent(e)
			case *bridge.ChannelAddEvent:
				u.handleChannelAddEvent(e)
			case *bridge.ChannelRemoveEvent:
				u.handleChannelRemoveEvent(e)
			case *bridge.ChannelCreateEvent:
				u.handleChannelCreateEvent(e)
			case *bridge.ChannelDeleteEvent:
				u.handleChannelDeleteEvent(e)
			case *bridge.ChannelUpdateEvent:
				u.handleChannelUpdateEvent(e)
			case *bridge.UserUpdateEvent:
				u.handleUserUpdateEvent(e)
			case *bridge.StatusChangeEvent:
				u.handleStatusChangeEvent(e)
			case *bridge.ReactionAddEvent, *bridge.ReactionRemoveEvent:
				u.handleReactionEvent(e)
			case *bridge.TypingEvent:
				u.handleTyping(e)
			case *bridge.LogoutEvent:
				// Flush immediately on explicit logout
				u.saveGlobalLastEventTime(u.lastEventTime.Load())

				return
			}

		case <-flushTicker.C:
			// Periodically flush the timestamp to BoltDB
			u.saveGlobalLastEventTime(u.lastEventTime.Load())
		}
	}
}

func (u *User) handleChannelTopicEvent(event *bridge.ChannelTopicEvent) {
	tu, ok := u.Srv.HasUserID(event.UserID)
	if event.UserID == u.User {
		ok = true
		tu = u
	}

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode

	if ok {
		ch := u.Srv.Channel(event.ChannelID)

		topic := utils.FormatMarkdownAndEmoji(
			event.Text,
			disableMarkdown,
			disableEmoji,
			blockQuoteCharDefault,
			inlineCode,
			customEmoji,
		)

		ch.Topic(tu, topic)

		u.saveLastViewedAt(event.ChannelID)

		return
	}

	logger.Errorf("topic change failure: userID %s not found", event.UserID)
}

const (
	blockQuoteCharDefault     = utils.BlockQuoteCharDefault
	blockQuoteCharNonUnicode  = "|"
	blockQuoteCharUnicode     = "🮇"
	codeBlockCharDefault      = ""
	codeBlockPrefixNonUnicode = "|"
	codeBlockPrefixUnicode    = "🮇"
)

func (u *User) getMarkdownBlockCodePrefix() (string, string) {
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	enableUnicode := u.br.FormatterConfig().Unicode

	// Block quotes
	var blockQuoteChar string
	if u.br.FormatterConfig().DisableMarkdownBlockQuote || disableMarkdown {
		blockQuoteChar = blockQuoteCharDefault
	} else if custom := u.br.FormatterConfig().MarkdownBlockQuoteChar; custom != "" {
		blockQuoteChar = custom
	} else if enableUnicode {
		blockQuoteChar = blockQuoteCharUnicode
	} else {
		blockQuoteChar = blockQuoteCharNonUnicode
	}

	// Code blocks
	var codeBlockPrefix string
	if u.br.FormatterConfig().DisableCodeBlockPrefix || disableMarkdown {
		codeBlockPrefix = codeBlockCharDefault
	} else if custom := u.br.FormatterConfig().CodeBlockPrefix; custom != "" {
		codeBlockPrefix = custom
	} else if enableUnicode {
		codeBlockPrefix = codeBlockPrefixUnicode
	} else {
		codeBlockPrefix = codeBlockPrefixNonUnicode
	}

	return blockQuoteChar, codeBlockPrefix
}

func (u *User) handleDirectMessageEvent(event *bridge.DirectMessageEvent) {
	if u.br.Protocol() == "mattermost" && u.cfg.Mattermost().ShowMentions {
		for _, m := range u.MentionKeys {
			if m == u.Nick || m == "" {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	var showContext bool
	var maxlen int

	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode

	text := event.Text
	prefix := ""
	suffix := ""
	if event.Event == "dm_topic" {
		showContext = false
		maxlen = 0
	} else {
		prefixUser := event.Sender.User
		if event.Sender.Me {
			prefixUser = event.Receiver.User
		}
		// Block quotes
		trimmedText := strings.TrimLeft(text, " \t")
		if !disableMarkdown && strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
			text = strings.Replace(text, blockQuoteCharDefault, blockQuoteChar, 1)
		}
		text, prefix, suffix, showContext, maxlen = u.handleMessageThreadContext(prefixUser, event.MessageID, event.ParentID, event.Event, text)
	}

	trimmedBlockQuoteDefault := strings.TrimSpace(blockQuoteCharDefault)
	trimmedBlockQuote := strings.TrimSpace(blockQuoteChar)
	trimmedCodeBlock := strings.TrimSpace(codeBlockPrefix)
	trimmedPrefix := strings.TrimSpace(prefix)
	trimmedSuffix := strings.TrimSpace(suffix)

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	prefixContext := u.br.BridgeConfig().PrefixContext
	showContextMulti := u.br.BridgeConfig().ShowContextMulti
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator

	text = utils.WrapMessage(text, maxlen)
	addPrefix := false

	// Remove message thread context prefix for formatting and remember to add it back
	if !addPrefix && prefixContext && !showContextMulti {
		switch {
		case prefix != "" && strings.HasPrefix(text, prefix):
			text = strings.TrimPrefix(text, prefix)
			addPrefix = true
		case trimmedPrefix != "" && strings.HasPrefix(text, trimmedPrefix+"\n"):
			text = strings.TrimPrefix(text, trimmedPrefix+"\n")
			addPrefix = true
		case trimmedPrefix != "" && text == trimmedPrefix:
			text = ""
			addPrefix = true
		}
	}

	// Strip trailing newlines/spaces from the entire message block first
	text = strings.TrimRight(text, " \t\r\n")

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
	}

	emitLine := func(line string) {
		if showContext || addPrefix {
			line = prefix + line + suffix
			addPrefix = false
		}

		if event.Sender.Me {
			if event.Receiver.Me {
				u.MsgSpoofUser(u, u.Nick, line, len(line))
			} else {
				// Outbound DM (e.g. from history replay)
				// Spoof as us, but target THEIR nick so it routes to their query window

				// FORCE ghost creation to bypass irckit's local-echo routing trap
				event.Sender.Me = false
				ghostMe := u.createUserFromInfo(event.Sender)
				event.Sender.Me = true
				u.MsgSpoofUser(ghostMe, event.Receiver.Nick, line, len(line))
			}
		} else {
			// Inbound DM
			// Spoof as them, target OUR nick
			u.MsgSpoofUser(u.createUserFromInfo(event.Sender), u.Nick, line, len(line))
		}
	}

	var (
		pendingEmpty []string
		hasEmitted   bool
	)

	utils.ProcessMessageText(text, opts, func(line string) {
		line = strings.TrimRight(line, " \t\r")
		trimmedLine := strings.TrimSpace(line)

		isEmpty := trimmedLine == "" || trimmedLine == trimmedPrefix || trimmedLine == trimmedSuffix ||
			trimmedLine == trimmedBlockQuote || trimmedLine == trimmedCodeBlock ||
			trimmedLine == trimmedBlockQuoteDefault

		if isEmpty {
			pendingEmpty = append(pendingEmpty, line)

			return
		}

		for _, pending := range pendingEmpty {
			emitLine(pending)
		}

		pendingEmpty = pendingEmpty[:0]

		emitLine(line)

		hasEmitted = true
	})

	// If the entire message consisted solely of markers (e.g. literal "|" or ">"), don't drop it completely
	if !hasEmitted && len(pendingEmpty) > 0 {
		for _, pending := range pendingEmpty {
			emitLine(pending)
		}
	}

	if u.br.Protocol() == "mattermost" && !u.cfg.Mattermost().DisableAutoView {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) handleChannelAddEvent(event *bridge.ChannelAddEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, added := range event.Added {
		if added.Me {
			u.syncChannel(event.ChannelID, u.br.GetChannelName(u.ctx, event.ChannelID))
			continue
		}

		ghost := u.createUserFromInfo(added)

		ch.Join(ghost)

		if event.Adder != nil && added.Nick != event.Adder.Nick && event.Adder.Nick != systemUser {
			ch.SpoofMessage(systemUser, "\x1dadded "+added.Nick+" to the channel by "+event.Adder.Nick+"\x1d")
		}
	}

	if u.br.Protocol() == "mattermost" && !u.cfg.Mattermost().DisableAutoView {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) handleChannelRemoveEvent(event *bridge.ChannelRemoveEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, removed := range event.Removed {
		if removed.Me {
			ch.Part(u, "")
			continue
		}

		ghost := u.createUserFromInfo(removed)

		ch.Part(ghost, "")

		if event.Remover != nil && removed.Nick != event.Remover.Nick && event.Remover.Nick != systemUser {
			ch.SpoofMessage(systemUser, "\x1dremoved "+removed.Nick+" from the channel by "+event.Remover.Nick+"\x1d")
		}
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) getMessageChannel(channelID string, sender *bridge.UserInfo) Channel {
	ch := u.Srv.Channel(channelID)
	ghost := u.createUserFromInfo(sender)

	// if it's another user, let them join
	if !ghost.Me && !ch.HasUser(ghost) {
		if u.br.Protocol() != "mastodon" {
			logger.Tracef("User %s is not in channel %s. Joining now", ghost.Nick, ch.String())
			ch.Join(ghost) //nolint:errcheck
		}
	}

	// check if we mayjoin this channel
	if u.mayJoin(channelID) {
		// if we are on it, just return it
		if ch.HasUser(u) {
			return ch
		}

		// otherwise first sync it
		u.syncChannel(channelID, u.br.GetChannelName(u.ctx, channelID))

		return ch
	}

	return u.Srv.Channel("&messages")
}

func (u *User) handleChannelMessageEvent(event *bridge.ChannelMessageEvent) {
	/*
		CHANNEL_OPEN                   = "O"
		CHANNEL_PRIVATE                = "P"
		CHANNEL_DIRECT                 = "D"
		CHANNEL_GROUP                  = "G"
	*/
	nick := sanitizeNick(event.Sender.Nick)
	logger.Tracef("in handleChannelMessageEvent from %s", nick)
	ch := u.getMessageChannel(event.ChannelID, event.Sender)
	if event.Sender.Me {
		nick = u.Nick
	}

	if event.ChannelType != "D" && ch.ID() == "&messages" {
		if u.br.BridgeConfig().ShowOnlyJoined {
			return
		}
		nick += "/" + u.Srv.Channel(event.ChannelID).String()
	}

	if u.br.Protocol() == "mattermost" && u.cfg.Mattermost().ShowMentions {
		for _, m := range u.MentionKeys {
			if m == u.Nick || m == "" {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode

	text := event.Text
	prefix := ""
	suffix := ""
	showContext := false
	maxlen := 460

	if u.Nick == systemUser || (event.Sender != nil && event.Sender.Nick == systemUser) {
		text = "\x1d" + text + "\x1d"
	} else {
		// Block quotes
		trimmedText := strings.TrimLeft(text, " \t")
		if !disableMarkdown && strings.HasPrefix(trimmedText, blockQuoteCharDefault) && blockQuoteChar != blockQuoteCharDefault {
			text = strings.Replace(text, blockQuoteCharDefault, blockQuoteChar, 1)
		}

		text, prefix, suffix, showContext, maxlen = u.handleMessageThreadContext(event.ChannelID, event.MessageID, event.ParentID, event.Event, text)
	}

	trimmedBlockQuoteDefault := strings.TrimSpace(blockQuoteCharDefault)
	trimmedBlockQuote := strings.TrimSpace(blockQuoteChar)
	trimmedCodeBlock := strings.TrimSpace(codeBlockPrefix)
	trimmedPrefix := strings.TrimSpace(prefix)
	trimmedSuffix := strings.TrimSpace(suffix)

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	prefixContext := u.br.BridgeConfig().PrefixContext
	showContextMulti := u.br.BridgeConfig().ShowContextMulti
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator

	text = utils.WrapMessage(text, maxlen)
	addPrefix := false

	// Remove message thread context prefix for formatting and remember to add it back
	if !addPrefix && prefixContext && !showContextMulti {
		switch {
		case prefix != "" && strings.HasPrefix(text, prefix):
			text = strings.TrimPrefix(text, prefix)
			addPrefix = true
		case trimmedPrefix != "" && strings.HasPrefix(text, trimmedPrefix+"\n"):
			text = strings.TrimPrefix(text, trimmedPrefix+"\n")
			addPrefix = true
		case trimmedPrefix != "" && text == trimmedPrefix:
			text = ""
			addPrefix = true
		}
	}

	// Strip trailing newlines/spaces from the entire message block first
	text = strings.TrimRight(text, " \t\r\n")

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
	}

	emitLine := func(line string) {
		if showContext || addPrefix {
			line = prefix + line + suffix
			addPrefix = false
		}

		switch event.MessageType {
		case "notice":
			ch.SpoofNotice(nick, line, len(line))
		default:
			ch.SpoofMessage(nick, line, len(line))
		}
	}

	var (
		pendingEmpty []string
		hasEmitted   bool
	)

	utils.ProcessMessageText(text, opts, func(line string) {
		line = strings.TrimRight(line, " \t\r")
		trimmedLine := strings.TrimSpace(line)

		isEmpty := trimmedLine == "" || trimmedLine == trimmedPrefix || trimmedLine == trimmedSuffix ||
			trimmedLine == trimmedBlockQuote || trimmedLine == trimmedCodeBlock ||
			trimmedLine == trimmedBlockQuoteDefault

		if isEmpty {
			pendingEmpty = append(pendingEmpty, line)

			return
		}

		for _, pending := range pendingEmpty {
			emitLine(pending)
		}

		pendingEmpty = pendingEmpty[:0]

		emitLine(line)

		hasEmitted = true
	})

	// If the entire message consisted solely of markers (e.g. literal "|" or ">"), don't drop it completely
	if !hasEmitted && len(pendingEmpty) > 0 {
		for _, pending := range pendingEmpty {
			emitLine(pending)
		}
	}

	if u.br.Protocol() == "mattermost" && !u.cfg.Mattermost().DisableAutoView {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	if u.br.BridgeConfig().ShowOnlyJoined && event.ChannelType != "D" {
		ch := u.getMessageChannel(event.ChannelID, event.Sender)
		if ch.ID() == "&messages" {
			return
		}
	}

	for _, f := range event.Files {
		sizeKB := f.Size / 1024
		if sizeKB == 0 && f.Size > 0 {
			sizeKB = 1
		}

		fileMsg := fmt.Sprintf("\x1ddownload file - %s (%s, %dKBytes)\x1d", f.URL, f.Name, sizeKB)
		if u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext {
			threadMsgID := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, "posted_file")
			fileMsg = u.formatContextMessage("", threadMsgID, fileMsg)
		}

		switch event.ChannelType {
		case "D":
			if event.Sender.Me {
				if event.Receiver.Me {
					u.MsgSpoofUser(u, u.Nick, fileMsg)
				} else {
					event.Sender.Me = false
					ghostMe := u.createUserFromInfo(event.Sender)
					event.Sender.Me = true
					u.MsgSpoofUser(ghostMe, event.Receiver.Nick, fileMsg)
				}
			} else {
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), u.Nick, fileMsg)
			}
		default:
			ch := u.getMessageChannel(event.ChannelID, event.Sender)
			if event.Sender.Me {
				ch.SpoofMessage(u.Nick, fileMsg)
			} else {
				ch.SpoofMessage(event.Sender.Nick, fileMsg)
			}
		}
	}
}

func (u *User) handleChannelCreateEvent(event *bridge.ChannelCreateEvent) {
	logger.Debugf("ACTION_CHANNEL_CREATED syncing channel %s (%s)", u.br.GetChannelName(u.ctx, event.ChannelID), event.ChannelID)

	u.syncChannel(event.ChannelID, u.br.GetChannelName(u.ctx, event.ChannelID))
}

func (u *User) handleChannelDeleteEvent(event *bridge.ChannelDeleteEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	if ch.HasUser(u) {
		logger.Debugf("ACTION_CHANNEL_DELETED removing myself from %s (%s)", u.br.GetChannelName(u.ctx, event.ChannelID), event.ChannelID)
		ch.Part(u, "")
	} else {
		logger.Debugf("ACTION_CHANNEL_DELETED not in channel %s (%s)", u.br.GetChannelName(u.ctx, event.ChannelID), event.ChannelID)
	}
}

//nolint:funlen
func (u *User) handleChannelUpdateEvent(e *bridge.ChannelUpdateEvent) {
	newIRCChanStr := "#" + e.Name

	// Snapshot the joined channels so we don't hold the lock during API calls
	u.RLock()

	channelsToCheck := make([]Channel, 0, len(u.channels))
	for ch := range u.channels {
		channelsToCheck = append(channelsToCheck, ch)
	}

	u.RUnlock()

	var staleChannels []Channel

	alreadyJoinedNew := false

	// Identify the stale channels using proper ch.String() calls
	for _, ch := range channelsToCheck {
		// Skip special matterircd system channels (they don't exist on Mattermost)
		if strings.HasPrefix(ch.String(), "&") {
			continue
		}

		if ch.String() == newIRCChanStr {
			alreadyJoinedNew = true
			continue
		}

		chName := strings.TrimPrefix(ch.String(), "#")
		chID := u.br.GetChannelID(u.ctx, chName, u.br.GetMe().TeamID)

		// A channel is stale if Mattermost 404s it (returns ""),
		// OR if the cache still links this old name to the ID that was just renamed!
		if chID == "" || chID == e.ChannelID {
			staleChannels = append(staleChannels, ch)
		}
	}

	// Just a display name change? exit.
	if alreadyJoinedNew && len(staleChannels) == 0 {
		return
	}

	// Part the stale channels, purge them from our routing map, AND Unlink them!
	// We MUST do this before joining the new one, so the IRC server forgets the old object completely.
	for _, ch := range staleChannels {
		ch.Part(u, "Channel renamed on server")

		u.Lock()
		delete(u.channels, ch)
		u.Unlock()

		// Nuke the channel from the IRC server's global registry so it cannot be reused or aliased
		ch.Unlink()
	}

	// Join the new channel and bind it to our routing map
	if !alreadyJoinedNew {
		newCh := u.Srv.Channel(newIRCChanStr)
		_ = newCh.Join(u)

		u.Lock()
		if u.channels == nil {
			u.channels = make(map[Channel]struct{})
		}

		u.channels[newCh] = struct{}{}
		u.Unlock()

		noticeMsg := &irc.Message{
			Prefix:   u.Srv.Prefix(),
			Command:  irc.NOTICE,
			Params:   []string{newIRCChanStr},
			Trailing: fmt.Sprintf("[SERVER NOTICE] You have been joined here because an existing channel was renamed to %s (Display Name: %s).", newIRCChanStr, e.DisplayName),
		}
		_ = u.Encode(noticeMsg)
	}
}

func (u *User) handleUserUpdateEvent(event *bridge.UserUpdateEvent) {
	u.updateUserFromInfo(event.User)
}

func (u *User) handleStatusChangeEvent(event *bridge.StatusChangeEvent) {
	if event.UserID == u.br.GetMe().User {
		switch event.Status {
		case "online":
			if u.away.Load() {
				logger.Debug("setting myself online")
				u.away.Store(false)
				u.Srv.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away") //nolint:errcheck
			}
		// Ignore `offline` status changes to prevent bouncing between being marked away and not.
		case "offline":
			logger.Debugf("doing nothing as status %s", event.Status)
		default:
			if !u.away.Load() {
				logger.Debug("setting myself away")
				u.away.Store(true)
				u.Srv.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away") //nolint:errcheck
			}
		}
	}
}

func (u *User) handleReactionEvent(event interface{}) {
	var (
		text, channelID, messageID, parentID, channelType, reaction string
		receiver, sender                                            *bridge.UserInfo
	)

	message := ""

	switch e := event.(type) {
	case *bridge.ReactionAddEvent:
		message = e.Message
		text = "added reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		receiver = e.Receiver
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
		parentID = e.ParentID
	case *bridge.ReactionRemoveEvent:
		message = e.Message
		text = "removed reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		receiver = e.Receiver
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
		parentID = e.ParentID
	}

	defer u.saveLastViewedAt(channelID)

	if u.br.Protocol() == "mattermost" && u.cfg.Mattermost().HideReactions {
		logger.Debug("Not showing reaction: " + text + reaction)
		return
	}

	if !u.br.FormatterConfig().DisableEmoji {
		if reactionEmoji, ok := utils.EmojiFromAlias(reaction, u.br.FormatterConfig().CustomEmoji); ok {
			reaction = reactionEmoji
		}
	}

	if channelType == "D" {
		e := &bridge.DirectMessageEvent{
			Text:      "\x1d" + text + reaction + "\x1d" + message,
			ChannelID: channelID,
			Receiver:  receiver,
			Sender:    sender,
			MessageID: messageID,
			Event:     "reaction",
			ParentID:  parentID,
		}

		u.handleDirectMessageEvent(e)
		return
	}

	e := &bridge.ChannelMessageEvent{
		Text:        "\x1d" + text + reaction + "\x1d" + message,
		ChannelID:   channelID,
		ChannelType: channelType,
		Sender:      sender,
		MessageID:   messageID,
		Event:       "reaction",
		ParentID:    parentID,
	}

	u.handleChannelMessageEvent(e)
}

func (u *User) CreateUserFromInfo(info *bridge.UserInfo) *User {
	return u.createUserFromInfo(info)
}

func (u *User) CreateUsersFromInfo(info []*bridge.UserInfo) []*User {
	users := make([]*User, 0, len(info))

	for _, userinfo := range info {
		if userinfo.Me {
			continue
		}

		// Force Ghost flag so the server knows this is not a real user,
		// allowing channels to safely close when real users leave.
		userinfo.Ghost = true

		if ghost, ok := u.Srv.HasUserID(userinfo.User); ok {
			// Do not overwrite existing ghost.UserInfo!
			// Existing ghosts are already functional. Overwriting them
			// risks injecting pointers from temporary connections.
			users = append(users, ghost)
			continue
		}

		ghost := NewUser(nil)
		ghost.UserInfo = userinfo
		nick := ghost.UserInfo.Nick
		if nick == "" {
			nick = ghost.UserInfo.Username
		}
		ghost.Nick = sanitizeNick(nick)

		u.Srv.Add(ghost)

		users = append(users, ghost)
	}

	return users
}

func (u *User) updateUserFromInfo(info *bridge.UserInfo) *User {
	info.Ghost = true

	if ghost, ok := u.Srv.HasUserID(info.User); ok {
		if ghost.Nick != info.Nick {
			changeMsg := &irc.Message{
				Prefix:  ghost.Prefix(),
				Command: irc.NICK,
				Params:  []string{info.Nick},
			}
			u.Encode(changeMsg)
		}
		// Do not overwrite existing ghost.UserInfo
		return ghost
	}

	ghost := NewUser(nil)
	ghost.UserInfo = info

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) createUserFromInfo(info *bridge.UserInfo) *User {
	info.Ghost = true

	if ghost, ok := u.Srv.HasUserID(info.User); ok {
		return ghost
	}

	// Use nil to avoid anchoring the TCP connection
	ghost := NewUser(nil)
	ghost.UserInfo = info
	ghost.Nick = sanitizeNick(ghost.Nick)

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) addUsersToChannel(users []*User, channel string, channelID string) {
	logger.Tracef("adding %d to %s", len(users), channel)

	ch := u.Srv.Channel(channelID)

	ch.BatchJoin(users)
}

func (u *User) addUsersToChannels() {
	time.Sleep(time.Millisecond * 500)
	// wait until the bridge is ready
	for u.br == nil {
		logger.Debug("bridge not ready yet, sleeping")
		time.Sleep(time.Millisecond * 500)
	}

	syncStartTime := time.Now()
	syncThresh := u.cfg.Mattermost().HeavySyncThreshold

	if syncThresh == 0 {
		syncThresh = 15 * time.Minute
	}
	u.eventLoopMutex.Lock()
	lastSyncThreshold := u.lastSync
	isHeavySync := !u.eventLoopStarted || time.Since(u.lastSync) > syncThresh
	u.eventLoopMutex.Unlock()

	ellipsis := "..."
	if u.br.FormatterConfig().Unicode {
		ellipsis = "…"
	}

	// Announce if this is initial login OR a long-offline reconnect
	if isHeavySync {
		if svc, ok := u.Srv.HasUser(u.br.Protocol()); ok {
			logger.Info("starting channel synchronization and history replays")
			u.MsgUser(svc, fmt.Sprintf("starting channel synchronization and history replays%s (this could be a while)%s", ellipsis, ellipsis))
		}
	}

	globalSince := u.getGlobalLastEventTime()
	if globalSince > 0 {
		logger.Infof("Loaded global last event timestamp: %s", time.UnixMilli(globalSince).Format("2006-01-02 15:04:05"))
	}

	srv := u.Srv
	throttle := time.NewTicker(time.Millisecond * 8)

	logger.Trace("in addUsersToChannels")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")

	// create and join the users
	users := u.CreateUsersFromInfo(u.br.GetUsers())
	if len(users) > 0 {
		srv.BatchAdd(users)
		u.addUsersToChannel(users, "&users", "&users")

		// join ourself
		ch.Join(u) //nolint:errcheck
	}

	if u.br.Protocol() == "mastodon" {
		ch = srv.Channel("mastodon")
		ch.Join(u) //nolint:errcheck
	}

	// channel that receives messages from channels not joined on irc
	ch = srv.Channel("&messages")
	ch.Join(u)

	// Fetch, filter, and sort the channels alphabetically
	var joinChannels []*bridge.ChannelInfo
	for _, brchannel := range u.br.GetChannels() {
		lastPost := time.UnixMilli(brchannel.LastPostAt)
		if brchannel.DM && !u.br.BridgeConfig().JoinDM {
			// Skip channels with no activity or older than 1 year (~365 days)
			if brchannel.LastPostAt == 0 || time.Since(lastPost) > 365*24*time.Hour {
				continue
			}

			threshold := lastSyncThreshold
			if threshold.IsZero() {
				dmThresh := u.cfg.Mattermost().DefaultDMOfflineThreshold
				if dmThresh == 0 {
					dmThresh = 24 * time.Hour
				}

				threshold = time.Now().Add(-dmThresh)
			}

			formatChannelDesc := func() string {
				if mUser := u.br.GetDMUser(u.ctx, brchannel.Name); mUser != nil {
					return fmt.Sprintf("DM with %s (%s)", mUser.Nick, mUser.User)
				}

				if u.br.IsDMChannelName(brchannel.Name) {
					if otherUserID, ok := u.br.GetDMOtherUserID(brchannel.Name, u.User); ok {
						return fmt.Sprintf("DM with UNKNOWN (%s)", otherUserID)
					}
				}

				if brchannel.DisplayName != "" {
					return fmt.Sprintf("GM with %s (%s)", brchannel.DisplayName, brchannel.Name)
				}

				return brchannel.Name
			}

			// If the channel has been dormant since before our threshold, safely skip it
			if lastPost.Before(threshold) {
				if logger.Logger.IsLevelEnabled(logrus.TraceLevel) {
					logger.Tracef("Skipping dormant %s (LastPost: %v, Threshold: %v)", formatChannelDesc(), lastPost, threshold)
				} else {
					logger.Debugf("Skipping dormant %s (LastPost: %v)", formatChannelDesc(), lastPost)
				}

				continue
			}

			if logger.Logger.IsLevelEnabled(logrus.TraceLevel) {
				logger.Tracef("SmartJoin: Joining %s due to recent offline activity (LastPost: %v, Threshold: %v)", formatChannelDesc(), lastPost, threshold)
			} else {
				logger.Debugf("SmartJoin: Joining %s due to recent offline activity (LastPost: %v)", formatChannelDesc(), lastPost)
			}
		}

		// If the channel is explicitly excluded in matterircd.toml, drop it now.
		if !brchannel.DM && !u.mayJoin(brchannel.ID) {
			logger.Debugf("Skipping channel %s due to JoinExclude/JoinOnly configuration (LastPost: %v)", brchannel.Name, lastPost)

			continue
		}

		joinChannels = append(joinChannels, brchannel)
	}

	sort.Slice(joinChannels, func(i, j int) bool {
		return strings.ToLower(joinChannels[i].Name) < strings.ToLower(joinChannels[j].Name)
	})

	channels := make(chan *bridge.ChannelInfo, len(joinChannels))
	var wg sync.WaitGroup

	// Use 4 workers instead of the previous 10 to preserve alphabetical pacing
	for i := range 4 {
		// Extract the current prefix (e.g., "matterircd"
		currentPrefix := ""
		if p, ok := logger.Data["prefix"].(string); ok {
			currentPrefix = p + ": "
		}

		// Create a new logger instance for this specific worker
		workerLogger := logger.WithField("prefix", fmt.Sprintf("%saddUserToChannelWorker%d", currentPrefix, i))

		wg.Add(1)
		go func() {
			defer wg.Done()
			u.addUserToChannelWorker(channels, throttle, workerLogger)
		}()
	}

	// Feed the sorted channels into the queue
	for _, brchannel := range joinChannels {
		logger.Debugf("Adding channel %#v", brchannel)
		channels <- brchannel
	}

	close(channels)

	// clean up tickers/timers after workers have finished
	go func() {
		wg.Wait()
		throttle.Stop()

		// Now that every worker has successfully finished syncing, move the watermark forward.
		u.eventLoopMutex.Lock()
		if syncStartTime.After(u.lastSync) {
			u.lastSync = syncStartTime
		}
		u.eventLoopMutex.Unlock()

		// Only announce completion if it was a heavy sync and wasn't aborted
		if isHeavySync && u.ctx != nil && u.ctx.Err() == nil {
			if svc, ok := u.Srv.HasUser(u.br.Protocol()); ok {
				duration := time.Since(syncStartTime).Round(time.Second)
				logger.Infof("channel synchronization completed and history replayed (took %s)", duration)
				u.MsgUser(svc, fmt.Sprintf("channel synchronization completed and history replayed (took %s).", duration))
			}
		}
	}()

	// Prevent leaking goroutines on internal bridge reconnects
	// by ensuring we only launch one event loop per session.
	u.eventLoopMutex.Lock()
	if !u.eventLoopStarted {
		u.eventLoopStarted = true
		// we did all the initialization, now listen for events
		go u.handleEventChan()
	}
	u.eventLoopMutex.Unlock()
}

func (u *User) createSpoof(mmchannel *bridge.ChannelInfo) func(string, string, ...int) {
	if !u.br.IsDMChannelName(mmchannel.Name) {
		return u.Srv.Channel(mmchannel.ID).SpoofMessage
	}

	var otherNick string

	// Resolve the other participant's IRC nick for DMs
	if dmUser := u.br.GetDMUser(u.ctx, mmchannel.Name); dmUser != nil {
		otherNick = dmUser.Nick
	}

	spoofDM := func(nick string, msg string, maxlen ...int) {
		// Outbound DM: Bypass local-echo trap and route to their query window
		if nick == u.Nick && otherNick != "" {
			_ = u.Encode(&irc.Message{
				Prefix:  u.Prefix(),
				Command: irc.PRIVMSG,
				Params:  []string{otherNick, msg},
			})

			return
		}

		// Inbound DM (or fallback if the user wasn't resolved)
		usr, ok := u.Srv.HasUser(nick)
		if !ok {
			logger.Errorf("%s not found for replay msg", nick)

			return
		}

		u.MsgSpoofUser(usr, u.Nick, msg)
	}

	return spoofDM
}

// getChannelSince calculates the 'since' timestamp for a channel based on the configured strategy.
//nolint:funlen,gocyclo
func (u *User) getChannelSince(ctx context.Context, brchannel *bridge.ChannelInfo, replayCutoff int64) (int64, string, bool) {
	// Fetch server-side last viewed if strategy allows
	strategy := u.cfg.Mattermost().ReplayStrategy
	if strategy == "" {
		strategy = "hybrid" //nolint:goconst
	}

	// Support alias normalizations
	switch strategy {
	case "saved+server", "server+saved", "stored+server", "server+stored":
		strategy = "hybrid"
	case "stored": //nolint:goconst
		strategy = "saved" //nolint:goconst
	}

	var serverSince int64
	if strategy != "saved" {
		serverSince = u.br.GetLastViewedAt(ctx, brchannel.ID)
	}

	// If strategy is server-only, return immediately
	if strategy == "server-only" {
		if serverSince == 0 {
			if brchannel.LastPostAt > replayCutoff {
				return brchannel.LastPostAt, "lastpost-fallback", false
			}

			return replayCutoff, "cutoff-fallback", false
		}

		if replayCutoff > 0 && serverSince < replayCutoff {
			return replayCutoff, "server-clamped", false
		}

		return serverSince, "server", false
	}

	var savedSince int64
	var inDB bool

	// Fetch locally saved BoltDB last viewed if strategy allows
	if strategy == "saved" || strategy == "hybrid" {
		_ = u.lastViewedAtDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(u.User))
			if b == nil {
				return nil
			}

			v := b.Get([]byte(brchannel.ID))
			if v != nil {
				val := binary.LittleEndian.Uint64(v)
				if val <= math.MaxInt64 {
					savedSince = int64(val)
					inDB = true
				}
			}

			return nil
		})
	}

	bestSince := serverSince
	source := "server"

	if strategy == "saved" || savedSince > bestSince {
		bestSince = savedSince
		source = "stored"
	}

	// If both server and BoltDB are 0 (or we don't have it in DB for a DM)
	isDM := u.br.IsDMChannelName(brchannel.Name)

	if strategy == "saved" && !inDB {
		return replayCutoff, "cutoff-fallback", false
	}

	if bestSince == 0 || (isDM && !inDB) {
		// If Mattermost gave us a valid LastPostAt, use it (if it's newer than the cutoff)
		if brchannel.LastPostAt > 0 {
			if replayCutoff > 0 && brchannel.LastPostAt < replayCutoff {
				return replayCutoff, "cutoff-fallback", inDB
			}

			return brchannel.LastPostAt, "lastpost-fallback", inDB
		}

		// If Mattermost failed, use our Global Last Event Timestamp
		globalSince := u.getGlobalLastEventTime()
		if globalSince > 0 {
			if replayCutoff > 0 && globalSince < replayCutoff {
				return replayCutoff, "cutoff-fallback", inDB
			}

			return globalSince, "global-event-fallback", inDB
		}

		// Only if it's the very first time booting up
		return replayCutoff, "cutoff-fallback", inDB
	}

	if replayCutoff > 0 && bestSince < replayCutoff {
		return replayCutoff, source + "-clamped", inDB
	}

	return bestSince, source, inDB
}

//nolint:funlen,cyclop,gocognit,gocyclo
func (u *User) addUserToChannelWorker(channels <-chan *bridge.ChannelInfo, throttle *time.Ticker, logger *logrus.Entry) {
	strategy := u.cfg.Mattermost().ReplayStrategy
	if strategy == "" {
		strategy = "hybrid"
	}

	replayDuration := u.cfg.Mattermost().MaxReplayDuration
	if replayDuration == 0 {
		replayDuration = 31 * 24 * time.Hour
	}

	replayCutoff := time.Now().Add(-replayDuration).UnixMilli()

	// Currently only supported and tested with Mattermost
	lazyJoin := u.br.Protocol() == "mattermost" && u.cfg.Mattermost().EnableLazyJoin

	lazyJoinDuration := u.cfg.Mattermost().LazyJoinDuration
	if lazyJoinDuration == 0 {
		lazyJoinDuration = 21 * 24 * time.Hour
	}

	lazyJoinCutoff := time.Now().Add(-lazyJoinDuration).UnixMilli()

	for {
		select {
		case <-u.ctx.Done():
			logger.Debug("addUserToChannelWorker aborted via context before fetching channel")
			return
		case brchannel, ok := <-channels:
			if !ok {
				// The channel was closed, worker is done draining the queue
				return
			}

			logger.Debugf("addUserToChannelWorker %s (using %s)", brchannel.Name, strategy)

			// Interruptible throttle wait
			select {
			case <-u.ctx.Done():
				logger.Debug("addUserToChannelWorker aborted via context during throttle")
				return
			case <-throttle.C:
			}

			since, sinceStr, inDB := u.getChannelSince(u.ctx, brchannel, replayCutoff)
			isDM := u.br.IsDMChannelName(brchannel.Name)

			// Determine if this channel is explicitly whitelisted to bypass LazyJoin
			channelName := brchannel.Name
			if brchannel.TeamID != u.br.GetMe().TeamID || (u.br.Protocol() == "mattermost" && u.cfg.Mattermost().PrefixMainTeam) {
				channelName = u.br.GetTeamName(u.ctx, brchannel.TeamID) + "/" + brchannel.Name
			}

			lje := u.cfg.Mattermost().LazyJoinExclude
			isConfigWhitelisted := len(lje) > 0 && (stringInRegexp(channelName, lje) || stringInRegexp("#"+channelName, lje))

			isRequested := u.isRequestedChannel(channelName)
			if !isRequested && channelName != brchannel.Name {
				isRequested = u.isRequestedChannel(brchannel.Name)
			}

			isWhitelisted := isConfigWhitelisted || isRequested
			if lazyJoin && isWhitelisted {
				logger.Debugf("Whitelisted channel from LazyJoin: %s", channelName)
			}

			// Dormancy Filter for Public Channels
			// DMs bypass this because they are already strictly filtered by
			// DefaultDMOfflineThreshold upstream in addUsersToChannels().
			// Explicitly whitelisted channels bypass this!
			if lazyJoin && !isDM && !isWhitelisted {
				// Strategy "saved": require prior presence in local BoltDB
				if strategy == "saved" && !inDB {
					continue
				}

				// Strategies "server" & "hybrid": consult server channel state and skip deleted channels
				if (strategy == "server" || strategy == "hybrid") && brchannel.DeleteAt > 0 {
					continue
				}

				u.eventLoopMutex.Lock()
				syncTime := u.lastSync
				u.eventLoopMutex.Unlock()

				var cutoff int64
				if syncTime.IsZero() {
					// Fresh boot: Use the lazy join window to catch recent history
					cutoff = lazyJoinCutoff
				} else {
					// Dynamic Reconnect: Use the exact time you dropped off
					cutoff = syncTime.UnixMilli()
				}

				// Evaluate dormancy using the ACTUAL Mattermost server activity
				if brchannel.LastPostAt < cutoff {
					logger.Debugf("Smart Lazy-join: Skipping dormant public channel %s (LastPost: %v)", brchannel.Name, time.UnixMilli(brchannel.LastPostAt).Format("2006-01-02 15:04:05"))
					continue
				}
			}

			// Replay Window Cap (Applies to ALL channels)
			if replayCutoff > 0 && since > 0 && since < replayCutoff {
				logger.Infof("Capping replay history for %s to %s (original since: %s)",
					brchannel.Name,
					replayDuration,
					time.UnixMilli(since).Format("2006-01-02"),
				)
				since = replayCutoff
				sinceStr = "replay-cutoff"
			}

			var syncErr error

			success := true
			if !isDM {
				// Give the bridge up to 3 attempts to fetch the heavy nicklist
				success = false

				for attempts := 1; attempts <= 3; attempts++ {
					syncErr = u.syncChannel(brchannel.ID, "#"+channelName)
					if syncErr == nil {
						success = true

						break
					}

					// Fast exit on disconnect / shutdown
					if errors.Is(syncErr, context.Canceled) || u.ctx.Err() != nil {
						return
					}

					logger.Warnf("Failed to sync channel %s (attempt %d/3): %v", brchannel.Name, attempts, syncErr)

					// Wait 5 seconds before retrying, but abort if the bridge is shutting down
					select {
					case <-u.ctx.Done():
						return
					case <-time.After(time.Second * 5):
					}
				}
			}

			// THE SHIELD: If it failed all 3 attempts, abort!
			// Do NOT replay history invisibly, and do NOT update the BoltDB.
			if !success {
				logger.Errorf("Giving up on syncing %s. Skipping replay to PRESERVE history for next startup.", brchannel.Name)
				continue
			}

			u.replayHistory(brchannel, since, sinceStr)

			if u.br.Protocol() == "mattermost" && !u.cfg.Mattermost().DisableAutoView {
				u.updateLastViewed(brchannel.ID)
			}
			u.saveLastViewedAt(brchannel.ID)
		}
	}
}

// replayHistory handles the actual fetching and spoofing of historical channel messages.
//
//nolint:funlen,gocognit,gocyclo
func (u *User) replayHistory(brchannel *bridge.ChannelInfo, since int64, logSince string) {
	if since == 0 {
		return
	}

	spoof := u.createSpoof(brchannel)
	channame := brchannel.Name
	if !brchannel.DM {
		channame = "#" + brchannel.Name
	}

	// Post everything to the channel we haven't seen yet
	events := u.br.GetReplayEvents(u.ctx, brchannel.ID, since)
	if len(events) == 0 {
		// If the channel is not from the primary team id, we can't get posts
		if events == nil && brchannel.TeamID == u.br.GetMe().TeamID {
			logger.Errorf("something wrong with GetReplayEvents for %s for channel %s (%s)", u.Nick, channame, brchannel.ID)
		}
		return
	}

	showReplayHdr := true

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
	}

	for _, event := range events {
		var createAt int64
		var text, nick, msgID, parentID string
		var files []*bridge.File

		// Extract attributes dynamically based on the event payload type
		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			createAt, text, nick, msgID, parentID, files = e.CreateAt, e.Text, e.Sender.Nick, e.MessageID, e.ParentID, e.Files
		case *bridge.DirectMessageEvent:
			createAt, text, nick, msgID, parentID, files = e.CreateAt, e.Text, e.Sender.Nick, e.MessageID, e.ParentID, e.Files
		case *bridge.ChannelAddEvent:
			createAt, text, nick = e.CreateAt, e.Text, systemUser
			if len(e.Added) > 0 {
				ghost := u.createUserFromInfo(e.Added[0])
				u.Srv.Channel(brchannel.ID).Join(ghost) //nolint:errcheck
			}
		case *bridge.ChannelRemoveEvent:
			createAt, text, nick = e.CreateAt, e.Text, systemUser
			if len(e.Removed) > 0 {
				ghost := u.createUserFromInfo(e.Removed[0])
				u.Srv.Channel(brchannel.ID).Part(ghost, "")
			}
		case *bridge.ChannelTopicEvent:
			u.handleChannelTopicEvent(e)
		default:
			continue
		}

		ts := time.Unix(0, createAt*int64(time.Millisecond))
		tsStr := ts.Format("2006-01-02 15:04")

		// Print the replay header on the very first valid event we process
		if showReplayHdr && (text != "" || len(files) > 0) {
			dateStr := ts.Format("2006-01-02 15:04:05")
			target := "matterircd"
			if brchannel.DM {
				target = nick
			}
			spoof(target, fmt.Sprintf("\x02Replaying msgs since %s\x02 \x1d(%s)\x1d", dateStr, logSince))
			logger.Infof("Replaying msgs for %s (%s) since %s (%s)", channame, brchannel.ID, dateStr, logSince)
			showReplayHdr = false
		}

		textToProcess := utils.WrapMessage(text, 460)

		utils.ProcessMessageText(textToProcess, opts, func(line string) {
			if line != "" {
				if nick == systemUser {
					line = "\x1d" + line + "\x1d"
				}

				// Safely unwrap CTCP actions
				isAction := strings.HasPrefix(line, "\x01ACTION ") && strings.HasSuffix(line, "\x01")
				if isAction {
					line = line[8 : len(line)-1]
				}

				replayMsg := fmt.Sprintf("[%s] %s", tsStr, line)
				if (u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext) && nick != systemUser {
					threadMsgID := u.prefixContext(brchannel.ID, msgID, parentID, "replay")
					replayMsg = u.formatContextMessage(tsStr, threadMsgID, line)
				}

				// Re-wrap the entire payload
				if isAction {
					replayMsg = "\x01ACTION " + replayMsg + "\x01"
				}

				spoof(nick, replayMsg)
			}
		})

		for _, f := range files {
			sizeKB := f.Size / 1024
			if sizeKB == 0 && f.Size > 0 {
				sizeKB = 1
			}

			fileMsg := fmt.Sprintf("\x1ddownload file - %s (%s, %dKBytes)\x1d", f.URL, f.Name, sizeKB)
			if u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext {
				threadMsgID := u.prefixContext(brchannel.ID, msgID, parentID, "replay_file")
				fileMsg = u.formatContextMessage(tsStr, threadMsgID, fileMsg)
			}

			spoof(nick, fileMsg)
		}
	}
}

func (u *User) MsgUser(toUser *User, msg string) {
	u.Encode(&irc.Message{
		Prefix:        toUser.Prefix(),
		Command:       irc.PRIVMSG,
		Params:        []string{u.Nick},
		Trailing:      msg,
		EmptyTrailing: true,
	})
}

func (u *User) MsgSpoofUser(sender *User, rcvuser string, text string, maxlen ...int) {
	if len(maxlen) == 0 {
		text = utils.WrapMessage(text, 460)
	} else {
		text = utils.WrapMessage(text, maxlen[0])
	}

	prefix := irc.Prefix{
		Name: sender.Nick,
		User: sender.Nick,
		Host: sender.Host,
	}
	msg := irc.Message{
		Prefix:        &prefix,
		Command:       irc.PRIVMSG,
		Params:        []string{rcvuser},
		EmptyTrailing: true,
	}

	for {
		line, rest, found := strings.Cut(text, "\n")
		line = strings.TrimSuffix(line, "\r")
		msg.Trailing = line

		u.Encode(&msg) //nolint:errcheck

		if !found {
			break
		}
		text = rest
	}
}

func (u *User) syncChannel(id string, name string) error {
	if u.ctx.Err() != nil {
		return u.ctx.Err()
	}

	if u.br == nil {
		return context.Canceled
	}

	users, err := u.br.GetChannelUsers(u.ctx, id)
	if err != nil {
		logger.Error(err)

		return err
	}

	srv := u.Srv

	// create and join the users
	batchUsers := u.CreateUsersFromInfo(users)
	srv.BatchAdd(batchUsers)
	u.addUsersToChannel(batchUsers, "&users", "&users")
	u.addUsersToChannel(batchUsers, name, id)

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode

	// add myself ONLY if I am actually a member
	ch := srv.Channel(id)
	if u.br.IsChannelMember(id) && !ch.HasUser(u) && u.mayJoin(id) {
		logger.Tracef("syncChannel adding myself to %s (id: %s)", name, id)
		ch.Join(u)
		svc, _ := srv.HasUser(u.br.Protocol())

		rawTopic := u.br.Topic(u.ctx, ch.ID())
		topic := utils.FormatMarkdownAndEmoji(
			rawTopic,
			disableMarkdown,
			disableEmoji,
			blockQuoteCharDefault,
			inlineCode,
			customEmoji,
		)

		ch.Topic(svc, topic)
	}

	return nil
}

func (u *User) mayJoin(channelID string) bool {
	ch := u.Srv.Channel(channelID)

	jo := u.br.BridgeConfig().JoinOnly
	ji := u.br.BridgeConfig().JoinInclude
	je := u.br.BridgeConfig().JoinExclude

	// If no join restrictions are configured, allow instantly
	if len(jo) == 0 && len(ji) == 0 && len(je) == 0 {
		return true
	}

	switch {
	// If we have joinonly channels specified and it matches, join
	case len(jo) != 0 && stringInRegexp(ch.String(), jo):
		logger.Tracef("mayjoin 0 %t ch: %s, match: %s", true, ch.String(), jo)
		return true
	// if we have joinonly channels specified we are only allowed to join those
	case len(jo) != 0 && !stringInRegexp(ch.String(), jo):
		logger.Tracef("mayjoin 0 %t ch: %s, match: %s", false, ch.String(), jo)
		return false
	// we only have exclude, do not join if in exclude
	case len(ji) == 0 && len(je) != 0:
		mayjoin := !stringInRegexp(ch.String(), je)
		logger.Tracef("mayjoin 1 %t ch: %s, match: %s", mayjoin, ch.String(), je)
		return mayjoin
	// nothing specified, everything may join
	case len(ji) == 0 && len(je) == 0:
		logger.Tracef("mayjoin 2 %t ch: %s, both empty", true, ch.String())
		return true
	// if we don't have joinexclude, then joininclude behaves as joinonly
	case len(ji) != 0 && len(je) == 0:
		mayjoin := stringInRegexp(ch.String(), ji)
		logger.Tracef("mayjoin 3 %t ch: %s, match: %s", mayjoin, ch.String(), ji)
		return mayjoin
	// joininclude overrides the joinexclude
	case len(ji) != 0 && len(je) != 0:
		// if explicit in ji we also may join
		mayjoin := stringInRegexp(ch.String(), ji)
		if mayjoin {
			logger.Tracef("mayjoin 4 %t ch: %s, match: %s", mayjoin, ch.String(), ji)
			return true
		}
		// otherwise evaluate against joinexclude
		mayjoin = !stringInRegexp(ch.String(), je)
		logger.Tracef("mayjoin 4 %t ch: %s, match: %s", mayjoin, ch.String(), je)
		return mayjoin
	}

	logger.Tracef("mayjoin default %t ch: %s, ji: %s, je: %s", false, ch.String(), ji, je)

	return false
}

func (u *User) isValidServer(server, protocol string) bool {
	var restrict []string

	switch protocol {
	case "mattermost":
		restrict = u.cfg.Mattermost().Bridge.Restrict

	case "slack":
		restrict = u.cfg.Slack().Bridge.Restrict

	case "mastodon":
		restrict = u.cfg.Mastodon().Bridge.Restrict

	default:
		return true
	}

	if len(restrict) == 0 {
		return true
	}

	logger.Debugf("restrict: %v", restrict)

	for _, srv := range restrict {
		if srv == server {
			return true
		}
	}

	return false
}

func (u *User) loginTo(protocol string) error {
	var err error

	// Reset the event loop tracker in case this User struct is recycled
	u.eventLoopMutex.Lock()
	u.eventLoopStarted = false
	u.eventLoopMutex.Unlock()

	switch protocol {
	case "mastodon":
		u.eventChan = make(chan *bridge.Event)
		u.br, err = mastodon.New(u.cfg, u.Credentials, u.eventChan, u.addUsersToChannels)
	case "slack":
		u.eventChan = make(chan *bridge.Event)
		u.br, err = slack.New(u.cfg, u.Credentials, u.eventChan, u.addUsersToChannels)
	case "mattermost":
		u.eventChan = make(chan *bridge.Event)
		u.br, _, err = mattermost.New(u.ctx, u.cfg, u.Credentials, u.eventChan, u.addUsersToChannels)
	}
	if err != nil {
		u.eventLoopMutex.Lock()
		u.br = nil
		u.eventLoopMutex.Unlock()

		return err
	}

	status, _ := u.br.StatusUser(u.ctx, u.br.GetMe().User)
	if status == "away" {
		u.Srv.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
	}

	info := u.br.GetMe()
	u.Me = true
	u.User = info.User
	u.MentionKeys = info.MentionKeys

	err = u.lastViewedAtDB.Update(func(tx *bolt.Tx) error {
		_, err2 := tx.CreateBucketIfNotExists([]byte(u.User))
		return err2
	})
	if err != nil {
		u.eventLoopMutex.Lock()
		u.br = nil
		u.eventLoopMutex.Unlock()

		return err
	}

	return nil
}

// nolint:unparam
func (u *User) logoutFrom(protocol string) error {
	logger.Debug("logging out from", protocol)

	u.eventLoopMutex.Lock()
	if u.br != nil {
		_ = u.br.Logout(u.ctx)
		u.br = nil
	}
	u.eventLoopMutex.Unlock()

	u.Srv.Logout(u)
	return nil
}

func (u *User) increaseMsgCounter(channelID string, skip int) int {
	u.msgCounterMutex.Lock()
	defer u.msgCounterMutex.Unlock()

	u.msgCounter[channelID]++

	// max 4096 entries (0xFFF); set back to 1, 0 is used for absent.
	if u.msgCounter[channelID] >= 4096 {
		u.msgCounter[channelID] = 1
	}

	if skip != 0 && u.msgCounter[channelID] == skip {
		u.msgCounter[channelID]++

		// max 4096 entries (0xFFF); set back to 1, 0 is used for absent.
		if u.msgCounter[channelID] >= 4096 {
			u.msgCounter[channelID] = 1
		}
	}

	return u.msgCounter[channelID]
}

func (u *User) updateMsgMapIndex(channelID string, counter int, messageID string) {
	u.msgMapIndexMutex.Lock()
	defer u.msgMapIndexMutex.Unlock()

	var (
		ok    bool
		msgID string
	)

	if _, ok = u.msgMapIndex[channelID]; !ok {
		u.msgMapIndex[channelID] = make(map[int]string)
	}

	if msgID, ok = u.msgMapIndex[channelID][counter]; !ok {
		u.msgMapIndex[channelID][counter] = messageID
		return
	}

	// map entry is the same as the one provided so do nothing.
	if msgID == messageID {
		return
	}

	// Remove previous msgID from MsgMap with the same counter.
	if _, ok = u.msgMap[channelID][msgID]; ok {
		delete(u.msgMap[channelID], msgID)
	}

	u.msgMapIndex[channelID][counter] = messageID
}

func (u *User) formatContextMessage(ts, threadMsgID, msg string) string {
	var formattedMsg string
	switch {
	case u.br.BridgeConfig().PrefixContext && threadMsgID != "":
		formattedMsg = threadMsgID + " " + msg
	case u.br.BridgeConfig().SuffixContext && threadMsgID != "":
		formattedMsg = msg + " " + threadMsgID
	default:
		formattedMsg = msg
	}
	if ts != "" {
		formattedMsg = "[" + ts + "] " + formattedMsg
	}
	return formattedMsg
}

func (u *User) prefixContext(channelID, messageID, parentID, event string) string {
	logger.Tracef("prefixContext ch %s msg %s parent %s event %s", channelID, messageID, parentID, event)

	// If the MessageID was deliberately blanked out (e.g., for system messages),
	// it is impossible to reply to it. Abort and return no thread context.
	if messageID == "" {
		return ""
	}

	prefixChar := "->"
	if u.br.FormatterConfig().Unicode {
		prefixChar = "↪"
	}

	if u.br.BridgeConfig().ThreadContext == "mattermost" || u.br.BridgeConfig().ThreadContext == "mattermost+post" {
		if parentID == "" {
			return "[@@" + messageID + "]"
		}
		if u.br.BridgeConfig().ThreadContext == "mattermost" || parentID == messageID {
			return "[" + prefixChar + "@@" + parentID + "]"
		}
		return "[" + prefixChar + "@@" + parentID + ",@@" + messageID + "]"
	}

	u.msgMapMutex.Lock()
	defer u.msgMapMutex.Unlock()

	var (
		currentcount, parentcount int
		ok                        bool
	)

	if _, ok = u.msgMap[channelID]; !ok {
		u.msgMap[channelID] = make(map[string]int)
	}

	if parentID != "" {
		if _, ok = u.msgMap[channelID][parentID]; !ok {
			u.msgMap[channelID][parentID] = u.increaseMsgCounter(channelID, parentcount)
		}

		parentcount = u.msgMap[channelID][parentID]
		u.updateMsgMapIndex(channelID, parentcount, parentID)
	}

	if _, ok = u.msgMap[channelID][messageID]; !ok {
		u.msgMap[channelID][messageID] = u.increaseMsgCounter(channelID, parentcount)
	}

	currentcount = u.msgMap[channelID][messageID]
	u.updateMsgMapIndex(channelID, currentcount, messageID)

	if parentID != "" {
		return fmt.Sprintf("[%s%03x,%03x]", prefixChar, parentcount, currentcount)
	}

	return fmt.Sprintf("[%03x]", currentcount)
}

func (u *User) updateLastViewed(channelID string) {
	u.updateCounterMutex.Lock()
	defer u.updateCounterMutex.Unlock()
	if t, ok := u.updateCounter[channelID]; ok {
		if time.Since(t) < time.Second*5 {
			return
		}
	}

	u.updateCounter[channelID] = time.Now()

	go func() {
		r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
		time.Sleep(time.Duration(r.Intn(3000)) * time.Millisecond)
		u.br.UpdateLastViewed(u.ctx, channelID)
	}()
}

func (u *User) saveLastViewedAt(channelID string) {
	if channelID == "" {
		return
	}

	currentTime := make([]byte, 8)
	//nolint:gosec // time.Now().UnixMilli() is positive (post-1970)
	binary.LittleEndian.PutUint64(currentTime, uint64(time.Now().UnixMilli()))

	err := u.lastViewedAtDB.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(u.User))
		if err != nil {
			return err
		}

		return b.Put([]byte(channelID), currentTime)
	})
	if err != nil {
		logger.Errorf("Failed to save last viewed time for channel %s: %v", channelID, err)
	}
}

func (u *User) saveGlobalLastEventTime(timestamp int64) {
	if timestamp <= 0 {
		return
	}

	_ = u.lastViewedAtDB.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(u.User))
		if err != nil {
			return err
		}

		// Use a reserved key that won't conflict with channel IDs
		buf := make([]byte, 8)

		//nolint:gosec // timestamp is strictly positive UnixMilli, safe to convert
		binary.LittleEndian.PutUint64(buf, uint64(timestamp))

		return b.Put([]byte("__global_last_event__"), buf)
	})
}

func (u *User) getGlobalLastEventTime() int64 {
	var timestamp int64

	_ = u.lastViewedAtDB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(u.User))
		if b == nil {
			return nil
		}

		v := b.Get([]byte("__global_last_event__"))

		if v != nil {
			val := binary.LittleEndian.Uint64(v)

			// Add a safety bounds check
			if val <= math.MaxInt64 {
				timestamp = int64(val)
			}
		}

		return nil
	})

	return timestamp
}

func (u *User) handleBannerChangeEvent(e *bridge.BannerChangeEvent) {
	// Prepending "*** " is a standard IRC convention for server announcements.
	// Many IRC clients look for this and will colorize it nicely in the main window.
	noticeMsg := "*** " + e.Text

	msg := &irc.Message{
		Prefix:   u.Srv.Prefix(),
		Command:  irc.NOTICE,
		Params:   []string{"*"},
		Trailing: noticeMsg,
	}

	_ = u.Encode(msg)
}

func (u *User) handleMessageThreadContext(channelID, messageID, parentID, event, text string) (string, string, string, bool, int) {
	prefix := ""
	suffix := ""
	maxlen := 460
	showContext := false

	newText := text
	switch {
	case u.br.BridgeConfig().PrefixContext && strings.HasPrefix(text, "\x01"):
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		newText = strings.Replace(text, "\x01ACTION ", "\x01ACTION "+prefix, 1)
		maxlen = len(newText)
	case u.br.BridgeConfig().PrefixContext && u.br.BridgeConfig().ShowContextMulti:
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		showContext = true
		maxlen -= len(prefix)
	case u.br.BridgeConfig().PrefixContext:
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		newText = prefix + text
	case u.br.BridgeConfig().SuffixContext && strings.HasSuffix(text, "\x01"):
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		newText = strings.Replace(text, " \x01", suffix+" \x01", 1)
		maxlen = len(newText)
	case u.br.BridgeConfig().SuffixContext && u.br.BridgeConfig().ShowContextMulti:
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		showContext = true
		maxlen -= len(suffix)
	case u.br.BridgeConfig().SuffixContext:
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		newText = strings.TrimRight(text, "\n") + suffix
	}

	return newText, prefix, suffix, showContext, maxlen
}

func (u *User) handleTyping(e *bridge.TypingEvent) {
	// Only send if this specific connected client negotiated message-tags
	if !u.HasCapability("message-tags") {
		return
	}

	// Construct the ghost user's IRC prefix safely from the Bridge event
	nick := e.Sender.Nick
	user := e.Sender.User

	if user == "" {
		user = nick
	}

	host := e.Sender.Host
	if host == "" {
		host = "mattermost"
	}

	prefix := fmt.Sprintf("%s!%s@%s", nick, user, host)

	// Determine the target routing (DM vs Channel)
	var target string
	if e.ChannelType == "D" {
		// For Direct Messages, the target is the connected client's own nick
		target = u.Nick
	} else {
		// For standard channels, resolve the mapped IRC channel name
		target = u.br.GetChannelName(u.ctx, e.ChannelID)
	}

	if target == "" {
		return
	}

	// Construct and encode TAGMSG
	rawCommand := fmt.Sprintf("@+typing=active :%s TAGMSG", prefix)

	msg := &irc.Message{
		Command: rawCommand,
		Params:  []string{target},
	}

	_ = u.Encode(msg)
}

func (u *User) addRequestedChannels(channelNames []string) {
	nameMap := make(map[string]struct{}, len(channelNames))
	for _, raw := range channelNames {
		name := strings.TrimPrefix(raw, "#")
		if name != "" && name != "&messages" && name != "&users" { //nolint:goconst
			nameMap[name] = struct{}{}
		}
	}

	if len(nameMap) == 0 {
		return
	}

	for {
		currentPtr := u.requestedChannels.Load()
		currentLen := 0

		if currentPtr != nil {
			currentLen = len(*currentPtr)
		}

		newMap := make(map[string]struct{}, currentLen+len(nameMap))

		if currentPtr != nil {
			for k := range *currentPtr {
				newMap[k] = struct{}{}
			}
		}

		for k := range nameMap {
			newMap[k] = struct{}{}
		}

		if u.requestedChannels.CompareAndSwap(currentPtr, &newMap) {
			return
		}
	}
}

func (u *User) isRequestedChannel(channelName string) bool {
	m := u.requestedChannels.Load()
	if m == nil {
		return false
	}

	name := strings.TrimPrefix(channelName, "#")
	_, ok := (*m)[name]

	return ok
}
