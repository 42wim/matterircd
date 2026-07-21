package matterclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	lru "github.com/hashicorp/golang-lru"
	"github.com/jpillora/backoff"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/mattermost/mattermost/server/public/model"
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
	MFAToken         string
}

type UsersCache struct {
	mu       sync.RWMutex
	users    map[string]*model.User
	channels map[string]map[string]struct{}
	teams    map[string]map[string]struct{}
	statuses map[string]string

	channelData    map[string]*model.Channel
	joinedChannels map[string]struct{}

	lastUpdated atomic.Int64
}

type Team struct {
	Team         *model.Team
	ID           string

	LastUserSync    time.Time
	LastChannelSync time.Time
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
	OtherTeams    map[string]*Team
	Client        *model.Client4
	User          *model.User
	Users         *UsersCache
	MessageChan   chan *Message
	WsClient      *model.WebSocketClient
	AntiIdle      bool
	AntiIdleChan  string
	AntiIdleIntvl int
	WsQuit        bool
	WsConnected   bool
	OnWsConnect   func()
	reconnectBusy bool
	Timeout       int

	logger      *logrus.Entry
	rootLogger  *logrus.Logger
	lruCache    *lru.Cache
	aliveChan   chan bool
	loginCancel context.CancelFunc

	lastWsActivity atomic.Int64
	connectedAt    atomic.Int64
}

var Matterircd bool

func New(login string, pass string, team string, server string, mfatoken string) *Client {
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: true,
		FullTimestamp: true,
	})

	cred := &Credentials{
		Login:    login,
		Pass:     pass,
		Team:     team,
		Server:   server,
		MFAToken: mfatoken,
	}

	cache, _ := lru.New(500)

	return &Client{
		Credentials: cred,
		MessageChan: make(chan *Message, 100),
		Users: &UsersCache{
			users:    make(map[string]*model.User),
			channels: make(map[string]map[string]struct{}),
			teams:    make(map[string]map[string]struct{}),
			statuses: make(map[string]string),

			channelData:    make(map[string]*model.Channel),
			joinedChannels: make(map[string]struct{}),
		},
		rootLogger: rootLogger,
		lruCache:   cache,
		logger:     rootLogger.WithFields(logrus.Fields{"prefix": "matterclient"}),
		aliveChan:  make(chan bool),
	}
}

