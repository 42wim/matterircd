package irckit

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/desertbit/timer"
	"github.com/sirupsen/logrus"
	"github.com/sorcix/irc"
)

// NewUser creates a *User, wrapping a connection with metadata we need for our server.
func NewUser(c Conn) *User {
	ctx, cancel := context.WithCancel(context.Background())
	u := &User{
		Conn:   c,
		cancel: cancel,
		ctx:    ctx,
	}

	if c != nil {
		u.UserInfo = &bridge.UserInfo{
			Host: "*",
		}
		u.DecodeCh = make(chan *irc.Message)
	}

	return u
}

// NewUserNet creates a *User from a net.Conn connection.
func NewUserNet(c net.Conn) *User {
	return NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
}

const defaultCloseMsg = "Closed."

type User struct {
	Conn

	sync.RWMutex
	*bridge.UserInfo

	BufferedMsg *irc.Message
	DecodeCh    chan *irc.Message

	channels map[Channel]struct{}

	cfg *config.Config

	UserBridge

	//nolint:containedctx // Tied to the lifecycle of the persistent client session
	ctx    context.Context
	cancel context.CancelFunc

	lastSync time.Time

	// IRCv3 capabilities client negotiated
	capsMu sync.RWMutex
	Caps   map[string]bool

	awayNotifyMu    sync.Mutex
	awayNotifiedAt  int64
	awayNotifiedGen uint64
}

func (u *User) ID() string {
	// return strings.ToLower(u.Nick)
	return strings.ToLower(u.User)
}

func (u *User) Prefix() *irc.Prefix {
	return &irc.Prefix{
		Name: u.Nick,
		User: u.User,
		Host: u.Host,
	}
}

func (u *User) Close() error {
	// Channels() copies under the lock, Part modifies u.channels.
	for _, ch := range u.Channels() {
		ch.Part(u, defaultCloseMsg)
	}

	// ghosts don't have a connection
	if u.Conn == nil {
		return nil
	}

	return u.Conn.Close()
}

func (u *User) String() string {
	return u.Prefix().String()
}

func (u *User) NumChannels() int {
	u.RLock()
	defer u.RUnlock()
	return len(u.channels)
}

func (u *User) Channels() []Channel {
	u.RLock()
	channels := make([]Channel, 0, len(u.channels))
	for ch := range u.channels {
		channels = append(channels, ch)
	}
	u.RUnlock()
	return channels
}

func (u *User) VisibleTo() []*User {
	seen := map[*User]struct{}{}
	seen[u] = struct{}{}

	channels := u.Channels()
	num := 0
	for _, ch := range channels {
		// Don't include self
		num += ch.Len()
	}

	// Pre-allocate
	users := make([]*User, 0, num)
	if num == 0 {
		return users
	}

	// Get all unique users
	for _, ch := range channels {
		for _, other := range ch.Users() {
			if _, dupe := seen[other]; dupe {
				continue
			}
			seen[other] = struct{}{}
			// TODO: Check visibility (once it's implemented)
			users = append(users, other)
		}
	}
	return users
}

const writeTimeout = 10 * time.Second

