package mattermost

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/utils"
	"github.com/davecgh/go-spew/spew"
	lru "github.com/hashicorp/golang-lru"
	"github.com/kenshaw/emoji"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/matterbridge/matterclient"
	"github.com/mattermost/mattermost-server/v6/model"
	"github.com/mitchellh/mapstructure"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type Mattermost struct {
	mc          *matterclient.Client
	credentials bridge.Credentials
	quitChan    []chan struct{}
	eventChan   chan *bridge.Event
	v           *viper.Viper
	connected   bool
	instanceTag string

	msgParentCache   *lru.Cache
	msgLastSentCache *lru.Cache
}

var logger *logrus.Entry

func New(v *viper.Viper, cred bridge.Credentials, eventChan chan *bridge.Event, onWsConnect func()) (bridge.Bridger, *matterclient.Client, error) {
	m := &Mattermost{
		credentials: cred,
		eventChan:   eventChan,
		v:           v,
	}
	m.msgParentCache, _ = lru.New(100)
	m.msgLastSentCache, _ = lru.New(10)

	ourlog := logrus.New()
	ourlog.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	logger = ourlog.WithFields(logrus.Fields{"prefix": "bridge/mattermost"})
	if v.GetBool("debug") {
		ourlog.SetLevel(logrus.DebugLevel)
	}

	if v.GetBool("trace") {
		ourlog.SetLevel(logrus.TraceLevel)
	}

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

	// Create a unique matterircd instance tag so we don't relay messages sent from it.
	charset := []byte("abcdefghijklmnopqrstuvwxyz")
	b := make([]byte, 8)
	r := rand.New(rand.NewSource(time.Now().UnixNano())) //nolint:gosec
	for i := range b {
		b[i] = charset[r.Intn(len(charset))]
	}
	m.instanceTag = string(b)

	return m, mc, nil
}

func (m *Mattermost) loginToMattermost(onWsConnect func()) (*matterclient.Client, error) {
	matterclient.Matterircd = true

	mc := matterclient.New(m.credentials.Login, m.credentials.Pass, m.credentials.Team, m.credentials.Server, m.credentials.MFAToken)
	if m.v.GetBool("mattermost.Insecure") {
		mc.Credentials.NoTLS = true
	}

	mc.AntiIdle = !m.v.GetBool("mattermost.DisableAutoView") || m.v.GetBool("mattermost.ForceAntiIdle")
	mc.AntiIdleChan = m.v.GetString("mattermost.AntiIdleChannel")
	mc.AntiIdleIntvl = m.v.GetInt("mattermost.AntiIdleInterval")
	mc.OnWsConnect = onWsConnect

	mc.Timeout = m.v.GetInt("ClientTimeout")
	if mc.Timeout == 0 {
		mc.Timeout = 10
	}

	if m.v.GetBool("debug") {
		mc.SetLogLevel("debug")
	}

	mc.Credentials.SkipTLSVerify = m.v.GetBool("mattermost.SkipTLSVerify")

	logger.Infof("login as %s (team: %s) on %s", m.credentials.Login, m.credentials.Team, m.credentials.Server)

	if err := mc.Login(); err != nil {
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

//nolint:cyclop
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

			switch message.Raw.EventType() {
			case model.WebsocketEventPosted:
				m.handleWsActionPost(message.Raw)
			case model.WebsocketEventPostEdited:
				m.handleWsActionPost(message.Raw)
			case model.WebsocketEventPostDeleted:
				m.handleWsActionPost(message.Raw)
			case model.WebsocketEventEphemeralMessage:
				m.handleWsActionPost(message.Raw)
			case model.WebsocketEventUserRemoved:
				m.handleWsActionUserRemoved(message.Raw)
			case model.WebsocketEventUserAdded:
				// check if we have the users/channels in our cache. If not update
				m.checkWsActionMessage(message.Raw, updateChannelsThrottle)
				m.handleWsActionUserAdded(message.Raw)
			case model.WebsocketEventChannelCreated:
				// check if we have the users/channels in our cache. If not update
				m.checkWsActionMessage(message.Raw, updateChannelsThrottle)
				m.handleWsActionChannelCreated(message.Raw)
			case model.WebsocketEventChannelDeleted:
				// check if we have the users/channels in our cache. If not update
				m.checkWsActionMessage(message.Raw, updateChannelsThrottle)
				m.handleWsActionChannelDeleted(message.Raw)
			case model.WebsocketEventChannelRestored:
				// check if we have the users/channels in our cache. If not update
				m.checkWsActionMessage(message.Raw, updateChannelsThrottle)
			case model.WebsocketEventChannelUpdated:
				m.handleWsActionPost(message.Raw)
			case model.WebsocketEventUserUpdated:
				m.handleWsActionUserUpdated(message.Raw)
			case model.WebsocketEventStatusChange:
				m.handleStatusChangeEvent(message.Raw)
			case model.WebsocketEventReactionAdded, model.WebsocketEventReactionRemoved:
				m.handleReactionEvent(message.Raw)
			}
		}
	}
}

func (m *Mattermost) checkWsActionMessage(rmsg *model.WebSocketEvent, throttle *time.Ticker) {
	if m.GetChannelName(rmsg.GetBroadcast().ChannelId) != "" {
		return
	}

	select {
	case <-throttle.C:
		logger.Debugf("Updating channels for %#v", rmsg.GetBroadcast())
		go m.UpdateChannels()
	default:
	}
}

