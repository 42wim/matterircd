package matrix

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/42wim/matterircd/utils"
	lru "github.com/hashicorp/golang-lru/v2"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/matrixclient"
	"github.com/sirupsen/logrus"
)

// defaultScrollbackLimit is the number of messages shown when none is requested.
const defaultScrollbackLimit = 60

// maxScrollbackLimit is the most messages one scrollback request fetches.
const maxScrollbackLimit = 1000

// logger is shared by all users, so creating it once avoids racing on a
// per-login replacement. It matches the format of the matterclient logging.
var logger = newLogger()

func newLogger() *logrus.Entry {
	l := logrus.New()
	l.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})

	return l.WithFields(logrus.Fields{"prefix": "bridge/matrix"})
}

const (
	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"

	lastSentSummaryLen = 90

	searchUsersLimit   = 50
	msgRoomCacheSize   = 2000
	msgTextCacheSize   = 2000
	fetchTextTimeout   = 2 * time.Second
	pinFetchMax        = 3
	pinFetchWindow     = 10 * time.Second
	editIDCacheSize    = 500
	msgTextMaxBytes    = 1024 // bound per cached message when ShortenRepliesTo is unlimited
	reactionCacheSize  = 100
	sentCacheSize      = 100
	sentReactionWindow = time.Minute

	roomNameResolveTimeout = 5 * time.Second
)

// markdownReplacer combines multi-line messages into a single line, making
// code blocks single code/monospace.
var markdownReplacer = strings.NewReplacer(
	"\n", " ",
	"```", "`",
	"~~~", "`",
)

// roomUpdate is a queued room change, see queueRoomUpdate.
type roomUpdate struct {
	ev   *bridge.ChannelUpdateEvent
	kind matrixclient.RoomRenameKind
}

type Matrix struct {
	mc        *matrixclient.Client
	eventChan chan *bridge.Event
	cfg       *config.Config
	nickSep   string

	// Fixed for the session, like nickSep: matrixclient is started with it.
	defaultProvider string
	onConnect       func()

	connected atomic.Bool
	e2ee      atomic.Bool
	ready     atomic.Bool
	readyCh   chan struct{} // closed once the initial sync was processed

	// nameResolveTried holds when a room name was last looked up synchronously, see
	// nameResolveDue, so an unreachable homeserver cannot stall every lookup.
	nameResolveTried sync.Map
	nameBudget       nameBudget

	// roomNamesMu guards roomNames, the channel names given to rooms, see assignRoomNames.
	roomNamesMu sync.Mutex
	roomNames   map[id.RoomID]roomName

	// roomUpdateMu guards the renames waiting for roomUpdateLoop, see queueRoomUpdate.
	roomUpdateMu     sync.Mutex
	roomUpdates      []*roomUpdate
	roomUpdateWake   chan struct{}
	roomUpdateCancel context.CancelFunc

	// unregisterReloadHook removes the hook that New registered on the config.
	unregisterReloadHook func()

	ctx    context.Context //nolint:containedctx
	cancel context.CancelFunc

	// initDone is closed once New published mc, as matrixclient.New starts syncing
	// before it returns and Ready may be called by then.
	initDone chan struct{}

	msgParentCache   *lru.Cache[id.EventID, string]
	msgLastSentCache *lru.Cache[string, string]
	msgRoomCache     *lru.Cache[id.EventID, id.RoomID]
	msgTextCache     *lru.Cache[id.EventID, string]   // first line of recent messages, shown when they are deleted
	editEventIDs     *lru.Cache[id.EventID, id.EventID] // replacement event to edited message, whose redaction is not a deleted message
	reactionCache    *lru.Cache[string, id.EventID]
	sentReactions    *lru.Cache[string, time.Time]    // reactions added from IRC, whose echo is dropped
	sentRedactions   *lru.Cache[id.EventID, struct{}] // messages deleted from IRC, whose echo is dropped

	pinFetchMu    sync.Mutex
	pinFetchStart time.Time
	pinFetchCount int

	// sentMu is never held across network I/O. It guards sentCache and pendingSends,
	// which together recognize the echo of a message sent from IRC, also when the
	// echo is synced before the send call returned its event ID.
	sentMu       sync.Mutex
	sentCache    *lru.Cache[id.EventID, struct{}]
	pendingSends map[string]int // text of the sends in flight

	// verifyMu guards verifyTxn, the verification the VERIFY commands act on.
	verifyMu    sync.Mutex
	verifyTxn   id.VerificationTransactionID
	verifySAS   bool // the SAS of verifyTxn was shown, which VERIFY CONFIRM requires
	verifyQueue chan *matrixclient.Verification
}

// resolveNickSep returns the configured separator and the one to use: an invalid
// separator falls back to "[]", and ':' is allowed with a warning.
func resolveNickSep(configured string) (string, string) {
	requested := cmp.Or(configured, "[]")
	sep := requested

	if !validNickSep(sep) {
		logger.Warnf("matrix.ReplaceColonChar %q is invalid (only punctuation that is not valid in a user ID or reserved in IRC nicks is allowed), using \"[]\"", sep)

		sep = "[]"
	}

	if strings.Contains(sep, ":") {
		logger.Warnf("matrix.ReplaceColonChar contains ':', which is not valid in IRC nicks and may break some IRC clients or bouncers. Use at your own risk")
	}

	return requested, sep
}

// sessionSettingsHook sets the log levels on reload and logs, once per new value, that
// settings fixed for the session changed and need a reconnect to take effect.
func sessionSettingsHook(sep, provider string, setLogLevels func(rc *config.RuntimeConfig)) func(rc *config.RuntimeConfig) {
	var lastSep, lastProvider atomic.Value

	lastSep.Store(sep)
	lastProvider.Store(provider)

	return func(rc *config.RuntimeConfig) {
		setLogLevels(rc)

		newSep := cmp.Or(rc.Matrix.ReplaceColonChar, "[]")
		if lastSep.Swap(newSep) != newSep && newSep != sep {
			logger.Infof("matrix.ReplaceColonChar changed from %q to %q, a reconnect (disconnect + connect) is required for it to take effect", sep, newSep)
		}

		newProvider := rc.Matrix.DefaultProvider
		if lastProvider.Swap(newProvider) != newProvider && newProvider != provider {
			logger.Infof("matrix.DefaultProvider changed from %q to %q, a reconnect (disconnect + connect) is required for it to take effect", provider, newProvider)
		}
	}
}

