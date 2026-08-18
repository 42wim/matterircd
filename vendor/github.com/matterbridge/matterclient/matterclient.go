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
	lru "github.com/hashicorp/golang-lru/v2"
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

type ChannelSummary struct {
	Id          string `json:"id"`
	UpdateAt    int64  `json:"update_at"`
	DeleteAt    int64  `json:"delete_at"`
	TeamId      string `json:"team_id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Name        string `json:"name"`
	Header      string `json:"header"`
	Purpose     string `json:"purpose"`
	LastPostAt  int64  `json:"last_post_at"`
	CreatorId   string `json:"creator_id"`
}

type CustomStatus struct {
	Emoji     string `json:"emoji"`
	Text      string `json:"text"`
	Duration  string `json:"duration"`
	ExpiresAt string `json:"expires_at"`
}

type FileInfo struct {
	Name string
	Size int64
	URL  string
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

type UsersCache struct {
	mu       sync.RWMutex
	users    map[string]*model.User
	channels map[string]map[string]struct{}
	teams    map[string]map[string]struct{}
	statuses map[string]string

	channelData    map[string]*model.Channel
	joinedChannels map[string]struct{}

	channelLastViewedAt map[string]int64

	customStatuses map[string]string

	lastUpdated atomic.Int64
}

type UserSummary struct {
	Id        string            `json:"id"`
	Username  string            `json:"username"`
	FirstName string            `json:"first_name"`
	LastName  string            `json:"last_name"`
	Nickname  string            `json:"nickname"`
	Roles     string            `json:"roles"`
	Props     map[string]string `json:"props"`
}

type Team struct {
	Team *model.Team
	ID   string

	LastUserSync    time.Time
	LastChannelSync time.Time
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

	logger     *logrus.Entry
	rootLogger *logrus.Logger

	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger

	lruCache  *lru.Cache[string, bool]
	postCache *lru.Cache[string, *model.Post]

	aliveChan   chan bool
	loginCancel context.CancelFunc

	lastWsActivity atomic.Int64
	connectedAt    atomic.Int64

	ForceSyncOnReconnect bool
	CacheClearCutoff     time.Duration

	UserAgent string

	//nolint:containedctx // Tied to the lifecycle of the persistent client session
	Ctx context.Context
}

// UserAgentTransport wraps an existing RoundTripper to inject a User-Agent header
type UserAgentTransport struct {
	Transport http.RoundTripper
	UserAgent string
}

var Matterircd bool

const (
	schemeHTTPS = "https://"
	schemeHTTP  = "http://"
)

// Mattermost has a hardcoded `PerPageMaximum = 200` & `LimitMaximum = 200`
// See https://github.com/mattermost/mattermost/blob/master/server/channels/web/params.go
const mattermostPerPageMax = 200

func New(login string, pass string, team string, server string, mfatoken string) *Client {
	// Logger for the rest of matterclient
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Logger for Mattermost API calls
	rootAPILogger := logrus.New()
	rootAPILogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Default to higher than "warn" to not log anything
	rootAPILogger.SetLevel(logrus.ErrorLevel)

	cred := &Credentials{
		Login:    login,
		Pass:     pass,
		Team:     team,
		Server:   server,
		MFAToken: mfatoken,
	}

	cache, _ := lru.New[string, bool](500)
	postCache, _ := lru.New[string, *model.Post](2000)

	defaultUserAgent := "matterclient/0.0"

	return &Client{
		Credentials: cred,
		MessageChan: make(chan *Message, 100),
		Users: &UsersCache{
			users:    make(map[string]*model.User, 1000),
			channels: make(map[string]map[string]struct{}, 1000),
			teams:    make(map[string]map[string]struct{}, 10),
			statuses: make(map[string]string, 1000),

			channelData:    make(map[string]*model.Channel, 1000),
			joinedChannels: make(map[string]struct{}, 200),

			channelLastViewedAt: make(map[string]int64, 1000),
		},
		lruCache:  cache,
		postCache: postCache,

		rootLogger: rootLogger,
		logger:     rootLogger.WithFields(logrus.Fields{"prefix": "matterclient"}),

		rootAPILogger: rootAPILogger,
		apiLogger:     rootAPILogger.WithFields(logrus.Fields{"prefix": "matterclient: API"}),

		aliveChan: make(chan bool),

		ForceSyncOnReconnect: false,
		CacheClearCutoff:     15 * time.Minute,

		UserAgent: defaultUserAgent,
	}
}

// Login tries to connect the client with the loging details with which it was initialized.
//
//nolint:funlen,gocyclo
func (m *Client) Login(ctx context.Context) error {
	// check if this is a first connect or a reconnection
	firstConnection := true
	if m.WsConnected {
		firstConnection = false
	}

	if !firstConnection {
		lastUpdatedUnix := m.Users.lastUpdated.Load()
		timeOffline := time.Since(time.Unix(lastUpdatedUnix, 0))

		switch {
		case timeOffline > m.CacheClearCutoff && m.ForceSyncOnReconnect:
			m.logger.Info("reconnect: flushing channel user cache to ensure state consistency")

			m.Users.mu.Lock()
			m.Users.channels = make(map[string]map[string]struct{}, 1000)
			m.Users.mu.Unlock()

			m.Users.lastUpdated.Store(time.Now().Unix())
		case timeOffline > m.CacheClearCutoff:
			m.logger.Debug("reconnect: skipping channel user cache flush (ForceSyncOnReconnect is disabled)")
		default:
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
	if err := m.initClient(ctx, b); err != nil {
		return err
	}

	if err := m.doLogin(ctx, firstConnection, b); err != nil {
		return err
	}

	if err := m.initUser(ctx); err != nil {
		return err
	}

	if m.Team == nil {
		validTeamNames := make([]string, 0, len(m.OtherTeams))
		for _, t := range m.OtherTeams {
			validTeamNames = append(validTeamNames, t.Team.Name)
		}

		return fmt.Errorf("Team '%s' not found in %v", m.Credentials.Team, validTeamNames)
	}

	if err := m.initUserChannels(ctx); err != nil {
		return err
	}

	// Connect websocket using the short-lived Login operation context.
	// If the login operation is canceled, this will cleanly abort.
	m.wsConnect(ctx)

	// Prepare the long-lived background context for goroutines.
	parentCtx := m.Ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	bgCtx, loginCancel := context.WithCancel(parentCtx)
	m.loginCancel = loginCancel

	m.logger.Trace("executing WsReceiver()")
	// Pass the long-lived background context to the goroutines.
	// They will run until m.loginCancel() is called during Logout.
	//nolint:contextcheck
	go m.WsReceiver(bgCtx)

	if m.WsClient != nil {
		m.logger.Trace("requesting initial user statuses for cache")
		m.WsClient.GetStatuses()
	}

	if m.OnWsConnect != nil {
		m.logger.Trace("executing OnWsConnect()")
		go m.OnWsConnect()
	}

	//nolint:contextcheck // Use the long-lived background context (bgCtx) here instead of ctx
	go m.checkConnection(bgCtx)

	if m.AntiIdle {
		if m.AntiIdleChan == "" {
			// do anti idle on town-square, every installation should have this channel
			m.AntiIdleChan = "town-square"
		}

		channels := m.GetChannels()
		for _, channel := range channels {
			if channel.Name == m.AntiIdleChan {
				//nolint:contextcheck // Use the long-lived background context (bgCtx) here instead of ctx
				go m.antiIdle(bgCtx, channel.Id, m.AntiIdleIntvl)

				continue
			}
		}
	}

	return nil
}

//nolint:contextcheck
func (m *Client) Reconnect(ctx context.Context) {
	if m.reconnectBusy {
		return
	}

	m.reconnectBusy = true
	defer func() { m.reconnectBusy = false }()

	// Use the persistent parent session context, not the worker's local context!
	// The worker's local context gets murdered by m.reconnectLogout.
	sessionCtx := m.Ctx
	if sessionCtx == nil {
		sessionCtx = context.Background()
	}

	m.logger.Info("reconnect: logout")
	m.reconnectLogout(sessionCtx)

	for {
		// Check if the actual IRC user disconnected
		if sessionCtx.Err() != nil {
			m.logger.Errorf("reconnect aborted: %v", sessionCtx.Err())
			return
		}

		m.logger.Info("reconnect: login")

		err := m.Login(sessionCtx)
		if err != nil {
			m.logger.Errorf("reconnect: login failed: %s, retrying in 10 seconds", err)
			select {
			case <-sessionCtx.Done():
				m.logger.Errorf("reconnect aborted during backoff: %v", sessionCtx.Err())
				return
			case <-time.After(time.Second * 10):
				continue
			}
		}

		break
	}

	m.logger.Info("reconnect successful")
}

// RoundTrip executes a single HTTP transaction, adding the User-Agent
func (t *UserAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	reqCopy := req.Clone(req.Context())
	reqCopy.Header.Set("User-Agent", t.UserAgent)

	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	return transport.RoundTrip(reqCopy)
}

func (m *Client) initClient(ctx context.Context, b *backoff.Backoff) error {
	uriScheme := schemeHTTPS
	if m.NoTLS {
		uriScheme = schemeHTTP
	}
	// login to mattermost
	m.Client = model.NewAPIv4Client(uriScheme + m.Credentials.Server)

	if m.Timeout == 0 {
		m.Timeout = 10
	}

	// Create the base transport with existing tuning
	baseTransport := &http.Transport{
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

		DisableCompression: true,
	}

	// Wrap the base transport to inject the User-Agent
	m.Client.HTTPClient.Transport = &UserAgentTransport{
		Transport: baseTransport,
		UserAgent: m.UserAgent,
	}

	m.Client.HTTPClient.Timeout = time.Second * time.Duration(m.Timeout)

	// handle MMAUTHTOKEN and personal token
	if err := m.handleLoginToken(); err != nil {
		return err
	}

	// check if server alive, retry until
	if err := m.serverAlive(ctx, b); err != nil {
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

func (m *Client) serverAlive(ctx context.Context, b *backoff.Backoff) error {
	if b != nil {
		defer b.Reset()
	}

	retryCount := 0
	for {
		if m.IsAborted(ctx) || ctx.Err() != nil {
			return errors.New("login aborted")
		}

		// bogus call to get the serverversion
		m.apiLogger.Infof("serverAlive: Logout check #%d", retryCount)
		resp, err := m.Client.Logout(ctx)

		// Success condition: No error, and we have a server version
		if err == nil && resp != nil && resp.ServerVersion != "" {
			m.logger.Infof("Found version %s", resp.ServerVersion)

			return nil
		}

		// If the API succeeded but the server version is empty,
		// we construct an error so HandleRetry knows to back off.
		if err == nil {
			err = errors.New("server up but returned empty ServerVersion")
		}

		// We use a higher maxLimit here (e.g. 30) because a server restart can
		// take a few minutes. 30 retries = ~7.5 minutes of total backoff time.
		shouldRetry, hErr := m.HandleRetry(ctx, "serverAlive", err, retryCount, 30, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("serverAlive check failed: %v", err)

		return err
	}
}

// initialize user and teams
//
//nolint:funlen,gocognit,gocyclo
func (m *Client) initUser(ctx context.Context) error {
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
		m.apiLogger.Warnf("initUser: GetTeamsForUser: UserID %s #%d", userID, retryCount)
		teams, resp, err = m.Client.GetTeamsForUser(ctx, userID, "")
		if err == nil {
			break
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetTeamsForUser", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return err
	}

	const batchSize = mattermostPerPageMax

	for _, team := range teams {
		if m.IsAborted(ctx) {
			return errors.New("login aborted")
		}

		m.Lock()
		existingTeam, exists := m.OtherTeams[team.Id]
		if team.Name == m.Credentials.Team {
			m.logger.Debugf("found our team %s (id: %s)", team.Name, team.Id)
		}
		m.Unlock()

		if exists && (!m.ForceSyncOnReconnect || time.Since(existingTeam.LastUserSync) < m.CacheClearCutoff) {
			m.logger.Debugf("skipping user fetch for team %s: cache is only %v old", team.Name, time.Since(existingTeam.LastUserSync).Round(time.Second))
			m.Lock()
			if team.Name == m.Credentials.Team {
				m.Team = existingTeam
			}
			m.Unlock()
			continue
		}

		m.logger.Tracef("fetching users for team %s (cache expired or missing)", team.Name)

		fetchedUsers := make([]UserSummary, 0, batchSize)

		idx := 0
		pageRetryCount := 0
		for {
			if m.IsAborted(ctx) {
				return errors.New("login aborted")
			}

			query := "/users?in_team=" + team.Id + "&page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
			m.apiLogger.Warnf("initUser: DoAPIGet: query %s #%d", query, retryCount)
			resp, err := m.Client.DoAPIGet(ctx, query, "")
			if err != nil {
				var mResp *model.Response
				if resp != nil {
					mResp = model.BuildResponse(resp)
				}
				shouldRetry, hErr := m.HandleRetry(ctx, "GetUsersInTeam", err, pageRetryCount, 10, mResp)
				if hErr == nil && shouldRetry {
					pageRetryCount++
					continue
				}
				m.logger.Errorf("failed to fetch users for team %s: %v", team.Name, err)
				return err
			}
			pageRetryCount = 0

			var list []UserSummary
			if jsonErr := json.NewDecoder(resp.Body).Decode(&list); jsonErr != nil {
				resp.Body.Close()
				return jsonErr
			}
			resp.Body.Close()

			fetchedUsers = append(fetchedUsers, list...)
			if len(list) < batchSize {
				break
			}

			idx++
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Millisecond * 200):
			}
		}
		if !exists {
			m.logger.Debugf("found %d users in team %s", len(fetchedUsers), team.Name)
		} else {
			m.logger.Debugf("found %d users in team %s (cache expired)", len(fetchedUsers), team.Name)
		}

		m.Users.mu.Lock()
		if m.Users.teams == nil {
			m.Users.teams = make(map[string]map[string]struct{})
		}
		m.Users.teams[team.Id] = make(map[string]struct{}, len(fetchedUsers))

		for _, u := range fetchedUsers {
			roles := u.Roles
			if roles == "system_user" {
				roles = "system_user"
			} else if roles == "system_admin system_user" {
				roles = "system_admin system_user"
			}

			cachedUser, exists := m.Users.users[u.Id]
			if !exists { //nolint:nestif,dupl
				cachedUser = &model.User{
					Id:        u.Id,
					Username:  u.Username,
					FirstName: u.FirstName,
					LastName:  u.LastName,
					Nickname:  u.Nickname,
					Roles:     roles,
					Props:     u.Props,
				}
				m.Users.users[u.Id] = cachedUser
			} else {
				if cachedUser.Username != u.Username {
					cachedUser.Username = u.Username
				}
				if cachedUser.FirstName != u.FirstName {
					cachedUser.FirstName = u.FirstName
				}
				if cachedUser.LastName != u.LastName {
					cachedUser.LastName = u.LastName
				}
				if cachedUser.Nickname != u.Nickname {
					cachedUser.Nickname = u.Nickname
				}
				if cachedUser.Roles != roles {
					cachedUser.Roles = roles
				}
				if u.Props != nil {
					cachedUser.Props = u.Props
				}
			}

			m.Users.teams[team.Id][cachedUser.Id] = struct{}{}
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
		}
		m.Unlock()
	}

	return nil
}

func (m *Client) initUserChannels(ctx context.Context) error {
	if err := m.UpdateChannels(ctx); err != nil {
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

	for _, t := range teams {
		var joinedChannels, directMessages, publicCount int

		for id, ch := range m.Users.channelData {
			if ch.TeamId != t.ID {
				continue
			}

			if _, joined := m.Users.joinedChannels[id]; joined {
				switch ch.Type {
				case model.ChannelTypeDirect, model.ChannelTypeGroup:
					directMessages++
				default:
					joinedChannels++
				}
			} else {
				publicCount++
			}
		}

		m.logger.Debugf("found %d channels for user (%d channels, %d DMs/GMs) in team %s",
			joinedChannels+directMessages, joinedChannels, directMessages, t.Team.Name)
		m.logger.Debugf("found %d public channels in team %s", publicCount, t.Team.Name)
	}

	return nil
}

func (m *Client) doLogin(ctx context.Context, firstConnection bool, b *backoff.Backoff) error {
	// b is kept for signature compatibility.
	if b != nil {
		defer b.Reset()
	}

	var (
		logmsg = "trying login"
		err    error
		resp   *model.Response
		user   *model.User
	)

	retryCount := 0
	for {
		if m.IsAborted(ctx) || ctx.Err() != nil {
			return errors.New("login aborted")
		}

		m.logger.Debugf("%s %s %s %s #%d", logmsg, m.Credentials.Team, m.Credentials.Login, m.Credentials.Server, retryCount)

		// Attempt the appropriate login method
		switch {
		case m.Credentials.Token != "":
			user, resp, err = m.doLoginToken(ctx)
		case m.Credentials.MFAToken != "":
			m.apiLogger.Info("doLogin: LoginWithMFA")
			user, resp, err = m.Client.LoginWithMFA(ctx, m.Credentials.Login, m.Credentials.Pass, m.Credentials.MFAToken)
		default:
			m.apiLogger.Info("doLogin: Login")
			user, resp, err = m.Client.Login(ctx, m.Credentials.Login, m.Credentials.Pass)
		}

		// Success condition
		if err == nil && user != nil {
			m.User = user

			return nil
		}

		//  Log the error and handle the firstConnection fail-fast behavior
		if err != nil {
			m.logger.Debug(err)
		}

		if firstConnection {
			return err
		}

		// Construct an error if user is somehow nil but the HTTP request didn't fail
		if err == nil && user == nil {
			err = errors.New("login succeeded but returned a nil user")
		}

		// Let HandleRetry dictate if we should loop.
		// We use a high maxLimit (e.g., 60) so it can survive an extended server outage (like a 1-hour DB maintenance)
		shouldRetry, hErr := m.HandleRetry(ctx, "doLogin", err, retryCount, 60, resp)
		if hErr == nil && shouldRetry {
			logmsg = "retrying login"
			retryCount++

			continue
		}

		m.logger.Errorf("LOGIN failed: %v", err)

		return err
	}
}

func (m *Client) doLoginToken(ctx context.Context) (*model.User, *model.Response, error) {
	var (
		resp   *model.Response
		logmsg = "trying login"
		user   *model.User
		err    error
	)

	m.Client.AuthType = model.HeaderBearer
	m.Client.AuthToken = m.Credentials.Token

	if m.Credentials.CookieToken {
		m.logger.Debugf("%s with cookie (MMAUTH) token", logmsg)
		m.Client.HTTPClient.Jar = m.createCookieJar(m.Credentials.Token)
	} else {
		m.logger.Debugf("%s with personal token", logmsg)
	}

	m.apiLogger.Info("doLoginToken: GetMe")

	user, resp, err = m.Client.GetMe(ctx, "")
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
	cookieURL, _ := url.Parse(schemeHTTPS + m.Server)

	jar.SetCookies(cookieURL, cookies)

	return jar
}

func (m *Client) wsConnect(ctx context.Context) {
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

	m.logger.Tracef("WsClient: making connection: %s", wsurl)

	for {
		if ctx.Err() != nil {
			m.logger.Errorf("wsConnect aborted: %v", ctx.Err())
			return
		}

		wsDialer := &websocket.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
			},
			Proxy:             http.ProxyFromEnvironment,
			EnableCompression: false,
		}

		var err error

		m.WsClient, err = model.NewWebSocketClientWithDialer(wsDialer, wsurl, m.Client.AuthToken)
		if err != nil {
			d := b.Duration()

			m.logger.Debugf("WSS: %s, reconnecting in %s", err, d)

			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}

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
	m.apiLogger.Info("doCheckAlive: GetPing")
	if _, _, err := m.Client.GetPing(ctx); err != nil {
		m.logger.Warnf("fallback HTTP ping failed (up %s): %s", uptime, err)
		return fmt.Errorf("fallback HTTP ping failed (up %s): %w", uptime, err)
	}

	m.lastWsActivity.Store(time.Now().Unix())

	return nil
}

func (m *Client) checkAlive(ctx context.Context) {
	ticker := time.NewTicker(time.Second * 30)
	defer ticker.Stop()

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
					select {
					case <-ctx.Done():
						m.logger.Debugf("checkAlive: ctx.Done() triggered during backoff")
						return
					case <-time.After(time.Second * 2):
					}
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
				select {
				case <-ctx.Done():
					m.logger.Debug("checkConnection: ctx.Done() triggered during reconnect wait, exiting")
					return
				case <-time.After(time.Second * 10):
				}

				if m.doCheckAlive(ctx) != nil {
					m.Reconnect(ctx)
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

			eventType := event.EventType()
			if m.rootLogger.IsLevelEnabled(logrus.DebugLevel) { //nolint:nestif
				userInfo := ""
				data := event.GetData()

				if userPtr, ok := data["user"].(*model.User); ok {
					if userPtr.Username != "" {
						userInfo = " [User: " + userPtr.Username + " (ID: " + userPtr.Id + ")]"
					}
				} else if userStr, ok := data["user"].(string); ok && userStr != "" {
					var summary UserSummary
					_ = json.NewDecoder(strings.NewReader(userStr)).Decode(&summary)
					if summary.Username != "" {
						userInfo = " [User: " + summary.Username + " (ID: " + summary.Id + ")]"
					}
				} else if userID, ok := data["user_id"].(string); ok && userID != "" {
					userInfo = " [UserID: " + userID + "]"
				}

				if eventType == model.WebsocketEventTyping {
					m.logger.Tracef("WsReceiver event%s: %#v", userInfo, event)
				} else {
					m.logger.Debugf("WsReceiver event%s: %#v", userInfo, event)
				}
			}

			m.lastWsActivity.Store(time.Now().Unix())

			// Synchronously update local cache to avoid race conditions downstream
			m.syncJoinedChannelsCache(event)

			go m.maintainUsersCache(ctx, event)

			msg := &Message{
				Raw:  event,
				Team: m.Credentials.Team,
			}

			if !Matterircd {
				m.parseMessage(ctx, msg)
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

			text, ok := response.Data["text"].(string)
			if ok && text == "pong" {
				m.logger.Tracef("WsReceiver response: %#v", response)
			} else {
				m.logger.Debugf("WsReceiver response: %#v", response)
			}

			m.lastWsActivity.Store(time.Now().Unix())

			m.parseResponse(response)
		case <-m.WsClient.PingTimeoutChannel:
			m.logger.Error("got a ping timeout")
			m.Reconnect(ctx)

			return
		case <-ticker.C:
			if m.WsClient.ListenError != nil {
				m.logger.Errorf("%#v", m.WsClient.ListenError)
				m.Reconnect(ctx)

				return
			}
		case <-ctx.Done():
			m.logger.Debugf("wsReceiver: ctx.Done() triggered")

			return
		}
	}
}

// reconnectLogout disconnects the client from the chat server for an internal reconnect.
func (m *Client) reconnectLogout(ctx context.Context) {
	err := m.Logout(ctx)

	// Always reset WsQuit so the subsequent Login() doesn't instantly abort
	m.WsQuit = false

	if err != nil {
		m.logger.Debugf("reconnectLogout encountered error during teardown: %v", err)
		// We deliberately don't return the error here.
		// A failure to cleanly logout should NOT stop us from trying to reconnect!
	}
}

// Logout disconnects the client from the chat server.
func (m *Client) Logout(ctx context.Context) error {
	m.logger.Debug("logout running loginCancel to exit goroutines")
	if m.loginCancel != nil {
		m.loginCancel()
	}

	m.logger.Debugf("logout as %s (team: %s) on %s", m.Credentials.Login, m.Credentials.Team, m.Credentials.Server)
	m.WsQuit = true

	// close the websocket
	if m.WsClient != nil {
		m.logger.Debug("closing websocket")
		m.WsClient.Close()
	}

	// NOTE: We no longer wipe the m.Users maps here.
	// In Bouncer mode, the cache survives for fast reconnects and is validated by Login().
	// In Dynamic mode, this entire Client struct will be Garbage Collected once the IRC session dies.

	if strings.Contains(m.Credentials.Pass, model.SessionCookieToken) {
		m.logger.Debug("Not invalidating session in logout, credential is a token")
		return nil
	}

	// actually log out
	m.logger.Debug("running m.Client.Logout")

	m.apiLogger.Info("Logout")
	if _, err := m.Client.Logout(ctx); err != nil {
		// Ignore context/network errors on teardown so reconnects can proceed
		m.logger.Debugf("HTTP logout failed (ignored): %v", err)
	}

	m.logger.Debug("exiting Logout()")

	return nil
}

// SetLogAPICalls sets the log level of the Mattermost API-call logger.
// Set to "warn" to log most API request operations (some lifecycle calls are logged at "info").
// Accepted levels are: 'debug', 'info', 'warn', 'error', 'fatal' and 'panic'.
func (m *Client) SetLogAPICalls(level string) {
	l, err := logrus.ParseLevel(level)
	if err != nil {
		m.logger.Warnf("Failed to parse specified log-level '%s': %#v", level, err)
	} else {
		m.rootAPILogger.SetLevel(l)
	}
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

func (m *Client) HandleRatelimit(ctx context.Context, name string, resp *model.Response) error {
	if resp == nil {
		return fmt.Errorf("got a nil model response from %s", name)
	}

	if resp.StatusCode != 429 {
		return fmt.Errorf("StatusCode error: %d", resp.StatusCode)
	}

	resetHeaderStr := resp.Header.Get("X-RateLimit-Reset") //nolint:canonicalheader
	if resetHeaderStr == "" {
		_, err := m.sleepWithContext(ctx, 3*time.Second)
		return err
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

	m.logger.Warnf("Ratelimited on %s for %d seconds", name, waitTime)
	_, err = m.sleepWithContext(ctx, time.Duration(waitTime)*time.Second)
	return err
}

func (m *Client) HandleRetry(ctx context.Context, name string, err error, current int, maxLimit int, resp *model.Response) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	if current >= maxLimit {
		return false, nil
	}

	sleepDuration := time.Duration(current+1) * time.Second

	if resp == nil {
		m.logger.Warnf("Network error on %s (resp is nil, err: %v), backing off: %s (attempt %d/%d)",
			name, err, sleepDuration, current+1, maxLimit)
		return m.sleepWithContext(ctx, sleepDuration)
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		_ = m.HandleRatelimit(ctx, name, resp)
		return true, nil

	case resp.StatusCode >= http.StatusInternalServerError:
		m.logger.Warnf("Transient error %d on %s, backing off: %s (attempt %d/%d)",
			resp.StatusCode, name, sleepDuration, current+1, maxLimit)
		return m.sleepWithContext(ctx, sleepDuration)

	case resp.StatusCode >= 300 && resp.StatusCode < 500:
		// Client errors (4xx) usually shouldn't be retried unless it's a 429
		return false, nil

	default:
		return m.sleepWithContext(ctx, sleepDuration)
	}
}

// Helper function to handle interruptible sleeps
func (m *Client) sleepWithContext(ctx context.Context, d time.Duration) (bool, error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err() // Context canceled during backoff
	case <-time.After(d):
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

			_ = m.UpdateLastViewed(ctx, channelID)
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

	m.apiLogger.Warnf("syncSingleUser: GetUser: UserID: %s", userID)
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
	case model.WebsocketEventNewUser, model.WebsocketEventUserUpdated, model.WebsocketEventUserAdded, model.WebsocketEventAddedToTeam:
		var u *model.User

		if userVal, ok := event.GetData()["user"]; ok {
			if userPtr, isPtr := userVal.(*model.User); isPtr {
				u = userPtr
			} else if userStr, isStr := userVal.(string); isStr && userStr != "" {
				var summary UserSummary
				_ = json.NewDecoder(strings.NewReader(userStr)).Decode(&summary)
				// Map it back to the required model.User for the cache functions
				u = &model.User{
					Id:        summary.Id,
					Username:  summary.Username,
					FirstName: summary.FirstName,
					LastName:  summary.LastName,
					Nickname:  summary.Nickname,
					Roles:     summary.Roles,
					Props:     summary.Props,
				}
			}
		} else if userID, ok := event.GetData()["user_id"].(string); ok && userID != "" {
			u = m.GetUser(ctx, userID)
		}

		// If we couldn't resolve a valid user, bail out early
		if u == nil || u.Id == "" {
			break
		}

		eventType := event.EventType()

		if eventType == model.WebsocketEventUserAdded {
			broadcast := event.GetBroadcast()
			if broadcast != nil && broadcast.ChannelId != "" {
				m.UpdateChannelUsersCache(broadcast.ChannelId, u)
				m.Users.lastUpdated.Store(time.Now().Unix())
			}
		} else {
			// Handles NewUser, UserUpdated, and AddedToTeam
			teamID, _ := event.GetData()["team_id"].(string)
			if teamID == "" && event.GetBroadcast() != nil {
				teamID = event.GetBroadcast().TeamId
			}

			if teamID != "" {
				m.UpdateTeamUsersCache(teamID, u)
			} else if eventType == model.WebsocketEventUserUpdated {
				m.UpdateUser(u)
			}
		}

		// Custom Status processing (UserUpdated only)
		if eventType == model.WebsocketEventUserUpdated {
			m.Users.mu.RLock()
			_, tracked := m.Users.customStatuses[u.Id]
			m.Users.mu.RUnlock()

			if tracked {
				m.Users.SetUserCustomStatus(u.Id, u.Props["customStatus"])
			}
		}

	case model.WebsocketEventLeaveTeam:
		userID, _ := event.GetData()["user_id"].(string)
		teamID, _ := event.GetData()["team_id"].(string)
		if teamID == "" && event.GetBroadcast() != nil {
			teamID = event.GetBroadcast().TeamId
		}

		if userID != "" && teamID != "" {
			m.UpdateTeamUsersCacheRemove(teamID, userID)
			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventUserRemoved:
		channelID := event.GetBroadcast().ChannelId
		if userID, ok := event.GetData()["user_id"].(string); ok && channelID != "" {
			m.UpdateChannelUsersCacheRemove(channelID, userID)
			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventPosted:
		channelID := event.GetBroadcast().ChannelId
		if channelID == "" {
			break
		}

		var post *model.Post

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			post = postPtr
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			// Fallback path: Driver left it as a JSON string
			post = &model.Post{}
			_ = json.NewDecoder(strings.NewReader(postStr)).Decode(post)
		}

		if post == nil || post.Id == "" {
			break
		}

		// Cache every incoming post for fast GetPost / thread lookups
		if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
			m.postCache.Add(post.Id, post)
		}

		// Update the LastPostAt in our channelData cache
		m.Users.mu.Lock()
		if ch, ok := m.Users.channelData[channelID]; ok {
			// Only update if the new post is newer (handles out-of-order websocket events)
			if post.CreateAt > ch.LastPostAt {
				ch.LastPostAt = post.CreateAt
			}
		}
		m.Users.mu.Unlock()

		// Mattermost assigns the leaving user's ID to the 'system_leave_channel' post.
		// We don't need to invalidate channel caches based on system message authorship.
		if strings.HasPrefix(post.Type, "system_") {
			break
		}

		m.Users.mu.RLock()
		channelMap, channelIsCached := m.Users.channels[channelID]
		var userIsCached bool
		if channelIsCached {
			_, userIsCached = channelMap[post.UserId]
		}
		m.Users.mu.RUnlock()

		// If we are actively caching this channel, but the user isn't in our list, our cache is stale!
		if channelIsCached && !userIsCached {
			m.logger.Warnf("Unrecognized user %s spoke in %s. Invalidating channel cache.", post.UserId, channelID)

			m.Users.mu.Lock()
			delete(m.Users.channels, channelID)
			m.Users.mu.Unlock()
			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventPostEdited:
		var post *model.Post

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			post = postPtr
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			post = &model.Post{}
			_ = json.NewDecoder(strings.NewReader(postStr)).Decode(post)
		}

		if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
			m.postCache.Add(post.Id, post)
		}

	case model.WebsocketEventPostDeleted:
		var postID string

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			postID = postPtr.Id
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			var post model.Post

			_ = json.NewDecoder(strings.NewReader(postStr)).Decode(&post)
			postID = post.Id
		} else if id, ok := event.GetData()["post_id"].(string); ok {
			postID = id
		}

		if postID != "" && m.postCache != nil {
			m.postCache.Remove(postID)
		}

	case model.WebsocketEventChannelCreated, model.WebsocketEventDirectAdded, model.WebsocketEventChannelUpdated:
		var channel *model.Channel

		if chPtr, ok := event.GetData()["channel"].(*model.Channel); ok {
			channel = chPtr
		} else if chStr, ok := event.GetData()["channel"].(string); ok && chStr != "" {
			var summary ChannelSummary
			_ = json.NewDecoder(strings.NewReader(chStr)).Decode(&summary)
			channel = &model.Channel{
				Id:          summary.Id,
				UpdateAt:    summary.UpdateAt,
				DeleteAt:    summary.DeleteAt,
				TeamId:      summary.TeamId,
				Type:        model.ChannelType(summary.Type),
				DisplayName: summary.DisplayName,
				Name:        summary.Name,
				Header:      summary.Header,
				Purpose:     summary.Purpose,
				LastPostAt:  summary.LastPostAt,
				CreatorId:   summary.CreatorId,
			}
		}

		// If we failed to extract a channel, attempt the last resort fallback fetch
		if channel == nil || channel.Id == "" {
			if event.EventType() != model.WebsocketEventChannelUpdated {
				if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
					m.GetChannel(ctx, channelID)
				}
			}
			break
		}

		// We have a valid channel object, cache it
		m.Users.mu.Lock()
		m.Users.channelData[channel.Id] = channel
		m.Users.mu.Unlock()
		m.Users.lastUpdated.Store(time.Now().Unix())

	case model.WebsocketEventChannelDeleted:
		if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
			// Mattermost soft-deletes channels. We keep the channel and users in our
			// local cache to gracefully handle history, references, or restorations.
			m.Users.mu.Lock()
			if ch, ok := m.Users.channelData[channelID]; ok {
				if deleteAt, ok := event.GetData()["delete_at"].(float64); ok && deleteAt > 0 {
					ch.DeleteAt = int64(deleteAt)
				} else {
					// Fallback if payload is missing the timestamp
					ch.DeleteAt = time.Now().UnixNano() / int64(time.Millisecond)
				}
			}
			m.Users.mu.Unlock()

			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventMultipleChannelsViewed:
		if channelTimes, ok := event.GetData()["channel_times"].(map[string]interface{}); ok {
			m.Users.mu.Lock()
			for chanID, timeVal := range channelTimes {
				if ts, ok := timeVal.(float64); ok {
					m.Users.channelLastViewedAt[chanID] = int64(ts)
				}
			}
			m.Users.mu.Unlock()
		}

	case model.WebsocketEventChannelViewed:
		if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
			m.Users.mu.Lock()
			// channel_viewed doesn't always contain a timestamp in the payload.
			// If it doesn't, we can just use the current time to update it locally!
			m.Users.channelLastViewedAt[channelID] = time.Now().UnixNano() / int64(time.Millisecond)
			m.Users.mu.Unlock()
		}

	case model.WebsocketEventStatusChange:
		statusRaw, ok := event.GetData()["status"].(string)
		if !ok || statusRaw == "" {
			break
		}

		userID, _ := event.GetData()["user_id"].(string)
		if userID == "" && event.GetBroadcast() != nil {
			userID = event.GetBroadcast().UserId
		}

		if userID == "" {
			break
		}

		m.SetUserStatus(userID, statusRaw)

	}
}

// syncJoinedChannelsCache synchronously updates the joined channels cache
// to prevent race conditions before handing off to background processes.
func (m *Client) syncJoinedChannelsCache(event *model.WebSocketEvent) {
	switch event.EventType() {
	case model.WebsocketEventUserAdded, model.WebsocketEventUserRemoved:
		if m.User == nil {
			break
		}

		userID, ok := event.GetData()["user_id"].(string)
		if !ok || userID != m.User.Id {
			break
		}

		broadcast := event.GetBroadcast()
		if broadcast == nil || broadcast.ChannelId == "" {
			break
		}

		m.Users.mu.Lock()
		if event.EventType() == model.WebsocketEventUserAdded {
			m.Users.joinedChannels[broadcast.ChannelId] = struct{}{}
		} else {
			delete(m.Users.joinedChannels, broadcast.ChannelId)
		}
		m.Users.mu.Unlock()

	case model.WebsocketEventDirectAdded, model.WebsocketEventChannelCreated:
		var chID string
		var chType model.ChannelType

		if chPtr, ok := event.GetData()["channel"].(*model.Channel); ok {
			chID = chPtr.Id
			chType = chPtr.Type
		} else if chStr, ok := event.GetData()["channel"].(string); ok && chStr != "" {
			// Fallback path: Fast partial unmarshal
			var ch struct {
				ID   string            `json:"id"`
				Type model.ChannelType `json:"type"`
			}
			if err := json.NewDecoder(strings.NewReader(chStr)).Decode(&ch); err == nil {
				chID = ch.ID
				chType = ch.Type
			}
		}

		if chID == "" {
			break
		}

		if chType == model.ChannelTypeDirect || chType == model.ChannelTypeGroup {
			m.Users.mu.Lock()
			m.Users.joinedChannels[chID] = struct{}{}
			m.Users.mu.Unlock()
		}
	}
}

// IsAborted checks if the user disconnected, logout was called, or the context died.
func (m *Client) IsAborted(ctx context.Context) bool {
	if m.WsQuit {
		return true
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return false
}
