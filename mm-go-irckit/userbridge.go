package irckit

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/bridge/mastodon"
	"github.com/42wim/matterircd/bridge/mattermost"
	"github.com/42wim/matterircd/bridge/slack"
	"github.com/alecthomas/chroma/v2/quick"
	"github.com/davecgh/go-spew/spew"
	"github.com/kenshaw/emoji"
	"github.com/mattermost/mattermost-server/v6/model"
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
	away        bool               //nolint:structcheck

	lastViewedAtDB *bolt.DB //nolint:structcheck

	msgCounterMutex sync.RWMutex   //nolint:structcheck
	msgCounter      map[string]int //nolint:structcheck

	msgLastMutex sync.RWMutex         //nolint:structcheck
	msgLast      map[string][2]string //nolint:structcheck

	msgMapMutex      sync.RWMutex              //nolint:structcheck
	msgMap           map[string]map[string]int //nolint:structcheck
	msgMapIndexMutex sync.RWMutex              //nolint:structcheck
	msgMapIndex      map[string]map[int]string //nolint:structcheck

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
	u.msgMapIndex = make(map[string]map[int]string)
	u.msgCounter = make(map[string]int)
	u.updateCounter = make(map[string]time.Time)
	u.eventChan = make(chan *bridge.Event, 1000)

	// used for login
	u.createService("mattermost", "loginservice")
	u.createService("slack", "loginservice")
	u.createService("mastodon", "loginservice")
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

			if m == "" {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	prefixUser := event.Sender.User
	if event.Sender.Me {
		prefixUser = event.Receiver.User
	}
	text, prefix, suffix, showContext, maxlen := u.handleMessageThreadContext(prefixUser, event.MessageID, event.ParentID, event.Event, event.Text)

	lexer := ""
	codeBlockBackTick := false
	codeBlockTilde := false
	text = wordwrap.String(text, maxlen)
	lines := strings.Split(text, "\n")
	for _, text := range lines {

		// TODO: Ideally, we want to read the whole code block and syntax highlight on that, but let's go with per-line for now.
		text, codeBlockBackTick, codeBlockTilde, lexer = u.formatCodeBlockText(text, prefix, codeBlockBackTick, codeBlockTilde, lexer)

		if text == "" {
			continue
		}

		if !u.v.GetBool(u.br.Protocol()+".disableircemphasis") && !codeBlockBackTick && !codeBlockTilde {
			text = markdown2irc(text)
		}

		if !u.v.GetBool(u.br.Protocol()+".disableemoji") && !codeBlockBackTick && !codeBlockTilde { //nolint:goconst
			text = emoji.ReplaceAliases(text)
		}

		if showContext {
			text = prefix + text + suffix
		}

		if event.Sender.Me {
			if event.Receiver.Me {
				u.MsgSpoofUser(u, u.Nick, text, len(text))
			} else {
				u.MsgSpoofUser(u, event.Receiver.Nick, text, len(text))
			}
		} else {
			u.MsgSpoofUser(u.createUserFromInfo(event.Sender), u.Nick, text, len(text))
		}
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
			ch.SpoofMessage(systemUser, "\x1dadded "+added.Nick+" to the channel by "+event.Adder.Nick+"\x1d")
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
			ch.SpoofMessage(systemUser, "\x1dremoved "+removed.Nick+" from the channel by "+event.Remover.Nick+"\x1d")
		}
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) getMessageChannel(channelID string, sender *bridge.UserInfo) Channel {
	ch := u.Srv.Channel(channelID)
	ghost := u.createUserFromInfo(sender)

	// if it's another user, let them join
	if !ghost.Me && !ch.HasUser(ghost) {
		if u.br.Protocol() != "mastodon" {
			logger.Debugf("User %s is not in channel %s. Joining now", ghost.Nick, ch.String())
			ch.Join(ghost) //nolint:errcheck
		}
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
		if u.v.GetBool(u.br.Protocol() + ".showonlyjoined") {
			return
		}
		nick += "/" + u.Srv.Channel(event.ChannelID).String()
	}

	if u.v.GetBool(u.br.Protocol() + ".showmentions") {
		for _, m := range u.MentionKeys {
			if m == u.Nick {
				continue
			}

			if m == "" {
				continue
			}

			if strings.Contains(event.Text, m) {
				event.Text = event.Text + " (mention " + u.Nick + ")"
			}
		}
	}

	text := event.Text
	prefix := ""
	suffix := ""
	showContext := false
	maxlen := 440
	if u.Nick != systemUser {
		text, prefix, suffix, showContext, maxlen = u.handleMessageThreadContext(event.ChannelID, event.MessageID, event.ParentID, event.Event, event.Text)
	} else {
		text = "\x1d" + text + "\x1d"
	}

	lexer := ""
	codeBlockBackTick := false
	codeBlockTilde := false
	text = wordwrap.String(text, maxlen)
	lines := strings.Split(text, "\n")
	for _, text := range lines {

		// TODO: Ideally, we want to read the whole code block and syntax highlight on that, but let's go with per-line for now.
		text, codeBlockBackTick, codeBlockTilde, lexer = u.formatCodeBlockText(text, prefix, codeBlockBackTick, codeBlockTilde, lexer)

		if text == "" {
			continue
		}

		if !u.v.GetBool(u.br.Protocol()+".disableircemphasis") && !codeBlockBackTick && !codeBlockTilde {
			text = markdown2irc(text)
		}

		if !u.v.GetBool(u.br.Protocol()+".disableemoji") && !codeBlockBackTick && !codeBlockTilde {
			text = emoji.ReplaceAliases(text)
		}

		if showContext {
			text = prefix + text + suffix
		}

		switch event.MessageType {
		case "notice":
			ch.SpoofNotice(nick, text, len(text))
		default:
			ch.SpoofMessage(nick, text, len(text))
		}
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableautoview") {
		u.updateLastViewed(event.ChannelID)
	}
	u.saveLastViewedAt(event.ChannelID)
}

