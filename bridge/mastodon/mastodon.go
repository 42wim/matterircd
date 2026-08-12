package mastodon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/42wim/matterircd/bridge"
	"github.com/42wim/matterircd/config"
	"github.com/davecgh/go-spew/spew"
	strip "github.com/grokify/html-strip-tags-go"
	"github.com/mattn/go-mastodon"
	logger "github.com/sirupsen/logrus"
)

type Mastodon struct {
	mc          *mastodon.Client
	connected   bool
	credentials bridge.Credentials
	eventChan   chan *bridge.Event
	eventChanIn chan mastodon.Event
	onConnect   func()
	sync.RWMutex

	cfg *config.Config
}

func New(cfg *config.Config, cred bridge.Credentials, eventChan chan *bridge.Event, onConnect func()) (bridge.Bridger, error) {
	m := &Mastodon{
		credentials: cred,
		eventChan:   eventChan,
		onConnect:   onConnect,
		cfg:         cfg,
	}

	rc := cfg.Current()

	logger.SetFormatter(&logger.TextFormatter{FullTimestamp: true})
	if rc.Debug {
		logger.SetLevel(logger.DebugLevel)
	}

	if rc.Trace {
		logger.SetLevel(logger.TraceLevel)
	}

	mc, err := m.loginToMastodon()
	if err != nil {
		return nil, err
	}

	// Helper closure to set bridge levels
	setBridgeLogLevels := func(rc *config.RuntimeConfig) {
		switch {
		case rc.Trace:
			logger.SetLevel(logger.TraceLevel)
		case rc.Debug:
			logger.SetLevel(logger.DebugLevel)
		default:
			logger.SetLevel(logger.InfoLevel)
		}
	}

	// Set initial log level
	setBridgeLogLevels(rc)

	// Register hook to update live on reloads
	cfg.RegisterReloadHook(func(newRC *config.RuntimeConfig) {
		setBridgeLogLevels(newRC)
	})

	m.mc = mc

	go m.handleMastodon()
	go m.onConnect()

	m.connected = true

	return m, nil
}

func (m *Mastodon) Invite(ctx context.Context, channelID, username string) error {
	return nil
}

func (m *Mastodon) IsChannelMember(channelID string) bool {
	// Mastodon only has the single unified timeline "channel"
	return channelID == "mastodon" //nolint:goconst
}

func (m *Mastodon) Join(ctx context.Context, channelName string) (string, string, error) {
	return "", "", nil
}

func (m *Mastodon) List(ctx context.Context) (map[string]string, error) {
	return make(map[string]string), nil
}

func (m *Mastodon) Part(ctx context.Context, channelID string) error {
	return nil
}

func (m *Mastodon) UpdateChannels(ctx context.Context) error {
	return nil
}

func (m *Mastodon) Logout(ctx context.Context) error {
	return nil
}

func (m *Mastodon) MsgUser(ctx context.Context, username, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) MsgChannel(ctx context.Context, channelID, text string) (string, error) {
	s, err := m.mc.PostStatus(context.Background(), &mastodon.Toot{
		Status: text,
	})
	if err != nil {
		return "", err
	}

	return string(s.ID), nil
}

func (m *Mastodon) StatusUser(ctx context.Context, name string) (string, error) {
	return "", nil
}

func (m *Mastodon) StatusUsers(ctx context.Context) (map[string]string, error) {
	return make(map[string]string), nil
}

func (m *Mastodon) Protocol() string {
	return "mastodon" //nolint:goconst
}

func (m *Mastodon) Kick(ctx context.Context, channelID, username string) error {
	return nil
}

func (m *Mastodon) SetStatus(ctx context.Context, status string) error {
	return nil
}

func (m *Mastodon) Nick(ctx context.Context, name string) error {
	return nil
}

func (m *Mastodon) GetChannelName(ctx context.Context, channelID string) string {
	if channelID == "mastodon" {
		return "#mastodon"
	}

	return channelID
}

func (m *Mastodon) GetChannelUsers(ctx context.Context, channelID string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (m *Mastodon) GetUsers() []*bridge.UserInfo {
	return []*bridge.UserInfo{}
}

func (m *Mastodon) GetChannels() []*bridge.ChannelInfo {
	return nil
}

func (m *Mastodon) CreateChannel(ctx context.Context, channelName string, channelType string) (*bridge.ChannelInfo, error) {
	return nil, errors.New("not implemented yet")
}

func (m *Mastodon) GetChannel(ctx context.Context, channelID string) (*bridge.ChannelInfo, error) {
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

func (m *Mastodon) GetUser(ctx context.Context, userID string) *bridge.UserInfo {
	return nil
}

func (m *Mastodon) GetMe() *bridge.UserInfo {
	return &bridge.UserInfo{Nick: "me", Username: "me", Me: true, User: "me"}
}

func (m *Mastodon) GetUserByUsername(ctx context.Context, username string) *bridge.UserInfo {
	return nil
}

func (m *Mastodon) GetTeamName(ctx context.Context, teamID string) string {
	return ""
}

func (m *Mastodon) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	return 0
}

func (m *Mastodon) GetPostsSince(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Mastodon) SearchPosts(ctx context.Context, search string) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Mastodon) UpdateLastViewed(ctx context.Context, channelID string) {
}

func (m *Mastodon) UpdateLastViewedUser(ctx context.Context, userID string) error {
	return nil
}

func (m *Mastodon) GetFilesInfo(ctx context.Context, fileIDs []string) []*bridge.File {
	return []*bridge.File{}
}

func (m *Mastodon) SearchUsers(ctx context.Context, query string) ([]*bridge.UserInfo, error) {
	return nil, nil
}

func (m *Mastodon) GetPosts(ctx context.Context, channelID string, limit int) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Mastodon) GetPostThread(ctx context.Context, postID string) []*bridge.Event {
	return []*bridge.Event{}
}

func (m *Mastodon) GetChannelID(ctx context.Context, name, teamID string) string {
	return ""
}

func (m *Mastodon) loginToMastodon() (*mastodon.Client, error) {
	rc := m.cfg.Current()

	mc := mastodon.NewClient(&mastodon.Config{
		Server:       rc.Mastodon.Server,
		ClientID:     rc.Mastodon.ClientID,
		ClientSecret: rc.Mastodon.ClientSecret,
		AccessToken:  rc.Mastodon.AccessToken,
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

func (m *Mastodon) MsgUserThread(ctx context.Context, username, parentID, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) MsgChannelThread(ctx context.Context, username, parentID, text string) (string, error) {
	return "", nil
}

func (m *Mastodon) ModifyPost(ctx context.Context, channelID, text string) error {
	return nil
}

func (m *Mastodon) AddReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (m *Mastodon) RemoveReaction(ctx context.Context, msgID, emoji string) error {
	return nil
}

func (m *Mastodon) SetTopic(ctx context.Context, channelID, text string) error {
	return nil
}

func (m *Mastodon) Topic(ctx context.Context, channelID string) string {
	return ""
}

func (m *Mastodon) GetLastSentMsgs() []string {
	return []string{}
}

func (m *Mastodon) Config() any {
	return &m.cfg.Current().Mastodon
}

func (m *Mastodon) BridgeConfig() *config.BridgeConfig {
	return &m.cfg.Current().Mastodon.Bridge
}

func (m *Mastodon) FormatterConfig() *config.FormatterConfig {
	return &m.cfg.Current().Mastodon.Formatter
}

func (m *Mastodon) GetReplayEvents(ctx context.Context, channelID string, since int64) []*bridge.Event {
	return []*bridge.Event{}
}
