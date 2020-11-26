package mattermost

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/pkg/matterclient"
	"github.com/davecgh/go-spew/spew"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mitchellh/mapstructure"
	logger "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type Mattermost struct {
	mc          *matterclient.Client
	credentials bridge.Credentials
	quitChan    []chan struct{}
	eventChan   chan *bridge.Event
	v           *viper.Viper
	connected   bool
}

func New(v *viper.Viper, cred bridge.Credentials, eventChan chan *bridge.Event, onWsConnect func()) (bridge.Bridger, *matterclient.Client, error) {
	m := &Mattermost{
		credentials: cred,
		eventChan:   eventChan,
		v:           v,
	}

	if v.GetBool("debug") {
		logger.SetLevel(logger.DebugLevel)
	}

	if v.GetBool("trace") {
		logger.SetLevel(logger.TraceLevel)
	}

	fmt.Println("loggerlevel:", logger.GetLevel())

	mc, err := m.loginToMattermost(onWsConnect)
	if err != nil {
		return nil, nil, err
	}

	if v.GetBool("debug") {
		mc.SetLogLevel("debug")
	}

	if v.GetBool("trace") {
		mc.SetLogLevel("trace")
	}

	m.mc = mc
	m.connected = true

	return m, mc, nil
}

func (m *Mattermost) loginToMattermost(onWsConnect func()) (*matterclient.Client, error) {
	mc := matterclient.New(m.credentials.Login, m.credentials.Pass, m.credentials.Team, m.credentials.Server, m.credentials.MFAToken)
	if m.v.GetBool("mattermost.Insecure") {
		mc.Credentials.NoTLS = true
	}

	// do anti idle on town-square, every installation should have this channel
	mc.AntiIdle = !m.v.GetBool("mattermost.DisableAutoView")
	mc.OnWsConnect = onWsConnect

	if m.v.GetBool("debug") {
		mc.SetLogLevel("debug")
	}

	mc.Credentials.SkipTLSVerify = m.v.GetBool("mattermost.SkipTLSVerify")

	/*
		if m.v.GetBool("debug") {
			mc.SetLogLevel("debug")
		}
	*/

	logger.Infof("login as %s (team: %s) on %s", m.credentials.Login, m.credentials.Team, m.credentials.Server)

	err := mc.Login()
	if err != nil {
		logger.Error("login failed", err)
		return nil, err
	}

	logger.Info("login succeeded")

	m.mc = mc
	m.mc.WsQuit = false

	quitChan := make(chan struct{})
	m.quitChan = append(m.quitChan, quitChan)

	go m.handleWsMessage(quitChan)

	return mc, nil
}

func (m *Mattermost) handleWsMessage(quitChan chan struct{}) {
	updateChannelsThrottle := time.NewTicker(time.Second * 60)

	for {
		if m.mc.WsQuit {
			logger.Debug("exiting handleWsMessage")
			return
		}

		logger.Debug("in handleWsMessage", len(m.mc.MessageChan))

		select {
		case <-quitChan:
			logger.Debug("exiting handleWsMessage")
			return
		case message := <-m.mc.MessageChan:
			logger.Debugf("MMUser WsReceiver: %#v", message.Raw)
			logger.Tracef("handleWsMessage %s", spew.Sdump(message))
			// check if we have the users/channels in our cache. If not update
			m.checkWsActionMessage(message.Raw, updateChannelsThrottle)

			switch message.Raw.Event {
			case model.WEBSOCKET_EVENT_POSTED:
				m.handleWsActionPost(message.Raw)
			case model.WEBSOCKET_EVENT_POST_EDITED:
				m.handleWsActionPost(message.Raw)
			case model.WEBSOCKET_EVENT_POST_DELETED:
				m.handleWsActionPost(message.Raw)
			case model.WEBSOCKET_EVENT_USER_REMOVED:
				m.handleWsActionUserRemoved(message.Raw)
			case model.WEBSOCKET_EVENT_USER_ADDED:
				m.handleWsActionUserAdded(message.Raw)
			case model.WEBSOCKET_EVENT_CHANNEL_CREATED:
				m.handleWsActionChannelCreated(message.Raw)
			case model.WEBSOCKET_EVENT_CHANNEL_DELETED:
				m.handleWsActionChannelDeleted(message.Raw)
			case model.WEBSOCKET_EVENT_USER_UPDATED:
				m.handleWsActionUserUpdated(message.Raw)
			case model.WEBSOCKET_EVENT_STATUS_CHANGE:
				m.handleStatusChangeEvent(message.Raw)
			case model.WEBSOCKET_EVENT_REACTION_ADDED, model.WEBSOCKET_EVENT_REACTION_REMOVED:
				m.handleReactionEvent(message.Raw)
			}
		}
	}
}

