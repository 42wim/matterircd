package irckit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/platform/model"
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

	Add(u *User) bool
	Handle(u *User)
	Logout(u *User)
	ChannelCount() int
	UserCount() int
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
}

func (c ServerConfig) Server() Server {
	publisher := c.Publisher
	if publisher == nil {
		publisher = SyncPublisher()
	}
	if c.NewChannel == nil {
		c.NewChannel = NewChannel
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
		Publisher: publisher,
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
	config  ServerConfig
	created time.Time

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
		s.encodeMessage(u, irc.ERR_NICKNAMEINUSE, []string{newNick}, "Nickname is already in use")
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

func (s *server) away(u *User, msg string) *irc.Message {
	u.mc.WsAway = false
	r := &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick},
		Command:  irc.RPL_UNAWAY,
		Trailing: "You are no longer marked as being away",
	}
	if msg != "" {
		r = &irc.Message{
			Prefix:   s.Prefix(),
			Params:   []string{u.Nick},
			Command:  irc.RPL_NOWAWAY,
			Trailing: "You have been marked as being away",
		}
		u.mc.WsAway = true
	}
	return r
}

func (s *server) who(u *User, mask string, op bool) []*irc.Message {
	// XXX: Cut this
	endMsg := &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick, mask},
		Command:  irc.RPL_ENDOFWHO,
		Trailing: "End of /WHO list.",
	}

	// TODO: Handle arbitrary masks, not just channels
	ch, exists := s.HasChannel(mask)
	if !exists {
		return []*irc.Message{endMsg}
	}

	r := make([]*irc.Message, 0, ch.Len()+1)
	for _, other := range ch.Users() {
		// <me> <channel> <user> <host> <server> <nick> [H/G]: 0 <real>
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Params:   []string{u.Nick, mask, other.User, other.Host, "*", other.Nick, "H"},
			Command:  irc.RPL_WHOREPLY,
			Trailing: "0 " + other.Real,
		})
	}

	r = append(r, endMsg)
	return r
}

func (s *server) whois(u *User, who string) []*irc.Message {
	endMsg := &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick},
		Command:  irc.RPL_ENDOFWHOIS,
		Trailing: "End of /WHOIS list.",
	}

	other, _ := s.HasUser(who)
	var r []*irc.Message
	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick, other.Nick, other.User, other.Host, "*"},
		Command:  irc.RPL_WHOISUSER,
		Trailing: other.Real,
	})

	var chlist string
	for _, ch := range other.Channels() {
		chlist += ch.String() + " "
	}

	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick, other.Nick},
		Command:  irc.RPL_WHOISCHANNELS,
		Trailing: chlist,
	})

	u.mc.UpdateUsers()
	if _, ok := u.mc.Users[other.User]; ok {
		idle := (model.GetMillis() - u.mc.Users[other.User].LastActivityAt) / 1000
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Params:   []string{u.Nick, other.Nick, strconv.FormatInt(idle, 10), "0"},
			Command:  irc.RPL_WHOISIDLE,
			Trailing: "seconds idle, signon time",
		})
	}

	r = append(r, endMsg)
	return r
}

func (s *server) topic(u *User, channelname string, header string) {
	ch := s.Channel(channelname)
	ch.Topic(u, header)
	channelname = strings.Replace(channelname, "#", "", -1)
	if u.mc != nil && u.mc.User != nil {
		u.mc.UpdateChannelHeader(u.mc.GetChannelId(channelname), header)
	}
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

func (s *server) motd(u *User) []*irc.Message {
	// XXX: Cut this
	r := make([]*irc.Message, 0, len(s.config.Motd)+2)

	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_MOTDSTART,
		Params:   []string{u.Nick},
		Trailing: fmt.Sprintf("- %s Message of the Day -", s.config.Name),
	})

	for _, line := range s.config.Motd {
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_MOTD,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("- %s", line),
		})
	}

	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_ENDOFMOTD,
		Params:   []string{u.Nick},
		Trailing: "End of /MOTD command.",
	})
	return r
}

func (s *server) ison(u *User, nicks ...string) []*irc.Message {
	// XXX: Cut this.
	on := make([]string, 0, len(nicks))
	for _, nick := range nicks {
		if _, ok := s.HasUser(nick); ok {
			on = append(on, nick)
		}
	}

	return []*irc.Message{
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_ISON,
			Params:   []string{u.Nick},
			Trailing: strings.Join(on, " "),
		},
	}
}