// Encode and send each msg until an error occurs, then returns.
func (u *User) Encode(msgs ...*irc.Message) (err error) {
	if u == nil || u.Ghost || u.Conn == nil {
		return nil
	}

	for _, msg := range msgs {
		if msg == nil {
			continue
		}

		msg.Trailing = strings.TrimRight(msg.Trailing, " \t\r\n")

		if err := u.Conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			logger.Warnf("failed to set write deadline for %s: %v", u.Nick, err)
		}

		if msg.Prefix != nil && msg.Command == irc.PRIVMSG && (msg.Prefix.Name == "slack" || msg.Prefix.Name == "mattermost") && msg.Prefix.Host == "service" && strings.Contains(msg.Trailing, "token") { //nolint:goconst,staticcheck
			logger.Debugf("-> %s %s %s", msg.Command, msg.Prefix.Name, "[token redacted]")

			err := u.Conn.Encode(msg)
			if err != nil {
				return err
			}

			continue
		}

		// msg.String() allocates, skip it when nobody reads the log.
		if logger.Logger.IsLevelEnabled(logrus.DebugLevel) {
			switch msg.Command {
			case irc.PONG, irc.TOPIC:
				logger.Tracef("-> %q", msg.String())
			case irc.RPL_ENDOFBANLIST, irc.RPL_NAMREPLY, irc.RPL_WHOREPLY:
				logger.Tracef("-> %q", msg.String())
			default:
				if strings.Contains(strings.ToLower(msg.Command), "+typing=") {
					logger.Tracef("-> %q", msg.String())
				} else {
					logger.Debugf("-> %q", msg.String())
				}
			}
		}

		err := u.Conn.Encode(msg)
		if err != nil {
			return err
		}
	}

	return nil
}

var (
	replyRegExp  = regexp.MustCompile(`^\@\@(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)\s`)
	modifyRegExp = regexp.MustCompile(`^s/(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)?/`)
)

// threadPrefix returns the thread ("@@id ") that text is a reply to, or an empty string.
func threadPrefix(text string) string {
	if !strings.HasPrefix(text, "@@") {
		return ""
	}

	return replyRegExp.FindString(text)
}