func (u *User) handleFileEvent(event *bridge.FileEvent) {
	if u.v.GetBool(u.br.Protocol()+".showonlyjoined") && event.ChannelType != "D" {
		ch := u.getMessageChannel(event.ChannelID, event.Sender)
		if ch.ID() == "&messages" {
			return
		}
	}

	for _, fname := range event.Files {
		fileMsg := "\x1ddownload file - " + fname.Name + "\x1d"
		if u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext") {
			threadMsgID := u.prefixContext(event.ChannelID, event.MessageID, event.ParentID, "posted_file")
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
				u.MsgSpoofUser(u.createUserFromInfo(event.Sender), u.Nick, fileMsg)
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

	for _, brchannel := range u.br.GetChannels() {
		if brchannel.ID == event.ChannelID {
			logger.Debugf("ACTION_CHANNEL_DELETED removing myself from %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)

			ch.Part(u, "")
			return
		}
	}

	logger.Debugf("ACTION_CHANNEL_DELETED not in channel %s (%s)", u.br.GetChannelName(event.ChannelID), event.ChannelID)
}

func (u *User) handleUserUpdateEvent(event *bridge.UserUpdateEvent) {
	u.updateUserFromInfo(event.User)
}

func (u *User) handleStatusChangeEvent(event *bridge.StatusChangeEvent) {
	if event.UserID == u.br.GetMe().User {
		switch event.Status {
		case "online":
			if u.away {
				logger.Debug("setting myself online")
				u.away = false
				u.Srv.EncodeMessage(u, irc.RPL_UNAWAY, []string{u.Nick}, "You are no longer marked as being away") //nolint:errcheck
			}
		// Ignore `offline` status changes to prevent bouncing between being marked away and not.
		case "offline":
			logger.Debugf("doing nothing as status %s", event.Status)
		default:
			if !u.away {
				logger.Debug("setting myself away")
				u.away = true
				u.Srv.EncodeMessage(u, irc.RPL_NOWAWAY, []string{u.Nick}, "You have been marked as being away") //nolint:errcheck
			}
		}
	}
}

func (u *User) handleReactionEvent(event interface{}) {
	var (
		text, channelID, messageID, parentID, channelType, reaction string
		sender                                                      *bridge.UserInfo
	)

	message := ""

	switch e := event.(type) {
	case *bridge.ReactionAddEvent:
		message = e.Message
		text = "added reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
		parentID = e.ParentID
	case *bridge.ReactionRemoveEvent:
		message = e.Message
		text = "removed reaction: "
		channelID = e.ChannelID
		messageID = e.MessageID
		sender = e.Sender
		channelType = e.ChannelType
		reaction = e.Reaction
		parentID = e.ParentID
	}

	defer u.saveLastViewedAt(channelID)

	if u.v.GetBool(u.br.Protocol() + ".hidereactions") {
		logger.Debug("Not showing reaction: " + text + reaction)
		return
	}

	if !u.v.GetBool(u.br.Protocol() + ".disableemoji") {
		reactionEmoji := emoji.FromAlias(reaction)
		if reactionEmoji != nil {
			reaction = fmt.Sprintf("%s", reactionEmoji)
		}
	}

	if channelType == "D" {
		e := &bridge.DirectMessageEvent{
			Text:      "\x1d" + text + reaction + "\x1d" + message,
			ChannelID: channelID,
			Receiver:  u.UserInfo,
			Sender:    sender,
			MessageID: messageID,
			Event:     "reaction",
			ParentID:  parentID,
		}

		u.handleDirectMessageEvent(e)
		return
	}

	e := &bridge.ChannelMessageEvent{
		Text:        "\x1d" + text + reaction + "\x1d" + message,
		ChannelID:   channelID,
		ChannelType: channelType,
		Sender:      sender,
		MessageID:   messageID,
		Event:       "reaction",
		ParentID:    parentID,
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
	if len(users) > 0 {
		srv.BatchAdd(users)
		u.addUsersToChannel(users, "&users", "&users")

		// join ourself
		ch.Join(u) //nolint:errcheck
	}

	if u.br.Protocol() == "mastodon" {
		ch = srv.Channel("mastodon")
		ch.Join(u) //nolint:errcheck
	}

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

func (u *User) createSpoof(mmchannel *bridge.ChannelInfo) func(string, string, ...int) {
	if strings.Contains(mmchannel.Name, "__") {
		return func(nick string, msg string, maxlen ...int) {
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

//nolint:funlen,gocognit,gocyclo,cyclop
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
		err := u.lastViewedAtDB.View(func(tx *bolt.Tx) error {
			b := tx.Bucket([]byte(u.User))
			if v := b.Get([]byte(key)); v != nil {
				lastViewedAt = int64(binary.LittleEndian.Uint64(v))
			}
			return nil
		})
		if err != nil {
			logger.Errorf("something wrong with u.lastViewedAtDB.View for %s for channel %s (%s)", u.Nick, channame, brchannel.ID)
			lastViewedAt = since
		}

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

		mmPostList, _ := postlist.(*model.PostList)
		if mmPostList == nil {
			continue
		}
		// traverse the order in reverse
		for i := len(mmPostList.Order) - 1; i >= 0; i-- {
			p := mmPostList.Posts[mmPostList.Order[i]]
			if p.Type == model.PostTypeJoinLeave {
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

			if p.Type == model.PostTypeAddToTeam || p.Type == model.PostTypeRemoveFromTeam {
				nick = systemUser
			}

			for _, post := range strings.Split(p.Message, "\n") {
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

				if nick == systemUser {
					post = "\x1d" + post + "\x1d"
				}

				replayMsg := fmt.Sprintf("[%s] %s", ts.Format("15:04"), post)
				if (u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext")) && nick != systemUser {
					threadMsgID := u.prefixContext(brchannel.ID, p.Id, p.RootId, "replay")
					replayMsg = u.formatContextMessage(ts.Format("15:04"), threadMsgID, post)
				}
				spoof(nick, replayMsg)
			}

			if len(p.FileIds) == 0 {
				continue
			}

			for _, fname := range u.br.GetFileLinks(p.FileIds) {
				fileMsg := "\x1ddownload file - " + fname + "\x1d"
				if u.v.GetBool(u.br.Protocol()+".prefixcontext") || u.v.GetBool(u.br.Protocol()+".suffixcontext") {
					threadMsgID := u.prefixContext(brchannel.ID, p.Id, p.RootId, "replay_file")
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
		Prefix:        toUser.Prefix(),
		Command:       irc.PRIVMSG,
		Params:        []string{u.Nick},
		Trailing:      msg,
		EmptyTrailing: true,
	})
}

func (u *User) MsgSpoofUser(sender *User, rcvuser string, msg string, maxlen ...int) {
	if len(maxlen) == 0 {
		msg = wordwrap.String(msg, 440)
	} else {
		msg = wordwrap.String(msg, maxlen[0])
	}
	lines := strings.Split(msg, "\n")
	for _, l := range lines {
		u.Encode(&irc.Message{
			Prefix: &irc.Prefix{
				Name: sender.Nick,
				User: sender.Nick,
				Host: sender.Host,
			},
			Command:       irc.PRIVMSG,
			Params:        []string{rcvuser},
			Trailing:      l,
			EmptyTrailing: true,
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
	case "mastodon":
		u.eventChan = make(chan *bridge.Event)
		u.br, err = mastodon.New(u.v, u.Credentials, u.eventChan, u.addUsersToChannels)
	case "slack":
		u.eventChan = make(chan *bridge.Event)
		u.br, err = slack.New(u.v, u.Credentials, u.eventChan, u.addUsersToChannels)
	case "mattermost":
		u.eventChan = make(chan *bridge.Event)
		if u.v.GetBool("mattermost.ignoreserverversion") || strings.HasPrefix(u.getMattermostVersion(), "7.") || strings.HasPrefix(u.getMattermostVersion(), "8.") || strings.HasPrefix(u.getMattermostVersion(), "9.") {
			u.br, _, err = mattermost.New(u.v, u.Credentials, u.eventChan, u.addUsersToChannels)
		} else {
			return fmt.Errorf("mattermost version %s not supported", u.getMattermostVersion())
		}
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

func (u *User) increaseMsgCounter(channelID string, skip int) int {
	u.msgCounterMutex.Lock()
	defer u.msgCounterMutex.Unlock()

	u.msgCounter[channelID]++

	// max 4096 entries (0xFFF); set back to 1, 0 is used for absent.
	if u.msgCounter[channelID] >= 4096 {
		u.msgCounter[channelID] = 1
	}

	if skip != 0 && u.msgCounter[channelID] == skip {
		u.msgCounter[channelID]++

		// max 4096 entries (0xFFF); set back to 1, 0 is used for absent.
		if u.msgCounter[channelID] >= 4096 {
			u.msgCounter[channelID] = 1
		}
	}

	return u.msgCounter[channelID]
}

func (u *User) updateMsgMapIndex(channelID string, counter int, messageID string) {
	u.msgMapIndexMutex.Lock()
	defer u.msgMapIndexMutex.Unlock()

	var (
		ok    bool
		msgID string
	)

	if _, ok = u.msgMapIndex[channelID]; !ok {
		u.msgMapIndex[channelID] = make(map[int]string)
	}

	if msgID, ok = u.msgMapIndex[channelID][counter]; !ok {
		u.msgMapIndex[channelID][counter] = messageID
		return
	}

	// map entry is the same as the one provided so do nothing.
	if msgID == messageID {
		return
	}

	// Remove previous msgID from MsgMap with the same counter.
	if _, ok = u.msgMap[channelID][msgID]; ok {
		delete(u.msgMap[channelID], msgID)
	}

	u.msgMapIndex[channelID][counter] = messageID
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

func (u *User) prefixContext(channelID, messageID, parentID, event string) string {
	logger.Tracef("prefixContext ch %s msg %s parent %s event %s", channelID, messageID, parentID, event)

	prefixChar := "->"
	if u.v.GetBool(u.br.Protocol() + ".unicode") {
		prefixChar = "↪"
	}

	if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" || u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost+post" {
		if parentID == "" {
			return fmt.Sprintf("[@@%s]", messageID)
		}
		if u.v.GetString(u.br.Protocol()+".threadcontext") == "mattermost" || parentID == messageID {
			return fmt.Sprintf("[%s@@%s]", prefixChar, parentID)
		}
		return fmt.Sprintf("[%s@@%s,@@%s]", prefixChar, parentID, messageID)
	}

	u.msgMapMutex.Lock()
	defer u.msgMapMutex.Unlock()

	var (
		currentcount, parentcount int
		ok                        bool
	)

	if _, ok = u.msgMap[channelID]; !ok {
		u.msgMap[channelID] = make(map[string]int)
	}

	if parentID != "" {
		if _, ok = u.msgMap[channelID][parentID]; !ok {
			u.msgMap[channelID][parentID] = u.increaseMsgCounter(channelID, parentcount)
		}

		parentcount = u.msgMap[channelID][parentID]
		u.updateMsgMapIndex(channelID, parentcount, parentID)
	}

	if _, ok = u.msgMap[channelID][messageID]; !ok {
		u.msgMap[channelID][messageID] = u.increaseMsgCounter(channelID, parentcount)
	}

	currentcount = u.msgMap[channelID][messageID]
	u.updateMsgMapIndex(channelID, currentcount, messageID)

	if parentID != "" {
		return fmt.Sprintf("[%s%03x,%03x]", prefixChar, parentcount, currentcount)
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
		r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
		time.Sleep(time.Duration(r.Intn(3000)) * time.Millisecond)
		u.br.UpdateLastViewed(channelID)
	}()
}

func (u *User) saveLastViewedAt(channelID string) {
	if channelID == "" {
		return
	}

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

func (u *User) getMattermostVersion() string {
	proto := "https"

	if u.v.GetBool("mattermost.insecure") {
		proto = "http"
	}

	resp, err := http.Get(proto + "://" + u.Credentials.Server)
	if err != nil {
		logger.Errorf("Failed to get mattermost version: %s", err)
		return ""
	}

	defer resp.Body.Close()

	return resp.Header.Get("X-Version-Id")
}

func (u *User) handleMessageThreadContext(channelID, messageID, parentID, event, text string) (string, string, string, bool, int) {
	newText := text
	prefix := ""
	suffix := ""
	maxlen := 440
	showContext := false

	switch {
	case u.v.GetBool(u.br.Protocol()+".prefixcontext") && strings.HasPrefix(text, "\x01"):
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		newText = strings.Replace(text, "\x01ACTION ", "\x01ACTION "+prefix, 1)
		maxlen = len(newText)
	case u.v.GetBool(u.br.Protocol()+".prefixcontext") && u.v.GetBool(u.br.Protocol()+".showcontextmulti"):
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		newText = text
		showContext = true
		maxlen -= len(prefix)
	case u.v.GetBool(u.br.Protocol() + ".prefixcontext"):
		prefix = u.prefixContext(channelID, messageID, parentID, event) + " "
		newText = prefix + text
	case u.v.GetBool(u.br.Protocol()+".suffixcontext") && strings.HasSuffix(text, "\x01"):
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		newText = strings.Replace(text, " \x01", suffix+" \x01", 1)
		maxlen = len(newText)
	case u.v.GetBool(u.br.Protocol()+".suffixcontext") && u.v.GetBool(u.br.Protocol()+".showcontextmulti"):
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		newText = text
		showContext = true
		maxlen -= len(suffix)
	case u.v.GetBool(u.br.Protocol() + ".suffixcontext"):
		suffix = " " + u.prefixContext(channelID, messageID, parentID, event)
		newText = strings.TrimRight(text, "\n") + suffix
	}

	return newText, prefix, suffix, showContext, maxlen
}

//nolint:gocyclo
func (u *User) formatCodeBlockText(text string, prefix string, codeBlockBackTick bool, codeBlockTilde bool, lexer string) (string, bool, bool, string) {
	// skip empty lines for anything not part of a code block.
	if text == "" {
		if codeBlockBackTick || codeBlockTilde {
			return " ", codeBlockBackTick, codeBlockTilde, lexer
		}
		return "", codeBlockBackTick, codeBlockTilde, lexer
	}

	syntaxHighlighting := u.v.GetString(u.br.Protocol() + ".syntaxhighlighting")

	if (strings.HasPrefix(text, "```") || strings.HasPrefix(text, prefix+"```")) && !codeBlockTilde {
		codeBlockBackTick = !codeBlockBackTick
		if codeBlockBackTick {
			lexer = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "```"), prefix+"```"))
		}
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}
	if (strings.HasPrefix(text, "~~~") || strings.HasPrefix(text, prefix+"~~~")) && !codeBlockBackTick {
		codeBlockTilde = !codeBlockTilde
		if codeBlockTilde {
			lexer = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "~~~"), prefix+"~~~"))
		}
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	if !(codeBlockBackTick || codeBlockTilde) || syntaxHighlighting == "" || lexer == "" {
		return text, codeBlockBackTick, codeBlockTilde, lexer
	}

	formatter := "terminal256"
	style := "pygments"
	v := strings.SplitN(syntaxHighlighting, ":", 2)
	if len(v) == 2 {
		formatter = v[0]
		style = v[1]
	}

	var b bytes.Buffer
	err := quick.Highlight(&b, text, lexer, formatter, style)
	if err == nil {
		text = b.String()
		// Work around https://github.com/alecthomas/chroma/issues/716
		text = strings.ReplaceAll(text, "\n", "")
	}

	return text, codeBlockBackTick, codeBlockTilde, lexer
}

// Use static initialisation to optimize.
// Bold & Italic - https://www.markdownguide.org/basic-syntax#bold-and-italic
var boldItalicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*\*\*)+?(.+?)(?:\*\*\*)+?`),
	regexp.MustCompile(`\b(?:\_\_\_)+?(.+?)(?:\_\_\_)+?\b`),
	regexp.MustCompile(`\b(?:\_\_\*)+?(.+?)(?:\*\_\_)+?\b`),
	regexp.MustCompile(`\b(?:\*\*\_)+?(.+?)(?:\_\*\*)+?\b`),
}

// Bold - https://www.markdownguide.org/basic-syntax#bold
var boldRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*\*)+?(.+?)(?:\*\*)+?`),
	regexp.MustCompile(`\b(?:\_\_)+?(.+?)(?:\_\_)+?\b`),
}

// Italic - https://www.markdownguide.org/basic-syntax#italic
var italicRegExp = []*regexp.Regexp{
	regexp.MustCompile(`(?:\*)+?([^\*]+?)(?:\*)+?`),
	regexp.MustCompile(`\b(?:\_)+?([^_]+?)(?:\_)+?\b`),
}

func markdown2irc(msg string) string {
	// Bold & Italic 0x02+0x1d
	for _, re := range boldItalicRegExp {
		msg = re.ReplaceAllString(msg, "\x02\x1d$1\x1d\x02")
	}

	// Bold 0x02
	for _, re := range boldRegExp {
		msg = re.ReplaceAllString(msg, "\x02$1\x02")
	}

	// Italic 0x1d
	for _, re := range italicRegExp {
		msg = re.ReplaceAllString(msg, "\x1d$1\x1d")
	}

	return msg
}
