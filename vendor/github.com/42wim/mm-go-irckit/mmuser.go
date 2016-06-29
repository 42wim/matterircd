package irckit

import (
	"net"
	"strings"
	"time"

	"github.com/42wim/matterbridge-plus/matterclient"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

type MmInfo struct {
	MmGhostUser bool
	Srv         Server
	Credentials *MmCredentials
	Cfg         *MmCfg
	mc          *matterclient.MMClient
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
	mc.SetLogLevel(LogLevel)
	logger.Infof("login as %s (team: %s) on %s", u.Credentials.Login, u.Credentials.Team, u.Credentials.Server)
	err := mc.Login()
	if err != nil {
		logger.Error("login failed")
		return nil, err
	}
	logger.Info("login succeeded")
	u.mc = mc
	u.mc.WsQuit = false
	go mc.WsReceiver()
	go u.handleWsMessage()
	return mc, nil
}

func (u *User) logoutFromMattermost() error {
	logger.Infof("logout as %s (team: %s) on %s", u.Credentials.Login, u.Credentials.Team, u.Credentials.Server)
	err := u.mc.Logout()
	if err != nil {
		logger.Error("logout failed")
	}
	logger.Info("logout succeeded")
	u.Srv.Logout(u)
	u.mc = nil
	return nil
}

func (u *User) createMMUser(mmuser *model.User) *User {
	if ghost, ok := u.Srv.HasUser(mmuser.Username); ok {
		return ghost
	}
	ghost := &User{Nick: mmuser.Username, User: mmuser.Id, Real: mmuser.FirstName + " " + mmuser.LastName, Host: u.mc.Client.Url, Roles: mmuser.Roles, channels: map[Channel]struct{}{}}
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

func (u *User) addUserToChannel(user *model.User, channel string, channelId string) {
	ghost := u.createMMUser(user)
	logger.Debugf("adding %s to %s", ghost.Nick, channel)
	ch := u.Srv.Channel(channelId)
	ch.Join(ghost)
}

func (u *User) addUsersToChannels() {
	srv := u.Srv
	throttle := time.Tick(time.Millisecond * 300)
	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")
	for _, mmuser := range u.mc.Users {
		// do not add our own nick
		if mmuser.Id == u.mc.User.Id {
			continue
		}
		u.createMMUser(mmuser)
		u.addUserToChannel(mmuser, "&users", "&users")
	}
	ch.Join(u)

	for _, mmchannel := range u.mc.GetChannels() {
		// exclude direct messages
		if strings.Contains(mmchannel.Name, "__") {
			continue
		}
		<-throttle
		go func(mmchannel *model.Channel) {
			channelName := mmchannel.Name
			if mmchannel.TeamId != u.mc.Team.Id {
				channelName = u.mc.GetTeamName(mmchannel.TeamId) + "/" + mmchannel.Name
			}
			u.syncMMChannel(mmchannel.Id, channelName)
			srv.Channel(mmchannel.Id)
			// post everything to the channel you haven't seen yet
			//postlist := u.mc.GetPostsSince(mmchannel.Id, u.mc.Team.Channels.Members[mmchannel.Id].LastViewedAt)
			postlist := u.mc.GetPostsSince(mmchannel.Id, u.mc.GetLastViewedAt(mmchannel.Id))
			if postlist == nil {
				logger.Error("something wrong with getMMPostsSince")
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
}

func (u *User) handleWsMessage() {
	for {
		if u.mc.WsQuit {
			logger.Debug("exiting handleWsMessage")
			return
		}
		logger.Debug("in handleWsMessage")
		message := <-u.mc.MessageChan
		logger.Debugf("WsReceiver: %#v", message.Raw)
		// check if we have the users/channels in our cache. If not update
		u.checkWsActionMessage(message.Raw)
		switch message.Raw.Action {
		case model.ACTION_POSTED:
			u.handleWsActionPost(message.Raw)
		case model.ACTION_USER_REMOVED:
			u.handleWsActionUserRemoved(message.Raw)
		case model.ACTION_USER_ADDED:
			u.handleWsActionUserAdded(message.Raw)
		}
	}
}

func (u *User) handleWsActionPost(rmsg *model.Message) {
	var ch Channel
	data := model.PostFromJson(strings.NewReader(rmsg.Props["post"]))
	props := rmsg.Props
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Props["post"]))["props"].(map[string]interface{})
	logger.Debugf("handleWsActionPost() receiving userid %s", data.UserId)
	if data.UserId == u.mc.User.Id {
		// space + ZWSP
		if strings.Contains(data.Message, " ​") {
			logger.Debugf("message is sent from IRC, contains unicode, not relaying %#v", data.Message)
			return
		}
		if data.Type == "system_join_leave" {
			logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
			return
		}
	}
	// create new "ghost" user
	ghost := u.createMMUser(u.mc.Users[data.UserId])
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == u.mc.User.Id {
		ghost = u
	}

	spoofUsername := ghost.Nick
	// check if we have a override_username (from webhooks) and use it
	overrideUsername, _ := extraProps["override_username"].(string)
	if overrideUsername != "" {
		spoofUsername = overrideUsername
	}

	msgs := strings.Split(data.Message, "\n")
	// direct message
	if props["channel_type"] == "D" {
		// our own message, ignore because we can't handle/fake those on IRC
		if data.UserId == u.mc.User.Id {
			return
		}
	}

	// not a private message so do channel stuff
	if props["channel_type"] != "D" {
		ch = u.Srv.Channel(data.ChannelId)
		// join if not in channel
		if !ch.HasUser(ghost) {
			ch.Join(ghost)
		}
	}

	// check if we have a override_username (from webhooks) and use it
	for _, m := range msgs {
		if m == "" {
			continue
		}
		if props["channel_type"] == "D" {
			u.MsgSpoofUser(spoofUsername, m)
		} else {
			ch.SpoofMessage(spoofUsername, m)
		}
	}

	if len(data.Filenames) > 0 {
		logger.Debugf("files detected")
		for _, fname := range u.mc.GetPublicLinks(data.Filenames) {
			if props["channel_type"] == "D" {
				u.MsgSpoofUser(spoofUsername, "download file - "+fname)
			} else {
				ch.SpoofMessage(spoofUsername, "download file - "+fname)
			}
		}
	}
	logger.Debugf("handleWsActionPost() user %s sent %s", u.mc.Users[data.UserId].Username, data.Message)
	logger.Debugf("%#v", data)

	// updatelastviewed
	u.mc.UpdateLastViewed(data.ChannelId)
}

func (u *User) handleWsActionUserRemoved(rmsg *model.Message) {
	ch := u.Srv.Channel(rmsg.ChannelId)

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
	u.addUserToChannel(u.mc.Users[rmsg.UserId], "#"+u.mc.GetChannelName(rmsg.ChannelId), rmsg.ChannelId)
}

func (u *User) checkWsActionMessage(rmsg *model.Message) {
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

// sync IRC with mattermost channel state
func (u *User) syncMMChannel(id string, name string) {
	srv := u.Srv
	edata, _ := u.mc.Client.GetChannelExtraInfo(id, -1, "")
	if edata == nil {
		return
	}
	// let everyone join
	for _, d := range edata.Data.(*model.ChannelExtra).Members {
		if d.Id != u.mc.User.Id {
			u.addUserToChannel(u.mc.Users[d.Id], "#"+name, id)
		}
	}
	// before joining ourself
	for _, d := range edata.Data.(*model.ChannelExtra).Members {
		// join all the channels we're on on MM
		if d.Id == u.mc.User.Id {
			ch := srv.Channel(id)
			ch.Topic(u, u.mc.GetChannelHeader(id))
			// only join when we're not yet on the channel
			if !ch.HasUser(u) {
				logger.Debugf("syncMMChannel adding myself to %s (id: %s)", name, id)
				ch.Join(u)
			}
			break
		}
	}
}

func (u *User) isValidMMServer(server string) bool {
	if len(u.Cfg.AllowedServers) > 0 {
		logger.Debugf("allowedservers: %s", u.Cfg.AllowedServers)
		for _, srv := range u.Cfg.AllowedServers {
			if srv == server {
				return true
			}
		}
		return false
	}
	return true
}
