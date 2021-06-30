package irckit

import (
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/42wim/matterircd/bridge/slack"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/muesli/reflow/wordwrap"
	"github.com/sorcix/irc"
	"github.com/spf13/viper"
)

const systemUser = "system"

type UserBridge struct {
	Srv         Server
	Credentials bridge.Credentials
	br          bridge.Bridger     //nolint:structcheck
	inprogress  bool               //nolint:structcheck
	eventChan   chan *bridge.Event //nolint:structcheck

	lastViewedAtDB *bolt.DB       //nolint:structcheck
	msgCounter     map[string]int //nolint:structcheck

	msgLastMutex sync.RWMutex         //nolint:structcheck
	msgLast      map[string][2]string //nolint:structcheck

	msgMapMutex sync.RWMutex              //nolint:structcheck
	msgMap      map[string]map[string]int //nolint:structcheck

	updateCounterMutex sync.Mutex           //nolint:structcheck
	updateCounter      map[string]time.Time //nolint:structcheck
}

func NewUserBridge(c net.Conn, srv Server, cfg *viper.Viper, db *bolt.DB) *User {
	u := NewUser(&conn{
		Conn:    c,
		Encoder: irc.NewEncoder(c),
		Decoder: irc.NewDecoder(c),
	})

	u.Srv = srv
	u.v = cfg
	u.lastViewedAtDB = db
	u.msgLast = make(map[string][2]string)
	u.msgMap = make(map[string]map[string]int)
	u.msgCounter = make(map[string]int)
	u.updateCounter = make(map[string]time.Time)
	u.eventChan = make(chan *bridge.Event, 1000)

	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	u.createService("matterircd", "systemservice")
	return u
}

