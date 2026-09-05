package irckit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/utils"
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
	if u.inprogress.Load() {
		u.MsgUser(toUser, "login or logout in progress. Please wait")
		return
	}

	u.inprogress.Store(true)
	defer u.inprogress.Store(false)

	_ = u.br.Logout(u.ctx)
	u.logoutFrom(u.br.Protocol())
}

func login(u *User, toUser *User, args []string, service string) {
	if u.inprogress.Load() {
		u.MsgUser(toUser, "login or logout in progress. Please wait")
		return
	}

	if service == "mastodon" {
		u.inprogress.Store(true)
		defer u.inprogress.Store(false)

		if u.br != nil && u.br.Connected() {
			u.MsgUser(toUser, "already logged in to mastodon. Please log out first")

			return
		}

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
			u.MsgUser(toUser, "already logged in to slack. Please log out first")

			return
		}

		u.inprogress.Store(true)
		defer u.inprogress.Store(false)

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
	datalen := len(args)

	if len(args) > 1 && strings.Contains(args[len(args)-1], "MFAToken=") {
		MFAToken := strings.Split(args[len(args)-1], "=")
		cred.MFAToken = MFAToken[1]
		datalen--
	}

	teamOrServer := ""
	creds := []*string{&cred.Server, &teamOrServer, &cred.Login, &cred.Pass}
	for argsIdx, credsIdx := datalen, len(creds); argsIdx > 0 && credsIdx > 0; argsIdx-- {
		*creds[credsIdx-1] = args[argsIdx-1]
		creds = creds[:credsIdx-1]
		credsIdx--
	}

	switch {
	// All params provided (server, team, username, & password) so set the team to provided.
	case cred.Server != "":
		cred.Team = teamOrServer
	// Missing both server and team, let's use DefaultServer and DefaultTeam.
	case teamOrServer == "" && u.cfg.Mattermost().DefaultServer != "" && u.cfg.Mattermost().DefaultTeam != "":
		cred.Server = u.cfg.Mattermost().DefaultServer
		cred.Team = u.cfg.Mattermost().DefaultTeam
	// Missing server, let's use DefaultServer.
	case u.cfg.Mattermost().DefaultServer != "":
		cred.Server = u.cfg.Mattermost().DefaultServer
		cred.Team = teamOrServer
	// Missing team, let's use DefaultTeam.
	case u.cfg.Mattermost().DefaultTeam != "":
		cred.Server = teamOrServer
		cred.Team = u.cfg.Mattermost().DefaultTeam
	default:
		cred.Server = teamOrServer
	}

	logger.Tracef("args_len: %d", len(args))
	logger.Tracef("team: %s", cred.Team)
	logger.Tracef("server: %s", cred.Server)
	if cred.MFAToken != "" {
		logger.Debug("found MFAToken")
	}

	switch {
	// Both server and team missing
	case cred.Team == "" && cred.Server == "":
		u.MsgUser(toUser, "need LOGIN <server> <team> <login> <pass>")
		u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
		u.MsgUser(toUser, "when using a mfa token use LOGIN <server> <team> <login> <pass> MFAToken=<yourmfatoken>")
		return
	// server missing
	case cred.Server == "":
		u.MsgUser(toUser, "need LOGIN <server> <login> <pass>")
		u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
		u.MsgUser(toUser, "when using a mfa token use LOGIN <server> <login> <pass> MFAToken=<yourmfatoken>")
		return
	// team missing
	case cred.Team == "":
		u.MsgUser(toUser, "need LOGIN <team> <login> <pass>")
		u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
		u.MsgUser(toUser, "when using a mfa token use LOGIN <team> <login> <pass> MFAToken=<yourmfatoken>")
		return
	// login/username or password/token missing
	case cred.Login == "" || cred.Pass == "":
		u.MsgUser(toUser, "need LOGIN <login> <pass>")
		u.MsgUser(toUser, "when using a personal token replace <pass> with token=<yourtoken>")
		u.MsgUser(toUser, "when using a mfa token use LOGIN <login> <pass> MFAToken=<yourmfatoken>")
		return
	}

	if !u.isValidServer(cred.Server, service) {
		u.MsgUser(toUser, "not allowed to connect to "+cred.Server)
		return
	}

	if u.br != nil && u.br.Connected() {
		u.MsgUser(toUser, "already logged in to mattermost. Please log out first")

		return
	}

	u.inprogress.Store(true)
	defer u.inprogress.Store(false)

	u.Credentials = cred

	err := u.loginTo("mattermost")
	if err != nil {
		u.MsgUser(toUser, err.Error())
		return
	}

	u.MsgUser(toUser, "login OK")
}