//nolint:funlen
func New(ctx context.Context, cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, *matrixclient.Client, error) {
	// The lifetime of this bridge, so Logout can release goroutines blocked on eventChan.
	bridgeCtx, cancel := context.WithCancel(ctx)

	// Fixed for the session: config reloads would otherwise change nicks without renaming existing ghosts.
	requestedSep, nickSep := resolveNickSep(cfg.Matrix().ReplaceColonChar)

	defaultProvider := cfg.Matrix().DefaultProvider

	m := &Matrix{
		nickSep:         nickSep,
		defaultProvider: defaultProvider,
		eventChan:       eventChan,
		cfg:             cfg,
		onConnect:       onConnect,
		ctx:             bridgeCtx,
		cancel:          cancel,
		initDone:        make(chan struct{}),
		readyCh:         make(chan struct{}),

		roomUpdateWake: make(chan struct{}, 1),
		verifyQueue:    make(chan *matrixclient.Verification, verificationQueueSize),
	}

	// Also unblocks a Ready that is waiting for a failed New.
	defer close(m.initDone)

	m.msgParentCache, _ = lru.New[id.EventID, string](100)
	m.msgLastSentCache, _ = lru.New[string, string](10)
	m.msgRoomCache, _ = lru.New[id.EventID, id.RoomID](msgRoomCacheSize)
	m.msgTextCache, _ = lru.New[id.EventID, string](msgTextCacheSize)
	m.editEventIDs, _ = lru.New[id.EventID, id.EventID](editIDCacheSize)
	m.pendingSends = make(map[string]int)
	m.sentCache, _ = lru.New[id.EventID, struct{}](sentCacheSize)
	m.sentReactions, _ = lru.New[string, time.Time](reactionCacheSize)
	m.sentRedactions, _ = lru.New[id.EventID, struct{}](sentCacheSize)
	m.reactionCache, _ = lru.New[string, id.EventID](reactionCacheSize)

	if cred.Token != "" && cred.Server == "" {
		cancel()

		return nil, nil, errors.New("matrix: login with an access token needs a server, use LOGIN <server> <token>")
	}

	opts, err := m.encryptionOptions(cred)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	if provider := m.defaultProvider; provider != "" {
		opts = append(opts, matrixclient.WithDefaultProvider(provider))
	}

	opts = append(opts, matrixclient.WithRoomNameResolved(m.onRoomNameResolved))

	mc, err := matrixclient.New(ctx, matrixclient.Credentials{
		Server: cred.Server,
		Login:  cred.Login,
		Pass:   cred.Pass,
		Token:  cred.Token,

		DeviceName: "matterircd",
	}, m, opts...)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	m.mc = mc
	m.e2ee.Store(mc.Encrypted())

	var loopCtx context.Context

	loopCtx, m.roomUpdateCancel = context.WithCancel(ctx)

	go m.roomUpdateLoop(loopCtx)

	setLogLevels := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			logger.Logger.SetLevel(logrus.TraceLevel)
		case rc.Debug:
			logger.Logger.SetLevel(logrus.DebugLevel)
		default:
			logger.Logger.SetLevel(logrus.InfoLevel)
		}

		mc.SetLogLevel("info")
		mc.SetLogAPICalls("error")

		if rc.Profiling {
			if rc.Trace {
				logger.Infof("enabling matrixclient API logging: level: trace")
				mc.SetLogAPICalls("trace")
			} else if rc.Debug {
				logger.Infof("enabling matrixclient API logging: level: warn")
				mc.SetLogAPICalls("warn")
			}
		}
	}

	setLogLevels(cfg.Current())

	// The hook keeps this client alive for as long as it is registered, so Logout
	// has to remove it again.
	m.unregisterReloadHook = cfg.RegisterReloadHook(sessionSettingsHook(requestedSep, defaultProvider, setLogLevels))
	m.connected.Store(true)

	return m, mc, nil
}

// Ready implements matrixclient.Handler.
func (m *Matrix) Ready() {
	select {
	case <-m.initDone:
	case <-m.ctx.Done():
		return
	}

	if m.mc == nil {
		return
	}

	m.ready.Store(true)
	close(m.readyCh)

	// Started after ready is set, as sendEventCtx drops notices before that.
	go m.verificationLoop()

	// Invitations of the initial sync are not delivered to OnInvite. The snapshot
	// is taken here, on the sync goroutine, so a later invite cannot be missed or
	// duplicated, and sent from another goroutine as the event loop starts after
	// onConnect.
	invites := m.mc.Invitations()

	go m.onConnect()

	if len(invites) > 0 {
		go func() {
			for i := range invites {
				m.sendInvite(&invites[i])
			}
		}()
	}
}

func (m *Matrix) sendEvent(ev *bridge.Event) {
	m.sendEventCtx(m.ctx, ev)
}

// sendEventCtx is sendEvent that also gives up when ctx is done.
func (m *Matrix) sendEventCtx(ctx context.Context, ev *bridge.Event) {
	// The event loop only starts from onConnect, which Ready triggers. Blocking
	// the sync goroutine on the unbuffered channel before that would deadlock the
	// initial sync, so its events are dropped, history is replayed on join.
	if !m.ready.Load() {
		return
	}

	select {
	case m.eventChan <- ev:
	case <-ctx.Done():
	case <-m.ctx.Done():
	}
}

// OnMessage implements matrixclient.Handler.
func (m *Matrix) OnMessage(msg *matrixclient.Message) {
	ghost := m.createUser(msg.Sender)
	if ghost == nil {
		return
	}

	// Cached before the echo check, as messages sent from IRC can be deleted or pinned too.
	m.cacheText(msg.EventID, msg.Text)

	if ghost.Me {
		if m.isSentEcho(msg.EventID, msg.Text) {
			return
		}
	}

	m.msgRoomCache.Add(msg.EventID, msg.RoomID)

	text := msg.Text

	file := m.fileFromMessage(msg)
	if file != nil {
		m.sendFile(ghost, msg, file)

		if !hasCaption(msg) {
			return
		}
	}

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies && msg.ParentID != "" {
		message, err := m.addParentMsg(m.ctx, msg.RoomID, msg.ParentID, text, mxCfg.ShortenRepliesTo, "@")
		if err != nil {
			logger.Errorf("Unable to get parent post for %s: %s", msg.EventID, err)
		}

		text = message
	}

	m.sendText(ghost, msg.RoomID, msg.Direct, msg.EventID, msg.ParentID, text, "", m.messageType(text), msg.Timestamp)
}

