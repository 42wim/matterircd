package irckit

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/42wim/matterbridge/matterclient"
	"github.com/42wim/matterircd/config"
	"github.com/mattermost/platform/model"
	"github.com/sorcix/irc"
)

type MmInfo struct {
	MmGhostUser bool
	Srv         Server
	Credentials *MmCredentials
	Cfg         *MmCfg
	mc          *matterclient.MMClient
	idleStop    chan struct{}
}

type MmCredentials struct {
	Login  string
	Team   string
	Pass   string
	Server string
}

type MmCfg struct {
	AllowedServers     []string
	SlackSettings      config.Settings
	MattermostSettings config.Settings
	DefaultServer      string
	DefaultTeam        string
	Insecure           bool
	SkipTLSVerify      bool
	JoinExclude        []string
	JoinInclude        []string
	PartFake           bool
}

func NewUserMM(c net.Conn, srv Server, cfg *MmCfg) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv
	u.MmInfo.Cfg = cfg
	u.MmInfo.Cfg.AllowedServers = cfg.MattermostSettings.Restrict
	u.MmInfo.Cfg.DefaultServer = cfg.MattermostSettings.DefaultServer
	u.MmInfo.Cfg.DefaultTeam = cfg.MattermostSettings.DefaultTeam
	u.MmInfo.Cfg.JoinInclude = cfg.MattermostSettings.JoinInclude
	u.MmInfo.Cfg.JoinExclude = cfg.MattermostSettings.JoinExclude
	u.MmInfo.Cfg.PartFake = cfg.MattermostSettings.PartFake
	u.MmInfo.Cfg.Insecure = cfg.MattermostSettings.Insecure
	u.MmInfo.Cfg.SkipTLSVerify = cfg.MattermostSettings.SkipTLSVerify

	u.idleStop = make(chan struct{})
	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	return u
}

func (u *User) loginToMattermost() (*matterclient.MMClient, error) {
	mc := matterclient.New(u.Credentials.Login, u.Credentials.Pass, u.Credentials.Team, u.Credentials.Server)
	if u.Cfg.Insecure {
		mc.Credentials.NoTLS = true
	}
	mc.Credentials.SkipTLSVerify = u.Cfg.SkipTLSVerify

	mc.SetLogLevel(LogLevel)
	logger.Infof("login as %s (team: %s) on %s", u.Credentials.Login, u.Credentials.Team, u.Credentials.Server)
	err := mc.Login()
	if err != nil {
		logger.Error("login failed", err)
		return nil, err
	}
	logger.Info("login succeeded")
	u.mc = mc
	u.mc.WsQuit = false
	go mc.WsReceiver()
	go u.handleWsMessage()

	// do anti idle on town-square, every installation should have this channel
	channels := u.mc.GetChannels()
	for _, channel := range channels {
		if channel.Name == "town-square" {
			go u.antiIdle(channel.Id)
			continue
		}
	}
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
	u.idleStop <- struct{}{}
	return nil
}

func (u *User) createMMUser(mmuser *model.User) *User {
	if mmuser == nil {
		return nil
	}
	if ghost, ok := u.Srv.HasUser(mmuser.Username); ok {
		return ghost
	}
	ghost := &User{Nick: mmuser.Username, User: mmuser.Id, Real: mmuser.FirstName + " " + mmuser.LastName, Host: u.mc.Client.Url, Roles: mmuser.Roles, channels: map[Channel]struct{}{}}
	ghost.MmGhostUser = true
	u.Srv.Add(ghost)
	return ghost
}

func (u *User) createService(nick string, what string) {
	service := &User{Nick: nick, User: nick, Real: what, Host: "service", channels: map[Channel]struct{}{}}
	service.MmGhostUser = true
	u.Srv.Add(service)
}

func (u *User) addUserToChannel(user *model.User, channel string, channelId string) {
	if user == nil {
		return
	}
	ghost := u.createMMUser(user)
	if ghost == nil {
		logger.Warnf("Cannot join %v into %s", user, channel)
		return
	}
	logger.Debugf("adding %s to %s", ghost.Nick, channel)
	ch := u.Srv.Channel(channelId)
	ch.Join(ghost)
}

