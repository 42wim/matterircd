package irckit

import (
	"errors"

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
	// LoggedIn is true when authenticated (logged in) against mattermost
	LoggedIn bool
}

type Commands interface {
	Add(Handler)
	Run(Server, *User, *irc.Message) error
}

// Commands is a registry for command handlers
type commands map[string]Handler

// Add registers a Handler. Will replace any existing handlers for the given Command.
func (cmds commands) Add(h Handler) {
	cmds[h.Command] = h
}

// Run executes an Handler to the irc.Message's Command.
func (cmds commands) Run(s Server, u *User, msg *irc.Message) error {
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
	if cmd.LoggedIn && u.sc != nil {
		return cmd.Call(s, u, msg)
	}
	// check if we're logged in
	if cmd.LoggedIn && (u.mc == nil || u.mc.User == nil) {
		return nil
	}
	return cmd.Call(s, u, msg)
}
