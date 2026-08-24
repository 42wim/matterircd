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
	Caps map[string]bool
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
	for ch := range u.channels {
		ch.Part(u, defaultCloseMsg)
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
	if u.Ghost {
		return nil
	}

	for _, msg := range msgs {
		msg.Trailing = strings.TrimRight(msg.Trailing, " \t\r\n")

		if err := u.Conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			logger.Warnf("failed to set write deadline for %s: %v", u.Nick, err)
		}

		if msg.Command == irc.PRIVMSG && (msg.Prefix.Name == "slack" || msg.Prefix.Name == "mattermost") && msg.Prefix.Host == "service" && strings.Contains(msg.Trailing, "token") { //nolint:goconst,staticcheck
			logger.Debugf("-> %s %s %s", msg.Command, msg.Prefix.Name, "[token redacted]")

			err := u.Conn.Encode(msg)
			if err != nil {
				return err
			}

			continue
		}

		switch msg.Command {
		case irc.PONG, irc.TOPIC:
			logger.Tracef("-> %q", msg.String())
		case irc.RPL_ENDOFBANLIST, irc.RPL_NAMREPLY, irc.RPL_WHOREPLY:
			logger.Tracef("-> %q", msg.String())
		default:
			if strings.Contains(msg.Command, "@+typing=active") {
				logger.Tracef("-> %q", msg.String())
			} else {
				logger.Debugf("-> %q", msg.String())
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
	replyRegExp  = regexp.MustCompile(`\@\@(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)\s`)
	modifyRegExp = regexp.MustCompile(`^s/(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)?/`)
)

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
	continuationTimeout := min(timeout, 200*time.Millisecond)
	t := timer.NewTimer(timeout)
	t.Stop()
	var wg sync.WaitGroup
	wg.Add(1)

	go func(buffer chan *irc.Message) {
		defer wg.Done()

		var (
			bufferedMsg    *irc.Message
			bufferDeadline time.Time
		)

		flush := func() {
			t.Stop()
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
			bufferDeadline = time.Time{}
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
						bufferDeadline = time.Time{}
					}
					logger.Tracef("decode buffer goroutine exiting for %s", u.Nick)
					return
				}

				if strings.HasPrefix(msg.Trailing, "\x01") || modifyRegExp.MatchString(msg.Trailing) {
					logger.Trace("flushing buffer because of CTCP or message modifications")
					flush()

					u.DecodeCh <- msg

					continue
				}

				// If starting a thread reply, flush any previous message buffer first.
				if replyRegExp.MatchString(msg.Trailing) {
					logger.Trace("flushing buffer because of replies to threads")
					flush()
				}

				// are we starting a new buffer ?
				if bufferedMsg == nil {
					bufferedMsg = msg
					bufferDeadline = time.Now().Add(timeout)

					t.Reset(continuationTimeout)
				} else {
					// make sure we're sending to the same recipient in the buffer
					if bufferedMsg.Params[0] == msg.Params[0] {
						// Guard against exceeding Mattermost post limit (16,384 characters)
						if len(bufferedMsg.Trailing)+len(msg.Trailing)+1 > 16250 {
							flush()

							bufferedMsg = msg
							bufferDeadline = time.Now().Add(timeout)

							t.Reset(continuationTimeout)
						} else {
							bufferedMsg.Trailing += "\n" + msg.Trailing

							remaining := time.Until(bufferDeadline)
							if remaining <= 0 {
								flush()
							} else {
								resetDuration := min(continuationTimeout, remaining)
								t.Reset(resetDuration)
							}
						}
					} else {
						flush()
						bufferedMsg = msg
						bufferDeadline = time.Now().Add(timeout)

						t.Reset(continuationTimeout)
					}
				}
			case <-t.C:
				flush()
			}
		}
	}(buffer)

	for {
		msg, err := u.Conn.Decode()
		if err != nil {
			close(buffer)
			wg.Wait()

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

		dmsg := "<- " + msg.String()
		if msg.Command == irc.PRIVMSG && msg.Params != nil && (msg.Params[0] == "slack" || msg.Params[0] == "mattermost") {
			// Don't log sensitive information
			trail := strings.Split(msg.Trailing, " ")
			if (msg.Trailing != "" && trail[0] == "login") || (len(msg.Params) > 1 && msg.Params[1] == "login") {
				dmsg = "<- PRIVMSG " + msg.Params[0] + " :login [redacted]"
			}
		} else if msg.Command == irc.PASS {
			dmsg = "<- PASS [redacted]"
		}

		// PRIVMSG can be buffered
		switch msg.Command {
		case irc.PRIVMSG:
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

func (u *User) HasCapability(capability string) bool {
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
