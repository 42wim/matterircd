package irckit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/42wim/matterircd/bridge"
	"github.com/mattermost/mattermost-server/v5/model"
)

type CommandHandler interface {
	handle(u *User, c *Command, args []string, service string)
}

// nolint:structcheck
type Command struct {
	handler   func(u *User, toUser *User, args []string, service string)
	minParams int
	maxParams int
	login     bool
}

func logout(u *User, toUser *User, args []string, service string) {
	if u.inprogress {
		u.MsgUser(toUser, "login or logout in progress. Please wait")
		return
	}
	u.br.Logout()
	u.logoutFrom(u.br.Protocol())
}

func login(u *User, toUser *User, args []string, service string) {
	if u.inprogress {
		u.MsgUser(toUser, "login or logout in progress. Please wait")
		return
	}

	if service == "slack" {
		var err error

		if len(args) != 1 && len(args) != 3 {
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass> or LOGIN <token>")
			return
		}

		if len(args) == 1 {
			u.Credentials.Token = args[len(args)-1]
		}

		if u.Credentials.Token == "help" {
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass> or LOGIN <token>")
			return
		}

		if len(args) == 3 {
			u.Credentials = bridge.Credentials{
				Team:  args[0],
				Login: args[1],
				Pass:  args[2],
			}
		}

		if len(args) == 4 {
			u.Credentials = bridge.Credentials{
				Team:  args[0],
				Login: args[1],
				Pass:  args[2],
				MFAToken:  args[3],
			}
		}

		if u.br != nil && u.br.Connected() {
			err = u.br.Logout()
			if err != nil {
				u.MsgUser(toUser, err.Error())
				return
			}
		}

		u.inprogress = true
		defer func() { u.inprogress = false }()

		err = u.loginTo("slack")
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}

		u.MsgUser(toUser, "login OK")
		if u.Credentials.Token != "" {
			u.MsgUser(toUser, "token used: "+u.Credentials.Token)
		}

		return
	}

	cred := bridge.Credentials{}
	datalen := 5

	if u.v.GetString("mattermost.DefaultTeam") != "" {
		cred.Team = u.v.GetString("mattermost.DefaultTeam")
		datalen--
	}

	if u.v.GetString("mattermost.DefaultServer") != "" {
		cred.Server = u.v.GetString("mattermost.DefaultServer")
		datalen--
	}

	if len(args) >= datalen {
		logger.Debugf("args_len: %d", len(args))
		logger.Debugf("team: %d", cred.Team)
		logger.Debugf("srv: %d", cred.Server)
		if strings.Contains(args[len(args)-1], "MFAToken=") {
			var MFAToken_str = strings.Split(args[len(args)-1], "=")
			cred.MFAToken = MFAToken_str[1]
			cred.Pass = args[len(args)-2]
			cred.Login = args[len(args)-3]
		} else {
			cred.Pass = args[len(args)-1]
			cred.Login = args[len(args)-2]
		}
		// no default server or team specified
		if cred.Server == "" && cred.Team == "" {
			cred.Server = args[0]
		}

		if cred.Team == "" {
			cred.Team = args[1]
		}

		if cred.Server == "" {
			cred.Server = args[0]
		}
	}

	// incorrect arguments
	if len(args) < datalen {
		switch {
		// no server or team
		case cred.Team != "" && cred.Server != "":
			u.MsgUser(toUser, "need LOGIN <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
            u.MsgUser(toUser, "when using a mfa token use LOGIN <login> <pass> MFAToken=<yourmfatoken>")
		// server missing
		case cred.Team != "":
			u.MsgUser(toUser, "need LOGIN <server> <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
            u.MsgUser(toUser, "when using a mfa token use LOGIN <server> <login> <pass> MFAToken=<yourmfatoken>")
		// team missing
		case cred.Server != "":
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
            u.MsgUser(toUser, "when using a mfa token use LOGIN <team> <login> <pass> MFAToken=<yourmfatoken>")
		default:
			u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
            u.MsgUser(toUser, "when using a mfa token use LOGIN <server> <team> <login> <pass> MFAToken=<yourmfatoken>")
		}

		return
	}

	if !u.isValidServer(cred.Server, service) {
		u.MsgUser(toUser, "not allowed to connect to "+cred.Server)
		return
	}

	if u.br != nil && u.br.Connected() {
		err := u.br.Logout()
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}
	}

	u.Credentials = cred

	err := u.loginTo("mattermost")
	if err != nil {
		u.MsgUser(toUser, err.Error())
		return
	}

	u.MsgUser(toUser, "login OK")
}