func replay(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	if len(args) == 0 || len(args) > 2 {
		u.MsgUser(toUser, "need REPLAY (#<channel>) [duration]")
		u.MsgUser(toUser, "e.g. REPLAY #bugs 24h")
		return
	}

	channelName := strings.TrimPrefix(args[0], "#")
	channelTeamID := u.br.GetMe().TeamID

	var since int64
	var logSince string

	// Parse optional duration argument
	if len(args) == 2 {
		if d, err := time.ParseDuration(args[1]); err == nil {
			// Mattermost uses Unix milliseconds
			since = time.Now().Add(-d).UnixMilli()
			logSince = fmt.Sprintf("user requested: %s", args[1])
		} else {
			u.MsgUser(toUser, "invalid duration format. e.g. 24h, 30m")
			return
		}
	}

	channelID := u.br.GetChannelID(u.ctx, channelName, channelTeamID)
	brchannel, err := u.br.GetChannel(u.ctx, channelID)
	if err != nil {
		u.MsgUser(toUser, channelName+" not found")
		return
	}

	// If no custom duration was provided, fall back to our configured strategy
	if since == 0 {
		// Cap manual unbounded replays just like the startup worker
		maxReplay := u.cfg.Mattermost().MaxReplayDuration
		if maxReplay == 0 {
			maxReplay = 31 * 24 * time.Hour
		}

		replayCutoff := time.Now().Add(-maxReplay).UnixMilli()
		since, logSince, _ = u.getChannelSince(u.ctx, brchannel, replayCutoff)
	}

	u.replayHistory(brchannel, since, logSince)
}

//nolint:forcetypeassert,goconst
func details(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	if len(args) != 1 {
		u.MsgUser(toUser, "need DETAILS <post/thread ID>")
		u.MsgUser(toUser, "e.g. DETAILS zhqrffbxupgepj1yquxaftfk4a")
		return
	}

	postID := args[0]

	proto := "https"
	if u.cfg.Mattermost().Insecure {
		proto = "http"
	}
	postlistURL := proto + "://" + u.Credentials.Server + "/" + u.Credentials.Team + "/pl/"

	switch {
	case len(postID) == 26:
		// Do nothing
	case strings.HasPrefix(postID, "@@"):
		postID = strings.TrimPrefix(postID, "@@")
	case strings.HasPrefix(strings.ToLower(postID), postlistURL):
		postID = strings.TrimPrefix(postID, postlistURL)
	default:
		u.MsgUser(toUser, "need DETAILS <post/thread ID>")
		u.MsgUser(toUser, "e.g. DETAILS zhqrffbxupgepj1yquxaftfk4a")
		return
	}

	events := u.br.GetPostThread(u.ctx, postID)
	if len(events) == 0 {
		u.MsgUser(toUser, "post not found")
		return
	}

	prefix := "\x0312|\x0f "
	u.MsgUser(toUser, prefix+postlistURL+postID)

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator
	preserveNewLines := u.br.FormatterConfig().PreserveNewLines

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
		PreserveNewLines:   preserveNewLines,
	}

	for _, event := range events {
		var createAt int64
		var text, nick, channel string

		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			createAt, text, nick, channel = e.CreateAt, e.Text, e.Sender.Nick, u.br.GetChannelName(u.ctx, e.ChannelID)
		case *bridge.DirectMessageEvent:
			createAt, text, nick, channel = e.CreateAt, e.Text, e.Sender.Nick, u.br.GetChannelName(u.ctx, e.ChannelID)
		default:
			continue
		}

		ts := time.Unix(0, createAt*int64(time.Millisecond)).Format("2006-01-02 15:04:05")
		u.MsgUser(toUser, prefix+"["+ts+"] <"+nick+"> in "+channel)

		textToProcess := utils.WrapMessage(text, 460)

		utils.ProcessMessageText(textToProcess, opts, func(line string) {
			// Visually translate actions for the details view
			if strings.HasPrefix(line, "\x01ACTION ") && strings.HasSuffix(line, "\x01") {
				line = "* " + nick + " " + line[8:len(line)-1]
			}
			u.MsgUser(toUser, prefix+"  "+line)
		})
	}
}

