package irckit

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

func DefaultCommands() Commands {
	cmds := commands{}

	cmds.Add(Handler{Command: irc.AWAY, Call: CmdAway, LoggedIn: true})
	cmds.Add(Handler{Command: irc.ISON, Call: CmdIson, MinParams: 1})
	cmds.Add(Handler{Command: irc.JOIN, Call: CmdJoin, MinParams: 1, LoggedIn: true})
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
	u.mc.WsAway = false
	if msg.Trailing == "" {
		return s.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away")
	}
	u.mc.WsAway = true
	return s.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
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

// CmdJoin is a handler for the /JOIN command.
func CmdJoin(s Server, u *User, msg *irc.Message) error {
	var err error
	channels := strings.Split(msg.Params[0], ",")
	for _, channel := range channels {
		channelName := strings.Replace(channel, "#", "", 1)
		// you can only join existing channels
		channelId := u.mc.GetChannelId(channelName, "")
		err := u.mc.JoinChannel(channelId)
		if err != nil {
			s.EncodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
			continue
		}
		ch := s.Channel(channelId)
		ch.Topic(u, u.mc.GetChannelHeader(channelId))
		u.syncMMChannel(channelId, channelName)
		ch.Join(u)
	}
	return err
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
	for _, channel := range append(u.mc.GetChannels(), u.mc.GetMoreChannels()...) {
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		if strings.Contains(channel.Name, "__") {
			continue
		}
		channelName := "#" + channel.Name                
		// prefix channels outside of our team with team name
		if channel.TeamId != u.mc.Team.Id {
			channelName = u.mc.GetTeamName(channel.TeamId) + "/" + channel.Name
		}
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LIST,
			Params:   []string{u.Nick,channelName,"0",strings.Replace(channel.Header, "\n", " | ", -1)},
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
		{
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_CHANNELMODEIS,
				Params:   []string{u.Nick, channel},
				Trailing: " " + " ",
			})
		}
	case "b":
		{
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_ENDOFBANLIST,
				Params:   []string{u.Nick, channel},
				Trailing: "End of channel ban list",
			})
		}
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
	channels := msg.Params

	r := []*irc.Message{}
	for _, channel := range channels {
		ch, exists := s.HasChannel(channel)
		if !exists {
			continue
		}
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		r = append(r, &irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_NAMREPLY,
			Params:   []string{u.Nick, "=", channel},
			Trailing: strings.Join(ch.Names(), " "),
		})
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

// CmdNick is a handler for the /NICK command.
func CmdNick(s Server, u *User, msg *irc.Message) error {
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
		u.mc.Client.LeaveChannel(ch.ID())
		// part all other (ghost)users on the channel
		for _, k := range ch.Users() {
			ch.Part(k, "")
		}
	}
	u.mc.UpdateChannels()
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
		msg.Trailing = msg.Trailing + tr
	}
	// empty message
	if msg.Trailing == "" {
		return nil
	}
	query := msg.Params[0]
	if ch, exists := s.HasChannel(query); exists {
		//p := strings.Replace(query, "#", "", -1)
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
		post := &model.Post{ChannelId: ch.ID(), Message: msg.Trailing}
		_, err := u.mc.Client.CreatePost(post)
		if err != nil {
			u.MsgSpoofUser("mattermost", "msg: "+msg.Trailing+" could not be send: "+err.Error())
		}
	} else if toUser, exists := s.HasUser(query); exists {
		if query == "mattermost" {
			go u.handleMMServiceBot(toUser, msg.Trailing)
			return nil
		}
		if toUser.MmGhostUser {
			u.mc.SendDirectMessage(toUser.User, msg.Trailing)
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
	if u.mc != nil {
		if u.mc.WsClient != nil {
			u.logoutFromMattermost()
		}
	}
	return nil
}

// CmdTopic is a handler for the /TOPIC command.
func CmdTopic(s Server, u *User, msg *irc.Message) error {
	channelname := msg.Params[0]
	ch := s.Channel(channelname)
	ch.Topic(u, msg.Trailing)
	u.mc.UpdateChannelHeader(ch.ID(), msg.Trailing)
	return nil
}

// CmdWho is a handler for the /WHO command.
func CmdWho(s Server, u *User, msg *irc.Message) error {
	// TODO: Use opFilter
	//opFilter := len(msg.Params) >= 2 && msg.Params[1] == "o"
	mask := msg.Params[0]

	// TODO: Handle arbitrary masks, not just channels
	ch, exists := s.HasChannel(mask)
	if !exists {
		return nil
		//return u.Encode(endMsg)
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

		u.mc.UpdateUsers()
		status := u.mc.GetStatus(other.User)
		/*
			if status != "" {
				idle := (model.GetMillis() - lastActivityAt/1000)
				r = append(r, &irc.Message{
					Prefix:   s.Prefix(),
					Params:   []string{u.Nick, other.Nick, strconv.FormatInt(idle, 10), "0"},
					Command:  irc.RPL_WHOISIDLE,
					Trailing: "seconds idle, signon time",
				})
			}
		*/
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
