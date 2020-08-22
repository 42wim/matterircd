package matterclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	lru "github.com/hashicorp/golang-lru"
	"github.com/jpillora/backoff"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/sirupsen/logrus"
)

type Credentials struct {
	Login            string
	Team             string
	Pass             string
	Token            string
	CookieToken      bool
	Server           string
	NoTLS            bool
	SkipTLSVerify    bool
	SkipVersionCheck bool
}

type Team struct {
	Team         *model.Team
	ID           string
	Channels     []*model.Channel
	MoreChannels []*model.Channel
	Users        map[string]*model.User
}

type Message struct {
	Raw      *model.WebSocketEvent
	Post     *model.Post
	Team     string
	Channel  string
	Username string
	Text     string
	Type     string
	UserID   string
}

type Client struct {
	sync.RWMutex
	*Credentials

	Team          *Team
	OtherTeams    []*Team
	Client        *model.Client4
	User          *model.User
	Users         map[string]*model.User
	MessageChan   chan *Message
	WsClient      *model.WebSocketClient
	WsQuit        bool
	WsConnected   bool
	OnWsConnect   func()
	reconnectBusy bool

	logger      *logrus.Entry
	rootLogger  *logrus.Logger
	lruCache    *lru.Cache
	aliveChan   chan bool
	loginCancel context.CancelFunc
}

func New(login string, pass string, team string, server string) *Client {
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: true,
	})

	cred := &Credentials{
		Login:  login,
		Pass:   pass,
		Team:   team,
		Server: server,
	}

	cache, _ := lru.New(500)

	return &Client{
		Credentials: cred,
		MessageChan: make(chan *Message, 100),
		Users:       make(map[string]*model.User),
		rootLogger:  rootLogger,
		lruCache:    cache,
		logger:      rootLogger.WithFields(logrus.Fields{"prefix": "matterclient"}),
		aliveChan:   make(chan bool),
	}
}

// Login tries to connect the client with the loging details with which it was initialized.
func (m *Client) Login() error {
	// check if this is a first connect or a reconnection
	firstConnection := true
	if m.WsConnected {
		firstConnection = false
	}

	m.WsConnected = false
	if m.WsQuit {
		return nil
	}

	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}

	// do initialization setup
	if err := m.initClient(b); err != nil {
		return err
	}

	if err := m.doLogin(firstConnection, b); err != nil {
		return err
	}

	if err := m.initUser(); err != nil {
		return err
	}

	if m.Team == nil {
		validTeamNames := make([]string, len(m.OtherTeams))
		for i, t := range m.OtherTeams {
			validTeamNames[i] = t.Team.Name
		}

		return fmt.Errorf("Team '%s' not found in %v", m.Credentials.Team, validTeamNames)
	}

	// connect websocket
	m.wsConnect()

	ctx, loginCancel := context.WithCancel(context.Background())
	m.loginCancel = loginCancel

	m.logger.Debug("starting wsreceiver")
	go m.WsReceiver(ctx)

	if m.OnWsConnect != nil {
		m.logger.Debug("executing OnWsConnect()")

		go m.OnWsConnect()
	}

	go m.checkConnection(ctx)

	return nil
}

func (m *Client) Reconnect() {
	if m.reconnectBusy {
		return
	}
	m.reconnectBusy = true

	m.logger.Info("reconnect: logout")
	m.reconnectLogout()
	for {
		m.logger.Info("reconnect: login")
		err := m.Login()
		if err != nil {
			m.logger.Errorf("reconnect: login failed: %s, retrying in 10 seconds", err)
			time.Sleep(time.Second * 10)

			continue
		}

		break
	}

	m.logger.Info("reconnect successful")

	m.reconnectBusy = false
}

func (m *Client) initClient(b *backoff.Backoff) error {
	uriScheme := "https://"
	if m.NoTLS {
		uriScheme = "http://"
	}
	// login to mattermost
	m.Client = model.NewAPIv4Client(uriScheme + m.Credentials.Server)
	m.Client.HttpClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
		},
		Proxy: http.ProxyFromEnvironment,
	}
	m.Client.HttpClient.Timeout = time.Second * 10

	// handle MMAUTHTOKEN and personal token
	if err := m.handleLoginToken(); err != nil {
		return err
	}

	// check if server alive, retry until
	if err := m.serverAlive(b); err != nil {
		return err
	}

	return nil
}

func (m *Client) handleLoginToken() error {
	switch {
	case strings.Contains(m.Credentials.Pass, model.SESSION_COOKIE_TOKEN):
		token := strings.Split(m.Credentials.Pass, model.SESSION_COOKIE_TOKEN+"=")
		if len(token) != 2 {
			return errors.New("incorrect MMAUTHTOKEN. valid input is MMAUTHTOKEN=yourtoken")
		}

		m.Credentials.Token = token[1]
		m.Credentials.CookieToken = true
	case strings.Contains(m.Credentials.Pass, "token="):
		token := strings.Split(m.Credentials.Pass, "token=")
		if len(token) != 2 {
			return errors.New("incorrect personal token. valid input is token=yourtoken")
		}

		m.Credentials.Token = token[1]
	}

	return nil
}

