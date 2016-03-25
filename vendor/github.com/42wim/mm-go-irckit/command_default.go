package irckit

import "github.com/sorcix/irc"

func DefaultCommands() Commands {
	cmds := Commands{}

	cmds.Add(Handler{Command: irc.ISON, Call: CmdIson, MinParams: 1})
	cmds.Add(Handler{Command: irc.JOIN, Call: CmdJoin, MinParams: 1})
	cmds.Add(Handler{Command: irc.MOTD, Call: CmdMotd})
	cmds.Add(Handler{Command: irc.NAMES, Call: CmdNames, MinParams: 1})
	cmds.Add(Handler{Command: irc.NICK, Call: CmdNick, MinParams: 1})
	cmds.Add(Handler{Command: irc.PART, Call: CmdPart, MinParams: 1})
	cmds.Add(Handler{Command: irc.PING, Call: CmdPing})
	cmds.Add(Handler{Command: irc.PRIVMSG, Call: CmdPrivMsg, MinParams: 1})
	cmds.Add(Handler{Command: irc.QUIT, Call: CmdQuit})
	cmds.Add(Handler{Command: irc.WHO, Call: CmdWho, MinParams: 1})

	// (Sync this list with https://github.com/shazow/go-irckit/issues/11)
	//
	// Commands left to implement:
	// - [ ] ADMIN
	// - [ ] AWAY
	// - [ ] CNOTICE
	// - [ ] CPRIVMSG
	// - [ ] CONNECT
	// - [ ] DIE
	// - [ ] ENCAP
	// - [ ] ERROR
	// - [ ] HELP
	// - [ ] INFO
	// - [ ] INVITE
	// - [x] ISON
	// - [x] JOIN
	// - [ ] KICK
	// - [ ] KILL
	// - [ ] KNOCK
	// - [ ] LINKS
	// - [ ] LIST
	// - [ ] LUSERS
	// - [ ] MODE
	// - [x] MOTD
	// - [x] NAMES
	// - [ ] NAMESX
	// - [x] NICK
	// - [ ] NOTICE
	// - [ ] OPER
	// - [x] PART
	// - [ ] PASS
	// - [x] PING
	// - [x] PONG
	// - [x] PRIVMSG
	// - [x] QUIT
	// - [ ] REHASH
	// - [ ] RESTART
	// - [ ] RULES
	// - [ ] SERVER
	// - [ ] SERVICE
	// - [ ] SERVLIST
	// - [ ] SQUERY
	// - [ ] SQUIT
	// - [ ] SETNAME
	// - [ ] SILENCE
	// - [ ] STATS
	// - [ ] SUMMON
	// - [ ] TIME
	// - [ ] TOPIC
	// - [ ] TRACE
	// - [ ] UHNAMES
	// - [ ] USER
	// - [ ] USERHOST
	// - [ ] USERIP
	// - [ ] USERS
	// - [ ] VERSION
	// - [ ] WALLOPS
	// - [ ] WATCH
	// - [x] WHO
	// - [ ] WHOIS
	// - [ ] WHOWAS

	return cmds
}