func (m *Matrix) sendFile(ghost *bridge.UserInfo, msg *matrixclient.Message, file *bridge.File) {
	channelType := ""
	if msg.Direct {
		channelType = "D"
	}

	receiver := m.dmReceiver(ghost, msg.RoomID, msg.Direct)
	if receiver == nil {
		return
	}

	m.sendEvent(&bridge.Event{
		Type: "file_event",
		Data: &bridge.FileEvent{
			Receiver:    receiver,
			Sender:      ghost,
			ChannelID:   msg.RoomID.String(),
			ChannelType: channelType,
			Files:       []*bridge.File{file},
			MessageID:   msg.EventID.String(),
			ParentID:    msg.ParentID.String(),
		},
	})
}

// OnReaction implements matrixclient.Handler.
func (m *Matrix) OnReaction(r *matrixclient.Reaction) {
	m.msgRoomCache.Add(r.ParentID, r.RoomID)

	ghost := m.createUser(r.Sender)
	if ghost == nil {
		return
	}

	if ghost.Me && (!m.cfg.Matrix().ShowOwnReactions || m.isSentReaction(r.ParentID, r.Key)) {
		return
	}

	var text string

	mxCfg := m.cfg.Matrix()
	if !mxCfg.HideReplies {
		message, err := m.addParentMsg(m.ctx, r.RoomID, r.ParentID, text, mxCfg.ShortenRepliesTo, "@")
		if err != nil {
			logger.Errorf("Unable to get parent post for %s: %s", r.ParentID, err)
		}

		text = message
	}

	channelType := ""
	if r.Direct {
		channelType = "D"
	}

	receiver := m.dmReceiver(ghost, r.RoomID, r.Direct)
	if receiver == nil {
		return
	}

	// The reacted message is the MessageID, a ParentID would show it as a reply to itself.
	m.sendEvent(&bridge.Event{
		Type: "reaction_add",
		Data: &bridge.ReactionAddEvent{
			ChannelID:   r.RoomID.String(),
			MessageID:   string(r.ParentID),
			Receiver:    receiver,
			Sender:      ghost,
			Reaction:    r.Key,
			ChannelType: channelType,
			Message:     text,
		},
	})
}

func (m *Matrix) Invite(ctx context.Context, channelID, username string) error {
	userID, ok := m.resolveUserID(username)
	if !ok {
		return errors.New("unknown user " + username)
	}

	return m.mc.Invite(ctx, id.RoomID(channelID), userID)
}

func (m *Matrix) IsChannelMember(channelID string) bool {
	for _, r := range m.mc.Rooms() {
		if r.ID.String() == channelID {
			return true
		}
	}

	return m.mc.IsDirect(id.RoomID(channelID))
}

func (m *Matrix) Join(ctx context.Context, channelName string) (string, string, error) {
	// "space/room" joins a room of a space we know of.
	if spaceName, name, ok := strings.Cut(channelName, "/"); ok && !strings.HasPrefix(channelName, "#") {
		if spaceID := m.mc.TeamByName(spaceName); spaceID != "" {
			channelID := m.GetChannelID(ctx, name, spaceID.String())
			if channelID == "" {
				return "", "", errors.New("room " + name + " not found in " + spaceName)
			}

			return channelID, spaceID.String(), nil
		}
	}

	// CmdJoin strips the leading '#' of an alias like "#room:server".
	target := channelName
	if !strings.HasPrefix(target, "!") && !strings.HasPrefix(target, "#") { //nolint:nestif
		// A listed name can contain a colon (a homeserver with a port), so known rooms come first.
		if channelID := m.GetChannelID(ctx, target, ""); channelID != "" {
			target = channelID
		} else {
			// Clients rejoin saved names that still carry the default provider. Keep it
			// explicit, as matrixclient appends the provider it was started with.
			if provider := m.defaultProvider; provider != "" {
				if name, ok := strings.CutSuffix(target, "/"+provider); ok {
					target = name + ":" + provider
				}
			}

			// Without a homeserver, Join appends the default provider.
			target = "#" + target
		}
	}

	roomID, err := m.mc.Join(ctx, target)
	if err != nil {
		return "", "", err
	}

	return roomID.String(), "", nil
}

func (m *Matrix) List(ctx context.Context) (map[string]string, error) {
	rooms := make(map[string]string)

	all, names := m.roomsWithNames()

	for _, room := range all {
		if room.IsDirect {
			continue
		}

		roomName := names[room.ID]

		name := "#" + roomName
		if teams := m.mc.RoomTeams(room.ID); len(teams) > 0 {
			name = m.GetTeamName(ctx, teams[0].String()) + "/" + roomName
		}

		rooms[name] = strings.ReplaceAll(m.mc.Topic(room.ID), "\n", " | ")
	}

	return rooms, nil
}

func (m *Matrix) Part(ctx context.Context, channelID string) error {
	m.nameResolveTried.Delete(id.RoomID(channelID))

	return m.mc.Leave(ctx, id.RoomID(channelID))
}

func (m *Matrix) UpdateChannels(ctx context.Context) error {
	return nil
}

func (m *Matrix) Logout(ctx context.Context) error {
	m.connected.Store(false)

	if m.unregisterReloadHook != nil {
		m.unregisterReloadHook()
	}

	m.mc.Logout()
	m.roomUpdateCancel()
	m.cancel()

	return nil
}

func (m *Matrix) MsgUser(ctx context.Context, userID, text string) (string, error) {
	return m.MsgUserThread(ctx, userID, "", text)
}