func (m *Mattermost) checkWsActionMessage(rmsg *model.WebSocketEvent, throttle *time.Ticker) {
	if m.GetChannelName(rmsg.Broadcast.ChannelId) != "" {
		return
	}

	select {
	case <-throttle.C:
		logger.Debugf("Updating channels for %#v", rmsg.Broadcast)
		go m.UpdateChannels()
	default:
	}
}

// antiIdle does a lastviewed every 60 seconds so that the user is shown as online instead of away
func (m *Mattermost) antiIdle(channelID string, quitChan chan struct{}) {
	ticker := time.NewTicker(time.Second * 60)

	for {
		select {
		case <-quitChan:
			logger.Debugf("stopping antiIdle loop for %s", channelID)
			return
		case <-ticker.C:
			if m.mc == nil {
				logger.Error("antiidle: don't have a connection, exiting loop.")
				return
			}

			logger.Tracef("antiIdle %s", channelID)
			m.mc.UpdateLastViewed(channelID)
		}
	}
}

func (m *Mattermost) Invite(channelID, username string) error {
	_, resp := m.mc.Client.AddChannelMember(channelID, username)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Join(channelName string) (string, string, error) {
	teamID := ""

	sp := strings.Split(channelName, "/")
	if len(sp) > 1 {
		team, _ := m.mc.Client.GetTeamByName(sp[0], "")
		if team == nil {
			return "", "", fmt.Errorf("cannot join channel (+i)")
		}

		teamID = team.Id
		channelName = sp[1]
	}

	if teamID == "" {
		teamID = m.mc.Team.ID
	}

	channelID := m.mc.GetChannelID(channelName, teamID)

	err := m.mc.JoinChannel(channelID)
	logger.Debugf("join channel %s, id %s, err: %v", channelName, channelID, err)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i)")
	}

	topic := m.mc.GetChannelHeader(channelID)

	return channelID, topic, nil
}

func (m *Mattermost) List() (map[string]string, error) {
	channelinfo := make(map[string]string)

	for _, channel := range append(m.mc.GetChannels(), m.mc.GetMoreChannels()...) {
		// FIXME: This needs to be broken up into multiple messages to fit <510 chars
		if strings.Contains(channel.Name, "__") {
			continue
		}

		channelName := "#" + channel.Name
		// prefix channels outside of our team with team name
		if channel.TeamId != m.mc.Team.ID {
			channelName = m.mc.GetTeamName(channel.TeamId) + "/" + channel.Name
		}

		channelinfo[channelName] = strings.ReplaceAll(channel.Header, "\n", " | ")
	}

	return channelinfo, nil
}

func (m *Mattermost) Part(channelID string) error {
	m.mc.Client.RemoveUserFromChannel(channelID, m.mc.User.Id)

	return nil
}

func (m *Mattermost) UpdateChannels() error {
	return m.mc.UpdateChannels()
}

func (m *Mattermost) Logout() error {
	if m.mc.WsClient != nil {
		err := m.mc.Logout()
		if err != nil {
			logger.Error("logout failed")
		}
		logger.Info("logout succeeded")

		m.eventChan <- &bridge.Event{
			Type: "logout",
			Data: &bridge.LogoutEvent{},
		}

		m.mc.WsQuit = true

		for _, c := range m.quitChan {
			c <- struct{}{}
		}
	}

	m.connected = false

	return nil
}

func (m *Mattermost) MsgUser(userID, text string) (string, error) {
	return m.MsgUserThread(userID, "", text)
}

func (m *Mattermost) MsgUserThread(userID, parentID, text string) (string, error) {
	props := make(map[string]interface{})

	props["matterircd_"+m.mc.User.Id] = true

	// create DM channel (only happens on first message)
	dchannel, resp := m.mc.Client.CreateDirectChannel(m.mc.User.Id, userID)
	if resp.Error != nil {
		return "", resp.Error
	}

	// build & send the message
	text = strings.ReplaceAll(text, "\r", "")
	post := &model.Post{
		ChannelId: dchannel.Id,
		Message:   text,
		RootId:    parentID,
	}

	post.SetProps(props)

	rp, resp := m.mc.Client.CreatePost(post)

	if resp.Error != nil {
		return "", resp.Error
	}

	return rp.Id, nil
}

