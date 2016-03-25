package irckit

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sorcix/irc"
)

// The error returned when an invalid command is issued.
var ErrUnknownCommand = errors.New("unknown command")

// Handler is a container for an irc.Message handler.
type Handler struct {
	// Command is the IRC command that Call handles.
	Command string
	// Handler is a function that takes the server, user who sent the message, and a message to perform some command.
	Call func(s Server, u *User, msg *irc.Message) error
	// MinParams is the minimum number of params required on the message.
	MinParams int
	// TODO: Add mode constraints?
}

// Commands is a registry for command handlers
type Commands map[string]Handler

// Add registers a Handler
func (cmds Commands) Add(h Handler) {
	cmds[h.Command] = h
}

// Run executes an Handler to the irc.Message's Command.
func (cmds Commands) Run(s Server, u *User, msg *irc.Message) error {
	cmd, ok := cmds[msg.Command]
	if !ok {
		return ErrUnknownCommand
	}
	if len(msg.Params) < cmd.MinParams {
		return u.Encode(&irc.Message{
			Prefix:  s.Prefix(),
			Command: irc.ERR_NEEDMOREPARAMS,
			Params:  []string{msg.Command},
		})
	}
	return cmd.Call(s, u, msg)
}

// CmdPart is a handler for the /PART command.
func CmdPart(s Server, u *User, msg *irc.Message) error {
	// TODO: Handle 0
	channels := strings.Split(msg.Params[0], ",")
	for _, chName := range channels {
		ch, exists := s.HasChannel(chName)
		if !exists {
			u.Encode(&irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.ERR_NOSUCHCHANNEL,
				Params:   []string{chName},
				Trailing: "No such channel",
			})
			continue
		}
		ch.Part(u, msg.Trailing)
	}
	return nil
}

// CmdQuit is a handler for the /QUIT command.
func CmdQuit(s Server, u *User, msg *irc.Message) error {
	u.Encode(&irc.Message{
		Prefix:   u.Prefix(),
		Command:  irc.QUIT,
		Trailing: msg.Trailing,
	})
	u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.ERROR,
		Trailing: "You will be missed.",
	})
	s.Publish(&event{QuitEvent, s, nil, u, msg})
	return nil
}

// CmdPing is a handler for the /PING command.
func CmdPing(s Server, u *User, msg *irc.Message) error {
	return u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.PONG,
		Params:   []string{s.Name()},
		Trailing: msg.Trailing,
	})
}

// CmdJoin is a handler for the /JOIN command.
func CmdJoin(s Server, u *User, msg *irc.Message) error {
	// TODO: Handle invite-only
	/*
		return u.Encode(&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.ERR_INVITEONLYCHAN,
			Trailing: "Cannot join channel (+i)",
		})
	*/
	channels := strings.Split(msg.Params[0], ",")
	for _, channel := range channels {
		// XXX: Handle no create permission.
		ch := s.Channel(channel)
		err := ch.Join(u)
		if err == nil {
			s.Publish(&event{JoinEvent, s, ch, u, msg})
		}
	}
	return nil
}

// CmdMotd is a handler for the /MOTD command.
func CmdMotd(s Server, u *User, _ *irc.Message) error {
	motd := s.Motd()
	r := make([]*irc.Message, 0, len(motd)+2)
	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_MOTDSTART,
		Params:   []string{u.Nick},
		Trailing: fmt.Sprintf("- %s Message of the Day -", s.Name()),
	})

	for _, line := range motd {
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

	return u.Encode(r...)
}

// CmdNames is a handler for the /NAMES command.
func CmdNames(s Server, u *User, msg *irc.Message) error {
	// TODO: Handle multiple channels? Queries?
	channels := msg.Params

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
	return u.Encode(r...)
}

// CmdWho is a handler for the /WHO command.
func CmdWho(s Server, u *User, msg *irc.Message) error {
	// TODO: Use opFilter
	//opFilter := len(msg.Params) >= 2 && msg.Params[1] == "o"
	mask := msg.Params[0]

	endMsg := &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick, mask},
		Command:  irc.RPL_ENDOFWHO,
		Trailing: "End of /WHO list.",
	}

	// TODO: Handle arbitrary masks, not just channels
	ch, exists := s.HasChannel(mask)
	if !exists {
		return u.Encode(endMsg)
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
	return u.Encode(r...)
}

// CmdIson is a handler for the /ISON command.
func CmdIson(s Server, u *User, msg *irc.Message) error {
	nicks := msg.Params
	on := make([]string, 0, len(nicks))
	for _, nick := range nicks {
		if _, ok := s.HasUser(nick); ok {
			on = append(on, nick)
		}
	}

	return u.Encode(
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_ISON,
			Params:   []string{u.Nick},
			Trailing: strings.Join(on, " "),
		},
	)
}

// CmdPrivMsg is a handler for the /PRIVMSG command.
func CmdPrivMsg(s Server, u *User, msg *irc.Message) error {
	query := msg.Params[0]
	if toChan, exists := s.HasChannel(query); exists {
		toChan.Message(u, msg.Trailing)
		s.Publish(&event{ChanMsgEvent, s, toChan, u, msg})
	} else if toUser, exists := s.HasUser(query); exists {
		s.Publish(&event{UserMsgEvent, s, nil, u, msg})
		toUser.Encode(&irc.Message{
			Prefix:   u.Prefix(),
			Command:  irc.PRIVMSG,
			Params:   []string{toUser.Nick},
			Trailing: msg.Trailing,
		})
	} else {
		return u.Encode(&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.ERR_NOSUCHNICK,
			Params:   msg.Params,
			Trailing: "No such nick/channel",
		})
	}
	return nil
}

// CmdNick is a handler for the /NICK command.
func CmdNick(s Server, u *User, msg *irc.Message) error {
	s.RenameUser(u, msg.Params[0])
	return nil
}