func (u *User) handleEventChan() {
	for event := range u.eventChan {
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

		u.saveLastViewedAt(event.ChannelID)
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
		case u.v.GetBool(u.br.Protocol()+".prefixcontext") && strings.HasPrefix(event.Text, "\x01"):
			event.Text = strings.Replace(event.Text, "\x01ACTION ", "\x01ACTION "+prefix+" ", 1)
		case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
			event.Text = prefix + " " + event.Text
		case u.v.GetBool(u.br.Protocol()+".suffixcontext") && strings.HasSuffix(event.Text, "\x01"):
			event.Text = strings.Replace(event.Text, " \x01", " "+prefix+" \x01", 1)
		case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
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
		u.MsgSpoofUser(u.createUserFromInfo(event.Sender), u.Nick, event.Text)
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
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

		if event.Adder != nil && added.Nick != event.Adder.Nick && event.Adder.Nick != systemUser {
			ch.SpoofMessage(systemUser, "added "+added.Nick+" to the channel by "+event.Adder.Nick)
		}
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
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

		if event.Remover != nil && removed.Nick != event.Remover.Nick && event.Remover.Nick != systemUser {
			ch.SpoofMessage(systemUser, "removed "+removed.Nick+" from the channel by "+event.Remover.Nick)
		}
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) getMessageChannel(channelID string, sender *bridge.UserInfo) Channel {
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
	nick := sanitizeNick(event.Sender.Nick)
	logger.Debug("in handleChannelMessageEvent")
	ch := u.getMessageChannel(event.ChannelID, event.Sender)
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

	if (u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext")) && u.Nick != systemUser {
		prefix := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, event.Event)
		switch {
		case u.v.GetBool(u.br.Protocol()+".prefixcontext") && strings.HasPrefix(event.Text, "\x01"):
			event.Text = strings.Replace(event.Text, "\x01ACTION ", "\x01ACTION "+prefix+" ", 1)
		case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
			event.Text = prefix + " " + event.Text
		case u.v.GetBool(u.br.Protocol()+".suffixcontext") && strings.HasSuffix(event.Text, "\x01"):
			event.Text = strings.Replace(event.Text, " \x01", " "+prefix+" \x01", 1)
		case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
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
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	for _, fname := range event.Files {
		fileMsg := "download file - " + fname.Name
		if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" {
			threadMsgID := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, "")
			fileMsg = u.formatContextMessage("", threadMsgID, fileMsg)
		}

		switch event.ChannelType {
		case "D":
			if event.Sender.Me {
				if event.Receiver.Me {
					u.MsgSpoofUser(u, u.Nick, fileMsg)
				} else {
					u.MsgSpoofUser(u, event.Receiver.Nick, fileMsg)
				}
			} else {
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), event.Receiver.Nick, fileMsg)
			}
		default:
			ch := u.getMessageChannel(event.ChannelID, event.Sender)
			if event.Sender.Me {
				ch.SpoofMessage(u.Nick, fileMsg)
			} else {
				ch.SpoofMessage(event.Sender.Nick, fileMsg)
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

	message := ""

	switch e := event.(type) {
	case *bridge.ReactionAddEvent:
		if !u.v.GetBool(u.br.Protocol() + ".hidereplies") {
			nick := "(none)"
			if e.ParentUser != nil {
				nick = sanitizeNick(e.ParentUser.Nick)
			}
			message = fmt.Sprintf(" (re @%s: %s)", nick, e.Message)
		}
		text = "added reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
	case *bridge.ReactionRemoveEvent:
		if !u.v.GetBool(u.br.Protocol() + ".hidereplies") {
			nick := "(none)"
			if e.ParentUser != nil {
				nick = sanitizeNick(e.ParentUser.Nick)
			}
			message = fmt.Sprintf(" (re @%s: %s)", nick, e.Message)
		}
		text = "removed reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
	}

	defer u.saveLastViewedAt(channelID)

	if u.v.GetBool(u.br.Protocol() + ".hidereactions") {
		logger.Debug("Not showing reaction: " + text + reaction)
		return
	}

	// No need to show added/removed reaction messages for our own.
	if sender.Me {
		logger.Debug("Not showing own reaction: " + text + reaction)
		return
	}

	if channelType == "D" {
		e := &bridge.DirectMessageEvent{
			Text:      text + reaction + message,
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
		Text:        text + reaction + message,
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
		ghost.Nick = sanitizeNick(ghost.Nick)
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
	ghost.Nick = sanitizeNick(ghost.Nick)

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

	// we did all the initialization, now listen for events
	go u.handleEventChan()
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

		logSince := "server"
		channame := brchannel.Name
		if !brchannel.DM {
			channame = fmt.Sprintf("#%s", brchannel.Name)
		}

		// We used to stored last viewed at if present.
		var lastViewedAt int64
		key := brchannel.ID
		u.lastViewedAtDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(u.User))
			if v := b.Get([]byte(key)); v != nil {
				lastViewedAt = int64(binary.LittleEndian.Uint64(v))
			}
			return nil
		})

		// But only use the stored last viewed if it's later than what the server knows.
		if lastViewedAt > since {
			since = lastViewedAt + 1
			logSince = "stored"
		}

		// post everything to the channel you haven't seen yet
		postlist := u.br.GetPostsSince(brchannel.ID, since)
		if postlist == nil {
			// if the channel is not from the primary team id, we can't get posts
			if brchannel.TeamID == u.br.GetMe().TeamID {
				logger.Errorf("something wrong with getPostsSince for %s for channel %s (%s)", u.Nick, channame, brchannel.ID)
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

			// GetPostsSince will return older messages with reaction
			// changes since LastViewedAt. This will be confusing as
			// the user will think it's a duplicate, or a post out of
			// order. Plus, we don't show reaction changes when
			// relaying messages/logs so let's skip these.
			if p.CreateAt < since {
				continue
			}

			ts := time.Unix(0, p.CreateAt*int64(time.Millisecond))

			props := p.GetProps()
			botname, override := props["override_username"].(string)
			user := u.br.GetUser(p.UserId)
			nick := user.Nick
			if override {
				nick = botname
			}

			if p.Type == "system_add_to_team" || p.Type == "system_remove_from_team" {
				nick = systemUser
			}

			codeBlock := false
			for _, post := range strings.Split(p.Message, "\n") {
				if post == "```" {
					codeBlock = !codeBlock
				}
				// skip empty lines for anything not part of a code block.
				if !codeBlock && post == "" {
					continue
				}

				if showReplayHdr {
					date := ts.Format("2006-01-02 15:04:05")
					if brchannel.DM {
						spoof(nick, fmt.Sprintf("\x02Replaying msgs since %s\x0f", date))
					} else {
						spoof("matterircd", fmt.Sprintf("\x02Replaying msgs since %s\x0f", date))
					}
					logger.Infof("Replaying msgs for %s for %s (%s) since %s (%s)", u.Nick, channame, brchannel.ID, date, logSince)
					showReplayHdr = false
				}

				replayMsg := fmt.Sprintf("[%s] %s", ts.Format("15:04"), post)
				if (u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext")) && u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" && nick != systemUser {
					threadMsgID := u.prefixContext("", p.Id, p.ParentId, "")
					replayMsg = u.formatContextMessage(ts.Format("15:04"), threadMsgID, post)
				}
				spoof(nick, replayMsg)
			}

			if len(p.FileIds) == 0 {
				continue
			}

			for _, fname := range u.br.GetFileLinks(p.FileIds) {
				fileMsg := "download file - " + fname
				if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" {
					threadMsgID := u.prefixContext("", p.Id, p.ParentId, "")
					fileMsg = u.formatContextMessage(ts.Format("15:04"), threadMsgID, fileMsg)
				}
				spoof(nick, fileMsg)
			}
		}

		if len(mmPostList.Order) > 0 {
			if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
				u.updateLastViewed(brchannel.ID)
			}
			u.saveLastViewedAt(brchannel.ID)
		}
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

	switch protocol {
	case "slack":
		u.eventChan = make(chan *bridge.Event)
		u.br, err = slack.New(u.v, u.Credentials, u.eventChan, u.addUsersToChannels)
	case "mattermost":
		u.eventChan = make(chan *bridge.Event)
		u.br, _, err = mattermost.New(u.v, u.Credentials, u.eventChan, u.addUsersToChannels)
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

	err = u.lastViewedAtDB.Update(func(tx *bolt.Tx) error {
		_, err2 := tx.CreateBucketIfNotExists([]byte(u.User))
		return err2
	})
	if err != nil {
		return err
	}

	return nil
}

// nolint:unparam
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

func (u *User) formatContextMessage(ts, threadMsgID, msg string) string {
	var formattedMsg string
	switch {
	case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
		formattedMsg = threadMsgID + " " + msg
	case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
		formattedMsg = msg + " " + threadMsgID
	}
	if ts != "" {
		formattedMsg = "[" + ts + "] " + formattedMsg
	}
	return formattedMsg
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

func (u *User) saveLastViewedAt(channelID string) {
	currentTime := make([]byte, 8)
	binary.LittleEndian.PutUint64(currentTime, uint64(model.GetMillis()))

	err := u.lastViewedAtDB.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(u.User))
		err := b.Put([]byte(channelID), currentTime)
		return err
	})
	if err != nil {
		logger.Fatal(err)
	}
}

const lastViewedStateFormat = int64(1)

const defaultStaleDuration = int64((30 * 24 * time.Hour) / time.Millisecond)

func loadLastViewedAtStateFile(statePath string, staleDuration string) (map[string]int64, error) {
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
	var stale int64
	val, err := time.ParseDuration(staleDuration)
	if err != nil {
		stale = defaultStaleDuration
	} else {
		stale = val.Milliseconds()
	}

	lastSaved, ok := lastViewedAt["__LastViewedStateSavedTime__"]
	if !ok || (lastSaved > 0 && lastSaved < currentTime-stale) {
		logger.Debug("File stale? Last saved too old: ", time.Unix(lastViewedAt["__LastViewedStateSavedTime__"]/1000, 0))
		return nil, errors.New("stale lastViewedAt state file")
	}

	return lastViewedAt, nil
}