func (m *Matrix) MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error) {
	logger.Debugf("sending message '%s' '%s' '%s'", userID, parentID, text)

	uid, ok := m.resolveUserID(userID)
	if !ok {
		return "", errors.New("unknown user " + userID)
	}

	m.beginSend(text)

	eventID, err := m.mc.SendDirect(ctx, uid, id.EventID(parentID), text)
	m.endSend(text, eventID, err)

	if err != nil {
		logger.Error("msguserthread sending message: error ", err)
		return "", err
	}

	if roomID := m.mc.DirectRoom(uid); roomID != "" {
		m.msgRoomCache.Add(eventID, roomID)
	}

	return m.recordSent(ctx, "", eventID, text), nil
}

// isSentReaction reports, once, whether our own reaction was added from IRC.
func (m *Matrix) isSentReaction(parentID id.EventID, key string) bool {
	sentKey := parentID.String() + "\x00" + key

	at, ok := m.sentReactions.Get(sentKey)
	if !ok {
		return false
	}

	m.sentReactions.Remove(sentKey)

	return time.Since(at) < sentReactionWindow
}

func (m *Matrix) beginSend(text string) {
	m.sentMu.Lock()
	m.pendingSends[text]++
	m.sentMu.Unlock()
}

func (m *Matrix) endSend(text string, eventID id.EventID, err error) {
	m.sentMu.Lock()
	defer m.sentMu.Unlock()

	if m.pendingSends[text] <= 1 {
		delete(m.pendingSends, text)
	} else {
		m.pendingSends[text]--
	}

	if err == nil {
		m.sentCache.Add(eventID, struct{}{})
	}
}

// isSentEcho reports whether our own message was sent from IRC. A message with
// the text of a send in flight is taken as its echo, as the event ID isn't known yet.
func (m *Matrix) isSentEcho(eventID id.EventID, text string) bool {
	m.sentMu.Lock()
	defer m.sentMu.Unlock()

	if _, ok := m.sentCache.Get(eventID); ok {
		return true
	}

	return m.pendingSends[text] > 0
}

func (m *Matrix) recordSent(ctx context.Context, channelID string, eventID id.EventID, text string) string {
	name := channelID
	if channelID != "" {
		name = m.GetChannelName(ctx, channelID)
	}

	summary := m.formatSummary(text, lastSentSummaryLen, "@")
	m.msgLastSentCache.Add(eventID.String(), name+": "+summary)

	return eventID.String()
}

func (m *Matrix) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	return m.MsgChannelThread(ctx, channelID, "", text)
}

func (m *Matrix) MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error) {
	logger.Debugf("msgchannelthread: sending message thread '%s' '%s' '%s'", m.roomLabel(id.RoomID(channelID)), parentID, text)

	m.beginSend(text)

	eventID, err := m.mc.Send(ctx, id.RoomID(channelID), id.EventID(parentID), text)
	m.endSend(text, eventID, err)

	if err != nil {
		return "", err
	}

	m.msgRoomCache.Add(eventID, id.RoomID(channelID))

	return m.recordSent(ctx, channelID, eventID, text), nil
}

func (m *Matrix) ModifyPost(ctx context.Context, msgID, text string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	m.msgParentCache.Remove(id.EventID(msgID))

	if text == "" {
		m.sentRedactions.Add(id.EventID(msgID), struct{}{})

		err := m.mc.Delete(ctx, roomID, id.EventID(msgID))
		if err != nil {
			m.sentRedactions.Remove(id.EventID(msgID))
		}

		return err
	}

	m.beginSend(text)

	eventID, err := m.mc.Edit(ctx, roomID, id.EventID(msgID), text)
	m.endSend(text, eventID, err)

	return err
}

func (m *Matrix) Topic(ctx context.Context, channelID string) string {
	return m.mc.Topic(id.RoomID(channelID))
}

func (m *Matrix) SetTopic(ctx context.Context, channelID, text string) error {
	return m.mc.SetTopic(ctx, id.RoomID(channelID), text)
}

func (m *Matrix) StatusUser(ctx context.Context, userID string) (string, error) {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return "", nil
	}

	err := m.mc.UpdateStatus(ctx, uid)
	if err != nil {
		return "", err
	}

	prefix := m.mc.GetStatus(uid)
	localTime := m.mc.GetUserLocalTime(ctx, uid)

	// Presence activity is in milliseconds.
	lastActivity := m.mc.GetUserLastActivity(uid) / 1000

	var lastOnline string

	if lastActivity > 0 {
		diff := max(0, time.Now().Unix()-lastActivity)
		if prefix == matrixclient.StatusOnline {
			lastOnline = formatLastOnline(lastActivity, true)
		} else if diff >= 20*60 {
			lastOnline = formatLastOnline(lastActivity)
		}
	}

	var details string

	switch {
	case lastOnline != "" && localTime != "":
		details = lastOnline + ", local time: " + localTime
	case lastOnline != "":
		details = lastOnline
	case localTime != "":
		details = "local time: " + localTime
	}

	status := prefix
	if details != "" {
		status += " (" + details + ")"
	}

	if custom := m.mc.GetUserCustomStatus(uid); custom != "" {
		status += ": " + custom
	}

	return status, nil
}

func formatLastOnline(lastActivity int64, isOnline ...bool) string {
	if lastActivity <= 0 {
		return ""
	}

	label := "Last online"
	if len(isOnline) > 0 && isOnline[0] {
		label = "Last activity"
	}

	diff := max(0, time.Now().Unix()-lastActivity)

	switch {
	case diff < 45:
		return label + " just now"
	case diff < 3300: // < 55 minutes
		mins := max(1, (diff+30)/60)
		if mins == 1 {
			return label + " 1 min. ago"
		}

		return fmt.Sprintf("%s %d mins. ago", label, mins)
	case diff < 84600: // < 23.5 hours
		hours := (diff + 1800) / 3600
		if hours == 1 {
			return label + " 1 hr. ago"
		}

		return fmt.Sprintf("%s %d hr. ago", label, hours)
	case diff < 30*86400:
		days := (diff + 43200) / 86400
		if days == 1 {
			return label + " 1 day ago"
		}

		return fmt.Sprintf("%s %d days ago", label, days)
	default:
		return fmt.Sprintf("%s %s", label, time.Unix(lastActivity, 0).Format("2006-01-02"))
	}
}

func (m *Matrix) HasCustomStatus(userID string) bool {
	uid, ok := m.resolveUserID(userID)

	return ok && m.mc.GetUserCustomStatus(uid) != ""
}