//nolint:cyclop
func search(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	limit := 0
	if len(args) > 1 {
		if val, err := strconv.Atoi(args[0]); err == nil {
			limit = val
			args = args[1:]
		}
	}

	if len(args) == 0 || args[0] == "help" {
		u.MsgUser(toUser, "need SEARCH <limit> <text>")
		u.MsgUser(toUser, "e.g. SEARCH 10 matterircd")
		return
	}

	searchStr := strings.Join(args, " ")
	events := u.br.SearchPosts(u.ctx, searchStr)
	if len(events) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	if limit > 0 && limit < len(events) {
		events = events[len(events)-limit:]
	}

	var searchRegexes []*regexp.Regexp
	if len(args) > 0 {
		searchRegexes = make([]*regexp.Regexp, 0, len(args))
		for _, term := range args {
			searchRegexes = append(searchRegexes, regexp.MustCompile(`(?i)(`+regexp.QuoteMeta(term)+`)`))
		}
	}

	for _, event := range events {
		dispatchHistoricalEvent(u, toUser, event, searchStr, searchRegexes)
	}
}

func searchUsers(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	users, err := u.br.SearchUsers(u.ctx, strings.Join(args, " "))
	if err != nil {
		u.MsgUser(toUser, fmt.Sprint("Error", err.Error()))
		return
	}

	for _, user := range users {
		u.MsgUser(toUser, fmt.Sprint(user.Nick, user.FirstName, user.LastName))
	}
}

func getChannelTargetName(u *User, channelID string) string {
	channelName := u.br.GetChannelName(u.ctx, channelID)

	if u.br.IsDMChannelName(channelName) {
		if dmUser := u.br.GetDMUser(u.ctx, channelName); dmUser != nil && dmUser.Nick != "" {
			return dmUser.Nick
		}
	}

	return channelName
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
	channelID := u.br.GetChannelID(u.ctx, channelName, channelTeamID)

	err := u.br.Part(u.ctx, channelID)
	if err != nil {
		u.MsgUser(toUser, fmt.Sprintf("could not part/leave %s", args[0]))
	}
}

func ping(u *User, toUser *User, args []string, service string) {
	pingCtx, cancel := context.WithTimeout(u.ctx, 5*time.Second)
	defer cancel()

	start := time.Now()

	err := u.br.Ping(pingCtx, "http")
	if err != nil {
		u.MsgUser(toUser, fmt.Sprintf("HTTP REST PING failed: %v", err))

		return
	}

	duration := time.Since(start).Round(time.Millisecond)

	u.MsgUser(toUser, fmt.Sprintf("PONG (HTTP REST): status=OK, latency=%s", duration))
}

