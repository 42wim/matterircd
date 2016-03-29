package irckit

import (
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/42wim/matterbridge-plus/matterclient"
	"github.com/gorilla/websocket"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

func NewUserMM(c net.Conn, srv Server, cfg *MmCfg) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv
	u.MmInfo.Cfg = cfg

	// used for login
	mattermostService := &User{Nick: "mattermost", User: "mattermost", Real: "loginservice", Host: "service", channels: map[Channel]struct{}{}}
	mattermostService.MmGhostUser = true
	srv.Add(mattermostService)
	if _, ok := srv.HasUser("mattermost"); !ok {
		go srv.Handle(mattermostService)
	}

	return u
}

func (u *User) loginToMattermost() (*matterclient.MMClient, error) {
	mc := matterclient.New(u.Credentials.Login, u.Credentials.Pass, u.Credentials.Team, u.Credentials.Server)
	if u.Cfg.Insecure {
		mc.Credentials.NoTLS = true
	}
	if logger.LogDebug() {
		mc.SetLogLevel("debug")
	}
	err := mc.Login()
	if err != nil {
		return nil, err
	}
	u.MmWsQuit = false
	return mc, nil
}

func (u *User) logoutFromMattermost() error {
	logger.Debug("LOGOUT")
	u.mc.Client.Logout()
	u.MmWsQuit = true
	u.mc.WsClient.Close()
	u.mc.WsClient.UnderlyingConn().Close()
	u.mc.WsClient = nil
	u.Srv.Logout(u)
	return nil
}

func (u *User) createMMUser(mmuser *model.User) *User {
	if ghost, ok := u.Srv.HasUser(mmuser.Username); ok {
		return ghost
	}
	ghost := &User{Nick: mmuser.Username, User: mmuser.Id, Real: mmuser.FirstName + " " + mmuser.LastName, Host: u.mc.Client.Url, channels: map[Channel]struct{}{}}
	ghost.MmGhostUser = true
	return ghost
}

func (u *User) addUsersToChannels() {
	var mmConnected bool
	srv := u.Srv
	// already connected to a mm server ? add teamname as suffix
	if _, ok := srv.HasChannel("#town-square"); ok {
		//mmConnected = true
	}
	rate := time.Second / 1
	throttle := time.Tick(rate)

	for _, mmchannel := range u.mc.Channels.Channels {

		// exclude direct messages
		if strings.Contains(mmchannel.Name, "__") {
			continue
		}
		<-throttle
		go func(mmchannel *model.Channel) {
			edata, _ := u.mc.Client.GetChannelExtraInfo(mmchannel.Id, -1, "")
			if mmConnected {
				mmchannel.Name = mmchannel.Name + "-" + u.mc.Team.Name
			}

			// join ourself to all channels
			ch := srv.Channel("#" + mmchannel.Name)
			ch.Topic(u, u.mc.GetChannelHeader(mmchannel.Id))
			ch.Join(u)

			// add everyone on the MM channel to the IRC channel
			for _, d := range edata.Data.(*model.ChannelExtra).Members {
				if mmConnected {
					d.Username = d.Username + "-" + u.mc.Team.Name
				}
				// already joined
				if d.Id == u.mc.User.Id {
					continue
				}

				cghost, ok := srv.HasUser(d.Username)
				if !ok {
					ghost := u.createMMUser(u.mc.Users[d.Id])
					ghost.MmGhostUser = true
					logger.Info("adding", ghost.Nick, "to #"+mmchannel.Name)
					srv.Add(ghost)
					go srv.Handle(ghost)
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(ghost)
				} else {
					ch := srv.Channel("#" + mmchannel.Name)
					ch.Join(cghost)
				}
			}

			// post everything to the channel you haven't seen yet
			postlist := u.mc.GetPostsSince(mmchannel.Id, u.mc.Channels.Members[mmchannel.Id].LastViewedAt)
			if postlist == nil {
				logger.Errorf("something wrong with getMMPostsSince")
				return
			}
			logger.Debugf("%#v", u.mc.Channels.Members[mmchannel.Id])
			// traverse the order in reverse
			for i := len(postlist.Order) - 1; i >= 0; i-- {
				for _, post := range strings.Split(postlist.Posts[postlist.Order[i]].Message, "\n") {
					ch.SpoofMessage(u.mc.Users[postlist.Posts[postlist.Order[i]].UserId].Username, post)
				}
			}
			u.mc.UpdateLastViewed(mmchannel.Id)

		}(mmchannel)
	}

	// add all users, also who are not on channels
	for _, mmuser := range u.mc.Users {
		// do not add our own nick
		if mmuser.Id == u.mc.User.Id {
			continue
		}
		_, ok := srv.HasUser(mmuser.Username)
		if !ok {
			if mmConnected {
				mmuser.Username = mmuser.Username + "-" + u.mc.Team.Name
			}
			ghost := u.createMMUser(mmuser)
			ghost.MmGhostUser = true
			logger.Info("adding", ghost.Nick, "without a channel")
			srv.Add(ghost)
			go srv.Handle(ghost)
		}
	}
}