func (m *Mattermost) MsgChannel(channelID, text string) (string, error) {
	return m.MsgChannelThread(channelID, "", text)
}

func (m *Mattermost) MsgChannelThread(channelID, parentID, text string) (string, error) {
	props := make(map[string]interface{})
	props["matterircd_"+m.mc.User.Id] = true

	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    parentID,
	}

	post.SetProps(props)

	rp, resp := m.mc.Client.CreatePost(post)

	if resp.Error != nil {
		return "", resp.Error
	}

	return rp.Id, nil
}

func (m *Mattermost) ModifyPost(msgID, text string) error {
	if text == "" {
		_, resp := m.mc.Client.DeletePost(msgID)
		if resp.Error != nil {
			return resp.Error
		}

		return nil
	}

	_, resp := m.mc.Client.PatchPost(msgID, &model.PostPatch{
		Message: &text,
	})

	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Topic(channelID string) string {
	return m.mc.GetChannelHeader(channelID)
}

func (m *Mattermost) SetTopic(channelID, text string) error {
	logger.Debugf("updating channelheader %#v, %#v", channelID, text)
	patch := &model.ChannelPatch{
		Header: &text,
	}

	_, resp := m.mc.Client.PatchChannel(channelID, patch)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) StatusUser(userID string) (string, error) {
	return m.mc.GetStatus(userID), nil
}

func (m *Mattermost) StatusUsers() (map[string]string, error) {
	return m.mc.GetStatuses(), nil
}

func (m *Mattermost) Protocol() string {
	return "mattermost"
}

func (m *Mattermost) Kick(channelID, username string) error {
	_, resp := m.mc.Client.RemoveUserFromChannel(channelID, username)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) SetStatus(status string) error {
	_, resp := m.mc.Client.UpdateUserStatus(m.mc.User.Id, &model.Status{
		Status: status,
		UserId: m.mc.User.Id,
	})
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Mattermost) Nick(name string) error {
	return m.mc.UpdateUserNick(name)
}

func (m *Mattermost) GetChannelName(channelID string) string {
	var name string

	channelName := m.mc.GetChannelName(channelID)

	if channelName == "" {
		m.mc.UpdateChannels()
	}

	channelName = m.mc.GetChannelName(channelID)

	// return DM channels immediately
	if strings.Contains(channelName, "__") {
		return channelName
	}

	teamID := m.mc.GetTeamFromChannel(channelID)
	teamName := m.mc.GetTeamName(teamID)

	if channelName != "" {
		if (teamName != "" && teamID != m.mc.Team.ID) || m.v.GetBool("mattermost.PrefixMainTeam") {
			name = "#" + teamName + "/" + channelName
		}
		if teamID == m.mc.Team.ID && !m.v.GetBool("mattermost.PrefixMainTeam") {
			name = "#" + channelName
		}
		if teamID == "G" {
			name = "#" + channelName
		}
	} else {
		name = channelID
	}

	return name
}

func (m *Mattermost) GetChannelUsers(channelID string) ([]*bridge.UserInfo, error) {
	var (
		mmusers, mmusersPaged []*model.User
		users                 []*bridge.UserInfo
		resp                  *model.Response
	)

	idx := 0
	max := 200

	for {
		mmusersPaged, resp = m.mc.Client.GetUsersInChannel(channelID, idx, max, "")
		if resp.Error == nil {
			break
		}

		if err := m.mc.HandleRatelimit("GetUsersInChannel", resp); err != nil {
			return nil, err
		}
	}

	for len(mmusersPaged) > 0 {
		for {
			mmusersPaged, resp = m.mc.Client.GetUsersInChannel(channelID, idx, max, "")
			if resp.Error == nil {
				idx++
				time.Sleep(time.Millisecond * 200)
				mmusers = append(mmusers, mmusersPaged...)

				break
			}

			if err := m.mc.HandleRatelimit("GetUsersInChannel", resp); err != nil {
				return nil, err
			}
		}
	}

	for _, mmuser := range mmusers {
		users = append(users, m.createUser(mmuser))
	}

	return users, nil
}

func (m *Mattermost) GetUsers() []*bridge.UserInfo {
	var users []*bridge.UserInfo

	for _, mmuser := range m.mc.GetUsers() {
		users = append(users, m.createUser(mmuser))
	}

	return users
}

func (m *Mattermost) GetChannels() []*bridge.ChannelInfo {
	var channels []*bridge.ChannelInfo

	chanMap := make(map[string]bool)

	for _, mmchannel := range m.mc.GetChannels() {
		// don't add the same channel twice
		// the same direct messages channels get listed for each team
		if chanMap[mmchannel.Id] {
			continue
		}

		channels = append(channels, &bridge.ChannelInfo{
			Name:    mmchannel.Name,
			ID:      mmchannel.Id,
			TeamID:  mmchannel.TeamId,
			DM:      mmchannel.IsGroupOrDirect(),
			Private: !mmchannel.IsOpen(),
		})

		chanMap[mmchannel.Id] = true
	}

	return channels
}

func (m *Mattermost) GetChannel(channelID string) (*bridge.ChannelInfo, error) {
	for _, channel := range m.GetChannels() {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	m.UpdateChannels()

	for _, channel := range m.GetChannels() {
		if channel.ID == channelID {
			return channel, nil
		}
	}

	return nil, errors.New("channel not found")
}

func (m *Mattermost) GetUser(userID string) *bridge.UserInfo {
	return m.createUser(m.mc.GetUser(userID))
}

func (m *Mattermost) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.User)
}