func (m *Client) serverAlive(b *backoff.Backoff) error {
	defer b.Reset()

	for {
		d := b.Duration()
		// bogus call to get the serverversion
		_, resp := m.Client.Logout()
		if resp.Error != nil {
			return fmt.Errorf("%#v", resp.Error.Error())
		}

		if resp.ServerVersion == "" {
			m.logger.Debugf("Server not up yet, reconnecting in %s", d)
			time.Sleep(d)
		} else {
			m.logger.Infof("Found version %s", resp.ServerVersion)

			return nil
		}
	}
}

// initialize user and teams
// nolint:funlen
func (m *Client) initUser() error {
	m.Lock()
	defer m.Unlock()
	// we only load all team data on initial login.
	// all other updates are for channels from our (primary) team only.
	teams, resp := m.Client.GetTeamsForUser(m.User.Id, "")
	if resp.Error != nil {
		return resp.Error
	}

	for _, team := range teams {
		idx := 0
		max := 200
		usermap := make(map[string]*model.User)

		mmusers, resp := m.Client.GetUsersInTeam(team.Id, idx, max, "")
		if resp.Error != nil {
			return errors.New(resp.Error.DetailedError)
		}

		for len(mmusers) > 0 {
			for _, user := range mmusers {
				usermap[user.Id] = user
			}

			mmusers, resp = m.Client.GetUsersInTeam(team.Id, idx, max, "")
			if resp.Error != nil {
				return errors.New(resp.Error.DetailedError)
			}

			idx++

			time.Sleep(time.Millisecond * 200)
		}

		m.logger.Infof("found %d users in team %s", len(usermap), team.Name)

		t := &Team{
			Team:  team,
			Users: usermap,
			ID:    team.Id,
		}

		mmchannels, resp := m.Client.GetChannelsForTeamForUser(team.Id, m.User.Id, false, "")
		if resp.Error != nil {
			return resp.Error
		}

		t.Channels = mmchannels

		mmchannels, resp = m.Client.GetPublicChannelsForTeam(team.Id, 0, 5000, "")
		if resp.Error != nil {
			return resp.Error
		}

		t.MoreChannels = mmchannels
		m.OtherTeams = append(m.OtherTeams, t)

		if team.Name == m.Credentials.Team {
			m.Team = t
			m.logger.Debugf("initUser(): found our team %s (id: %s)", team.Name, team.Id)
		}
		// add all users
		for k, v := range t.Users {
			m.Users[k] = v
		}
	}

	return nil
}

func (m *Client) doLogin(firstConnection bool, b *backoff.Backoff) error {
	var (
		resp   *model.Response
		appErr *model.AppError
		logmsg = "trying login"
		err    error
	)

	for {
		m.logger.Debugf("%s %s %s %s", logmsg, m.Credentials.Team, m.Credentials.Login, m.Credentials.Server)

		if m.Credentials.Token != "" {
			resp, err = m.doLoginToken()
			if err != nil {
				return err
			}
		} else {
			m.User, resp = m.Client.Login(m.Credentials.Login, m.Credentials.Pass)
		}

		appErr = resp.Error
		if appErr != nil {
			d := b.Duration()
			m.logger.Debug(appErr.DetailedError)

			if firstConnection {
				if appErr.Message == "" {
					return errors.New(appErr.DetailedError)
				}

				return errors.New(appErr.Message)
			}

			m.logger.Debugf("LOGIN: %s, reconnecting in %s", appErr, d)

			time.Sleep(d)

			logmsg = "retrying login"

			continue
		}

		break
	}
	// reset timer
	b.Reset()

	return nil
}

func (m *Client) doLoginToken() (*model.Response, error) {
	var (
		resp   *model.Response
		logmsg = "trying login"
	)

	m.Client.AuthType = model.HEADER_BEARER
	m.Client.AuthToken = m.Credentials.Token

	if m.Credentials.CookieToken {
		m.logger.Debugf(logmsg + " with cookie (MMAUTH) token")
		m.Client.HttpClient.Jar = m.createCookieJar(m.Credentials.Token)
	} else {
		m.logger.Debugf(logmsg + " with personal token")
	}

	m.User, resp = m.Client.GetMe("")
	if resp.Error != nil {
		return resp, resp.Error
	}

	if m.User == nil {
		m.logger.Errorf("LOGIN TOKEN: %s is invalid", m.Credentials.Pass)

		return resp, errors.New("invalid token")
	}

	return resp, nil
}

