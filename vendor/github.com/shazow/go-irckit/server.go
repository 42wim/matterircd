package irckit

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sorcix/irc"
)

var ErrHandshakeFailed = errors.New("handshake failed")

var defaultVersion = "go-irckit"

const handshakeMsgTolerance = 20

// ID will normalize a name to be used as a unique identifier for comparison.
func ID(s string) string {
	return strings.ToLower(s)
}

type Prefixer interface {
	// Prefix returns a prefix configuration for the origin of the message.
	Prefix() *irc.Prefix
}

type Server interface {
	Prefixer
	Publisher

	// Name of the server (usually hostname).
	Name() string

	// Motd is the Message of the Day for the server.
	Motd() []string

	// Connect starts the handshake for a new user, blocks until it's completed or failed with an error.
	Connect(*User) error

	// Quit removes the user from all the channels and disconnects.
	Quit(*User, string)

	// HasUser returns an existing User with a given Nick.
	HasUser(string) (*User, bool)

	// RenameUser changes the Nick of a User if the new name is available.
	// Returns whether the rename was was successful.
	RenameUser(*User, string) bool

	// Channel gets or creates a new channel with the given name.
	Channel(string) Channel

	// HasChannel returns an existing Channel with a given name.
	HasChannel(string) (Channel, bool)

	// UnlinkChannel removes the channel from the server's storage if it
	// exists. Once removed, the server is free to create a fresh channel with
	// the same ID. The server is not responsible for evicting members of an
	// unlinked channel.
	UnlinkChannel(Channel)
}

// ServerConfig produces a Server setup with configuration options.
type ServerConfig struct {
	// Name is used as the prefix for the server.
	Name string
	// Version string of the server (default: go-irckit).
	Version string
	// Motd is the message of the day for the server, list of message lines where each line should be max 80 chars.
	Motd []string
	// InviteOnly prevents regular users from joining and making new channels.
	InviteOnly bool
	// MaxNickLen is the maximum length for a NICK value (default: 32)
	MaxNickLen int

	// Publisher to use. If nil, a new SyncPublisher will be used.
	Publisher Publisher
	// DiscardEmpty setting will start a goroutine to discard empty channels.
	DiscardEmpty bool
	// NewChannel overrides the constructor for a new Channel in a given Server and Name.
	NewChannel func(s Server, name string) Channel
	// Commands is the handler registry to use (default: DefaultCommands())
	Commands Commands
}

func (c ServerConfig) Server() Server {
	if c.Publisher == nil {
		c.Publisher = SyncPublisher()
	}
	if c.NewChannel == nil {
		c.NewChannel = NewChannel
	}
	if c.Commands == nil {
		c.Commands = DefaultCommands()
	}

	if c.Version == "" {
		c.Version = defaultVersion
	}
	if c.Name == "" {
		c.Name = "go-irckit"
	}
	if c.MaxNickLen == 0 {
		c.MaxNickLen = 32
	}

	srv := &server{
		config:    c,
		users:     map[string]*User{},
		channels:  map[string]Channel{},
		created:   time.Now(),
		commands:  c.Commands,
		Publisher: c.Publisher,
	}
	if c.DiscardEmpty {
		srv.channelEvents = make(chan Event, 1)
		go srv.cleanupEmpty()
	}

	return srv
}

// NewServer creates a server.
func NewServer(name string) Server {
	return ServerConfig{Name: name}.Server()
}

type server struct {
	created  time.Time
	config   ServerConfig
	commands Commands

	sync.RWMutex
	count         int
	users         map[string]*User
	channels      map[string]Channel
	channelEvents chan Event

	Publisher
}

func (s *server) Name() string {
	return s.config.Name
}

func (s *server) Motd() []string {
	return s.config.Motd
}

func (s *server) Close() error {
	// TODO: Send notice or something?
	// TODO: Clear channels?
	s.Lock()
	for _, u := range s.users {
		u.Close()
	}
	s.Publisher.Close()
	s.Unlock()
	return nil
}

// Prefix returns the server's command prefix string.
func (s *server) Prefix() *irc.Prefix {
	return &irc.Prefix{Name: s.config.Name}
}

// HasUser returns whether a given user is in the server.
func (s *server) HasUser(nick string) (*User, bool) {
	s.RLock()
	u, exists := s.users[ID(nick)]
	s.RUnlock()
	return u, exists
}

// Rename will attempt to rename the given user's Nick if it's available.
func (s *server) RenameUser(u *User, newNick string) bool {
	if len(newNick) > s.config.MaxNickLen {
		newNick = newNick[:s.config.MaxNickLen]
	}

	s.Lock()
	if _, exists := s.users[ID(newNick)]; exists {
		s.Unlock()
		u.Encode(&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.ERR_NICKNAMEINUSE,
			Params:   []string{newNick},
			Trailing: "Nickname is already in use",
		})
		return false
	}

	delete(s.users, u.ID())
	oldPrefix := u.Prefix()
	u.Nick = newNick
	s.users[u.ID()] = u
	s.Unlock()

	changeMsg := &irc.Message{
		Prefix:  oldPrefix,
		Command: irc.NICK,
		Params:  []string{newNick},
	}
	u.Encode(changeMsg)
	for _, other := range u.VisibleTo() {
		other.Encode(changeMsg)
	}
	return true
}

// HasChannel returns whether a given channel already exists.
func (s *server) HasChannel(name string) (Channel, bool) {
	s.RLock()
	ch, exists := s.channels[ID(name)]
	s.RUnlock()
	return ch, exists
}

