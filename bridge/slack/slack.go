package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/davecgh/go-spew/spew"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

type Slack struct {
	sc           *slack.Client
	rtm          *slack.RTM
	sinfo        *slack.Info
	susers       map[string]slack.User
	connected    bool
	userlistdone bool
	credentials  bridge.Credentials
	eventChan    chan *bridge.Event
	onConnect    func()
	msgLast      map[string]string
	sync.RWMutex

	cfg *config.Config
}

var logger *logrus.Entry

func New(cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, error) {
	s := &Slack{
		credentials: cred,
		eventChan:   eventChan,
		onConnect:   onConnect,
		cfg:         cfg,
	}

	rc := cfg.Current()

	ourlog := logrus.New()
	ourlog.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})
	logger = ourlog.WithFields(logrus.Fields{"prefix": "bridge/slack"})
	if rc.Debug {
		ourlog.SetLevel(logrus.DebugLevel)
	}

	if rc.Trace {
		ourlog.SetLevel(logrus.TraceLevel)
	}

	sc, err := s.loginToSlack()
	if err != nil {
		return nil, err
	}
	// Helper closure to set bridge levels
	setBridgeLogLevels := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			ourlog.SetLevel(logrus.TraceLevel)
		case rc.Debug:
			ourlog.SetLevel(logrus.DebugLevel)
		default:
			ourlog.SetLevel(logrus.InfoLevel)
		}
	}

	// Set initial log level
	setBridgeLogLevels(rc)

	// Register hook to update live on reloads
	cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
		setBridgeLogLevels(newRC)
	})

	s.sc = sc
	s.msgLast = make(map[string]string)

	users, _ := s.sc.GetUsers()
	for _, mmuser := range users {
		// do not add our own nick
		if mmuser.ID == s.sinfo.User.ID {
			continue
		}

		s.susers[mmuser.ID] = mmuser
	}

	s.userlistdone = true

	return s, nil
}

func (s *Slack) Invite(ctx context.Context, channelID, username string) error {
	_, err := s.sc.InviteUsersToConversation(strings.ToUpper(channelID), username)
	return err
}

func (s *Slack) IsChannelMember(channelID string) bool {
	info, err := s.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return false
	}

	return info.IsMember
}

func (s *Slack) Join(ctx context.Context, channelName string) (string, string, error) {
	mychan, _, _, err := s.sc.JoinConversation(channelName)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i): %s", err)
	}

	return mychan.ID, mychan.Topic.Value, nil
}

func (s *Slack) List(ctx context.Context) (map[string]string, error) {
	channelinfo := make(map[string]string)

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: true,
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

OUTER:
	for {
		conversations, nextCursor, _ := s.sc.GetConversations(&params)
		params.Cursor = nextCursor

		for _, channel := range conversations {
			channelinfo["#"+channel.Name] = strings.ReplaceAll(channel.Topic.Value, "\n", " | ")
			if nextCursor == "" {
				break OUTER
			}
		}
	}

	return channelinfo, nil
}

func (s *Slack) Part(ctx context.Context, channelID string) error {
	_, err := s.sc.LeaveConversation(strings.ToUpper(channelID))
	return err
}

func (s *Slack) UpdateChannels(ctx context.Context) error {
	return nil
}

func (s *Slack) Logout(ctx context.Context) error {
	logger.Debug("calling logout from slack")

	err := s.rtm.Disconnect()
	if err != nil {
		logger.Debug("logoutfrom slack", err)
		return err
	}

	s.sc = nil

	logger.Info("logout succeeded")

	s.eventChan <- &bridge.Event{
		Type: "logout",
		Data: &bridge.LogoutEvent{},
	}

	s.connected = false

	return nil
}

func (s *Slack) createSlackMsgOption(text string) []slack.MsgOption {
	np := slack.NewPostMessageParameters()
	np.AsUser = true
	np.Parse = "full"
	// np.Username = u.User

	var opts []slack.MsgOption
	opts = append(opts,
		slack.MsgOptionPostMessageParameters(np),
		// provide regular text field (fallback used in Slack notifications, etc.)
		slack.MsgOptionText(text, false),
	)

	return opts
}