func (m *Client) createCookieJar(token string) *cookiejar.Jar {
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

func (m *Client) wsConnect() {
	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}

	m.WsConnected = false
	wsScheme := "wss://"
	if m.NoTLS {
		wsScheme = "ws://"
	}

	// setup websocket connection
	wsurl := wsScheme + m.Credentials.Server
	// + model.API_URL_SUFFIX_V4
	// + "/websocket"
	header := http.Header{}
	header.Set(model.HEADER_AUTH, "BEARER "+m.Client.AuthToken)

	m.logger.Debugf("WsClient: making connection: %s", wsurl)
	for {
		wsDialer := &websocket.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
			},
			Proxy: http.ProxyFromEnvironment,
		}

		var err *model.AppError

		m.WsClient, err = model.NewWebSocketClientWithDialer(wsDialer, wsurl, m.Client.AuthToken)
		if err != nil {
			d := b.Duration()

			m.logger.Debugf("WSS: %s, reconnecting in %s", err, d)

			time.Sleep(d)

			continue
		}

		break
	}

	m.WsClient.Listen()

	m.logger.Debug("WsClient: connected")

	// only start to parse WS messages when login is completely done
	m.WsConnected = true
}

func (m *Client) doCheckAlive() error {
	_, resp := m.Client.GetMe("")
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Client) checkAlive(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 60)

	for {
		select {
		case <-ctx.Done():
			m.logger.Debugf("checkAlive: ctx.Done() triggered")

			return
		case <-ticker.C:
			// check if session still is valid
			err := m.doCheckAlive()
			if err != nil {
				m.logger.Errorf("connection not alive: %s", err)
				m.aliveChan <- false
			}

			m.aliveChan <- true
		}
	}
}

func (m *Client) checkConnection(ctx context.Context) {
	go m.checkAlive(ctx)

	for {
		select {
		case alive := <-m.aliveChan:
			if !alive {
				time.Sleep(time.Second * 10)
				if m.doCheckAlive() != nil {
					m.Reconnect()
				}
			}
		case <-ctx.Done():
			m.logger.Debug("checkConnection: ctx.Done() triggered, exiting")

			return
		}
	}
}

// WsReceiver implements the core loop that manages the connection to the chat server. In
// case of a disconnect it will try to reconnect. A call to this method is blocking until
// the 'WsQuite' field of the MMClient object is set to 'true'.
func (m *Client) WsReceiver(ctx context.Context) {
	m.logger.Debug("starting WsReceiver")
	ticker := time.NewTicker(time.Second * 10)
	for {
		select {
		case event := <-m.WsClient.EventChannel:
			if event == nil {
				return
			}

			if !event.IsValid() {
				continue
			}

			m.logger.Debugf("WsReceiver event: %#v", event)

			msg := &Message{
				Raw:  event,
				Team: m.Credentials.Team,
			}

			m.MessageChan <- msg
		case response := <-m.WsClient.ResponseChannel:
			if response == nil || !response.IsValid() {
				continue
			}

			m.logger.Debugf("WsReceiver response: %#v", response)
			m.parseResponse(response)
		case <-m.WsClient.PingTimeoutChannel:
			m.logger.Error("got a ping timeout")
			m.Reconnect()

			return
		case <-ticker.C:
			if m.WsClient.ListenError != nil {
				m.logger.Errorf("%#v", m.WsClient.ListenError)
				m.Reconnect()

				return
			}
		case <-ctx.Done():
			m.logger.Debugf("wsReceiver: ctx.Done() triggered")

			return
		}
	}
}

// Logout disconnects the client from the chat server.
func (m *Client) reconnectLogout() error {
	err := m.Logout()
	m.WsQuit = false

	if err != nil {
		return err
	}

	return nil
}

// Logout disconnects the client from the chat server.
func (m *Client) Logout() error {
	m.logger.Debug("logout running loginCancel to exit goroutines")
	m.loginCancel()

	m.logger.Debugf("logout as %s (team: %s) on %s", m.Credentials.Login, m.Credentials.Team, m.Credentials.Server)
	m.WsQuit = true
	// close the websocket
	m.logger.Debug("closing websocket")
	m.WsClient.Close()

	if strings.Contains(m.Credentials.Pass, model.SESSION_COOKIE_TOKEN) {
		m.logger.Debug("Not invalidating session in logout, credential is a token")

		return nil
	}

	// actually log out
	m.logger.Debug("running m.Client.Logout")
	_, resp := m.Client.Logout()
	if resp.Error != nil {
		return resp.Error
	}

	m.logger.Debug("exiting Logout()")

	return nil
}

// SetLogLevel tries to parse the specified level and if successful sets
// the log level accordingly. Accepted levels are: 'debug', 'info', 'warn',
// 'error', 'fatal' and 'panic'.
func (m *Client) SetLogLevel(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		m.logger.Warnf("Failed to parse specified log-level '%s': %#v", level, err)
	} else {
		m.rootLogger.SetLevel(l)
	}
}
