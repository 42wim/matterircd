package matterclient

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/gorilla/websocket"
	"github.com/jpillora/backoff"
	"github.com/mattermost/platform/model"
)

type Credentials struct {
	Login         string
	Team          string
	Pass          string
	Server        string
	NoTLS         bool
	SkipTLSVerify bool
}

type Message struct {
	Raw      *model.Message
	Post     *model.Post
	Team     string
	Channel  string
	Username string
	Text     string
}

type Team struct {
	Team         *model.Team
	Id           string
	Channels     *model.ChannelList
	MoreChannels *model.ChannelList
	Users        map[string]*model.User
}

type MMClient struct {
	*Credentials
	Team        *Team
	OtherTeams  []*Team
	Client      *model.Client
	WsClient    *websocket.Conn
	WsQuit      bool
	WsAway      bool
	WsConnected bool
	User        *model.User
	Users       map[string]*model.User
	MessageChan chan *Message
	log         *log.Entry
}

func New(login, pass, team, server string) *MMClient {
	cred := &Credentials{Login: login, Pass: pass, Team: team, Server: server}
	mmclient := &MMClient{Credentials: cred, MessageChan: make(chan *Message, 100), Users: make(map[string]*model.User)}
	mmclient.log = log.WithFields(log.Fields{"module": "matterclient"})
	log.SetFormatter(&log.TextFormatter{FullTimestamp: true})
	return mmclient
}

func (m *MMClient) SetLogLevel(level string) {
	l, err := log.ParseLevel(level)
	if err != nil {
		log.SetLevel(log.InfoLevel)
		return
	}
	log.SetLevel(l)
}

func (m *MMClient) Login() error {
	m.WsConnected = false
	if m.WsQuit {
		return nil
	}
	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}
	uriScheme := "https://"
	wsScheme := "wss://"
	if m.NoTLS {
		uriScheme = "http://"
		wsScheme = "ws://"
	}
	// login to mattermost
	m.Client = model.NewClient(uriScheme + m.Credentials.Server)
	m.Client.HttpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: m.SkipTLSVerify}}
	var myinfo *model.Result
	var appErr *model.AppError
	var logmsg = "trying login"
	for {
		m.log.Debugf("%s %s %s %s", logmsg, m.Credentials.Team, m.Credentials.Login, m.Credentials.Server)
		if strings.Contains(m.Credentials.Pass, model.SESSION_COOKIE_TOKEN) {
			m.log.Debugf(logmsg+" with ", model.SESSION_COOKIE_TOKEN)
			token := strings.Split(m.Credentials.Pass, model.SESSION_COOKIE_TOKEN+"=")
			m.Client.HttpClient.Jar = m.createCookieJar(token[1])
			m.Client.MockSession(token[1])
			myinfo, appErr = m.Client.GetMe("")
			if myinfo.Data.(*model.User) == nil {
				m.log.Errorf("LOGIN TOKEN: %s is invalid", m.Credentials.Pass)
				return errors.New("invalid " + model.SESSION_COOKIE_TOKEN)
			}
		} else {
			myinfo, appErr = m.Client.Login(m.Credentials.Login, m.Credentials.Pass)
		}
		if appErr != nil {
			d := b.Duration()
			m.log.Debug(appErr.DetailedError)
			if !strings.Contains(appErr.DetailedError, "connection refused") &&
				!strings.Contains(appErr.DetailedError, "invalid character") {
				if appErr.Message == "" {
					return errors.New(appErr.DetailedError)
				}
				return errors.New(appErr.Message)
			}
			m.log.Debugf("LOGIN: %s, reconnecting in %s", appErr, d)
			time.Sleep(d)
			logmsg = "retrying login"
			continue
		}
		break
	}
	// reset timer
	b.Reset()

	err := m.initUser()
	if err != nil {
		return err
	}

	// set our team id as default route
	m.Client.SetTeamId(m.Team.Id)
	if m.Team == nil {
		return errors.New("team not found")
	}

	// setup websocket connection
	wsurl := wsScheme + m.Credentials.Server + "/api/v3/users/websocket"
	header := http.Header{}
	header.Set(model.HEADER_AUTH, "BEARER "+m.Client.AuthToken)

	m.log.Debug("WsClient: making connection")
	for {
		wsDialer := &websocket.Dialer{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{InsecureSkipVerify: m.SkipTLSVerify}}
		m.WsClient, _, err = wsDialer.Dial(wsurl, header)
		if err != nil {
			d := b.Duration()
			m.log.Debugf("WSS: %s, reconnecting in %s", err, d)
			time.Sleep(d)
			continue
		}
		break
	}
	b.Reset()

	// only start to parse WS messages when login is completely done
	m.WsConnected = true

	return nil
}

func (m *MMClient) WsReceiver() {
	var rmsg model.Message
	for {
		if m.WsQuit {
			m.log.Debug("exiting WsReceiver")
			return
		}
		if err := m.WsClient.ReadJSON(&rmsg); err != nil {
			m.log.Error("error:", err)
			// reconnect
			m.Login()
		}
		// we're not fully logged in yet.
		if !m.WsConnected {
			continue
		}
		if rmsg.Action == "ping" {
			m.handleWsPing()
			continue
		}
		msg := &Message{Raw: &rmsg, Team: m.Credentials.Team}
		m.parseMessage(msg)
		m.MessageChan <- msg
	}

}

