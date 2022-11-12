package mastodon

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/42wim/matterircd/bridge"
	"github.com/davecgh/go-spew/spew"
	strip "github.com/grokify/html-strip-tags-go"
	"github.com/mattn/go-mastodon"
	logger "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

type Mastodon struct {
	mc          *mastodon.Client
	connected   bool
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	eventChanIn chan mastodon.Event
	onConnect   func()
	sync.RWMutex
	v *viper.Viper
}

func New(v *viper.Viper, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, error) {
	m := &Mastodon{
		credentials: cred,
		eventChan:   eventChan,
		onConnect:   onConnect,
		v:           v,
	}

	var err error

	logger.SetFormatter(&logger.TextFormatter{FullTimestamp: true})
	if v.GetBool("debug") {
		logger.SetLevel(logger.DebugLevel)
	}

	if v.GetBool("trace") {
		logger.SetLevel(logger.TraceLevel)
	}

	m.mc, err = m.loginToMastodon()
	if err != nil {
		return nil, err
	}

	go m.handleMastodon()
	go m.onConnect()

	m.connected = true

	return m, nil
}

func (m *Mastodon) Invite(channelID, username string) error {
	return nil
}

func (m *Mastodon) Join(channelName string) (string, string, error) {
	return "", "", nil
}

func (m *Mastodon) List() (map[string]string, error) {
	return make(map[string]string), nil
}

func (m *Mastodon) Part(channelID string) error {
	return nil
}

func (m *Mastodon) UpdateChannels() error {
	return nil
}

func (m *Mastodon) Logout() error {
	return nil
}

func (m *Mastodon) MsgUser(username, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) MsgChannel(channelID, text string) (string, error) {
	s, err := m.mc.PostStatus(context.Background(), &mastodon.Toot{
		Status: text,
	})
	if err != nil {
		return "", err
	}

	return string(s.ID), nil
}

func (m *Mastodon) StatusUser(name string) (string, error) {
	return "", nil
}

func (m *Mastodon) StatusUsers() (map[string]string, error) {
	return make(map[string]string), nil
}

func (m *Mastodon) Protocol() string {
	return "mastodon" //nolint:goconst
}

func (m *Mastodon) Kick(channelID, username string) error {
	return nil
}

func (m *Mastodon) SetStatus(status string) error {
	return nil
}

func (m *Mastodon) Nick(name string) error {
	return nil
}

func (m *Mastodon) GetChannelName(channelID string) string {
	if channelID == "mastodon" {
		return "#mastodon"
	}

	return channelID
}

