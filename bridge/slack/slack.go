package slack

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/davecgh/go-spew/spew"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/slackclient"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

type Slack struct {
	sync.RWMutex

	sc          *slackclient.Client
	connected   bool
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	onConnect   func()

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
	// Set initial log level
	configureBridgeLogging(ourlog, sc, rc)

	// Register hook to update live on reloads
	cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
		configureBridgeLogging(ourlog, sc, newRC)
	})

	s.sc = sc
	s.sc.InitUsers()

	return s, nil
}

func configureBridgeLogging(ourlog *logrus.Logger, sc *slackclient.Client, rc *config.RuntimeConfig) {
	switch {
	case rc.Trace:
		ourlog.SetLevel(logrus.TraceLevel)
		sc.SetLogLevel("trace")
	case rc.Debug:
		ourlog.SetLevel(logrus.DebugLevel)
		sc.SetLogLevel("debug")
	default:
		ourlog.SetLevel(logrus.InfoLevel)
		sc.SetLogLevel("info")
	}

	sc.SetLogAPICalls("error")

	if rc.Profiling {
		if rc.Trace {
			logger.Infof("enabling slackclient API logging: level: trace")
			sc.SetLogAPICalls("trace")
		} else if rc.Debug {
			logger.Infof("enabling slackclient API logging: level: warn")
			sc.SetLogAPICalls("warn")
		}
	}
}

func (s *Slack) Invite(ctx context.Context, channelID, username string) error {
	return s.sc.Invite(channelID, username)
}

func (s *Slack) IsChannelMember(channelID string) bool {
	return s.sc.IsChannelMember(channelID)
}

func (s *Slack) Join(ctx context.Context, channelName string) (string, string, error) {
	return s.sc.JoinChannel(channelName)
}

func (s *Slack) List(ctx context.Context) (map[string]string, error) {
	channelinfo := make(map[string]string)

	conversations, _ := s.sc.GetConversations()
	for _, channel := range conversations {
		channelinfo["#"+channel.Name] = strings.ReplaceAll(channel.Topic.Value, "\n", " | ")
	}

	return channelinfo, nil
}

func (s *Slack) Part(ctx context.Context, channelID string) error {
	return s.sc.LeaveChannel(channelID)
}

func (s *Slack) UpdateChannels(ctx context.Context) error {
	return nil
}

func (s *Slack) Logout(ctx context.Context) error {
	err := s.sc.Logout()
	if err != nil {
		return err
	}

	// The event loop may never have started, or it ended already when the user disconnected
	// (ctx is canceled then), so don't wait for it forever.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	select {
	case s.eventChan <- &bridge.Event{Type: "logout", Data: &bridge.LogoutEvent{}}:
	case <-ctx.Done():
	case <-timer.C:
	}

	s.connected = false

	return nil
}

func (s *Slack) MsgUser(ctx context.Context, username, text string) (string, error) {
	// CTCP ACTION (/me)
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		text = after
		text = strings.TrimSuffix(text, "\x01")
		text = "*" + text + "*"
	}

	return s.sc.SendDirectMessage(username, text)
}

func (s *Slack) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	// CTCP ACTION (/me)
	if after, ok := strings.CutPrefix(text, "\x01ACTION "); ok {
		text = after
		text = strings.TrimSuffix(text, "\x01")
		text = "*" + text + "*"
	}

	return s.sc.PostMessage(channelID, text)
}

func (s *Slack) Topic(ctx context.Context, channelID string) string {
	return s.sc.Topic(channelID)
}

func (s *Slack) SetTopic(ctx context.Context, channelID, text string) error {
	return s.sc.SetTopic(channelID, text)
}

func (s *Slack) HasCustomStatus(userID string) bool {
	return false
}

func (s *Slack) IsOnline(status string) bool {
	return status == "active"
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
	return s.sc.Kick(channelID, username)
}

func (s *Slack) SetStatus(ctx context.Context, status string) error {
	return s.sc.SetStatus(status)
}

func (s *Slack) Nick(ctx context.Context, name string) error {
	return nil
}

func (s *Slack) GetChannelName(ctx context.Context, channelID string) string {
	return s.sc.GetChannelName(channelID)
}