func (s *Slack) MsgUser(ctx context.Context, username, text string) (string, error) {
	dchannel, _, _, err := s.sc.OpenConversation(&slack.OpenConversationParameters{
		Users: []string{username},
	})
	if err != nil {
		return "", err
	}

	// CTCP ACTION (/me)
	if strings.HasPrefix(text, "\x01ACTION ") {
		text = strings.TrimPrefix(text, "\x01ACTION ")
		text = strings.TrimSuffix(text, "\x01")
		text = "*" + text + "*"
	}

	opts := s.createSlackMsgOption(text)

	_, msgID, err := s.sc.PostMessage(dchannel.ID, opts...)
	if err != nil {
		return "", err
	}

	s.RLock()
	s.msgLast[dchannel.ID] = msgID
	s.RUnlock()

	return msgID, nil
}

func (s *Slack) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	// CTCP ACTION (/me)
	if strings.HasPrefix(text, "\x01ACTION ") {
		text = strings.TrimPrefix(text, "\x01ACTION ")
		text = strings.TrimSuffix(text, "\x01")
		text = "*" + text + "*"
	}

	opts := s.createSlackMsgOption(text)

	_, msgID, err := s.sc.PostMessage(strings.ToUpper(channelID), opts...)
	if err != nil {
		return "", err
	}

	s.RLock()
	s.msgLast[strings.ToUpper(channelID)] = msgID
	s.RUnlock()

	return msgID, nil
}

func (s *Slack) Topic(ctx context.Context, channelID string) string {
	info, err := s.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: strings.ToUpper(channelID),
	})
	if err != nil {
		logger.Errorf("error getting topic of %s: %s", channelID, err)
		return ""
	}

	return info.Topic.Value
}

func (s *Slack) SetTopic(ctx context.Context, channelID, text string) error {
	_, err := s.sc.SetTopicOfConversation(strings.ToUpper(channelID), text)
	return err
}

func (s *Slack) StatusUser(ctx context.Context, name string) (string, error) {
	return "", nil
}

func (s *Slack) StatusUsers(ctx context.Context) (map[string]string, error) {
	return make(map[string]string), nil
}

func (s *Slack) Protocol() string {
	return "slack"
}

func (s *Slack) Kick(ctx context.Context, channelID, username string) error {
	return s.sc.KickUserFromConversation(strings.ToUpper(channelID), username)
}

func (s *Slack) SetStatus(ctx context.Context, status string) error {
	switch status {
	case "online":
		return s.sc.SetUserPresence("auto")
	case "away":
		return s.sc.SetUserPresence("away")
	}

	return nil
}

func (s *Slack) Nick(ctx context.Context, name string) error {
	return nil
}

func (s *Slack) GetChannelName(ctx context.Context, channelID string) string {
	var name string

	info, err := s.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		name = channelID
	} else {
		name = "#" + info.Name
	}

	return name
}

func (s *Slack) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	var users []*bridge.UserInfo

	limit := 100

	info, err := s.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return nil, err
	}

	if info == nil {
		return nil, errors.New("Unknown channel seen (" + channelID + ")")
	}

	params := slack.GetUsersInConversationParameters{
		ChannelID: channelID,
		Cursor:    "",
		Limit:     limit,
	}

	for {
		members, nextCursor, _ := s.sc.GetUsersInConversation(&params)
		params.Cursor = nextCursor

		for _, user := range members {
			if s.sinfo.User.ID == user {
				continue
			}

			suser := s.getSlackUser(user)
			users = append(users, s.createUser(suser))
		}

		if nextCursor == "" {
			break
		}
	}

	// Add slackbot to all channels
	slackuser := s.getSlackUser("USLACKBOT")
	users = append(users, s.createUser(slackuser), s.GetMe())

	return users, nil
}

func (s *Slack) GetDMChannelName(userID1 string, userID2 string) string {
	return ""
}

func (s *Slack) GetDMUser(ctx context.Context, channelName string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetDMUserIDs(channelName string) (string, string, bool) {
	return "", "", false
}

func (s *Slack) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	return "", false
}

