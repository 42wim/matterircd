package irckit

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
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
	inprogress  bool
	eventChan   chan *bridge.Event
	away        bool

	eventLoopMutex   sync.Mutex
	eventLoopStarted bool

	lastViewedAtDB *bolt.DB

	lastEventTimeMutex sync.RWMutex
	lastEventTime      int64

	msgCounterMutex sync.RWMutex
	msgCounter      map[string]int

	msgLastMutex sync.RWMutex
	msgLast      map[string][2]string

	msgMapMutex      sync.RWMutex
	msgMap           map[string]map[string]int
	msgMapIndexMutex sync.RWMutex
	msgMapIndex      map[string]map[int]string

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
			u.lastEventTimeMutex.Lock()
			u.lastEventTime = time.Now().UnixMilli()
			u.lastEventTimeMutex.Unlock()

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
				u.lastEventTimeMutex.RLock()
				u.saveGlobalLastEventTime(u.lastEventTime)
				u.lastEventTimeMutex.RUnlock()

				return
			}

		case <-flushTicker.C:
			// Periodically flush the timestamp to BoltDB
			u.lastEventTimeMutex.RLock()
			currentTimestamp := u.lastEventTime
			u.lastEventTimeMutex.RUnlock()
			u.saveGlobalLastEventTime(currentTimestamp)
		}
	}
}

func (u *User) handleChannelTopicEvent(event *bridge.ChannelTopicEvent) {
	tu, ok := u.Srv.HasUserID(event.UserID)
	if event.UserID == u.User {
		ok = true
		tu = u
	}

	if ok {
		ch := u.Srv.Channel(event.ChannelID)
		ch.Topic(tu, event.Text)

		u.saveLastViewedAt(event.ChannelID)
		return
	}

	logger.Errorf("topic change failure: userID %s not found", event.UserID)
}

