package irckit

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/42wim/matterircd/utils"
	"github.com/sorcix/irc"
)

func DefaultCommands() Commands {
	cmds := commands{}

	cmds.Add(Handler{Command: irc.AWAY, Call: CmdAway, LoggedIn: true})
	cmds.Add(Handler{Command: irc.CAP, Call: CmdCap})
	cmds.Add(Handler{Command: irc.ISON, Call: CmdIson})
	cmds.Add(Handler{Command: irc.INVITE, Call: CmdInvite, LoggedIn: true, MinParams: 2})
	cmds.Add(Handler{Command: irc.JOIN, Call: CmdJoin, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.KICK, Call: CmdKick, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.LIST, Call: CmdList, LoggedIn: true})
	cmds.Add(Handler{Command: irc.LUSERS, Call: CmdLusers})
	cmds.Add(Handler{Command: irc.MODE, Call: CmdMode, MinParams: 1, LoggedIn: true})
	cmds.Add(Handler{Command: irc.MOTD, Call: CmdMotd})
	cmds.Add(Handler{Command: irc.NAMES, Call: CmdNames, MinParams: 0, LoggedIn: true})
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
	if msg.Trailing == "" {
		_ = u.br.SetStatus(u.ctx, "online")
		return s.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away")
	}

	_ = u.br.SetStatus(u.ctx, "away")
	return s.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
}

func CmdCap(s Server, u *User, msg *irc.Message) error {
	if len(msg.Params) < 1 {
		return nil
	}

	subcommand := strings.ToUpper(msg.Params[0])
	switch subcommand {
	case irc.CAP_LS, irc.CAP_LIST:
		params := fmt.Sprintf("* %s :message-tags", subcommand)
		_ = s.EncodeMessage(u, irc.CAP, []string{params}, "")

	case irc.CAP_REQ:
		reqCaps := msg.Trailing
		if reqCaps == "" && len(msg.Params) > 1 {
			reqCaps = msg.Params[1]
		}

		if strings.Contains(reqCaps, "message-tags") {
			if u.Caps == nil {
				u.Caps = make(map[string]bool)
			}

			u.Caps["message-tags"] = true

			// Ensure we are sending the ACK EXACTLY as IRCv3 expects.
			// IRC formatting: CAP * ACK :message-tags
			params := fmt.Sprintf("* ACK :%s", reqCaps)
			_ = s.EncodeMessage(u, irc.CAP, []string{params}, "")
		} else {
			params := fmt.Sprintf("* NAK :%s", reqCaps)
			_ = s.EncodeMessage(u, irc.CAP, []string{params}, "")
		}

	case irc.CAP_END:
		// Do nothing
	default:
		params := fmt.Sprintf("* %s", subcommand)
		_ = s.EncodeMessage(u, "410", []string{params}, "Invalid or unsupported CAP command")
	}

	return nil
}

func CmdInvite(s Server, u *User, msg *irc.Message) error {
	who := msg.Params[0]
	channel := msg.Params[1]
	other, ok := s.HasUser(who)
	if !ok {
		return nil
	}

	if ch, exists := s.HasChannel(channel); exists {
		logger.Tracef("inviting %s to %s", other.User, ch.ID())
		err := u.br.Invite(u.ctx, ch.ID(), other.User)
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
		err := u.br.Kick(u.ctx, ch.ID(), other.User)
		if err != nil {
			return err
		}
	}

	return nil
}

