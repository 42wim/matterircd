package irckit

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/mattermost/platform/model"
	"github.com/nlopes/slack"
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
	if u.mc != nil {
		u.mc.UpdateStatus(u.mc.User.Id, "online")
	}
	if u.sc != nil {
		u.sc.SetUserPresence("auto")
	}
	if msg.Trailing == "" {
		return s.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away")
	}
	if u.mc != nil {
		u.mc.UpdateStatus(u.mc.User.Id, "away")
	}
	if u.sc != nil {
		u.sc.SetUserPresence("away")
	}
	return s.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
}

func CmdInvite(s Server, u *User, msg *irc.Message) error {
	who := msg.Params[0]
	channel := msg.Params[1]
	other, ok := s.HasUser(who)
	if !ok {
		return nil
	}

	if u.mc != nil {
		channelName := strings.Replace(channel, "#", "", 1)
		id := u.mc.GetChannelId(channelName, "")
		if id == "" {
			return nil
		}
		_, resp := u.mc.Client.AddChannelMember(id, other.User)
		if resp.Error != nil {
			return resp.Error
		}
	}
	if u.sc != nil {
		if ch, exists := s.HasChannel(channel); exists {
			logger.Debugf("inviting %s to %s", other.User, strings.ToUpper(ch.ID()))
			if strings.HasPrefix(ch.ID(), "c") {
				u.sc.InviteUserToChannel(strings.ToUpper(ch.ID()), other.User)
			} else {
				u.sc.InviteUserToGroup(strings.ToUpper(ch.ID()), other.User)
			}
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
	channelName := strings.Replace(channel, "#", "", 1)
	if u.mc != nil {
		id := u.mc.GetChannelId(channelName, "")
		if id == "" {
			return nil
		}
		_, resp := u.mc.Client.RemoveUserFromChannel(id, other.User)
		if resp.Error != nil {
			return resp.Error
		}
	}
	if u.sc != nil {
		if ch, exists := s.HasChannel(channel); exists {
			if strings.HasPrefix(ch.ID(), "c") {
				u.sc.KickUserFromChannel(strings.ToUpper(ch.ID()), other.User)
			} else {
				u.sc.KickUserFromGroup(strings.ToUpper(ch.ID()), other.User)
			}
		}
	}
	return nil
}

// CmdJoin is a handler for the /JOIN command.
func CmdJoin(s Server, u *User, msg *irc.Message) error {
	var (
		channelId string
		topic     string
		sync      func(string, string)
	)

	channels := strings.Split(msg.Params[0], ",")
	for _, channel := range channels {
		channelName := strings.Replace(channel, "#", "", 1)
		// you can only join existing channels
		if u.mc != nil {
			channelId = u.mc.GetChannelId(channelName, "")
			err := u.mc.JoinChannel(channelId)
			if err != nil {
				s.EncodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
				continue
			}
			logger.Debugf("Join channel %s, id %s, err: %v", channelName, channelId, err)
			topic = u.mc.GetChannelHeader(channelId)
			sync = u.syncMMChannel
		}
		if u.sc != nil {
			mychan, err := u.sc.JoinChannel(channelName)
			if err != nil {
				s.EncodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
				continue
			}
			channelId = mychan.ID
			logger.Debugf("Join channel %s, id %s, err: %v", channelName, channelId, err)
			topic = mychan.Topic.Value
			sync = u.syncSlackChannel
		}
		ch := s.Channel(channelId)
		ch.Topic(u, topic)
		sync(channelId, channelName)
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
	if u.mc != nil {
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
				Params:   []string{u.Nick, channelName, "0", strings.Replace(channel.Header, "\n", " | ", -1)},
				Trailing: "",
			})
		}
	}
	if u.sc != nil {
		groups, _ := u.sc.GetGroups(false)
		channels, _ := u.sc.GetChannels(false)
		for _, channel := range channels {
			channelName := "#" + channel.Name
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_LIST,
				Params:   []string{u.Nick, channelName, "0", strings.Replace(channel.Topic.Value, "\n", " | ", -1)},
				Trailing: "",
			})
		}
		for _, channel := range groups {
			channelName := "#" + channel.Name
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Command:  irc.RPL_LIST,
				Params:   []string{u.Nick, channelName, "0", strings.Replace(channel.Topic.Value, "\n", " | ", -1)},
				Trailing: "",
			})
		}
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
	if len(msg.Params) < 1 {
		return nil
	}

	for _, channel := range strings.Split(msg.Params[0], ",") {
		ch, exists := s.HasChannel(channel)
		if !exists {
			continue
		}
		ch.SendNamesResponse(u);
	}
	return nil
}