//nolint:funlen,gocognit,gocyclo,cyclop
func scrollback(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	var err error
	limit := 0
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
	if u.cfg.Mattermost().Insecure {
		proto = "http"
	}
	postlistURL := proto + "://" + u.Credentials.Server + "/" + u.Credentials.Team + "/pl/"

	switch {
	case strings.HasPrefix(search, "#"):
		channelName := strings.ReplaceAll(search, "#", "")
		channelID = u.br.GetChannelID(u.ctx, channelName, u.br.GetMe().TeamID)
	case exists && scrollbackUser.Ghost:
		dmName := u.br.GetDMChannelName(u.User, scrollbackUser.User)
		channelID = u.br.GetChannelID(u.ctx, dmName, u.br.GetMe().TeamID)
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

	var events []*bridge.Event
	if searchPostID != "" {
		events = u.br.GetPostThread(u.ctx, searchPostID)
	} else {
		events = u.br.GetPosts(u.ctx, channelID, limit)
	}

	if len(events) == 0 {
		u.MsgUser(toUser, "no results")
		return
	}

	if limit > 0 && limit < len(events) {
		events = events[len(events)-limit:]
	}

	for _, event := range events {
		dispatchHistoricalEvent(u, toUser, event, search, nil)
	}

	if !u.cfg.Mattermost().CollapseScrollback {
		u.MsgUser(toUser, fmt.Sprintf("scrollback results shown in \x1d%s\x1d", search))
	}
}

// Unified dispatcher for handling formatted search and scrollback payloads
//
//nolint:funlen,gocyclo,unparam
func dispatchHistoricalEvent(u *User, toUser *User, event *bridge.Event, searchCtx string, searchRegexes []*regexp.Regexp) {
	var createAt int64
	var text, nick, msgID, parentID, channelID string
	var files []*bridge.File

	switch e := event.Data.(type) {
	case *bridge.ChannelMessageEvent:
		createAt, text, nick, msgID, parentID, files, channelID = e.CreateAt, e.Text, e.Sender.Nick, e.MessageID, e.ParentID, e.Files, e.ChannelID
	case *bridge.DirectMessageEvent:
		createAt, text, nick, msgID, parentID, files, channelID = e.CreateAt, e.Text, e.Sender.Nick, e.MessageID, e.ParentID, e.Files, e.ChannelID
	case *bridge.ChannelAddEvent:
		createAt, text, nick, channelID = e.CreateAt, e.Text, systemUser, e.ChannelID
	case *bridge.ChannelRemoveEvent:
		createAt, text, nick, channelID = e.CreateAt, e.Text, systemUser, e.ChannelID
	default:
		return
	}

	channelName := getChannelTargetName(u, channelID)
	scrollbackUser, _ := u.Srv.HasUser(searchCtx)

	tsStr := time.Unix(0, createAt*int64(time.Millisecond)).Format("2006-01-02 15:04")

	customEmoji := u.br.FormatterConfig().CustomEmoji
	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator
	preserveNewLines := u.br.FormatterConfig().PreserveNewLines

	opts := utils.ProcessMessageOpts{
		DisableEmoji:       disableEmoji,
		CustomEmoji:        customEmoji,
		DisableMarkdown:    disableMarkdown,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
		PreserveNewLines:   preserveNewLines,
	}

	textToProcess := utils.WrapMessage(text, 460)

	utils.ProcessMessageText(textToProcess, opts, func(line string) {
		if line != "" {
			if nick == systemUser {
				line = "\x1d" + line + "\x1d"
			}
			if len(searchRegexes) > 0 {
				for _, re := range searchRegexes {
					line = re.ReplaceAllString(line, "\x02$1\x02")
				}
			}
			formatScrollbackMsg(u, channelID, channelName, scrollbackUser, nick, tsStr, msgID, parentID, line)
		}
	})

	for _, f := range files {
		sizeKB := f.Size / 1024
		if sizeKB == 0 && f.Size > 0 {
			sizeKB = 1
		}

		fileMsg := fmt.Sprintf("\x1ddownload file - %s (%s, %dKBytes)\x1d", f.URL, f.Name, sizeKB)
		formatScrollbackMsg(u, channelID, channelName, scrollbackUser, nick, tsStr, msgID, parentID, fileMsg)
	}
}

func formatScrollbackMsg(u *User, channelID string, channel string, user *User, nick string, tsStr string, msgID, parentID, msgText string) {
	// Safely unwrap CTCP actions
	isAction := strings.HasPrefix(msgText, "\x01ACTION ") && strings.HasSuffix(msgText, "\x01")
	if isAction {
		msgText = msgText[8 : len(msgText)-1]
	}

	var msg string
	var send func(string)

	switch {
	case u.cfg.Mattermost().CollapseScrollback && strings.HasPrefix(channel, "#"):
		threadMsgID := u.prefixContext(channelID, msgID, parentID, "scrollback")
		msg = u.formatContextMessage(tsStr, threadMsgID, msgText)
		nick += "/" + channel
		send = func(m string) { u.Srv.Channel("&messages").SpoofMessage(nick, m) }
	case u.cfg.Mattermost().CollapseScrollback:
		threadMsgID := u.prefixContext(channelID, msgID, parentID, "scrollback")
		msg = u.formatContextMessage(tsStr, threadMsgID, msgText)
		send = func(m string) { u.Srv.Channel("&messages").SpoofMessage(nick, m) }
	case (u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext) && strings.HasPrefix(channel, "#") && nick != systemUser:
		threadMsgID := u.prefixContext(channelID, msgID, parentID, "scrollback")
		msg = u.formatContextMessage(tsStr, threadMsgID, msgText)
		send = func(m string) { u.Srv.Channel(channelID).SpoofMessage(nick, m) }
	case strings.HasPrefix(channel, "#"):
		msg = "[" + tsStr + "] " + msgText
		send = func(m string) { u.Srv.Channel(channelID).SpoofMessage(nick, m) }
	case u.br.BridgeConfig().PrefixContext || u.br.BridgeConfig().SuffixContext:
		threadMsgID := u.prefixContext(channelID, msgID, parentID, "scrollback")
		msg = u.formatContextMessage(tsStr, threadMsgID, msgText)
		send = func(m string) { u.MsgSpoofUser(user, nick, m) }
	default:
		msg = "[" + tsStr + "] <" + nick + "> " + msgText
		send = func(m string) { u.MsgSpoofUser(user, nick, m) }
	}

	// Re-wrap the entire payload so the IRC client parses it
	if isAction {
		msg = "\x01ACTION " + msg + "\x01"
	}

	send(msg)
}

func updatelastviewed(u *User, toUser *User, args []string, service string) {
	if service != "mattermost" {
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

		channelID = u.br.GetChannelID(u.ctx, args[0], u.br.GetMe().TeamID)
		if channelID == "" {
			u.MsgUser(toUser, "channel does not exist")
			return
		}
	} else if updateUser, exists := u.Srv.HasUser(args[0]); exists && updateUser.Ghost {
		err := u.br.UpdateLastViewedUser(u.ctx, updateUser.User)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("updatelastviewed for %#v failed: %s", updateUser.User, err))
			return
		}
		return
	} else {
		u.MsgUser(toUser, fmt.Sprintf("user %s does not exist", args[0]))
		return
	}

	u.br.UpdateLastViewed(u.ctx, channelID)
	u.MsgUser(toUser, fmt.Sprintf("set viewed for %s", args[0]))
}

