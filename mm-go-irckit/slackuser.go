package irckit

import (
	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/slack"
	//"github.com/slack-go/slack"
)

type SlackInfo struct {
	inprogress bool
	/*	Token      string
		sc         *slack.Client
		rtm        *slack.RTM
		sinfo      *slack.Info
		susers     map[string]slack.User
		connected  bool
		inprogress bool
		// br         bridge.Bridger
		sync.RWMutex
	*/
}

// code taken from tanya project
// see https://github.com/nolanlum/tanya/blob/master/LICENSE
/*
func (u *User) getSlackToken() (string, error) {
	type findTeamResponseFull struct {
		SSO    bool   `json:"sso"`
		TeamID string `json:"team_id"`
		slack.SlackResponse
	}
	type loginResponseFull struct {
		Token string `json:"token"`
		slack.SlackResponse
	}

	resp, err := http.PostForm("https://slack.com/api/auth.findTeam", url.Values{"domain": {u.Credentials.Team}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var findTeamResponse findTeamResponseFull
	err = json.Unmarshal(body, &findTeamResponse)
	if err != nil {
		return "", err
	}
	if findTeamResponse.SSO {
		return "", errors.New("SSO teams not yet supported")
	}
	resp, err = http.PostForm("https://slack.com/api/auth.signin",
		url.Values{"team": {findTeamResponse.TeamID}, "email": {u.Credentials.Login}, "password": {u.Credentials.Pass}})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var loginResponse loginResponseFull
	err = json.Unmarshal(body, &loginResponse)
	if err != nil {
		return "", err
	}

	if !loginResponse.Ok {
		return "", errors.New(loginResponse.Error)
	}
	return loginResponse.Token, nil
}
*/

func (u *User) loginToSlack() error {
	cred := slack.Credentials{
		Login:  u.Credentials.Login,
		Pass:   u.Credentials.Pass,
		Team:   u.Credentials.Team,
		Server: u.Credentials.Server,
		Token:  u.Credentials.Token,
	}

	eventChan := make(chan *bridge.Event)
	br, err := slack.New(u.MmInfo.Cfg, cred, eventChan, u.addUsersToChannels)
	if err != nil {
		return err
	}

	u.br = br

	go u.handleEventChan(eventChan)

	return nil
}

/*

	var err error
	if u.Credentials != nil {
		u.Token, err = u.getSlackToken()
		if err != nil {
			return nil, err
		}
	}
	u.sc = slack.New(u.Token)
	u.rtm = u.sc.NewRTM()
	u.Lock()
	u.susers = make(map[string]slack.User)
	u.Unlock()
	go u.rtm.ManageConnection()
	// time.Sleep(time.Second * 2)
	u.sinfo = u.rtm.GetInfo()
	count := 0
	for u.sinfo == nil {
		time.Sleep(time.Millisecond * 500)
		logger.Debug("still waiting for sinfo")
		u.sinfo = u.rtm.GetInfo()
		count++
		if count == 20 {
			return nil, errors.New("couldn't connect in 10 seconds. Check your credentials")
		}
	}

	u.br = ircslack.New(u.sc, u.sinfo)

	// we only know which server we are connecting to when we actually are connected.
	// disconnect if we're not allowed
	if len(u.MmInfo.Cfg.SlackSettings.Restrict) > 0 {
		ok := false
		for _, domain := range u.MmInfo.Cfg.SlackSettings.Restrict {
			if domain == u.sinfo.Team.Domain {
				ok = true
				break
			}
		}
		if !ok {
			u.rtm.Disconnect()
			return nil, errors.New("Not allowed to connect to " + u.sinfo.Team.Domain + " slack")
		}
	}
	// we only know which user we are when we actually are connected.
	// disconnect if we're not allowed
	if len(u.MmInfo.Cfg.SlackSettings.BlackListUser) > 0 {
		ok := false
		for _, user := range u.MmInfo.Cfg.SlackSettings.BlackListUser {
			if user == u.sinfo.User.Name {
				ok = true
				break
			}
		}
		if ok {
			u.rtm.Disconnect()
			return nil, errors.New("Not allowed to connect")
		}
	}

	go u.handleSlack()
	u.addSlackUsersToChannels()
	u.connected = true
	return u.sc, nil
}
*/

func (u *User) logoutFromSlack() error {
	u.Srv.Logout(u)
	return nil
}

