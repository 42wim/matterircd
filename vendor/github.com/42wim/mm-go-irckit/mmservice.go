package irckit

import (
	"strconv"
	"strings"
	"time"
)

func (u *User) handleMMServiceBot(toUser *User, msg string) {
	commands := strings.Fields(msg)
	switch commands[0] {
	case "LOGOUT", "logout":
		{
			u.logoutFromMattermost()
		}
	case "LOGIN", "login":
		{
			if u.mc != nil {
				u.logoutFromMattermost()
			}
			cred := &MmCredentials{}
			datalen := 5
			if u.Cfg.DefaultTeam != "" {
				cred.Team = u.Cfg.DefaultTeam
				datalen--
			}
			if u.Cfg.DefaultServer != "" {
				cred.Server = u.Cfg.DefaultServer
				datalen--
			}
			data := strings.Split(msg, " ")
			if len(data) == datalen {
				cred.Pass = data[len(data)-1]
				cred.Login = data[len(data)-2]
				// no default server or team specified
				if cred.Server == "" && cred.Team == "" {
					cred.Server = data[len(data)-4]
				}
				if cred.Team == "" {
					cred.Team = data[len(data)-3]
				}
				if cred.Server == "" {
					cred.Server = data[len(data)-3]
				}

			}

			// incorrect arguments
			if len(data) != datalen {
				// no server or team
				if cred.Team != "" && cred.Server != "" {
					u.MsgUser(toUser, "need LOGIN <login> <pass>")
					return
				}
				// server missing
				if cred.Team != "" {
					u.MsgUser(toUser, "need LOGIN <server> <login> <pass>")
					return
				}
				// team missing
				if cred.Server != "" {
					u.MsgUser(toUser, "need LOGIN <team> <login> <pass>")
					return
				}
				u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
				return
			}

			if !u.isValidMMServer(cred.Server) {
				u.MsgUser(toUser, "not allowed to connect to "+cred.Server)
				return
			}

			u.Credentials = cred
			var err error
			u.mc, err = u.loginToMattermost()
			if err != nil {
				u.MsgUser(toUser, err.Error())
				return
			}
			u.addUsersToChannels()
			u.MsgUser(toUser, "login OK")
		}
	case "SEARCH", "search":
		{
			if u.mc.Client == nil {
				u.MsgUser(toUser, "Can not search, you're not logged in. Use LOGIN first.")
				return
			}
			postlist := u.mc.SearchPosts(strings.Join(commands[1:], " "))
			if postlist == nil || len(postlist.Order) == 0 {
				u.MsgUser(toUser, "no results")
				return
			}
			for i := len(postlist.Order) - 1; i >= 0; i-- {
				timestamp := time.Unix(postlist.Posts[postlist.Order[i]].CreateAt/1000, 0).Format("January 02, 2006 15:04")
				channelname := u.mc.GetChannelName(postlist.Posts[postlist.Order[i]].ChannelId)
				u.MsgUser(toUser, "#"+channelname+" <"+u.mc.Users[postlist.Posts[postlist.Order[i]].UserId].Username+"> "+timestamp)
				u.MsgUser(toUser, strings.Repeat("=", len("#"+channelname+" <"+u.mc.Users[postlist.Posts[postlist.Order[i]].UserId].Username+"> "+timestamp)))
				for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
					u.MsgUser(toUser, post)
				}
				u.MsgUser(toUser, "")
				u.MsgUser(toUser, "")
			}
		}
	case "SCROLLBACK", "scrollback", "sb":
		{
			if len(commands) != 3 {
				u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
				u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
				return
			}
			limit, err := strconv.Atoi(commands[2])
			if err != nil {
				u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
				u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
				return
			}
			if !strings.Contains(commands[1], "#") {
				u.MsgUser(toUser, "need SCROLLBACK <channel> <lines>")
				u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
				return
			}
			commands[1] = strings.Replace(commands[1], "#", "", -1)
			postlist := u.mc.GetPosts(u.mc.GetChannelId(commands[1]), limit)
			if postlist == nil || len(postlist.Order) == 0 {
				u.MsgUser(toUser, "no results")
				return
			}
			for i := len(postlist.Order) - 1; i >= 0; i-- {
				nick := u.mc.Users[postlist.Posts[postlist.Order[i]].UserId].Username
				for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
					u.MsgUser(toUser, "<"+nick+"> "+post)
				}
			}
		}
	default:
		u.MsgUser(toUser, "possible commands: LOGIN, SEARCH, SCROLLBACK")
		u.MsgUser(toUser, "<command> help for more info")
	}
}