func (u *User) addUsersToChannels() {
	srv := u.Srv
	throttle := time.Tick(time.Millisecond * 50)
	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")
	for _, mmuser := range u.mc.GetUsers() {
		// do not add our own nick
		if mmuser.Id == u.mc.User.Id {
			continue
		}
		u.createMMUser(mmuser)
		u.addUserToChannel(mmuser, "&users", "&users")
	}
	ch.Join(u)

	// channel that receives messages from channels not joined on irc
	ch = srv.Channel("&messages")
	ch.Join(u)

	channels := make(chan *model.Channel, 5)
	for i := 0; i < 10; i++ {
		go u.addUserToChannelWorker(channels, throttle)
	}

	for _, mmchannel := range u.mc.GetChannels() {
		logger.Debug("Adding channel", mmchannel)
		channels <- mmchannel
	}
	close(channels)
}

func (u *User) addUserToChannelWorker(channels <-chan *model.Channel, throttle <-chan time.Time) {
	for {
		mmchannel, ok := <-channels
		if !ok {
			logger.Debug("Done adding user to channels")
			return
		}
		logger.Debug("addUserToChannelWorker", mmchannel)

		<-throttle
		// exclude direct messages
		var spoof func(string, string)
		if strings.Contains(mmchannel.Name, "__") {
			userId := strings.Split(mmchannel.Name, "__")[0]
			u.createMMUser(u.mc.GetUser(userId))
			spoof = u.MsgSpoofUser
		} else {
			channelName := mmchannel.Name
			if mmchannel.TeamId != u.mc.Team.Id {
				channelName = u.mc.GetTeamName(mmchannel.TeamId) + "/" + mmchannel.Name
			}
			u.syncMMChannel(mmchannel.Id, channelName)
			ch := u.Srv.Channel(mmchannel.Id)
			spoof = ch.SpoofMessage
		}
		since := u.mc.GetLastViewedAt(mmchannel.Id)
		// ignore invalid/deleted/old channels
		if since == 0 {
			continue
		}
		// post everything to the channel you haven't seen yet
		postlist := u.mc.GetPostsSince(mmchannel.Id, since)
		if postlist == nil {
			// if the channel is not from the primary team id, we can't get posts
			if mmchannel.TeamId == u.mc.Team.Id {
				logger.Errorf("something wrong with getPostsSince for channel %s (%s)", mmchannel.Id, mmchannel.Name)
			}
			continue
		}
		var prevDate string

		// traverse the order in reverse
		for i := len(postlist.Order) - 1; i >= 0; i-- {
			p := postlist.Posts[postlist.Order[i]]
			if p.Type == model.POST_JOIN_LEAVE {
				continue
			}
			if p.DeleteAt > p.CreateAt {
				continue
			}
			ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))
			for _, post := range strings.Split(p.Message, "\n") {
				if user, ok := u.mc.Users[p.UserId]; ok {
					date := ts.Format("2006-01-02")
					if date != prevDate {
						spoof("matterircd", fmt.Sprintf("Replaying since %s", date))
						prevDate = date
					}
					spoof(user.Username, fmt.Sprintf("[%s] %s", ts.Format("15:04"), post))
				}
			}
		}
		u.mc.UpdateLastViewed(mmchannel.Id)
	}
}

func (u *User) handleWsMessage() {
	updateChannelsThrottle := time.Tick(time.Second * 60)

	for {
		if u.mc.WsQuit {
			logger.Debug("exiting handleWsMessage")
			return
		}
		logger.Debug("in handleWsMessage", len(u.mc.MessageChan))
		message := <-u.mc.MessageChan
		logger.Debugf("MMUser WsReceiver: %#v", message.Raw)
		// check if we have the users/channels in our cache. If not update
		u.checkWsActionMessage(message.Raw, updateChannelsThrottle)
		switch message.Raw.Event {
		case model.WEBSOCKET_EVENT_POSTED:
			u.handleWsActionPost(message.Raw)
		case model.WEBSOCKET_EVENT_POST_EDITED:
			u.handleWsActionPost(message.Raw)
		case model.WEBSOCKET_EVENT_USER_REMOVED:
			u.handleWsActionUserRemoved(message.Raw)
		case model.WEBSOCKET_EVENT_USER_ADDED:
			u.handleWsActionUserAdded(message.Raw)
		}
	}
}

