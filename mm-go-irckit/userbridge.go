package irckit

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"encoding/gob"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/42wim/matterircd/bridge/slack"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
	"github.com/spf13/viper"
)

type UserBridge struct {
	Srv                Server
	Credentials        bridge.Credentials
	br                 bridge.Bridger            //nolint:structcheck
	inprogress         bool                      //nolint:structcheck
	lastViewedAt       map[string]int64          //nolint:structcheck
	lastViewedAtMutex  sync.RWMutex              //nolint:structcheck
	lastViewedAtSaved  int64                     //nolint:structcheck
	msgCounter         map[string]int            //nolint:structcheck
	msgLast            map[string][2]string      //nolint:structcheck
	msgLastMutex       sync.RWMutex              //nolint:structcheck
	msgMap             map[string]map[string]int //nolint:structcheck
	msgMapMutex        sync.RWMutex              //nolint:structcheck
	updateCounter      map[string]time.Time      //nolint:structcheck
	updateCounterMutex sync.Mutex                //nolint:structcheck
}

func NewUserBridge(c net.Conn, srv Server, cfg *viper.Viper) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})

	u.Srv = srv
	u.v = cfg
	u.lastViewedAt = make(map[string]int64)
	u.msgLast = make(map[string][2]string)
	u.msgMap = make(map[string]map[string]int)
	u.msgCounter = make(map[string]int)
	u.updateCounter = make(map[string]time.Time)

	statePath := u.v.GetString("mattermost.lastviewedsavefile")
	if statePath != "" {
		staleDuration := u.v.GetString("mattermost.lastviewedstaleduration")
		lastViewedAt, err := loadLastViewedState(statePath, staleDuration)
		if err == nil {
			logger.Info("Loaded lastViewedAt from ", time.Unix(lastViewedAt["__LastViewedStateSavedTime__"]/1000, 0))
			u.lastViewedAt = lastViewedAt
		} else {
			logger.Warning("Unable to load saved lastViewedAt, using empty values: ", err)
		}
		u.lastViewedAtSaved = model.GetMillis()
	}

	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	u.createService("matterircd", "systemservice")
	return u
}

func (u *User) handleEventChan(events chan *bridge.Event) {
	for event := range events {
		logger.Tracef("eventchan %s", spew.Sdump(event))
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
		case *bridge.UserUpdateEvent:
			u.handleUserUpdateEvent(e)
		case *bridge.StatusChangeEvent:
			u.handleStatusChangeEvent(e)
		case *bridge.ReactionAddEvent, *bridge.ReactionRemoveEvent:
			u.handleReactionEvent(e)
		case *bridge.LogoutEvent:
			return
		}
	}
}

func (u *User) handleChannelTopicEvent(event *bridge.ChannelTopicEvent) {
	tu, ok := u.Srv.HasUserID(event.UserID)
	if event.UserID == u.User {
		ok = true
		tu = u
	}

	if ok {
		ch := u.Srv.Channel(event.ChannelID)
		ch.Topic(tu, event.Text)

		return
	}

	logger.Errorf("topic change failure: userID %s not found", event.UserID)
}