var cmds = map[string]Command{
	"channel":          {handler: channelCmd, login: true, minParams: 2, maxParams: 3},
	"details":          {handler: details, login: true, minParams: 1, maxParams: 1},
	"header":           {handler: channelHeader, login: true, minParams: 2, maxParams: -1},
	"lastsent":         {handler: lastsent, login: true, minParams: 0, maxParams: 0},
	"login":            {handler: login, minParams: 2, maxParams: 5},
	"logout":           {handler: logout, login: true, minParams: 0, maxParams: 0},
	"part":             {handler: part, login: true, minParams: 1, maxParams: 1},
	"ping":             {handler: ping, login: true, minParams: 0, maxParams: 0},
	"replay":           {handler: replay, login: true, minParams: 1, maxParams: 2},
	"scrollback":       {handler: scrollback, login: true, minParams: 2, maxParams: 2},
	"search":           {handler: search, login: true, minParams: 1, maxParams: -1},
	"searchusers":      {handler: searchUsers, login: true, minParams: 1, maxParams: -1},
	"summarize":        {handler: summarize, login: true, minParams: 1, maxParams: 3},
	"summary":          {handler: summarize, login: true, minParams: 1, maxParams: 3},
	"tldr":             {handler: summarize, login: true, minParams: 1, maxParams: 3},
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

//nolint:funlen
func channelHeader(u *User, toUser *User, args []string, service string) {
	showHelp := func() {
		u.MsgUser(toUser, "need HEADER GET (#<channel>|<user>)")
		u.MsgUser(toUser, "e.g. HEADER GET #bugs")
		u.MsgUser(toUser, "need HEADER SET (#<channel>|<user>) <new header>")
		u.MsgUser(toUser, "e.g. HEADER SET #bugs I have just set a new header here")
		u.MsgUser(toUser, "need HEADER UPDATE (#<channel>|<user>) s/search/replace/")
		u.MsgUser(toUser, "e.g. HEADER UPDATE #bugs s/my topic/updated my new topic with stuff/")
	}

	if len(args) < 2 {
		showHelp()
		return
	}

	cmd := strings.ToLower(args[0])
	channel := args[1]

	// Both SET and UPDATE require additional arguments
	if cmd == "set" || cmd == "update" {
		if len(args) <= 2 {
			showHelp()
			return
		}
		args = args[2:]
	}

	var channelID, channelName string
	DMUser, exists := u.Srv.HasUser(channel)

	switch {
	case strings.HasPrefix(channel, "#"):
		channelName = strings.ReplaceAll(channel, "#", "")
		channelID = u.br.GetChannelID(u.ctx, channelName, u.br.GetMe().TeamID)
	case exists && DMUser.Ghost:
		channelName = u.br.GetDMChannelName(u.User, DMUser.User)
		channelID = u.br.GetChannelID(u.ctx, channelName, u.br.GetMe().TeamID)
	default:
		showHelp()
		return
	}

	if channelID == "" {
		u.MsgUser(toUser, "channel does not exist")
		return
	}

	switch {
	case cmd == "get":
		currentHeader := u.br.Topic(u.ctx, channelID)
		u.MsgUser(toUser, channel+"'s channel header is: "+currentHeader)

	case cmd == "set":
		err := u.br.SetTopic(u.ctx, channelID, strings.Join(args, " "))
		if err != nil {
			u.MsgUser(toUser, "failed to set header: "+err.Error())
		} else {
			u.MsgUser(toUser, channel+"'s channel header updated successfully.")
		}

	case cmd == "update":
		payload := strings.Join(args, " ")

		// Basic check for the sed-like substitute string (e.g. "s/old/new/")
		if len(payload) < 4 || payload[0] != 's' {
			u.MsgUser(toUser, "update format must be s/search/replace/")
			return
		}

		// Use the character right after 's' as the delimiter (usually '/')
		delim := string(payload[1])

		// Split into a max of 3 parts to allow delimiters inside the replacement text if needed
		parts := strings.SplitN(payload[2:], delim, 3)
		if len(parts) < 2 {
			u.MsgUser(toUser, "update format must be s/search/replace/")
			return
		}

		searchPattern := parts[0]
		replaceStr := parts[1]

		// Compile the regex pattern
		re, err := regexp.Compile(searchPattern)
		if err != nil {
			u.MsgUser(toUser, "invalid regex pattern: "+err.Error())
			return
		}

		currentHeader := u.br.Topic(u.ctx, channelID)
		newHeader := re.ReplaceAllString(currentHeader, replaceStr)

		if newHeader == currentHeader {
			u.MsgUser(toUser, "header unchanged (no match found for '"+searchPattern+"')")
			return
		}

		err = u.br.SetTopic(u.ctx, channelID, newHeader)
		if err != nil {
			u.MsgUser(toUser, "failed to update header: "+err.Error())
		} else {
			u.MsgUser(toUser, channel+"'s channel header updated to: "+newHeader)
		}

	default:
		showHelp()
		return
	}
}

//nolint:funlen
func channelCmd(u *User, toUser *User, args []string, service string) {
	if service == "slack" {
		u.MsgUser(toUser, "not implemented")
		return
	}

	showHelp := func() {
		u.MsgUser(toUser, "need CHANNEL CREATE #<channel> [public|private]")
		u.MsgUser(toUser, "e.g. CHANNEL CREATE #new-bugs")
		u.MsgUser(toUser, "e.g. CHANNEL CREATE #secret-bugs private")
		u.MsgUser(toUser, "need CHANNEL INFO (#<channel>|<id>)")
		u.MsgUser(toUser, "e.g. CHANNEL INFO #bugs")
	}

	if len(args) < 2 {
		showHelp()
		return
	}

	cmd := strings.ToLower(args[0])
	channelStr := args[1]

	// Strip the '#' for API calls
	channelName := strings.TrimPrefix(channelStr, "#")

	switch cmd {
	case "create":
		channelType := ""
		if len(args) > 2 {
			channelType = args[2]
		}

		info, err := u.br.CreateChannel(u.ctx, channelName, channelType)
		if err != nil {
			u.MsgUser(toUser, "failed to create channel: "+err.Error())
			return
		}

		typeStr := "public"
		if info.Private {
			typeStr = "private"
		}

		u.MsgUser(toUser, fmt.Sprintf("Channel %s (%s) created successfully.", channelStr, typeStr))
		u.MsgUser(toUser, "ID: "+info.ID)

	case "info":
		// Try to resolve it as a channel name first
		channelID := u.br.GetChannelID(u.ctx, channelName, u.br.GetMe().TeamID)

		// If that fails, assume the input is directly an ID
		if channelID == "" {
			channelID = channelName
		}

		info, err := u.br.GetChannel(u.ctx, channelID)
		if err != nil {
			u.MsgUser(toUser, "failed to get channel info: "+err.Error())
			return
		}

		displayName := "#" + info.Name
		if info.DM {
			displayName = "DM (" + info.Name + ")"
		}

		u.MsgUser(toUser, fmt.Sprintf("--- Channel Info for %s ---", displayName))
		u.MsgUser(toUser, "ID: "+info.ID)
		u.MsgUser(toUser, "Name: "+info.Name)

		isPrivate := "No"
		if info.Private {
			isPrivate = "Yes"
		}

		u.MsgUser(toUser, "Private: "+isPrivate)

		isDM := "No"
		if info.DM {
			isDM = "Yes"
		}

		u.MsgUser(toUser, "Direct Message: "+isDM)

		// Mattermost stores timestamps in milliseconds
		if info.DeleteAt > 0 {
			delTime := time.Unix(info.DeleteAt/1000, 0).Format(time.RFC1123)
			u.MsgUser(toUser, "Status: Deleted at "+delTime)
		} else {
			u.MsgUser(toUser, "Status: Active")
		}

		if info.LastPostAt > 0 {
			lastPost := time.Unix(info.LastPostAt/1000, 0).Format(time.RFC1123)
			u.MsgUser(toUser, "Last Post At: "+lastPost)
		} else {
			u.MsgUser(toUser, "Last Post At: Never")
		}

		u.MsgUser(toUser, "---------------------------")

	default:
		showHelp()
	}
}

type summarizeQuery struct {
	Count    int
	Since    int64
	Thinking string
}

func (u *User) getSummarizeEvents(target string, query summarizeQuery) (string, []*bridge.Event, error) {
	target = strings.TrimSpace(target)

	// Post / Thread ID (@@<id> or bare 26-character post ID)
	postID, isThread := strings.CutPrefix(target, "@@")
	if isThread || (len(target) == 26 && !strings.HasPrefix(target, "#") && !strings.HasPrefix(target, "@")) {
		if !isThread {
			postID = target
		}

		events := u.br.GetPostThread(u.ctx, postID)
		if len(events) == 0 {
			return "", nil, fmt.Errorf("thread '@@%s' not found", postID)
		}

		return "thread " + postID, events, nil
	}

	// Channel (#...)
	if channelName, ok := strings.CutPrefix(target, "#"); ok {
		teamID := u.br.GetMe().TeamID
		channelID := u.br.GetChannelID(u.ctx, channelName, teamID)

		brchannel, err := u.br.GetChannel(u.ctx, channelID)
		if err != nil {
			return "", nil, fmt.Errorf("channel '#%s' not found", channelName)
		}

		var events []*bridge.Event
		if query.Since > 0 {
			events = u.br.GetPostsSince(u.ctx, brchannel.ID, query.Since)
		} else {
			events = u.br.GetPosts(u.ctx, brchannel.ID, query.Count)
		}

		return "#" + channelName, events, nil
	}

	// Direct Message (user or @user)
	userName := strings.TrimPrefix(target, "@")

	channelID := u.br.GetChannelID(u.ctx, userName, "")
	brchannel, err := u.br.GetChannel(u.ctx, channelID)
	if err != nil {
		return "", nil, fmt.Errorf("user '@%s' not found", userName)
	}

	var events []*bridge.Event
	if query.Since > 0 {
		events = u.br.GetPostsSince(u.ctx, brchannel.ID, query.Since)
	} else {
		events = u.br.GetPosts(u.ctx, brchannel.ID, query.Count)
	}

	return "@" + userName, events, nil
}

//nolint:funlen
func parseSummarizeLimit(extraArgs []string, defaultLimit, maxLimit int) (summarizeQuery, error) {
	if maxLimit == 0 {
		maxLimit = 100
	}

	if defaultLimit == 0 {
		defaultLimit = 30
	}

	query := summarizeQuery{Count: defaultLimit}
	hasLimit := false

	for _, arg := range extraArgs {
		if arg == "" {
			continue
		}

		if mode, ok := parseThinkingMode(arg); ok {
			if query.Thinking != "" {
				return summarizeQuery{}, errors.New("thinking mode specified more than once")
			}

			query.Thinking = mode

			continue
		}

		// Try parsing as a time duration (e.g. 24h, 30m)
		d, err := time.ParseDuration(arg)
		if err == nil {
			if hasLimit {
				return summarizeQuery{}, errors.New("limit or duration specified more than once")
			}

			if d <= 0 {
				return summarizeQuery{}, errors.New("duration must be positive")
			}

			query.Since = time.Now().Add(-d).UnixMilli()
			query.Count = 0
			hasLimit = true

			continue
		}

		// Fall back to parsing as an integer count (e.g. 50)
		val, err := strconv.Atoi(arg)
		if err != nil || val <= 0 {
			return summarizeQuery{}, errors.New("invalid option: expected post count (e.g. 50), duration (e.g. 24h), or thinking mode (off, low, medium, high)")
		}

		if hasLimit {
			return summarizeQuery{}, errors.New("limit or duration specified more than once")
		}

		if val > maxLimit {
			val = maxLimit
		}

		query.Count = val
		hasLimit = true
	}

	return query, nil
}

func parseThinkingMode(arg string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "off", "none":
		return "off", true
	case "low":
		return "low", true
	case "medium", "med":
		return "medium", true
	case "high":
		return "high", true
	case "auto":
		return "auto", true
	default:
		return "", false
	}
}