/*
func (u *User) createSlackUser(slackuser *slack.User) *User {
	if slackuser == nil {
		return nil
	}

	if ghost, ok := u.Srv.HasUser(slackuser.Name); ok {
		return ghost
	}

	ghost := &User{
		UserInfo: &bridge.UserInfo{
			Nick:        slackuser.Name,
			User:        slackuser.ID,
			Real:        slackuser.RealName,
			Host:        "host",
			Roles:       "",
			DisplayName: slackuser.Profile.DisplayName,
			Ghost:       true,
		},
		channels: map[Channel]struct{}{},
	}

	u.Srv.Add(ghost)

	return ghost
}
*/

/*
func (u *User) addSlackUserToChannel(user *slack.User, channel string, channelId string) {
	if user == nil {
		return
	}
	ghost := u.createSlackUser(user)
	if ghost == nil {
		logger.Warnf("Cannot join %v into %s", user, channel)
		return
	}
	logger.Debugf("adding %s to %s (%s)", ghost.Nick, channel, channelId)
	ch := u.Srv.Channel(channelId)
	logger.Debugf("channel: %#v %#v", ch.String(), ch.ID())
	ch.Join(ghost)
}
*/

/*
func (u *User) addSlackUsersToChannels() {
	srv := u.Srv
	throttle := time.Tick(time.Millisecond * 100)
	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")
	users, _ := u.sc.GetUsers()
	for _, mmuser := range users {
		// do not add our own nick
		if mmuser.ID == u.sinfo.User.ID {
			continue
		}
		u.createSlackUser(&mmuser)
		u.addSlackUserToChannel(&mmuser, "&users", "&users")
		u.Lock()
		u.susers[mmuser.ID] = mmuser
		u.Unlock()
	}
	ch.Join(u)

	channels := make(chan interface{}, 10)
	for i := 0; i < 10; i++ {
		go u.addSlackUserToChannelWorker(channels, throttle)
	}

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: "true",
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

	for {
		mmchannels, nextCursor, _ := u.sc.GetConversations(&params)
		params.Cursor = nextCursor
		for _, mmchannel := range mmchannels {
			if mmchannel.IsMember {
				if mmchannel.IsMpIM && u.Cfg.SlackSettings.JoinMpImOnTalk {
					continue
				}
				logger.Debug("Adding channel", mmchannel)
				channels <- mmchannel
			}
		}
		if nextCursor == "" {
			break
		}
	}
	close(channels)
}
*/

/*

func (u *User) addSlackUserToChannelWorker(channels <-chan interface{}, throttle <-chan time.Time) {
	var ID, name string
	for {
		mmchannel, ok := <-channels
		if !ok {
			logger.Debug("Done adding user to channels")
			return
		}
		<-throttle
		switch mmchannel.(type) {
		case slack.Channel:
			ID = mmchannel.(slack.Channel).ID
			name = mmchannel.(slack.Channel).Name
			u.syncSlackChannel(ID, name)
		case slack.Group:
			ID = mmchannel.(slack.Group).ID
			name = mmchannel.(slack.Group).Name
			logger.Debugf("GROUP %#v", mmchannel.(slack.Group))
			u.syncSlackGroup(ID, name)

		}
		// exclude direct messages
		// var spoof func(string, string)
		// ch := u.Srv.Channel(mmchannel.ID)
		// post everything to the channel you haven't seen yet
	}
}


func formatTs(unixts string) string {
	var targetts, targetus int64
	fmt.Sscanf(unixts, "%d.%d", &targetts, &targetus)
	ts := time.Unix(targetts, targetus*1000)

	if ts.YearDay() != time.Now().YearDay() {
		return ts.Format("2.1. 15:04:05")
	} else {
		return ts.Format("15:04:05")
	}
}
*/
/*
func (u *User) handleSlack() {
	for {
		logger.Debug("in handleSlack")
		for msg := range u.rtm.IncomingEvents {
			switch ev := msg.Data.(type) {
			case *slack.MessageEvent:
				if ev.SubType == "group_join" {
					u.syncSlackGroup(ev.Channel, "")
				}
				if ev.SubType == "channel_join" {
					u.syncSlackChannel(ev.Channel, "")
				}
				if ev.SubType == "message_deleted" {
					ts := formatTs(ev.DeletedTimestamp)
					msg := "[M " + ts + "] Message deleted"
					u.handleSlackActionMisc("USLACKBOT", ev.Channel, msg)
				}
				u.handleSlackActionPost(ev)
			case *slack.DisconnectedEvent:
				logger.Debug("disconnected event received, we should reconnect now..")
				// return
			case *slack.ReactionAddedEvent:
				logger.Debugf("ReactionAdded msg %#v", ev)
				ts := formatTs(ev.Item.Timestamp)
				msg := "[M " + ts + "] Added reaction :" + ev.Reaction + ":"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			case *slack.ReactionRemovedEvent:
				logger.Debugf("ReactionRemoved msg %#v", ev)
				ts := formatTs(ev.Item.Timestamp)
				msg := "[M " + ts + "] Removed reaction :" + ev.Reaction + ":"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			case *slack.StarAddedEvent:
				logger.Debugf("StarAdded msg %#v", ev)
				ts := formatTs(ev.Item.Message.Timestamp)
				msg := "[M " + ts + "] Message starred (" + ev.Item.Message.Text + ")"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			case *slack.StarRemovedEvent:
				logger.Debugf("StarRemoved msg %#v", ev)
				ts := formatTs(ev.Item.Message.Timestamp)
				msg := "[M " + ts + "] Message unstarred (" + ev.Item.Message.Text + ")"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			case *slack.PinAddedEvent:
				logger.Debugf("PinAdded msg %#v", ev)
				ts := formatTs(ev.Item.Message.Timestamp)
				msg := "[M " + ts + "] Message pinned (" + ev.Item.Message.Text + ")"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			case *slack.PinRemovedEvent:
				logger.Debugf("PinRemoved msg %#v", ev)
				ts := formatTs(ev.Item.Message.Timestamp)
				msg := "[M " + ts + "] Message unpinned (" + ev.Item.Message.Text + ")"
				u.handleSlackActionMisc(ev.User, ev.Item.Channel, msg)
			}
		}
	}
}
*/