// list lists all channels on the server
func (s *server) list(u *User) []*irc.Message {
	r := []*irc.Message{}
	msg := irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_LISTSTART,
		Params:   []string{u.Nick},
		Trailing: "Channel Users Topic",
	}
	r = append(r, &msg)
	if u.mc != nil && u.mc.User != nil {
		for _, channel := range append(u.mc.Channels.Channels, u.mc.MoreChannels.Channels...) {
			// FIXME: This needs to be broken up into multiple messages to fit <510 chars
			if strings.Contains(channel.Name, "__") {
				continue
			}
			msg := irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_LIST,
				Params:   []string{u.Nick},
				Trailing: channel.Name + " #? " + strings.Replace(channel.Header, "\n", " | ", -1),
			}
			r = append(r, &msg)
		}
	}
	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick},
		Command:  irc.RPL_LISTEND,
		Trailing: "End of /LIST",
	})
	return r
}

// handle channel/user mode
func (s *server) mode(u *User, channel string, modetype string) []*irc.Message {
	r := []*irc.Message{}
	switch modetype {
	case "":
		{
			msg := irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_CHANNELMODEIS,
				Params:   []string{u.Nick, channel},
				Trailing: " " + " ",
			}
			r = append(r, &msg)
		}
	case "b":
		{
			msg := irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_ENDOFBANLIST,
				Params:   []string{u.Nick, channel},
				Trailing: "End of channel ban list",
			}
			r = append(r, &msg)
		}
	}
	return r
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

func (s *server) lusers(u *User) []*irc.Message {
	r := []*irc.Message{}
	msg := irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_LUSERCLIENT,
		Params:   []string{u.Nick},
		Trailing: "There are " + strconv.Itoa(u.Srv.UserCount()) + " users and " + strconv.Itoa(u.Srv.ChannelCount()) + " channels on 1 server",
	}
	r = append(r, &msg)
	return r
}

func (s *server) okParams(u *User, msg *irc.Message, length int) bool {
	if len(msg.Params) < length {
		s.encodeMessage(u, irc.ERR_NEEDMOREPARAMS, []string{msg.Command, u.Nick}, "Not enough parameters")
		return false
	}
	return true
}

func (s *server) encodeMessage(u *User, cmd string, params []string, trailing string) error {
	return u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  cmd,
		Params:   params,
		Trailing: trailing,
	})
}

