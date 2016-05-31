package irckit

import (
	"net"
	"strings"
	"time"

	"github.com/42wim/matterbridge-plus/matterclient"
	"github.com/gorilla/websocket"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

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

func NewUserMM(c net.Conn, srv Server, cfg *MmCfg) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv
	u.MmInfo.Cfg = cfg

	// used for login
	u.createService("mattermost", "loginservice")
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
	u.Srv.Add(ghost)
	go u.Srv.Handle(ghost)
	return ghost
}

func (u *User) createService(nick string, what string) {
	service := &User{Nick: nick, User: nick, Real: what, Host: "service", channels: map[Channel]struct{}{}}
	service.MmGhostUser = true
	u.Srv.Add(service)
	go u.Srv.Handle(service)
}

func (u *User) addUserToChannel(user *model.User, channel string) {
	ghost := u.createMMUser(user)
	logger.Info("adding", ghost.Nick, "to #"+channel)
	ch := u.Srv.Channel("#" + channel)
	ch.Join(ghost)
}

func (u *User) addUsersToChannels() {
	srv := u.Srv
	throttle := time.Tick(time.Millisecond * 300)

	for _, mmchannel := range u.mc.Channels.Channels {
		// exclude direct messages
		if strings.Contains(mmchannel.Name, "__") {
			continue
		}
		<-throttle
		go func(mmchannel *model.Channel) {
			u.syncMMChannel(mmchannel.Id, mmchannel.Name)
			ch := srv.Channel("#" + mmchannel.Name)
			// post everything to the channel you haven't seen yet
			postlist := u.mc.GetPostsSince(mmchannel.Id, u.mc.Channels.Members[mmchannel.Id].LastViewedAt)
			if postlist == nil {
				logger.Errorf("something wrong with getMMPostsSince")
				return
			}
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
		u.createMMUser(mmuser)
	}
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
		// check if we have the users/channels in our cache. If not update
		u.checkWsActionMessage(&rmsg)
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
		// check if this is a message we sent; if so, ignore it
		if u.ownMessages.messageIn(data.ChannelId, data.Message) {
			logger.Debugf("message is sent from IRC, contains unicode, not relaying %#v", data.Message)
			return
		}
		if data.Type == "system_join_leave" {
			logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
			return
		}
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
}

func (u *User) handleWsActionUserRemoved(rmsg *model.Message) {
	ch := u.Srv.Channel("#" + u.mc.GetChannelName(rmsg.ChannelId))

	// remove ourselves from the channel
	if rmsg.UserId == u.mc.User.Id {
		return
	}

	ghost := u.createMMUser(u.mc.Users[rmsg.UserId])
	if ghost == nil {
		logger.Debug("couldn't remove user", rmsg.UserId, u.mc.Users[rmsg.UserId].Username)
		return
	}
	ch.Part(ghost, "")
}

func (u *User) handleWsActionUserAdded(rmsg *model.Message) {
	// do not add ourselves to the channel
	if rmsg.UserId == u.mc.User.Id {
		logger.Debug("ACTION_USER_ADDED not adding myself to", u.mc.GetChannelName(rmsg.ChannelId), rmsg.ChannelId)
		return
	}
	u.addUserToChannel(u.mc.Users[rmsg.UserId], u.mc.GetChannelName(rmsg.ChannelId))
}

func (u *User) checkWsActionMessage(rmsg *model.Message) {
	// Don't check pings
	if rmsg.Action == "ping" {
		return
	}
	logger.Debugf("checkWsActionMessage %#v\n", rmsg)
	if u.mc.GetChannelName(rmsg.ChannelId) == "" {
		u.mc.UpdateChannels()
	}
	if u.mc.Users[rmsg.UserId] == nil {
		u.mc.UpdateUsers()
	}
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
		_, err := u.mc.Client.CreateDirectChannel(toUser.User)
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

// sync IRC with mattermost channel state
func (u *User) syncMMChannel(id string, name string) {
	srv := u.Srv
	edata, _ := u.mc.Client.GetChannelExtraInfo(id, -1, "")
	for _, d := range edata.Data.(*model.ChannelExtra).Members {
		// join all the channels we're on on MM
		if d.Id == u.mc.User.Id {
			ch := srv.Channel("#" + name)
			ch.Topic(u, u.mc.GetChannelHeader(id))
			// only join when we're not yet on the channel
			if !ch.HasUser(u) {
				logger.Debug("syncMMChannel adding myself to ", name, id)
				ch.Join(u)
			}
			continue
		}
		u.addUserToChannel(u.mc.Users[d.Id], name)
	}
}

func (u *User) joinMMChannel(channel string) error {
	u.mc.JoinChannel(channel)
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