/*
func (u *User) handleSlackActionMisc(userid string, channel string, message string) {
	var ch Channel

	user, err := u.rtm.GetUserInfo(userid)
	if err != nil {
		return
	}

	// create new "ghost" user
	ghost := u.createSlackUser(user)

	spoofUsername := ""
	if user != nil {
		spoofUsername = user.ID
		if ghost != nil {
			spoofUsername = ghost.Nick
			if ghost.DisplayName != "" && ghost.DisplayName != ghost.Nick && u.MmInfo.Cfg.SlackSettings.UseDisplayName {
				spoofUsername = "|"
				//	spoofUsername = ghost.DisplayName
			}
		}
	}

	msgs := strings.Split(message, "\n")

	// direct message
	ch = u.Srv.Channel(channel)

	// do not join channel for direct messages
	if !strings.HasPrefix(channel, "D") {
		if ghost != nil {
			// join if not in channel
			if !ch.HasUser(ghost) {
				ch.Join(ghost)
			}
		}

		// join channel if we haven't yet
		if !ch.HasUser(u) {
			ch.Join(u)
		}
	}

	spoofUsername = strings.Replace(spoofUsername, " ", "_", -1)
	for _, m := range msgs {
		// cleanup the message
		m = u.replaceMention(m)
		m = u.replaceVariable(m)
		m = u.replaceChannel(m)
		m = u.replaceURL(m)
		m = html.UnescapeString(m)

		if strings.HasPrefix(channel, "D") {
			if u.Nick == ghost.Nick {
				members, _, _ := u.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: channel})
				for _, member := range members {
					if member != u.sinfo.User.ID {
						ghostuser, _ := u.rtm.GetUserInfo(member)
						ghost := u.createSlackUser(ghostuser)
						u.MsgSpoofUser(u, ghost.Nick, m)
					}
				}
			} else {
				u.MsgSpoofUser(ghost, u.Nick, m)
			}
		} else {
			if ghost != nil && ghost.DisplayName != "" && ghost.DisplayName != ghost.Nick &&
				u.MmInfo.Cfg.SlackSettings.UseDisplayName {
				m = "<" + ghost.DisplayName + "> " + m
			}
			ch.SpoofMessage(spoofUsername, m)
		}
	}
}

func (u *User) handleSlackActionPost(rmsg *slack.MessageEvent) {
	var ch Channel
	logger.Debugf("handleSlackActionPost() receiving msg %#v", rmsg)
	// ignore specific subtypes
	switch rmsg.SubType {
	case "channel_join":
		return
	case "message_deleted":
		return
	}
	if len(rmsg.Attachments) > 0 {
		// skip messages we made ourselves
		if rmsg.Attachments[0].CallbackID == "matterircd_"+u.sinfo.User.ID {
			return
		}
	}

	usr := rmsg.User
	if rmsg.SubType == "message_changed" {
		usr = rmsg.SubMessage.User
	}
	user, err := u.rtm.GetUserInfo(usr)
	if err != nil {
		if rmsg.BotID == "" {
			return
		}
	}

	msghandled := false

	// handle bot messages
	botname := ""
	if rmsg.User == "" && rmsg.BotID != "" {
		botname = rmsg.Username
		if botname == "" {
			bot, _ := u.rtm.GetBotInfo(rmsg.BotID)
			if bot.Name != "" {
				botname = bot.Name
			}
		}
	}

	// create new "ghost" user
	ghost := u.createSlackUser(user)

	spoofUsername := ""
	if user != nil {
		spoofUsername = user.ID
		if ghost != nil {
			spoofUsername = ghost.Nick
			if ghost.DisplayName != "" && ghost.DisplayName != ghost.Nick && u.MmInfo.Cfg.SlackSettings.UseDisplayName {
				// spoofUsername = "|"
				spoofUsername = ghost.DisplayName
			}
		}
	}

	// if we have a botname, use it
	if botname != "" {
		spoofUsername = strings.TrimSpace(botname)
	}

	msgs := []string{}

	if rmsg.Text != "" {
		msgs = append(msgs, strings.Split(rmsg.Text, "\n")...)
		msghandled = true
	}

	// direct message

	ch = u.Srv.Channel(rmsg.Channel)

	// do not join channel for direct messages
	if !strings.HasPrefix(rmsg.Channel, "D") {
		if ghost != nil {
			// join if not in channel
			if !ch.HasUser(ghost) {
				ch.Join(ghost)
			}
		}

		// join channel if we haven't yet
		if !ch.HasUser(u) {
			ch.Join(u)
		}
	}

	// look in attachments
	for _, attach := range rmsg.Attachments {
		if attach.Pretext != "" {
			msgs = append(msgs, strings.Split(attach.Pretext, "\n")...)
		}

		if attach.Text != "" {
			for i, row := range strings.Split(attach.Text, "\n") {
				msgs = append(msgs, "> "+row)
				if i > 4 {
					msgs = append(msgs, "> ...")
					break
				}
			}
		}
		msghandled = true
	}

	// List files
	for _, file := range rmsg.Files {
		msgs = append(msgs, "Uploaded "+file.Mode+" "+
			file.Name+" / "+file.Title+" ("+file.Filetype+"): "+file.URLPrivate)
		msghandled = true
	}

	if msghandled {
		if rmsg.ThreadTimestamp != "" && len(msgs) > 0 {
			msgs[0] = "[T " + formatTs(rmsg.ThreadTimestamp) + "] " + msgs[0]
		}
	}
	if rmsg.SubType == "message_changed" {
		msgs = append(msgs, strings.Split(rmsg.SubMessage.Text, "\n")...)
		if len(msgs) > 0 {
			msgs[0] = "[C " + formatTs(rmsg.SubMessage.Timestamp) + "] " + msgs[0]
		}
		msghandled = true
	}

	spoofUsername = strings.Replace(spoofUsername, " ", "_", -1)
	for _, m := range msgs {
		// cleanup the message
		m = u.replaceMention(m)
		m = u.replaceVariable(m)
		m = u.replaceChannel(m)
		m = u.replaceURL(m)
		m = html.UnescapeString(m)

		// still no text, ignore this message
		if !msghandled {
			// continue
			m = fmt.Sprintf("Empty: %#v", rmsg)
		}

		if strings.HasPrefix(rmsg.Channel, "D") {
			if u == nil || ghost == nil {
				logger.Errorf("%#v or %#v is nil with msg %#v. Shouldn't happen", u, ghost, rmsg)
				return
			}
			if u.Nick == ghost.Nick {
				members, _, _ := u.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: rmsg.Channel})
				for _, member := range members {
					if member != u.sinfo.User.ID {
						ghostuser, _ := u.rtm.GetUserInfo(member)
						ghost := u.createSlackUser(ghostuser)
						u.MsgSpoofUser(u, ghost.Nick, m)
					}
				}
			} else {
				u.MsgSpoofUser(ghost, u.Nick, m)
			}
		} else {
			if ghost != nil && ghost.DisplayName != "" && ghost.DisplayName != ghost.Nick &&
				u.MmInfo.Cfg.SlackSettings.UseDisplayName {
				m = "<" + ghost.DisplayName + "> " + m
			}
			ch.SpoofMessage(spoofUsername, m)
		}
	}
}
*/
// sync IRC with mattermost channel state
/*
func (u *User) syncSlackChannel(id string, name string) {
	srv := u.Srv
	info, err := u.sc.GetConversationInfo(id, false)
	if err != nil {
		logger.Info(err)
	}
	if info == nil {
		logger.Info("Unknown channel seen (" + id + ")")
		return
	}

	if name == "" {
		name = info.Name
	}

	params := slack.GetUsersInConversationParameters{
		ChannelID: id,
		Cursor:    "",
		Limit:     100,
	}

	for {
		members, nextCursor, _ := u.sc.GetUsersInConversation(&params)
		params.Cursor = nextCursor
		for _, user := range members {
			if u.sinfo.User.ID != user {
				// slackuser, _ := u.sc.GetUserInfo(user)
				slackuser := u.getSlackUser(user)
				if slackuser != nil {
					u.addSlackUserToChannel(slackuser, "#"+name, id)
				}
			}
		}
		if nextCursor == "" {
			break
		}
	}

	// Add slackbot to all channels
	slackuser := u.getSlackUser("USLACKBOT")
	if slackuser != nil {
		u.addSlackUserToChannel(slackuser, "#"+name, id)
	}

	ch := srv.Channel(id)
	svc, _ := srv.HasUser("slack")
	ch.Topic(svc, info.Topic.Value)
	if !ch.HasUser(u) {
		logger.Debugf("syncSlackchannel adding myself to %s (id: %s)", name, id)
		ch.Join(u)
	}
}
*/