func (u *User) handleDirectMessageEvent(event *bridge.DirectMessageEvent) {
	if u.v.GetBool(u.br.Protocol() + ".showmentions") {
		for _, m := range u.MentionKeys {
			if m == u.Nick {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	if u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext") {
		prefixUser := event.Sender.User

		if event.Sender.Me {
			prefixUser = event.Receiver.User
		}

		prefix := u.prefixContext(prefixUser, event.MessageID, event.ParentID, event.Event)

		switch {
		case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
			event.Text = strings.Replace(event.Text, "\x01ACTION ", "\x01ACTION "+prefix+" ", 1)
			event.Text = prefix + " " + event.Text
		case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
			event.Text = strings.Replace(event.Text, " \x01", " "+prefix+" \x01", 1)
			event.Text = event.Text + " " + prefix
		}
	}

	if event.Sender.Me {
		if event.Receiver.Me {
			u.MsgSpoofUser(u, u.Nick, event.Text)
		} else {
			u.MsgSpoofUser(u, event.Receiver.Nick, event.Text)
		}
	} else {
		u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver.Nick, event.Text)
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.lastViewedAtMutex.Lock()
	defer u.lastViewedAtMutex.Unlock()
	u.lastViewedAt[event.ChannelID] = model.GetMillis()
	statePath := u.v.GetString(u.br.Protocol() + ".lastviewedsavefile")
	if statePath == "" {
		return
	}
	// We only want to save or dump out saved lastViewedAt on new
	// messages after X time (default 5mins).
	saveInterval := int64(300000)
	val, err := time.ParseDuration(u.v.GetString(u.br.Protocol() + ".lastviewedsaveinterval"))
	if err == nil {
		saveInterval = val.Milliseconds()
	}
	if u.lastViewedAtSaved < (model.GetMillis() - saveInterval) {
		saveLastViewedState(statePath, u.lastViewedAt)
		u.lastViewedAtSaved = model.GetMillis()
	}
}

func (u *User) handleChannelAddEvent(event *bridge.ChannelAddEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, added := range event.Added {
		if added.Me {
			u.syncChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
			continue
		}

		ghost := u.createUserFromInfo(added)

		ch.Join(ghost)

		if event.Adder != nil && added.Nick != event.Adder.Nick && event.Adder.Nick != "system" {
			ch.SpoofMessage("system", "added "+added.Nick+" to the channel by "+event.Adder.Nick)
		}
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.lastViewedAtMutex.Lock()
	defer u.lastViewedAtMutex.Unlock()
	u.lastViewedAt[event.ChannelID] = model.GetMillis()
	statePath := u.v.GetString(u.br.Protocol() + ".lastviewedsavefile")
	if statePath == "" {
		return
	}
	// We only want to save or dump out saved lastViewedAt on new
	// messages after X time (default 5mins).
	saveInterval := int64(300000)
	val, err := time.ParseDuration(u.v.GetString(u.br.Protocol() + ".lastviewedsaveinterval"))
	if err == nil {
		saveInterval = val.Milliseconds()
	}
	if u.lastViewedAtSaved < (model.GetMillis() - saveInterval) {
		saveLastViewedState(statePath, u.lastViewedAt)
		u.lastViewedAtSaved = model.GetMillis()
	}
}

func (u *User) handleChannelRemoveEvent(event *bridge.ChannelRemoveEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	for _, removed := range event.Removed {
		if removed.Me {
			ch.Part(u, "")
			continue
		}

		ghost := u.createUserFromInfo(removed)

		ch.Part(ghost, "")

		if event.Remover != nil && removed.Nick != event.Remover.Nick && event.Remover.Nick != "system" {
			ch.SpoofMessage("system", "removed "+removed.Nick+" from the channel by "+event.Remover.Nick)
		}
	}
	u.lastViewedAtMutex.Lock()
	defer u.lastViewedAtMutex.Unlock()
	u.lastViewedAt[event.ChannelID] = model.GetMillis()
	statePath := u.v.GetString(u.br.Protocol() + ".lastviewedsavefile")
	if statePath == "" {
		return
	}
	// We only want to save or dump out saved lastViewedAt on new
	// messages after X time (default 5mins).
	saveInterval := int64(300000)
	val, err := time.ParseDuration(u.v.GetString(u.br.Protocol() + ".lastviewedsaveinterval"))
	if err == nil {
		saveInterval = val.Milliseconds()
	}
	if u.lastViewedAtSaved < (model.GetMillis() - saveInterval) {
		saveLastViewedState(statePath, u.lastViewedAt)
		u.lastViewedAtSaved = model.GetMillis()
	}
}

func (u *User) getMessageChannel(channelID, channelType string, sender *bridge.UserInfo) Channel {
	ch := u.Srv.Channel(channelID)
	ghost := u.createUserFromInfo(sender)

	// if it's another user, let them join
	if !ghost.Me && !ch.HasUser(ghost) {
		logger.Debugf("User %s is not in channel %s. Joining now", ghost.Nick, ch.String())
		ch.Join(ghost)
	}

	// check if we mayjoin this channel
	if u.mayJoin(channelID) {
		// if we are on it, just return it
		if ch.HasUser(u) {
			return ch
		}

		// otherwise first sync it
		u.syncChannel(channelID, u.br.GetChannelName(channelID))

		return ch
	}

	return u.Srv.Channel("&messages")
}

func (u *User) handleChannelMessageEvent(event *bridge.ChannelMessageEvent) {
	/*
			      CHANNEL_OPEN                   = "O"
		        CHANNEL_PRIVATE                = "P"
		        CHANNEL_DIRECT                 = "D"
				CHANNEL_GROUP                  = "G"
	*/
	nick := event.Sender.Nick
	logger.Debug("in handleChannelMessageEvent")
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)
	if event.Sender.Me {
		nick = u.Nick
	}

	if event.ChannelType != "D" && ch.ID() == "&messages" {
		nick += "/" + u.Srv.Channel(event.ChannelID).String()
	}

	if u.v.GetBool(u.br.Protocol() + ".showmentions") {
		for _, m := range u.MentionKeys {
			if m == u.Nick {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	if u.v.GetBool(u.br.Protocol() + ".prefixcontext") {
		prefix := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, event.Event)

		if strings.HasPrefix(event.Text, "\x01") {
			event.Text = strings.Replace(event.Text, "\x01ACTION ", "\x01ACTION "+prefix+" ", 1)
		} else {
			event.Text = prefix + " " + event.Text
		}
	} else if u.v.GetBool(u.br.Protocol() + ".suffixcontext") {
		prefix := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, event.Event)

		if strings.HasSuffix(event.Text, "\x01") {
			event.Text = strings.Replace(event.Text, " \x01", " "+prefix+" \x01", 1)
		} else {
			event.Text = event.Text + " " + prefix
		}
	}

	switch event.MessageType {
	case "notice":
		ch.SpoofNotice(nick, event.Text)
	default:
		ch.SpoofMessage(nick, event.Text)
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.lastViewedAtMutex.Lock()
	defer u.lastViewedAtMutex.Unlock()
	u.lastViewedAt[event.ChannelID] = model.GetMillis()
	statePath := u.v.GetString(u.br.Protocol() + ".lastviewedsavefile")
	if statePath == "" {
		return
	}
	// We only want to save or dump out saved lastViewedAt on new
	// messages after X time (default 5mins).
	saveInterval := int64(300000)
	val, err := time.ParseDuration(u.v.GetString(u.br.Protocol() + ".lastviewedsaveinterval"))
	if err == nil {
		saveInterval = val.Milliseconds()
	}
	if u.lastViewedAtSaved < (model.GetMillis() - saveInterval) {
		saveLastViewedState(statePath, u.lastViewedAt)
		u.lastViewedAtSaved = model.GetMillis()
	}
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	ch := u.getMessageChannel(event.ChannelID, event.ChannelType, event.Sender)

	switch event.ChannelType {
	case "D":
		for _, fname := range event.Files {
			if event.Sender.Me {
				if event.Receiver.Me {
					u.MsgSpoofUser(u, u.Nick, "download file -"+fname.Name)
				} else {
					u.MsgSpoofUser(u, event.Receiver.Nick, "download file -"+fname.Name)
				}
			} else {
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver.Nick, "download file -"+fname.Name)
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

	u.syncChannel(event.ChannelID, u.br.GetChannelName(event.ChannelID))
}

func (u *User) handleChannelDeleteEvent(event *bridge.ChannelDeleteEvent) {
	ch := u.Srv.Channel(event.ChannelID)

	logger.Debugf("ACTION_CHANNEL_DELETED removing myself from %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

	ch.Part(u, "")
}

func (u *User) handleUserUpdateEvent(event *bridge.UserUpdateEvent) {
	u.updateUserFromInfo(event.User)
}

func (u *User) handleStatusChangeEvent(event *bridge.StatusChangeEvent) {
	if event.UserID == u.br.GetMe().User {
		switch event.Status {
		case "online":
			logger.Debug("setting myself online")
			u.Srv.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away")
		default:
			logger.Debug("setting myself away")
			u.Srv.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
		}
	}
}

func (u *User) handleReactionEvent(event interface{}) {
	var (
		text, channelID, messageID, channelType, reaction string
		sender                                            *bridge.UserInfo
	)

	switch e := event.(type) {
	case *bridge.ReactionAddEvent:
		text = "added reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
	case *bridge.ReactionRemoveEvent:
		text = "removed reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
	}

	if channelType == "D" {
		e := &bridge.DirectMessageEvent{
			Text:      text + reaction,
			ChannelID: channelID,
			Receiver:  u.UserInfo,
			Sender:    sender,
			MessageID: messageID,
			Event:     "reaction",
			ParentID:  messageID,
		}

		u.handleDirectMessageEvent(e)

		return
	}

	e := &bridge.ChannelMessageEvent{
		Text:        text + reaction,
		ChannelID:   channelID,
		ChannelType: channelType,
		Sender:      sender,
		MessageID:   messageID,
		Event:       "reaction",
		ParentID:    messageID,
	}

	u.handleChannelMessageEvent(e)
}

func (u *User) CreateUserFromInfo(info *bridge.UserInfo) *User {
	return u.createUserFromInfo(info)
}

func (u *User) CreateUsersFromInfo(info []*bridge.UserInfo) []*User {
	var users []*User

	for _, userinfo := range info {
		if userinfo.Me {
			continue
		}

		userinfo := userinfo
		ghost := NewUser(u.Conn)
		ghost.UserInfo = userinfo
		users = append(users, ghost)
	}

	return users
}

func (u *User) updateUserFromInfo(info *bridge.UserInfo) *User {
	if ghost, ok := u.Srv.HasUserID(info.User); ok {
		if ghost.Nick != info.Nick {
			changeMsg := &irc.Message{
				Prefix:  ghost.Prefix(),
				Command: irc.NICK,
				Params:  []string{info.Nick},
			}
			u.Encode(changeMsg)
		}

		ghost.UserInfo = info

		return ghost
	}

	ghost := NewUser(u.Conn)
	ghost.UserInfo = info

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) createUserFromInfo(info *bridge.UserInfo) *User {
	if ghost, ok := u.Srv.HasUserID(info.User); ok {
		return ghost
	}

	ghost := NewUser(u.Conn)
	ghost.UserInfo = info

	u.Srv.Add(ghost)

	return ghost
}

func (u *User) addUsersToChannel(users []*User, channel string, channelID string) {
	logger.Debugf("adding %d to %s", len(users), channel)

	ch := u.Srv.Channel(channelID)

	ch.BatchJoin(users)
}

func (u *User) addUsersToChannels() {
	// wait until the bridge is ready
	for u.br == nil {
		logger.Debug("bridge not ready yet, sleeping")
		time.Sleep(time.Millisecond * 500)
	}

	srv := u.Srv
	throttle := time.NewTicker(time.Millisecond * 200)

	logger.Debug("in addUsersToChannels()")
	// add all users, also who are not on channels
	ch := srv.Channel("&users")

	// create and join the users
	users := u.CreateUsersFromInfo(u.br.GetUsers())
	srv.BatchAdd(users)
	u.addUsersToChannel(users, "&users", "&users")

	// join ourself
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

		// only joindm when specified
		if brchannel.DM && !u.v.GetBool(u.br.Protocol()+".joindm") {
			logger.Debugf("Skipping IM channel %s", brchannel.Name)

			continue
		}

		channels <- brchannel
	}

	close(channels)
}

func (u *User) createSpoof(mmchannel *bridge.ChannelInfo) func(string, string) {
	if strings.Contains(mmchannel.Name, "__") {
		return func(nick string, msg string) {
			if usr, ok := u.Srv.HasUser(nick); ok {
				u.MsgSpoofUser(usr, u.Nick, msg)
			} else {
				logger.Errorf("%s not found for replay msg", nick)
			}
		}
	}

	channelName := mmchannel.Name

	if mmchannel.TeamID != u.br.GetMe().TeamID || u.v.GetBool(u.br.Protocol()+".prefixmainteam") {
		channelName = u.br.GetTeamName(mmchannel.TeamID) + "/" + mmchannel.Name
	}

	u.syncChannel(mmchannel.ID, "#"+channelName)
	ch := u.Srv.Channel(mmchannel.ID)

	return ch.SpoofMessage
}

func (u *User) addUserToChannelWorker(channels <-chan *bridge.ChannelInfo, throttle *time.Ticker) {
	for brchannel := range channels {
		logger.Debug("addUserToChannelWorker", brchannel)

		<-throttle.C
		// exclude direct messages
		spoof := u.createSpoof(brchannel)

		since := u.br.GetLastViewedAt(brchannel.ID)
		// ignore invalid/deleted/old channels
		if since == 0 {
			continue
		}
		// We used to stored last viewed at if present.
		u.lastViewedAtMutex.RLock()
		if lastViewedAt, ok := u.lastViewedAt[brchannel.ID]; ok {
			since = lastViewedAt
		}
		u.lastViewedAtMutex.RUnlock()
		// post everything to the channel you haven't seen yet
		postlist := u.br.GetPostsSince(brchannel.ID, since)
		if postlist == nil {
			// if the channel is not from the primary team id, we can't get posts
			if brchannel.TeamID == u.br.GetMe().TeamID {
				logger.Errorf("something wrong with getPostsSince for channel %s (%s)", brchannel.ID, brchannel.Name)
			}
			continue
		}

		showReplayHdr := true

		mmPostList := postlist.(*model.PostList)
		if mmPostList == nil {
			continue
		}
		// traverse the order in reverse
		for i := len(mmPostList.Order) - 1; i >= 0; i-- {
			p := mmPostList.Posts[mmPostList.Order[i]]
			if p.Type == model.POST_JOIN_LEAVE {
				continue
			}

			if p.DeleteAt > p.CreateAt {
				continue
			}

			ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

			props := p.GetProps()
			botname, override := props["override_username"].(string)

			for _, post := range strings.Split(p.Message, "\n") {
				user := u.br.GetUser(p.UserId)
				nick := user.Nick
				if override {
					nick = botname
				}
				if showReplayHdr {
					date := ts.Format("2006-01-02 15:04:05")
					if brchannel.DM {
						spoof(nick, fmt.Sprintf("Replaying since %s", date))
					} else {
						spoof("matterircd", fmt.Sprintf("Replaying since %s", date))
					}
					showReplayHdr = false
				}

				replayMsg := fmt.Sprintf("[%s] %s", ts.Format("15:04"), post)
				if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" {
					threadMsg := fmt.Sprintf("@@%s", p.Id)
					if p.ParentId != "" {
						if u.v.GetBool(u.br.Protocol() + ".unicode") {
							threadMsg = fmt.Sprintf("↪@@%s", p.ParentId)
						} else {
							threadMsg = fmt.Sprintf("->@@%s", p.ParentId)
						}
					}

					switch {
					case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
						replayMsg = fmt.Sprintf("[%s] [%s] %s", ts.Format("15:04"), threadMsg, post)
					case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
						replayMsg = fmt.Sprintf("[%s] %s [%s]", ts.Format("15:04"), post, threadMsg)
					}
				}
				spoof(nick, replayMsg)
			}
		}

		if len(mmPostList.Order) > 0 {
			if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
				u.updateLastViewed(brchannel.ID)
			}
			u.lastViewedAtMutex.Lock()
			u.lastViewedAt[brchannel.ID] = model.GetMillis()
			u.lastViewedAtMutex.Unlock()
		}
	}
	u.lastViewedAtMutex.Lock()
	defer u.lastViewedAtMutex.Unlock()
	statePath := u.v.GetString(u.br.Protocol() + ".lastviewedsavefile")
	if statePath == "" {
		return
	}
	// We only want to save or dump out saved lastViewedAt on new
	// messages after X time (default 5mins).
	saveInterval := int64(300000)
	val, err := time.ParseDuration(u.v.GetString(u.br.Protocol() + ".lastviewedsaveinterval"))
	if err == nil {
		saveInterval = val.Milliseconds()
	}
	if u.lastViewedAtSaved < (model.GetMillis() - saveInterval) {
		saveLastViewedState(statePath, u.lastViewedAt)
		u.lastViewedAtSaved = model.GetMillis()
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

func (u *User) syncChannel(id string, name string) {
	users, err := u.br.GetChannelUsers(id)
	if err != nil {
		fmt.Println(err)
		return
	}

	srv := u.Srv

	// create and join the users
	batchUsers := u.CreateUsersFromInfo(users)
	srv.BatchAdd(batchUsers)
	u.addUsersToChannel(batchUsers, "&users", "&users")
	u.addUsersToChannel(batchUsers, name, id)

	// add myself
	ch := srv.Channel(id)
	if !ch.HasUser(u) && u.mayJoin(id) {
		logger.Debugf("syncChannel adding myself to %s (id: %s)", name, id)
		ch.Join(u)
		svc, _ := srv.HasUser(u.br.Protocol())
		ch.Topic(svc, u.br.Topic(ch.ID()))
	}
}

func (u *User) mayJoin(channelID string) bool {
	ch := u.Srv.Channel(channelID)

	jo := u.v.GetStringSlice(u.br.Protocol() + ".joinonly")
	ji := u.v.GetStringSlice(u.br.Protocol() + ".joininclude")
	je := u.v.GetStringSlice(u.br.Protocol() + ".joinexclude")

	switch {
	// if we have joinonly channels specified we are only allowed to join those
	case len(jo) != 0 && !stringInRegexp(ch.String(), jo):
		logger.Tracef("mayjoin 0 %t ch: %s, match: %s", false, ch.String(), jo)
		return false
	// we only have exclude, do not join if in exclude
	case len(ji) == 0 && len(je) != 0:
		mayjoin := !stringInRegexp(ch.String(), je)
		logger.Tracef("mayjoin 1 %t ch: %s, match: %s", mayjoin, ch.String(), je)
		return mayjoin
	// nothing specified, everything may join
	case len(ji) == 0 && len(je) == 0:
		logger.Tracef("mayjoin 2 %t ch: %s, both empty", true, ch.String())
		return true
	// if we don't have joinexclude, then joininclude behaves as joinonly
	case len(ji) != 0 && len(je) == 0:
		mayjoin := stringInRegexp(ch.String(), ji)
		logger.Tracef("mayjoin 3 %t ch: %s, match: %s", mayjoin, ch.String(), ji)
		return mayjoin
	// joininclude overrides the joinexclude
	case len(ji) != 0 && len(je) != 0:
		// if explicit in ji we also may join
		mayjoin := stringInRegexp(ch.String(), ji)
		logger.Tracef("mayjoin 4 %t ch: %s, match: %s", mayjoin, ch.String(), ji)
		return mayjoin
	}

	logger.Tracef("mayjoin default %t ch: %s, ji: %s, je: %s", false, ch.String(), ji, je)

	return false
}

func (u *User) isValidServer(server, protocol string) bool {
	if len(u.v.GetStringSlice(protocol+".restrict")) == 0 {
		return true
	}

	logger.Debugf("restrict: %s", u.v.GetStringSlice(protocol+".restrict"))

	for _, srv := range u.v.GetStringSlice(protocol + ".restrict") {
		if srv == server {
			return true
		}
	}

	return false
}

func (u *User) loginTo(protocol string) error {
	var err error

	eventChan := make(chan *bridge.Event)

	switch protocol {
	case "slack":
		u.br, err = slack.New(u.v, u.Credentials, eventChan, u.addUsersToChannels)
	case "mattermost":
		u.br, _, err = mattermost.New(u.v, u.Credentials, eventChan, u.addUsersToChannels)
	}

	if err != nil {
		return err
	}

	status, _ := u.br.StatusUser(u.br.GetMe().User)
	if status == "away" {
		u.Srv.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away")
	}

	info := u.br.GetMe()
	u.Me = true
	u.User = info.User
	u.MentionKeys = info.MentionKeys

	go u.handleEventChan(eventChan)

	return nil
}

// nolint:unparam,unused
func (u *User) logoutFrom(protocol string) error {
	logger.Debug("logging out from", protocol)

	u.Srv.Logout(u)
	return nil
}

func (u *User) increaseMsgCounter(channelID string) int {
	u.msgCounter[channelID]++

	// max 4096 entries
	if u.msgCounter[channelID] == 4095 {
		u.msgCounter[channelID] = 0
	}

	return u.msgCounter[channelID]
}

func (u *User) prefixContextModified(channelID, messageID string) string {
	var (
		ok           bool
		currentcount int
	)

	if _, ok = u.msgMap[channelID]; !ok {
		u.msgMap[channelID] = make(map[string]int)
	}

	// check if we already have a counter for this messageID otherwise
	// increase counter and create it
	if currentcount, ok = u.msgMap[channelID][messageID]; !ok {
		currentcount = u.increaseMsgCounter(channelID)
	}

	return fmt.Sprintf("[%03x]", currentcount)
}

func (u *User) prefixContext(channelID, messageID, parentID, event string) string {
	if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" {
		if parentID == "" {
			return fmt.Sprintf("[@@%s]", messageID)
		}
		if u.v.GetBool(u.br.Protocol() + ".unicode") {
			return fmt.Sprintf("[↪@@%s]", parentID)
		}
		return fmt.Sprintf("[->@@%s]", parentID)
	}

	u.msgMapMutex.Lock()
	defer u.msgMapMutex.Unlock()

	if event == "post_edited" || event == "post_deleted" || event == "reaction" {
		return u.prefixContextModified(channelID, messageID)
	}

	var (
		currentcount, parentcount int
		ok                        bool
	)

	if parentID != "" {
		if _, ok = u.msgMap[channelID]; !ok {
			u.msgMap[channelID] = make(map[string]int)
		}

		if _, ok = u.msgMap[channelID][parentID]; !ok {
			u.increaseMsgCounter(channelID)
			u.msgMap[channelID][parentID] = u.msgCounter[channelID]
		}

		parentcount = u.msgMap[channelID][parentID]
	}

	currentcount = u.increaseMsgCounter(channelID)

	if _, ok = u.msgMap[channelID]; !ok {
		u.msgMap[channelID] = make(map[string]int)
	}

	u.msgMap[channelID][messageID] = u.msgCounter[channelID]

	if parentID != "" {
		return fmt.Sprintf("[%03x->%03x]", currentcount, parentcount)
	}

	return fmt.Sprintf("[%03x]", currentcount)
}

func (u *User) updateLastViewed(channelID string) {
	u.updateCounterMutex.Lock()
	defer u.updateCounterMutex.Unlock()
	if t, ok := u.updateCounter[channelID]; ok {
		if time.Since(t) < time.Second*5 {
			return
		}
	}

	u.updateCounter[channelID] = time.Now()

	go func() {
		rand.Seed(time.Now().UnixNano())
		r := rand.Intn(3000)
		time.Sleep(time.Duration(r) * time.Millisecond)
		u.br.UpdateLastViewed(channelID)
	}()
}

const lastViewedStateFormat = int64(1)

// Default 30 days
const defaultStaleDuration = int64(86400 * 30 * 1000)

func saveLastViewedState(statePath string, lastViewedAt map[string]int64) error {
	f, err := os.Create(statePath)
	if err != nil {
		logger.Warning("Unable to save lastViewedAt: ", err)
		return err
	}
	defer f.Close()

	currentTime := model.GetMillis()

	lastViewedAt["__LastViewedStateFormat__"] = lastViewedStateFormat
	if _, ok := lastViewedAt["__LastViewedStateCreateTime__"]; !ok {
		lastViewedAt["__LastViewedStateCreateTime__"] = currentTime
	}
	lastViewedAt["__LastViewedStateSavedTime__"] = currentTime
	// Simple checksum
	lastViewedAt["__LastViewedStateChecksum__"] = lastViewedAt["__LastViewedStateCreateTime__"] ^ currentTime

	logger.Debug("Saving lastViewedAt")
	
	if err := gob.NewEncoder(f).Encode(lastViewedAt); err != nil {
		return fmt.Errorf("gob encoding failed: %s",err)
	}
	
	return nil
}

func loadLastViewedState(statePath string, staleDuration string) (map[string]int64, error) {
	f, err := os.Open(statePath)
	if err != nil {
		logger.Debug("Unable to load lastViewedAt: ", err)
		return nil, err
	}
	defer f.Close()

	var lastViewedAt map[string]int64
	err = gob.NewDecoder(f).Decode(&lastViewedAt)
	if err != nil {
		logger.Debug("Unable to load lastViewedAt: ", err)
		return nil, err
	}

	if lastViewedAt["__LastViewedStateFormat__"] != lastViewedStateFormat {
		logger.Debug("State format version mismatch: ", lastViewedAt["__LastViewedStateFormat__"], " vs. ", lastViewedStateFormat)
		return nil, errors.New("version mismatch")
	}
	checksum := lastViewedAt["__LastViewedStateChecksum__"]
	createtime := lastViewedAt["__LastViewedStateCreateTime__"]
	savedtime := lastViewedAt["__LastViewedStateSavedTime__"]
	if createtime^savedtime != checksum {
		logger.Debug("Checksum mismatch: (saved checksum, state file creation, last saved time)", checksum, createtime, savedtime)
		return nil, errors.New("checksum mismatch")
	}

	currentTime := model.GetMillis()

	// Check if stale, time last saved older than defined
	// (default 30 days).
	stale := defaultStaleDuration
	val, err := time.ParseDuration(staleDuration)
	if err != nil {
		stale = val.Milliseconds()
		return fmt.Errorf("incorrect lastviewedstaleduration: %s",err)
	}
	
	stale = val.Milliseconds()
	lastSaved, ok := lastViewedAt["__LastViewedStateSavedTime__"]
	if !ok || (lastSaved > 0 && lastSaved < currentTime-stale) {
		logger.Debug("File stale? Last saved too old: ", time.Unix(lastViewedAt["__LastViewedStateSavedTime__"]/1000, 0))
		return nil, errors.New("stale lastViewedAt state file")
	}

	return lastViewedAt, nil
}
