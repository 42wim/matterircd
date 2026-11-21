package rocketchat

import (
	"cmp"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/rocketchatclient"
	"github.com/sirupsen/logrus"
)

const (
	protocolName         = "rocketchat"
	defaultPostSizeLimit = 5000
	lastSentSummaryLen   = 90
	msgRoomCacheSize     = 2000
	msgTextCacheSize     = 2000
	msgTextMaxBytes      = 1024
	maxHistoryFetchLimit = 200
	defaultSearchCount   = 20
	maxSearchRooms       = 25
	searchTimeout        = 15 * time.Second
)

// roomMentionRegexp matches Rocket.Chat's @all and @here along with Mattermost/Matrix @channel and @room.
var roomMentionRegexp = regexp.MustCompile(`@(?:room|channel|all|here)(?:\W|$)`)

var markdownReplacer = strings.NewReplacer(
	"\n", " ",
	"```", "`",
	"~~~", "`",
)

var logger = newLogger()

func newLogger() *logrus.Entry {
	l := logrus.New()
	l.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	return l.WithFields(logrus.Fields{"prefix": "bridge/rocketchat"})
}

// RocketChat implements bridge.Bridger and rocketchatclient.Handler.
type RocketChat struct {
	rc        *rocketchatclient.Client
	eventChan chan *bridge.Event
	cfg       *config.Config
	onConnect func()

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	initDone chan struct{}
	readyCh  chan struct{}

	connected atomic.Bool
	ready     atomic.Bool

	msgRoomCache     *lru.Cache[string, string]
	msgTextCache     *lru.Cache[string, string]
	msgAuthorCache   *lru.Cache[string, rocketchatclient.UserRef]
	msgParentCache   *lru.Cache[string, string]
	msgLastSentCache *lru.Cache[string, string]

	roomMetaMu sync.Mutex
	roomMeta   map[string]roomSnapshot

	unregisterReloadHook func()
}

type roomSnapshot struct {
	name  string
	topic string
}

// New authenticates with Rocket.Chat, starts the DDP event loop, and returns a bridge.Bridger.
func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, *rocketchatclient.Client, error) {
	rcCfg := cfg.RocketChat()

	server := cmp.Or(cred.Server, rcCfg.Server)
	if server == "" {
		return nil, nil, errors.New("rocketchat: server is required")
	}

	bridgeCtx, cancel := context.WithCancel(ctx)

	r := &RocketChat{
		eventChan: eventChan,
		cfg:       cfg,
		onConnect: onConnect,
		ctx:       bridgeCtx,
		cancel:    cancel,
		initDone:  make(chan struct{}),
		readyCh:   make(chan struct{}),
	}

	r.msgRoomCache, _ = lru.New[string, string](msgRoomCacheSize)
	r.msgTextCache, _ = lru.New[string, string](msgTextCacheSize)
	r.msgAuthorCache, _ = lru.New[string, rocketchatclient.UserRef](msgTextCacheSize)
	r.msgParentCache, _ = lru.New[string, string](200)
	r.msgLastSentCache, _ = lru.New[string, string](10)
	r.roomMeta = make(map[string]roomSnapshot)

	defer close(r.initDone)

	var opts []rocketchatclient.Option
	if config.UserAgent != "" {
		opts = append(opts, rocketchatclient.WithUserAgent(config.UserAgent))
	}

	rcCred := rocketchatclient.Credentials{
		Server:        server,
		Login:         cred.Login,
		Pass:          cred.Pass,
		Token:         cred.Token,
		TwoFactorCode: cred.MFAToken,
		NoTLS:         rcCfg.Insecure,
		SkipTLSVerify: rcCfg.SkipTLSVerify,
	}

	rc, err := rocketchatclient.New(bridgeCtx, rcCred, r, opts...)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	r.rc = rc

	setLogLevels := makeLogLevelSetter(rc)
	setLogLevels(cfg.Current())
	r.unregisterReloadHook = cfg.RegisterReloadHook(setLogLevels)
	r.connected.Store(true)

	return r, rc, nil
}

func makeLogLevelSetter(rc *rocketchatclient.Client) func(*config.RuntimeConfig) {
	return func(runtimeCfg *config.RuntimeConfig) {
		switch {
		case runtimeCfg.Trace:
			logger.Logger.SetLevel(logrus.TraceLevel)
		case runtimeCfg.Debug:
			logger.Logger.SetLevel(logrus.DebugLevel)
		default:
			logger.Logger.SetLevel(logrus.InfoLevel)
		}

		rc.SetLogLevel("info")

		if runtimeCfg.RocketChat.RocketchatclientLogLevel != "" {
			logger.Infof("enabling rocketchatclient logging: level: %s", runtimeCfg.RocketChat.RocketchatclientLogLevel)
			rc.SetLogLevel(strings.ToLower(runtimeCfg.RocketChat.RocketchatclientLogLevel))
		}

		rc.SetLogAPICalls("error")

		if runtimeCfg.Profiling {
			if runtimeCfg.Trace {
				rc.SetLogAPICalls("trace")
			} else if runtimeCfg.Debug {
				rc.SetLogAPICalls("warn")
			}
		}
	}
}

// Ready implements rocketchatclient.Handler.
func (r *RocketChat) Ready() {
	select {
	case <-r.initDone:
	case <-r.ctx.Done():
		return
	}

	if r.rc == nil {
		return
	}

	r.seedRoomMeta()
	r.ready.Store(true)
	close(r.readyCh)

	if r.onConnect != nil {
		go r.onConnect()
	}
}

func (r *RocketChat) seedRoomMeta() {
	rooms := r.rc.Rooms()

	r.roomMetaMu.Lock()
	for _, room := range rooms {
		r.roomMeta[room.ID] = roomSnapshot{
			name:  r.roomName(room),
			topic: room.Topic,
		}
	}
	r.roomMetaMu.Unlock()
}

// OnReconnect implements rocketchatclient.ReconnectHandler.
func (r *RocketChat) OnReconnect(downtime time.Duration) {
	if !r.ready.Load() || r.ctx.Err() != nil || r.onConnect == nil {
		return
	}

	thresh := r.cfg.RocketChat().HeavySyncThreshold
	if thresh <= 0 {
		thresh = 15 * time.Minute
	}

	if downtime < thresh {
		return
	}

	logger.Infof("rocketchat sync recovered after %s, triggering channel synchronization", downtime.Round(time.Second))

	go r.onConnect()
}

// OnMessage implements rocketchatclient.Handler.
func (r *RocketChat) OnMessage(msg *rocketchatclient.Message) {
	if !r.ready.Load() || msg == nil || msg.RoomID == "" {
		return
	}

	r.cacheMessageMeta(msg)

	room, ok := r.rc.Room(msg.RoomID)
	isDM := ok && r.isOneToOneDM(room)
	text, msgType := r.formatIncomingText(msg, isDM)
	files := r.extractFiles(msg)
	sender := r.senderInfo(msg.User)
	createAt := msg.Timestamp.Time.UnixMilli()

	if text != "" || len(files) == 0 {
		r.dispatchText(msg.RoomID, msg.ID, msg.ThreadID, "", msgType, text, sender, createAt)
	}

	if len(files) > 0 {
		r.dispatchFiles(msg.RoomID, msg.ID, msg.ThreadID, files, sender, createAt)
	}
}