func search(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	list := u.br.SearchPosts(strings.Join(args, " "))
	if list == nil || len(list.(*model.PostList).Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	postlist := list.(*model.PostList)

	for i := len(postlist.Order) - 1; i >= 0; i-- {
		if postlist.Posts[postlist.Order[i]].DeleteAt > postlist.Posts[postlist.Order[i]].CreateAt {
			continue
		}

		timestamp := time.Unix(postlist.Posts[postlist.Order[i]].CreateAt/1000, 0).Format("January 02, 2006 15:04")
		channelname := u.br.GetChannelName(postlist.Posts[postlist.Order[i]].ChannelId)

		nick := u.br.GetUser(postlist.Posts[postlist.Order[i]].UserId).Nick

		u.MsgUser(toUser, "#"+channelname+" <"+nick+"> "+timestamp)
		u.MsgUser(toUser, strings.Repeat("=", len("#"+channelname+" <"+nick+"> "+timestamp)))

		for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
			if post != "" {
				u.MsgUser(toUser, post)
			}
		}

		if len(postlist.Posts[postlist.Order[i]].FileIds) > 0 {
			for _, fname := range u.br.GetFileLinks(postlist.Posts[postlist.Order[i]].FileIds) {
				u.MsgUser(toUser, "download file - "+fname)
			}
		}

		u.MsgUser(toUser, "")
		u.MsgUser(toUser, "")
	}
}

func searchUsers(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	users, err := u.br.SearchUsers(strings.Join(args, " "))
	if err != nil {
		u.MsgUser(toUser, fmt.Sprint("Error", err.Error()))
		return
	}

	for _, user := range users {
		u.MsgUser(toUser, fmt.Sprint(user.Nick, user.FirstName, user.LastName))
	}
}

func scrollback(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	if len(args) != 2 {
		u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
		u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
		return
	}

	limit, err := strconv.Atoi(args[1])
	if err != nil {
		u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
		u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
		return
	}

	if !strings.Contains(args[0], "#") {
		u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
		u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
		return
	}

	args[0] = strings.ReplaceAll(args[0], "#", "")

	list := u.br.GetPosts(u.br.GetChannelID(args[0], u.br.GetMe().TeamID), limit)
	if list == nil || len(list.(*model.PostList).Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	postlist := list.(*model.PostList)

	for i := len(postlist.Order) - 1; i >= 0; i-- {
		p := postlist.Posts[postlist.Order[i]]
		ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

		nick := u.br.GetUser(p.UserId).Nick

		for _, post := range strings.Split(p.Message, "\n") {
			if post != "" {
				u.MsgUser(toUser, "["+ts.Format("2006-01-02 15:04")+"]"+" <"+nick+"> "+post)
			}
		}

		if len(p.FileIds) > 0 {
			for _, fname := range u.br.GetFileLinks(p.FileIds) {
				u.MsgUser(toUser, "["+ts.Format("2006-01-02 15:04")+"]"+" <"+nick+"> download file - "+fname)
			}
		}
	}
}

func updatelastviewed(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	channelID := ""

	if len(args) != 1 {
		u.MsgUser(toUser, "need UPDATELASTVIEWED <channel>")
		u.MsgUser(toUser, "e.g. UPDATELASTVIEWED #bugs")
		return
	}

	if strings.Contains(args[0], "#") {
		args[0] = strings.ReplaceAll(args[0], "#", "")

		channelID = u.br.GetChannelID(args[0], u.br.GetMe().TeamID)
		if channelID == "" {
			u.MsgUser(toUser, "channel does not exist")
			return
		}
	} else if updateUser, exists := u.Srv.HasUser(args[0]); exists && updateUser.Ghost {
		err := u.br.UpdateLastViewedUser(updateUser.User)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("updatelastviewed for %#v failed: %s", updateUser.User, err))
			return
		}
		return
	} else {
		u.MsgUser(toUser, fmt.Sprintf("user %s does not exist", args[0]))
		return
	}

	u.br.UpdateLastViewed(channelID)
	u.MsgUser(toUser, fmt.Sprintf("set viewed for %s", args[0]))
}