func (s *Slack) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	s.RLock()

	for _, user := range s.susers {
		user := user
		users = append(users, s.createUser(&user))
	}

	s.RUnlock()

	return users
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: true,
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

	for {
		mmchannels, nextCursor, err := s.sc.GetConversations(&params)
		if err != nil {
			logger.Error("GetChannels", err)
		}
		params.Cursor = nextCursor
		for _, mmchannel := range mmchannels {
			if !mmchannel.IsMember {
				continue
			}

			channels = append(channels, &bridge.ChannelInfo{
				Name:    mmchannel.Name,
				ID:      mmchannel.ID,
				TeamID:  s.sinfo.Team.ID,
				DM:      mmchannel.IsIM || mmchannel.IsMpIM,
				Private: !mmchannel.IsOpen,
			})
		}

		if nextCursor == "" {
			break
		}
	}

	return channels
}

func (s *Slack) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	return nil, errors.New("not implemented yet")
}

func (s *Slack) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
	channels := s.GetChannels()
	for _, channel := range channels {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (s *Slack) GetChannelCount() int {
	return 0
}

func (s *Slack) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return s.createUser(s.getSlackUser(userID))
}

func (s *Slack) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	return []string{}, nil
}

func (s *Slack) GetUserCount() int {
	return 0
}

func (s *Slack) GetMe() *bridge.UserInfo {
	me, _ := s.sc.GetUserInfo(s.sinfo.User.ID)
	return s.createUser(me)
}

func (s *Slack) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetTeamName(ctx context.Context, teamID string) string {
	return s.sinfo.Team.Name
}

func (s *Slack) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (s *Slack) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) IsDMChannelName(channelName string) bool {
	return false
}

func (s *Slack) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) UpdateLastViewed(ctx context.Context, channelID string) {
}

func (s *Slack) UpdateLastViewedUser(ctx context.Context, userID string) error {
	return nil
}

func (s *Slack) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	return []*bridge.File{}
}

func (s *Slack) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (s *Slack) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) GetPostThread(ctx context.Context, channelID string) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) GetChannelID(ctx context.Context, name, teamID string) string {
	return ""
}

func (s *Slack) allowedLogin() error {
	rc := s.cfg.Current()
	// we only know which server we are connecting to when we actually are connected.
	// disconnect if we're not allowed
	if len(rc.Slack.Bridge.Restrict) > 0 {
		ok := false
		for _, domain := range rc.Slack.Bridge.Restrict {
			if domain == s.sinfo.Team.Domain {
				ok = true
				break
			}
		}
		if !ok {
			s.rtm.Disconnect()
			return errors.New("Not allowed to connect to " + s.sinfo.Team.Domain + " slack")
		}
	}
	// we only know which user we are when we actually are connected.
	// disconnect if we're not allowed
	if len(rc.Slack.DenyUsers) > 0 {
		ok := false
		for _, user := range rc.Slack.DenyUsers {
			if user == s.sinfo.User.Name {
				ok = true
				break
			}
		}
		if ok {
			s.rtm.Disconnect()
			return errors.New("not allowed to connect")
		}
	}

	return nil
}

func (s *Slack) loginToSlack() (*slack.Client, error) {
	var err error

	if s.credentials.Token == "" {
		s.credentials.Token, err = s.getSlackToken()
		if err != nil {
			return nil, err
		}
	}

	var cookie string

	token := s.credentials.Token

	if strings.HasPrefix(s.credentials.Token, "xoxc") {
		token, cookie, err = passwordToTokenAndCookie(s.credentials.Token)
		if err != nil {
			return nil, err
		}
	}

	if cookie == "" {
		s.sc = slack.New(token, slack.OptionDebug(true))
	} else {
		s.sc = slack.New(token, slack.OptionDebug(true), slack.OptionHTTPClient(&httpClient{cookie: cookie}))
	}

	s.rtm = s.sc.NewRTM()
	s.susers = make(map[string]slack.User)

	go s.rtm.ManageConnection()

	count := 0

	s.sinfo = s.rtm.GetInfo()
	for s.sinfo == nil {
		time.Sleep(time.Millisecond * 500)
		logger.Debug("still waiting for sinfo")
		s.sinfo = s.rtm.GetInfo()
		count++
		if count == 20 {
			return nil, errors.New("couldn't connect in 10 seconds. Check your credentials")
		}
	}

	err = s.allowedLogin()
	if err != nil {
		return nil, err
	}

	go s.handleSlack(context.TODO())
	go s.onConnect()

	s.connected = true

	return s.sc, nil
}