func (m *Matrix) StatusUsers(ctx context.Context) (map[string]string, error) {
	statuses := make(map[string]string)

	for _, userID := range m.mc.KnownUsers() {
		statuses[userID.String()] = m.mc.GetStatus(userID)
	}

	return statuses, nil
}

func (m *Matrix) IsOnline(status string) bool {
	return status == "online" || (strings.HasPrefix(status, "online (") && !strings.Contains(status, "): "))
}

func (m *Matrix) Protocol() string {
	return "matrix"
}

func (m *Matrix) Kick(ctx context.Context, channelID, username string) error {
	userID, ok := m.resolveUserID(username)
	if !ok {
		return errors.New("unknown user " + username)
	}

	return m.mc.Kick(ctx, id.RoomID(channelID), userID, "")
}

func (m *Matrix) SetStatus(ctx context.Context, status string) error {
	return m.mc.SetStatus(ctx, status, "")
}

func (m *Matrix) Nick(ctx context.Context, name string) error {
	return m.mc.SetDisplayName(ctx, name)
}

func (m *Matrix) GetChannelName(ctx context.Context, channelID string) string {
	m.resolveRoomName(ctx, id.RoomID(channelID))

	channel, err := m.GetChannel(ctx, channelID)
	if err != nil {
		return channelID
	}

	if channel.DM {
		return channel.Name
	}

	return "#" + channel.Name
}

// resolveRoomName looks up a room known only by its ID before its channel is
// created, within a short deadline. Later failures are healed in the background.
func (m *Matrix) resolveRoomName(ctx context.Context, roomID id.RoomID) {
	timeout := min(roomNameResolveTimeout, m.nameBudget.remaining())
	if timeout <= 0 || !m.nameResolveDue(roomID) {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	defer func() { m.nameBudget.spend(time.Since(start)) }()

	m.mc.ResolveRoomName(ctx, roomID)
}

func (m *Matrix) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	var users []*bridge.UserInfo

	ids, err := m.mc.JoinedMembers(ctx, id.RoomID(channelID))
	if err != nil {
		return nil, err
	}

	logger.Debugf("GetChannelUsers %s %d", m.roomLabel(id.RoomID(channelID)), len(ids))

	for _, userID := range ids {
		if u := m.createUser(userID); u != nil {
			users = append(users, u)
		}
	}

	return users, nil
}

func (m *Matrix) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	userIDs := m.mc.KnownUsers()

	logger.Debugf("GetUsers %d", len(userIDs))

	for _, userID := range userIDs {
		if u := m.createUser(userID); u != nil {
			users = append(users, u)
		}
	}

	return users
}

func (m *Matrix) GetChannels() []*bridge.ChannelInfo {
	rooms, names := m.roomsWithNames()
	channels := make([]*bridge.ChannelInfo, 0, len(rooms))

	for _, room := range rooms {
		channels = append(channels, m.channelInfo(room, names[room.ID]))
	}

	return channels
}

// channelInfo builds the channel for a room, RoomTeams being the costly part.
func (m *Matrix) channelInfo(room matrixclient.RoomInfo, name string) *bridge.ChannelInfo {
	teamID := ""
	if teams := m.mc.RoomTeams(room.ID); len(teams) > 0 && !room.IsDirect {
		teamID = teams[0].String()
	}

	// IsDMChannelName and GetDMUser need the raw room ID of a one-to-one DM.
	if room.IsDirect && len(m.mc.DirectUsers(room.ID)) == 1 {
		name = room.ID.String()
	}

	return &bridge.ChannelInfo{
		Name:       name,
		ID:         room.ID.String(),
		TeamID:     teamID,
		DM:         room.IsDirect,
		Private:    false,
		LastPostAt: room.LastActivity,
	}
}

func (m *Matrix) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	rooms, names := m.roomsWithNames()

	for _, room := range rooms {
		if room.ID.String() == channelID {
			return m.channelInfo(room, names[room.ID]), nil
		}
	}

	return nil, errors.New("channel not found")
}

func (m *Matrix) GetChannelCount() int {
	return len(m.mc.Rooms())
}

func (m *Matrix) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return nil
	}

	return m.createUser(uid)
}

func (m *Matrix) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return []string{}, nil
	}

	var channels []string

	rooms, names := m.roomsWithNames()

	for _, room := range rooms {
		if room.IsDirect {
			continue
		}

		members, err := m.mc.RoomUsers(ctx, room.ID)
		if err == nil && slices.Contains(members, uid) {
			channels = append(channels, "#"+names[room.ID])
		}
	}

	return channels, nil
}

func (m *Matrix) GetUserCount() int {
	return len(m.mc.KnownUsers())
}

func (m *Matrix) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.UserID())
}

func (m *Matrix) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	userID, ok := m.resolveUserID(username)
	if !ok {
		return nil
	}

	return m.createUser(userID)
}

// UserDetails returns the avatar URL and whether the user is verified, for WHOIS.
func (m *Matrix) UserDetails(ctx context.Context, userID string) (string, bool) {
	uid, ok := m.resolveUserID(userID)
	if !ok {
		return "", false
	}

	secure, err := m.mc.UserVerified(ctx, uid)

	return m.mc.GetUserAvatarURL(ctx, uid), err == nil && secure
}

// hostWithoutPort drops the port from a server name, as an IRC host can't contain ':'.
func hostWithoutPort(serverName string) string {
	host, _, err := net.SplitHostPort(serverName)
	if err != nil {
		return serverName
	}

	return host
}

// nickAffixes returns the text placed before the homeserver and after it in a
// remote user's nick. "[]" is special and wraps it as localpart[homeserver].
func nickAffixes(sep string) (string, string) {
	if sep == "[]" {
		return "[", "]"
	}

	return sep, ""
}

// Matrix IDs can be 255 bytes and escaping can triple a nick, which would push the
// nick!ident@host prefix past the 512 byte IRC line limit and truncate the command.
const (
	maxNickLen  = 64
	maxIdentLen = 96
)