func (u *User) handleWsActionPost(rmsg *model.WebSocketEvent) {
	var ch Channel
	data := model.PostFromJson(strings.NewReader(rmsg.Data["post"].(string)))
	props := rmsg.Data
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Data["post"].(string)))["props"].(map[string]interface{})
	logger.Debugf("handleWsActionPost() receiving userid %s", data.UserId)
	if rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED && data.HasReactions == true {
		logger.Debugf("edit post with reactions, do not relay. We don't know if a reaction is added or the post has been edited")
		return
	}
	if data.UserId == u.mc.User.Id {
		if _, ok := extraProps["matterircd_"+u.mc.User.Id].(bool); ok {
			logger.Debugf("message is sent from matterirc, not relaying %#v", data.Message)
			return
		}
		if data.Type == model.POST_JOIN_LEAVE {
			logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
			return
		}
	}
	if data.ParentId != "" {
		parentPost, resp := u.mc.Client.GetPost(data.ParentId, "")
		if resp.Error != nil {
			logger.Debugf("Unable to get parent post for", data)
		} else {
			parentGhost := u.createMMUser(u.mc.GetUser(parentPost.UserId))
			data.Message = fmt.Sprintf("%s (re @%s: %s)", data.Message, parentGhost.Nick, parentPost.Message)
		}
	}
	// create new "ghost" user
	ghost := u.createMMUser(u.mc.GetUser(data.UserId))
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == u.mc.User.Id {
		ghost = u
	}

	spoofUsername := data.UserId
	if ghost != nil {
		spoofUsername = ghost.Nick
	}

	// if we got attachments (eg slack attachments) and we have a fallback message, show this.
	if entries, ok := extraProps["attachments"].([]interface{}); ok {
		for _, entry := range entries {
			if f, ok := entry.(map[string]interface{}); ok {
				data.Message = data.Message + "\n" + f["fallback"].(string)
			}
		}
	}

	// check if we have a override_username (from webhooks) and use it
	overrideUsername, _ := extraProps["override_username"].(string)
	if overrideUsername != "" {
		// only allow valid irc nicks
		re := regexp.MustCompile("^[a-zA-Z0-9_]*$")
		if re.MatchString(overrideUsername) {
			spoofUsername = overrideUsername
		}
	}

	msgs := strings.Split(data.Message, "\n")
	// direct message
	if props["channel_type"] == "D" {
		// our own message, ignore because we can't handle/fake those on IRC
		if data.UserId == u.mc.User.Id {
			return
		}
	}

	if data.Type == model.POST_JOIN_LEAVE || data.Type == "system_leave_channel" || data.Type == "system_join_channel" {
		logger.Debugf("join/leave message. not relaying %#v", data.Message)
		ch = u.Srv.Channel(data.ChannelId)
		if ghost == nil {
			return
		}
		if !ch.HasUser(ghost) {
			// TODO use u.handleWsActionUserAdded()
			ch.Join(ghost)
		} else {
			// TODO use u.handleWsActionUserRemoved()
			//u.handleWsActionUserRemoved(data)
			ch.Part(ghost, "")
		}
		return
	}

	// not a private message so do channel stuff
	if props["channel_type"] != "D" && ghost != nil {
		ch = u.Srv.Channel(data.ChannelId)
		// join if not in channel
		if !ch.HasUser(ghost) {
			logger.Debugf("User %s is not in channel %s. Joining now", ghost.Nick, ch.String())
			//ch = u.Srv.Channel("&messages")
			ch.Join(ghost)
		}
		// excluded channel
		if stringInSlice(ch.String(), u.Cfg.JoinExclude) {
			logger.Debugf("channel %s is in JoinExclude, send to &messages", ch.String())
			ch = u.Srv.Channel("&messages")
		}
		// not in included channel
		if len(u.Cfg.JoinInclude) > 0 && !stringInSlice(ch.String(), u.Cfg.JoinInclude) {
			logger.Debugf("channel %s is not in JoinInclude, send to &messages", ch.String())
			ch = u.Srv.Channel("&messages")
		}
	}

	// add an edited string when messages are edited
	if len(msgs) > 0 && rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED {
		msgs[len(msgs)-1] = msgs[len(msgs)-1] + " (edited)"
	}
	// append channel name where messages are sent from
	if props["channel_type"] != "D" && ch.ID() == "&messages" {
		spoofUsername += "/" + u.Srv.Channel(data.ChannelId).String()
	}
	for _, m := range msgs {
		if m == "" {
			continue
		}
		if props["channel_type"] == "D" {
			u.MsgSpoofUser(spoofUsername, m)
			continue
		}
		if strings.Contains(data.Message, "@channel") || strings.Contains(data.Message, "@here") {
			ch.SpoofNotice(spoofUsername, m)
			continue
		}
		ch.SpoofMessage(spoofUsername, m)
	}

	if len(data.FileIds) > 0 {
		logger.Debugf("files detected")
		for _, fname := range u.mc.GetFileLinks(data.FileIds) {
			if props["channel_type"] == "D" {
				u.MsgSpoofUser(spoofUsername, "download file - "+fname)
			} else {
				ch.SpoofMessage(spoofUsername, "download file - "+fname)
			}
		}
	}
	logger.Debugf("handleWsActionPost() user %s sent %s", u.mc.GetUser(data.UserId).Username, data.Message)
	logger.Debugf("%#v", data)

	// updatelastviewed
	u.mc.UpdateLastViewed(data.ChannelId)
}

