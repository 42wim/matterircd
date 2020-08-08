package slack

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/schollz/logger"
	"github.com/slack-go/slack"
	"github.com/spf13/viper"
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
	sync.RWMutex
	v *viper.Viper
}

func New(v *viper.Viper, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, error) {
	s := &Slack{
		credentials: cred,
		eventChan:   eventChan,
		onConnect:   onConnect,
		v:           v,
	}

	var err error

	s.sc, err = s.loginToSlack()
	if err != nil {
		return nil, err
	}

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

func (s *Slack) Invite(channelID, username string) error {
	_, err := s.sc.InviteUsersToConversation(strings.ToUpper(channelID), username)
	return err
}

func (s *Slack) Join(channelName string) (string, string, error) {
	mychan, _, _, err := s.sc.JoinConversation(channelName)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i): %s", err)
	}

	return mychan.ID, mychan.Topic.Value, nil
}

func (s *Slack) List() (map[string]string, error) {
	channelinfo := make(map[string]string)

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: "true",
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

func (s *Slack) Part(channelID string) error {
	_, err := s.sc.LeaveConversation(strings.ToUpper(channelID))
	return err
}

func (s *Slack) UpdateChannels() error {
	return nil
}

func (s *Slack) Logout() error {
	logger.Debug("calling logout from slack")

	err := s.rtm.Disconnect()
	if err != nil {
		logger.Debug("logoutfrom slack", err)
		return err
	}

	s.sc = nil

	logger.Info("logout succeeded")

	s.connected = false

	return nil
}

func (s *Slack) createSlackMsgOption(text string) []slack.MsgOption {
	np := slack.NewPostMessageParameters()
	np.AsUser = true
	// np.Username = u.User

	var opts []slack.MsgOption
	opts = append(opts,
		slack.MsgOptionPostMessageParameters(np),
		// provide regular text field (fallback used in Slack notifications, etc.)
		slack.MsgOptionText(text, false),

		// add a callback ID so we can see we created it
		slack.MsgOptionBlocks(slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, text, false, false),
			nil, nil,
			slack.SectionBlockOptionBlockID("matterircd_"+s.sinfo.User.ID),
		)),
	)

	return opts
}

func (s *Slack) MsgUser(username, text string) error {
	_, _, dchannel, err := s.sc.OpenIMChannel(username)
	if err != nil {
		return err
	}

	opts := s.createSlackMsgOption(text)

	_, _, err = s.sc.PostMessage(dchannel, opts...)
	if err != nil {
		return err
	}
	return nil
}

func (s *Slack) MsgChannel(channelID, text string) error {
	opts := s.createSlackMsgOption(text)

	_, _, err := s.sc.PostMessage(strings.ToUpper(channelID), opts...)
	if err != nil {
		return err
	}

	return nil
}

func (s *Slack) Topic(channelID string) string {
	info, err := s.sc.GetConversationInfo(channelID, false)
	if err != nil {
		return ""
	}

	return info.Topic.Value
}

func (s *Slack) SetTopic(channelID, text string) error {
	_, err := s.sc.SetTopicOfConversation(strings.ToUpper(channelID), text)
	return err
}

func (s *Slack) StatusUser(name string) (string, error) {
	return "", nil
}

func (s *Slack) StatusUsers() (map[string]string, error) {
	return make(map[string]string), nil
}

func (s *Slack) Protocol() string {
	return "slack"
}

func (s *Slack) Kick(channelID, username string) error {
	return s.sc.KickUserFromConversation(strings.ToUpper(channelID), username)
}

func (s *Slack) SetStatus(status string) error {
	switch status {
	case "online":
		return s.sc.SetUserPresence("auto")
	case "away":
		return s.sc.SetUserPresence("away")
	}

	return nil
}

func (s *Slack) Nick(name string) error {
	return nil
}

func (s *Slack) GetChannelName(channelID string) string {
	var name string

	info, err := s.sc.GetConversationInfo(channelID, false)
	if err != nil {
		name = channelID
	} else {
		name = "#" + info.Name
	}

	return name
}

func (s *Slack) GetChannelUsers(channelID string) ([]*bridge.UserInfo, error) {
	var users []*bridge.UserInfo

	limit := 100

	info, err := s.sc.GetConversationInfo(channelID, false)
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
			users = append(users, s.createSlackUser(suser))
		}

		if nextCursor == "" {
			break
		}
	}

	// Add slackbot to all channels
	slackuser := s.getSlackUser("USLACKBOT")
	users = append(users, s.createSlackUser(slackuser), s.GetMe())

	return users, nil
}

