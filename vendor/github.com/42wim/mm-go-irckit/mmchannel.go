package irckit

import (
	"github.com/sorcix/irc"
)

func (ch *channel) Spoof(from string, text string, cmd string) {
	for len(text) > 400 {
		msg := &irc.Message{
			Prefix:   &irc.Prefix{Name: from, User: from, Host: from},
			Command:  cmd,
			Params:   []string{ch.name},
			Trailing: text[:400] + "\n",
		}
		ch.mu.RLock()
		for to := range ch.usersIdx {
			to.Encode(msg)
		}
		ch.mu.RUnlock()
		text = text[400:]
	}
	msg := &irc.Message{
		Prefix:   &irc.Prefix{Name: from, User: from, Host: from},
		Command:  cmd,
		Params:   []string{ch.name},
		Trailing: text,
	}
	ch.mu.RLock()
	for to := range ch.usersIdx {
		to.Encode(msg)
	}
	ch.mu.RUnlock()
}

func (ch *channel) SpoofMessage(from string, text string) {
	ch.Spoof(from, text, irc.PRIVMSG)
}

func (ch *channel) SpoofNotice(from string, text string) {
	ch.Spoof(from, text, irc.NOTICE)
}