func (u *User) handleWsActionUserRemoved(rmsg *model.WebSocketEvent) {
	userId, ok := rmsg.Data["user_id"].(string)
	if !ok {
		return
	}
	ch := u.Srv.Channel(rmsg.Broadcast.ChannelId)

	// remove ourselves from the channel
	if userId == u.mc.User.Id {
		return
	}

	ghost := u.createMMUser(u.mc.GetUser(userId))
	if ghost == nil {
		logger.Debugf("couldn't remove user %s (%s)", userId, u.mc.GetUser(userId).Username)
		return
	}
	ch.Part(ghost, "")
}

func (u *User) handleWsActionUserAdded(rmsg *model.WebSocketEvent) {
	userId, ok := rmsg.Data["user_id"].(string)
	if !ok {
		return
	}

	// do not add ourselves to the channel
	if userId == u.mc.User.Id {
		logger.Debugf("ACTION_USER_ADDED not adding myself to %s (%s)", u.mc.GetChannelName(rmsg.Broadcast.ChannelId), rmsg.Broadcast.ChannelId)
		return
	}
	u.addUserToChannel(u.mc.GetUser(userId), "#"+u.mc.GetChannelName(rmsg.Broadcast.ChannelId), rmsg.Broadcast.ChannelId)
}

func (u *User) checkWsActionMessage(rmsg *model.WebSocketEvent, throttle <-chan time.Time) {
	if u.mc.GetChannelName(rmsg.Broadcast.ChannelId) == "" {
		select {
		case <-throttle:
			logger.Debugf("Updating channels for %#v", rmsg.Broadcast)
			go u.mc.UpdateChannels()
		default:
		}
	}
	if rmsg.Data == nil {
		return
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
	for len(msg) > 400 {
		u.Encode(&irc.Message{
			Prefix:   &irc.Prefix{Name: rcvuser, User: rcvuser, Host: rcvuser},
			Command:  irc.PRIVMSG,
			Params:   []string{u.Nick},
			Trailing: msg[:400] + "\n",
		})
		msg = msg[400:]
	}
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
	mmusers, resp := u.mc.Client.GetUsersInChannel(id, 0, 50000, "")
	if resp.Error != nil {
		return
	}
	for _, user := range mmusers {
		if user.Id != u.mc.User.Id {
			u.addUserToChannel(user, "#"+name, id)
		}
	}
	// before joining ourself
	for _, user := range mmusers {
		// join all the channels we're on on MM
		if user.Id == u.mc.User.Id {
			ch := srv.Channel(id)
			// only join when we're not yet on the channel
			if !ch.HasUser(u) {
				logger.Debugf("syncMMChannel adding myself to %s (id: %s)", name, id)
				if !stringInSlice(ch.String(), u.Cfg.JoinExclude) {
					if len(u.Cfg.JoinInclude) > 0 {
						if stringInSlice(ch.String(), u.Cfg.JoinInclude) {
							ch.Join(u)
						}
					} else {
						ch.Join(u)
					}
				}
				ch.Topic(u, u.mc.GetChannelHeader(id))
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

// antiIdle does a lastviewed every 60 seconds so that the user is shown as online instead of away
func (u *User) antiIdle(channelId string) {
	ticker := time.NewTicker(time.Second * 60)
	for {
		select {
		case <-u.idleStop:
			logger.Debug("stopping antiIdle loop")
			return
		case <-ticker.C:
			u.mc.UpdateLastViewed(channelId)
		}
	}
}