//nolint:funlen,gocyclo
func (s *Slack) handleSlack(ctx context.Context) {
	for msg := range s.rtm.IncomingEvents {
		logger.Tracef("handleSlack %s", spew.Sdump(msg))
		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			switch ev.SubType {
			case "group_join", "channel_join", "group_leave", "channel_leave":
			default:
				s.handleSlackActionPost(ev)
			}
		case *slack.MemberLeftChannelEvent:
			s.handleMemberLeftChannel(ctx, ev)
		case *slack.MemberJoinedChannelEvent:
			s.handleMemberJoinedChannel(ctx, ev)
		case *slack.DisconnectedEvent:
			logger.Debug("disconnected event received, we should reconnect now..")
		case *slack.ReactionAddedEvent:
			logger.Debugf("ReactionAdded msg %#v", ev)
			ts := formatTS(ev.Item.Timestamp)
			msg := "[M " + ts + "] Added reaction :" + ev.Reaction + ":"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.ReactionRemovedEvent:
			logger.Debugf("ReactionRemoved msg %#v", ev)
			ts := formatTS(ev.Item.Timestamp)
			msg := "[M " + ts + "] Removed reaction :" + ev.Reaction + ":"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.StarAddedEvent:
			logger.Debugf("StarAdded msg %#v", ev)
			if ev.Item.Message == nil {
				continue
			}
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message starred (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.StarRemovedEvent:
			logger.Debugf("StarRemoved msg %#v", ev)
			if ev.Item.Message == nil {
				continue
			}
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message unstarred (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.PinAddedEvent:
			logger.Debugf("PinAdded msg %#v", ev)
			if ev.Item.Message == nil {
				continue
			}
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message pinned (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.PinRemovedEvent:
			logger.Debugf("PinRemoved msg %#v", ev)
			if ev.Item.Message == nil {
				continue
			}
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message unpinned (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		}
	}
}

func (s *Slack) handleActionMisc(userID, channelID, msg string) {
	suser, err := s.rtm.GetUserInfo(userID)
	if err != nil {
		return
	}

	// create new "ghost" user
	ghost := s.createUser(suser)

	// direct message
	switch {
	case strings.HasPrefix(channelID, "D"):
		sender := ghost
		receiver := ghost
		if ghost.Me {
			members, _, _ := s.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{
				ChannelID: channelID,
			})
			for _, member := range members {
				if member == s.GetMe().User {
					continue
				}

				ghostuser, _ := s.rtm.GetUserInfo(member)
				receiver = s.createUser(ghostuser)
			}
		}

		s.sendDirectMessage(sender, receiver, msg, channelID)
	default:
		event := &bridge.Event{
			Type: "channel_message",
			Data: &bridge.ChannelMessageEvent{
				Text:      msg,
				ChannelID: channelID,
				Sender:    ghost,
			},
		}

		s.eventChan <- event
	}
}

func (s *Slack) handleMemberLeftChannel(ctx context.Context, rmsg *slack.MemberLeftChannelEvent) {
	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Removed: []*bridge.UserInfo{
				s.GetUser(ctx, rmsg.User),
			},
			ChannelID: rmsg.Channel,
		},
	}

	s.eventChan <- event
}

func (s *Slack) handleMemberJoinedChannel(ctx context.Context, rmsg *slack.MemberJoinedChannelEvent) {
	var adder *bridge.UserInfo

	if rmsg.Inviter != "" {
		adder = &bridge.UserInfo{
			Nick: s.GetUser(ctx, rmsg.Inviter).Nick,
		}
	}

	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				s.GetUser(ctx, rmsg.User),
			},
			Adder:     adder,
			ChannelID: rmsg.Channel,
		},
	}
	s.eventChan <- event
}