func (m *Mattermost) Invite(channelID, username string) error {
	_, _, err := m.mc.Client.AddChannelMember(channelID, username)
	if err != nil {
		return err
	}

	return nil
}

func (m *Mattermost) Join(channelName string) (string, string, error) {
	teamID := ""

	sp := strings.Split(channelName, "/")
	if len(sp) > 1 {
		team, _, _ := m.mc.Client.GetTeamByName(sp[0], "")
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
	// create DM channel (only happens on first message)
	dchannel, _, err := m.mc.Client.CreateDirectChannel(m.mc.User.Id, userID)
	if err != nil {
		return "", err
	}

	// build & send the message
	text = strings.ReplaceAll(text, "\r", "")

	return m.MsgChannelThread(dchannel.Id, parentID, text)
}

func (m *Mattermost) MsgChannel(channelID, text string) (string, error) {
	return m.MsgChannelThread(channelID, "", text)
}

func (m *Mattermost) MsgChannelThread(channelID, parentID, text string) (string, error) {
	props := map[string]interface{}{
		"matterircd_" + m.mc.User.Id: m.instanceTag,
	}

	msgType := ""
	// CTCP ACTION (/me)
	if strings.HasPrefix(text, "\x01ACTION ") {
		text = strings.TrimPrefix(text, "\x01ACTION ")
		text = strings.TrimSuffix(text, "\x01")
		msgType = "me"
	}

	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    parentID,
		Type:      msgType,
	}

	post.SetProps(props)

	rp, _, err := m.mc.Client.CreatePost(post)
	if err == nil {
		return rp.Id, nil
	}

	if parentID == "" {
		return "", err
	}

	// Try to work out if we're trying to reply to a post within a thread.
	replyPost, _, err := m.mc.Client.GetPost(parentID, "")
	if err != nil {
		return "", err
	}

	post = &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    replyPost.RootId,
		Type:      msgType,
	}

	post.SetProps(props)

	rp, _, err = m.mc.Client.CreatePost(post)
	if err == nil {
		return rp.Id, nil
	}

	return "", err
}

func (m *Mattermost) ModifyPost(msgID, text string) error {
	if text == "" {
		_, err := m.mc.Client.DeletePost(msgID)
		if err != nil {
			return err
		}

		return nil
	}

	_, _, err := m.mc.Client.PatchPost(msgID, &model.PostPatch{
		Message: &text,
	})
	if err != nil {
		return err
	}

	return nil
}

func (m *Mattermost) AddReaction(msgID, emoji string) error {
	logger.Debugf("adding reaction %#v, %#v", msgID, emoji)
	reaction := &model.Reaction{
		UserId:    m.mc.User.Id,
		PostId:    msgID,
		EmojiName: emoji,
		CreateAt:  0,
	}

	_, _, err := m.mc.Client.SaveReaction(reaction)
	if err != nil {
		return err
	}

	return nil
}

