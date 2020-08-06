package irckit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sorcix/irc"
)

func DefaultCommands() Commands {
	cmds := commands{}

	cmds.Add(Handler{Command: irc.AWAY, Call: CmdAway, LoggedIn: true})
	cmds.Add(Handler{Command: irc.ISON, Call: CmdIson})
	cmds.Add(Handler{Command: irc.INVITE, Call: CmdInvite, LoggedIn: true, MinParams: 2})
	cmds.Add(Handler{Command: irc.JOIN, Call: CmdJoin, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.KICK, Call: CmdKick, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.LIST, Call: CmdList, LoggedIn: true})
	cmds.Add(Handler{Command: irc.LUSERS, Call: CmdLusers})
	cmds.Add(Handler{Command: irc.MODE, Call: CmdMode, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.MOTD, Call: CmdMotd})
	cmds.Add(Handler{Command: irc.NAMES, Call: CmdNames, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.NICK, Call: CmdNick, MinParams: 1})
	cmds.Add(Handler{Command: irc.PART, Call: CmdPart, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.PING, Call: CmdPing})
	cmds.Add(Handler{Command: irc.PRIVMSG, Call: CmdPrivMsg, MinParams: 1})
	cmds.Add(Handler{Command: irc.QUIT, Call: CmdQuit})
	cmds.Add(Handler{Command: irc.TOPIC, Call: CmdTopic, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.WHO, Call: CmdWho, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.WHOIS, Call: CmdWhois, MinParams: 1, LoggedIn: true})

	return &cmds
}

func CmdAway(s Server, u *User, msg *irc.Message) error {
	u.br.SetStatus("online")

	if msg.Trailing == "" {
		return s.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away")
	}

	u.br.SetStatus("away")

	return s.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
}

func CmdInvite(s Server, u *User, msg *irc.Message) error {
	who := msg.Params[0]
	channel := msg.Params[1]
	other, ok := s.HasUser(who)
	if !ok {
		return nil
	}

	if ch, exists := s.HasChannel(channel); exists {
		logger.Debugf("inviting %s to %s", other.User, ch.ID())
		err := u.br.Invite(ch.ID(), other.User)
		if err != nil {
			return err
		}
	}

	return nil
}

// CmdIson is a handler for the /ISON command.
func CmdIson(s Server, u *User, msg *irc.Message) error {
	nicks := msg.Params
	if len(msg.Params) == 0 {
		nicks = strings.Fields(msg.Trailing)
	}
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

func CmdKick(s Server, u *User, msg *irc.Message) error {
	channel := msg.Params[0]
	who := msg.Params[1]

	other, ok := s.HasUser(who)
	if !ok {
		return nil
	}

	if ch, exists := s.HasChannel(channel); exists {
		err := u.br.Kick(ch.ID(), other.User)
		if err != nil {
			return err
		}
	}

	return nil
}

// CmdJoin is a handler for the /JOIN command.
func CmdJoin(s Server, u *User, msg *irc.Message) error {
	var sync func(string, string)

	channels := strings.Split(msg.Params[0], ",")
	for _, channel := range channels {
		channelName := strings.Replace(channel, "#", "", 1)
		// you can only join existing channels
		var err error

		channelID, topic, err := u.br.Join(channelName)
		if err != nil {
			fmt.Println(err)
		}

		logger.Debugf("Join channel %s, id %s, err: %v", channelName, channelID, err)

		sync = u.syncMMChannel
		/*		if u.br.Protocol() == "mattermost" {
					sync = u.syncMMChannel
				} else {
					sync = u.syncSlackChannel
				}
		*/

		// if we joined, remove channel from exclude and add to include
		u.Cfg.JoinExclude = removeStringInSlice(channel, u.Cfg.JoinExclude)
		if len(u.Cfg.JoinInclude) > 0 {
			u.Cfg.JoinInclude = append(u.Cfg.JoinInclude, channel)
		}

		ch := s.Channel(channelID)
		ch.Topic(u, topic)

		sync(channelID, channelName)

		ch.Join(u)
	}

	return nil
}

// CmdList is a handler for the /LIST command.
func CmdList(s Server, u *User, msg *irc.Message) error {
	r := []*irc.Message{}
	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_LISTSTART,
		Params:   []string{u.Nick},
		Trailing: "Channel Users Topic",
	})

	info, err := u.br.List()
	if err != nil {
		return err
	}

	for channelName, topic := range info {
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LIST,
			Params:   []string{u.Nick, channelName, "0", topic},
			Trailing: "",
		})
	}

	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick},
		Command:  irc.RPL_LISTEND,
		Trailing: "End of /LIST",
	})
	return u.Encode(r...)
}