func (s *Slack) getSlackUserFromMessage(rmsg *slack.MessageEvent) (*slack.User, error) {
	usr := rmsg.User
	if rmsg.SubType == "message_changed" {
		usr = rmsg.SubMessage.User
	}

	if rmsg.SubType == "message_deleted" {
		usr = "USLACKBOT"
	}

	if rmsg.SubType == "bot_message" {
		suser := &slack.User{
			ID:     rmsg.BotID,
			TeamID: s.sinfo.Team.ID,
			Profile: slack.UserProfile{
				FirstName: "bot",
				LastName:  "bot",
				RealName:  "bot",
			},
			Name: rmsg.Username,
		}

		if rmsg.Username == "" {
			bot, err := s.rtm.GetBotInfo(slack.GetBotInfoParameters{Bot: rmsg.BotID})
			if err != nil {
				suser.Profile.DisplayName = "bot"
				suser.Name = "bot"
			} else {
				suser.Profile.DisplayName = bot.Name
				suser.Name = bot.Name
			}
		}

		return suser, nil
	}

	suser, err := s.rtm.GetUserInfo(usr)
	if err != nil {
		return nil, err
	}

	return suser, nil
}

func (s *Slack) sendDirectMessage(sender, receiver *bridge.UserInfo, msg string, channelID string) {
	event := &bridge.Event{
		Type: "direct_message",
	}

	d := &bridge.DirectMessageEvent{
		Text:      msg,
		ChannelID: channelID,
	}

	d.Sender = sender
	d.Receiver = receiver

	event.Data = d

	s.eventChan <- event
}

func (s *Slack) sendPublicMessage(ghost *bridge.UserInfo, msg, channelID string) {
	event := &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:      msg,
			ChannelID: channelID,
			Sender:    ghost,
		},
	}

	s.eventChan <- event
}

// nolint:funlen,gocognit,gocyclo
func (s *Slack) handleSlackActionPost(rmsg *slack.MessageEvent) {
	logger.Debugf("handleSlackActionPost: receiving msg %#v", rmsg)

	// Is this our own message
	if rmsg.User == s.sinfo.User.ID {
		lastmsg := s.msgLast[rmsg.Channel]

		// Slack can be faster in sending new message than replying to POST
		if lastmsg < rmsg.Timestamp {
			time.Sleep(100 * time.Millisecond)
			lastmsg = s.msgLast[rmsg.Channel]
		}

		// Is this really the message we just sent
		if lastmsg == rmsg.Timestamp {
			return
		}
	}

	if rmsg.SubType == "message_deleted" {
		ts := formatTS(rmsg.DeletedTimestamp)
		rmsg.Text = "[M " + ts + "] Message deleted"
	}

	// TODO: cache userinfo
	suser, err := s.getSlackUserFromMessage(rmsg)
	if err != nil {
		logger.Errorf("couldn't find user in message %#v: %s", spew.Sdump(rmsg), err)
		return
	}

	msghandled := false

	// create new "ghost" user
	ghost := s.createUser(suser)

	spoofUsername := ghost.Nick

	msgs := []string{}

	if rmsg.Text != "" {
		msgs = append(msgs, strings.Split(rmsg.Text, "\n")...)
		msghandled = true
	}

	// look in attachments
	for _, attach := range rmsg.Attachments {
		if attach.Pretext != "" {
			msgs = append(msgs, strings.Split(attach.Pretext, "\n")...)
			msghandled = true
		}

		if attach.Text == "" {
			continue
		}

		for i, row := range strings.Split(attach.Text, "\n") {
			msgs = append(msgs, "> "+row)
			if i > 4 {
				msgs = append(msgs, "> ...")
				break
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
			msgs[0] = "[T " + formatTS(rmsg.ThreadTimestamp) + "] " + msgs[0]
		}
	}

	if rmsg.SubType == "message_changed" {
		msgs = append(msgs, strings.Split(rmsg.SubMessage.Text, "\n")...)
		if len(msgs) > 0 {
			msgs[0] = "[C " + formatTS(rmsg.SubMessage.Timestamp) + "] " + msgs[0]
		}
		msghandled = true
	}

	channelID := rmsg.Channel

	for _, msg := range msgs {
		// cleanup the message
		msg = s.cleanupMessage(msg)

		// still no text, ignore this message
		if !msghandled {
			msg = fmt.Sprintf("Empty: %#v", rmsg)
		}

		// direct message
		switch {
		case strings.HasPrefix(rmsg.Channel, "D"):

			sender := ghost
			receiver := ghost
			if ghost.Me {
				members, _, _ := s.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{
					ChannelID: channelID,
				})
				for _, member := range members {
					if member == s.GetMe().User {
						continue
					}

					ghostuser, _ := s.rtm.GetUserInfo(member)
					receiver = s.createUser(ghostuser)
				}
			}

			s.sendDirectMessage(sender, receiver, msg, channelID)
		default:
			// could be a bot
			ghost.Nick = spoofUsername
			s.sendPublicMessage(ghost, msg, channelID)
		}
	}
}