// capLen shortens s to at most limit bytes, ending in "^h" and a hash of the user ID so that
// distinct users keep distinct values. 'h' isn't a hex digit, so the suffix can't equal an
// escape sequence ('^' plus two hex digits) and a capped value never matches an uncapped one.
func capLen(s string, userID id.UserID, limit int) string {
	if len(s) <= limit {
		return s
	}

	sum := sha256.Sum256([]byte(userID))

	// Don't split an escape sequence ('^' plus two digits).
	cut := limit - 14
	for cut > 0 && strings.LastIndexByte(s[max(cut-2, 0):cut], '^') >= 0 {
		cut--
	}

	// Drops a rune that the cut left incomplete.
	head := strings.ToValidUTF8(s[:cut], "")

	return head + "^h" + hex.EncodeToString(sum[:6])
}

// validNickSep reports whether sep can safely separate localpart and homeserver in a nick.
// Letters and digits are rejected as nick lookups are case-insensitive, '^' is the escape
// marker, '.', '-', '_' can occur in homeservers and localparts, and the characters that
// sanitizeNick replaces would collapse into '-'. Escaping can't separate any of those.
func validNickSep(sep string) bool {
	for _, r := range sep {
		if r > '~' || r <= ' ' || strings.ContainsRune("^.-_!+%@&#$'\"?*,~", r) {
			return false
		}

		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return false
		}
	}

	return sep != ""
}

// escapeNickPart percent-style escapes ('^' plus two hex digits) the escape marker, every
// character of the separator, every character sanitizeNick would later turn into '-' and the
// rfc1459 case-mapped characters (all ASCII, so always two digits). Otherwise distinct users could share a nick, e.g.
// alice+bob and alice-bob. '^' isn't a legal localpart character, and unlike '~' it
// doesn't mark an ident in resolveUserID. Used for both the localpart and homeserver.
func escapeNickPart(part, sep string) string {
	// "[]\\~" fold to "{}|^" under IRC's rfc1459 case mapping, so all of them are escaped.
	const reserved = "^!+%@&#$:'\"?*, []\\{}|~"

	if !strings.ContainsAny(part, reserved+sep) {
		return part
	}

	var b strings.Builder

	for _, r := range part {
		if strings.ContainsRune(reserved, r) || strings.ContainsRune(sep, r) {
			fmt.Fprintf(&b, "^%02x", r)

			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

// NickAllowsColon reports whether nicks may contain ':', so sanitizeNick must keep it.
func (m *Matrix) NickAllowsColon() bool {
	return strings.Contains(m.nickSep, ":")
}

func (m *Matrix) createUser(userID id.UserID) *bridge.UserInfo {
	localpart, homeserver, err := userID.Parse()
	if err != nil {
		return nil
	}

	displayName := m.mc.DisplayName(userID)
	if displayName == "" {
		displayName = localpart + "@" + homeserver
	}

	// Drop the homeserver/host part if it's a user on the same homeserver as us.
	// The separator can't appear in the localpart, so it is escaped.
	prefix, suffix := nickAffixes(m.nickSep)
	safeLocalpart := escapeNickPart(localpart, m.nickSep)
	nick := capLen(safeLocalpart+prefix+escapeNickPart(homeserver, m.nickSep)+suffix, userID, maxNickLen)

	// The IRC user is the key of a ghost. It always has the homeserver, as '/' is
	// legal in a localpart and would otherwise collide with a remote user.
	ident := capLen("~"+localpart+"/"+homeserver, userID, maxIdentLen)

	_, myHomeserver, err := m.mc.UserID().Parse()
	if err == nil && homeserver == myHomeserver {
		nick = capLen(safeLocalpart, userID, maxNickLen)
	}

	info := &bridge.UserInfo{
		Nick:      nick,
		User:      ident,
		Real:      displayName,
		Host:      hostWithoutPort(homeserver),
		Ghost:     true,
		Me:        userID == m.mc.UserID(),
		Username:  nick,
		Directory: m.directoryURL(nick, localpart),
	}

	if m.mc.IsBot(userID) {
		info.Roles = "bot"
	}

	if info.Me {
		info.MentionKeys = []string{localpart, displayName, userID.String()}
	}

	return info
}

// directoryURL expands the {nick} and {username} placeholders of matrix.DirectoryURL,
// escaping them for the path and query part respectively.
func (m *Matrix) directoryURL(nick, username string) string {
	tmpl := m.cfg.Matrix().Bridge.DirectoryURL
	if tmpl == "" {
		return ""
	}

	pathPart, queryPart, hasQuery := strings.Cut(tmpl, "?")

	directoryURL := strings.NewReplacer(
		"{nick}", url.PathEscape(nick),
		"{username}", url.PathEscape(username),
	).Replace(pathPart)

	if hasQuery {
		directoryURL += "?" + strings.NewReplacer(
			"{nick}", url.QueryEscape(nick),
			"{username}", url.QueryEscape(username),
		).Replace(queryPart)
	}

	return directoryURL
}

// roomMentionRegexp matches the Matrix @room mention along with Mattermost's @channel, @all and @here.
var roomMentionRegexp = regexp.MustCompile(`@(?:room|channel|all|here)(?:\W|$)`)

// messageType returns "notice" for messages mentioning the whole room, so they don't highlight.
func (m *Matrix) messageType(text string) string {
	if !m.cfg.Matrix().DisableDefaultMentions && roomMentionRegexp.MatchString(text) {
		return "notice"
	}

	return ""
}

func (m *Matrix) addParentMsg(ctx context.Context, roomID id.RoomID, parentID id.EventID, msg string, newLen int, uncounted string) (string, error) {
	// Search and use cached reply if it exists.
	replyMessage, ok := m.msgParentCache.Get(parentID)
	if ok {
		logger.Debugf("Found saved reply for parent post %s, using:%s", parentID, replyMessage)

		return strings.TrimRight(msg, "\n") + replyMessage, nil
	}

	// None found, so we'll need to create one and save it for future uses.
	replyMessage, err := m.createParentReply(ctx, roomID, parentID, newLen, uncounted)
	if err != nil {
		return msg, err
	}

	logger.Debugf("Created reply for parent post %s:%s", parentID.String(), replyMessage)
	m.msgParentCache.Add(parentID, replyMessage)

	return strings.TrimRight(msg, "\n") + replyMessage, nil
}

func (m *Matrix) createParentReply(ctx context.Context, roomID id.RoomID, parentID id.EventID, newLen int, uncounted string) (string, error) {
	resp, err := m.mc.Event(ctx, roomID, parentID)
	if err != nil {
		return "", err
	}

	body, _ := resp.Content.Raw["body"].(string)

	nick := resp.Sender.String()
	if parentUser := m.GetUser(ctx, resp.Sender.String()); parentUser != nil {
		nick = parentUser.Nick
	}

	summary := m.formatSummary(body, newLen, uncounted)
	if mxCfg := m.cfg.Matrix(); mxCfg.DisableDefaultMentions {
		summary = utils.SanitizeDefaultMentions(summary, mxCfg.Formatter.Unicode)
	}

	return " (re @" + nick + ": " + summary + ")", nil
}

func (m *Matrix) GetTeamName(ctx context.Context, teamID string) string {
	for _, team := range m.mc.Teams() {
		if team.ID.String() == teamID {
			return team.Name
		}
	}

	return ""
}

// GetLastViewedAt returns the time of the event our read marker points to, 0 if unknown.
func (m *Matrix) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return m.mc.LastViewedAt(ctx, id.RoomID(channelID))
}

func (m *Matrix) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return m.historySince(ctx, channelID, since, "scrollback")
}

func (m *Matrix) historySince(ctx context.Context, channelID string, since int64, eventType string) []*bridge.Event {
	msgs, err := m.mc.GetMessagesSince(ctx, id.RoomID(channelID), since)
	if err != nil {
		logger.Errorf("matrix: fetching messages of %s: %s", m.roomLabel(id.RoomID(channelID)), err)

		// nil, unlike an empty result, tells replay that the fetch failed.
		return nil
	}

	return m.msgsToEvents(msgs, eventType)
}

func (m *Matrix) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	// A negative capacity panics and zero fetches nothing, so default like Mattermost.
	if limit <= 0 {
		limit = defaultScrollbackLimit
	}

	// The limit sizes an allocation, so a huge one from a client would panic.
	limit = min(limit, maxScrollbackLimit)

	msgs, err := m.mc.GetMessages(ctx, id.RoomID(channelID), limit)
	if err != nil {
		logger.Errorf("matrix: fetching messages of %s: %s", m.roomLabel(id.RoomID(channelID)), err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "scrollback")
}

func (m *Matrix) GetPostChannelID(ctx context.Context, postID string) string {
	roomID, ok := m.roomOf(postID)
	if !ok {
		return ""
	}

	return roomID.String()
}

func (m *Matrix) GetPostSizeLimit() int {
	// Matrix limits event PDUs to 65,536 bytes (64 KiB). Accounting for worst-case
	// JSON escaping (\u003c) and Megolm base64 ciphertext expansion (~8x), a conservative
	// 4,000 raw byte cap guarantees encrypted room posts stay well within homeserver limits.
	return 4000
}

func (m *Matrix) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	roomID, ok := m.roomOf(postID)
	if !ok {
		return []*bridge.Event{}
	}

	msgs, err := m.mc.GetThread(ctx, roomID, id.EventID(postID))
	if err != nil {
		logger.Errorf("matrix: fetching thread %s: %s", postID, err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "details")
}

func (m *Matrix) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return m.historySince(ctx, channelID, since, "replay")
}