func (m *MMClient) handleWsPing() {
	m.log.Debug("Ws PING")
	if !m.WsQuit && !m.WsAway {
		m.log.Debug("Ws PONG")
		m.WsClient.WriteMessage(websocket.PongMessage, []byte{})
	}
}

func (m *MMClient) parseMessage(rmsg *Message) {
	switch rmsg.Raw.Action {
	case model.ACTION_POSTED:
		m.parseActionPost(rmsg)
		/*
			case model.ACTION_USER_REMOVED:
				m.handleWsActionUserRemoved(&rmsg)
			case model.ACTION_USER_ADDED:
				m.handleWsActionUserAdded(&rmsg)
		*/
	}
}

func (m *MMClient) parseActionPost(rmsg *Message) {
	data := model.PostFromJson(strings.NewReader(rmsg.Raw.Props["post"]))
	// we don't have the user, refresh the userlist
	if m.Users[data.UserId] == nil {
		m.UpdateUsers()
	}
	rmsg.Username = m.Users[data.UserId].Username
	rmsg.Channel = m.GetChannelName(data.ChannelId)
	rmsg.Team = m.GetTeamName(rmsg.Raw.TeamId)
	// direct message
	if data.Type == "D" {
		rmsg.Channel = m.Users[data.UserId].Username
	}
	rmsg.Text = data.Message
	rmsg.Post = data
	return
}

func (m *MMClient) UpdateUsers() error {
	mmusers, _ := m.Client.GetProfilesForDirectMessageList(m.Team.Id)
	m.Users = mmusers.Data.(map[string]*model.User)
	return nil
}

func (m *MMClient) UpdateChannels() error {
	mmchannels, _ := m.Client.GetChannels("")
	m.Team.Channels = mmchannels.Data.(*model.ChannelList)
	mmchannels, _ = m.Client.GetMoreChannels("")
	m.Team.MoreChannels = mmchannels.Data.(*model.ChannelList)
	return nil
}

func (m *MMClient) GetChannelName(channelId string) string {
	for _, t := range m.OtherTeams {
		for _, channel := range append(t.Channels.Channels, t.MoreChannels.Channels...) {
			if channel.Id == channelId {
				return channel.Name
			}
		}
	}
	return ""
}

func (m *MMClient) GetChannelId(name string, teamId string) string {
	if teamId == "" {
		teamId = m.Team.Id
	}
	for _, t := range m.OtherTeams {
		if t.Id == teamId {
			for _, channel := range append(t.Channels.Channels, t.MoreChannels.Channels...) {
				if channel.Name == name {
					return channel.Id
				}
			}
		}
	}
	return ""
}

func (m *MMClient) GetChannelHeader(channelId string) string {
	for _, t := range m.OtherTeams {
		for _, channel := range append(t.Channels.Channels, t.MoreChannels.Channels...) {
			if channel.Id == channelId {
				return channel.Header
			}
		}
	}
	return ""
}

func (m *MMClient) PostMessage(channelId string, text string) {
	post := &model.Post{ChannelId: channelId, Message: text}
	m.Client.CreatePost(post)
}

func (m *MMClient) JoinChannel(channelId string) error {
	for _, c := range m.Team.Channels.Channels {
		if c.Id == channelId {
			m.log.Debug("Not joining ", channelId, " already joined.")
			return nil
		}
	}
	m.log.Debug("Joining ", channelId)
	_, err := m.Client.JoinChannel(channelId)
	if err != nil {
		return errors.New("failed to join")
	}
	return nil
}

func (m *MMClient) GetPostsSince(channelId string, time int64) *model.PostList {
	res, err := m.Client.GetPostsSince(channelId, time)
	if err != nil {
		return nil
	}
	return res.Data.(*model.PostList)
}

func (m *MMClient) SearchPosts(query string) *model.PostList {
	res, err := m.Client.SearchPosts(query, false)
	if err != nil {
		return nil
	}
	return res.Data.(*model.PostList)
}

func (m *MMClient) GetPosts(channelId string, limit int) *model.PostList {
	res, err := m.Client.GetPosts(channelId, 0, limit, "")
	if err != nil {
		return nil
	}
	return res.Data.(*model.PostList)
}

func (m *MMClient) GetPublicLink(filename string) string {
	res, err := m.Client.GetPublicLink(filename)
	if err != nil {
		return ""
	}
	return res.Data.(string)
}

func (m *MMClient) GetPublicLinks(filenames []string) []string {
	var output []string
	for _, f := range filenames {
		res, err := m.Client.GetPublicLink(f)
		if err != nil {
			continue
		}
		output = append(output, res.Data.(string))
	}
	return output
}

func (m *MMClient) UpdateChannelHeader(channelId string, header string) {
	data := make(map[string]string)
	data["channel_id"] = channelId
	data["channel_header"] = header
	m.log.Debugf("updating channelheader %#v, %#v", channelId, header)
	_, err := m.Client.UpdateChannelHeader(data)
	if err != nil {
		log.Error(err)
	}
}