func (m *Mattermost) RemoveReaction(msgID, emoji string) error {
	logger.Debugf("removing reaction %#v, %#v", msgID, emoji)
	reaction := &model.Reaction{
		UserId:    m.mc.User.Id,
		PostId:    msgID,
		EmojiName: emoji,
		CreateAt:  0,
	}

	_, err := m.mc.Client.DeleteReaction(reaction)
	if err != nil {
		return err
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

	_, _, err := m.mc.Client.PatchChannel(channelID, patch)
	if err != nil {
		return err
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
	_, err := m.mc.Client.RemoveUserFromChannel(channelID, username)
	if err != nil {
		return err
	}

	return nil
}

func (m *Mattermost) SetStatus(status string) error {
	_, _, err := m.mc.Client.UpdateUserStatus(m.mc.User.Id, &model.Status{
		Status: status,
		UserId: m.mc.User.Id,
	})
	if err != nil {
		return err
	}

	return nil
}

func (m *Mattermost) Nick(name string) error {
	return m.mc.UpdateUserNick(name)
}

func (m *Mattermost) GetChannelName(channelID string) string {
	var name string

	if channelID == "" || strings.HasPrefix(channelID, "&") || channelID == m.mc.User.Nickname || channelID == m.mc.User.Username {
		return channelID
	}

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
		mmusersPaged []*model.User
		err          error
		resp         *model.Response
	)

	idx := 0
	const batchSize = 200
	users := make([]*bridge.UserInfo, 0, batchSize)

	for {
		mmusersPaged, resp, err = m.mc.Client.GetUsersInChannel(channelID, idx, batchSize, "")
		if err != nil {
			if rlErr := m.mc.HandleRatelimit("GetUsersInChannel", resp); rlErr != nil {
				return nil, rlErr
			}
			continue
		}

		for _, mmuser := range mmusersPaged {
			users = append(users, m.createUser(mmuser))
		}

		if len(mmusersPaged) < batchSize {
			break
		}

		idx++
	}

	return users, nil
}

func (m *Mattermost) GetUsers() []*bridge.UserInfo {
	mmusers := m.mc.GetUsers()
	users := make([]*bridge.UserInfo, 0, len(mmusers))

	for _, mmuser := range mmusers {
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
	if channelID == "" || strings.HasPrefix(channelID, "&") || channelID == m.mc.User.Nickname || channelID == m.mc.User.Username {
		return nil, errors.New("channel not found")
	}

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

	// Fallback if it's not found in the cache.
	mmchannel, _, err := m.mc.Client.GetChannel(channelID, "")
	if err != nil {
		return nil, errors.New("channel not found")
	}
	return &bridge.ChannelInfo{
		Name:    mmchannel.Name,
		ID:      mmchannel.Id,
		TeamID:  mmchannel.TeamId,
		DM:      mmchannel.IsGroupOrDirect(),
		Private: !mmchannel.IsOpen(),
	}, nil
}

func (m *Mattermost) GetUser(userID string) *bridge.UserInfo {
	return m.createUser(m.mc.GetUser(userID))
}

func (m *Mattermost) GetMe() *bridge.UserInfo {
	return m.createUser(m.mc.User)
}

func (m *Mattermost) GetUserByUsername(username string) *bridge.UserInfo {
	for {
		mmuser, resp, err := m.mc.Client.GetUserByUsername(username, "")
		if err == nil {
			return m.createUser(mmuser)
		}

		if err := m.mc.HandleRatelimit("GetUserByUsername", resp); err != nil {
			return &bridge.UserInfo{}
		}
	}
}

func (m *Mattermost) createUser(mmuser *model.User) *bridge.UserInfo {
	if mmuser == nil {
		return &bridge.UserInfo{}
	}

	nick := mmuser.Username
	if m.v.GetBool("mattermost.PreferNickname") && isValidNick(mmuser.Nickname) {
		nick = mmuser.Nickname
	}

	me := mmuser.Id == m.mc.User.Id
	teamID := ""
	if me {
		teamID = m.mc.Team.ID
	}

	// We only care about mentions for ourselves
	var mentionKeys []string
	if keys := mmuser.NotifyProps["mention_keys"]; me && keys != "" {
		mentionKeys = strings.Split(keys, ",")
	}

	info := &bridge.UserInfo{
		Nick:        nick,
		User:        mmuser.Id,
		Real:        mmuser.FirstName + " " + mmuser.LastName,
		Host:        m.mc.Client.URL,
		Roles:       mmuser.Roles,
		Ghost:       true,
		Me:          me,
		TeamID:      teamID,
		Username:    mmuser.Username,
		FirstName:   mmuser.FirstName,
		LastName:    mmuser.LastName,
		MentionKeys: mentionKeys,
	}

	return info
}

//nolint:cyclop
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

const (
	blockquoteCharNonUnicode = "|"
	blockquoteCharUnicode    = "▕"
)

//nolint:forcetypeassert
func (m *Mattermost) wsActionPostSkip(rmsg *model.WebSocketEvent) bool {
	postData, ok := rmsg.GetData()["post"].(string)
	if !ok {
		return true
	}

	disableMarkdown := m.v.GetBool("mattermost.disablemarkdown")
	disableEmoji := m.v.GetBool("mattermost.disableemoji")
	useUnicode := m.v.GetBool("mattermost.unicode")
	blockquoteChar := blockquoteCharNonUnicode
	if useUnicode {
		blockquoteChar = blockquoteCharUnicode
	}
	shortenMsgLen := m.v.GetInt("mattermost.ShortenRepliesTo")

	var data model.Post
	if err := json.NewDecoder(strings.NewReader(postData)).Decode(&data); err != nil {
		return true
	}

	if data.UserId != m.GetMe().User {
		return false
	}

	extraProps := data.GetProps()
	if tag, ok := extraProps["matterircd_"+m.GetMe().User]; !ok || tag != m.instanceTag {
		return false
	}

	if data.Type == model.PostTypeLeaveChannel || data.Type == model.PostTypeJoinChannel {
		logger.Debugf("our own join/leave message. not relaying %#v", data.Message)
		return true
	}

	// Show own edited / deleted
	if !m.v.GetBool("disableshowownmodified") && (rmsg.EventType() == model.WebsocketEventPostEdited || rmsg.EventType() == model.WebsocketEventPostDeleted) {
		return false
	}

	channel := m.GetChannelName(data.ChannelId)

	if strings.Contains(channel, "__") {
		receiver := m.getDMUser(channel)
		channel = receiver.Username
	}

	msgID := data.Id
	var sbSuffix strings.Builder
	if data.RootId != "" {
		msgID = data.RootId
		if !m.v.GetBool("mattermost.hidereplies") {
			parentReplyMsg, err := m.getParentReplyMsg(data.RootId, shortenMsgLen, "@", useUnicode)
			if err == nil {
				sbSuffix.WriteString(parentReplyMsg)
			}
		}
	}

	lastSentMsg := strings.ReplaceAll(data.Message, "\n", " ")

	if !disableMarkdown {
		lastSentMsg = utils.Markdown2irc(lastSentMsg, blockquoteChar)
	}

	if !disableEmoji {
		lastSentMsg = emoji.ReplaceAliases(lastSentMsg)
	}

	lastSentMsg = maybeShorten(lastSentMsg, 90, "@", useUnicode)
	var sb strings.Builder
	sb.WriteString(channel)
	sb.WriteString(": ")
	sb.WriteString(lastSentMsg)
	sb.WriteString(sbSuffix.String())
	m.msgLastSentCache.Add(msgID, sb.String())

	logger.Debugf("message is sent from this matterircd instance, not relaying %#v", data.Message)
	return true
}

// maybeShorten returns a prefix of msg that is approximately newLen
// characters long, followed by "...".  Words that start with uncounted
// are included in the result but are not reckoned against newLen.
//
//nolint:cyclop
func maybeShorten(msg string, newLen int, uncounted string, unicode bool) string {
	if newLen == 0 || len(msg) < newLen {
		return msg
	}

	ellipsis := "..."
	if unicode {
		ellipsis = "…"
	}

	var b strings.Builder
	b.Grow(min(len(msg), newLen+8))

	fields := strings.FieldsFunc(msg, func(r rune) bool {
		return r == ' ' || r == '\n'
	})

	for _, word := range fields {
		if b.Len() > 0 {
			if b.Len() >= newLen {
				break
			}
			b.WriteByte(' ')
		}

		if uncounted != "" && strings.HasPrefix(word, uncounted) {
			newLen += len(word) + 1
		} else if len(word) > newLen {
			// Truncate very long words, but only if they were not skipped, on the
			// assumption that such words are important enough to be preserved whole.
			word = word[:newLen*2/3] + "[" + ellipsis + "]"
		}

		b.WriteString(word)
	}

	// We also want to reset any formatting which can be carried over from shortening
	b.WriteByte('\x0f')
	b.WriteByte(' ')
	b.WriteString(ellipsis)

	return b.String()
}

var markdownReplacer = strings.NewReplacer(
	"\n", " ",
	// Since we're combining multi lines into one, make code blocks single code/monospace
	"```", "`",
	"~~~", "`",
)

func (m *Mattermost) getParentReplyMsg(parentID string, newLen int, uncounted string, unicode bool) (string, error) {
	var replyMessage string

	disableMarkdown := m.v.GetBool("mattermost.disablemarkdown")
	disableEmoji := m.v.GetBool("mattermost.disableemoji")
	useUnicode := m.v.GetBool("mattermost.unicode")
	blockquoteChar := blockquoteCharNonUnicode
	if useUnicode {
		blockquoteChar = blockquoteCharUnicode
	}

	// Search and use cached reply if it exists.
	// None found, so we'll need to create one and save it for future uses.
	if v, ok := m.msgParentCache.Get(parentID); !ok {
		parentPost, _, err := m.mc.Client.GetPost(parentID, "")
		// Retry once on failure.
		if err != nil {
			parentPost, _, err = m.mc.Client.GetPost(parentID, "")
		}
		if err != nil {
			return "", err
		}

		msg := parentPost.Message
		if msg == "" {
			// If we have message attachments and there is a fallback message, use it.
			if attachments := parentPost.Attachments(); len(attachments) > 0 {
				if attachments[0].Fallback != "" {
					msg = attachments[0].Fallback
				} else if attachments[0].Text != "" {
					msg = attachments[0].Text
				}
			}
		}

		if !disableMarkdown {
			msg = markdownReplacer.Replace(msg)
			msg = utils.Markdown2irc(msg, blockquoteChar)
		} else {
			msg = strings.ReplaceAll(msg, "\n", " ")
		}

		if !disableEmoji {
			msg = emoji.ReplaceAliases(msg)
		}

		parentUser := m.GetUser(parentPost.UserId)
		parentMessage := maybeShorten(msg, newLen, uncounted, unicode)
		replyMessage = " (re @" + parentUser.Nick + ": " + parentMessage + ")"
		logger.Debugf("Created reply for parent post %s:%s", parentID, replyMessage)

		m.msgParentCache.Add(parentID, replyMessage)
	} else if replyMessage, ok = v.(string); ok {
		logger.Debugf("Found saved reply for parent post %s, using:%s", parentID, replyMessage)
	}

	return replyMessage, nil
}

var (
	validIRCNickRegExp    = regexp.MustCompile("^[a-zA-Z0-9_]*$")
	channelMentionsRegExp = regexp.MustCompile(`@(channel|all|here)\W`)
)

//nolint:funlen,gocognit,gocyclo,cyclop,forcetypeassert
func (m *Mattermost) handleWsActionPost(rmsg *model.WebSocketEvent) {
	wsData := rmsg.GetData()
	postData, ok := wsData["post"].(string)
	if !ok {
		return
	}

	var data model.Post
	if err := json.NewDecoder(strings.NewReader(postData)).Decode(&data); err != nil {
		return
	}
	extraProps := data.GetProps()

	logger.Debugf("handleWsActionPost() receiving userid %s", data.UserId)
	if m.wsActionPostSkip(rmsg) {
		return
	}

	useUnicode := m.v.GetBool("mattermost.unicode")

	var sbSuffix strings.Builder
	if !m.v.GetBool("mattermost.hidereplies") && data.RootId != "" {
		parentReplyMsg, err := m.getParentReplyMsg(data.RootId, m.v.GetInt("mattermost.ShortenRepliesTo"), "@", useUnicode)
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v", data) //nolint:govet
		} else {
			sbSuffix.WriteString(parentReplyMsg)
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

	// check if we have a override_username (from webhooks) and use it
	overrideUsername, _ := extraProps["override_username"].(string)
	if overrideUsername != "" {
		logger.Debugf("found override username %s", overrideUsername)
		// only allow valid irc nicks
		if validIRCNickRegExp.MatchString(overrideUsername) {
			ghost.Nick = overrideUsername
			ghost.Me = false
		}
	}

	channelType := ""
	if t, ok := wsData["channel_type"].(string); ok {
		channelType = t
	}
	dmchannel := ""
	if t, ok := wsData["channel_name"].(string); ok {
		dmchannel = t
	}

	switch data.Type {
	case model.PostTypeJoinChannel, model.PostTypeLeaveChannel, model.PostTypeAddToChannel, model.PostTypeRemoveFromChannel:
		logger.Debugf("join/leave message. not relaying %#v", data.Message)
		_ = m.UpdateChannels()

		m.wsActionPostJoinLeave(&data, extraProps)
		return

	case model.PostTypeHeaderChange:
		if _, ok := extraProps["new_header"].(string); !ok {
			return
		}
		topic := extraProps["new_header"].(string)

		if channelType == "D" {
			event := &bridge.Event{
				Type: "direct_message",
			}

			d := &bridge.DirectMessageEvent{
				Text:      "\x01ACTION updated topic to: " + topic + "\x01",
				ChannelID: data.ChannelId,
				MessageID: data.Id,
				Event:     "dm_topic",
			}

			userUpdated := extraProps["username"].(string)
			if userUpdated == m.GetMe().Nick {
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
			return
		}

		event := &bridge.Event{
			Type: "channel_topic",
			Data: &bridge.ChannelTopicEvent{
				Text:      topic,
				ChannelID: data.ChannelId,
				UserID:    data.UserId,
			},
		}

		m.eventChan <- event
		return

	case model.PostTypeSystemGeneric, model.PostTypeEphemeral, model.PostTypeAddToTeam, model.PostTypeRemoveFromTeam:
		ghost = &bridge.UserInfo{
			Nick: "system",
		}
	}

	eventType := rmsg.EventType()
	// add an edited/deleted string when messages are edited/deleted
	if eventType == model.WebsocketEventPostEdited || eventType == model.WebsocketEventPostDeleted {
		if eventType == model.WebsocketEventPostDeleted {
			sbSuffix.WriteString(" \x1d(deleted)\x1d")
		} else {
			sbSuffix.WriteString(" \x1d(edited)\x1d")
		}

		// check if we have an edited direct message (channels have __)
		name := m.GetChannelName(data.ChannelId)
		if strings.Contains(name, "__") {
			channelType = "D"
		}
		dmchannel = name

		// We need to remove it from the cache so that replies use the latest msg.
		m.msgParentCache.Remove(data.Id)
	}

	msg := data.Message
	var sbMsg strings.Builder
	attachments := data.Attachments()

	switch {
	case data.Type == "me":
		sbMsg.WriteString("\x01ACTION ")
		sbMsg.WriteString(msg)
		sbMsg.WriteString(sbSuffix.String())
		sbMsg.WriteString("\x01")
	case data.Type == "slack_attachment":
		useFallback := msg == ""
		// https://docs.slack.dev/tools/node-slack-sdk/reference/web-api/interfaces/MessageAttachment/
		attachmentMsg := m.parseMessageAttachments(attachments, useFallback)
		if msg == "" {
			sbMsg.WriteString(attachmentMsg)
		} else if attachmentMsg != "" {
			sbMsg.WriteString(msg)
			sbMsg.WriteString("\n")
			sbMsg.WriteString(attachmentMsg)
		}
	case data.Type == "custom_matterpoll":
		pollMsg := parseMatterpollToMsg(attachments, useUnicode)
		sbMsg.WriteString(msg)
		sbMsg.WriteString(pollMsg)
	case len(attachments) > 0:
		useFallback := msg == ""
		// https://developers.mattermost.com/integrate/reference/message-attachments/
		attachmentMsg := m.parseMessageAttachments(attachments, useFallback)
		if msg == "" {
			sbMsg.WriteString(attachmentMsg)
		} else if attachmentMsg != "" {
			sbMsg.WriteString(msg)
			sbMsg.WriteString("\n")
			sbMsg.WriteString(attachmentMsg)
		}
	default:
		sbMsg.WriteString(msg)
	}

	if sbSuffix.Len() > 0 && data.Type != "me" {
		sbMsg.WriteString(sbSuffix.String())
	}

	switch {
	case channelType == "D":
		event := &bridge.Event{
			Type: "direct_message",
		}

		d := &bridge.DirectMessageEvent{
			Text:      sbMsg.String(),
			ChannelID: data.ChannelId,
			MessageID: data.Id,
			Event:     eventType,
			ParentID:  data.RootId,
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
	default:
		messageType := ""
		if !m.v.GetBool("mattermost.disabledefaultmentions") && channelMentionsRegExp.MatchString(data.Message) {
			messageType = "notice"
		}

		event := &bridge.Event{
			Type: "channel_message",
			Data: &bridge.ChannelMessageEvent{
				Text:        sbMsg.String(),
				ChannelID:   data.ChannelId,
				Sender:      ghost,
				MessageType: messageType,
				ChannelType: channelType,
				MessageID:   data.Id,
				Event:       eventType,
				ParentID:    data.RootId,
			},
		}

		m.eventChan <- event
	}

	if len(data.FileIds) > 0 {
		m.handleFileEvent(channelType, ghost, &data, rmsg)
	}

	logger.Debugf("handleWsActionPost() user %s sent %#v", m.mc.GetUser(data.UserId).Username, data.Message)
	logger.Debugf("%#v", data) //nolint:govet
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
		MessageID:   data.Id,
		ParentID:    data.RootId,
	}

	event.Data = fileEvent

	for _, fname := range m.getFilesFromData(data) {
		fileEvent.Files = append(fileEvent.Files, &bridge.File{
			Name: fname.Name,
		})
	}

	if len(fileEvent.Files) == 0 {
		logger.Debugf("handleFileEvent() user %s sent 0 files %#v", m.mc.GetUser(data.UserId).Username, data.FileIds)
		return
	}

	switch {
	case channelType == "D":
		if ghost.Me {
			fileEvent.Sender = ghost
			fileEvent.Receiver = m.getDMUser(rmsg.GetData()["channel_name"])
		} else {
			fileEvent.Sender = m.getDMUser(rmsg.GetData()["channel_name"])
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

	logger.Debugf("handleFileEvent() user %s sent %d files %#v", m.mc.GetUser(data.UserId).Username, len(fileEvent.Files), data.FileIds)
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
	userID, ok := rmsg.GetData()["user_id"].(string)
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
			ChannelID: rmsg.GetBroadcast().ChannelId,
		},
	}

	m.eventChan <- event
}

func (m *Mattermost) handleWsActionUserRemoved(rmsg *model.WebSocketEvent) {
	wsData := rmsg.GetData()
	userID, ok := wsData["user_id"].(string)
	if !ok {
		userID = rmsg.GetBroadcast().UserId
	}

	removerID, ok := wsData["remover_id"].(string)
	if !ok {
		fmt.Println("not ok removerID", removerID)
		return
	}

	channelID, ok := wsData["channel_id"].(string)
	if !ok {
		channelID = rmsg.GetBroadcast().ChannelId
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

	err := Decode(rmsg.GetData()["user"], &info)
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
	channelID, ok := rmsg.GetData()["channel_id"].(string)
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
	channelID, ok := rmsg.GetData()["channel_id"].(string)
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

	err := Decode(rmsg.GetData(), &info)
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

//nolint:forcetypeassert
func (m *Mattermost) handleReactionEvent(rmsg *model.WebSocketEvent) {
	reactionData, ok := rmsg.GetData()["reaction"].(string)
	if !ok {
		return
	}

	var reaction model.Reaction
	if err := json.NewDecoder(strings.NewReader(reactionData)).Decode(&reaction); err != nil {
		return
	}

	userID := m.GetUser(reaction.UserId)
	sender := userID
	receiver := m.GetMe()

	// Don't show our own reaction messages unless mattermost.showownreactions is enabled.
	if userID.Me && !m.v.GetBool("mattermost.showownreactions") {
		logger.Debugf("Not showing own reaction: %s: %s", rmsg.EventType(), reaction.EmojiName)
		return
	}

	var event *bridge.Event

	channelType := ""
	channelID := rmsg.GetBroadcast().ChannelId
	name := m.GetChannelName(channelID)
	if strings.Contains(name, "__") {
		channelType = "D"
		dmUser := m.getDMUser(name)
		if dmUser == nil {
			logger.Errorf("reaction: unable to resolve DM peer for channel %q", name)
			return
		}
		if userID.Me {
			receiver = m.getDMUser(name)
		} else {
			receiver = sender
			sender = m.getDMUser(name)
		}
	}

	var parentUser *bridge.UserInfo
	var sbSuffix strings.Builder
	if !m.v.GetBool("mattermost.hidereplies") {
		parentReplyMsg, err := m.getParentReplyMsg(reaction.PostId, m.v.GetInt("mattermost.ShortenRepliesTo"), "@", m.v.GetBool("mattermost.unicode"))
		if err != nil {
			logger.Errorf("Unable to get parent post for %#v", reaction)
		}
		sbSuffix.WriteString(parentReplyMsg)
	}

	parentID := reaction.PostId
	parentPost, _, err := m.mc.Client.GetPost(reaction.PostId, "")
	if err == nil {
		parentID = parentPost.RootId
	}

	switch rmsg.EventType() {
	case model.WebsocketEventReactionAdded:
		event = &bridge.Event{
			Type: "reaction_add",
			Data: &bridge.ReactionAddEvent{
				ChannelID:   channelID,
				MessageID:   reaction.PostId,
				Receiver:    receiver,
				Sender:      sender,
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
				ParentUser:  parentUser,
				Message:     sbSuffix.String(),
				ParentID:    parentID,
			},
		}
	case model.WebsocketEventReactionRemoved:
		event = &bridge.Event{
			Type: "reaction_remove",
			Data: &bridge.ReactionRemoveEvent{
				ChannelID:   channelID,
				MessageID:   reaction.PostId,
				Receiver:    receiver,
				Sender:      sender,
				Reaction:    reaction.EmojiName,
				ChannelType: channelType,
				ParentUser:  parentUser,
				Message:     sbSuffix.String(),
				ParentID:    parentID,
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
		dc, resp, err := m.mc.Client.CreateDirectChannel(m.mc.User.Id, userID)
		if err == nil {
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
	users, _, err := m.mc.Client.SearchUsers(&model.UserSearch{Term: query})
	if err != nil {
		return nil, err
	}

	brusers := make([]*bridge.UserInfo, 0, len(users))

	for _, u := range users {
		brusers = append(brusers, m.createUser(u))
	}

	return brusers, nil
}

func (m *Mattermost) GetPosts(channelID string, limit int) interface{} {
	return m.mc.GetPosts(channelID, limit)
}

func (m *Mattermost) GetPostThread(postID string) interface{} {
	return m.mc.GetPostThread(postID)
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

const (
	messageAttachmentCharNonUnicode = "|"
	// right one quarter block (U+1FB87)
	messageAttachmentCharUnicode     = "🮇"
	messageAttachmentSpaceNonUnicode = " "
	// non-breaking space / no-break space / nbsp (U+00A0)
	messageAttachmentSpaceUnicode = " "
)

func parseMatterpollToMsg(attachments []*model.SlackAttachment, unicode bool) string {
	msg := ""
	prefixChar := messageAttachmentCharNonUnicode
	spaceChar := messageAttachmentSpaceNonUnicode
	if unicode {
		prefixChar = messageAttachmentCharUnicode
		spaceChar = messageAttachmentSpaceUnicode
	}
	for _, attachment := range attachments {
		prefix := "\033[1;38;2;0;82;204m" + prefixChar + "\033[0m" + spaceChar

		if attachment.AuthorName != "" {
			msg += prefix + "@" + attachment.AuthorName + "\n"
		}
		if attachment.Title != "" {
			msg += prefix + "\x02" + attachment.Title + "\x02\n"
		}

		for _, action := range attachment.Actions {
			if strings.HasPrefix(action.Id, "vote") {
				msg += prefix + "•" + spaceChar + action.Name + "\n"
			}
		}

		if attachment.Text != "" {
			lines := strings.Split(attachment.Text, "\n")
			for _, text := range lines {
				msg += prefix + text + "\n"
			}
		}
		if !strings.HasPrefix(attachment.Text, "This poll has ended.") {
			msg += prefix + "\n"
			msg += prefix + "\x1dUse the web UI to cast your vote\x1d"
		}

		for _, field := range attachment.Fields {
			msg += prefix + "•" + spaceChar + field.Title + ":" + spaceChar
			lines := strings.Split(fmt.Sprintf("%s", field.Value), "\n")
			newPrefix := ""
			for _, text := range lines {
				msg += newPrefix + text + "\n"
				newPrefix = prefix
			}
		}
	}

	return strings.TrimRight(msg, "\n")
}

const blockQuoteCharDefault = ">"

//nolint:funlen,gocognit,gocyclo
func (m *Mattermost) parseMessageAttachments(attachments []*model.SlackAttachment, useFallback bool) string {
	useUnicode := m.v.GetBool("mattermost.unicode")
	syntaxHighlighting := m.v.GetString("mattermost.syntaxhighlighting")
	codeBlockPrefix := m.v.GetString("mattermost.codeblockprefix")
	disableMarkdown := m.v.GetBool("mattermost.disablemarkdown")
	disableEmoji := m.v.GetBool("mattermost.disableemoji")

	prefixChar := messageAttachmentCharNonUnicode
	spaceChar := messageAttachmentSpaceNonUnicode
	blockquoteChar := blockquoteCharNonUnicode
	if useUnicode {
		prefixChar = messageAttachmentCharUnicode
		spaceChar = messageAttachmentSpaceUnicode
		blockquoteChar = blockquoteCharUnicode
		// Downgrade heavy vertical to light as we're using heavy already
		if strings.ContainsAny(codeBlockPrefix, "┃🮇▎") {
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "┃", "│", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "🮇", "▕", 1)
			codeBlockPrefix = strings.Replace(codeBlockPrefix, "▎", "▏", 1)
		}
	}

	var b strings.Builder

	for _, attachment := range attachments {
		prefix := "\033[1m" + prefixChar + "\033[0m" + spaceChar
		switch {
		// https://docs.slack.dev/tools/node-slack-sdk/reference/web-api/interfaces/MessageAttachment/#color
		case attachment.Color == "danger":
			prefix = "\033[31m" + prefixChar + "\033[0m" + spaceChar
		case attachment.Color == "good":
			prefix = "\033[32m" + prefixChar + "\033[0m" + spaceChar
		case attachment.Color == "warning":
			prefix = "\033[33m" + prefixChar + "\033[0m" + spaceChar
		case strings.HasPrefix(attachment.Color, "#"):
			hex := strings.TrimPrefix(attachment.Color, "#")
			rr, _ := strconv.ParseInt(hex[0:2], 16, 0)
			gg, _ := strconv.ParseInt(hex[2:4], 16, 0)
			bb, _ := strconv.ParseInt(hex[4:6], 16, 0)
			// https://modern.ircdocs.horse/formatting.html#hex-color
			prefix = "\033[1;38;2;" +
				strconv.Itoa(int(rr)) + ";" +
				strconv.Itoa(int(gg)) + ";" +
				strconv.Itoa(int(bb)) + "m" +
				prefixChar + "\033[0m" + spaceChar
		}

		var fallbackText string
		if useFallback {
			fallbackText, _, _ = strings.Cut(attachment.Fallback, "\n")

			// In some cases, no fallback message present
			// e.g. https://github.com/fluxcd/notification-controller/pull/1322
			if fallbackText == "" {
				fallbackText, _, _ = strings.Cut(attachment.Text, "\n")
				if attachment.AuthorName != "" {
					fallbackText = attachment.AuthorName + ":" + spaceChar + fallbackText
				}
			}

			if !disableMarkdown {
				fallbackText = utils.Markdown2irc(fallbackText, blockquoteChar)
			}

			if !disableEmoji {
				fallbackText = emoji.ReplaceAliases(fallbackText)
			}

			b.WriteString(fallbackText)
			b.WriteByte('\n')
		}

		if attachment.AuthorName != "" {
			b.WriteString(prefix)
			b.WriteString(attachment.AuthorName)
			if attachment.AuthorLink != "" {
				b.WriteString(spaceChar)
				b.WriteString("(")
				b.WriteString(attachment.AuthorLink)
				b.WriteString(")")
			}
			b.WriteByte('\n')
		}
		if attachment.Title != "" {
			b.WriteString(prefix)
			b.WriteByte('\x02')
			b.WriteString(attachment.Title)
			b.WriteByte('\x02')
			if attachment.TitleLink != "" {
				b.WriteString(" (\x1d")
				b.WriteString(attachment.TitleLink)
				b.WriteByte('\x1d')
				b.WriteByte(')')
			}
			b.WriteByte('\n')
		}
		if attachment.Text != "" {
			lexer := ""
			codeBlockBackTick := false
			codeBlockTilde := false
			lines := strings.Split(attachment.Text, "\n")
			for _, text := range lines {
				text, codeBlockBackTick, codeBlockTilde, lexer = utils.FormatCodeBlockText(text, codeBlockBackTick, codeBlockTilde, lexer, syntaxHighlighting, codeBlockPrefix)

				if !disableMarkdown && !codeBlockBackTick && !codeBlockTilde {
					text = utils.Markdown2irc(text, blockquoteChar)
				}

				if !disableEmoji && !codeBlockBackTick && !codeBlockTilde {
					text = emoji.ReplaceAliases(text)
				}

				b.WriteString(prefix)
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
		if attachment.ImageURL != "" {
			b.WriteString(prefix)
			b.WriteString(attachment.ImageURL)
			b.WriteByte('\n')
		}

		for i := 0; i < len(attachment.Fields); {
			field := attachment.Fields[i]
			// In case the value has any new lines, strip it to avoid messing with our table format
			val1Str := strings.TrimPrefix(fmt.Sprintf("%v", field.Value), "\n")

			// Block quotes
			if !disableMarkdown && strings.HasPrefix(val1Str, blockQuoteCharDefault) {
				val1Str = strings.Replace(val1Str, blockQuoteCharDefault, prefixChar, 1)
			}

			// Check if this field and the next field are both flagged as "short"
			if field.Short && i+1 < len(attachment.Fields) && attachment.Fields[i+1].Short {
				nextField := attachment.Fields[i+1]
				// Same, avoid messing with our table format
				val2Str := strings.TrimPrefix(fmt.Sprintf("%v", nextField.Value), "\n")

				b.WriteString(prefix)
				b.WriteByte('\x02')
				b.WriteString(fmt.Sprintf("%-30s %s", field.Title, nextField.Title))
				b.WriteByte('\x02')
				b.WriteByte('\n')

				val1Lines := strings.Split(val1Str, "\n")
				val2Lines := strings.Split(val2Str, "\n")

				maxLines := len(val1Lines)
				if len(val2Lines) > maxLines {
					maxLines = len(val2Lines)
				}

				for j := 0; j < maxLines; j++ {
					v1, v2 := "", ""
					if j < len(val1Lines) {
						v1 = val1Lines[j]
					}
					if j < len(val2Lines) {
						v2 = val2Lines[j]
					}
					b.WriteString(prefix)
					b.WriteString(fmt.Sprintf("%-30s %s", v1, v2))
					b.WriteByte('\n')
				}

				i += 2
			} else {
				// Fallback to original behavior for long fields or unpaired short fields

				if field.Title != "" {
					b.WriteString(prefix)
					b.WriteByte('\x02')
					b.WriteString(field.Title)
					b.WriteByte('\x02')
					b.WriteByte('\n')
				}

				lexer := ""
				codeBlockBackTick := false
				codeBlockTilde := false
				lines := strings.Split(val1Str, "\n")
				for _, text := range lines {
					text, codeBlockBackTick, codeBlockTilde, lexer = utils.FormatCodeBlockText(text, codeBlockBackTick, codeBlockTilde, lexer, syntaxHighlighting, codeBlockPrefix)

					if !disableMarkdown && !codeBlockBackTick && !codeBlockTilde {
						text = utils.Markdown2irc(text, blockquoteChar)
					}

					if !disableEmoji && !codeBlockBackTick && !codeBlockTilde {
						text = emoji.ReplaceAliases(text)
					}

					// Ignore duplicate content when field value is the same as fallback
					// e.g. https://github.com/jenkinsci/mattermost-plugin/pull/18
					if useFallback && fallbackText != "" && text == fallbackText {
						continue
					}

					b.WriteString(prefix)
					b.WriteString(text)
					b.WriteByte('\n')
				}
				i++
			}
		}
	}

	msg := b.String()
	if strings.HasSuffix(msg, "\n") { //nolint:gosimple
		msg = msg[:len(msg)-1]
	}
	return msg
}

func (m *Mattermost) GetLastSentMsgs() []string {
	data := make([]string, 0)

	for _, k := range m.msgLastSentCache.Keys() {
		if v, ok := m.msgLastSentCache.Get(k); ok {
			msg, _ := v.(string)
			data = append(data, "[@@"+fmt.Sprint(k)+"] "+msg)
		}
	}

	return data
}