// OnEdit implements rocketchatclient.Handler.
func (r *RocketChat) OnEdit(msg *rocketchatclient.Message) {
	if !r.ready.Load() || msg == nil || msg.RoomID == "" {
		return
	}

	sender := r.senderInfo(msg.User)
	rcCfg := r.cfg.RocketChat()

	r.msgParentCache.Remove(msg.ID)
	r.cacheMessageMeta(msg)

	if sender.Me && rcCfg.DisableShowOwnModified {
		return
	}

	room, ok := r.rc.Room(msg.RoomID)
	isDM := ok && r.isOneToOneDM(room)
	text, msgType := r.formatIncomingText(msg, isDM)

	text = utils.AppendMessageSuffix(text, utils.FormatActionSuffix(utils.SuffixEdited, rcCfg.Formatter.Unicode))

	r.dispatchText(msg.RoomID, msg.ID, msg.ThreadID, "post_edited", msgType, text, sender, msg.Timestamp.Time.UnixMilli())
}

// OnDelete implements rocketchatclient.Handler.
func (r *RocketChat) OnDelete(roomID, msgID string) {
	if !r.ready.Load() || roomID == "" || msgID == "" {
		return
	}

	rcCfg := r.cfg.RocketChat()

	authorRef, hasAuthor := r.msgAuthorCache.Get(msgID)
	r.msgAuthorCache.Remove(msgID)

	if hasAuthor && authorRef.ID == r.rc.UserID() && rcCfg.DisableShowOwnModified {
		r.msgTextCache.Remove(msgID)
		r.msgParentCache.Remove(msgID)

		return
	}

	text, _ := r.msgTextCache.Get(msgID)
	r.msgTextCache.Remove(msgID)
	r.msgParentCache.Remove(msgID)

	summary := ""
	if text != "" {
		summary = r.formatSummary(text, rcCfg.ShortenRepliesTo, "")
	}

	summary = utils.AppendMessageSuffix(summary, utils.FormatActionSuffix(utils.SuffixDeleted, rcCfg.Formatter.Unicode))

	var sender *bridge.UserInfo
	if hasAuthor {
		sender = r.senderInfo(authorRef)
	} else {
		sender = &bridge.UserInfo{
			Nick:  protocolName,
			User:  protocolName,
			Ghost: true,
		}
	}

	r.dispatchText(roomID, msgID, "", "post_deleted", "", summary, sender, 0)
}

// OnReaction implements rocketchatclient.Handler.
func (r *RocketChat) OnReaction(ev *rocketchatclient.ReactionEvent) {
	if !r.ready.Load() || ev == nil || ev.RoomID == "" || ev.MessageID == "" {
		return
	}

	rcCfg := r.cfg.RocketChat()
	if rcCfg.HideReactions {
		return
	}

	sender := r.reactionSender(ev)
	if sender == nil || (sender.Me && !rcCfg.ShowOwnReactions) {
		return
	}

	r.msgRoomCache.Add(ev.MessageID, ev.RoomID)

	var text string
	if !rcCfg.HideReplies {
		text = r.withParentPrefix(ev.MessageID, "")
	}

	emoji := strings.Trim(ev.Emoji, ":")
	room, ok := r.rc.Room(ev.RoomID)

	var (
		receiver *bridge.UserInfo
		chType   string
	)

	if ok && r.isOneToOneDM(room) {
		receiver = r.dmPeer(room)
		if receiver == nil {
			receiver = sender
		}

		chType = "D"
	} else {
		chType = r.channelType(room)
	}

	r.emitReaction(ev, sender, receiver, chType, emoji, text)
}

func (r *RocketChat) emitReaction(ev *rocketchatclient.ReactionEvent, sender, receiver *bridge.UserInfo, chType, emoji, text string) {
	data := bridge.ReactionAddEvent{
		ChannelID:   ev.RoomID,
		MessageID:   ev.MessageID,
		Receiver:    receiver,
		Sender:      sender,
		Reaction:    emoji,
		ChannelType: chType,
		Message:     text,
	}

	if ev.Added {
		r.sendEvent(&bridge.Event{
			Type: "reaction_add",
			Data: &data,
		})

		return
	}

	rem := bridge.ReactionRemoveEvent(data)
	r.sendEvent(&bridge.Event{
		Type: "reaction_remove",
		Data: &rem,
	})
}

func (r *RocketChat) reactionSender(ev *rocketchatclient.ReactionEvent) *bridge.UserInfo {
	if ev.UserID != "" {
		if u := r.GetUser(r.ctx, ev.UserID); u != nil {
			return u
		}
	}

	if ev.Username != "" {
		if u := r.GetUserByUsername(r.ctx, ev.Username); u != nil {
			return u
		}
	}

	if ev.UserID == "" && ev.Username == "" {
		return nil
	}

	id := cmp.Or(ev.UserID, ev.Username)
	nick := cmp.Or(ev.Username, ev.UserID)

	return &bridge.UserInfo{
		Nick:     nick,
		User:     id,
		Real:     nick,
		Username: ev.Username,
		Ghost:    id != r.rc.UserID(),
		Me:       id == r.rc.UserID(),
	}
}

// OnRoomChange implements rocketchatclient.Handler.
func (r *RocketChat) OnRoomChange(room *rocketchatclient.Room) {
	if !r.ready.Load() || room == nil || room.ID == "" {
		return
	}

	newName := r.roomName(room)

	r.roomMetaMu.Lock()
	prev, seen := r.roomMeta[room.ID]
	r.roomMeta[room.ID] = roomSnapshot{
		name:  newName,
		topic: room.Topic,
	}
	r.roomMetaMu.Unlock()

	if seen && !r.isOneToOneDM(room) && newName != "" && newName != prev.name {
		r.sendEvent(&bridge.Event{
			Type: "channel_update",
			Data: &bridge.ChannelUpdateEvent{
				ChannelID:   room.ID,
				Name:        newName,
				DisplayName: cmp.Or(room.FName, room.Name),
			},
		})
	}

	if (!seen || room.Topic != prev.topic) && room.Topic != "" && (seen || r.cfg.RocketChat().ShowTopic) {
		userID := ""
		if room.Owner != nil {
			userID = room.Owner.ID
		}

		r.sendEvent(&bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				ChannelID: room.ID,
				Text:      room.Topic,
				UserID:    userID,
			},
		})
	}
}