// CmdLusers is a handler for the /LUSERS command.
func CmdLusers(s Server, u *User, msg *irc.Message) error {
	return s.EncodeMessage(u, irc.RPL_LUSERCLIENT, []string{u.Nick},
		"There are "+strconv.Itoa(s.UserCount())+" users and "+strconv.Itoa(s.ChannelCount())+" channels on 1 server")
}

// CmdMode is a handler for the /MODE command.
func CmdMode(s Server, u *User, msg *irc.Message) error {
	modetype := ""
	channel := msg.Params[0]
	r := []*irc.Message{}
	if len(msg.Params) > 1 {
		modetype = msg.Params[1]
	}
	switch modetype {
	case "":
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_CHANNELMODEIS,
			Params:   []string{u.Nick, channel},
			Trailing: " " + " ",
		})
	case "b":
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_ENDOFBANLIST,
			Params:   []string{u.Nick, channel},
			Trailing: "End of channel ban list",
		})
	}
	return u.Encode(r...)
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
	if len(msg.Params) < 1 {
		return nil
	}

	for _, channel := range strings.Split(msg.Params[0], ",") {
		ch, exists := s.HasChannel(channel)
		if !exists {
			continue
		}
		ch.SendNamesResponse(u)
	}
	return nil
}

// CmdNick is a handler for the /NICK command.
func CmdNick(s Server, u *User, msg *irc.Message) error {
	// only update mattermost nick if we're logged in
	err := u.br.Nick(msg.Params[0])
	if err != nil {
		s.EncodeMessage(u, irc.ERR_ERRONEUSNICKNAME, []string{u.Nick}, "Erroneus nickname")
		return err
	}

	s.RenameUser(u, msg.Params[0])
	return nil
}

// CmdPart is a handler for the /PART command.
func CmdPart(s Server, u *User, msg *irc.Message) error {
	var err error

	channels := strings.Split(msg.Params[0], ",")
	for _, chName := range channels {
		// we can not leave & channels
		if strings.HasPrefix(chName, "&") {
			continue
		}

		ch, exists := s.HasChannel(chName)
		if !exists {
			err = s.EncodeMessage(u, irc.ERR_NOSUCHCHANNEL, []string{chName}, "No such channel")
			continue
		}
		// first part on irc
		ch.Part(u, msg.Trailing)
		// now part on mattermost
		if !u.Cfg.PartFake {
			err = u.br.Part(ch.ID())
			if err != nil {
				return err
			}
		}
		// part all other (ghost)users on the channel
		for _, k := range ch.Users() {
			ch.Part(k, "")
			// if we parted, remove channel from include
			u.Cfg.JoinInclude = removeStringInSlice(chName, u.Cfg.JoinInclude)
		}
	}

	u.br.UpdateChannels()

	return err
}

// CmdPing is a handler for the /PING command.
func CmdPing(s Server, u *User, msg *irc.Message) error {
	if len(msg.Params) > 0 {
		msg.Trailing = msg.Params[0]
	}
	s.EncodeMessage(u, irc.PONG, []string{s.Name()}, msg.Trailing)
	return nil
}

// CmdPrivMsg is a handler for the /PRIVMSG command.
func CmdPrivMsg(s Server, u *User, msg *irc.Message) error {
	var err error
	if len(msg.Params) > 1 {
		tr := strings.Join(msg.Params[1:], " ")
		msg.Params = []string{msg.Params[0]}
		msg.Trailing += tr
	}
	// empty message
	if msg.Trailing == "" {
		return nil
	}
	if msg.Params[0] == "&users" {
		return nil
	}
	query := msg.Params[0]

	// p := strings.Replace(query, "#", "", -1)
	msg.Trailing = strings.ReplaceAll(msg.Trailing, "\r", "")
	// fix non-rfc clients
	if !strings.HasPrefix(msg.Trailing, ":") {
		if len(msg.Params) == 2 {
			msg.Trailing = msg.Params[1]
		}
	}
	// CTCP ACTION (/me)
	if strings.HasPrefix(msg.Trailing, "\x01ACTION ") {
		msg.Trailing = strings.ReplaceAll(msg.Trailing, "\x01ACTION ", "")
		msg.Trailing = strings.ReplaceAll(msg.Trailing, "\x01", "")
		msg.Trailing = "*" + msg.Trailing + "*"
	}
	// strip IRC colors
	re := regexp.MustCompile(`\x03([019]?[0-9](,[019]?[0-9])?)?`)
	// re := regexp.MustCompile(`[[:cntrl:]](?:\d{1,2}(?:,\d{1,2})?)?`)
	msg.Trailing = re.ReplaceAllString(msg.Trailing, "")

	if ch, exists := s.HasChannel(query); exists {
		err = u.br.MsgChannel(ch.ID(), msg.Trailing)
		if err != nil {
			u.MsgSpoofUser(u, "mattermost", "msg: "+msg.Trailing+" could not be send: "+err.Error())
		}
	} else if toUser, exists := s.HasUser(query); exists {
		if query == "mattermost" {
			go u.handleServiceBot(query, toUser, msg.Trailing)
			msg.Trailing = "<redacted>"
			return nil
		}

		if query == "slack" {
			go u.handleServiceBot(query, toUser, msg.Trailing)
			msg.Trailing = "<redacted>"
			return nil
		}

		if toUser.Ghost {
			err = u.br.MsgUser(toUser.User, msg.Trailing)
			if err != nil {
				return err
			}

			return nil
		}

		err = s.EncodeMessage(u, irc.PRIVMSG, []string{toUser.Nick}, msg.Trailing)
	} else {
		err = s.EncodeMessage(u, irc.ERR_NOSUCHNICK, msg.Params, "No such nick/channel")
	}

	return err
}