func (s *Slack) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	susers, err := s.sc.GetChannelUsers(channelID)
	if err != nil {
		return nil, err
	}

	users := make([]*bridge.UserInfo, 0, len(susers)+2)
	for _, suser := range susers {
		users = append(users, s.createUser(suser))
	}

	// Add slackbot to all channels
	slackuser := s.sc.GetUser("USLACKBOT")
	users = append(users, s.createUser(slackuser), s.GetMe())

	return users, nil
}

func (s *Slack) GetDMChannelID(ctx context.Context, userID string) string {
	return ""
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
	susers := s.sc.GetUsers()
	users := make([]*bridge.UserInfo, 0, len(susers))

	for i := range susers {
		users = append(users, s.createUser(&susers[i]))
	}

	return users
}

func (s *Slack) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	teamID := s.sc.TeamID()

	for _, mmchannel := range s.sc.GetChannels() {
		channels = append(channels, &bridge.ChannelInfo{
			Name:    mmchannel.Name,
			ID:      mmchannel.ID,
			TeamID:  teamID,
			DM:      mmchannel.IsIM || mmchannel.IsMpIM,
			Private: !mmchannel.IsOpen,
		})
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
	return s.createUser(s.sc.GetUser(userID))
}

func (s *Slack) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	return []string{}, nil
}

func (s *Slack) GetUserCount() int {
	return 0
}

func (s *Slack) GetMe() *bridge.UserInfo {
	return s.createUser(s.sc.GetMe())
}

func (s *Slack) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (s *Slack) GetTeamName(ctx context.Context, teamID string) string {
	return s.sc.TeamName()
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
		ok := slices.Contains(rc.Slack.Bridge.Restrict, s.sc.TeamDomain())
		if !ok {
			_ = s.sc.Disconnect()
			return errors.New("Not allowed to connect to " + s.sc.TeamDomain() + " slack")
		}
	}
	// we only know which user we are when we actually are connected.
	// disconnect if we're not allowed
	if len(rc.Slack.DenyUsers) > 0 {
		ok := slices.Contains(rc.Slack.DenyUsers, s.sc.CurrentUserName())
		if ok {
			_ = s.sc.Disconnect()
			return errors.New("not allowed to connect")
		}
	}

	return nil
}

func (s *Slack) loginToSlack() (*slackclient.Client, error) {
	rc := s.cfg.Current()

	sc := slackclient.New(slackclient.Credentials{
		Login: s.credentials.Login,
		Team:  s.credentials.Team,
		Pass:  s.credentials.Pass,
		Token: s.credentials.Token,
	})

	if rc.Debug {
		sc.SetLogLevel("debug")
		sc.SetLogAPICalls("warn")
	}

	if rc.Trace {
		sc.SetLogLevel("trace")
		sc.SetLogAPICalls("trace")
	}

	err := sc.Login()
	if err != nil {
		return nil, err
	}

	s.sc = sc

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
	for msg := range s.sc.IncomingEvents() {
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
	suser, err := s.sc.GetUserInfo(userID)
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
			members, _ := s.sc.GetUsersInConversation(channelID)
			for _, member := range members {
				if member == s.GetMe().User {
					continue
				}

				ghostuser, _ := s.sc.GetUserInfo(member)
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

//nolint:funlen,gocognit,gocyclo
func (s *Slack) handleSlackActionPost(rmsg *slack.MessageEvent) {
	logger.Debugf("handleSlackActionPost: receiving msg %#v", rmsg)

	// Is this our own message
	if s.sc.IsOwnMessageEcho(rmsg.User, rmsg.Channel, rmsg.Timestamp) {
		return
	}

	if rmsg.SubType == "message_deleted" {
		ts := formatTS(rmsg.DeletedTimestamp)
		rmsg.Text = "[M " + ts + "] Message deleted"
	}

	// TODO: cache userinfo
	suser, err := s.sc.GetUserFromMessage(rmsg)
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
				members, _ := s.sc.GetUsersInConversation(channelID)
				for _, member := range members {
					if member == s.GetMe().User {
						continue
					}

					ghostuser, _ := s.sc.GetUserInfo(member)
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

	me := slackuser.ID == s.sc.UserID()

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
		TeamID:      s.sc.TeamID(),
	}

	return info
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

func (s *Slack) GetPostChannelID(ctx context.Context, postID string) string {
	return ""
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

func (s *Slack) NormalizePostID(msgID string) string {
	return msgID
}

func (s *Slack) SendTyping(ctx context.Context, channelName string) {
}