// CmdJoin is a handler for the /JOIN command.
func CmdJoin(s Server, u *User, msg *irc.Message) error {
	channels := strings.Split(msg.Params[0], ",")
	for _, channel := range channels {
		channelName := strings.Replace(channel, "#", "", 1)
		if channelName == "&messages" || channelName == "&users" { //nolint:goconst
			continue
		}

		channelID, _, err := u.br.Join(u.ctx, channelName)
		if err != nil {
			logger.Errorf("Cannot join channel %s, id %s, err: %v", channelName, channelID, err)
			s.EncodeMessage(u, irc.ERR_INVITEONLYCHAN, []string{u.Nick, channel}, "Cannot join channel (+i)")
			continue
		}

		logger.Tracef("Join channel %s, id %s, err: %v", channelName, channelID, err)

		ch := s.Channel(channelID)

		err = u.syncChannel(channelID, channelName)
		if err == nil {
			_ = ch.Join(u)

			continue
		}

		// The error path: log, notify, and safely degrade by joining anyway
		logger.Warnf("Failed to sync nicklist for manual join to %s: %v", channelName, err)

		errMsg := fmt.Sprintf("Warning: Failed to fetch the user list for %s (timeout). You have joined the channel, but the nicklist may be incomplete.", channel)

		if svc, ok := s.HasUser(u.br.Protocol()); ok {
			u.MsgUser(svc, errMsg)
		} else {
			_ = s.EncodeMessage(u, "NOTICE", []string{u.Nick}, errMsg)
		}

		_ = ch.Join(u)
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

	info, err := u.br.List(u.ctx)
	if err != nil {
		return err
	}

	channelNames := make([]string, 0, len(info))
	for name := range info {
		channelNames = append(channelNames, name)
	}
	sort.Strings(channelNames)

	for _, channelName := range channelNames {
		topic := info[channelName]
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
		Command:  irc.RPL_LISTEND, // nolint:misspell
		Trailing: "End of /LIST",
	})
	return u.Encode(r...)
}

// CmdLusers is a handler for the /LUSERS command.
func CmdLusers(s Server, u *User, msg *irc.Message) error {
	var userCount, channelCount int

	if u.br != nil {
		userCount = u.br.GetUserCount()
		channelCount = u.br.GetChannelCount()
	} else {
		userCount = s.UserCount()
	}

	var (
		users    = strconv.Itoa(userCount)
		clients  = strconv.Itoa(s.ClientCount())
		channels = strconv.Itoa(channelCount)
	)

	return u.Encode(
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LUSERCLIENT,
			Params:   []string{u.Nick},
			Trailing: "There are " + users + " users and 0 services on 1 servers",
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LUSERCHANNELS,
			Params:   []string{u.Nick, channels},
			Trailing: "channels formed",
		},
		&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_LUSERME,
			Params:   []string{u.Nick},
			Trailing: "I have " + clients + " clients and 1 servers",
		},
	)
}

// CmdMode is a handler for the /MODE command.
func CmdMode(s Server, u *User, msg *irc.Message) error {
	modetype := ""
	channel := msg.Params[0]

	mode := "+"
	if s.Channel(channel).IsPrivate() {
		mode = "+pi"
	}

	r := []*irc.Message{}
	if len(msg.Params) > 1 {
		modetype = msg.Params[1]
	}
	switch modetype {
	case "":
		r = append(r, &irc.Message{
			Prefix:  s.Prefix(),
			Command: irc.RPL_CHANNELMODEIS,
			Params:  []string{u.Nick, channel, mode},
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
	var userCount, channelCount int

	if u.br != nil {
		userCount = u.br.GetUserCount()
		channelCount = u.br.GetChannelCount()
	} else {
		userCount = s.UserCount()
	}

	clientCount := s.ClientCount()
	motd := s.Motd()

	err := u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_MOTDSTART,
		Params:   []string{u.Nick},
		Trailing: fmt.Sprintf("- %s Message of the Day -", s.Name()),
	})
	if err != nil {
		return err
	}

	if IsDebugLevel() {
		motd = append(motd, "server is running in debugmode.")
	}

	for _, line := range motd {
		err = u.Encode(&irc.Message{
			Prefix:   s.Prefix(),
			Command:  irc.RPL_MOTD,
			Params:   []string{u.Nick},
			Trailing: fmt.Sprintf("- %s", line),
		})
		if err != nil {
			return err
		}
	}

	statsLine := fmt.Sprintf(
		"Current stats: %d IRC client(s) connected | %d total team users | %d active channels",
		clientCount, userCount, channelCount,
	)

	err = u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_MOTD,
		Params:   []string{u.Nick},
		Trailing: fmt.Sprintf("- %s", statsLine),
	})
	if err != nil {
		return err
	}

	return u.Encode(&irc.Message{
		Prefix:   s.Prefix(),
		Command:  irc.RPL_ENDOFMOTD,
		Params:   []string{u.Nick},
		Trailing: "End of /MOTD command.",
	})
}

