package irckit

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
)

// Channel is a representation of a room in our server
type Channel interface {
	Prefixer
	Publisher

	// ID is a normalized unique identifier for the channel
	ID() string

	// Created returns the time when the Channel was created.
	Created() time.Time

	// Names returns a sorted slice of Nicks in the channel
	Names() []string

	// Users returns a slice of Users in the channel.
	Users() []*User

	// HasUser returns whether a User is in the channel.
	HasUser(*User) bool

	// Invite prompts the User to join the Channel on behalf of Prefixer.
	Invite(from Prefixer, u *User) error

	// Join introduces the User to the channel (handler for JOIN).
	Join(u *User) error

	// Part removes the User from the channel (handler for PART).
	Part(u *User, text string)

	// Message transmits a message from a User to the channel (handler for PRIVMSG).
	Message(u *User, text string)

	// Topic sets the topic of the channel (handler for TOPIC).
	Topic(from Prefixer, text string)

	// Unlink will disassociate the Channel from its Server.
	Unlink()

	// Len returns the number of Users in the channel.
	Len() int

	// String returns the name of the channel
	String() string
}

type channel struct {
	Publisher
	created time.Time
	name    string
	server  Server

	mu       sync.RWMutex
	topic    string
	usersIdx map[*User]struct{}
}

// NewChannel returns a Channel implementation for a given Server.
func NewChannel(server Server, name string) Channel {
	return &channel{
		Publisher: SyncPublisher(),
		created:   time.Now(),
		server:    server,
		name:      name,
		usersIdx:  map[*User]struct{}{},
	}
}

func (ch channel) Prefix() *irc.Prefix {
	return ch.server.Prefix()
}

func (ch *channel) String() string {
	return ch.name
}

// Created returns the time when the Channel was created.
func (ch *channel) Created() time.Time {
	return ch.created
}

// ID returns a normalized unique identifier for the channel.
func (ch *channel) ID() string {
	return ID(ch.name)
}

func (ch *channel) Message(from *User, text string) {
	msg := &irc.Message{
		Prefix:   from.Prefix(),
		Command:  irc.PRIVMSG,
		Params:   []string{ch.name},
		Trailing: text,
	}
	ch.mu.RLock()
	for to := range ch.usersIdx {
		// TODO: Check err and kick failures?
		if to == from {
			continue
		}
		to.Encode(msg)
	}
	ch.mu.RUnlock()
}

// Quit will remove the user from the channel and emit a PART message.
func (ch *channel) Part(u *User, text string) {
	msg := &irc.Message{
		Prefix:   u.Prefix(),
		Command:  irc.PART,
		Params:   []string{ch.name},
		Trailing: text,
	}
	ch.mu.Lock()
	if _, ok := ch.usersIdx[u]; !ok {
		ch.mu.Unlock()
		u.Encode(&irc.Message{
			Prefix:   ch.Prefix(),
			Command:  irc.ERR_NOTONCHANNEL,
			Params:   []string{ch.name},
			Trailing: "You're not on that channel",
		})
		return
	}
	for to := range ch.usersIdx {
		to.Encode(msg)
	}
	delete(ch.usersIdx, u)
	n := len(ch.usersIdx)
	ch.mu.Unlock()
	u.Lock()
	delete(u.channels, ch)
	u.Unlock()
	ch.server.Publish(&event{PartEvent, ch.server, ch, u, msg})
	if n == 0 {
		ch.Publish(&event{EmptyChanEvent, ch.server, ch, u, nil})
	}
}

// Unlink will disassociate the Channel from the Server.
func (ch *channel) Unlink() {
	ch.server.UnlinkChannel(ch)
}

// Close will evict all users in the channel.
func (ch *channel) Close() error {
	ch.mu.Lock()
	for to := range ch.usersIdx {
		to.Encode(&irc.Message{
			Prefix:  to.Prefix(),
			Command: irc.PART,
			Params:  []string{ch.name},
		})
	}
	ch.usersIdx = map[*User]struct{}{}
	ch.Publisher.Close()
	ch.mu.Unlock()
	return nil
}

// Invite prompts the User to join the Channel on behalf of Prefixer.
func (ch *channel) Invite(from Prefixer, u *User) error {
	return u.Encode(&irc.Message{
		Prefix:  from.Prefix(),
		Command: irc.INVITE,
		Params:  []string{u.Nick, ch.name},
	})
	// TODO: Save state that the user is invited?
}

// Topic sets the topic of the channel (handler for TOPIC).
func (ch *channel) Topic(from Prefixer, text string) {
	ch.mu.RLock()
	ch.topic = text

	msg := &irc.Message{
		Prefix:   from.Prefix(),
		Command:  irc.TOPIC,
		Params:   []string{ch.name},
		Trailing: text,
	}
	for to := range ch.usersIdx {
		to.Encode(msg)
	}

	ch.mu.RUnlock()
}

// Join introduces the User to the channel (sends relevant messages, stores).
func (ch *channel) Join(u *User) error {
	// TODO: Check if user is already here?
	ch.mu.Lock()
	if _, exists := ch.usersIdx[u]; exists {
		ch.mu.Unlock()
		return nil
	}
	topic := ch.topic
	ch.usersIdx[u] = struct{}{}
	ch.mu.Unlock()
	u.Lock()
	u.channels[ch] = struct{}{}
	u.Unlock()

	msg := &irc.Message{
		Prefix:  u.Prefix(),
		Command: irc.JOIN,
		Params:  []string{ch.name},
	}
	for to := range ch.usersIdx {
		to.Encode(msg)
	}

	msgs := []*irc.Message{}
	if topic != "" {
		msgs = append(msgs, &irc.Message{
			Prefix:   ch.Prefix(),
			Command:  irc.RPL_TOPIC,
			Params:   []string{u.Nick, ch.name},
			Trailing: topic,
		})
	}

	msgs = append(msgs,
		&irc.Message{
			Prefix:   ch.Prefix(),
			Command:  irc.RPL_NAMREPLY,
			Params:   []string{u.Nick, "=", ch.name},
			Trailing: strings.Join(ch.Names(), " "),
		},
		&irc.Message{
			Prefix:   ch.Prefix(),
			Params:   []string{u.Nick, ch.name},
			Command:  irc.RPL_ENDOFNAMES,
			Trailing: "End of /NAMES list.",
		},
	)

	return u.Encode(msgs...)
}

func (ch *channel) HasUser(u *User) bool {
	ch.mu.RLock()
	_, ok := ch.usersIdx[u]
	ch.mu.RUnlock()
	return ok
}

// Users returns an unsorted slice of users who are in the channel.
func (ch *channel) Users() []*User {
	ch.mu.RLock()
	users := make([]*User, 0, len(ch.usersIdx))
	for u := range ch.usersIdx {
		users = append(users, u)
	}
	ch.mu.RUnlock()
	return users
}

// Names returns a sorted slice of Nick strings of users who are in the channel.
func (ch channel) Names() []string {
	users := ch.Users()
	names := make([]string, 0, len(users))
	for _, u := range users {
		names = append(names, u.Nick)
	}
	// TODO: Append in sorted order?
	sort.Strings(names)
	return names
}

// Len returns the number of users in the channel.
func (ch *channel) Len() int {
	ch.mu.RLock()
	defer ch.mu.RUnlock()
	return len(ch.usersIdx)
}