// CmdNick is a handler for the /NICK command.
func CmdNick(s Server, u *User, msg *irc.Message) error {
	// only update mattermost nick if we're logged in
	if u.mc != nil {
		err := u.mc.UpdateUserNick(msg.Params[0])
		if err != nil {
			s.EncodeMessage(u, irc.ERR_ERRONEUSNICKNAME, []string{u.Nick}, "Erroneus nickname")
			return err
		}
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
			if u.mc != nil {
				u.mc.Client.RemoveUserFromChannel(ch.ID(), u.mc.User.Id)
			}
			if u.sc != nil {
				if strings.HasPrefix(ch.ID(), "c") {
					u.sc.LeaveChannel(strings.ToUpper(ch.ID()))
				} else {
					u.sc.LeaveGroup(strings.ToUpper(ch.ID()))
				}
			}
		}
		// part all other (ghost)users on the channel
		for _, k := range ch.Users() {
			ch.Part(k, "")
		}
	}
	if u.mc != nil {
		u.mc.UpdateChannels()
	}
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
		msg.Trailing = msg.Trailing + tr
	}
	// empty message
	if msg.Trailing == "" {
		return nil
	}
	if msg.Params[0] == "&users" {
		return nil
	}
	query := msg.Params[0]

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
		msg.Trailing = strings.Replace(msg.Trailing, "\x01", "", -1)
		msg.Trailing = "*" + msg.Trailing + "*"
	}
	// strip IRC colors
	re := regexp.MustCompile(`[[:cntrl:]](?:\d{1,2}(?:,\d{1,2})?)?`)
	msg.Trailing = re.ReplaceAllString(msg.Trailing, "")

	if ch, exists := s.HasChannel(query); exists {
		if ch.Service() == "slack" {
			np := slack.NewPostMessageParameters()
			np.AsUser = true
			np.LinkNames = 1
			np.Username = u.User
			np.Attachments = append(np.Attachments, slack.Attachment{CallbackID: "matterircd_" + u.sinfo.User.ID})
			_, _, err := u.sc.PostMessage(strings.ToUpper(ch.ID()), msg.Trailing, np)
			if err != nil {
				return err
			}
		}
		if ch.Service() == "mattermost" {
			props := make(map[string]interface{})
			props["matterircd_"+u.mc.User.Id] = true
			post := &model.Post{ChannelId: ch.ID(), Message: msg.Trailing, Props: props}
			_, resp := u.mc.Client.CreatePost(post)
			if resp.Error != nil {
				u.MsgSpoofUser("mattermost", "msg: "+msg.Trailing+" could not be send: "+resp.Error.Error())
			}
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
		if toUser.MmGhostUser {
			if u.sc != nil {
				_, _, dchannel, err := u.sc.OpenIMChannel(toUser.User)
				if err != nil {
					return err
				}
				np := slack.NewPostMessageParameters()
				np.AsUser = true
				np.Username = u.User
				np.Attachments = append(np.Attachments, slack.Attachment{CallbackID: "matterircd_" + u.sinfo.User.ID})
				_, _, err = u.sc.PostMessage(dchannel, msg.Trailing, np)
				if err != nil {
					return err
				}
			}
			if u.mc != nil {
				u.mc.SendDirectMessage(toUser.User, msg.Trailing)
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
	if u.mc != nil {
		if u.mc.WsClient != nil {
			u.logoutFromMattermost()
		}
	}
	u.Conn.Close()
	return nil
}

// CmdTopic is a handler for the /TOPIC command.
func CmdTopic(s Server, u *User, msg *irc.Message) error {
	channelname := msg.Params[0]
	ch := s.Channel(channelname)
	if msg.Trailing != "" {
		ch.Topic(u, msg.Trailing)
		if u.mc != nil {
			u.mc.UpdateChannelHeader(ch.ID(), msg.Trailing)
		}
		if u.sc != nil {
			u.sc.SetChannelTopic(strings.ToUpper(ch.ID()), msg.Trailing)
		}
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
	//opFilter := len(msg.Params) >= 2 && msg.Params[1] == "o"
	mask := msg.Params[0]

	// TODO: Handle arbitrary masks, not just channels
	ch, exists := s.HasChannel(mask)
	if !exists {
		return nil
		//return u.Encode(endMsg)
	}

	r := make([]*irc.Message, 0, ch.Len()+1)
	statuses := make(map[string]string)
	if u.sc == nil {
		statuses = u.mc.GetStatuses()
	}

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
		var status string
		if u.sc == nil {
			status = u.mc.GetStatus(other.User)
		}
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
