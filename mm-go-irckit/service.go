package irckit

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/42wim/matterircd/bridge"
	"github.com/mattermost/mattermost-server/v6/model"
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

	if service == "mastodon" {
		fmt.Println("login mastodon")
		err := u.loginTo("mastodon")
		if err != nil {
			u.MsgUser(toUser, err.Error())
			return
		}

		u.MsgUser(toUser, "login OK")

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
				Team:     args[0],
				Login:    args[1],
				Pass:     args[2],
				MFAToken: args[3],
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
	datalen := 4

	if len(args) > 1 && strings.Contains(args[len(args)-1], "MFAToken=") {
		datalen = 5
	}

	if u.v.GetString("mattermost.DefaultTeam") != "" {
		cred.Team = u.v.GetString("mattermost.DefaultTeam")
		datalen--
	}

	if u.v.GetString("mattermost.DefaultServer") != "" {
		cred.Server = u.v.GetString("mattermost.DefaultServer")
		datalen--
	}

	if len(args) >= datalen { // nolint:nestif
		logger.Debugf("args_len: %d", len(args))
		logger.Debugf("team: %s", cred.Team)
		logger.Debugf("server: %s", cred.Server)
		if strings.Contains(args[len(args)-1], "MFAToken=") {
			logger.Debug("found MFAToken")
			MFAToken := strings.Split(args[len(args)-1], "=")
			cred.MFAToken = MFAToken[1]
			cred.Pass = args[len(args)-2]
			cred.Login = args[len(args)-3]
		} else {
			cred.Pass = args[len(args)-1]
			cred.Login = args[len(args)-2]
		}
		// no default server or team specified
		if cred.Server == "" && cred.Team == "" {
			cred.Server = args[0]
			cred.Team = args[1]
		}

		if cred.Team == "" {
			cred.Team = args[0]
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

//nolint:cyclop
func search(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	limit := 0
	if len(args) > 1 {
		var err error
		limit, err = strconv.Atoi(args[0])
		if err == nil {
			args = args[1:]
		}
	}

	if len(args) == 0 || args[0] == "help" {
		u.MsgUser(toUser, "need SEARCH <limit> <text>")
		u.MsgUser(toUser, "e.g. SEARCH 10 matterircd")
		return
	}

	list := u.br.SearchPosts(strings.Join(args, " "))

	if list == nil || list.(*model.PostList) == nil || len(list.(*model.PostList).Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	postlist, _ := list.(*model.PostList)

	if limit == 0 || limit > len(postlist.Order) {
		limit = len(postlist.Order)
	}

	for i := limit - 1; i >= 0; i-- {
		p := postlist.Posts[postlist.Order[i]]
		if p.Type == model.PostTypeJoinLeave {
			continue
		}

		if p.DeleteAt > p.CreateAt {
			continue
		}

		props := p.GetProps()
		botname, override := props["override_username"].(string)
		user := u.br.GetUser(p.UserId)
		nick := user.Nick
		if override {
			nick = botname
		}

		channelname := getMattermostChannelName(u, p.ChannelId)

		if p.Type == model.PostTypeAddToTeam || p.Type == model.PostTypeRemoveFromTeam {
			nick = systemUser
		}

		for _, post := range strings.Split(p.Message, "\n") {
			if nick == systemUser {
				post = "\x1d" + post + "\x1d"
			}
			for _, term := range args {
				re := regexp.MustCompile(`(?i)(` + regexp.QuoteMeta(term) + `)`)
				post = re.ReplaceAllString(post, "\x02$1\x02")
			}
			formatSearchMsg(u, p.ChannelId, channelname, toUser, nick, p, post)
		}

		if len(p.FileIds) == 0 {
			continue
		}

		for _, fname := range u.br.GetFileLinks(p.FileIds) {
			fileMsg := "\x1ddownload file - " + fname + "\x1d" //nolint:goconst
			formatSearchMsg(u, p.ChannelId, channelname, toUser, nick, p, fileMsg)
		}
	}
}

func formatSearchMsg(u *User, channelID string, channel string, user *User, nick string, p *model.Post, msgText string) {
	ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

	switch {
	case (u.v.GetBool(u.br.Protocol()+".collapsescrollback") && strings.HasPrefix(channel, "#")):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		nick += "/" + channel
		u.Srv.Channel("&messages").SpoofMessage(nick, msg)
	case u.v.GetBool(u.br.Protocol() + ".collapsescrollback"):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		u.Srv.Channel("&messages").SpoofMessage(nick, msg)
	case (u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext")) && strings.HasPrefix(channel, "#"):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		nick += "/" + channel
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, "<"+nick+"> "+msgText)
		u.MsgUser(user, msg)
	case strings.HasPrefix(channel, "#"):
		nick += "/" + channel
		msg := "[" + ts.Format("2006-01-02 15:04") + "] <" + nick + "> " + msgText
		u.MsgUser(user, msg)
	case u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext"):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, "<"+nick+"> "+msgText)
		u.MsgUser(user, msg)
	default:
		msg := "[" + ts.Format("2006-01-02 15:04") + "] <" + nick + "> " + msgText
		u.MsgUser(user, msg)
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

func getMattermostChannelName(u *User, channelID string) string {
	channelName := u.br.GetChannelName(channelID)
	channelMembers := strings.Split(channelName, "__")

	if len(channelMembers) != 2 {
		return channelName
	}

	if channelMembers[0] == u.br.GetMe().User {
		return channelMembers[1]
	}
	return channelMembers[0]
}

func part(u *User, toUser *User, args []string, service string) {
	if len(args) != 1 {
		u.MsgUser(toUser, "need PART #<channel>")
		u.MsgUser(toUser, "e.g. PART #bugs")
		return
	}

	channelName := strings.TrimPrefix(args[0], "#")
	channelTeamID := u.br.GetMe().TeamID
	if len(args) == 2 {
		channelTeamID = args[1]
	}
	channelID := u.br.GetChannelID(channelName, channelTeamID)

	err := u.br.Part(channelID)
	if err != nil {
		u.MsgUser(toUser, fmt.Sprintf("could not part/leave %s", args[0]))
	}
}

//nolint:funlen,gocognit,gocyclo,cyclop
func scrollback(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	var err error
	limit := 0
	err = nil
	if len(args) == 2 {
		limit, err = strconv.Atoi(args[1])
	}
	if len(args) == 0 || len(args) > 2 || err != nil {
		u.MsgUser(toUser, "need SCROLLBACK (#<channel>|<user>|<post/thread ID>) <lines>")
		u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
		return
	}

	search := args[0]

	var channelID, searchPostID string
	scrollbackUser, exists := u.Srv.HasUser(search)

	proto := "https"
	if u.v.GetBool(u.br.Protocol() + ".insecure") {
		proto = "http"
	}
	postlistURL := proto + "://" + u.Credentials.Server + "/" + u.Credentials.Team + "/pl/"

	switch {
	case strings.HasPrefix(search, "#"):
		channelName := strings.ReplaceAll(search, "#", "")
		channelID = u.br.GetChannelID(channelName, u.br.GetMe().TeamID)
	case exists && scrollbackUser.Ghost:
		// We need to sort the two user IDs to construct the DM
		// channel name.
		userIDs := []string{u.User, scrollbackUser.User}
		sort.Strings(userIDs)
		channelName := userIDs[0] + "__" + userIDs[1]
		channelID = u.br.GetChannelID(channelName, u.br.GetMe().TeamID)
	case len(search) == 26:
		searchPostID = search
	case strings.HasPrefix(search, "@@"):
		searchPostID = strings.TrimPrefix(search, "@@")
	case strings.HasPrefix(strings.ToLower(search), postlistURL):
		searchPostID = strings.TrimPrefix(search, postlistURL)
	default:
		u.MsgUser(toUser, "need SCROLLBACK (#<channel>|<user>|<post/thread ID>) <lines>")
		u.MsgUser(toUser, "e.g. SCROLLBACK #bugs 10 (show last 10 lines from #bugs)")
		return
	}

	var list interface{}
	if searchPostID != "" {
		list = u.br.GetPostThread(searchPostID)
	} else {
		list = u.br.GetPosts(channelID, limit)
	}
	if list == nil || list.(*model.PostList) == nil || len(list.(*model.PostList).Order) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	postlist, _ := list.(*model.PostList)

	// Workaround https://github.com/mattermost/mattermost-server/issues/23081
	plOrder := postlist.Order
	if searchPostID != "" {
		plOrder = append(plOrder, searchPostID)
	}
	skipRoot := false

	for i := len(plOrder) - 1; i >= 0; i-- {
		if limit != 0 && len(plOrder) > limit && i < len(plOrder)-limit {
			continue
		}

		p := postlist.Posts[plOrder[i]]

		// Workaround https://github.com/mattermost/mattermost-server/issues/23081
		if searchPostID != "" && p.Id == searchPostID {
			if skipRoot {
				continue
			}
			skipRoot = true
		}

		props := p.GetProps()
		botname, override := props["override_username"].(string)
		user := u.br.GetUser(p.UserId)
		nick := user.Nick
		if override {
			nick = botname
		}

		if p.Type == model.PostTypeAddToTeam || p.Type == model.PostTypeRemoveFromTeam {
			nick = systemUser
		}

		if searchPostID != "" && channelID == "" {
			channelID = p.ChannelId
			search = getMattermostChannelName(u, p.ChannelId)
			if !strings.HasPrefix(search, "#") {
				user := u.br.GetUser(search)
				search = user.Nick
				if override {
					search = botname
				}
				scrollbackUser, _ = u.Srv.HasUser(search)
			}
		}

		for _, post := range strings.Split(p.Message, "\n") {
			if nick == systemUser {
				post = "\x1d" + post + "\x1d"
			}
			formatScrollbackMsg(u, channelID, search, scrollbackUser, nick, p, post)
		}

		if len(p.FileIds) == 0 {
			continue
		}

		for _, fname := range u.br.GetFileLinks(p.FileIds) {
			fileMsg := "\x1ddownload file - " + fname + "\x1d"
			formatScrollbackMsg(u, channelID, search, scrollbackUser, nick, p, fileMsg)
		}
	}

	if !u.v.GetBool(u.br.Protocol() + ".collapsescrollback") {
		u.MsgUser(toUser, fmt.Sprintf("scrollback results shown in %s", search))
	}
}

func formatScrollbackMsg(u *User, channelID string, channel string, user *User, nick string, p *model.Post, msgText string) {
	ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

	switch {
	case (u.v.GetBool(u.br.Protocol()+".collapsescrollback") && strings.HasPrefix(channel, "#")):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		nick += "/" + channel
		u.Srv.Channel("&messages").SpoofMessage(nick, msg)
	case u.v.GetBool(u.br.Protocol() + ".collapsescrollback"):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		nick += "/" + channel
		u.Srv.Channel("&messages").SpoofMessage(nick, msg)
	case (u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext")) && strings.HasPrefix(channel, "#") && nick != systemUser:
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		u.Srv.Channel(channelID).SpoofMessage(nick, msg)
	case strings.HasPrefix(channel, "#"):
		msg := "[" + ts.Format("2006-01-02 15:04") + "] " + msgText
		u.Srv.Channel(channelID).SpoofMessage(nick, msg)
	case u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext"):
		threadMsgID := u.prefixContext(channelID, p.Id, p.RootId, "scrollback")
		msg := u.formatContextMessage(ts.Format("2006-01-02 15:04"), threadMsgID, msgText)
		u.MsgSpoofUser(user, nick, msg)
	default:
		msg := "[" + ts.Format("2006-01-02 15:04") + "]" + " <" + nick + "> " + msgText
		u.MsgSpoofUser(user, nick, msg)
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
	"lastsent":         {handler: lastsent, login: true, minParams: 0, maxParams: 0},
	"logout":           {handler: logout, login: true, minParams: 0, maxParams: 0},
	"login":            {handler: login, minParams: 2, maxParams: 5},
	"part":             {handler: part, login: true, minParams: 1, maxParams: 1},
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

func lastsent(u *User, toUser *User, args []string, service string) {
	for _, line := range u.br.GetLastSentMsgs() {
		u.MsgUser(toUser, line)
	}
}