func (s *Slack) createUser(slackuser *slack.User) *bridge.UserInfo {
	if slackuser == nil {
		return &bridge.UserInfo{}
	}

	rc := s.cfg.Current()

	nick := slackuser.Name
	if (rc.Slack.PreferNickname || rc.Slack.UseDisplayName) && isValidNick(slackuser.Profile.DisplayName) {
		nick = slackuser.Profile.DisplayName
	}

	me := false

	if slackuser.ID == s.sinfo.User.ID {
		me = true
	}

	info := &bridge.UserInfo{
		Nick:        nick,
		User:        slackuser.ID,
		Real:        slackuser.RealName,
		Host:        "host",
		Roles:       "",
		DisplayName: slackuser.Profile.DisplayName,
		Ghost:       true,
		Me:          me,
		Username:    slackuser.Profile.RealName,
		FirstName:   slackuser.Profile.FirstName,
		LastName:    slackuser.Profile.LastName,
		TeamID:      s.sinfo.Team.ID,
	}

	return info
}

func (s *Slack) userName(id string) string {
	s.RLock()
	defer s.RUnlock()
	// TODO dynamically update when new users are joining slack
	for _, us := range s.susers {
		if us.ID != id {
			continue
		}

		if us.Profile.DisplayName != "" {
			return us.Profile.DisplayName
		}

		return us.Name
	}

	if id == s.sinfo.User.ID {
		return s.sinfo.User.Name
	}

	return ""
}

func (s *Slack) getSlackUser(userID string) *slack.User {
	s.RLock()
	defer s.RUnlock()

	if user, ok := s.susers[userID]; ok {
		user := user
		return &user
	}

	logger.Debugf("user %s not in cache, asking slack", userID)
	user, err := s.sc.GetUserInfo(userID)
	if err != nil {
		return nil
	}

	s.susers[user.ID] = *user

	return user
}

// nolint:unused
func (s *Slack) ratelimitCheck(err error) {
	if rateLimitedError, ok := err.(*slack.RateLimitedError); ok {
		time.Sleep(rateLimitedError.RetryAfter)
	}
}

func (s *Slack) Connected() bool {
	return s.connected
}

func (s *Slack) MsgUserThread(ctx context.Context, username, parentID, text string) (string, error) {
	return "", nil
}

func (s *Slack) MsgChannelThread(ctx context.Context, username, parentID, text string) (string, error) {
	return "", nil
}

func (s *Slack) ModifyPost(ctx context.Context, channelID, text string) error {
	return nil
}

func (s *Slack) AddReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (s *Slack) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (s *Slack) GetLastSentMsgs() []string {
	return []string{}
}

func (s *Slack) Config() any {
	return &s.cfg.Current().Slack
}

func (s *Slack) BridgeConfig() *config.BridgeConfig {
	return &s.cfg.Current().Slack.Bridge
}

func (s *Slack) FormatterConfig() *config.FormatterConfig {
	return &s.cfg.Current().Slack.Formatter
}

func (s *Slack) GetPostSizeLimit() int {
	return 4000
}

func (s *Slack) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (s *Slack) Ping(ctx context.Context, proto ...string) error {
	return nil
}

func (s *Slack) SendTyping(ctx context.Context, channelName string) {
}