func (m *Mattermost) GetUserByUsername(username string) *bridge.UserInfo {
	for {
		mmuser, resp := m.mc.Client.GetUserByUsername(username, "")
		if resp.Error == nil {
			return m.createUser(mmuser)
		}

		if err := m.mc.HandleRatelimit("GetUserByUsername", resp); err != nil {
			return &bridge.UserInfo{}
		}
	}
}

func (m *Mattermost) createUser(mmuser *model.User) *bridge.UserInfo {
	teamID := ""

	if mmuser == nil {
		return &bridge.UserInfo{}
	}

	nick := mmuser.Username
	if m.v.GetBool("mattermost.PreferNickname") && isValidNick(mmuser.Nickname) {
		nick = mmuser.Nickname
	}

	me := false

	if mmuser.Id == m.mc.User.Id {
		me = true
		teamID = m.mc.Team.ID
	}

	mentionkeys := mmuser.NotifyProps["mention_keys"]

	info := &bridge.UserInfo{
		Nick:        nick,
		User:        mmuser.Id,
		Real:        mmuser.FirstName + " " + mmuser.LastName,
		Host:        m.mc.Client.Url,
		Roles:       mmuser.Roles,
		Ghost:       true,
		Me:          me,
		TeamID:      teamID,
		Username:    mmuser.Username,
		FirstName:   mmuser.FirstName,
		LastName:    mmuser.LastName,
		MentionKeys: strings.Split(mentionkeys, ","),
	}

	return info
}

func isValidNick(s string) bool {
	/* IRC RFC ([0] - see below) mentions a limit of 9 chars for
	 * IRC nicks, but modern clients allow more than that. Let's
	 * use a "sane" big value, the triple of the spec.
	 */
	if len(s) < 1 || len(s) > 27 {
		return false
	}

	/* According to IRC RFC [0], the allowed chars to have as nick
	 * are: ( letter / special-'-' ).*( letter / digit / special ),
	 * where:
	 * letter = [a-z / A-Z]; digit = [0-9];
	 * special = [';', '[', '\', ']', '^', '_', '`', '{', '|', '}', '-']
	 *
	 * ASCII codes (decimal) for the allowed chars:
	 * letter = [65-90,97-122]; digit = [48-57]
	 * special = [59, 91-96, 123-125, 45]
	 * [0] RFC 2812 (tools.ietf.org/html/rfc2812)
	 */

	if s[0] != 59 && (s[0] < 65 || s[0] > 125) {
		return false
	}

	for i := 1; i < len(s); i++ {
		if s[i] != 45 && s[i] != 59 && (s[i] < 65 || s[i] > 125) {
			if s[i] < 48 || s[i] > 57 {
				return false
			}
		}
	}

	return true
}

