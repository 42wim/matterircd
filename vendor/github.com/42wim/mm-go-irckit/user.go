package irckit

import (
	"net"
	"strings"
	"sync"

	"github.com/sorcix/irc"
)

// NewUser creates a *User, wrapping a connection with metadata we need for our server.
func NewUser(c Conn) *User {
	return &User{
		Conn:     c,
		Host:     "*",
		channels: map[Channel]struct{}{},
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
	Nick  string // From NICK command
	User  string // From USER command
	Real  string // From USER command
	Host  string
	Roles string

	channels map[Channel]struct{}

	MmInfo
	SlackInfo
}

func (u *User) ID() string {
	return strings.ToLower(u.Nick)
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
func (user *User) Encode(msgs ...*irc.Message) (err error) {
	if user.MmGhostUser {
		return nil
	}
	for _, msg := range msgs {
		logger.Debugf("-> %s", msg)
		err := user.Conn.Encode(msg)
		if err != nil {
			return err
		}
	}
	return nil
}

// Decode will receive and return a decoded message, or an error.
func (user *User) Decode() (*irc.Message, error) {
	if user.MmGhostUser {
		// block
		c := make(chan struct{})
		<-c
	}
	msg, err := user.Conn.Decode()
	if err == nil && msg != nil {
		logger.Debugf("<- %s", msg)
	}
	return msg, err
}