// Decode will receive and return a decoded message, or an error.
// nolint:funlen,gocognit,gocyclo
func (u *User) Decode() {
	if u.Ghost {
		logger.Trace("ghost user, skipping Decode")
		return
	}
	buffer := make(chan *irc.Message, 512)
	bufferTimeout := u.cfg.Current().PasteBufferTimeout
	// we need at least 100
	if bufferTimeout < 100 {
		bufferTimeout = 100
	}
	logger.Tracef("using paste buffer timeout: %#v", bufferTimeout)
	timeout := time.Duration(bufferTimeout) * time.Millisecond
	t := timer.NewTimer(timeout)
	t.Stop()
	var wg sync.WaitGroup
	wg.Add(1)

	go func(buffer chan *irc.Message) {
		defer wg.Done()

		var (
			bufferedMsg    *irc.Message
			bufferDeadline time.Time
			// When the last line was added to the buffer.
			lastLine time.Time
			// Set when a message was sent because it had been held for the full timeout: the recipient
			// and the thread of it. A paste that is still going carries on in that thread.
			resumeTarget, resumeReply string
		)

		flush := func() {
			t.Stop()

			resumeTarget, resumeReply = "", ""
			if bufferedMsg == nil {
				return
			}

			// trim last newline
			bufferedMsg.Trailing = strings.TrimSpace(bufferedMsg.Trailing)

			// Redact sensitive information for logging
			logMsg := bufferedMsg
			if bufferedMsg.Command == irc.PRIVMSG && strings.HasPrefix(bufferedMsg.Trailing, "login") {
				// Create a shallow copy so we don't mutate the actual message sent to the channel
				redactedMsg := *bufferedMsg
				redactedMsg.Trailing = "login [redacted]"
				logMsg = &redactedMsg
			}

			logger.Tracef("flushing buffer: %#v", logMsg)
			u.DecodeCh <- bufferedMsg
			// clear buffer
			bufferedMsg = nil
		}

		// The timer is not restarted by the lines added to the message, it is the limit
		// for how long a message is held, however many lines it consists of.
		startBuffer := func(msg *irc.Message) {
			resumeTarget, resumeReply = "", ""
			bufferedMsg = msg
			lastLine = time.Now()
			bufferDeadline = lastLine.Add(timeout)

			t.Reset(timeout)
		}

		for {
			select {
			case msg, ok := <-buffer:
				if !ok {
					// Best-effort flush on shutdown. Avoid blocking forever if nobody is draining DecodeCh.
					t.Stop()
					if bufferedMsg != nil {
						// trim last newline
						bufferedMsg.Trailing = strings.TrimSpace(bufferedMsg.Trailing)
						select {
						case u.DecodeCh <- bufferedMsg:
						case <-time.After(1 * time.Second):
							logger.Warnf("timed out flushing decode buffer for %s", u.Nick)
						}
						bufferedMsg = nil
					}
					logger.Tracef("decode buffer goroutine exiting for %s", u.Nick)
					return
				}

				// Only PRIVMSGs get merged. QUIT and PART are in the buffer to go after the
				// message that is held, they must not wait for the timer.
				// Without a target (e.g. "PRIVMSG :text") there is nothing to compare, the
				// command handler will reject it. Never buffer those.
				// The same goes for commands (CTCP, message modifications, reactions and the
				// messages to the service bots): they are not text to post, so they must not
				// get merged with the lines around them or wait for the timer.
				if msg.Command != irc.PRIVMSG ||
					len(msg.Params) == 0 ||
					isServiceBot(msg.Params[0]) ||
					strings.HasPrefix(msg.Trailing, "\x01") ||
					modifyRegExp.MatchString(msg.Trailing) ||
					(strings.HasPrefix(msg.Trailing, "@@") && parseReactionToMsgRegExp.MatchString(msg.Trailing)) {
					logger.Trace("flushing buffer because of QUIT, PART, CTCP, message modifications, reactions, service bots or a missing target")
					flush()

					u.DecodeCh <- msg

					continue
				}

				// If starting a thread reply, flush any previous message buffer first.
				if strings.HasPrefix(msg.Trailing, "@@") && replyRegExp.MatchString(msg.Trailing) {
					logger.Trace("flushing buffer because of replies to threads")
					flush()
				}

				// are we starting a new buffer ?
				if bufferedMsg == nil {
					// A paste that is still going after its message was sent for having been held for
					// the full timeout (the line follows less than that after the previous one) carries
					// on in the thread of that message, instead of ending up in the channel.
					if resumeReply != "" && resumeTarget == msg.Params[0] && time.Since(lastLine) < timeout {
						msg.Trailing = resumeReply + msg.Trailing
					}

					startBuffer(msg)
				} else {
					// make sure we're sending to the same recipient in the buffer
					if bufferedMsg.Params[0] == msg.Params[0] {
						// Guard against exceeding post size limit
						postLimit := 460
						if br := u.currentBridge(); br != nil {
							postLimit = br.GetPostSizeLimit()
						}

						if len(bufferedMsg.Trailing)+len(msg.Trailing)+1 > postLimit-50 || time.Until(bufferDeadline) <= 0 {
							replyPrefix := threadPrefix(bufferedMsg.Trailing)

							flush()

							msg.Trailing = replyPrefix + msg.Trailing

							startBuffer(msg)
						} else {
							bufferedMsg.Trailing += "\n" + msg.Trailing
							lastLine = time.Now()
						}
					} else {
						flush()

						startBuffer(msg)
					}
				}
			case <-t.C:
				// The message has been held for as long as it is allowed to.
				var target, reply string
				if bufferedMsg != nil {
					target, reply = bufferedMsg.Params[0], threadPrefix(bufferedMsg.Trailing)
				}

				flush()

				resumeTarget, resumeReply = target, reply
			}
		}
	}(buffer)

	for {
		msg, err := u.Conn.Decode()
		if err != nil {
			close(buffer)
			wg.Wait()

			// The message that was held has been handed over by now, but handle() may not have
			// queued it yet and Quit has to wait for it. handle() only takes this empty message
			// when it is done with the previous one.
			if u.DecodeCh != nil {
				select {
				case u.DecodeCh <- nil:
				case <-time.After(1 * time.Second):
				}
			}

			isEOF := errors.Is(err, io.EOF) || err.Error() == "EOF"
			isClosed := errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed network connection")
			if !isEOF && !isClosed {
				logger.Errorf("msg: %v err: %v", msg, err)
			}
			break
		}

		if msg == nil {
			continue
		}

		// Don't spend time formatting every incoming message when nobody reads the log.
		dmsg := ""
		if logger.Logger.IsLevelEnabled(logrus.DebugLevel) {
			dmsg = incomingLogLine(msg)
		}

		// PRIVMSG can be buffered. QUIT and PART go through the buffer as well, so that what is
		// held in it is sent before them (and not canceled by them).
		command := msg.Command
		if strings.HasPrefix(command, "@") && len(msg.Params) > 0 {
			command = strings.ToUpper(msg.Params[0])
		}

		switch command {
		case irc.PRIVMSG, irc.QUIT, irc.PART:
			logger.Debugf("B: %#v", dmsg)
			buffer <- msg
		case irc.PING, irc.MODE:
			logger.Trace(dmsg)
			u.DecodeCh <- msg
		default:
			logger.Debug(dmsg)
			u.DecodeCh <- msg
		}
	}

	if u.Srv != nil {
		u.Srv.Quit(u, "connection closed")
	}

	if u.DecodeCh != nil {
		close(u.DecodeCh)
	}
}