func (m *Mattermost) wsActionPostSkip(rmsg *model.WebSocketEvent) bool {
	data := model.PostFromJson(strings.NewReader(rmsg.Data["post"].(string)))
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Data["post"].(string)))["props"].(map[string]interface{})

	if rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED && data.HasReactions {
		logger.Debugf("edit post with reactions, do not relay. We don't know if a reaction is added or the post has been edited")
		return true
	}

	if data.UserId == m.GetMe().User {
		if _, ok := extraProps["matterircd_"+m.GetMe().User].(bool); ok {
			logger.Debugf("message is sent from matterirc, not relaying %#v", data.Message)
			return true
		}

		if data.Type == model.POST_JOIN_LEAVE || data.Type == model.POST_JOIN_CHANNEL {
			logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
			return true
		}
	}

	return false
}

// maybeShorten returns a prefix of msg that is approximately newLen
// characters long, followed by "...".  Words that start with uncounted
// are included in the result but are not reckoned against newLen.
func maybeShorten(msg string, newLen int, uncounted string) string {
	if newLen == 0 || len(msg) < newLen {
		return msg
	}
	newMsg := ""
	for _, word := range strings.Split(strings.ReplaceAll(msg, "\n", " "), " ") {
		if newMsg == "" {
			newMsg = word
			continue
		}
		if len(newMsg) < newLen {
			skipped := false
			if uncounted != "" && strings.HasPrefix(word, uncounted) {
				newLen += len(word) + 1
				skipped = true
			}
			// Truncate very long words, but only if they were not skipped, on the
			// assumption that such words are important enough to be preserved whole.
			if !skipped && len(word) > newLen {
				word = fmt.Sprintf("%s[...]", word[0:(newLen*2/3)])
			}
			newMsg = fmt.Sprintf("%s %s", newMsg, word)
			continue
		}
		break
	}

	return fmt.Sprintf("%s ...", newMsg)
}

