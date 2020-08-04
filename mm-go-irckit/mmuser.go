package irckit

import (
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/42wim/matterbridge/matterclient"
	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/42wim/matterircd/config"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/model"
	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
)

type MmInfo struct {
	//MmGhostUser bool
	Srv         Server
	Credentials *MmCredentials
	Cfg         *mattermost.MmCfg
	mc          *matterclient.MMClient
	idleStop    chan struct{}
	br          bridge.Bridger
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
	PrefixMainTeam     bool
	PasteBufferTimeout int
	DisableAutoView    bool
	PreferNickname     bool
	HideReplies        bool
}

func NewUserBridge(c net.Conn, srv Server, cfg *mattermost.MmCfg) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})
	u.Srv = srv

	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")

	return u
}

func NewUserMM(c net.Conn, srv Server, cfg *mattermost.MmCfg) *User {
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
	u.MmInfo.Cfg.PrefixMainTeam = cfg.MattermostSettings.PrefixMainTeam
	u.MmInfo.Cfg.DisableAutoView = cfg.MattermostSettings.DisableAutoView
	u.MmInfo.Cfg.PreferNickname = cfg.MattermostSettings.PreferNickname
	u.MmInfo.Cfg.HideReplies = cfg.MattermostSettings.HideReplies

	u.idleStop = make(chan struct{})
	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	return u
}

func (u *User) handleEventChan(events chan *bridge.Event) {
	for event := range events {
		spew.Dump("receiving", event)
		switch e := event.Data.(type) {
		case *bridge.ChannelMessageEvent:
			u.handleChannelMessageEvent(e)
		case *bridge.DirectMessageEvent:
			u.handleDirectMessageEvent(e)
		case *bridge.ChannelTopicEvent:
			u.handleChannelTopicEvent(e)
		case *bridge.FileEvent:
			u.handleFileEvent(e)
		case *bridge.ChannelAddEvent:
			u.handleChannelAddEvent(e)
		case *bridge.ChannelRemoveEvent:
			u.handleChannelRemoveEvent(e)
		case *bridge.ChannelCreateEvent:
			u.handleChannelCreateEvent(e)
		case *bridge.ChannelDeleteEvent:
			u.handleChannelDeleteEvent(e)
		}
	}
}

func (u *User) handleChannelTopicEvent(event *bridge.ChannelTopicEvent) {
	tu, _ := u.Srv.HasUser(event.Sender)
	ch := u.Srv.Channel(event.ChannelID)
	ch.Topic(tu, event.Text)
}

func (u *User) handleDirectMessageEvent(event *bridge.DirectMessageEvent) {
	if event.Sender.Me {
		u.MsgSpoofUser(u, event.Receiver, event.Text)
	} else {
		u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver, event.Text)
	}
}

func (u *User) handleChannelAddEvent(event *bridge.ChannelAddEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, added := range event.Added {
		if added.Me {
			u.syncMMChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
			continue
		}

		ghost := u.createUserFromInfo(added)
		ch.Join(ghost)

		ch.SpoofMessage("system", "added "+added.Nick+" to the channel by "+event.Adder.Nick)
	}
}

func (u *User) handleChannelRemoveEvent(event *bridge.ChannelRemoveEvent) {
	spew.Dump(event)

	ch := u.Srv.Channel(event.ChannelID)

	for _, removed := range event.Removed {
		if removed.Me {
			ch.Part(u, "")
			continue
		}

		ghost := u.createUserFromInfo(removed)

		ch.Part(ghost, "")
		if event.Remover != nil {
			ch.SpoofMessage("system", "removed "+removed.Nick+" from the channel by "+event.Remover.Nick)
		} else {
			ch.SpoofMessage("system", "removed "+removed.Nick+" from the channel")
		}

	}
}

func (u *User) getMessageChannel(channelID, channelType string, sender *bridge.UserInfo) Channel {
	//event *bridge.ChannelMessageEvent) Channel {
	//ghost *User, props map[string]interface{}, data *model.Post) Channel {
	ch := u.Srv.Channel(channelID)
	// in an group
	if channelType == "G" {
		myself := u.createUserFromInfo(u.br.GetMe())
		if !ch.HasUser(myself) {
			ch.Join(myself)
			u.syncMMChannel(channelID, u.br.GetChannelName(channelID))
		}
	}
	ghost := u.createUserFromInfo(sender)
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

	return ch
}