type MmInfo struct {
	MmGhostUser    bool
	MmClient       *model.Client
	MmWsClient     *websocket.Conn
	MmWsQuit       bool
	Srv            Server
	MmUsers        map[string]*model.User
	MmUser         *model.User
	MmChannels     *model.ChannelList
	MmMoreChannels *model.ChannelList
	MmTeam         *model.Team
	Credentials    *MmCredentials
	Cfg            *MmCfg
	mc             *matterclient.MMClient
}

type MmCredentials struct {
	Login  string
	Team   string
	Pass   string
	Server string
}

type MmCfg struct {
	AllowedServers []string
	DefaultServer  string
	DefaultTeam    string
	Insecure       bool
}

func (u *User) WsReceiver() {
	var rmsg model.Message
	for {
		if u.MmWsQuit {
			logger.Debug("exiting WsReceiver")
			return
		}
		logger.Debug("in WsReceiver")
		if err := u.mc.WsClient.ReadJSON(&rmsg); err != nil {
			logger.Critical(err)
			if u.MmWsQuit {
				logger.Debug("exiting WsReceiver - MmWsQuit - ReadJSON")
				return
			}
			// did the user quit
			if _, ok := u.Srv.HasUser(u.Nick); !ok {
				logger.Debug("user has quit, not reconnecting")
				u.mc.WsClient.Close()
				return
			}
			// reconnect
			u.mc, _ = u.loginToMattermost()
			u.addUsersToChannels()
		}
		logger.Debugf("WsReceiver: %#v", rmsg)
		switch rmsg.Action {
		case model.ACTION_POSTED:
			u.handleWsActionPost(&rmsg)
		case model.ACTION_USER_REMOVED:
			u.handleWsActionUserRemoved(&rmsg)
		case model.ACTION_USER_ADDED:
			u.handleWsActionUserAdded(&rmsg)
		}
	}
}

func (u *User) handleWsActionPost(rmsg *model.Message) {
	data := model.PostFromJson(strings.NewReader(rmsg.Props["post"]))
	logger.Debug("receiving userid", data.UserId)
	if data.UserId == u.mc.User.Id {
		// our own message - http://www.fileformat.info/info/unicode/char/180e/fontsupport.htm
		if strings.Contains(data.Message, "᠎") {
			logger.Debugf("message is sent from IRC, contains unicode, not relaying", data.Message)
			return
		}
	}
	// we don't have the user, refresh the userlist
	if u.mc.Users[data.UserId] == nil {
		u.mc.UpdateUsers()
	}
	ghost := u.createMMUser(u.mc.Users[data.UserId])
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == u.mc.User.Id {
		ghost = u
	}
	rcvchannel := u.mc.GetChannelName(data.ChannelId)
	// direct message
	if strings.Contains(rcvchannel, "__") {
		// our own message, ignore because we can't handle/fake those on IRC
		if data.UserId == u.mc.User.Id {
			return
		}
		logger.Debug("direct message")
		var rcvuser string
		rcvusers := strings.Split(rcvchannel, "__")
		if rcvusers[0] != u.mc.User.Id {
			rcvuser = u.mc.Users[rcvusers[0]].Username
		} else {
			rcvuser = u.mc.Users[rcvusers[1]].Username
		}
		msgs := strings.Split(data.Message, "\n")
		for _, m := range msgs {
			u.MsgSpoofUser(rcvuser, m)
		}
		return
	}

	logger.Debugf("channel id %#v, name %#v", data.ChannelId, u.mc.GetChannelName(data.ChannelId))
	ch := u.Srv.Channel("#" + rcvchannel)

	// join if not in channel
	if !ch.HasUser(ghost) {
		ch.Join(ghost)
	}
	msgs := strings.Split(data.Message, "\n")

	// check if we have a override_username (from webhooks) and use it
	props := map[string]interface{}(data.Props)
	overrideUsername, _ := props["override_username"].(string)
	for _, m := range msgs {
		if overrideUsername != "" {
			ch.SpoofMessage(overrideUsername, m)
		} else {
			ch.Message(ghost, m)
		}
	}

	if len(data.Filenames) > 0 {
		logger.Debugf("files detected")
		for _, fname := range data.Filenames {
			logger.Debug("filename: ", fname)
			ch.Message(ghost, "download file - https://"+u.Credentials.Server+"/api/v1/files/get"+fname)
		}
	}
	logger.Debug(u.mc.Users[data.UserId].Username, ":", data.Message)
	logger.Debugf("%#v", data)

	// updatelastviewed
	u.mc.UpdateLastViewed(data.ChannelId)
	return
}

func (u *User) handleWsActionUserRemoved(rmsg *model.Message) {
	if u.mc.Users[rmsg.UserId] == nil {
		u.mc.UpdateUsers()
	}
	ch := u.Srv.Channel("#" + u.mc.GetChannelName(rmsg.ChannelId))

	// remove ourselves from the channel
	if rmsg.UserId == u.mc.User.Id {
		ch.Part(u, "")
		return
	}

	ghost := u.createMMUser(u.mc.Users[rmsg.UserId])
	if ghost == nil {
		logger.Debug("couldn't remove user", rmsg.UserId, u.mc.Users[rmsg.UserId].Username)
		return
	}
	ch.Part(ghost, "")
	return
}