// OnMember implements rocketchatclient.MemberHandler.
func (r *RocketChat) OnMember(ev *rocketchatclient.MemberEvent) {
	if !r.ready.Load() || ev == nil || ev.RoomID == "" {
		return
	}

	if room, ok := r.rc.Room(ev.RoomID); ok && r.isOneToOneDM(room) {
		return
	}

	target := r.memberTargetUser(ev)
	if target == nil {
		return
	}

	var actor *bridge.UserInfo
	if ev.Sender != "" {
		actor = r.GetUser(r.ctx, ev.Sender)
	}

	if actor == nil {
		actor = &bridge.UserInfo{Nick: "system"}
	}

	switch ev.Action {
	case "join":
		r.sendEvent(&bridge.Event{
			Type: "channel_add",
			Data: &bridge.ChannelAddEvent{
				Adder:     actor,
				Added:     []*bridge.UserInfo{target},
				ChannelID: ev.RoomID,
			},
		})
	case "leave":
		r.sendEvent(&bridge.Event{
			Type: "channel_remove",
			Data: &bridge.ChannelRemoveEvent{
				Remover:   actor,
				Removed:   []*bridge.UserInfo{target},
				ChannelID: ev.RoomID,
			},
		})
	}
}

func (r *RocketChat) memberTargetUser(ev *rocketchatclient.MemberEvent) *bridge.UserInfo {
	if ev.UserID == r.rc.UserID() {
		return r.GetMe()
	}

	if ev.UserID != "" {
		if u := r.GetUser(r.ctx, ev.UserID); u != nil {
			return u
		}
	}

	if ev.Username != "" {
		if u := r.GetUserByUsername(r.ctx, ev.Username); u != nil {
			return u
		}
	}

	if ev.UserID == "" && ev.Username == "" {
		return nil
	}

	id := cmp.Or(ev.UserID, ev.Username)
	nick := cmp.Or(ev.Username, ev.UserID)

	return &bridge.UserInfo{
		Nick:     nick,
		User:     id,
		Real:     nick,
		Username: ev.Username,
		Ghost:    id != r.rc.UserID(),
		Me:       id == r.rc.UserID(),
	}
}

// OnPresence implements rocketchatclient.Handler.
func (r *RocketChat) OnPresence(userID, status string) {
	if !r.ready.Load() || userID == "" {
		return
	}

	r.sendEvent(&bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: userID,
			Status: status,
		},
	})
}

func (r *RocketChat) dispatchText(roomID, msgID, parentID, eventType, msgType, text string, sender *bridge.UserInfo, createAt int64) {
	room, ok := r.rc.Room(roomID)
	if ok && r.isOneToOneDM(room) {
		peer := r.dmPeer(room)
		if peer == nil {
			peer = sender
		}

		r.sendEvent(&bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: roomID,
				Receiver:  peer,
				Sender:    sender,
				MessageID: msgID,
				Event:     eventType,
				ParentID:  parentID,
				CreateAt:  createAt,
			},
		})

		return
	}

	r.sendEvent(&bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   roomID,
			Sender:      sender,
			MessageType: msgType,
			ChannelType: r.channelType(room),
			MessageID:   msgID,
			Event:       eventType,
			ParentID:    parentID,
			CreateAt:    createAt,
		},
	})
}

func (r *RocketChat) dispatchFiles(roomID, msgID, parentID string, files []*bridge.File, sender *bridge.UserInfo, createAt int64) {
	if len(files) == 0 {
		return
	}

	room, ok := r.rc.Room(roomID)
	channelType := ""
	receiver := r.GetMe()

	if ok && r.isOneToOneDM(room) {
		channelType = "D"

		if sender != nil && sender.Me {
			if peer := r.dmPeer(room); peer != nil {
				receiver = peer
			}
		}
	}

	if receiver == nil {
		receiver = sender
	}

	r.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Receiver:    receiver,
			Sender:      sender,
			ChannelID:   roomID,
			ChannelType: channelType,
			Files:       files,
			MessageID:   msgID,
			ParentID:    parentID,
			CreateAt:    createAt,
		},
	})
}

func (r *RocketChat) formatIncomingText(msg *rocketchatclient.Message, isDM bool) (string, string) {
	text := msg.Text
	if text == "" && msg.File != nil && len(msg.Attachments) > 0 && msg.Attachments[0].Description != "" {
		text = msg.Attachments[0].Description
	}

	text = r.resolveMentions(msg, text)
	rcCfg := r.cfg.RocketChat()

	if rcCfg.DisableDefaultMentions {
		text = utils.SanitizeDefaultMentions(text, rcCfg.Formatter.Unicode)
	}

	if !rcCfg.HideReplies && msg.ThreadID != "" {
		text = r.withParentPrefix(msg.ThreadID, text)
	}

	if msg.Type == "me" && text != "" {
		text = "\x01ACTION " + text + "\x01"
	}

	if !rcCfg.DisableDefaultMentions && !isDM && (roomMentionRegexp.MatchString(text) || hasRoomMention(msg)) {
		return text, "notice"
	}

	return text, ""
}

func hasRoomMention(msg *rocketchatclient.Message) bool {
	if msg == nil {
		return false
	}

	for i := range msg.Mentions {
		m := &msg.Mentions[i]
		if m.ID == "all" || m.ID == "here" || m.Username == "all" || m.Username == "here" {
			return true
		}
	}

	return false
}

func (r *RocketChat) resolveMentions(msg *rocketchatclient.Message, text string) string {
	if !strings.Contains(text, "<@") {
		return text
	}

	var byID map[string]string
	if msg != nil && len(msg.Mentions) > 0 {
		byID = make(map[string]string, len(msg.Mentions))
		for i := range msg.Mentions {
			m := &msg.Mentions[i]
			if m.ID != "" && m.Username != "" {
				byID[m.ID] = m.Username
			}
		}
	}

	var sb strings.Builder

	for {
		idx := strings.Index(text, "<@")
		if idx < 0 {
			if sb.Len() == 0 {
				return text
			}

			sb.WriteString(text)

			return sb.String()
		}

		end := strings.IndexByte(text[idx+2:], '>')
		if end < 0 {
			if sb.Len() == 0 {
				return text
			}

			sb.WriteString(text)

			return sb.String()
		}

		sb.Grow(len(text))
		sb.WriteString(text[:idx])

		uid := strings.TrimPrefix(text[idx+2:idx+2+end], "!")
		if name, _, hasPipe := strings.Cut(uid, "|"); hasPipe {
			uid = name
		}

		resolved := r.resolveMentionName(byID, uid)
		if resolved != "" {
			sb.WriteByte('@')
			sb.WriteString(resolved)
		} else {
			sb.WriteString(text[idx : idx+2+end+1])
		}

		text = text[idx+2+end+1:]
	}
}

func (r *RocketChat) resolveMentionName(byID map[string]string, uid string) string {
	if uid == "" {
		return ""
	}

	if byID != nil {
		if name, ok := byID[uid]; ok {
			return name
		}
	}

	if u, ok := r.rc.User(uid); ok && u.Username != "" {
		return u.Username
	}

	return ""
}