// CmdNames is a handler for the /NAMES command.
func CmdNames(s Server, u *User, msg *irc.Message) error {
	if len(msg.Params) < 1 || msg.Params[0] == "**" {
		srv, ok := s.(*server)
		if !ok {
			return nil
		}

		srv.RLock()
		joined := make([]*channel, 0, len(srv.channels))

	ChannelLoop:
		for _, intfCh := range srv.channels {
			ch, ok := intfCh.(*channel)
			if !ok {
				continue
			}

			if !ch.HasUser(u) {
				continue
			}

			for _, existing := range joined {
				if existing.name == ch.name {
					continue ChannelLoop
				}
			}

			joined = append(joined, ch)
		}
		srv.RUnlock()

		sort.Slice(joined, func(i, j int) bool {
			return joined[i].name < joined[j].name
		})

		// Send the deduplicated, sorted responses
		for _, ch := range joined {
			_ = ch.SendNamesResponse(u)
		}

		return nil
	}

	channels := msg.Params[0]
	for len(channels) > 0 {
		var channel string
		idx := strings.IndexByte(channels, ',')
		if idx < 0 {
			channel = channels
			channels = ""
		} else {
			channel = channels[:idx]
			channels = channels[idx+1:]
		}

		if ch, exists := s.HasChannel(channel); exists {
			_ = ch.SendNamesResponse(u)
		}
	}
	return nil
}