// nolint:funlen,gocognit,gocyclo
func (m *Mattermost) handleWsActionPost(rmsg *model.WebSocketEvent) {
	data := model.PostFromJson(strings.NewReader(rmsg.Data["post"].(string)))
	props := rmsg.Data
	extraProps := model.StringInterfaceFromJson(strings.NewReader(rmsg.Data["post"].(string)))["props"].(map[string]interface{})

	logger.Debugf("handleWsActionPost() receiving userid %s", data.UserId)
	if m.wsActionPostSkip(rmsg) {
		return
	}

	// nolint:nestif
	if data.ParentId != "" {
		parentPost, resp := m.mc.Client.GetPost(data.ParentId, "")
		if resp.Error != nil {
			logger.Errorf("Unable to get parent post for %#v", data)
		} else {
			parentGhost := m.GetUser(parentPost.UserId)
			// Include parent userid / IRC nicks so hilights still work when people reply to our messages.
			if m.v.GetBool("mattermost.HideReplies") || m.v.GetBool("mattermost.prefixContext") || m.v.GetBool("mattermost.suffixContext") {
				data.Message = fmt.Sprintf("%s (re @%s)", data.Message, parentGhost.Nick)
			} else {
				parentMessage := maybeShorten(parentPost.Message, m.v.GetInt("mattermost.ShortenRepliesTo"), "@")
				data.Message = fmt.Sprintf("%s (re @%s: %s)", data.Message, parentGhost.Nick, parentMessage)
			}
		}
	}

	// create new "ghost" user
	ghost := m.GetUser(data.UserId)
	// our own message, set our IRC self as user, not our mattermost self
	if data.UserId == m.GetMe().User {
		ghost = m.GetMe()
	}

	if ghost == nil {
		ghost = &bridge.UserInfo{
			Nick: data.UserId,
		}
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
		logger.Debugf("found override username %s", overrideUsername)
		// only allow valid irc nicks
		re := regexp.MustCompile("^[a-zA-Z0-9_]*$")
		if re.MatchString(overrideUsername) {
			ghost.Nick = overrideUsername
			ghost.Me = false
		}
	}

	if data.Type == model.POST_JOIN_LEAVE || data.Type == "system_leave_channel" ||
		data.Type == "system_join_channel" || data.Type == "system_add_to_channel" ||
		data.Type == "system_remove_from_channel" {
		logger.Debugf("join/leave message. not relaying %#v", data.Message)
		m.UpdateChannels()

		m.wsActionPostJoinLeave(data, extraProps)
		return
	}

	if data.Type == "system_header_change" {
		if topic, ok := extraProps["new_header"].(string); ok {
			event := &bridge.Event{
				Type: "channel_topic",
				Data: &bridge.ChannelTopicEvent{
					Text:      topic,
					ChannelID: data.ChannelId,
					UserID:    data.UserId,
				},
			}
			m.eventChan <- event
		}

		return
	}

	msgs := strings.Split(data.Message, "\n")

	channelType := ""
	if t, ok := props["channel_type"].(string); ok {
		channelType = t
	}

	dmchannel, _ := rmsg.Data["channel_name"].(string)

	// add an edited/deleted string when messages are edited/deleted
	if len(msgs) > 0 && (rmsg.Event == model.WEBSOCKET_EVENT_POST_EDITED ||
		rmsg.Event == model.WEBSOCKET_EVENT_POST_DELETED) {
		postfix := " (edited)"

		if rmsg.Event == model.WEBSOCKET_EVENT_POST_DELETED {
			postfix = " (deleted)"
		}

		msgs[len(msgs)-1] = msgs[len(msgs)-1] + postfix

		// check if we have an edited direct message (channels have __)
		name := m.GetChannelName(data.ChannelId)
		if strings.Contains(name, "__") {
			channelType = "D"
		}
		dmchannel = name
	}

	for _, msg := range msgs {
		if msg == "" {
			continue
		}

		switch {
		// DirectMessage
		case channelType == "D":
			event := &bridge.Event{
				Type: "direct_message",
			}

			if data.Type == "me" {
				msg = strings.TrimLeft(msg, "*")
				msg = strings.TrimRight(msg, "*")
				msg = "\x01ACTION " + msg + " \x01"
			}

			d := &bridge.DirectMessageEvent{
				Text:      msg,
				Files:     m.getFilesFromData(data),
				ChannelID: data.ChannelId,
				MessageID: data.Id,
				Event:     rmsg.Event,
				ParentID:  data.ParentId,
			}

			if ghost.Me {
				d.Sender = ghost
				d.Receiver = m.getDMUser(dmchannel)
			} else {
				d.Sender = m.getDMUser(dmchannel)
				d.Receiver = ghost
			}

			if d.Sender == nil || d.Receiver == nil {
				logger.Errorf("dm: couldn't resolve sender or receiver: %#v", rmsg)
				return
			}

			event.Data = d

			m.eventChan <- event

			if data.Type == "me" {
				break
			}
		case strings.Contains(data.Message, "@channel") || strings.Contains(data.Message, "@here") ||
			strings.Contains(data.Message, "@all"):
			event := &bridge.Event{
				Type: "channel_message",
				Data: &bridge.ChannelMessageEvent{
					Text:        msg,
					ChannelID:   data.ChannelId,
					Sender:      ghost,
					MessageType: "notice",
					ChannelType: channelType,
					Files:       m.getFilesFromData(data),
					MessageID:   data.Id,
					Event:       rmsg.Event,
					ParentID:    data.ParentId,
				},
			}

			m.eventChan <- event
		default:
			if data.Type == "me" {
				msg = strings.TrimLeft(msg, "*")
				msg = strings.TrimRight(msg, "*")
				msg = "\x01ACTION " + msg + " \x01"
			}

			event := &bridge.Event{
				Type: "channel_message",
				Data: &bridge.ChannelMessageEvent{
					Text:        msg,
					ChannelID:   data.ChannelId,
					Sender:      ghost,
					ChannelType: channelType,
					Files:       m.getFilesFromData(data),
					MessageID:   data.Id,
					Event:       rmsg.Event,
					ParentID:    data.ParentId,
				},
			}

			m.eventChan <- event

			if data.Type == "me" {
				break
			}
		}
	}

	m.handleFileEvent(channelType, ghost, data, rmsg)

	logger.Debugf("handleWsActionPost() user %s sent %s", m.mc.GetUser(data.UserId).Username, data.Message)
	logger.Debugf("%#v", data)
}

func (m *Mattermost) getFilesFromData(data *model.Post) []*bridge.File {
	files := []*bridge.File{}

	for _, fname := range m.mc.GetFileLinks(data.FileIds) {
		files = append(files, &bridge.File{
			Name: fname,
		})
	}

	return files
}