func (r *RocketChat) extractFiles(msg *rocketchatclient.Message) []*bridge.File {
	if msg == nil {
		return nil
	}

	if msg.File == nil && len(msg.Files) == 0 {
		return r.extractAttachmentFiles(msg.Attachments)
	}

	var firstAttSize int64
	if len(msg.Attachments) > 0 {
		firstAttSize = msg.Attachments[0].ImageSize
	}

	seen := make(map[string]struct{}, 1+len(msg.Files))
	files := r.appendMessageFile(nil, seen, msg.File, firstAttSize)

	for i := range msg.Files {
		files = r.appendMessageFile(files, seen, &msg.Files[i], 0)
	}

	if len(files) > 0 {
		return files
	}

	return r.extractAttachmentFiles(msg.Attachments)
}

func (r *RocketChat) appendMessageFile(files []*bridge.File, seen map[string]struct{}, f *rocketchatclient.File, size int64) []*bridge.File {
	if f == nil || f.ID == "" {
		return files
	}

	if _, exists := seen[f.ID]; exists {
		return files
	}

	seen[f.ID] = struct{}{}

	fileURL := r.rc.DownloadURL(f.ID, f.Name)
	if fileURL == "" {
		return files
	}

	return append(files, &bridge.File{
		Name: cmp.Or(f.Name, f.ID),
		Size: size,
		URL:  fileURL,
	})
}

func (r *RocketChat) extractAttachmentFiles(attachments []rocketchatclient.Attachment) []*bridge.File {
	var files []*bridge.File

	for i := range attachments {
		att := &attachments[i]
		if att.MessageLink != "" {
			continue
		}

		raw := cmp.Or(att.TitleLink, att.ImageURL, att.AudioURL, att.VideoURL)
		if !strings.Contains(raw, "/file-upload/") {
			continue
		}

		resolved := r.rc.ResolveAttachmentURL(raw)
		if resolved == "" {
			continue
		}

		files = append(files, &bridge.File{
			Name: cmp.Or(att.Title, "attachment"),
			Size: att.ImageSize,
			URL:  resolved,
		})
	}

	return files
}

func (r *RocketChat) cacheMessageMeta(msg *rocketchatclient.Message) {
	if msg == nil || msg.ID == "" {
		return
	}

	if msg.RoomID != "" {
		r.msgRoomCache.Add(msg.ID, msg.RoomID)
	}

	if msg.User.ID != "" || msg.User.Username != "" {
		r.msgAuthorCache.Add(msg.ID, msg.User)
	}

	short := r.shortText(msg.Text)
	if short == "" {
		r.msgTextCache.Remove(msg.ID)

		return
	}

	r.msgTextCache.Add(msg.ID, short)
}

func (r *RocketChat) shortText(text string) string {
	line, rest, multiline := strings.Cut(text, "\n")
	if line == "" {
		return ""
	}

	truncated := multiline && strings.TrimSpace(rest) != ""

	limit := msgTextMaxBytes
	if n := r.cfg.RocketChat().ShortenRepliesTo; n > 0 {
		limit = min(n*4, msgTextMaxBytes)
	}

	if len(line) > limit {
		line = strings.ToValidUTF8(line[:limit], "")
		truncated = true
	}

	line = strings.Clone(line)

	if truncated {
		if r.cfg.RocketChat().Formatter.Unicode {
			line += "…"
		} else {
			line += "..."
		}
	}

	return line
}

func (r *RocketChat) withParentPrefix(parentID, text string) string {
	if parentID == "" {
		return text
	}

	if cached, ok := r.msgParentCache.Get(parentID); ok {
		return strings.TrimRight(text, "\n") + cached
	}

	parentMsg, err := r.rc.ResolveMessage(r.ctx, parentID)
	if err != nil || parentMsg == nil {
		return text
	}

	nick := cmp.Or(parentMsg.User.Username, parentMsg.User.ID)
	if u := r.senderInfo(parentMsg.User); u != nil && u.Nick != "" {
		nick = u.Nick
	}

	rcCfg := r.cfg.RocketChat()
	summary := r.formatSummary(parentMsg.Text, rcCfg.ShortenRepliesTo, "@")

	if rcCfg.DisableDefaultMentions {
		summary = utils.SanitizeDefaultMentions(summary, rcCfg.Formatter.Unicode)
	}

	reply := " (re @" + nick + ": " + summary + ")"
	r.msgParentCache.Add(parentID, reply)

	return strings.TrimRight(text, "\n") + reply
}

func (r *RocketChat) formatSummary(msg string, maxLen int, uncounted string) string {
	fc := &r.cfg.RocketChat().Formatter
	unicode := fc.Unicode

	if !fc.DisableMarkdown {
		msg = markdownReplacer.Replace(msg)

		blockquoteChar := "|"
		if unicode {
			blockquoteChar = "▕"
		}

		msg = utils.Markdown2irc(msg, blockquoteChar, fc.MarkdownInlineCode)
	} else {
		msg = strings.ReplaceAll(msg, "\n", " ")
	}

	if !fc.DisableEmoji {
		msg = utils.EmojiReplaceAliases(msg, fc.CustomEmoji)
	}

	return utils.FormatAndShortenSummary(msg, utils.SummaryOpts{
		DisableMarkdown: true,
		DisableEmoji:    true,
		MaxLength:       maxLen,
		UncountedPrefix: uncounted,
		Unicode:         unicode,
	})
}

func (r *RocketChat) sendEvent(ev *bridge.Event) {
	if !r.ready.Load() {
		return
	}

	select {
	case r.eventChan <- ev:
	case <-r.ctx.Done():
	}
}

func (r *RocketChat) senderInfo(ref rocketchatclient.UserRef) *bridge.UserInfo {
	if ref.ID != "" {
		if u := r.GetUser(r.ctx, ref.ID); u != nil {
			return u
		}
	}

	nick := cmp.Or(ref.Username, ref.ID)
	realName := cmp.Or(ref.Name, nick)

	return &bridge.UserInfo{
		Nick:      nick,
		User:      ref.ID,
		Real:      realName,
		Username:  ref.Username,
		FirstName: ref.Name,
		Ghost:     ref.ID != r.rc.UserID(),
		Me:        ref.ID == r.rc.UserID(),
	}
}

func (r *RocketChat) toBridgeUser(u *rocketchatclient.User) *bridge.UserInfo {
	if u == nil {
		return nil
	}

	nick := cmp.Or(u.Username, u.ID)
	realName := cmp.Or(u.Name, nick)
	meID := r.rc.UserID()

	roles := strings.Join(u.Roles, ",")
	if u.Type == "bot" && !slices.Contains(u.Roles, "bot") {
		if roles == "" {
			roles = "bot"
		} else {
			roles += ",bot"
		}
	}

	return &bridge.UserInfo{
		Nick:        nick,
		User:        u.ID,
		Real:        realName,
		Roles:       roles,
		DisplayName: u.Name,
		Ghost:       u.ID != meID,
		Me:          u.ID == meID,
		Username:    u.Username,
		FirstName:   u.Name,
		MentionKeys: mentionKeysForUser(u, nick, u.ID == meID),
	}
}