func (s *Slack) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	s.RLock()

	for _, user := range s.susers {
		user := user
		users = append(users, s.createSlackUser(&user))
	}

	s.RUnlock()

	return users
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: "true",
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

	for {
		mmchannels, nextCursor, _ := s.sc.GetConversations(&params)
		params.Cursor = nextCursor
		for _, mmchannel := range mmchannels {
			if !mmchannel.IsMember {
				continue
			}

			if mmchannel.IsMpIM && s.v.GetBool("slack.joinMpImOnTalk") {
				continue
			}

			logger.Debug("Adding channel", mmchannel)
			channels = append(channels, &bridge.ChannelInfo{
				Name:   mmchannel.Name,
				ID:     mmchannel.ID,
				TeamID: s.sinfo.Team.ID,
			})
		}

		if nextCursor == "" {
			break
		}
	}

	return channels
}

func (s *Slack) GetUser(userID string) *bridge.UserInfo {
	return s.createSlackUser(s.getSlackUser(userID))
}

func (s *Slack) GetMe() *bridge.UserInfo {
	me, _ := s.sc.GetUserInfo(s.sinfo.User.ID)
	return s.createSlackUser(me)
}

func (s *Slack) GetUserByUsername(username string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetTeamName(teamID string) string {
	return s.sinfo.Team.Name
}

func (s *Slack) GetLastViewedAt(channelID string) int64 {
	return 0
}

func (s *Slack) GetPostsSince(channelID string, since int64) interface{} {
	return nil
}

func (s *Slack) SearchPosts(search string) interface{} {
	return nil
}

func (s *Slack) UpdateLastViewed(channelID string) {
}

func (s *Slack) UpdateLastViewedUser(userID string) error {
	return nil
}

func (s *Slack) GetFileLinks(fileIDs []string) []string {
	return []string{}
}

func (s *Slack) SearchUsers(query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (s *Slack) GetPosts(channelID string, limit int) interface{} {
	return nil
}

func (s *Slack) GetChannelID(name, teamID string) string {
	return ""
}

func (s *Slack) allowedLogin() error {
	// we only know which server we are connecting to when we actually are connected.
	// disconnect if we're not allowed
	if len(s.v.GetStringSlice("slack.restrict")) > 0 {
		ok := false
		for _, domain := range s.v.GetStringSlice("slack.restrict") {
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
	if len(s.v.GetStringSlice("slack.BlackListUser")) > 0 {
		ok := false
		for _, user := range s.v.GetStringSlice("slack.BlackListUser") {
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

	s.sc = slack.New(s.credentials.Token, slack.OptionDebug(true))
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

	go s.handleSlack()
	go s.onConnect()

	s.connected = true

	return s.sc, nil
}

func (s *Slack) handleSlack() {
	for msg := range s.rtm.IncomingEvents {
		switch ev := msg.Data.(type) {
		case *slack.MessageEvent:
			if ev.SubType == "group_join" || ev.SubType == "channel_join" || ev.SubType == "member_joined_channel" {
				s.handleActionJoin(ev)
			} else {
				s.handleSlackActionPost(ev)
			}
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
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message starred (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.StarRemovedEvent:
			logger.Debugf("StarRemoved msg %#v", ev)
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message unstarred (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.PinAddedEvent:
			logger.Debugf("PinAdded msg %#v", ev)
			ts := formatTS(ev.Item.Message.Timestamp)
			msg := "[M " + ts + "] Message pinned (" + ev.Item.Message.Text + ")"
			s.handleActionMisc(ev.User, ev.Item.Channel, msg)
		case *slack.PinRemovedEvent:
			logger.Debugf("PinRemoved msg %#v", ev)
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
	ghost := s.createSlackUser(suser)

	// direct message
	switch {
	case strings.HasPrefix(channelID, "D"):
		spoofUsername := ghost.Nick
		event := &bridge.Event{
			Type: "direct_message",
		}

		d := &bridge.DirectMessageEvent{
			Text: msg,
		}

		if !ghost.Me {
			d.Sender = ghost
		} else {
			members, _, _ := s.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: channelID})
			for _, member := range members {
				if member != s.sinfo.User.ID {
					other, _ := s.rtm.GetUserInfo(member)
					otheruser := s.createSlackUser(other)
					spoofUsername = otheruser.Nick
					break
				}
			}
		}

		d.Receiver = spoofUsername

		s.eventChan <- event
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

func (s *Slack) handleActionJoin(rmsg *slack.MessageEvent) {
	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				s.GetUser(rmsg.User),
			},
			Adder: &bridge.UserInfo{
				Nick: rmsg.Inviter,
			},
			ChannelID: rmsg.Channel,
		},
	}

	s.eventChan <- event
}

func (s *Slack) getBotname(rmsg *slack.MessageEvent) string {
	botname := ""

	if rmsg.User == "" && rmsg.BotID != "" {
		botname = rmsg.Username
		if botname == "" {
			bot, _ := s.rtm.GetBotInfo(rmsg.BotID)
			if bot.Name != "" {
				botname = bot.Name
			}
		}
	}

	return botname
}

func (s *Slack) getSlackUserFromMessage(rmsg *slack.MessageEvent) *slack.User {
	usr := rmsg.User
	if rmsg.SubType == "message_changed" {
		usr = rmsg.SubMessage.User
	}

	if rmsg.SubType == "message_deleted" {
		usr = "USLACKBOT"
	}

	suser, err := s.rtm.GetUserInfo(usr)
	if err != nil {
		if rmsg.BotID == "" {
			return nil
		}
	}

	return suser
}

func (s *Slack) sendDirectMessage(ghost *bridge.UserInfo, msg string, channelID string) {
	spoofUsername := ghost.Nick
	event := &bridge.Event{
		Type: "direct_message",
	}

	d := &bridge.DirectMessageEvent{
		Text: msg,
	}

	if !ghost.Me {
		d.Sender = ghost
	} else {
		members, _, _ := s.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: channelID})
		for _, member := range members {
			if member != s.sinfo.User.ID {
				other, _ := s.rtm.GetUserInfo(member)
				otheruser := s.createSlackUser(other)
				spoofUsername = otheruser.Nick
				break
			}
		}
	}

	d.Receiver = spoofUsername

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
	logger.Debugf("handleSlackActionPost() receiving msg %#v", rmsg)

	hasOurCallbackID := false
	if len(rmsg.Blocks.BlockSet) == 1 {
		block, ok := rmsg.Blocks.BlockSet[0].(*slack.SectionBlock)
		hasOurCallbackID = ok && block.BlockID == "matterircd_"+s.sinfo.User.ID
	}

	if rmsg.SubMessage != nil && len(rmsg.SubMessage.Blocks.BlockSet) == 1 {
		block, ok := rmsg.SubMessage.Blocks.BlockSet[0].(*slack.SectionBlock)
		hasOurCallbackID = ok && block.BlockID == "matterircd_"+s.sinfo.User.ID
	}

	if hasOurCallbackID {
		return
	}

	if rmsg.SubType == "message_deleted" {
		ts := formatTS(rmsg.DeletedTimestamp)
		rmsg.Text = "[M " + ts + "] Message deleted"
	}

	// TODO: cache userinfo
	suser := s.getSlackUserFromMessage(rmsg)

	msghandled := false

	// handle bot messages
	botname := s.getBotname(rmsg)

	// create new "ghost" user
	ghost := s.createSlackUser(suser)

	spoofUsername := ghost.Nick
	// if we have a botname, use it
	if botname != "" {
		spoofUsername = strings.TrimSpace(botname)
	}

	msgs := []string{}

	if rmsg.Text != "" {
		msgs = append(msgs, strings.Split(rmsg.Text, "\n")...)
		msghandled = true
	}

	// look in attachments
	for _, attach := range rmsg.Attachments {
		if attach.Pretext != "" {
			msgs = append(msgs, strings.Split(attach.Pretext, "\n")...)
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
			s.sendDirectMessage(ghost, msg, channelID)
		default:
			// could be a bot
			ghost.Nick = spoofUsername
			s.sendPublicMessage(ghost, msg, channelID)
		}
	}
}

func (s *Slack) createSlackUser(slackuser *slack.User) *bridge.UserInfo {
	if slackuser == nil {
		return &bridge.UserInfo{}
	}

	nick := slackuser.Name
	if (s.v.GetBool("slack.PreferNickname") || s.v.GetBool("slack.UseDisplayName")) && isValidNick(slackuser.Profile.DisplayName) {
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
		if us.ID == id && us.Profile.DisplayName != "" {
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