//nolint:funlen,gocyclo
func summarize(u *User, toUser *User, args []string, service string) {
	cfg := u.cfg.Current().AI
	if !cfg.Enabled {
		u.MsgUser(toUser, "AI summarization is not enabled.")

		return
	}

	if len(args) == 0 || len(args) > 3 {
		u.MsgUser(toUser, "Usage: SUMMARIZE <#channel | user | @@thread_id> [post_count | duration] [thinking]")
		u.MsgUser(toUser, "e.g. SUMMARIZE #dev 50, SUMMARIZE #dev 24h low, SUMMARIZE @@q3kz9... high, SUMMARIZE someuser 30m")

		return
	}

	query, err := parseSummarizeLimit(args[1:], cfg.DefaultPostLimit, cfg.MaxPostLimit)
	if err != nil {
		u.MsgUser(toUser, err.Error())

		return
	}

	var aiClient utils.Summarizer

	switch strings.ToLower(cfg.Provider) {
	case "copilot", "github":
		aiClient, err = utils.NewCopilotClient(cfg.Token, cfg.Model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Copilot AI: %v", err))

			return
		}
	default:
		if cfg.ServiceAccountFile == "" {
			u.MsgUser(toUser, "Gemini AI is not configured (missing service_account_file).")

			return
		}

		aiClient, err = utils.NewAIClient(u.ctx, cfg.ServiceAccountFile, cfg.Project, cfg.Location, cfg.Model)
		if err != nil {
			u.MsgUser(toUser, fmt.Sprintf("Failed to initialize Gemini AI: %v", err))

			return
		}
	}

	contextLabel, events, err := u.getSummarizeEvents(args[0], query)
	if err != nil {
		u.MsgUser(toUser, err.Error())

		return
	}

	if len(events) == 0 {
		u.MsgUser(toUser, "No messages found to summarize.")

		return
	}

	ellipsis := "..."
	if u.br.FormatterConfig().Unicode {
		ellipsis = "…"
	}

	u.MsgUser(toUser, fmt.Sprintf("Fetching messages for %s and generating summary%s", contextLabel, ellipsis))

	var transcript strings.Builder

	for _, event := range events {
		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			if e.Sender != nil && e.Text != "" {
				fmt.Fprintf(&transcript, "%s: %s\n", e.Sender.Nick, e.Text)
			}
		case *bridge.DirectMessageEvent:
			if e.Sender != nil && e.Text != "" {
				fmt.Fprintf(&transcript, "%s: %s\n", e.Sender.Nick, e.Text)
			}
		}
	}

	extraInstructions := ""
	if trimmed := strings.TrimSpace(cfg.Prompt); trimmed != "" {
		extraInstructions = trimmed + " "
	}

	prompt := fmt.Sprintf(
		"You are an assistant summarizing chat history for an IRC client. "+
			"Summarize the following %s conversation. "+
			"%sHighlight decisions, action items, and main points. Prefix users with '@'. "+
			"Do not include a title or Markdown tables.\n\n"+
			"[TRANSCRIPT]\n%s[/TRANSCRIPT]\n",
		contextLabel, extraInstructions, transcript.String(),
	)

	summary, err := aiClient.Summarize(u.ctx, prompt, query.Thinking)
	if err != nil {
		u.MsgUser(toUser, fmt.Sprintf("Summarization failed: %v", err))

		return
	}

	heading := "Summary for " + contextLabel
	headingLen := 4 + len(heading) + 4

	u.MsgUser(toUser, "=== \x02\x1d"+heading+"\x1d\x02 ===")

	disableEmoji := u.br.FormatterConfig().DisableEmoji
	disableMarkdown := u.br.FormatterConfig().DisableMarkdown
	inlineCode := u.br.FormatterConfig().MarkdownInlineCode
	blockQuoteChar, codeBlockPrefix := u.getMarkdownBlockCodePrefix()
	syntaxHighlighting := u.br.FormatterConfig().SyntaxHighlighting
	codeBlockSeparator := u.br.FormatterConfig().CodeBlockSeparator
	preserveNewLines := u.br.FormatterConfig().PreserveNewLines

	opts := utils.ProcessMessageOpts{
		DisableMarkdown:    disableMarkdown,
		DisableEmoji:       disableEmoji,
		SyntaxHighlighting: syntaxHighlighting,
		CodeBlockPrefix:    codeBlockPrefix,
		CodeBlockSeparator: codeBlockSeparator,
		BlockquoteChar:     blockQuoteChar,
		InlineCodeChar:     inlineCode,
		PreserveNewLines:   preserveNewLines,
	}

	utils.ProcessMessageText(summary, opts, func(line string) {
		trimmed := strings.TrimRight(line, " \r\t")
		if trimmed != "" {
			u.MsgUser(toUser, trimmed)
		}
	})

	u.MsgUser(toUser, strings.Repeat("=", headingLen))
}