func mentionKeysForUser(u *rocketchatclient.User, nick string, me bool) []string {
	if !me || u == nil {
		return nil
	}

	candidates := []string{nick, u.Username, u.Name, u.ID}

	var keys []string

	for _, c := range candidates {
		c = strings.TrimSpace(c)
		if c != "" && !slices.Contains(keys, c) {
			keys = append(keys, c)
		}
	}

	return keys
}

// ChannelUserPrefix implements bridge.ChannelRolePrefixer for Rocket.Chat NAMES modes.
func (r *RocketChat) ChannelUserPrefix(channelID, userID string) string {
	roles := r.rc.RoomUserRoles(channelID, userID)
	if slices.Contains(roles, "owner") {
		return "@"
	}

	if slices.Contains(roles, "moderator") {
		return "+"
	}

	if u, ok := r.rc.User(userID); ok && slices.Contains(u.Roles, "admin") {
		return "@"
	}

	return ""
}

func (r *RocketChat) isOneToOneDM(room *rocketchatclient.Room) bool {
	if room == nil || room.Type != rocketchatclient.RoomTypeDirect {
		return false
	}

	if len(room.UIDs) == 2 {
		return true
	}

	return len(room.UIDs) == 1 && room.UIDs[0] == r.rc.UserID()
}

func (r *RocketChat) channelType(room *rocketchatclient.Room) string {
	if room == nil {
		return "O"
	}

	switch room.Type {
	case rocketchatclient.RoomTypeDirect:
		if r.isOneToOneDM(room) {
			return "D"
		}

		return "G"
	case rocketchatclient.RoomTypePrivate:
		return "P"
	default:
		return "O"
	}
}

func (r *RocketChat) roomName(room *rocketchatclient.Room) string {
	if room == nil {
		return ""
	}

	if r.isOneToOneDM(room) {
		return room.ID
	}

	if room.Type == rocketchatclient.RoomTypeDirect {
		return r.groupDMName(room)
	}

	return cmp.Or(room.Name, room.FName, room.ID)
}

func (r *RocketChat) groupDMName(room *rocketchatclient.Room) string {
	if name := cmp.Or(room.Name, room.FName); name != "" {
		return name
	}

	meID := r.rc.UserID()
	names := make([]string, 0, len(room.UIDs))

	for _, uid := range room.UIDs {
		if uid == meID {
			continue
		}

		if u, ok := r.rc.User(uid); ok && u.Username != "" {
			names = append(names, u.Username)
		} else {
			names = append(names, uid)
		}
	}

	if len(names) > 0 {
		slices.Sort(names)

		return "dm-" + strings.Join(names, "-")
	}

	return room.ID
}

func (r *RocketChat) toChannelInfo(room *rocketchatclient.Room) *bridge.ChannelInfo {
	if room == nil {
		return nil
	}

	var lastPost int64
	if !room.LastMessage.Time.IsZero() {
		lastPost = room.LastMessage.Time.UnixMilli()
	}

	return &bridge.ChannelInfo{
		Name:        r.roomName(room),
		ID:          room.ID,
		DM:          room.Type == rocketchatclient.RoomTypeDirect,
		Private:     room.Type == rocketchatclient.RoomTypePrivate,
		DisplayName: cmp.Or(room.FName, room.Name),
		LastPostAt:  lastPost,
	}
}

func (r *RocketChat) dmPeer(room *rocketchatclient.Room) *bridge.UserInfo {
	if !r.isOneToOneDM(room) {
		return nil
	}

	meID := r.rc.UserID()
	if len(room.UIDs) == 1 && room.UIDs[0] == meID {
		return r.GetMe()
	}

	for _, uid := range room.UIDs {
		if uid != meID {
			return r.GetUser(r.ctx, uid)
		}
	}

	return nil
}

func (r *RocketChat) Protocol() string {
	return protocolName
}

func (r *RocketChat) Connected() bool {
	return r.connected.Load() && r.rc != nil && r.rc.Connected()
}

func (r *RocketChat) Logout(ctx context.Context) error {
	r.connected.Store(false)

	if r.unregisterReloadHook != nil {
		r.unregisterReloadHook()
	}

	if r.rc != nil {
		r.rc.Logout()
	}

	r.cancel()

	return nil
}

func (r *RocketChat) Ping(ctx context.Context, proto ...string) error {
	if !r.Connected() {
		return errors.New("rocketchat: not connected")
	}

	return nil
}

func (r *RocketChat) GetMe() *bridge.UserInfo {
	if u := r.rc.Me(); u != nil {
		return r.toBridgeUser(u)
	}

	return &bridge.UserInfo{
		User: r.rc.UserID(),
		Me:   true,
	}
}

func (r *RocketChat) GetUsers() []*bridge.UserInfo {
	rcUsers := r.rc.Users()
	out := make([]*bridge.UserInfo, 0, len(rcUsers))

	for _, u := range rcUsers {
		out = append(out, r.toBridgeUser(u))
	}

	return out
}

func (r *RocketChat) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	if userID == "" {
		return nil
	}

	if u, ok := r.rc.User(userID); ok {
		return r.toBridgeUser(u)
	}

	u, err := r.rc.GetUserInfo(ctx, userID, "")
	if err != nil {
		return nil
	}

	return r.toBridgeUser(u)
}

func (r *RocketChat) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	if username == "" {
		return nil
	}

	if u, ok := r.rc.UserByUsername(username); ok {
		return r.toBridgeUser(u)
	}

	u, err := r.rc.GetUserInfo(ctx, "", username)
	if err != nil {
		return nil
	}

	return r.toBridgeUser(u)
}

func (r *RocketChat) GetUserCount() int {
	return len(r.rc.Users())
}

func (r *RocketChat) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	rcUsers := r.rc.Users()
	out := make([]*bridge.UserInfo, 0, len(rcUsers))

	for _, u := range rcUsers {
		if q == "" || strings.Contains(strings.ToLower(u.Username), q) || strings.Contains(strings.ToLower(u.Name), q) {
			out = append(out, r.toBridgeUser(u))
		}
	}

	return out, nil
}

func (r *RocketChat) GetChannels() []*bridge.ChannelInfo {
	rooms := r.rc.Rooms()
	out := make([]*bridge.ChannelInfo, 0, len(rooms))

	for _, room := range rooms {
		if _, subbed := r.rc.Subscription(room.ID); !subbed {
			continue
		}

		out = append(out, r.toChannelInfo(room))
	}

	return out
}

func (r *RocketChat) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return nil, errors.New("channel not found")
	}

	return r.toChannelInfo(room), nil
}

func (r *RocketChat) GetChannelCount() int {
	return len(r.rc.Subscriptions())
}