// CmdNick is a handler for the /NICK command.
func CmdNick(s Server, u *User, msg *irc.Message) error {
	if u.br == nil {
		s.RenameUser(u, msg.Params[0])
		return nil
	}
	// only update mattermost nick if we're logged in
	err := u.br.Nick(u.ctx, msg.Params[0])
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
		// now part on mattermost/slack
		if !u.br.BridgeConfig().PartFake {
			err = u.br.Part(u.ctx, ch.ID())
			if err != nil {
				return err
			}
		}
		// part all other (ghost)users on the channel
		for _, k := range ch.Users() {
			ch.Part(k, "")
		}
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

// Use static initialization to optimize.
// Color - https://modern.ircdocs.horse/formatting.html#color
var colorRegExp = regexp.MustCompile(`\x03([019]?[0-9](,[019]?[0-9])?)?`)

// Hex Color - https://modern.ircdocs.horse/formatting.html#hex-color
var hexColorRegExp = regexp.MustCompile(`\x04[0-9a-fA-F]{6}`)

// CmdPrivMsg is a handler for the /PRIVMSG command.
func CmdPrivMsg(s Server, u *User, msg *irc.Message) error {
	var err error

	if len(msg.Params) > 1 {
		tr := strings.Join(msg.Params[1:], " ")
		msg.Params = []string{msg.Params[0]}
		msg.Trailing += tr
	}

	query := msg.Params[0]

	// empty message or in &users channel
	if msg.Trailing == "" || query == "&users" {
		return nil
	}

	msg.Trailing = strings.ReplaceAll(msg.Trailing, "\r", "")
	// fix non-rfc clients
	if !strings.HasPrefix(msg.Trailing, ":") {
		if len(msg.Params) == 2 {
			msg.Trailing = msg.Params[1]
		}
	}

	// strip IRC colors
	msg.Trailing = colorRegExp.ReplaceAllString(msg.Trailing, "")
	msg.Trailing = hexColorRegExp.ReplaceAllString(msg.Trailing, "")

	// Convert IRC formatting / emphasis to markdown.
	msg.Trailing = irc2markdown(msg.Trailing)

	// are we sending to a channel
	if ch, exists := s.HasChannel(query); exists {
		if ch.ID() == "&messages" || ch.ID() == "&users" {
			return nil
		}

		targetLock := u.getMsgChanLock(ch.ID())
		targetLock.Lock()
		defer targetLock.Unlock()

		if parseReactionToMsg(u, msg, ch.ID()) {
			return nil
		}

		if threadMsgChannel(u, msg, ch.ID()) {
			return nil
		}

		if parseModifyMsg(u, msg, ch.ID()) {
			return nil
		}

		msgID, err2 := u.br.MsgChannel(u.ctx, ch.ID(), msg.Trailing)
		if err2 != nil {
			u.MsgSpoofUser(u, u.br.Protocol(), "msg: "+msg.Trailing+" could not be sent "+err2.Error())
			return err2
		}

		u.msgLastMutex.Lock()
		u.msgLast[ch.ID()] = [2]string{msgID, ""}
		u.msgLastMutex.Unlock()
		u.saveLastViewedAt(ch.ID())

		if u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext {
			u.prefixContext(ch.ID(), msgID, "", "posted_self")
		}

		return nil
	}

	// or a user
	if toUser, exists := s.HasUser(query); exists {
		switch {
		case query == "mattermost" || query == "slack" || query == "mastodon": //nolint:goconst
			go u.handleServiceBot(query, toUser, msg.Trailing)
			msg.Trailing = "<redacted>"
		case toUser.Ghost, toUser.Me:
			logger.Tracef("sending message %s to user %s", msg.Trailing, toUser.User)
			// no messages when we're not logged in
			if u.br == nil {
				logger.Tracef("u.br was nil, ignored message")
				return nil
			}

			targetLock := u.getMsgChanLock(toUser.User)
			targetLock.Lock()
			defer targetLock.Unlock()

			if parseReactionToMsg(u, msg, toUser.User) {
				logger.Trace("matched parseReactionToMsg")
				return nil
			}

			// If the user is away, DND, offline, or has a custom status, reply with 301 RPL_AWAY
			// But after reactions as we don't care if we're reacting to an existing message.
			status, err := u.br.StatusUser(u.ctx, toUser.User)
			if err == nil && status != "" && status != "online" { //nolint:goconst
				status = utils.EmojiReplaceAliases(status, u.br.FormatterConfig().CustomEmoji)
				_ = s.EncodeMessage(u, irc.RPL_AWAY, []string{u.Nick, toUser.Nick}, status)
			}

			if threadMsgUser(u, msg, toUser.User) {
				logger.Trace("matched threadMsgUser")
				return nil
			}

			if parseModifyMsg(u, msg, toUser.User) {
				logger.Trace("matched parseModifyMsg")
				return nil
			}

			msgID, err2 := u.br.MsgUser(u.ctx, toUser.User, msg.Trailing)
			if err2 != nil {
				u.MsgSpoofUser(u, u.br.Protocol(), "msg: "+msg.Trailing+" could not be sent "+err2.Error())
				return err2
			}

			u.msgLastMutex.Lock()
			u.msgLast[toUser.User] = [2]string{msgID, ""}
			u.msgLastMutex.Unlock()
			u.saveLastViewedAt(u.br.GetChannelID(u.ctx, toUser.User, ""))

			if u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext {
				u.prefixContext(toUser.User, msgID, "", "posted_self")
			}

		default:
			err = s.EncodeMessage(u, irc.PRIVMSG, []string{toUser.Nick}, msg.Trailing)
		}
		return err
	}

	// no channel or user
	return s.EncodeMessage(u, irc.ERR_NOSUCHNICK, msg.Params, "No such nick/channel")
}

var parseReactionToMsgRegExp = regexp.MustCompile(`^\@\@([0-9a-f]{3}|[0-9a-z]{26})\s+([\-\+]):(\S+):\s*$`)

func parseReactionToMsg(u *User, msg *irc.Message, channelID string) bool {
	matches := parseReactionToMsgRegExp.FindStringSubmatch(msg.Trailing)
	if len(matches) != 4 {
		return false
	}

	msgID := matches[1]
	action := matches[2]
	emoji := matches[3]

	// matterircd style prefix/suffix contexts (e.g. 001 and fa2).
	if len(msgID) == 3 {
		id, err := strconv.ParseInt(msgID, 16, 0)
		if err != nil {
			logger.Errorf("couldn't parseint %s: %s", msgID, err)
		}

		u.msgMapIndexMutex.RLock()
		defer u.msgMapIndexMutex.RUnlock()

		if _, ok := u.msgMapIndex[channelID][int(id)]; ok {
			msgID = u.msgMapIndex[channelID][int(id)]
		}
	}

	if action == "-" {
		err := u.br.RemoveReaction(u.ctx, msgID, emoji)
		if err != nil {
			u.MsgSpoofUser(u, u.br.Protocol(), "reaction: "+emoji+" could not be removed: "+err.Error())
		}

		return true
	}

	err := u.br.AddReaction(u.ctx, msgID, emoji)
	if err != nil {
		u.MsgSpoofUser(u, u.br.Protocol(), "reaction: "+emoji+" could not be added: "+err.Error())
	}

	return true
}

var parseModifyMsgRegExp = regexp.MustCompile(`^s(\/(?:[0-9a-f]{3}|[0-9a-z]{26}|!!)?\/)(.*)`)

func parseModifyMsg(u *User, msg *irc.Message, channelID string) bool {
	matches := parseModifyMsgRegExp.FindStringSubmatch(msg.Trailing)
	text := msg.Trailing

	// only two so s/xxx/ which means a delete
	if len(matches) != 2 && len(matches) != 3 {
		return false
	}

	switch len(matches) {
	case 2:
		text = ""
	case 3:
		text = matches[2]
	}

	msgID := ""

	switch {
	// last message from user. Also support '!!' like shell's history
	// substitution for previous command.
	case matches[1] == "//" || matches[1] == "/!!/":
		u.msgLastMutex.RLock()
		defer u.msgLastMutex.RUnlock()
		if msgLast, ok := u.msgLast[channelID]; ok {
			msgID = msgLast[0]
		}
	// Mattermost message/thread ID (e.g. 'cfrakpwix7y8pgzux6ta76pm9c')
	case len(matches[1]) == 28:
		msgID = strings.ReplaceAll(matches[1], "/", "")
		u.msgLastMutex.Lock()
		defer u.msgLastMutex.Unlock()
		u.msgLast[channelID] = [2]string{msgID, ""}
	// matterircd message/thread ID (e.g. '004' and 'a12')
	case len(matches[1]) == 5:
		id, err := strconv.ParseInt(strings.ReplaceAll(matches[1], "/", ""), 16, 0)
		if err != nil {
			logger.Errorf("couldn't parseint %s: %s", matches[1], err)
		}

		u.msgMapIndexMutex.RLock()
		defer u.msgMapIndexMutex.RUnlock()

		if _, ok := u.msgMapIndex[channelID][int(id)]; ok {
			msgID = u.msgMapIndex[channelID][int(id)]

			u.msgLastMutex.Lock()
			defer u.msgLastMutex.Unlock()

			u.msgLast[channelID] = [2]string{msgID, ""}
		}
	}

	if msgID == "" {
		return false
	}

	err := u.br.ModifyPost(u.ctx, msgID, text)
	if err != nil {
		// probably a wrong id, just put it through as normally
		if strings.Contains(err.Error(), "permissions") {
			logger.Trace("parseModifyMsg triggered permissions error")
			return false
		}
		u.MsgSpoofUser(u, u.br.Protocol(), "msg: "+text+" could not be modified "+err.Error())
	} else {
		u.saveLastViewedAt(channelID)
	}

	return true
}

var parseThreadIDRegExp = regexp.MustCompile(`(?s)^\@\@(?:(!!|[0-9a-f]{3}|[0-9a-z]{26})\s)(.*)`)

func parseThreadID(u *User, msg *irc.Message, channelID string) (string, string) {
	matches := parseThreadIDRegExp.FindStringSubmatch(msg.Trailing)
	if len(matches) == 0 {
		return "", ""
	}
	const expected = 3
	if len(matches) != expected {
		logger.Errorf("parseThreadID: expected %d matches for re match against %q, got %d",
			expected, msg.Trailing, len(matches))
		return "", ""
	}
	switch {
	case matches[1] == "!!":
		u.msgLastMutex.RLock()
		defer u.msgLastMutex.RUnlock()
		msgLast, ok := u.msgLast[channelID]
		if !ok {
			return "", ""
		}
		parentID := msgLast[0]
		if msgLast[1] != "" {
			parentID = msgLast[1]
		}
		return parentID, matches[2]
	case len(matches[1]) == 3:
		id, err := strconv.ParseInt(matches[1], 16, 0)
		if err != nil {
			logger.Errorf("couldn't parseint %s: %s", matches[1], err)
			return "", ""
		}

		u.msgMapIndexMutex.RLock()
		defer u.msgMapIndexMutex.RUnlock()

		if _, ok := u.msgMapIndex[channelID][int(id)]; ok {
			return u.msgMapIndex[channelID][int(id)], matches[2]
		}
	case len(matches[1]) == 26:
		return matches[1], matches[2]
	default:
		logger.Errorf("parseThreadID: could not parse reply ID %q", matches[1])
		return "", ""
	}
	return "", ""
}

func threadMsgChannelUser(u *User, msg *irc.Message, channelID string, toUser bool) bool {
	threadID, text := parseThreadID(u, msg, channelID)
	if threadID == "" {
		return false
	}

	var msgID string
	var err error
	if toUser {
		msgID, err = u.br.MsgUserThread(u.ctx, channelID, threadID, text)
	} else {
		msgID, err = u.br.MsgChannelThread(u.ctx, channelID, threadID, text)
	}
	if err != nil {
		u.MsgSpoofUser(u, u.br.Protocol(), "msg: "+text+" could not be sent "+err.Error())
		return false
	}

	u.msgLastMutex.Lock()
	defer u.msgLastMutex.Unlock()
	u.msgLast[channelID] = [2]string{msgID, threadID}

	if !toUser {
		u.saveLastViewedAt(channelID)
	} else {
		u.saveLastViewedAt(u.br.GetChannelID(u.ctx, channelID, ""))
	}

	if u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext {
		u.prefixContext(channelID, msgID, threadID, "posted_self")
	}

	return true
}

func threadMsgChannel(u *User, msg *irc.Message, channelID string) bool {
	logger.Trace("entering threadMsgChannel")
	return threadMsgChannelUser(u, msg, channelID, false)
}

func threadMsgUser(u *User, msg *irc.Message, toUser string) bool {
	return threadMsgChannelUser(u, msg, toUser, true)
}

// CmdQuit is a handler for the /QUIT command.
func CmdQuit(s Server, u *User, msg *irc.Message) error {
	partMsg := msg.Trailing

	s.EncodeMessage(u, irc.QUIT, []string{}, partMsg)
	s.EncodeMessage(u, irc.ERROR, []string{}, "You will be missed.")

	s.Quit(u, partMsg)

	return nil
}

// CmdTopic is a handler for the /TOPIC command.
func CmdTopic(s Server, u *User, msg *irc.Message) error {
	channelname := msg.Params[0]
	if channelname == "" || !strings.HasPrefix(channelname, "#") {
		return nil
	}

	ch := s.Channel(channelname)

	if msg.Trailing != "" {
		err := u.br.SetTopic(u.ctx, ch.ID(), msg.Trailing)
		if err != nil {
			return s.EncodeMessage(u, irc.ERR_CHANOPRIVSNEEDED, msg.Params, err.Error())
		}

		ch.Topic(u, msg.Trailing)
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

	users := ch.Users()

	sort.Slice(users, func(i, j int) bool {
		return users[i].Nick < users[j].Nick
	})

	numUsers := len(users)

	r := make([]*irc.Message, numUsers+1)
	messages := make([]irc.Message, numUsers+1)
	paramsBacking := make([]string, numUsers*7)

	statuses, _ := u.br.StatusUsers(u.ctx)

	prefix := s.Prefix()
	myNick := u.Nick
	for i, other := range users {
		status := "H"
		if statuses[other.User] != "online" {
			status = "G"
		}

		pIdx := i * 7
		params := paramsBacking[pIdx : pIdx+7 : pIdx+7]
		params[0] = myNick
		params[1] = mask
		params[2] = other.User
		params[3] = other.Host
		params[4] = "*"
		params[5] = other.Nick
		params[6] = status

		// <me> <channel> <user> <host> <server> <nick> [H/G]: 0 <real>
		messages[i] = irc.Message{
			Prefix:   prefix,
			Params:   params,
			Command:  irc.RPL_WHOREPLY,
			Trailing: "0 " + other.Real,
		}

		r[i] = &messages[i]
	}

	messages[numUsers] = irc.Message{
		Prefix:   prefix,
		Params:   []string{myNick, mask},
		Command:  irc.RPL_ENDOFWHO,
		Trailing: "End of /WHO list.",
	}
	r[numUsers] = &messages[numUsers]

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

		status, _ := u.br.StatusUser(u.ctx, other.User)

		if status != "online" {
			r = append(r, &irc.Message{
				Prefix:   s.Prefix(),
				Params:   []string{u.Nick, other.Nick},
				Command:  irc.RPL_AWAY,
				Trailing: utils.EmojiReplaceAliases(status, u.br.FormatterConfig().CustomEmoji),
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

func irc2markdown(msg string) string {
	// https://modern.ircdocs.horse/formatting.html
	emphasisSupported := map[byte][]byte{
		'\x02': {'*', '*'}, // Bold      0x02  **   (**text**)
		'\x1d': {'_'},      // Italics   0x1D  _    (_text_)
		'\x11': {'`'},      // Monospace 0x11  `    (`text`)
		'\x0f': {' '},      // Reset     0x0F       (**text\x0f)
	}
	emphasisUnsupported := map[byte]string{
		'\x1f': "", // Underline 0x1f
		'\x1e': "", // Strikethr 0x1e
		'\x16': "", // Reverse Color
	}

	var buf []byte

	var currentEmphasis []byte
	for _, char := range []byte(msg) {
		var ok bool
		var emp []byte

		// Strip or ignore unsuppored IRC formatting / emphasis
		if _, ok = emphasisUnsupported[char]; ok {
			continue
		}

		// Not an IRC formatting / emphasis character so copy as is
		if emp, ok = emphasisSupported[char]; !ok {
			buf = append(buf, char)
			continue
		}

		// IRC reset so reset formatting
		if char == '\x0f' {
			// Close off any current formatting / emphasis
			for _, c := range currentEmphasis {
				buf = append(buf, emphasisSupported[c]...)
			}
			currentEmphasis = nil
			continue
		}

		buf = append(buf, emp...)

		// Closing emphasis, they're in pairs, remove for list of outstanding
		found := false
		var newEmphasis []byte
		for _, c := range currentEmphasis {
			if !found && c == char {
				found = true
				continue
			}
			newEmphasis = append(newEmphasis, c)
		}
		if found {
			currentEmphasis = newEmphasis
			continue
		}

		currentEmphasis = append([]byte{char}, currentEmphasis...)
	}

	// Close off any current formatting / emphasis
	for _, c := range currentEmphasis {
		buf = append(buf, emphasisSupported[c]...)
	}

	return string(buf)
}
