package irckit

import (
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
	return &User{
		Conn: c,
		UserInfo: &bridge.UserInfo{
			Host: "*",
		},
		channels: map[Channel]struct{}{},
		DecodeCh: make(chan *irc.Message),
	}
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
		if err := u.Conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
			logger.Warnf("failed to set write deadline for %s: %v", u.Nick, err)
		}

		if msg.Command == "PRIVMSG" && (msg.Prefix.Name == "slack" || msg.Prefix.Name == "mattermost") && msg.Prefix.Host == "service" && strings.Contains(msg.Trailing, "token") {
			logger.Debugf("-> %s %s %s", msg.Command, msg.Prefix.Name, "[token redacted]")

			err := u.Conn.Encode(msg)
			if err != nil {
				return err
			}

			continue
		}

		logger.Debugf("-> \"%s\"", msg)

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
		logger.Debug("ghost user, skipping Decode()")
		return
	}
	buffer := make(chan *irc.Message, 512)
	bufferTimeout := u.cfg.Current().PasteBufferTimeout
	// we need at least 100
	if bufferTimeout < 100 {
		bufferTimeout = 100
	}
	logger.Debugf("using paste buffer timeout: %#v", bufferTimeout)
	timeout := time.Duration(bufferTimeout) * time.Millisecond
	t := timer.NewTimer(timeout)
	t.Stop()
	var wg sync.WaitGroup
	wg.Add(1)
	go func(buffer chan *irc.Message) {
		defer wg.Done()

		var bufferedMsg *irc.Message

		flush := func() {
			t.Stop()
			if bufferedMsg == nil {
				return
			}

			// trim last newline
			bufferedMsg.Trailing = strings.TrimSpace(bufferedMsg.Trailing)
			logger.Debugf("flushing buffer: %#v", bufferedMsg)
			u.DecodeCh <- bufferedMsg
			// clear buffer
			bufferedMsg = nil
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
					logger.Debugf("decode buffer goroutine exiting for %s", u.Nick)
					return
				}
				// are we starting a new buffer ?
				if bufferedMsg == nil {
					bufferedMsg = msg
					// start timer now
					t.Reset(timeout)
				} else {
					if strings.HasPrefix(msg.Trailing, "\x01ACTION") || replyRegExp.MatchString(msg.Trailing) || modifyRegExp.MatchString(msg.Trailing) {
						// flush buffer
						logger.Debug("flushing buffer because of /me, replies to threads, and message modifications")
						flush()
						// send CTCP message
						u.DecodeCh <- msg
						continue
					}
					// make sure we're sending to the same recipient in the buffer
					if bufferedMsg.Params[0] == msg.Params[0] {
						bufferedMsg.Trailing += "\n" + msg.Trailing
					} else {
						flush()
						bufferedMsg = msg
						t.Reset(timeout)
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
		if msg.Command == "PRIVMSG" && msg.Params != nil && (msg.Params[0] == "slack" || msg.Params[0] == "mattermost") {
			// Don't log sensitive information
			trail := strings.Split(msg.Trailing, " ")
			if (msg.Trailing != "" && trail[0] == "login") || (len(msg.Params) > 1 && msg.Params[1] == "login") {
				dmsg = "<- PRIVMSG " + msg.Params[0] + " :login [redacted]"
			}
		}
		// PRIVMSG can be buffered
		if msg.Command == "PRIVMSG" {
			logger.Debugf("B: %#v", dmsg)
			buffer <- msg
		} else {
			logger.Debug(dmsg)
			u.DecodeCh <- msg
		}
	}
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