func (r *RocketChat) GetChannelName(ctx context.Context, channelID string) string {
	if channelID == "" || strings.HasPrefix(channelID, "&") || strings.HasPrefix(channelID, "#") {
		return channelID
	}

	room, ok := r.rc.Room(channelID)
	if !ok {
		return ""
	}

	if r.isOneToOneDM(room) {
		return room.ID
	}

	return "#" + r.roomName(room)
}

func (r *RocketChat) GetChannelID(ctx context.Context, name, teamID string) string {
	clean := strings.TrimPrefix(name, "#")

	for _, room := range r.rc.Rooms() {
		if room.ID == clean || r.roomName(room) == clean {
			return room.ID
		}
	}

	if strings.HasPrefix(name, "#") {
		return ""
	}

	if uid, ok := r.resolveUserID(ctx, clean); ok {
		return r.GetDMChannelID(ctx, uid)
	}

	return ""
}

func (r *RocketChat) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	members, err := r.rc.GetRoomMembers(ctx, channelID)
	if err == nil {
		out := make([]*bridge.UserInfo, 0, len(members))
		for _, m := range members {
			if u := r.toBridgeUser(m); u != nil {
				out = append(out, u)
			}
		}

		return out, nil
	}

	room, ok := r.rc.Room(channelID)
	if !ok {
		return nil, err
	}

	if len(room.UIDs) == 0 {
		return nil, err
	}

	out := make([]*bridge.UserInfo, 0, len(room.UIDs))
	for _, uid := range room.UIDs {
		if u := r.GetUser(ctx, uid); u != nil {
			out = append(out, u)
		}
	}

	return out, nil
}

func (r *RocketChat) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	var channels []string

	meID := r.rc.UserID()

	for _, room := range r.rc.Rooms() {
		if room.Type == rocketchatclient.RoomTypeDirect {
			continue
		}

		if userID == meID {
			if _, ok := r.rc.Subscription(room.ID); ok {
				channels = append(channels, "#"+r.roomName(room))
			}

			continue
		}

		if slices.Contains(room.UIDs, userID) {
			channels = append(channels, "#"+r.roomName(room))
		}
	}

	return channels, nil
}

func (r *RocketChat) IsChannelMember(channelID string) bool {
	_, ok := r.rc.Subscription(channelID)

	return ok
}

func (r *RocketChat) List(ctx context.Context) (map[string]string, error) {
	rooms := r.rc.Rooms()
	out := make(map[string]string, len(rooms))

	for _, room := range rooms {
		if room.Type == rocketchatclient.RoomTypeDirect {
			continue
		}

		out["#"+r.roomName(room)] = room.Topic
	}

	return out, nil
}

func (r *RocketChat) Topic(ctx context.Context, channelID string) string {
	room, ok := r.rc.Room(channelID)
	if !ok {
		return ""
	}

	return room.Topic
}

func (r *RocketChat) GetDMChannelID(ctx context.Context, userID string) string {
	meID := r.rc.UserID()

	for _, room := range r.rc.Rooms() {
		if !r.isOneToOneDM(room) {
			continue
		}

		if userID == meID && len(room.UIDs) == 1 && room.UIDs[0] == meID {
			return room.ID
		}

		if slices.Contains(room.UIDs, userID) && slices.Contains(room.UIDs, meID) {
			return room.ID
		}
	}

	return ""
}

func (r *RocketChat) GetDMChannelName(userID1 string, userID2 string) string {
	meID := r.rc.UserID()

	target := userID2
	if target == meID {
		target = userID1
	}

	return r.GetDMChannelID(r.ctx, target)
}

func (r *RocketChat) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return nil
	}

	return r.dmPeer(room)
}

func (r *RocketChat) GetDMUserIDs(channelName string) (string, string, bool) {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return "", "", false
	}

	peer := r.dmPeer(room)
	if peer == nil {
		return "", "", false
	}

	return r.rc.UserID(), peer.User, true
}

func (r *RocketChat) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	room, ok := r.rc.Room(channelName)
	if !ok {
		return "", false
	}

	peer := r.dmPeer(room)
	if peer == nil {
		return "", false
	}

	return peer.User, true
}

func (r *RocketChat) IsDMChannelName(channelName string) bool {
	room, ok := r.rc.Room(channelName)

	return ok && r.isOneToOneDM(room)
}

func (r *RocketChat) StatusUser(ctx context.Context, userID string) (string, error) {
	u, ok := r.rc.User(userID)
	if !ok || u.Status == "" {
		return rocketchatclient.StatusOffline, nil
	}

	if u.StatusText != "" {
		return u.Status + ": " + u.StatusText, nil
	}

	return u.Status, nil
}

func (r *RocketChat) StatusUsers(ctx context.Context) (map[string]string, error) {
	rcUsers := r.rc.Users()
	out := make(map[string]string, len(rcUsers))

	for _, u := range rcUsers {
		if u.Status != "" {
			out[u.ID] = u.Status
		}
	}

	return out, nil
}

func (r *RocketChat) IsOnline(status string) bool {
	return status == rocketchatclient.StatusOnline || strings.HasPrefix(status, rocketchatclient.StatusOnline+": ")
}

func (r *RocketChat) HasCustomStatus(userID string) bool {
	u, ok := r.rc.User(userID)

	return ok && u.StatusText != ""
}

func (r *RocketChat) Config() any {
	return r.cfg.RocketChat()
}

func (r *RocketChat) BridgeConfig() *config.BridgeConfig {
	return &r.cfg.RocketChat().Bridge
}

func (r *RocketChat) FormatterConfig() *config.FormatterConfig {
	return &r.cfg.RocketChat().Formatter
}

func (r *RocketChat) GetPostSizeLimit() int {
	return defaultPostSizeLimit
}

func (r *RocketChat) GetLastSentMsgs() []string {
	keys := r.msgLastSentCache.Keys()
	out := make([]string, 0, len(keys))

	for _, k := range slices.Backward(keys) {
		if v, ok := r.msgLastSentCache.Get(k); ok {
			out = append(out, k+" "+v)
		}
	}

	return out
}

func (r *RocketChat) NormalizePostID(msgID string) string {
	return strings.TrimSpace(msgID)
}

func (r *RocketChat) GetTeamName(ctx context.Context, teamID string) string {
	return ""
}

func (r *RocketChat) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	if channelID == "" {
		return 0
	}

	sub, ok := r.rc.Subscription(channelID)
	if !ok || sub.LastSeen.Time.IsZero() {
		return 0
	}

	return sub.LastSeen.Time.UnixMilli()
}

func (r *RocketChat) UpdateChannels(ctx context.Context) error {
	return nil
}

func (r *RocketChat) UpdateLastViewed(ctx context.Context, channelID string) {
	if channelID == "" {
		return
	}

	err := r.rc.MarkRead(ctx, channelID)
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Errorf("rocketchat: UpdateLastViewed %s: %s", channelID, err)
	}
}