func (m *MMClient) UpdateLastViewed(channelId string) {
	m.log.Debugf("posting lastview %#v", channelId)
	_, err := m.Client.UpdateLastViewedAt(channelId)
	if err != nil {
		m.log.Error(err)
	}
}

func (m *MMClient) UsernamesInChannel(channelId string) []string {
	ceiRes, err := m.Client.GetChannelExtraInfo(channelId, 5000, "")
	if err != nil {
		m.log.Errorf("UsernamesInChannel(%s) failed: %s", channelId, err)
		return []string{}
	}
	extra := ceiRes.Data.(*model.ChannelExtra)
	result := []string{}
	for _, member := range extra.Members {
		result = append(result, member.Username)
	}
	return result
}

func (m *MMClient) createCookieJar(token string) *cookiejar.Jar {
	var cookies []*http.Cookie
	jar, _ := cookiejar.New(nil)
	firstCookie := &http.Cookie{
		Name:   "MMAUTHTOKEN",
		Value:  token,
		Path:   "/",
		Domain: m.Credentials.Server,
	}
	cookies = append(cookies, firstCookie)
	cookieURL, _ := url.Parse("https://" + m.Credentials.Server)
	jar.SetCookies(cookieURL, cookies)
	return jar
}

// SendDirectMessage sends a direct message to specified user
func (m *MMClient) SendDirectMessage(toUserId string, msg string) {
	m.log.Debugf("SendDirectMessage to %s, msg %s", toUserId, msg)
	// create DM channel (only happens on first message)
	_, err := m.Client.CreateDirectChannel(toUserId)
	if err != nil {
		m.log.Debugf("SendDirectMessage to %#v failed: %s", toUserId, err)
	}
	channelName := model.GetDMNameFromIds(toUserId, m.User.Id)

	// update our channels
	mmchannels, _ := m.Client.GetChannels("")
	m.Team.Channels = mmchannels.Data.(*model.ChannelList)

	// build & send the message
	msg = strings.Replace(msg, "\r", "", -1)
	post := &model.Post{ChannelId: m.GetChannelId(channelName, ""), Message: msg}
	m.Client.CreatePost(post)
}

// GetTeamName returns the name of the specified teamId
func (m *MMClient) GetTeamName(teamId string) string {
	for _, t := range m.OtherTeams {
		if t.Id == teamId {
			return t.Team.Name
		}
	}
	return ""
}

// GetChannels returns all channels we're members off
func (m *MMClient) GetChannels() []*model.Channel {
	var channels []*model.Channel
	// our primary team channels first
	channels = append(channels, m.Team.Channels.Channels...)
	for _, t := range m.OtherTeams {
		if t.Id != m.Team.Id {
			channels = append(channels, t.Channels.Channels...)
		}
	}
	return channels
}

// GetMoreChannels returns existing channels where we're not a member off.
func (m *MMClient) GetMoreChannels() []*model.Channel {
	var channels []*model.Channel
	for _, t := range m.OtherTeams {
		channels = append(channels, t.MoreChannels.Channels...)
	}
	return channels
}

// GetTeamFromChannel returns teamId belonging to channel (DM channels have no teamId).
func (m *MMClient) GetTeamFromChannel(channelId string) string {
	var channels []*model.Channel
	for _, t := range m.OtherTeams {
		channels = append(channels, t.Channels.Channels...)
		for _, c := range channels {
			if c.Id == channelId {
				return t.Id
			}
		}
	}
	return ""
}

func (m *MMClient) GetLastViewedAt(channelId string) int64 {
	for _, t := range m.OtherTeams {
		if _, ok := t.Channels.Members[channelId]; ok {
			return t.Channels.Members[channelId].LastViewedAt
		}
	}
	return 0
}

// initialize user and teams
func (m *MMClient) initUser() error {
	m.log.Debug("initUser()")
	initLoad, err := m.Client.GetInitialLoad()
	if err != nil {
		return err
	}
	initData := initLoad.Data.(*model.InitialLoad)
	m.User = initData.User
	// we only load all team data on initial login.
	// all other updates are for channels from our (primary) team only.
	m.log.Debug("initUser(): loading all team data")
	for _, v := range initData.Teams {
		m.Client.SetTeamId(v.Id)
		mmusers, _ := m.Client.GetProfiles(v.Id, "")
		t := &Team{Team: v, Users: mmusers.Data.(map[string]*model.User), Id: v.Id}
		mmchannels, _ := m.Client.GetChannels("")
		t.Channels = mmchannels.Data.(*model.ChannelList)
		mmchannels, _ = m.Client.GetMoreChannels("")
		t.MoreChannels = mmchannels.Data.(*model.ChannelList)
		m.OtherTeams = append(m.OtherTeams, t)
		if v.Name == m.Credentials.Team {
			m.Team = t
			m.log.Debugf("initUser(): found our team %s (id: %s)", v.Name, v.Id)
		}
		// add all users
		for k, v := range t.Users {
			m.Users[k] = v
		}
	}
	return nil
}