// Channel returns an existing or new channel with the give name.
func (s *server) Channel(name string) Channel {
	s.Lock()
	id := ID(name)
	ch, ok := s.channels[id]
	if !ok {
		newFn := s.config.NewChannel
		ch = newFn(s, name)
		id = ch.ID()
		s.channels[id] = ch
		s.Unlock()
		if s.config.DiscardEmpty {
			ch.Subscribe(s.channelEvents)
		}
		s.Publish(&event{NewChanEvent, s, ch, nil, nil})
	} else {
		s.Unlock()
	}
	return ch
}

// cleanupEmpty receives Channel candidates for cleaning up and removes them if they're empty. (Blocking)
func (s *server) cleanupEmpty() {
	for evt := range s.channelEvents {
		if evt.Kind() != EmptyChanEvent {
			continue
		}
		ch := evt.Channel()
		s.Lock()
		if s.channels[ch.ID()] != ch {
			// Not the same channel anymore, already been replaced.
			s.Unlock()
			continue
		}
		if ch.Len() != 0 {
			// Not empty.
			s.Unlock()
			continue
		}
		delete(s.channels, ch.ID())
		s.Unlock()
	}
}

// UnlinkChannel unlinks the channel from the server's storage, returns whether it existed.
func (s *server) UnlinkChannel(ch Channel) {
	s.Lock()
	chStored := s.channels[ch.ID()]
	r := chStored == ch
	if r {
		delete(s.channels, ch.ID())
	}
	s.Unlock()
}

// Connect starts the handshake for a new User and returns when complete or failed.
func (s *server) Connect(u *User) error {
	err := s.handshake(u)
	if err != nil {
		u.Close()
		return err
	}
	go s.handle(u)
	s.Publish(&event{ConnectEvent, s, nil, u, nil})
	return nil
}

// Quit will remove the user from all channels and disconnect.
func (s *server) Quit(u *User, message string) {
	go u.Close()
	s.Lock()
	delete(s.users, u.ID())
	s.Unlock()
}

func (s *server) guestNick() string {
	s.Lock()
	defer s.Unlock()

	s.count++
	return fmt.Sprintf("Guest%d", s.count)
}

// Len returns the number of users connected to the server.
func (s *server) Len() int {
	s.RLock()
	defer s.RUnlock()
	return len(s.users)
}

func (s *server) welcome(u *User) error {
	err := u.Encode(
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_WELCOME,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("Welcome! %s", u.Prefix()),
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_YOURHOST,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("Your host is %s, running version %s", s.config.Name, s.config.Version),
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_CREATED,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("This server was created %s", s.created.Format(time.UnixDate)),
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_MYINFO,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("%s %s o o", s.config.Name, s.config.Version),
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LUSERCLIENT,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("There are %d users and 0 services on 1 servers", s.Len()),
		},
	)
	if err != nil {
		return err
	}
	// Always include motd, even if it's empty? Seems some clients expect it (libpurple?).
	return CmdMotd(s, u, nil)
}

// names lists all names for a given channel
func (s *server) names(u *User, channels ...string) []*irc.Message {
	// TODO: Support full list?
	r := []*irc.Message{}
	for _, channel := range channels {
		ch, exists := s.HasChannel(channel)
		if !exists {
			continue
		}
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		msg := irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_NAMREPLY,
			Params:   []string{u.Nick, "=", channel},
			Trailing: strings.Join(ch.Names(), " "),
		}
		r = append(r, &msg)
	}
	endParams := []string{u.Nick}
	if len(channels) == 1 {
		endParams = append(endParams, channels[0])
	}
	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   endParams,
		Command:  irc.RPL_ENDOFNAMES,
		Trailing: "End of /NAMES list.",
	})
	return r
}

func (s *server) handle(u *User) {
	var partMsg string
	defer s.Quit(u, partMsg)

	for {
		msg, err := u.Decode()
		if err != nil {
			logger.Errorf("handle decode error for %s: %s", u.ID(), err.Error())
			return
		}
		if msg == nil {
			// Ignore empty messages
			continue
		}

		err = s.commands.Run(s, u, msg)
		if err == ErrUnknownCommand {
			// TODO: Emit event?
		} else if err != nil {
			logger.Errorf("handler error for %s: %s", u.ID(), err.Error())
			return
		}
	}
}

func (s *server) add(u *User) (ok bool) {
	s.Lock()
	defer s.Unlock()

	id := u.ID()
	if _, exists := s.users[id]; exists {
		return false
	}

	s.users[id] = u
	return true
}

func (s *server) handshake(u *User) error {
	// Assign host
	u.Host = u.ResolveHost()

	// Read messages until we filled in USER details.
	for i := handshakeMsgTolerance; i > 0; i-- {
		// Consume N messages then give up.
		msg, err := u.Decode()
		if err != nil {
			return err
		}
		if msg == nil {
			// Empty message, ignore.
			continue
		}

		if len(msg.Params) < 1 {
			u.Encode(&irc.Message{
				Prefix:  s.Prefix(),
				Command: irc.ERR_NEEDMOREPARAMS,
				Params:  []string{msg.Command},
			})
			continue
		}

		switch msg.Command {
		case irc.NICK:
			u.Nick = msg.Params[0]
		case irc.USER:
			u.User = msg.Params[0]
			u.Real = msg.Trailing
		}

		if u.Nick == "" || u.User == "" {
			// Wait for both to be set before proceeding
			continue
		}
		if len(u.Nick) > s.config.MaxNickLen {
			u.Nick = u.Nick[:s.config.MaxNickLen]
		}

		ok := s.add(u)
		if !ok {
			u.Encode(
				&irc.Message{
					Prefix:   s.Prefix(),
					Command:  irc.ERR_NICKNAMEINUSE,
					Params:   []string{u.Nick},
					Trailing: "Nickname is already in use",
				},
			)
			continue
		}

		return s.welcome(u)
	}
	return ErrHandshakeFailed
}