func (r *RocketChat) UpdateLastViewedUser(ctx context.Context, userID string) error {
	if userID == "" {
		return errors.New("rocketchat: userID is required")
	}

	roomID := r.GetDMChannelID(ctx, userID)
	if roomID == "" {
		if resolved, ok := r.resolveUserID(ctx, userID); ok {
			roomID = r.GetDMChannelID(ctx, resolved)
		}
	}

	if roomID == "" {
		return errors.New("rocketchat: direct room not found for user " + userID)
	}

	return r.rc.MarkRead(ctx, roomID)
}

func (r *RocketChat) SendTyping(ctx context.Context, channelName string) {}

func (r *RocketChat) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	files := make([]*bridge.File, 0, len(fileIDs))

	for _, id := range fileIDs {
		fileURL := r.rc.DownloadURL(id, "")
		if fileURL != "" {
			files = append(files, &bridge.File{Name: id, URL: fileURL})
		}
	}

	return files
}

func (r *RocketChat) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return r.historySince(ctx, channelID, since, "scrollback")
}

func (r *RocketChat) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	if channelID == "" {
		return []*bridge.Event{}
	}

	msgs, err := r.rc.GetHistory(ctx, channelID, limit, time.Time{})
	if err != nil {
		logger.Errorf("rocketchat: GetPosts %s: %s", channelID, err)

		return []*bridge.Event{}
	}

	return r.msgsToEvents(msgs, "scrollback", 0)
}

func (r *RocketChat) GetPostChannelID(ctx context.Context, postID string) string {
	if postID == "" {
		return ""
	}

	if roomID, ok := r.msgRoomCache.Get(postID); ok {
		return roomID
	}

	msg, err := r.rc.ResolveMessage(ctx, postID)
	if err != nil || msg == nil {
		return ""
	}

	r.cacheMessageMeta(msg)

	return msg.RoomID
}

func (r *RocketChat) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	if postID == "" {
		return []*bridge.Event{}
	}

	msgs, err := r.rc.GetThreadMessages(ctx, postID)
	if err != nil {
		logger.Errorf("rocketchat: GetPostThread %s: %s", postID, err)

		return []*bridge.Event{}
	}

	return r.msgsToEvents(msgs, "details", 0)
}

func (r *RocketChat) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return r.historySince(ctx, channelID, since, "replay")
}

func (r *RocketChat) historySince(ctx context.Context, channelID string, since int64, eventType string) []*bridge.Event {
	if channelID == "" {
		return []*bridge.Event{}
	}

	var oldest time.Time
	if since > 0 {
		oldest = time.UnixMilli(since)
	}

	msgs, err := r.rc.GetHistory(ctx, channelID, maxHistoryFetchLimit, oldest)
	if err != nil {
		logger.Errorf("rocketchat: fetching history for %s: %s", channelID, err)

		// nil, unlike an empty slice, tells replay that the fetch failed.
		return nil
	}

	return r.msgsToEvents(msgs, eventType, since)
}

func (r *RocketChat) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	query := strings.TrimSpace(search)
	if query == "" {
		return []*bridge.Event{}
	}

	if events, handled := r.searchScopedChannel(ctx, query); handled {
		return events
	}

	roomIDs := r.searchRoomIDs()
	if len(roomIDs) == 0 {
		return []*bridge.Event{}
	}

	searchCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()

	all := r.collectSearchMatches(searchCtx, roomIDs, query)

	slices.SortStableFunc(all, func(a, b rocketchatclient.Message) int {
		if c := a.Timestamp.Time.Compare(b.Timestamp.Time); c != 0 {
			return c
		}

		return cmp.Compare(a.ID, b.ID)
	})

	return r.msgsToEvents(all, "search", 0)
}

// searchScopedChannel handles optional "#channel <query>" searches because Rocket.Chat chat.search requires a roomId.
func (r *RocketChat) searchScopedChannel(ctx context.Context, query string) ([]*bridge.Event, bool) {
	if !strings.HasPrefix(query, "#") {
		return nil, false
	}

	chTok, rest, ok := strings.Cut(query, " ")
	rest = strings.TrimSpace(rest)

	if !ok || rest == "" {
		return nil, false
	}

	roomID := r.GetChannelID(ctx, chTok, "")
	if roomID == "" {
		return nil, false
	}

	msgs, err := r.rc.SearchMessages(ctx, roomID, rest, 100)
	if err != nil {
		logger.Errorf("rocketchat: SearchPosts %s: %s", chTok, err)

		return []*bridge.Event{}, true
	}

	return r.msgsToEvents(msgs, "search", 0), true
}

func (r *RocketChat) collectSearchMatches(ctx context.Context, roomIDs []string, query string) []rocketchatclient.Message {
	seen := make(map[string]struct{})

	var all []rocketchatclient.Message

	for _, roomID := range roomIDs {
		msgs, err := r.rc.SearchMessages(ctx, roomID, query, defaultSearchCount)
		if err != nil {
			if ctx.Err() != nil {
				break
			}

			logger.Debugf("rocketchat: chat.search %s: %s", roomID, err)

			continue
		}

		for i := range msgs {
			m := &msgs[i]
			if m.ID == "" {
				continue
			}

			if _, exists := seen[m.ID]; exists {
				continue
			}

			seen[m.ID] = struct{}{}
			all = append(all, *m)
		}
	}

	return all
}

func (r *RocketChat) searchRoomIDs() []string {
	subs := r.rc.Subscriptions()
	if len(subs) == 0 {
		return nil
	}

	slices.SortFunc(subs, func(a, b *rocketchatclient.Subscription) int {
		if a.Open != b.Open {
			if a.Open {
				return -1
			}

			return 1
		}

		return b.UpdatedAt.Time.Compare(a.UpdatedAt.Time)
	})

	limit := min(len(subs), maxSearchRooms)
	roomIDs := make([]string, 0, limit)

	for i := range limit {
		if subs[i].RoomID != "" {
			roomIDs = append(roomIDs, subs[i].RoomID)
		}
	}

	return roomIDs
}

func (r *RocketChat) msgsToEvents(msgs []rocketchatclient.Message, eventType string, since int64) []*bridge.Event {
	events := make([]*bridge.Event, 0, len(msgs))

	for i := range msgs {
		if ev := r.msgToEvent(&msgs[i], eventType, since); ev != nil {
			events = append(events, ev)
		}
	}

	return events
}

func (r *RocketChat) msgToEvent(msg *rocketchatclient.Message, eventType string, since int64) *bridge.Event {
	if msg == nil || msg.ID == "" || msg.RoomID == "" {
		return nil
	}

	if msg.Type != "" && msg.Type != "me" {
		return nil
	}

	createAt := msg.Timestamp.Time.UnixMilli()
	if since > 0 && createAt <= since {
		return nil
	}

	r.cacheMessageMeta(msg)

	room, ok := r.rc.Room(msg.RoomID)
	isDM := ok && r.isOneToOneDM(room)
	text, msgType := r.formatIncomingText(msg, isDM)
	files := r.extractFiles(msg)

	if text == "" && len(files) == 0 {
		return nil
	}

	return r.buildHistoricalEvent(room, isDM, msg, eventType, msgType, text, files, createAt)
}