const (
	serviceSlack      = "slack"
	serviceMattermost = "mattermost"
	serviceMastodon   = "mastodon"
)

// isServiceBot tells if nick is one of the service bots (the ones to login to and to send commands to).
func isServiceBot(nick string) bool {
	return nick == serviceSlack || nick == serviceMattermost || nick == serviceMastodon
}

// incomingLogLine describes a message from the client for the log, without the credentials in it.
func incomingLogLine(msg *irc.Message) string {
	const loginCommand = "login"

	dmsg := "<- " + msg.String()

	switch {
	case msg.Command == irc.PRIVMSG && len(msg.Params) > 0 && isServiceBot(msg.Params[0]):
		// Don't log sensitive information
		trail := strings.Split(msg.Trailing, " ")
		if (msg.Trailing != "" && trail[0] == loginCommand) || (len(msg.Params) > 1 && msg.Params[1] == loginCommand) {
			dmsg = "<- PRIVMSG " + msg.Params[0] + " :" + loginCommand + " [redacted]"
		}
	case msg.Command == irc.PASS:
		dmsg = "<- PASS [redacted]"
	}

	return dmsg
}

func (u *User) HasCapability(capability string) bool {
	if capability == "server-time" && (u.cfg == nil || !u.cfg.Current().ServerTime) {
		return false
	}

	u.capsMu.RLock()
	defer u.capsMu.RUnlock()

	if u.Caps == nil {
		return false
	}

	return u.Caps[capability]
}

func (u *User) createService(nick string, what string) {
	u.CreateUserFromInfo(
		&bridge.UserInfo{
			Nick:  nick,
			User:  nick,
			Real:  what,
			Host:  "service",
			Ghost: true,
		})
}

// ShouldNotifyAway checks if the away cooldown has elapsed and returns
// whether to notify, the current timestamp, and the generation snapshot.
func (u *User) ShouldNotifyAway(cooldownSec int64) (bool, int64, uint64) {
	u.awayNotifyMu.Lock()
	defer u.awayNotifyMu.Unlock()

	now := time.Now().Unix()
	if now-u.awayNotifiedAt < cooldownSec {
		return false, 0, 0
	}

	return true, now, u.awayNotifiedGen
}

// MarkAwayNotified records that an away notification was delivered if no reset
// occurred while the message was in-flight.
func (u *User) MarkAwayNotified(now int64, expectedGen uint64) {
	u.awayNotifyMu.Lock()
	defer u.awayNotifyMu.Unlock()

	if u.awayNotifiedGen == expectedGen {
		u.awayNotifiedAt = now
	}
}

// ResetAwayNotified clears the cooldown and bumps the generation counter so
// any stale in-flight sends are discarded.
func (u *User) ResetAwayNotified() {
	u.awayNotifyMu.Lock()
	defer u.awayNotifyMu.Unlock()

	u.awayNotifiedAt = 0
	u.awayNotifiedGen++
}
