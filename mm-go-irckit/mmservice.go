package irckit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mattermost/mattermost-server/model"
)

type CommandHandler interface {
	handle(u *User, c *Command, args []string, service string)
}

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
	switch service {
	case "mattermost":
		u.logoutFromMattermost()
	case "slack":
		u.logoutFromSlack()
	}
}

func login(u *User, toUser *User, args []string, service string) {
	if u.inprogress {
		u.MsgUser(toUser, "login or logout in progress. Please wait")
		return
	}
	if service == "slack" {
		var err error
		//fmt.Println(len(args))
		if len(args) != 1 && len(args) != 3 {
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass> or LOGIN <token>")
			return
		}
		//fmt.Println(len(args))
		if len(args) == 1 {
			u.Token = args[len(args)-1]
		}
		if u.Token == "help" {
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass> or LOGIN <token>")
			return
		}
		if len(args) == 3 {
			cred := &MmCredentials{}
			cred.Team = args[0]
			cred.Login = args[1]
			cred.Pass = args[2]
			u.Credentials = cred
		}
		if u.sc != nil {
			fmt.Println("login, starting logout")
			err := u.logoutFromSlack()
			if err != nil {
				u.MsgUser(toUser, err.Error())
				return
			}
		}
		if u.mc != nil {
			err := u.logoutFromMattermost()
			if err != nil {
				u.MsgUser(toUser, err.Error())
				return
			}
		}
		u.inprogress = true
		defer func() { u.inprogress = false }()
		u.sc, err = u.loginToSlack()
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}
		u.MsgUser(toUser, "login OK")
		if u.Credentials != nil && u.Token != "" {
			u.MsgUser(toUser, "token used: "+u.Token)
		}
		return
	}

	cred := &MmCredentials{}
	datalen := 4
	if u.Cfg.DefaultTeam != "" {
		cred.Team = u.Cfg.DefaultTeam
		datalen--
	}
	if u.Cfg.DefaultServer != "" {
		cred.Server = u.Cfg.DefaultServer
		datalen--
	}
	if len(args) == datalen {
		cred.Pass = args[len(args)-1]
		cred.Login = args[len(args)-2]
		// no default server or team specified
		if cred.Server == "" && cred.Team == "" {
			cred.Server = args[len(args)-4]
		}
		if cred.Team == "" {
			cred.Team = args[len(args)-3]
		}
		if cred.Server == "" {
			cred.Server = args[len(args)-3]
		}

	}

	// incorrect arguments
	if len(args) != datalen {
		// no server or team
		if cred.Team != "" && cred.Server != "" {
			u.MsgUser(toUser, "need LOGIN <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
			return
		}
		// server missing
		if cred.Team != "" {
			u.MsgUser(toUser, "need LOGIN <server> <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
			return
		}
		// team missing
		if cred.Server != "" {
			u.MsgUser(toUser, "need LOGIN <team> <login> <pass>")
			u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
			return
		}
		u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
		u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
		return
	}

	if !u.isValidMMServer(cred.Server) {
		u.MsgUser(toUser, "not allowed to connect to "+cred.Server)
		return
	}

	if u.sc != nil {
		fmt.Println("login, starting logout")
		err := u.logoutFromSlack()
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}
	}
	if u.mc != nil {
		err := u.logoutFromMattermost()
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}
	}
	u.Credentials = cred
	var err error
	u.mc, err = u.loginToMattermost()
	if err != nil {
		u.MsgUser(toUser, err.Error())
		return
	}
	u.mc.OnWsConnect = u.addUsersToChannels
	go u.mc.StatusLoop()
	u.MsgUser(toUser, "login OK")

}