func (r *RocketChat) buildHistoricalEvent(room *rocketchatclient.Room, isDM bool, msg *rocketchatclient.Message, eventType, msgType, text string, files []*bridge.File, createAt int64) *bridge.Event {
	sender := r.senderInfo(msg.User)

	if isDM {
		peer := r.dmPeer(room)
		if peer == nil {
			peer = sender
		}

		return &bridge.Event{
			Type: "direct_message",
			Data: &bridge.DirectMessageEvent{
				Text:      text,
				ChannelID: msg.RoomID,
				Receiver:  peer,
				Sender:    sender,
				Files:     files,
				MessageID: msg.ID,
				Event:     eventType,
				ParentID:  msg.ThreadID,
				CreateAt:  createAt,
			},
		}
	}

	return &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:        text,
			ChannelID:   msg.RoomID,
			Sender:      sender,
			MessageType: msgType,
			ChannelType: r.channelType(room),
			Files:       files,
			MessageID:   msg.ID,
			Event:       eventType,
			ParentID:    msg.ThreadID,
			CreateAt:    createAt,
		},
	}
}

func (r *RocketChat) resolveUserID(ctx context.Context, userIDOrName string) (string, bool) {
	if userIDOrName == "" {
		return "", false
	}

	if u, ok := r.rc.User(userIDOrName); ok {
		return u.ID, true
	}

	if u := r.GetUserByUsername(ctx, strings.TrimPrefix(userIDOrName, "@")); u != nil && u.User != "" {
		return u.User, true
	}

	if u := r.GetUser(ctx, userIDOrName); u != nil && u.User != "" {
		return u.User, true
	}

	return "", false
}

func (r *RocketChat) Invite(ctx context.Context, channelID, username string) error {
	userID, ok := r.resolveUserID(ctx, username)
	if !ok {
		return errors.New("rocketchat: unknown user " + username)
	}

	return r.rc.InviteUser(ctx, channelID, userID)
}

func (r *RocketChat) Join(ctx context.Context, channelName string) (string, string, error) {
	channelID := r.GetChannelID(ctx, channelName, "")

	room, err := r.rc.JoinChannel(ctx, channelID, channelName)
	if err != nil {
		return "", "", err
	}

	return room.ID, room.Topic, nil
}

func (r *RocketChat) Part(ctx context.Context, channel string) error {
	return r.rc.LeaveRoom(ctx, channel)
}

func (r *RocketChat) SetTopic(ctx context.Context, channelID, text string) error {
	return r.rc.SetTopic(ctx, channelID, text)
}

func (r *RocketChat) Kick(ctx context.Context, channelID, username string) error {
	userID, ok := r.resolveUserID(ctx, username)
	if !ok {
		return errors.New("rocketchat: unknown user " + username)
	}

	return r.rc.KickUser(ctx, channelID, userID)
}

func (r *RocketChat) Nick(ctx context.Context, name string) error {
	return errors.New("rocketchat: Nick not implemented yet")
}

func (r *RocketChat) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return r.MsgUserThread(ctx, userID, "", text)
}

func (r *RocketChat) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	roomID := r.GetDMChannelID(ctx, userID)
	if roomID == "" {
		u := r.GetUser(ctx, userID)
		if u == nil || u.Username == "" {
			return "", errors.New("rocketchat: direct room not found for user " + userID)
		}

		room, err := r.rc.CreateDM(ctx, u.Username)
		if err != nil {
			return "", err
		}

		roomID = room.ID
	}

	return r.MsgChannelThread(ctx, roomID, parentID, text)
}

func (r *RocketChat) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return r.MsgChannelThread(ctx, channelID, "", text)
}

func (r *RocketChat) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	sendText := rocketChatActionText(text)

	msgID, err := r.rc.SendMessage(ctx, channelID, sendText, parentID)
	if err != nil {
		return "", err
	}

	r.recordSent(ctx, channelID, msgID, parentID, sendText)

	return msgID, nil
}

func rocketChatActionText(text string) string {
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		return "_" + strings.TrimSuffix(after, "\x01") + "_"
	}

	return text
}

func (r *RocketChat) recordSent(ctx context.Context, channelID, msgID, parentID, text string) {
	if msgID == "" {
		return
	}

	r.msgRoomCache.Add(msgID, channelID)

	if short := r.shortText(text); short != "" {
		r.msgTextCache.Add(msgID, short)
	}

	name := cmp.Or(r.GetChannelName(ctx, channelID), channelID)
	summary := r.formatSummary(text, lastSentSummaryLen, "@")

	if parentID == "" {
		r.msgLastSentCache.Add(msgID, name+": "+summary)

		return
	}

	if !r.cfg.RocketChat().HideReplies {
		summary = r.withParentPrefix(parentID, summary)
	}

	r.msgLastSentCache.Add(parentID, name+": "+summary)
}

func (r *RocketChat) ModifyPost(ctx context.Context, msgID, text string) error {
	roomID := r.GetPostChannelID(ctx, msgID)
	if roomID == "" {
		return errors.New("rocketchat: unknown message " + msgID)
	}

	r.msgParentCache.Remove(msgID)

	if text == "" {
		err := r.rc.DeleteMessage(ctx, roomID, msgID)
		if err != nil {
			return err
		}

		r.msgTextCache.Remove(msgID)
		r.msgAuthorCache.Remove(msgID)

		return nil
	}

	err := r.rc.UpdateMessage(ctx, roomID, msgID, text)
	if err != nil {
		return err
	}

	if short := r.shortText(text); short != "" {
		r.msgTextCache.Add(msgID, short)
	}

	return nil
}

func (r *RocketChat) AddReaction(ctx context.Context, msgID, emoji string) error {
	return r.rc.React(ctx, msgID, emoji, true)
}

func (r *RocketChat) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	return r.rc.React(ctx, msgID, emoji, false)
}

func (r *RocketChat) SetStatus(ctx context.Context, status string) error {
	normalized := strings.ToLower(strings.TrimSpace(status))

	switch normalized {
	case rocketchatclient.StatusOnline, rocketchatclient.StatusAway, rocketchatclient.StatusBusy, rocketchatclient.StatusOffline:
	case "":
		return errors.New("rocketchat: status is required")
	default:
		normalized = rocketchatclient.StatusAway
	}

	return r.rc.SetStatus(ctx, normalized, "")
}

func (r *RocketChat) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	private := strings.EqualFold(channelType, "P") || strings.EqualFold(channelType, "private")

	room, err := r.rc.CreateRoom(ctx, channelName, private)
	if err != nil {
		return nil, err
	}

	return r.toChannelInfo(room), nil
}