var cmds = map[string]Command{
	"logout":           {handler: logout, login: true, minParams: 0, maxParams: 0},
	"login":            {handler: login, minParams: 2, maxParams: 5},
	"search":           {handler: search, login: true, minParams: 1, maxParams: -1},
	"searchusers":      {handler: searchUsers, login: true, minParams: 1, maxParams: -1},
	"scrollback":       {handler: scrollback, login: true, minParams: 2, maxParams: 2},
	"updatelastviewed": {handler: updatelastviewed, login: true, minParams: 1, maxParams: 1},
}

func (u *User) handleServiceBot(service string, toUser *User, msg string) {
	// func (u *User) handleMMServiceBot(toUser *User, msg string) {
	commands, err := parseCommandString(msg)
	if err != nil {
		u.MsgUser(toUser, fmt.Sprintf("\"%s\" is improperly formatted", msg))
		return
	}

	cmd, ok := cmds[strings.ToLower(commands[0])]
	if !ok {
		keys := make([]string, 0)
		for k := range cmds {
			keys = append(keys, k)
		}
		u.MsgUser(toUser, "possible commands: "+strings.Join(keys, ", "))
		u.MsgUser(toUser, "<command> help for more info")
		return
	}

	if cmd.login {
		if u.br == nil {
			u.MsgUser(toUser, "You're not logged in. Use LOGIN first.")
			return
		}
	}
	/*
		if cmd.minParams > len(commands[1:]) {
			u.MsgUser(toUser, fmt.Sprintf("%s requires at least %v arguments", commands[0], cmd.minParams))
			return
		}
	*/
	if cmd.maxParams > -1 && len(commands[1:]) > cmd.maxParams {
		u.MsgUser(toUser, fmt.Sprintf("%s takes at most %v arguments", commands[0], cmd.maxParams))
		return
	}

	cmd.handler(u, toUser, commands[1:], service)
}

func parseCommandString(line string) ([]string, error) {
	args := []string{}
	buf := ""
	var escaped, doubleQuoted, singleQuoted bool

	got := false

	for _, r := range line {
		// If the string is escaped
		if escaped {
			buf += string(r)
			escaped = false
			continue
		}

		// If "\"
		if r == '\\' {
			if singleQuoted {
				buf += string(r)
			} else {
				escaped = true
			}
			continue
		}

		// If it is whitespace
		if unicode.IsSpace(r) {
			if singleQuoted || doubleQuoted {
				buf += string(r)
			} else if got {
				args = append(args, buf)
				buf = ""
				got = false
			}
			continue
		}
		// If Quoted
		switch r {
		case '"':
			if !singleQuoted {
				doubleQuoted = !doubleQuoted
				continue
			}
		case '\'':
			if !doubleQuoted {
				singleQuoted = !singleQuoted
				continue
			}
		}
		got = true
		buf += string(r)
	}

	if got {
		args = append(args, buf)
	}

	if escaped || singleQuoted || doubleQuoted {
		return nil, errors.New("invalid command line string")
	}

	return args, nil
}
