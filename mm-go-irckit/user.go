package irckit

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/desertbit/timer"
	"github.com/sorcix/irc"
	"github.com/spf13/viper"
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

	v *viper.Viper

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

// Encode and send each msg until an error occurs, then returns.
func (u *User) Encode(msgs ...*irc.Message) (err error) {
	if u.Ghost {
		return nil
	}

	for _, msg := range msgs {
		if msg.Command == "PRIVMSG" && (msg.Prefix.Name == "slack" || msg.Prefix.Name == "mattermost") && msg.Prefix.Host == "service" && strings.Contains(msg.Trailing, "token") {
			logger.Debugf("-> %s %s %s", msg.Command, msg.Prefix.Name, "[token redacted]")

			err := u.Conn.Encode(msg)
			if err != nil {
				return err
			}

			continue
		}

		logger.Debugf("-> %s", msg)

		err := u.Conn.Encode(msg)
		if err != nil {
			return err
		}
	}

	return nil
}

// Decode will receive and return a decoded message, or an error.
// nolint:funlen,gocognit,gocyclo
func (u *User) Decode() {
	if u.Ghost {
		// block
		c := make(chan struct{})
		<-c
	}
	buffer := make(chan *irc.Message)
	stop := make(chan struct{})
	bufferTimeout := u.v.GetInt("PasteBufferTimeout")
	// we need at least 100
	if bufferTimeout < 100 {
		bufferTimeout = 100
	}
	logger.Debugf("using paste buffer timeout: %#v\n", bufferTimeout)
	t := timer.NewTimer(time.Duration(bufferTimeout) * time.Millisecond)
	t.Stop()
	go func(buffer chan *irc.Message, stop chan struct{}) {
		for {
			select {
			case msg := <-buffer:
				// are we starting a new buffer ?
				if u.BufferedMsg == nil {
					u.BufferedMsg = msg
					// start timer now
					t.Reset(time.Duration(bufferTimeout) * time.Millisecond)
				} else {
					replyRe := regexp.MustCompile(`\@\@(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)\s`)
					modifyRe := regexp.MustCompile(`^s/(?:[0-9a-z]{26}|[0-9a-f]{3}|!!)?/`)
					if strings.HasPrefix(msg.Trailing, "\x01ACTION") || replyRe.MatchString(msg.Trailing) || modifyRe.MatchString(msg.Trailing) {
						// flush buffer
						logger.Debug("flushing buffer because of /me, replies to threads, and message modifications")
						u.BufferedMsg.Trailing = strings.TrimSpace(u.BufferedMsg.Trailing)
						u.DecodeCh <- u.BufferedMsg
						u.BufferedMsg = nil
						// send CTCP message
						u.DecodeCh <- msg
						continue
					}
					// make sure we're sending to the same recipient in the buffer
					if u.BufferedMsg.Params[0] == msg.Params[0] {
						u.BufferedMsg.Trailing += "\n" + msg.Trailing
					} else {
						u.DecodeCh <- msg
					}
				}
			case <-t.C:
				if u.BufferedMsg != nil {
					// trim last newline
					u.BufferedMsg.Trailing = strings.TrimSpace(u.BufferedMsg.Trailing)
					logger.Debugf("flushing buffer: %#v\n", u.BufferedMsg)
					u.DecodeCh <- u.BufferedMsg
					// clear buffer
					u.BufferedMsg = nil
					t.Stop()
				}
			case <-stop:
				logger.Debug("closing decode()")
				return
			}
		}
	}(buffer, stop)
	for {
		msg, err := u.Conn.Decode()
		if err != nil {
			close(stop)
			if err.Error() != "EOF" {
				logger.Errorf("msg: %s err: %s", msg, err)
			}
			break
		}

		if msg == nil {
			continue
		}

		dmsg := fmt.Sprintf("<- %s", msg)
		if msg.Command == "PRIVMSG" && msg.Params != nil && (msg.Params[0] == "slack" || msg.Params[0] == "mattermost") {
			// Don't log sensitive information
			trail := strings.Split(msg.Trailing, " ")
			if (msg.Trailing != "" && trail[0] == "login") || (len(msg.Params) > 1 && msg.Params[1] == "login") {
				dmsg = fmt.Sprintf("<- PRIVMSG %s :login [redacted]", msg.Params[0])
			}
		}
		// PRIVMSG can be buffered
		if msg.Command == "PRIVMSG" {
			logger.Debugf("B: %#v\n", dmsg)
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