func (m *Mattermost) handleFileEvent(channelType string, ghost *bridge.UserInfo, data *model.Post, rmsg *model.WebSocketEvent) {
	event := &bridge.Event{
		Type: "file_event",
	}

	fileEvent := &bridge.FileEvent{
		Sender:      ghost,
		Receiver:    ghost,
		ChannelType: channelType,
		ChannelID:   data.ChannelId,
	}

	event.Data = fileEvent

	for _, fname := range m.getFilesFromData(data) {
		fileEvent.Files = append(fileEvent.Files, &bridge.File{
			Name: fname.Name,
		})
	}

	if len(fileEvent.Files) > 0 {
		switch {
		case channelType == "D":
			if ghost.Me {
				fileEvent.Sender = ghost
				fileEvent.Receiver = m.getDMUser(rmsg.Data["channel_name"])
			} else {
				fileEvent.Sender = m.getDMUser(rmsg.Data["channel_name"])
				fileEvent.Receiver = ghost
			}

			if fileEvent.Sender == nil || fileEvent.Receiver == nil {
				logger.Errorf("filedm: couldn't resolve sender or receiver: %#v", rmsg)
				return
			}

			m.eventChan <- event
		default:
			m.eventChan <- event
		}
	}
}

func (m *Mattermost) wsActionPostJoinLeave(data *model.Post, extraProps map[string]interface{}) {
	logger.Debugf("wsActionPostJoinLeave: extraProps: %#v", extraProps)
	switch data.Type {
	case "system_add_to_channel":
		if added, ok := extraProps["addedUsername"].(string); ok {
			if adder, ok := extraProps["username"].(string); ok {
				event := &bridge.Event{
					Type: "channel_add",
					Data: &bridge.ChannelAddEvent{
						Added: []*bridge.UserInfo{
							m.GetUserByUsername(added),
						},
						Adder:     m.GetUserByUsername(adder),
						ChannelID: data.ChannelId,
					},
				}

				m.eventChan <- event
			}
		}
	case "system_remove_from_channel":
		if removed, ok := extraProps["removedUsername"].(string); ok {
			event := &bridge.Event{
				Type: "channel_remove",
				Data: &bridge.ChannelRemoveEvent{
					Removed: []*bridge.UserInfo{
						m.GetUserByUsername(removed),
					},
					ChannelID: data.ChannelId,
				},
			}

			m.eventChan <- event
		}
	}
}