func (u *User) handleWsActionUserAdded(rmsg *model.Message) {
	if u.mc.GetChannelName(rmsg.ChannelId) == "" {
		u.mc.UpdateChannels()
	}

	if u.mc.Users[rmsg.UserId] == nil {
		u.mc.UpdateUsers()
	}

	ch := u.Srv.Channel("#" + u.mc.GetChannelName(rmsg.ChannelId))
	// add ourselves to the channel
	if rmsg.UserId == u.mc.User.Id {
		logger.Debug("ACTION_USER_ADDED adding myself to", u.mc.GetChannelName(rmsg.ChannelId), rmsg.ChannelId)
		ch.Topic(u, u.mc.GetChannelHeader(rmsg.ChannelId))
		ch.Join(u)
		return
	}
	ghost := u.createMMUser(u.mc.Users[rmsg.UserId])
	if ghost == nil {
		logger.Debug("couldn't add user", rmsg.UserId, u.mc.Users[rmsg.UserId].Username)
		return
	}
	ch.Join(ghost)
	return
}

func (u *User) MsgUser(toUser *User, msg string) {
	u.Encode(&irc.Message{
		Prefix:   toUser.Prefix(),
		Command:  irc.PRIVMSG,
		Params:   []string{u.Nick},
		Trailing: msg,
	})
}

func (u *User) MsgSpoofUser(rcvuser string, msg string) {
	u.Encode(&irc.Message{
		Prefix:   &irc.Prefix{Name: rcvuser, User: rcvuser, Host: rcvuser},
		Command:  irc.PRIVMSG,
		Params:   []string{u.Nick},
		Trailing: msg,
	})
}

func (u *User) handleMMDM(toUser *User, msg string) {
	var channel string
	// We don't have a DM with this user yet.
	if u.mc.GetChannelId(toUser.User+"__"+u.mc.User.Id) == "" && u.mc.GetChannelId(u.mc.User.Id+"__"+toUser.User) == "" {
		// create DM channel
		_, err := u.mc.Client.CreateDirectChannel(map[string]string{"user_id": toUser.User})
		if err != nil {
			logger.Debugf("direct message to %#v failed: %s", toUser, err)
		}
		// update our channels
		mmchannels, _ := u.mc.Client.GetChannels("")
		u.mc.Channels = mmchannels.Data.(*model.ChannelList)
	}

	// build the channel name
	if toUser.User > u.mc.User.Id {
		channel = u.mc.User.Id + "__" + toUser.User
	} else {
		channel = toUser.User + "__" + u.mc.User.Id
	}
	// build & send the message
	msg = strings.Replace(msg, "\r", "", -1)
	post := &model.Post{ChannelId: u.mc.GetChannelId(channel), Message: msg}
	u.mc.Client.CreatePost(post)
}

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
			go u.WsReceiver()
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

func (u *User) syncMMChannel(id string, name string) {
	var mmConnected bool
	srv := u.Srv
	// already connected to a mm server ? add teamname as suffix
	if _, ok := srv.HasChannel("#town-square"); ok {
		//mmConnected = true
	}

	edata, _ := u.mc.Client.GetChannelExtraInfo(id, -1, "")
	for _, d := range edata.Data.(*model.ChannelExtra).Members {
		if mmConnected {
			d.Username = d.Username + "-" + u.mc.Team.Name
		}
		// join all the channels we're on on MM
		if d.Id == u.mc.User.Id {
			ch := srv.Channel("#" + name)
			logger.Debug("syncMMChannel adding myself to ", name, id)
			ch.Topic(u, u.mc.GetChannelHeader(id))
			ch.Join(u)
		}

		cghost, ok := srv.HasUser(d.Username)
		if !ok {
			ghost := u.createMMUser(u.mc.Users[d.Id])
			ghost.MmGhostUser = true
			logger.Info("adding", ghost.Nick, "to #"+name)
			srv.Add(ghost)
			go srv.Handle(ghost)
			ch := srv.Channel("#" + name)
			ch.Join(ghost)
		} else {
			ch := srv.Channel("#" + name)
			ch.Join(cghost)
		}
	}
}

func (u *User) joinMMChannel(channel string) error {
	u.mc.JoinChannel(channel)
	u.syncMMChannel(u.mc.GetChannelId(strings.Replace(channel, "#", "", 1)), strings.Replace(channel, "#", "", 1))
	return nil
}

func (u *User) isValidMMServer(server string) bool {
	if len(u.Cfg.AllowedServers) > 0 {
		logger.Debug("allowedservers:", u.Cfg.AllowedServers)
		for _, srv := range u.Cfg.AllowedServers {
			if srv == server {
				return true
			}
		}
		return false
	}
	return true
}