// CmdQuit is a handler for the /QUIT command.
func CmdQuit(s Server, u *User, msg *irc.Message) error {
	partMsg := msg.Trailing

	s.EncodeMessage(u, irc.QUIT, []string{}, partMsg)
	s.EncodeMessage(u, irc.ERROR, []string{}, "You will be missed.")

	u.br.Logout()
	u.Srv.Logout(u)

	u.Conn.Close()

	return nil
}

// CmdTopic is a handler for the /TOPIC command.
func CmdTopic(s Server, u *User, msg *irc.Message) error {
	channelname := msg.Params[0]
	ch := s.Channel(channelname)

	if msg.Trailing != "" {
		ch.Topic(u, msg.Trailing)
		u.br.SetTopic(ch.ID(), msg.Trailing)
	} else {
		r := make([]*irc.Message, 0, ch.Len()+1)

		t := ch.GetTopic()
		if t == "" {
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Params:   []string{u.Nick, channelname},
				Command:  irc.RPL_NOTOPIC,
				Trailing: "No topic is set",
			})
		} else {
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Params:   []string{u.Nick, channelname},
				Command:  irc.RPL_TOPIC,
				Trailing: t,
			})
		}

		return u.Encode(r...)
	}

	return nil
}

// CmdWho is a handler for the /WHO command.
func CmdWho(s Server, u *User, msg *irc.Message) error {
	// TODO: Use opFilter
	// opFilter := len(msg.Params) >= 2 && msg.Params[1] == "o"
	mask := msg.Params[0]

	// TODO: Handle arbitrary masks, not just channels
	ch, exists := s.HasChannel(mask)
	if !exists {
		return nil
	}

	r := make([]*irc.Message, 0, ch.Len()+1)

	statuses, _ := u.br.StatusUsers()

	for _, other := range ch.Users() {
		status := "H"
		if statuses[other.User] != "online" {
			status = "G"
		}
		// <me> <channel> <user> <host> <server> <nick> [H/G]: 0 <real>
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Params:   []string{u.Nick, mask, other.User, other.Host, "*", other.Nick, status},
			Command:  irc.RPL_WHOREPLY,
			Trailing: "0 " + other.Real,
		})
	}

	r = append(r, &irc.Message{
		Prefix:   s.Prefix(),
		Params:   []string{u.Nick, mask},
		Command:  irc.RPL_ENDOFWHO,
		Trailing: "End of /WHO list.",
	})

	return u.Encode(r...)
}

// CmdWhois is a handler for the /WHOIS command.
func CmdWhois(s Server, u *User, msg *irc.Message) error {
	who := msg.Params[0]
	if _, ok := s.HasUser(msg.Params[0]); ok {
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

		status, _ := u.br.StatusUser(other.User)

		if status != "online" {
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Params:   []string{u.Nick, other.Nick},
				Command:  irc.RPL_AWAY,
				Trailing: status,
			})
		}

		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Params:   []string{u.Nick, other.Nick},
			Command:  irc.RPL_ENDOFWHOIS,
			Trailing: "End of /WHOIS list.",
		})
		return u.Encode(r...)
	}
	return s.EncodeMessage(u, irc.ERR_NOSUCHNICK, msg.Params, "No such nick/channel")
}