func (m *Mattermost) handleWsActionUserAdded(rmsg *model.WebSocketEvent) {
	userID, ok := rmsg.Data["user_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_add",
		Data: &bridge.ChannelAddEvent{
			Added: []*bridge.UserInfo{
				m.GetUser(userID),
			},
			Adder: &bridge.UserInfo{
				Nick: "system",
			},
			ChannelID: rmsg.Broadcast.ChannelId,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserRemoved(rmsg *model.WebSocketEvent) {
	userID, ok := rmsg.Data["user_id"].(string)
	if !ok {
		userID = rmsg.Broadcast.UserId
	}

	removerID, ok := rmsg.Data["remover_id"].(string)
	if !ok {
		fmt.Println("not ok removerID", removerID)
		return
	}

	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		channelID = rmsg.Broadcast.ChannelId
	}

	event := &bridge.Event{
		Type: "channel_remove",
		Data: &bridge.ChannelRemoveEvent{
			Remover: m.GetUser(removerID),
			Removed: []*bridge.UserInfo{
				m.GetUser(userID),
			},
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserUpdated(rmsg *model.WebSocketEvent) {
	var info model.User

	err := Decode(rmsg.Data["user"], &info)
	if err != nil {
		fmt.Println("decode", err)
		return
	}

	event := &bridge.Event{
		Type: "user_updated",
		Data: &bridge.UserUpdateEvent{
			User: m.createUser(&info),
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelCreated(rmsg *model.WebSocketEvent) {
	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_create",
		Data: &bridge.ChannelCreateEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionChannelDeleted(rmsg *model.WebSocketEvent) {
	channelID, ok := rmsg.Data["channel_id"].(string)
	if !ok {
		return
	}

	event := &bridge.Event{
		Type: "channel_delete",
		Data: &bridge.ChannelDeleteEvent{
			ChannelID: channelID,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleStatusChangeEvent(rmsg *model.WebSocketEvent) {
	var info model.Status

	err := Decode(rmsg.Data, &info)
	if err != nil {
		fmt.Println("decode", err)

		return
	}

	event := &bridge.Event{
		Type: "status_change",
		Data: &bridge.StatusChangeEvent{
			UserID: info.UserId,
			Status: info.Status,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleReactionEvent(rmsg *model.WebSocketEvent) {
	reaction := model.ReactionFromJson(strings.NewReader(rmsg.Data["reaction"].(string)))

	var event *bridge.Event

	channelType := ""

	name := m.GetChannelName(rmsg.Broadcast.ChannelId)
	if strings.Contains(name, "__") {
		channelType = "D"
	}

	switch rmsg.Event {
	case model.WEBSOCKET_EVENT_REACTION_ADDED:
		event = &bridge.Event{
			Type: "reaction_add",
			Data: &bridge.ReactionAddEvent{
				ChannelID:   rmsg.Broadcast.ChannelId,
				MessageID:   reaction.PostId,
				Sender:      m.GetUser(reaction.UserId),
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
			},
		}
	case model.WEBSOCKET_EVENT_REACTION_REMOVED:
		event = &bridge.Event{
			Type: "reaction_remove",
			Data: &bridge.ReactionRemoveEvent{
				ChannelID:   rmsg.Broadcast.ChannelId,
				MessageID:   reaction.PostId,
				Sender:      m.GetUser(reaction.UserId),
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
			},
		}
	}

	m.eventChan <- event
}

func (m *Mattermost) GetTeamName(teamID string) string {
	return m.mc.GetTeamName(teamID)
}

func (m *Mattermost) GetLastViewedAt(channelID string) int64 {
	x := m.mc.GetLastViewedAt(channelID)
	logger.Tracef("getLastViewedAt %s: %#v", channelID, x)

	return x
}

func (m *Mattermost) GetPostsSince(channelID string, since int64) interface{} {
	return m.mc.GetPostsSince(channelID, since)
}

func (m *Mattermost) UpdateLastViewed(channelID string) {
	logger.Tracef("Updatelastviewed %s", channelID)
	err := m.mc.UpdateLastViewed(channelID)
	if err != nil {
		logger.Errorf("updateLastViewed failed: %s", err)
	}
}

func (m *Mattermost) UpdateLastViewedUser(userID string) error {
	for {
		dc, resp := m.mc.Client.CreateDirectChannel(m.mc.User.Id, userID)
		if resp.Error == nil {
			return m.mc.UpdateLastViewed(dc.Id)
		}

		if err := m.mc.HandleRatelimit("CreateDirectChannel", resp); err != nil {
			return err
		}
	}
}

func (m *Mattermost) SearchPosts(search string) interface{} {
	return m.mc.SearchPosts(search)
}

func (m *Mattermost) GetFileLinks(fileIDs []string) []string {
	return m.mc.GetFileLinks(fileIDs)
}

func (m *Mattermost) SearchUsers(query string) ([]*bridge.UserInfo, error) {
	users, resp := m.mc.Client.SearchUsers(&model.UserSearch{Term: query})
	if resp.Error != nil {
		return nil, resp.Error
	}

	var brusers []*bridge.UserInfo

	for _, u := range users {
		brusers = append(brusers, m.createUser(u))
	}

	return brusers, nil
}

func (m *Mattermost) GetPosts(channelID string, limit int) interface{} {
	return m.mc.GetPosts(channelID, limit)
}

func (m *Mattermost) GetChannelID(name, teamID string) string {
	return m.mc.GetChannelID(name, teamID)
}

func (m *Mattermost) Connected() bool {
	return m.connected
}

func Decode(input interface{}, output interface{}) error {
	config := &mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   output,
		TagName:  "json",
	}

	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}

	return decoder.Decode(input)
}

func (m *Mattermost) getDMUser(name interface{}) *bridge.UserInfo {
	if channel, ok := name.(string); ok {
		channelmembers := strings.Split(channel, "__")
		if len(channelmembers) != 2 {
			logger.Errorf("not a DM message, incorrect channelID: %s", channel)
			return nil
		}

		// ourself
		if channelmembers[0] == channelmembers[1] {
			return m.createUser(m.mc.User)
		}

		otheruser := m.GetUser(channelmembers[1])
		if channelmembers[1] == m.mc.User.Id {
			otheruser = m.GetUser(channelmembers[0])
		}

		return otheruser
	}

	return nil
}
