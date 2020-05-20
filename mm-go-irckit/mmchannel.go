package irckit

import (
	"strings"

	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
)

func (ch *channel) Spoof(from string, text string, cmd string) {
	text = wordwrap.String(text, 440)
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) == 0 {
			continue
		}

		msg := &irc.Message{
			Prefix:   &irc.Prefix{Name: from, User: from, Host: from},
			Command:  cmd,
			Params:   []string{ch.name},
			Trailing: l + "\n",
		}
		ch.mu.RLock()
		for to := range ch.usersIdx {
			to.Encode(msg)
		}
		ch.mu.RUnlock()
	}
}

func (ch *channel) SpoofMessage(from string, text string) {
	ch.Spoof(from, text, irc.PRIVMSG)
}

func (ch *channel) SpoofNotice(from string, text string) {
	ch.Spoof(from, text, irc.NOTICE)
}