func search(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}
	postlist := u.mc.SearchPosts(strings.Join(args, " "))
	if postlist == nil || len(postlist.Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}
	for i := len(postlist.Order) - 1; i >= 0; i-- {
		if postlist.Posts[postlist.Order[i]].DeleteAt > postlist.Posts[postlist.Order[i]].CreateAt {
			continue
		}
		timestamp := time.Unix(postlist.Posts[postlist.Order[i]].CreateAt/1000, 0).Format("January 02, 2006 15:04")
		channelname := u.mc.GetChannelName(postlist.Posts[postlist.Order[i]].ChannelId)

		nick := u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Username
		if (u.Cfg.PreferNickname &&
		    u.isValidNick(u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Nickname)) {
			nick = u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Nickname
		}
		u.MsgUser(toUser, "#"+channelname+" <"+nick+"> "+timestamp)
		u.MsgUser(toUser, strings.Repeat("=", len("#"+channelname+" <"+nick+"> "+timestamp)))
		for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
			if post != "" {
				u.MsgUser(toUser, post)
			}
		}
		if len(postlist.Posts[postlist.Order[i]].FileIds) > 0 {
			for _, fname := range u.mc.GetFileLinks(postlist.Posts[postlist.Order[i]].FileIds) {
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
	users, resp := u.mc.Client.SearchUsers(&model.UserSearch{Term: strings.Join(args, " ")})
	if resp.Error != nil {
		u.MsgUser(toUser, fmt.Sprint("Error", resp.Error))
		return
	}
	for _, user := range users {
		u.MsgUser(toUser, fmt.Sprint(user.Nickname, user.FirstName, user.LastName))
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
	args[0] = strings.Replace(args[0], "#", "", -1)
	postlist := u.mc.GetPosts(u.mc.GetChannelId(args[0], u.mc.Team.Id), limit)
	if postlist == nil || len(postlist.Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}
	for i := len(postlist.Order) - 1; i >= 0; i-- {
		nick := u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Username
		if (u.Cfg.PreferNickname &&
		    u.isValidNick(u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Nickname)) {
			nick = u.mc.GetUser(postlist.Posts[postlist.Order[i]].UserId).Nickname
		}
		for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
			if post != "" {
				u.MsgUser(toUser, "<"+nick+"> "+post)
			}
		}
		if len(postlist.Posts[postlist.Order[i]].FileIds) > 0 {
			for _, fname := range u.mc.GetFileLinks(postlist.Posts[postlist.Order[i]].FileIds) {
				u.MsgUser(toUser, "<"+nick+"> download file - "+fname)
			}
		}
	}

}

func updatelastviewed(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}
	channelId := ""
	if len(args) != 1 {
		u.MsgUser(toUser, "need UPDATELASTVIEWED <channel>")
		u.MsgUser(toUser, "e.g. UPDATELASTVIEWED #bugs")
		return
	}
	if strings.Contains(args[0], "#") {
		args[0] = strings.Replace(args[0], "#", "", -1)
		channelId = u.mc.GetChannelId(args[0], u.mc.Team.Id)
		if channelId == "" {
			u.MsgUser(toUser, "channel does not exist")
			return
		}
	} else if updateUser, exists := u.Srv.HasUser(args[0]); exists && updateUser.MmGhostUser {
		dc, resp := u.mc.Client.CreateDirectChannel(u.mc.User.Id, updateUser.User)
		if resp.Error != nil {
			u.MsgUser(toUser, fmt.Sprintf("CreateDirectChannel to %#v failed: %s", updateUser.User, resp.Error))
			return
		}
		channelId = dc.Id
	} else {
		u.MsgUser(toUser, fmt.Sprintf("user %s does not exist", args[0]))
		return
	}
	u.mc.UpdateLastViewed(channelId)
	u.MsgUser(toUser, fmt.Sprintf("set viewed for %s", args[0]))
}

var cmds = map[string]Command{
	"logout":           {handler: logout, login: true, minParams: 0, maxParams: 0},
	"login":            {handler: login, minParams: 2, maxParams: 4},
	"search":           {handler: search, login: true, minParams: 1, maxParams: -1},
	"searchusers":      {handler: searchUsers, login: true, minParams: 1, maxParams: -1},
	"scrollback":       {handler: scrollback, login: true, minParams: 2, maxParams: 2},
	"updatelastviewed": {handler: updatelastviewed, login: true, minParams: 1, maxParams: 1},
}

func (u *User) handleServiceBot(service string, toUser *User, msg string) {

	//func (u *User) handleMMServiceBot(toUser *User, msg string) {
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
		switch service {
		case "mattermost":
			if u.mc == nil {
				u.MsgUser(toUser, "You're not logged in. Use LOGIN first.")
				return
			}
		case "slack":
			if u.sc == nil {
				u.MsgUser(toUser, "You're not logged in. Use LOGIN first.")
				return
			}
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