func (u *User) handleChannelMessageEvent(event *bridge.ChannelMessageEvent) {
	/*
			      CHANNEL_OPEN                   = "O"
		        CHANNEL_PRIVATE                = "P"
		        CHANNEL_DIRECT                 = "D"
				CHANNEL_GROUP                  = "G"
	*/
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)
	if event.Sender.Me {
		event.Sender.Nick = u.Nick
	}

	switch event.MessageType {
	case "notice":
		ch.SpoofNotice(event.Sender.Nick, event.Text)
	default:
		ch.SpoofMessage(event.Sender.Nick, event.Text)
	}
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)

	switch event.ChannelType {
	case "D":
		for _, fname := range event.Files {
			if event.Sender.Me {
				u.MsgSpoofUser(u, event.Receiver, "download file -"+fname.Name)
			} else {
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver, "download file -"+fname.Name)
			}
		}
	default:
		for _, fname := range event.Files {
			if event.Sender.Me {
				ch.SpoofMessage(u.Nick, "download file -"+fname.Name)
			} else {
				ch.SpoofMessage(event.Sender.Nick, "download file -"+fname.Name)
			}
		}
	}
}

func (u *User) handleChannelCreateEvent(event *bridge.ChannelCreateEvent) {
	u.br.UpdateChannels()

	logger.Debugf("ACTION_CHANNEL_CREATED adding myself to %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

	u.syncMMChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
}

func (u *User) handleChannelDeleteEvent(event *bridge.ChannelDeleteEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	logger.Debugf("ACTION_CHANNEL_DELETED removing myself from %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

	ch.Part(u, "")
}

func (u *User) loginToMattermost() (*matterclient.MMClient, error) {
	cred := mattermost.Credentials{
		Login:  u.Credentials.Login,
		Pass:   u.Credentials.Pass,
		Team:   u.Credentials.Team,
		Server: u.Credentials.Server,
	}

	eventChan := make(chan *bridge.Event)
	br, mc, err := mattermost.New(u.MmInfo.Cfg, cred, eventChan)
	if err != nil {
		return nil, err
	}

	u.br = br

	go u.handleEventChan(eventChan)

	//go u.handleWsMessage()

	return mc, nil

}

func (u *User) logoutFromMattermost2() error {
	u.Srv.Logout(u)
	u.idleStop <- struct{}{}

	return nil
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

func (u *User) createService(nick string, what string) {
	service := &User{
		UserInfo: &bridge.UserInfo{
			Nick:  nick,
			User:  nick,
			Real:  what,
			Host:  "service",
			Ghost: true,
		},
		channels: map[Channel]struct{}{},
	}

	u.Srv.Add(service)
}

func (u *User) createUserFromInfo(info *bridge.UserInfo) *User {
	if ghost, ok := u.Srv.HasUser(info.Nick); ok {
		return ghost
	}

	ghost := &User{
		UserInfo: info,
		channels: map[Channel]struct{}{},
	}

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) addRealUserToChannel(ghost *User, channel string, channelId string) {
	if ghost == nil {
		return
	}

	if _, ok := u.Srv.HasUser(ghost.Nick); !ok {
		u.Srv.Add(ghost)
	}

	logger.Debugf("adding %s to %s", ghost.Nick, channel)

	ch := u.Srv.Channel(channelId)

	ch.Join(ghost)
}

func (u *User) addUserToChannel(ghost *User, channel string, channelId string) {
	if ghost == nil {
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

	for _, bruser := range u.br.GetUsers() {
		if bruser.Me {
			continue
		}

		ghost := u.createUserFromInfo(bruser)
		u.addUserToChannel(ghost, "&users", "&users")
	}

	ch.Join(u)

	// channel that receives messages from channels not joined on irc
	ch = srv.Channel("&messages")
	ch.Join(u)

	channels := make(chan *bridge.ChannelInfo, 5)
	for i := 0; i < 10; i++ {
		go u.addUserToChannelWorker(channels, throttle)
	}

	for _, brchannel := range u.br.GetChannels() {
		logger.Debugf("Adding channel %#v", brchannel)
		channels <- brchannel
	}

	close(channels)
}

func (u *User) createSpoof(mmchannel *bridge.ChannelInfo) func(string, string) {
	if strings.Contains(mmchannel.Name, "__") {
		userId := strings.Split(mmchannel.Name, "__")[0]
		u.createUserFromInfo(u.br.GetUser(userId))
		// wrap MsgSpoofser here
		return func(spoofUsername string, msg string) {
			u.MsgSpoofUser(u, spoofUsername, msg)
		}
	}

	channelName := mmchannel.Name

	if mmchannel.TeamID != u.mc.Team.Id || u.Cfg.PrefixMainTeam {
		channelName = u.mc.GetTeamName(mmchannel.TeamID) + "/" + mmchannel.Name
	}

	u.syncMMChannel(mmchannel.ID, channelName)
	ch := u.Srv.Channel(mmchannel.ID)

	return ch.SpoofMessage
}

func (u *User) addUserToChannelWorker(channels <-chan *bridge.ChannelInfo, throttle <-chan time.Time) {
	for brchannel := range channels {
		logger.Debug("addUserToChannelWorker", brchannel)

		<-throttle
		// exclude direct messages
		spoof := u.createSpoof(brchannel)

		since := u.mc.GetLastViewedAt(brchannel.ID)
		// ignore invalid/deleted/old channels
		if since == 0 {
			continue
		}
		// post everything to the channel you haven't seen yet
		postlist := u.mc.GetPostsSince(brchannel.ID, since)
		if postlist == nil {
			// if the channel is not from the primary team id, we can't get posts
			if brchannel.TeamID == u.mc.Team.Id {
				logger.Errorf("something wrong with getPostsSince for channel %s (%s)", brchannel.ID, brchannel.Name)
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

					nick := user.Username
					if u.Cfg.PreferNickname && isValidNick(user.Nickname) {
						nick = user.Nickname
					}

					spoof(nick, fmt.Sprintf("[%s] %s", ts.Format("15:04"), post))
				}
			}
		}

		if !u.Cfg.DisableAutoView {
			u.mc.UpdateLastViewed(brchannel.ID)
		}
	}
}

func (u *User) wsActionPostGetChannel(ghost *User, props map[string]interface{}, data *model.Post) Channel {
	ch := u.Srv.Channel(data.ChannelId)
	// in an group
	if props["channel_type"] == "G" {
		myself := u.createUserFromInfo(u.br.GetMe())
		if !ch.HasUser(myself) {
			ch.Join(myself)
			u.syncMMChannel(data.ChannelId, u.br.GetChannelName(data.ChannelId))
		}
	}
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

	return ch
}

func (u *User) checkWsActionMessage(rmsg *model.WebSocketEvent, throttle <-chan time.Time) {
	if u.br.GetChannelName(rmsg.Broadcast.ChannelId) != "" {
		return
	}

	select {
	case <-throttle:
		logger.Debugf("Updating channels for %#v", rmsg.Broadcast)
		go u.br.UpdateChannels()
	default:
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

func (u *User) MsgSpoofUser(sender *User, rcvuser string, msg string) {
	msg = wordwrap.String(msg, 440)
	lines := strings.Split(msg, "\n")

	for _, l := range lines {
		l = strings.TrimSpace(l)
		if len(l) == 0 {
			continue
		}

		u.Encode(&irc.Message{
			Prefix: &irc.Prefix{
				Name: sender.Nick,
				User: sender.Nick,
				Host: sender.Host,
			},
			Command:  irc.PRIVMSG,
			Params:   []string{rcvuser},
			Trailing: l + "\n",
		})
	}
}

// sync IRC with mattermost channel state
func (u *User) syncMMChannel(id string, name string) {

	users, err := u.br.GetChannelUsers(id)
	if err != nil {
		fmt.Println(err)
		return
	}

	srv := u.Srv

	for _, ghost := range users {
		if ghost.Me {
			continue
		}

		u.addRealUserToChannel(u.createUserFromInfo(ghost), "#"+name, id)
	}

	for _, ghost := range users {
		if !ghost.Me {
			continue
		}

		ch := srv.Channel(id)
		// only join when we're not yet on the channel
		if ch.HasUser(u) {
			break
		}

		logger.Debugf("syncMMChannel adding myself to %s (id: %s)", name, id)

		if stringInSlice(ch.String(), u.Cfg.JoinExclude) {
			continue
		}

		ch.Join(u)

		svc, _ := srv.HasUser("mattermost")

		ch.Topic(svc, u.br.Topic(ch.ID()))
	}
}

func (u *User) isValidMMServer(server string) bool {
	if len(u.Cfg.AllowedServers) == 0 {
		return true
	}

	logger.Debugf("allowedservers: %s", u.Cfg.AllowedServers)

	for _, srv := range u.Cfg.AllowedServers {
		if srv == server {
			return true
		}
	}

	return false
}