func (m *Mastodon) GetChannelUsers(channelID string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (m *Mastodon) GetUsers() []*bridge.UserInfo {
	return []*bridge.UserInfo{}
}

func (m *Mastodon) GetChannels() []*bridge.ChannelInfo {
	return nil
}

func (m *Mastodon) GetChannel(channelID string) (*bridge.ChannelInfo, error) {
	if channelID != "mastodon" {
		return nil, fmt.Errorf("channel not found")
	}

	return &bridge.ChannelInfo{
		ID:      "mastodon",
		Name:    "#mastodon",
		TeamID:  "mastodon",
		DM:      false,
		Private: false,
	}, nil
}

func (m *Mastodon) GetUser(userID string) *bridge.UserInfo {
	return nil
}

func (m *Mastodon) GetMe() *bridge.UserInfo {
	return &bridge.UserInfo{Nick: "me", Username: "me", Me: true, User: "me"}
}

func (m *Mastodon) GetUserByUsername(username string) *bridge.UserInfo {
	return nil
}

func (m *Mastodon) GetTeamName(teamID string) string {
	return ""
}

func (m *Mastodon) GetLastViewedAt(channelID string) int64 {
	return 0
}

func (m *Mastodon) GetPostsSince(channelID string, since int64) interface{} {
	return nil
}

func (m *Mastodon) SearchPosts(search string) interface{} {
	return nil
}

func (m *Mastodon) UpdateLastViewed(channelID string) {
}

func (m *Mastodon) UpdateLastViewedUser(userID string) error {
	return nil
}

func (m *Mastodon) GetFileLinks(fileIDs []string) []string {
	return []string{}
}

func (m *Mastodon) SearchUsers(query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (m *Mastodon) GetPosts(channelID string, limit int) interface{} {
	return nil
}

func (m *Mastodon) GetChannelID(name, teamID string) string {
	return ""
}

func (m *Mastodon) loginToMastodon() (*mastodon.Client, error) {
	mc := mastodon.NewClient(&mastodon.Config{
		Server:       m.v.GetString("mastodon.server"),
		ClientID:     m.v.GetString("mastodon.clientid"),
		ClientSecret: m.v.GetString("mastodon.clientsecret"),
		AccessToken:  m.v.GetString("mastodon.accesstoken"),
	})

	// events, err := mc.StreamingPublic(context.Background(), false)
	events, err := mc.StreamingUser(context.Background())
	if err != nil {
		return nil, err
	}

	m.eventChanIn = events

	return mc, nil
}

func (m *Mastodon) handleMastodon() {
	for event := range m.eventChanIn {
		logger.Tracef("handleMastodon %s", spew.Sdump(event))
		switch event := event.(type) {
		case *mastodon.UpdateEvent:
			m.handleMastodonUpdate(event)
		case *mastodon.NotificationEvent:
			m.handleMastodonNotification(event)
			/*				case *mastodon.DeleteEvent:
							m.handleMastodonDelete(event)
			*/
		}
	}
}

func (m *Mastodon) sendPublicMessage(ghost *bridge.UserInfo, msg, channelID string) {
	msg = strip.StripTags(msg)

	event := &bridge.Event{
		Type: "channel_message",
		Data: &bridge.ChannelMessageEvent{
			Text:      msg,
			ChannelID: channelID,
			Sender:    ghost,
		},
	}

	m.eventChan <- event
}

func (m *Mastodon) handleMastodonNotification(event *mastodon.NotificationEvent) {
	if event.Notification == nil {
		return
	}

	logger.Tracef("handleMastodonNotification %s", spew.Sdump(event))
}

func (m *Mastodon) handleMastodonUpdate(event *mastodon.UpdateEvent) {
	if event.Status == nil {
		return
	}

	logger.Tracef("handleMastodonUpdate %s", spew.Sdump(event))

	s := event.Status

	msghandled := false
	ghost := m.createUser(&s.Account)
	spoofUsername := ghost.Nick

	msgs := []string{}

	if s.Content != "" {
		msgs = append(msgs, strings.Split(s.Content, "\n")...)
		msghandled = true
	}

	channelID := "mastodon"

	for _, msg := range msgs {
		// still no text, ignore this message
		if !msghandled {
			msg = fmt.Sprintf("Empty: %#v", msg)
		}

		ghost.Nick = spoofUsername
		m.sendPublicMessage(ghost, msg, channelID)
	}
}

func (m *Mastodon) createUser(muser *mastodon.Account) *bridge.UserInfo {
	if muser.Username == "" {
		return &bridge.UserInfo{}
	}

	host := "unknown"
	username := muser.Username
	u, err := url.Parse(m.mc.Config.Server)
	if err == nil {
		host = u.Hostname()
	}

	sp := strings.Split(muser.Acct, "@")
	if len(sp) == 2 {
		username = strings.TrimSpace(sp[0])
		host = strings.TrimSpace(sp[1])
	}

	info := &bridge.UserInfo{
		Nick:        strings.ReplaceAll(muser.Acct, "@", "|"),
		User:        username,
		Real:        host,
		Host:        host,
		Roles:       "",
		DisplayName: muser.DisplayName,
		Ghost:       true,
		Me:          false,
		Username:    muser.Username,
		FirstName:   "",
		LastName:    "",
		TeamID:      "mastodon",
	}

	return info
}

func (m *Mastodon) Connected() bool {
	return m.connected
}

func (m *Mastodon) MsgUserThread(username, parentID, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) MsgChannelThread(username, parentID, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) ModifyPost(channelID, text string) error {
	return nil
}

func (m *Mastodon) AddReaction(msgID, emoji string) error {
	return nil
}

func (m *Mastodon) RemoveReaction(msgID, emoji string) error {
	return nil
}

func (m *Mastodon) SetTopic(channelID, text string) error {
	return nil
}

func (m *Mastodon) Topic(channelID string) string {
	return ""
}