func (s *server) Handle(u *User) {
	s.handle(u)
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
		// TODO: Move this giant switch statement into a command registry system, similar to https://godoc.org/github.com/shazow/ssh-chat/chat#Commands
		switch msg.Command {
		case irc.AWAY:
			if u.mc != nil && u.mc.User != nil {
				u.Encode(s.away(u, msg.Trailing))
			}
		case irc.PART:
			if s.okParams(u, msg, 1) {
				logger.Debugf("channels: %#v", s.channels)
				channels := strings.Split(msg.Params[0], ",")
				for _, chName := range channels {
					ch, exists := s.HasChannel(chName)
					if !exists {
						err = s.encodeMessage(u, irc.ERR_NOSUCHCHANNEL, []string{chName}, "No such channel")
						continue
					}
					// first part on irc
					ch.Part(u, msg.Trailing)
					// now part on mattermost
					u.mc.Client.LeaveChannel(u.mc.GetChannelId(strings.Replace(chName, "#", "", 1)))
					// part all other (ghost)users on the channel
					for _, k := range ch.Users() {
						ch.Part(k, "")
					}
				}
			}
		case irc.QUIT:
			partMsg = msg.Trailing
			s.encodeMessage(u, irc.QUIT, []string{}, partMsg)
			s.encodeMessage(u, irc.ERROR, []string{}, "You will be missed.")
			if u.mc != nil {
				if u.mc.WsClient != nil {
					u.logoutFromMattermost()
				}
			}
			return
		case irc.PING:
			s.encodeMessage(u, irc.PONG, []string{s.config.Name}, msg.Trailing)
		case irc.JOIN:
			if s.okParams(u, msg, 1) {
				if s.config.InviteOnly || u.mc == nil || u.mc.User == nil {
					channels := strings.Split(msg.Params[0], ",")
					for _, channel := range channels {
						err = s.encodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
					}
				} else {
					channels := strings.Split(msg.Params[0], ",")
					for _, channel := range channels {
						// you can only join existing channels
						err := u.mc.JoinChannel(channel)
						if err != nil {
							s.encodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
							continue
						}
						ch := s.Channel(channel)
						ch.Topic(u, u.mc.GetChannelHeader(u.mc.GetChannelId(strings.Replace(channel, "#", "", -1))))
						ch.Join(u)
						u.syncMMChannel(u.mc.GetChannelId(strings.Replace(channel, "#", "", 1)), strings.Replace(channel, "#", "", 1))
					}
				}
			}
		case irc.MOTD:
			err = u.Encode(s.motd(u)...)
		case irc.NAMES:
			if !s.okParams(u, msg, 1) {
				err = u.Encode(s.names(u, msg.Params[0])...)
			}
		case irc.LIST:
			u.Encode(s.list(u)...)
		case irc.LUSERS:
			u.Encode(s.lusers(u)...)
		case irc.TOPIC:
			if s.okParams(u, msg, 1) {
				s.topic(u, msg.Params[0], msg.Trailing)
			}
		case irc.WHO:
			if s.okParams(u, msg, 1) {
				opFilter := len(msg.Params) >= 2 && msg.Params[1] == "o"
				err = u.Encode(s.who(u, msg.Params[0], opFilter)...)
			}
		case irc.WHOIS:
			if s.okParams(u, msg, 1) {
				if _, ok := s.HasUser(msg.Params[0]); ok {
					err = u.Encode(s.whois(u, msg.Params[0])...)
				} else {
					s.encodeMessage(u, irc.ERR_NOSUCHNICK, msg.Params, "No such nick/channel")
				}
			}
		case irc.MODE:
			if s.okParams(u, msg, 1) {
				if len(msg.Params) > 1 {
					u.Encode(s.mode(u, msg.Params[0], msg.Params[1])...)
				} else {
					u.Encode(s.mode(u, msg.Params[0], "")...)
				}
			}
		case irc.ISON:
			if s.okParams(u, msg, 1) {
				err = u.Encode(s.ison(u, msg.Params...)...)
			}
		case irc.PRIVMSG:
			if s.okParams(u, msg, 1) {
				//fix clients not sending colons
				if len(msg.Params) > 1 {
					tr := strings.Join(msg.Params[1:], " ")
					msg.Trailing = msg.Trailing + tr
				}
				// empty message
				if msg.Trailing == "" {
					continue
				}
				query := msg.Params[0]
				if _, exists := s.HasChannel(query); exists {
					p := strings.Replace(query, "#", "", -1)
					msg.Trailing = strings.Replace(msg.Trailing, "\r", "", -1)
					// fix non-rfc clients
					if !strings.HasPrefix(msg.Trailing, ":") {
						if len(msg.Params) == 2 {
							msg.Trailing = msg.Params[1]
						}
					}
					// CTCP ACTION (/me)
					if strings.HasPrefix(msg.Trailing, "\x01ACTION ") {
						msg.Trailing = strings.Replace(msg.Trailing, "\x01ACTION ", "", -1)
						msg.Trailing = "*" + msg.Trailing + "*"
					}
					msg.Trailing += " ​"
					post := &model.Post{ChannelId: u.mc.GetChannelId(p), Message: msg.Trailing}
					u.mc.Client.CreatePost(post)
				} else if toUser, exists := s.HasUser(query); exists {
					if query == "mattermost" {
						go u.handleMMServiceBot(toUser, msg.Trailing)
						continue
					}
					if toUser.MmGhostUser {
						u.mc.SendDirectMessage(toUser.User, msg.Trailing)
						continue
					}
					err = s.encodeMessage(u, irc.PRIVMSG, []string{toUser.Nick}, msg.Trailing)
				} else {
					err = s.encodeMessage(u, irc.ERR_NOSUCHNICK, msg.Params, "No such nick/channel")
				}
			}
		case irc.NICK:
			if s.okParams(u, msg, 1) {
				s.RenameUser(u, msg.Params[0])
			}
		}
		if err != nil {
			logger.Errorf("handle encode error for %s: %s", u.ID(), err.Error())
			return
		}
	}
}

func (s *server) Add(u *User) (ok bool) {
	return s.add(u)
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

		// apparently NICK message can have a : prefix on connection
		// https://github.com/42wim/matterircd/issues/32
		if msg.Command == irc.NICK && msg.Trailing != "" {
			msg.Params = append(msg.Params, msg.Trailing)
		}
		if !s.okParams(u, msg, 1) {
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
			s.encodeMessage(u, irc.ERR_NICKNAMEINUSE, []string{u.Nick}, "Nickname is already in use")
			continue
		}

		return s.welcome(u)
	}
	return ErrHandshakeFailed
}