// sync IRC with mattermost channel state
/*
func (u *User) syncSlackGroup(id string, name string) {
	srv := u.Srv
	info, err := u.sc.GetGroupInfo(id)
	if err != nil {
		logger.Info(err)
	}

	if name == "" {
		name = info.Name
	}

	for _, user := range info.Members {
		if u.sinfo.User.ID != user {
			// slackuser, _ := u.sc.GetUserInfo(user)
			slackuser := u.getSlackUser(user)
			if slackuser != nil {
				u.addSlackUserToChannel(slackuser, "#"+name, id)
			}
		}
	}

	// Add slackbot to all channels
	slackuser := u.getSlackUser("USLACKBOT")
	if slackuser != nil {
		u.addSlackUserToChannel(slackuser, "#"+name, id)
	}

	ch := srv.Channel(id)
	svc, _ := srv.HasUser("slack")
	ch.Topic(svc, info.Topic.Value)
	if !ch.HasUser(u) {
		logger.Debugf("syncSlackchannel adding myself to %s (id: %s)", name, id)
		ch.Join(u)
	}
}
*/

/*
// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (u *User) replaceMention(text string) string {
	results := regexp.MustCompile(`<@([a-zA-z0-9]+)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, "<@"+r[1]+">", "@"+u.userName(r[1]), -1)
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_channels_and_users
func (u *User) replaceChannel(text string) string {
	results := regexp.MustCompile(`<#[a-zA-Z0-9]+\|(.+?)>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, r[0], "#"+r[1], -1)
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#variables
func (u *User) replaceVariable(text string) string {
	results := regexp.MustCompile(`<!((?:subteam\^)?[a-zA-Z0-9]+)(?:\|@?(.+?))?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		if r[2] != "" {
			text = strings.Replace(text, r[0], "@"+r[2], -1)
		} else {
			text = strings.Replace(text, r[0], "@"+r[1], -1)
		}
	}
	return text
}

// @see https://api.slack.com/docs/message-formatting#linking_to_urls
func (u *User) replaceURL(text string) string {
	results := regexp.MustCompile(`<(.*?)(\|.*?)?>`).FindAllStringSubmatch(text, -1)
	for _, r := range results {
		text = strings.Replace(text, r[0], r[1], -1)
	}
	return text
}

func (u *User) getSlackUser(name string) *slack.User {
	u.RLock()
	defer u.RUnlock()
	if user, ok := u.susers[name]; ok {
		return &user
	}
	return nil
}

func (u *User) userName(id string) string {
	u.RLock()
	defer u.RUnlock()
	// TODO dynamically update when new users are joining slack
	for _, us := range u.susers {
		if us.ID == id {
			if us.Profile.DisplayName != "" {
				return us.Profile.DisplayName
			}
			return us.Name
		}
	}
	if id == u.sinfo.User.ID {
		return u.sinfo.User.Name
	}
	return ""
}
*/

/*
func (u *User) isConnected() bool {
	return u.connected
}
*/