func (m *Matrix) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	msgs, err := m.mc.SearchMessages(ctx, search)
	if err != nil {
		logger.Errorf("matrix: searching messages: %s", err)

		return []*bridge.Event{}
	}

	return m.msgsToEvents(msgs, "search")
}

func (m *Matrix) UpdateLastViewed(ctx context.Context, channelID string) {
	roomID := id.RoomID(channelID)

	// The client orders events by timeline position, unlike arrival or decryption order.
	evID := m.mc.LastEventID(roomID)
	if evID == "" {
		return
	}

	sendLogErr("marking "+channelID+" as read", m.mc.UpdateLastViewed(ctx, roomID, evID))
}

// ReplayHidesHistory reports whether replay can't show the history of a room: encrypted
// events aren't fetched, so marking the room read afterwards would acknowledge unseen messages.
func (m *Matrix) ReplayHidesHistory(channelID string) bool {
	return m.mc.IsEncrypted(id.RoomID(channelID))
}

func (m *Matrix) UpdateLastViewedUser(ctx context.Context, userID string) error {
	roomID := m.dmRoom(userID)
	if roomID == "" {
		return nil
	}

	m.UpdateLastViewed(ctx, roomID.String())

	return nil
}

// GetFilesInfo takes mxc:// URIs as file IDs.
func (m *Matrix) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	files := make([]*bridge.File, 0, len(fileIDs))

	for _, fileID := range fileIDs {
		link := m.mc.PublicLink(id.ContentURIString(fileID))
		if link != "" {
			files = append(files, &bridge.File{Name: fileID, URL: link})
		}
	}

	return files
}

func (m *Matrix) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	found, err := m.mc.SearchUsers(ctx, query, searchUsersLimit)
	if err != nil {
		return nil, err
	}

	users := make([]*bridge.UserInfo, 0, len(found))

	for _, u := range found {
		if info := m.createUser(u.ID); info != nil {
			users = append(users, info)
		}
	}

	return users, nil
}

func (m *Matrix) GetChannelID(ctx context.Context, name, teamID string) string {
	name = strings.TrimPrefix(name, "#")

	for _, channel := range m.GetChannels() {
		if channel.ID != name && channel.Name != name {
			continue
		}

		if teamID == "" || channel.ID == name || m.inTeam(channel.ID, teamID) {
			return channel.ID
		}
	}

	// A room of the space named by its own name, even if its channel name got a suffix.
	if teamID != "" {
		rooms, _ := m.roomsWithNames()

		for _, room := range rooms {
			if strings.TrimPrefix(room.Name, "#") == name && m.inTeam(room.ID.String(), teamID) {
				return room.ID.String()
			}
		}
	}

	// A user resolves to the direct room with them.
	if roomID := m.dmRoom(name); roomID != "" {
		return roomID.String()
	}

	return ""
}

func (m *Matrix) inTeam(channelID, teamID string) bool {
	return slices.Contains(m.mc.RoomTeams(id.RoomID(channelID)), id.RoomID(teamID))
}

func (m *Matrix) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	roomID, err := m.mc.CreateRoom(ctx, strings.TrimPrefix(channelName, "#"), "", channelType == "O")
	if err != nil {
		return nil, err
	}

	return &bridge.ChannelInfo{Name: channelName, ID: roomID.String(), Private: channelType != "O"}, nil
}