// Login tries to connect the client with the loging details with which it was initialized.
func (m *Client) Login() error {
	// check if this is a first connect or a reconnection
	firstConnection := true
	if m.WsConnected {
		firstConnection = false
	}

	if !firstConnection {
		lastUpdatedUnix := m.Users.lastUpdated.Load()
		timeOffline := time.Since(time.Unix(lastUpdatedUnix, 0))

		if timeOffline > 15*time.Minute {
			m.logger.Info("reconnect: flushing channel user cache to ensure state consistency")

			m.Users.mu.Lock()
			m.Users.channels = make(map[string]map[string]struct{})
			m.Users.mu.Unlock()

			m.Users.lastUpdated.Store(time.Now().Unix())
		} else {
			m.logger.Debugf("reconnect: preserving channel user cache (offline for only %v)", timeOffline.Round(time.Second))
		}
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
		for _, t := range m.OtherTeams {
			validTeamNames = append(validTeamNames, t.Team.Name)
		}

		return fmt.Errorf("Team '%s' not found in %v", m.Credentials.Team, validTeamNames)
	}

	if err := m.initUserChannels(); err != nil {
		return err
	}

	// connect websocket
	m.wsConnect()

	ctx, loginCancel := context.WithCancel(context.TODO())
	m.loginCancel = loginCancel

	m.logger.Debug("starting wsreceiver")

	go m.WsReceiver(ctx)

	if m.WsClient != nil {
		m.logger.Debug("requesting initial user statuses for cache")
		m.WsClient.GetStatuses()
	}

	if m.OnWsConnect != nil {
		m.logger.Debug("executing OnWsConnect()")

		go m.OnWsConnect()
	}

	go m.checkConnection(ctx)

	if m.AntiIdle {
		if m.AntiIdleChan == "" {
			// do anti idle on town-square, every installation should have this channel
			m.AntiIdleChan = "town-square"
		}

		channels := m.GetChannels()
		for _, channel := range channels {
			if channel.Name == m.AntiIdleChan {
				go m.antiIdle(ctx, channel.Id, m.AntiIdleIntvl)

				continue
			}
		}
	}

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

	if m.Timeout == 0 {
		m.Timeout = 10
	}
	m.Client.HTTPClient.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
		},
		Proxy: http.ProxyFromEnvironment,

		// https://github.com/golang/go/issues/39299
		DialContext: (&net.Dialer{
			Timeout:   time.Second * time.Duration(m.Timeout),
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Second * time.Duration(m.Timeout),
		ExpectContinueTimeout: 1 * time.Second,

		// Additional tuning
		MaxIdleConnsPerHost: 10,
	}

	m.Client.HTTPClient.Timeout = time.Second * time.Duration(m.Timeout)

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
	case strings.Contains(m.Credentials.Pass, model.SessionCookieToken):
		token := strings.Split(m.Credentials.Pass, model.SessionCookieToken+"=")
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
		resp, err := m.Client.Logout(context.TODO())
		if err != nil {
			return err
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
	ctx := context.TODO()

	m.Lock()
	if m.OtherTeams == nil {
		m.OtherTeams = make(map[string]*Team)
	}
	userID := m.User.Id
	m.Unlock()

	var teams []*model.Team

	retryCount := 0
	for {
		var resp *model.Response
		var err error
		teams, resp, err = m.Client.GetTeamsForUser(ctx, userID, "")
		if err == nil {
			break
		}

		shouldRetry, hErr := m.HandleRetry("GetTeamsForUser", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return err
	}

	const batchSize = 200

	for _, team := range teams {
		m.Lock()
		existingTeam, exists := m.OtherTeams[team.Id]
		m.Unlock()

		if exists && time.Since(existingTeam.LastUserSync) < 15*time.Minute {
			m.logger.Debugf("skipping user fetch for team %s: cache is only %v old", team.Name, time.Since(existingTeam.LastUserSync).Round(time.Second))
			m.Lock()
			if team.Name == m.Credentials.Team {
				m.Team = existingTeam
			}
			m.Unlock()
			continue
		}

		m.logger.Debugf("fetching users for team %s (cache expired or missing)", team.Name)

		var teamUsers []*model.User

		idx := 0
		pageRetryCount := 0
		for {
			mmusers, resp, err := m.Client.GetUsersInTeam(ctx, team.Id, idx, batchSize, "")
			if err != nil {
				shouldRetry, hErr := m.HandleRetry("GetUsersInTeam", pageRetryCount, 10, resp)
				if hErr == nil && shouldRetry {
					pageRetryCount++
					continue
				}
				m.logger.Errorf("failed to fetch users for team %s: %v", team.Name, err)
				return err
			}
			pageRetryCount = 0

			teamUsers = append(teamUsers, mmusers...)

			if len(mmusers) < batchSize {
				break
			}
			idx++
			time.Sleep(time.Millisecond * 200)
		}
		m.logger.Debugf("found %d users in team %s", len(teamUsers), team.Name)

		m.Users.mu.Lock()
		if m.Users.teams == nil {
			m.Users.teams = make(map[string]map[string]struct{})
		}
		m.Users.teams[team.Id] = make(map[string]struct{})
		for _, u := range teamUsers {
			m.Users.users[u.Id] = u
			m.Users.teams[team.Id][u.Id] = struct{}{}
		}
		m.Users.lastUpdated.Store(time.Now().Unix())
		m.Users.mu.Unlock()

		m.Lock()
		m.OtherTeams[team.Id] = &Team{
			Team:         team,
			ID:           team.Id,
			LastUserSync: time.Now(),
		}

		if team.Name == m.Credentials.Team {
			m.Team = m.OtherTeams[team.Id]
			m.logger.Debugf("initUser(): found our team %s (id: %s)", team.Name, team.Id)
		}
		m.Unlock()
	}

	return nil
}

func (m *Client) initUserChannels() error {
	if err := m.UpdateChannels(); err != nil {
		return err
	}

	m.RLock()
	teams := make([]*Team, 0, len(m.OtherTeams))
	for _, t := range m.OtherTeams {
		teams = append(teams, t)
	}
	m.RUnlock()

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	var dmCount int
	for id, ch := range m.Users.channelData {
		if ch.TeamId == "" {
			if _, joined := m.Users.joinedChannels[id]; joined {
				dmCount++
			}
		}
	}
	m.logger.Debugf("found %d direct/group message channels", dmCount)

	for _, t := range teams {
		var joinedCount, publicCount int

		for id, ch := range m.Users.channelData {
			if ch.TeamId == t.ID {
				if _, joined := m.Users.joinedChannels[id]; joined {
					joinedCount++
				} else {
					publicCount++
				}
			}
		}

		m.logger.Debugf("found %d channels for user in team %s", joinedCount, t.Team.Name)
		m.logger.Debugf("found %d public channels in team %s", publicCount, t.Team.Name)
	}

	return nil
}

func (m *Client) doLogin(firstConnection bool, b *backoff.Backoff) error {
	ctx := context.TODO()
	var (
		logmsg = "trying login"
		err    error
		user   *model.User
	)

	for {
		m.logger.Debugf("%s %s %s %s", logmsg, m.Credentials.Team, m.Credentials.Login, m.Credentials.Server)

		switch {
		case m.Credentials.Token != "":
			user, _, err = m.doLoginToken()
			if err != nil {
				return err
			}
		case m.Credentials.MFAToken != "":
			user, _, err = m.Client.LoginWithMFA(ctx, m.Credentials.Login, m.Credentials.Pass, m.Credentials.MFAToken)
		default:
			user, _, err = m.Client.Login(ctx, m.Credentials.Login, m.Credentials.Pass)
		}

		if err != nil {
			d := b.Duration()

			m.logger.Debug(err)

			if firstConnection {
				return err
			}

			m.logger.Debugf("LOGIN: %s, reconnecting in %s", err, d)

			time.Sleep(d)

			logmsg = "retrying login"

			continue
		}

		m.User = user

		break
	}
	// reset timer
	b.Reset()

	return nil
}

func (m *Client) doLoginToken() (*model.User, *model.Response, error) {
	var (
		resp   *model.Response
		logmsg = "trying login"
		user   *model.User
		err    error
	)

	m.Client.AuthType = model.HeaderBearer
	m.Client.AuthToken = m.Credentials.Token

	if m.Credentials.CookieToken {
		m.logger.Debugf(logmsg + " with cookie (MMAUTH) token")
		m.Client.HTTPClient.Jar = m.createCookieJar(m.Credentials.Token)
	} else {
		m.logger.Debugf(logmsg + " with personal token")
	}

	user, resp, err = m.Client.GetMe(context.TODO(), "")
	if err != nil {
		return user, resp, err
	}

	if user == nil {
		m.logger.Errorf("LOGIN TOKEN: %s is invalid", m.Credentials.Pass)

		return user, resp, errors.New("invalid token")
	}

	return user, resp, nil
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
	header.Set(model.HeaderAuth, "BEARER "+m.Client.AuthToken)

	m.logger.Debugf("WsClient: making connection: %s", wsurl)

	for {
		wsDialer := &websocket.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
			},
			Proxy: http.ProxyFromEnvironment,
		}

		var err error

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

	m.lastWsActivity.Store(time.Now().Unix())
	m.connectedAt.Store(time.Now().Unix())

	m.logger.Debug("WsClient: connected")

	// only start to parse WS messages when login is completely done
	m.WsConnected = true
}

func (m *Client) doCheckAlive(ctx context.Context) error {
	if m.WsClient != nil && m.WsClient.ListenError != nil {
		return fmt.Errorf("websocket listen error: %w", m.WsClient.ListenError)
	}

	connectedUnix := m.connectedAt.Load()
	uptime := time.Since(time.Unix(connectedUnix, 0)).Round(time.Second)

	lastActiveUnix := m.lastWsActivity.Load()
	timeSinceActivity := time.Since(time.Unix(lastActiveUnix, 0))

	if timeSinceActivity < 20*time.Second {
		m.logger.Tracef("websocket is active (last event %v ago; up %s), skipping ping", timeSinceActivity.Round(time.Second), uptime)
		return nil
	}

	if timeSinceActivity < 55*time.Second {
		// Send a ping down the websocket to try to keep it active/alive
		if m.WsClient != nil {
			m.logger.Tracef("websocket has been quiet (last event %v ago; up %s), sending websocket ping", timeSinceActivity.Round(time.Second), uptime)
			m.WsClient.SendMessage("ping", nil)
			return nil
		}
	}

	m.logger.Tracef("websocket has been quiet (last event %v ago; up %s), falling back to HTTP GetPing", timeSinceActivity.Round(time.Second), uptime)
	if _, _, err := m.Client.GetPing(ctx); err != nil {
		m.logger.Warnf("fallback HTTP ping failed (up %s): %s", uptime, err)
		return fmt.Errorf("fallback HTTP ping failed (up %s): %w", uptime, err)
	}

	m.lastWsActivity.Store(time.Now().Unix())

	return nil
}

func (m *Client) checkAlive(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 30)

	for {
		select {
		case <-ctx.Done():
			m.logger.Debugf("checkAlive: ctx.Done() triggered")

			return
		case <-ticker.C:
			var err error

			// check if session still is valid
			for i := 0; i < 3; i++ { //nolint:intrange
				err = m.doCheckAlive(ctx)
				if err == nil {
					break
				}

				if i < 2 {
					m.logger.Warnf("alive check failed, retrying %d/3: %s", i+1, err)
					time.Sleep(time.Second * 2)
				}
			}

			if err != nil {
				m.logger.Errorf("connection not alive: %s", err)
				m.aliveChan <- false
				continue
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

				if m.doCheckAlive(ctx) != nil {
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

			m.lastWsActivity.Store(time.Now().Unix())

			go m.maintainUsersCache(ctx, event)

			msg := &Message{
				Raw:  event,
				Team: m.Credentials.Team,
			}

			if !Matterircd {
				m.parseMessage(msg)
			}

			select {
			case m.MessageChan <- msg:
				// Message sent successfully
			default:
				// Message channel buffer full, drop/discard
				m.logger.Errorf("CRITICAL: MessageChan is blocked! Downstream processor is hung. Dropping event: %s", event.EventType())
			}
		case response := <-m.WsClient.ResponseChannel:
			if response == nil || !response.IsValid() {
				continue
			}

			m.logger.Debugf("WsReceiver response: %#v", response)

			m.lastWsActivity.Store(time.Now().Unix())

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

	if strings.Contains(m.Credentials.Pass, model.SessionCookieToken) {
		m.logger.Debug("Not invalidating session in logout, credential is a token")

		return nil
	}

	// actually log out
	m.logger.Debug("running m.Client.Logout")

	if _, err := m.Client.Logout(context.TODO()); err != nil {
		return err
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

func (m *Client) HandleRatelimit(name string, resp *model.Response) error {
	if resp == nil {
		return fmt.Errorf("Got a nil model response from %s", name)
	}

	if resp.StatusCode != 429 {
		return fmt.Errorf("StatusCode error: %d", resp.StatusCode)
	}

	resetHeaderStr := resp.Header.Get("X-RateLimit-Reset") //nolint:canonicalheader
	if resetHeaderStr == "" {
		time.Sleep(3 * time.Second)
		return nil
	}

	headerValue, err := strconv.ParseInt(resetHeaderStr, 10, 64)
	if err != nil {
		return fmt.Errorf("failed to parse rate limit header: %v", err)
	}

	var waitTime int64

	// Handle epoch vs. delta
	if headerValue > 1000000 {
		now := time.Now().Unix()
		waitTime = headerValue - now
	} else {
		waitTime = headerValue
	}

	if waitTime < 1 {
		waitTime = 1
	}

	if waitTime > 300 {
		m.logger.Warnf("Rate limit reset time (%d seconds) looks suspiciously long; capping sleep to 5 seconds", waitTime)
		waitTime = 5
	}

	m.logger.Warnf("Ratelimited on %s for %d", name, waitTime)
	time.Sleep(time.Duration(waitTime) * time.Second)

	return nil
}

func (m *Client) HandleRetry(name string, current int, maxLimit int, resp *model.Response) (bool, error) {
	if current >= maxLimit {
		return false, nil
	}

	if resp == nil {
		m.logger.Warnf("Network error on %s (resp is nil), backing off: %ds (attempt %d/%d)",
			name, current+1, current+1, maxLimit)
		time.Sleep(time.Duration(current+1) * time.Second)
		return true, nil
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		_ = m.HandleRatelimit(name, resp)
		return true, nil

	case resp.StatusCode >= http.StatusInternalServerError:
		m.logger.Warnf("Transient error %d on %s, backing off: %ds (attempt %d/%d)",
			resp.StatusCode, name, current+1, current+1, maxLimit)
		time.Sleep(time.Duration(current+1) * time.Second)
		return true, nil

	case resp.StatusCode >= 300 && resp.StatusCode < 500:
		return false, nil

	default:
		time.Sleep(time.Duration(current+1) * time.Second)
		return true, nil
	}
}

func (m *Client) antiIdle(ctx context.Context, channelID string, interval int) {
	if interval == 0 {
		interval = 60
	}

	m.logger.Debugf("starting antiIdle for %s every %d secs", channelID, interval)
	ticker := time.NewTicker(time.Second * time.Duration(interval))

	for {
		select {
		case <-ctx.Done():
			m.logger.Debugf("antiIlde: ctx.Done() triggered, exiting for %s", channelID)

			return
		case <-ticker.C:
			m.logger.Tracef("antiIdle %s", channelID)

			m.UpdateLastViewed(channelID)
		}
	}
}

func (m *Client) UpdateTeamUsersCache(teamID string, user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.Users.users[user.Id] = user

	if teamID != "" {
		if m.Users.teams == nil {
			m.Users.teams = make(map[string]map[string]struct{})
		}
		if m.Users.teams[teamID] == nil {
			m.Users.teams[teamID] = make(map[string]struct{})
		}

		m.Users.teams[teamID][user.Id] = struct{}{}
	}

	m.Users.lastUpdated.Store(time.Now().Unix())
}

//nolint:unused
func (m *Client) syncSingleUser(ctx context.Context, event *model.WebSocketEvent) {
	userID, ok := event.GetData()["user_id"].(string)
	if !ok {
		return
	}

	user, _, err := m.Client.GetUser(ctx, userID, "")
	if err != nil {
		m.logger.Errorf("syncSingleUser failed to get user %s: %v", userID, err)
		return
	}

	m.logger.Debugf("dynamically caching updated/new user: %s", user.Username)

	teamID, _ := event.GetData()["team_id"].(string)

	m.UpdateTeamUsersCache(teamID, user)
}

//nolint:gocognit,gocyclo,funlen
func (m *Client) maintainUsersCache(ctx context.Context, event *model.WebSocketEvent) {
	switch event.EventType() {
	case model.WebsocketEventNewUser:
		if userID, ok := event.GetData()["user_id"].(string); ok {
			if user := m.GetUser(ctx, userID); user != nil {
				if teamID, hasTeam := event.GetData()["team_id"].(string); hasTeam && teamID != "" {
					m.UpdateTeamUsersCache(teamID, user)
				}
			}
		}

	case model.WebsocketEventUserUpdated:
		if userStr, ok := event.GetData()["user"].(string); ok {
			user := &model.User{}
			if err := json.Unmarshal([]byte(userStr), user); err == nil {
				if teamID, hasTeam := event.GetData()["team_id"].(string); hasTeam && teamID != "" {
					m.UpdateTeamUsersCache(teamID, user)
				} else {
					m.UpdateUser(user)
				}
			}
		}

	case model.WebsocketEventUserAdded:
		channelID := event.GetBroadcast().ChannelId
		if userID, ok := event.GetData()["user_id"].(string); ok && channelID != "" {
			if user := m.GetUser(ctx, userID); user != nil {
				m.UpdateChannelUsersCache(channelID, user)
				m.Users.lastUpdated.Store(time.Now().Unix())
			}
		}

	case model.WebsocketEventUserRemoved:
		channelID := event.GetBroadcast().ChannelId
		if userID, ok := event.GetData()["user_id"].(string); ok && channelID != "" {
			m.UpdateChannelUsersCacheRemove(channelID, userID)
			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventPosted:
		channelID := event.GetBroadcast().ChannelId
		if postStr, ok := event.GetData()["post"].(string); ok && channelID != "" {
			post := &model.Post{}
			if err := json.Unmarshal([]byte(postStr), post); err == nil {
				m.Users.mu.RLock()
				_, channelIsCached := m.Users.channels[channelID]
				_, userIsCached := m.Users.channels[channelID][post.UserId]
				m.Users.mu.RUnlock()

				// If we are actively caching this channel, but the user isn't in our list, our cache is stale!
				if channelIsCached && !userIsCached {
					m.logger.Warnf("Unrecognized user %s spoke in %s. Invalidating channel cache.", post.UserId, channelID)

					m.Users.mu.Lock()
					delete(m.Users.channels, channelID)
					m.Users.mu.Unlock()
					m.Users.lastUpdated.Store(time.Now().Unix())
				}
			}
		}

	case model.WebsocketEventChannelCreated, model.WebsocketEventDirectAdded:
		if channelStr, ok := event.GetData()["channel"].(string); ok {
			channel := &model.Channel{}
			if err := json.Unmarshal([]byte(channelStr), channel); err == nil {
				m.Users.mu.Lock()
				m.Users.channelData[channel.Id] = channel
				if channel.Type == model.ChannelTypeDirect || channel.Type == model.ChannelTypeGroup {
					m.Users.joinedChannels[channel.Id] = struct{}{}
				}
				m.Users.mu.Unlock()
				m.Users.lastUpdated.Store(time.Now().Unix())
			}
		} else if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
			m.GetChannel(ctx, channelID)
		}

	case model.WebsocketEventChannelUpdated:
		if channelStr, ok := event.GetData()["channel"].(string); ok {
			channel := &model.Channel{}
			if err := json.Unmarshal([]byte(channelStr), channel); err == nil {
				m.Users.mu.Lock()
				m.Users.channelData[channel.Id] = channel
				m.Users.mu.Unlock()
				m.Users.lastUpdated.Store(time.Now().Unix())
			}
		}

	case model.WebsocketEventChannelDeleted:
		if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
			// Mattermost soft-deletes channels. We keep the channel and users in our
			// local cache to gracefully handle history, references, or restorations.
			m.Users.lastUpdated.Store(time.Now().Unix())
		}
	}
}