const (
	blockQuoteCharDefault     = ">"
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
			if m == u.Nick {
				continue
			}

			if m == "" {
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
	trimmedPrefix := strings.TrimSpace(prefix)
	trimmedSuffix := strings.TrimSpace(suffix)

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

	opts := utils.ProcessMessageOpts{
		DisableMarkdown:    disableMarkdown,
		DisableEmoji:       disableEmoji,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
	}

	utils.ProcessMessageText(text, opts, func(line string) {
		if line == "" || line == trimmedPrefix || line == trimmedSuffix {
			return
		}

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
	})

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
			if m == u.Nick {
				continue
			}

			if m == "" {
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
	maxlen := 440

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

	trimmedPrefix := strings.TrimSpace(prefix)
	trimmedSuffix := strings.TrimSpace(suffix)

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

	opts := utils.ProcessMessageOpts{
		DisableMarkdown:    disableMarkdown,
		DisableEmoji:       disableEmoji,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
	}

	utils.ProcessMessageText(text, opts, func(line string) {
		if line == "" || line == trimmedPrefix || line == trimmedSuffix {
			return
		}

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
	})

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

	for _, fname := range event.Files {
		fileMsg := "\x1ddownload file - " + fname.Name + "\x1d"
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

func (u *User) handleUserUpdateEvent(event *bridge.UserUpdateEvent) {
	u.updateUserFromInfo(event.User)
}

func (u *User) handleStatusChangeEvent(event *bridge.StatusChangeEvent) {
	if event.UserID == u.br.GetMe().User {
		switch event.Status {
		case "online":
			if u.away {
				logger.Debug("setting myself online")
				u.away = false
				u.Srv.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away") //nolint:errcheck
			}
		// Ignore `offline` status changes to prevent bouncing between being marked away and not.
		case "offline":
			logger.Debugf("doing nothing as status %s", event.Status)
		default:
			if !u.away {
				logger.Debug("setting myself away")
				u.away = true
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
		if reactionEmoji, ok := utils.EmojiFromAlias(reaction); ok {
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
	logger.Debugf("adding %d to %s", len(users), channel)

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

	logger.Trace("in addUsersToChannels()")
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
		if brchannel.DM && !u.br.BridgeConfig().JoinDM {
			lastPost := time.UnixMilli(brchannel.LastPostAt)

			threshold := lastSyncThreshold
			if threshold.IsZero() {
				dmThresh := u.cfg.Mattermost().DefaultDMOfflineThreshold
				if dmThresh == 0 {
					dmThresh = 24 * time.Hour
				}

				threshold = time.Now().Add(-dmThresh)
			}

			// If the channel has been dormant since before our threshold, safely skip it
			if lastPost.Before(threshold) {
				if logger.Logger.IsLevelEnabled(logrus.TraceLevel) {
					logger.Tracef("Skipping dormant DM channel %s (LastPost: %v, Threshold: %v)", brchannel.Name, lastPost, threshold)
				} else {
					logger.Debugf("Skipping dormant DM channel %s (LastPost: %v)", brchannel.Name, lastPost)
				}

				continue
			}

			if logger.Logger.IsLevelEnabled(logrus.TraceLevel) {
				logger.Tracef("SmartJoin: Joining DM channel %s due to recent offline activity (LastPost: %v, Threshold: %v)", brchannel.Name, lastPost, threshold)
			} else {
				logger.Debugf("SmartJoin: Joining DM channel %s due to recent offline activity (LastPost: %v)", brchannel.Name, lastPost)
			}
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
	if !strings.Contains(mmchannel.Name, "__") {
		return u.Srv.Channel(mmchannel.ID).SpoofMessage
	}

	var otherNick string

	// Resolve the other participant's IRC nick for DMs
	parts := strings.Split(mmchannel.Name, "__")
	if len(parts) == 2 {
		myID := u.br.GetMe().User
		otherID := parts[0]

		if otherID == myID {
			otherID = parts[1]
		}

		if info := u.br.GetUser(u.ctx, otherID); info != nil {
			otherNick = info.Nick
		}
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
	isDM := strings.Contains(brchannel.Name, "__")
	if bestSince == 0 || (isDM && !inDB) {
		// If Mattermost gave us a valid LastPostAt, use it (if it's newer than 31 days)
		if brchannel.LastPostAt > 0 {
			return brchannel.LastPostAt, "lastpost-fallback", inDB
		}

		// If Mattermost failed, use our Global Last Event Timestamp
		globalSince := u.getGlobalLastEventTime()
		if globalSince > 0 {
			return globalSince, "global-event-fallback", inDB
		}

		// Only if it's the very first time booting up
		return replayCutoff, "cutoff-fallback", inDB
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
			isDM := strings.Contains(brchannel.Name, "__")

			// If Lazy-Join is enabled AND replay strategy is "saved", ONLY join channels already known in
			// the last saved DB. If Lazy-Join is disabled, joins all channels user is member of!
			if strategy == "saved" && lazyJoin && !inDB && !isDM {
				continue
			}

			// Dormancy Filter for Public Channels
			// DMs bypass this because they are already strictly filtered by
			// DefaultDMOfflineThreshold upstream in addUsersToChannels().
			if lazyJoin && !isDM {
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
			if since > 0 && since < replayCutoff {
				logger.Infof("Capping replay history for %s to %s (original since: %s)",
					brchannel.Name,
					replayDuration,
					time.UnixMilli(since).Format("2006-01-02"),
				)
				since = replayCutoff
				sinceStr = "replay-cutoff"
			}

			// Actually join the IRC channel!
			if !isDM {
				channelName := brchannel.Name
				if brchannel.TeamID != u.br.GetMe().TeamID || (u.br.Protocol() == "mattermost" && u.cfg.Mattermost().PrefixMainTeam) {
					channelName = u.br.GetTeamName(u.ctx, brchannel.TeamID) + "/" + brchannel.Name
				}
				u.syncChannel(brchannel.ID, "#"+channelName)
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

	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator

	opts := utils.ProcessMessageOpts{
		DisableMarkdown:    disableMarkdown,
		DisableEmoji:       disableEmoji,
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

		textToProcess := utils.WrapMessage(text, 440)

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
			fileMsg := "\x1ddownload file - " + f.Name + "\x1d"
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
		text = utils.WrapMessage(text, 440)
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

func (u *User) syncChannel(id string, name string) {
	users, err := u.br.GetChannelUsers(u.ctx, id)
	if err != nil {
		logger.Error(err)
		return
	}

	srv := u.Srv

	// create and join the users
	batchUsers := u.CreateUsersFromInfo(users)
	srv.BatchAdd(batchUsers)
	u.addUsersToChannel(batchUsers, "&users", "&users")
	u.addUsersToChannel(batchUsers, name, id)

	// add myself ONLY if I am actually a member
	ch := srv.Channel(id)
	if u.br.IsChannelMember(id) && !ch.HasUser(u) && u.mayJoin(id) {
		logger.Debugf("syncChannel adding myself to %s (id: %s)", name, id)
		ch.Join(u)
		svc, _ := srv.HasUser(u.br.Protocol())
		ch.Topic(svc, u.br.Topic(u.ctx, ch.ID()))
	}
}

func (u *User) mayJoin(channelID string) bool {
	ch := u.Srv.Channel(channelID)

	jo := u.br.BridgeConfig().JoinOnly
	ji := u.br.BridgeConfig().JoinInclude
	je := u.br.BridgeConfig().JoinExclude

	switch {
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
		logger.Tracef("mayjoin 4 %t ch: %s, match: %s", mayjoin, ch.String(), ji)
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
		if u.cfg.Mattermost().IgnoreServerVersion || strings.HasPrefix(u.getMattermostVersion(), "7.") || strings.HasPrefix(u.getMattermostVersion(), "8.") || strings.HasPrefix(u.getMattermostVersion(), "9.") || strings.HasPrefix(u.getMattermostVersion(), "10.") || strings.HasPrefix(u.getMattermostVersion(), "11.") {
			u.br, _, err = mattermost.New(u.ctx, u.cfg, u.Credentials, u.eventChan, u.addUsersToChannels)
		} else {
			return fmt.Errorf("mattermost version %s not supported", u.getMattermostVersion())
		}
	}
	if err != nil {
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
		return err
	}

	return nil
}

// nolint:unparam
func (u *User) logoutFrom(protocol string) error {
	logger.Debug("logging out from", protocol)

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
	case u.br.BridgeConfig().PrefixContext:
		formattedMsg = threadMsgID + " " + msg
	case u.br.BridgeConfig().SuffixContext:
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
		b := tx.Bucket([]byte(u.User))
		err := b.Put([]byte(channelID), currentTime)
		return err
	})
	if err != nil {
		logger.Fatal(err)
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

func (u *User) getMattermostVersion() string {
	proto := "https"
	if u.cfg.Mattermost().Insecure {
		proto = "http"
	}

	resp, err := http.Get(proto + "://" + u.Credentials.Server)
	if err != nil {
		logger.Errorf("Failed to get mattermost version: %s", err)
		return ""
	}

	defer resp.Body.Close()

	return resp.Header.Get("X-Version-Id")
}

func (u *User) handleBannerChangeEvent(e *bridge.BannerChangeEvent) {
	// Prepending "*** " is a standard IRC convention for server announcements.
	// Many IRC clients look for this and will colorize it nicely in the main window.
	noticeMsg := "*** " + e.Text

	msg := &irc.Message{
		Prefix:   u.Srv.Prefix(),
		Command:  "NOTICE",
		Params:   []string{"*"},
		Trailing: noticeMsg,
	}

	_ = u.Encode(msg)
}

func (u *User) handleMessageThreadContext(channelID, messageID, parentID, event, text string) (string, string, string, bool, int) {
	prefix := ""
	suffix := ""
	maxlen := 440
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