func (m *Matrix) GetDMChannelID(ctx context.Context, userID string) string {
	return m.dmRoom(userID).String()
}

// GetDMChannelName returns the room ID, which GetChannelID resolves again.
func (m *Matrix) GetDMChannelName(userID1 string, userID2 string) string {
	return m.dmRoom(userID2).String()
}

func (m *Matrix) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	users := m.mc.DirectUsers(id.RoomID(channelName))
	if len(users) != 1 {
		return nil
	}

	return m.createUser(users[0])
}

func (m *Matrix) GetDMUserIDs(channelName string) (string, string, bool) {
	other := m.dmPeer(id.RoomID(channelName))
	if other == nil {
		return "", "", false
	}

	return m.GetMe().User, other.User, true
}

func (m *Matrix) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	other := m.dmPeer(id.RoomID(channelName))
	if other == nil {
		return "", false
	}

	return other.User, true
}

func (m *Matrix) IsDMChannelName(channelName string) bool {
	return m.mc.IsDirect(id.RoomID(channelName))
}

// EncryptionEnabled reports whether this session can use encrypted rooms.
func (m *Matrix) EncryptionEnabled() bool {
	return m.e2ee.Load()
}

// DeviceInfo returns the device ID and Ed25519 fingerprint to verify this
// session manually from another client.
func (m *Matrix) DeviceInfo() (string, string) {
	return m.mc.DeviceID().String(), m.mc.Fingerprint()
}

// WaitReady blocks until the initial sync was processed, so the crypto store is
// up to date. It reports false when ctx or the bridge ends first.
func (m *Matrix) WaitReady(ctx context.Context) bool {
	select {
	case <-m.readyCh:
		return true
	case <-ctx.Done():
	case <-m.ctx.Done():
	}

	return false
}

// deviceVerifyTimeout bounds the verification state lookup at login.
const deviceVerifyTimeout = 10 * time.Second

// DeviceVerified reports whether this session is verified or cross-signed. A
// non-nil error means the state could not be determined.
func (m *Matrix) DeviceVerified() (bool, error) {
	// Optional lookup: a stalled homeserver must not hold the login guard for the HTTP client timeout.
	ctx, cancel := context.WithTimeout(m.ctx, deviceVerifyTimeout)
	defer cancel()

	return m.mc.DeviceVerified(ctx)
}

func (m *Matrix) Connected() bool {
	return m.connected.Load()
}

func (m *Matrix) reactionKey(emoji string) string {
	emoji = strings.Trim(emoji, ":")

	if unicode, ok := utils.EmojiFromAlias(emoji, m.cfg.Matrix().Formatter.CustomEmoji); ok {
		return unicode
	}

	return emoji
}

func (m *Matrix) AddReaction(ctx context.Context, msgID, emoji string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	key := m.reactionKey(emoji)

	// The reaction has no event ID to match its echo, so the echo is expected for a while.
	sentKey := msgID + "\x00" + key
	m.sentReactions.Add(sentKey, time.Now())

	reactionID, err := m.mc.React(ctx, roomID, id.EventID(msgID), key)
	if err != nil {
		m.sentReactions.Remove(sentKey)

		return err
	}

	m.reactionCache.Add(msgID+"\x00"+key, reactionID)

	return nil
}

func (m *Matrix) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	roomID, ok := m.roomOf(msgID)
	if !ok {
		return errors.New("unknown message " + msgID)
	}

	cacheKey := msgID + "\x00" + m.reactionKey(emoji)

	reactionID, ok := m.reactionCache.Get(cacheKey)
	if !ok {
		return errors.New("reaction was not added by this session")
	}

	err := m.mc.DeleteReaction(ctx, roomID, reactionID)
	if err != nil {
		return err
	}

	m.reactionCache.Remove(cacheKey)

	return nil
}

func (m *Matrix) GetLastSentMsgs() []string {
	data := make([]string, 0)

	for _, k := range m.msgLastSentCache.Keys() {
		if v, ok := m.msgLastSentCache.Get(k); ok {
			data = append(data, fmt.Sprintf("[@@%s] %s", k, v))
		}
	}

	return data
}

func (m *Matrix) Config() any {
	return &m.cfg.Current().Matrix
}

func (m *Matrix) BridgeConfig() *config.BridgeConfig {
	return &m.cfg.Current().Matrix.Bridge
}

func (m *Matrix) FormatterConfig() *config.FormatterConfig {
	return &m.cfg.Current().Matrix.Formatter
}

func (m *Matrix) Ping(ctx context.Context, proto ...string) error {
	return m.mc.Ping(ctx)
}

func (m *Matrix) NormalizePostID(msgID string) string {
	if strings.HasPrefix(msgID, "$") {
		return msgID
	}

	return ""
}

func (m *Matrix) SendTyping(ctx context.Context, channelName string) {
	rc := m.cfg.Current()
	if !rc.EnableTyping || rc.DisableSendTyping || strings.HasPrefix(channelName, "&") {
		return
	}

	var roomID id.RoomID

	if postID, ok := strings.CutPrefix(channelName, "@@"); ok {
		roomID, _ = m.roomOf(postID)
	} else if roomID = id.RoomID(m.GetChannelID(ctx, channelName, "")); roomID == "" {
		if userID, found := m.resolveUserID(channelName); found {
			roomID = m.mc.DirectRoom(userID)
		}
	}

	if roomID == "" {
		return
	}

	sendLogErr("sending typing to "+roomID.String(), m.mc.SendTyping(ctx, roomID))
}

// formatSummary renders msg like the Mattermost bridge does for replies: markdown
// to IRC formatting, emoji aliases replaced, then shortened to maxLen.
func (m *Matrix) formatSummary(msg string, maxLen int, uncounted string) string {
	// Single config snapshot so a reload cannot change settings mid-format.
	fc := &m.cfg.Matrix().Formatter
	unicode := fc.Unicode

	if !fc.DisableMarkdown {
		msg = markdownReplacer.Replace(msg)

		blockquoteChar := blockquoteCharNonUnicode
		if unicode {
			blockquoteChar = blockquoteCharUnicode
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
