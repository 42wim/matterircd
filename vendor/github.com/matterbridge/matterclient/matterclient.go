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
	"net/http/httptrace"
	"net/url"
	"slices"
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

// statusVerification records when the server confirmed a last activity value, and that value.
type statusVerification struct {
	at       int64
	activity int64
}

type UsersCache struct {
	mu       sync.RWMutex
	users    map[string]*model.User
	channels map[string]map[string]struct{}
	teams    map[string]map[string]struct{}
	statuses map[string]string

	channelData    map[string]*model.Channel
	joinedChannels map[string]struct{}
	// membershipWatchers collect the joins (true) and leaves (false) applied while a channel refresh is in flight, so
	// that its stale response doesn't undo them. Only populated during a refresh.
	membershipWatchers map[int]map[string]bool
	nextWatcherID      int

	channelLastViewedAt map[string]int64

	customStatuses        map[string]string
	customStatusExpiresAt map[string]int64
	lastUserActivity      map[string]int64
	statusLastUpdated     map[string]int64
	statusRESTFetched     map[string]int64
	statusVerified        map[string]statusVerification

	timezones map[string]map[string]string

	// publicChannelsSynced holds the teams of which all the public channels have been cached.
	publicChannelsSynced map[string]struct{}

	lastUpdated atomic.Int64
}

type UserSummary struct {
	Id             string            `json:"id"`
	UpdateAt       int64             `json:"update_at"`
	DeleteAt       int64             `json:"delete_at"`
	Username       string            `json:"username"`
	Email          string            `json:"email"`
	FirstName      string            `json:"first_name"`
	LastName       string            `json:"last_name"`
	Nickname       string            `json:"nickname"`
	Roles          string            `json:"roles"`
	Props          map[string]string `json:"props"`
	Timezone       map[string]string `json:"timezone"`
	IsBot          bool              `json:"is_bot"`
	LastActivityAt int64             `json:"last_activity_at"`
	Position       string            `json:"position"`
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

	Team           *Team
	OtherTeams     map[string]*Team
	Client         *model.Client4
	User           *model.User
	Users          *UsersCache
	MessageChan    chan *Message
	WsClient       *model.WebSocketClient
	AntiIdle       bool
	AntiIdleChan   string
	AntiIdleIntvl  int
	WsQuit         bool
	WsConnected    bool
	OnWsConnect    func()
	OnWsDisconnect func(err error)
	reconnectBusy  bool
	Timeout        int

	// wsReconnectMu serializes in-place websocket replacement between WsReceiver and checkConnection.
	wsReconnectMu sync.Mutex
	// wsMu guards WsClient, WsConnected and WsQuit. Never hold it across network operations.
	wsMu sync.RWMutex
	// resyncCancel cancels the resync of the previous in-place reconnect, guarded by wsMu.
	resyncCancel context.CancelFunc

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

	pingSeq   atomic.Int64
	pingChans sync.Map

	typingLock     sync.Mutex
	lastTypingSent map[string]time.Time

	// OnChannelNameResolved, if set, is called after a channel name that was previously
	// unresolved (still showing an ID) has been healed. It must not block.
	OnChannelNameResolved func(channelID, name string)
	// nameHealing tracks channel IDs with a running healing goroutine.
	nameHealing sync.Map
	// loginCtx is the context of the current login, canceled on Logout.
	loginCtx atomic.Pointer[context.Context]

	//nolint:containedctx // Tied to the lifecycle of the persistent client session
	Ctx context.Context
}

var Matterircd bool

const (
	schemeHTTPS = "https://"
	schemeHTTP  = "http://"
)

const mattermostIDLen = 26

// Mattermost has a hardcoded `PerPageMaximum = 200` & `LimitMaximum = 200`
// See https://github.com/mattermost/mattermost/blob/master/server/channels/web/params.go
const mattermostPerPageMax = 200

//nolint:funlen
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

			customStatuses:        make(map[string]string),
			customStatusExpiresAt: make(map[string]int64, 1000),
			lastUserActivity:      make(map[string]int64, 1000),
			statusLastUpdated:     make(map[string]int64, 1000),
		},
		lruCache:  cache,
		postCache: postCache,

		rootLogger: rootLogger,
		logger:     rootLogger.WithFields(logrus.Fields{"prefix": "matterclient"}),

		rootAPILogger: rootAPILogger,
		apiLogger:     rootAPILogger.WithFields(logrus.Fields{"prefix": "matterclient: API"}),

		aliveChan: make(chan bool),

		lastTypingSent: make(map[string]time.Time),

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
	// A retained websocket means a reconnect, even when an in-place redial left WsConnected false.
	firstConnection := true

	if ws, _ := m.wsSnapshot(); ws != nil {
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

	m.wsMu.Lock()
	m.WsConnected = false
	quit := m.WsQuit
	m.wsMu.Unlock()

	if quit {
		return nil
	}

	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}

	// do initialization setup
	err := m.initClient(ctx, b)
	if err != nil {
		return err
	}

	err = m.doLogin(ctx, firstConnection, b)
	if err != nil {
		return err
	}

	err = m.initUser(ctx)
	if err != nil {
		return err
	}

	if m.Team == nil {
		validTeamNames := make([]string, 0, len(m.OtherTeams))
		for _, t := range m.OtherTeams {
			validTeamNames = append(validTeamNames, t.Team.Name)
		}

		return fmt.Errorf("Team '%s' not found in %v", m.Credentials.Team, validTeamNames)
	}

	err = m.initUserChannels(ctx)
	if err != nil {
		return err
	}

	// Statuses are fetched over HTTP, so overlap them with the websocket handshake.
	m.logger.Trace("requesting initial user statuses for cache")

	statusesDone := make(chan struct{})

	go func() {
		defer close(statusesDone)

		_ = m.GetStatuses(ctx)
	}()

	// Connect websocket using the short-lived Login operation context.
	// If the login operation is canceled, this will cleanly abort.
	err = m.wsConnect(ctx)

	<-statusesDone

	if err != nil {
		return err
	}

	// Prepare the long-lived background context for goroutines.
	parentCtx := m.Ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	bgCtx, loginCancel := context.WithCancel(parentCtx)
	m.loginCancel = loginCancel
	//nolint:contextcheck // bgCtx is derived from m.Ctx, the long-lived session context
	m.loginCtx.Store(&bgCtx)

	m.logger.Trace("executing WsReceiver()")
	// Pass the long-lived background context to the goroutines.
	// They will run until m.loginCancel() is called during Logout.
	//nolint:contextcheck
	go m.WsReceiver(bgCtx)

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

	//nolint:contextcheck // Use the long-lived background context (bgCtx) here instead of ctx
	go m.syncPeriodicStatus(bgCtx, 30*time.Minute)

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

	if m.OnWsDisconnect != nil {
		go m.OnWsDisconnect(errors.New("connection lost, reconnecting"))
	}

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

type timedError struct {
	err      error
	duration time.Duration
}

func (e *timedError) Error() string {
	return fmt.Sprintf("%s (%s)", e.err.Error(), e.duration.Round(time.Millisecond))
}

func (e *timedError) Unwrap() error {
	return e.err
}

func (e *timedError) Timeout() bool {
	if netErr, ok := errors.AsType[net.Error](e.err); ok {
		return netErr.Timeout()
	}

	return false
}

func (e *timedError) Temporary() bool {
	if netErr, ok := errors.AsType[net.Error](e.err); ok {
		return netErr.Temporary() //nolint:staticcheck
	}

	return false
}

type timedTransport struct {
	base   http.RoundTripper
	logger *logrus.Entry
}

func (t *timedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()

	resp, err := t.base.RoundTrip(req)

	duration := time.Since(start)

	if t.logger != nil {
		// Only the path is logged, the query string can carry credentials.
		if err != nil {
			t.logger.Warnf("%s %s: %s (%s)", req.Method, req.URL.Path, err, duration.Round(time.Millisecond))
		} else {
			t.logger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration.Round(time.Millisecond))
		}
	}

	if err != nil {
		return nil, &timedError{
			err:      err,
			duration: duration,
		}
	}

	return resp, nil
}

func (m *Client) initClient(ctx context.Context, b *backoff.Backoff) error {
	uriScheme := schemeHTTPS
	if m.NoTLS {
		uriScheme = schemeHTTP
	}
	// login to mattermost
	m.Client = model.NewAPIv4Client(uriScheme + m.Server)

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

	m.Client.HTTPClient.Transport = &timedTransport{
		base:   baseTransport,
		logger: m.apiLogger,
	}
	m.Client.HTTPClient.Timeout = time.Second * time.Duration(m.Timeout)

	if m.UserAgent != "" {
		m.Client.HTTPHeader = map[string]string{
			"User-Agent": m.UserAgent,
		}
	}

	m.Client.HTTPClient.Timeout = time.Second * time.Duration(m.Timeout)

	// handle MMAUTHTOKEN and personal token
	err := m.handleLoginToken()
	if err != nil {
		return err
	}

	// check if server alive, retry until
	err = m.serverAlive(ctx, b)
	if err != nil {
		return err
	}

	return nil
}

func (m *Client) handleLoginToken() error {
	switch {
	case strings.Contains(m.Pass, model.SessionCookieToken):
		token := strings.Split(m.Pass, model.SessionCookieToken+"=")
		if len(token) != 2 {
			return errors.New("incorrect MMAUTHTOKEN. valid input is MMAUTHTOKEN=yourtoken")
		}

		m.Token = token[1]
		m.CookieToken = true
	case strings.Contains(m.Pass, "token="):
		token := strings.Split(m.Pass, "token=")
		if len(token) != 2 {
			return errors.New("incorrect personal token. valid input is token=yourtoken")
		}

		m.Token = token[1]
	}

	return nil
}

func (m *Client) serverAlive(ctx context.Context, b *backoff.Backoff) error {
	if b != nil {
		defer b.Reset()
	}

	var negotiatedProto string

	trace := &httptrace.ClientTrace{
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if err == nil {
				negotiatedProto = state.NegotiatedProtocol
			}
		},
	}
	traceCtx := httptrace.WithClientTrace(ctx, trace)

	retryCount := 0

	for {
		if m.IsAborted(ctx) || ctx.Err() != nil {
			return errors.New("login aborted")
		}

		// bogus call to get the serverversion
		m.apiLogger.Infof("serverAlive: Logout check #%d", retryCount)
		resp, err := m.Client.Logout(traceCtx)

		// Success condition: No error, and we have a server version
		if err == nil && resp != nil && resp.ServerVersion != "" {
			proto := "HTTP/1.1"
			if negotiatedProto == "h2" {
				proto = "HTTP/2.0"
			} else if negotiatedProto != "" {
				proto = negotiatedProto
			}

			m.logger.Infof("Found version %s (protocol: %s)", resp.ServerVersion, proto)

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
		var (
			resp *model.Response
			err  error
		)

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

		teamUsers := make(map[string]struct{}, batchSize)

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

			count, jsonErr := m.updateUsersFromJSON(resp.Body, func(u *model.User) {
				teamUsers[u.Id] = struct{}{}
			})
			resp.Body.Close()

			if jsonErr != nil {
				return jsonErr
			}

			if count < batchSize {
				break
			}

			idx++

			if ctx.Err() != nil {
				return ctx.Err()
			}
		}

		if !exists {
			m.logger.Debugf("found %d users in team %s", len(teamUsers), team.Name)
		} else {
			m.logger.Debugf("found %d users in team %s (cache expired)", len(teamUsers), team.Name)
		}

		m.Users.mu.Lock()
		if m.Users.teams == nil {
			m.Users.teams = make(map[string]map[string]struct{})
		}

		m.Users.teams[team.Id] = teamUsers
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
	err := m.UpdateChannels(ctx)
	if err != nil {
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

//nolint:funlen
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

		m.logger.Debugf("%s %s %s %s #%d", logmsg, m.Credentials.Team, m.Credentials.Login, m.Server, retryCount)

		// Attempt the appropriate login method
		switch {
		case m.Token != "":
			user, resp, err = m.doLoginToken(ctx)
		case m.MFAToken != "":
			m.apiLogger.Info("doLogin: LoginWithMFA")
			user, resp, err = m.Client.LoginWithMFA(ctx, m.Credentials.Login, m.Pass, m.MFAToken)
		default:
			m.apiLogger.Info("doLogin: Login")
			user, resp, err = m.Client.Login(ctx, m.Credentials.Login, m.Pass)
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
	m.Client.AuthToken = m.Token

	if m.CookieToken {
		m.logger.Debugf("%s with cookie (MMAUTH) token", logmsg)
		m.Client.HTTPClient.Jar = m.createCookieJar(m.Token)
	} else {
		m.logger.Debugf("%s with personal token", logmsg)
	}

	m.apiLogger.Info("doLoginToken: GetMe")

	user, resp, err = m.Client.GetMe(ctx, "")
	if err != nil {
		return user, resp, err
	}

	if user == nil {
		m.logger.Errorf("LOGIN TOKEN: %s is invalid", m.Pass)

		return user, resp, errors.New("invalid token")
	}

	return user, resp, nil
}

func (m *Client) createCookieJar(token string) *cookiejar.Jar {
	var cookies []*http.Cookie

	jar, _ := cookiejar.New(nil)

	firstCookie := &http.Cookie{ //nolint:gosec
		Name:   "MMAUTHTOKEN",
		Value:  token,
		Path:   "/",
		Domain: m.Server,
	}

	cookies = append(cookies, firstCookie)
	cookieURL, _ := url.Parse(schemeHTTPS + m.Server)

	jar.SetCookies(cookieURL, cookies)

	return jar
}

func (m *Client) wsURL() string {
	if m.NoTLS {
		return "ws://" + m.Server
	}

	return "wss://" + m.Server
}

func (m *Client) dialWs(wsurl string) (*model.WebSocketClient, error) {
	wsDialer := &websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: m.SkipTLSVerify, //nolint:gosec
		},
		Proxy:             http.ProxyFromEnvironment,
		EnableCompression: false,
		// Same as websocket.DefaultDialer, otherwise a server that never answers the handshake blocks forever.
		HandshakeTimeout: 45 * time.Second,
	}

	return model.NewWebSocketClientWithDialer(wsDialer, wsurl, m.Client.AuthToken)
}

func (m *Client) wsSnapshot() (*model.WebSocketClient, bool) {
	m.wsMu.RLock()
	defer m.wsMu.RUnlock()

	return m.WsClient, m.WsConnected
}

func (m *Client) setWsQuit(quit bool) {
	m.wsMu.Lock()
	m.WsQuit = quit
	m.wsMu.Unlock()
}

// installWs publishes a freshly dialed websocket. It refuses once the login that owns ctx was canceled or Logout ran:
// Logout cancels first and then closes whatever is published under wsMu, so a socket is either closed by Logout or
// refused here, and never left running without an owner. The caller closes a refused ws.
func (m *Client) installWs(ctx context.Context, ws *model.WebSocketClient) error {
	m.wsMu.Lock()
	defer m.wsMu.Unlock()

	if ctx.Err() != nil {
		return fmt.Errorf("websocket install aborted: %w", ctx.Err())
	}

	if m.WsQuit {
		return errors.New("websocket install aborted: logged out")
	}

	m.WsClient = ws
	m.WsConnected = true

	return nil
}

// discardWs closes a websocket whose listener is running. The listener closes its channels and stops the ping watchdog
// once the connection is gone, so draining ends. A socket that was never published still needs Listen called first,
// as only the listener's cleanup stops the watchdog.
func discardWs(ws *model.WebSocketClient) {
	// The SDK's Close waits for its writer, which is stuck in a write on a half-open connection until the transport
	// is closed, so close that first.
	_ = wsSafely(func() { _ = ws.Conn.Close() })
	_ = wsSafely(ws.Close)

	go drainChannel(ws.EventChannel)
	go drainChannel(ws.ResponseChannel)
}

// dialWsWithRetry dials a websocket with a short backoff, giving up after a few attempts so the caller can fall back.
func (m *Client) dialWsWithRetry(ctx context.Context) (*model.WebSocketClient, error) {
	const maxAttempts = 5

	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    8 * time.Second,
		Jitter: true,
	}

	wsurl := m.wsURL()

	for attempt := 1; ; attempt++ {
		ws, err := m.dialWs(wsurl)
		if err == nil {
			return ws, nil
		}

		if attempt == maxAttempts {
			return nil, fmt.Errorf("websocket reconnect failed after %d attempts: %w", maxAttempts, err)
		}

		d := b.Duration()

		m.logger.Warnf("websocket reconnect %d/%d failed: %s, retrying in %s", attempt, maxAttempts, err, d)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("websocket reconnect aborted: %w", ctx.Err())
		case <-time.After(d):
		}
	}
}

// reconnectWs replaces a dead websocket in place, keeping the HTTP session and the caches. old is the client the caller
// saw fail; if it was already replaced by another goroutine, there is nothing left to do.
func (m *Client) reconnectWs(ctx context.Context, old *model.WebSocketClient) error {
	m.wsReconnectMu.Lock()
	defer m.wsReconnectMu.Unlock()

	if current, _ := m.wsSnapshot(); current != old {
		return nil
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Only an invalid session needs a full login; a transient network error is covered by the dial retries below.
	_, resp, err := m.Client.GetMe(ctx, "")
	if err != nil && resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		return fmt.Errorf("session no longer valid: %w", err)
	}

	// A full reconnect doesn't take wsReconnectMu, so it may have replaced the socket during GetMe.
	m.wsMu.Lock()
	if ctx.Err() != nil || m.WsClient != old {
		m.wsMu.Unlock()

		return nil
	}

	m.WsConnected = false
	m.wsMu.Unlock()

	if old != nil {
		// The reader blocks on full channels until they are drained, see Logout.
		discardWs(old)
	}

	ws, err := m.dialWsWithRetry(ctx)
	if err != nil {
		return err
	}

	err = m.installWs(ctx, ws)
	if err != nil {
		// Listen so the SDK's cleanup runs on close, see discardWs.
		ws.Listen()
		discardWs(ws)

		return err
	}

	ws.Listen()

	m.lastWsActivity.Store(time.Now().Unix())
	m.connectedAt.Store(time.Now().Unix())

	m.logger.Info("websocket reconnected in place")

	go m.resyncAfterWsReconnect(m.beginResync(ctx), ws)

	return nil
}

// beginResync returns the context of the resync for a newly installed websocket, canceling the resync of the previous
// one, so that only one resync is ever running.
func (m *Client) beginResync(ctx context.Context) context.Context {
	m.wsMu.Lock()
	defer m.wsMu.Unlock()

	if m.resyncCancel != nil {
		m.resyncCancel()
	}

	resyncCtx, cancel := context.WithCancel(ctx)
	m.resyncCancel = cancel

	return resyncCtx
}

func (m *Client) isCurrentWs(ws *model.WebSocketClient) bool {
	current, _ := m.wsSnapshot()

	return current == ws
}

// resyncAfterWsReconnect catches up on what was missed while the websocket was down. Refreshing the channels updates
// LastPostAt, which OnWsConnect consumers rely on to replay missed posts: GetPostsSince skips channels whose cached
// LastPostAt is stale, so replaying after a failed refresh would silently lose posts. If the refresh keeps failing,
// fall back to a full reconnect, which replays through Login.
func (m *Client) resyncAfterWsReconnect(ctx context.Context, ws *model.WebSocketClient) {
	const maxAttempts = 3

	var err error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = m.UpdateChannels(ctx)
		if err == nil {
			break
		}

		if ctx.Err() != nil {
			return
		}

		m.logger.Warnf("websocket reconnect: updating channels failed %d/%d: %s", attempt, maxAttempts, err)

		if attempt == maxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt) * 2 * time.Second):
		}
	}

	if err != nil {
		// A newer socket has superseded this resync, and its own resync is in charge of recovery.
		if ctx.Err() != nil || !m.isCurrentWs(ws) {
			return
		}

		m.logger.Warnf("websocket reconnect: channel refresh failed, falling back to full reconnect")
		m.Reconnect(ctx)

		return
	}

	// GetStatuses only fetches users missing from the cache, so ask for everyone. The responses update the cache.
	m.Users.mu.RLock()
	userIDs := make([]string, 0, len(m.Users.users))

	for id := range m.Users.users {
		userIDs = append(userIDs, id)
	}

	m.Users.mu.RUnlock()

	m.WsGetStatusesByIds(userIDs)

	if ctx.Err() != nil || !m.isCurrentWs(ws) {
		return
	}

	if m.OnWsConnect != nil {
		m.OnWsConnect()
	}
}

// recoverWs re-establishes only the websocket, and falls back to a full Reconnect if that fails. It reports whether
// the websocket was recovered, i.e. whether the goroutines of the current login are still valid.
func (m *Client) recoverWs(ctx context.Context, old *model.WebSocketClient) bool {
	if ctx.Err() != nil {
		return false
	}

	err := m.reconnectWs(ctx, old)
	if err == nil {
		return true
	}

	if ctx.Err() != nil {
		return false
	}

	m.logger.Warnf("websocket reconnect failed, falling back to full reconnect: %s", err)
	m.Reconnect(ctx)

	return false
}

//nolint:funlen
func (m *Client) wsConnect(ctx context.Context) error {
	b := &backoff.Backoff{
		Min:    time.Second,
		Max:    5 * time.Minute,
		Jitter: true,
	}

	m.wsMu.Lock()
	m.WsConnected = false
	m.wsMu.Unlock()

	wsScheme := "wss://"

	if m.NoTLS {
		wsScheme = "ws://"
	}

	// setup websocket connection
	wsurl := wsScheme + m.Server
	// + model.API_URL_SUFFIX_V4
	// + "/websocket"
	header := http.Header{}
	header.Set(model.HeaderAuth, "BEARER "+m.Client.AuthToken)

	m.logger.Tracef("WsClient: making connection: %s", wsurl)

	var wsClient *model.WebSocketClient

	for {
		if ctx.Err() != nil {
			m.logger.Errorf("wsConnect aborted: %v", ctx.Err())
			return fmt.Errorf("wsConnect aborted: %w", ctx.Err())
		}

		ws, err := m.dialWs(wsurl)
		if err != nil {
			d := b.Duration()

			m.logger.Debugf("WSS: %s, reconnecting in %s", err, d)

			select {
			case <-ctx.Done():
				return fmt.Errorf("wsConnect aborted: %w", ctx.Err())
			case <-time.After(d):
			}

			continue
		}

		// Only set m.WsClient once connected: a failed attempt returns nil, which WsReceiver crashes on.
		err = m.installWs(ctx, ws)
		if err != nil {
			// Listen so the SDK's cleanup runs on close, see discardWs.
			ws.Listen()
			discardWs(ws)

			return err
		}

		wsClient = ws

		break
	}

	wsClient.Listen()

	m.lastWsActivity.Store(time.Now().Unix())
	m.connectedAt.Store(time.Now().Unix())

	m.logger.Debug("WsClient: connected")

	return nil
}

//nolint:funcorder
func (m *Client) doCheckAlive(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("websocket ping panic (connection closed): %v", r)
		}
	}()

	ws, _ := m.wsSnapshot()
	if ws != nil && ws.ListenError != nil {
		return fmt.Errorf("websocket listen error: %w", ws.ListenError)
	}

	connectedUnix := m.connectedAt.Load()
	uptime := time.Since(time.Unix(connectedUnix, 0)).Round(time.Second)

	lastActiveUnix := m.lastWsActivity.Load()
	timeSinceActivity := time.Since(time.Unix(lastActiveUnix, 0))

	if timeSinceActivity < 10*time.Second {
		m.logger.Tracef("websocket is active (last event %v ago; up %s), skipping ping", timeSinceActivity.Round(time.Second), uptime)
		return nil
	}

	if timeSinceActivity < 45*time.Second {
		// Send a ping down the websocket to try to keep it active/alive
		if ws != nil {
			m.logger.Tracef("websocket has been quiet (last event %v ago; up %s), sending websocket ping", timeSinceActivity.Round(time.Second), uptime)
			ws.SendMessage("ping", nil)

			return nil
		}
	}

	m.logger.Tracef("websocket has been quiet (last event %v ago; up %s), falling back to HTTP GetPing", timeSinceActivity.Round(time.Second), uptime)
	m.apiLogger.Info("doCheckAlive: GetPing")

	_, _, err = m.Client.GetPing(ctx)
	if err != nil {
		m.logger.Warnf("fallback HTTP ping failed (up %s): %s", uptime, err)
		return fmt.Errorf("fallback HTTP ping failed (up %s): %w", uptime, err)
	}

	// The server answers every websocket ping, so a socket silent for this long is dead even though the session is fine.
	// Don't refresh lastWsActivity here: that would hide a half-open connection forever.
	return fmt.Errorf("websocket silent for %v despite pings (up %s)", timeSinceActivity.Round(time.Second), uptime)
}

func (m *Client) checkAlive(ctx context.Context) {
	// Frequent enough to keep proxies and NATs from dropping an idle connection.
	ticker := time.NewTicker(time.Second * 15)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Debugf("checkAlive: ctx.Done() triggered")

			return
		case <-ticker.C:
			var err error

			// check if session still is valid
			for i := range 3 {
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
			}

			// checkConnection stops receiving once ctx is done, don't block forever on it.
			select {
			case m.aliveChan <- err == nil:
			case <-ctx.Done():
				m.logger.Debugf("checkAlive: ctx.Done() triggered while reporting")

				return
			}
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

				ws, _ := m.wsSnapshot()

				if m.doCheckAlive(ctx) != nil {
					if m.OnWsDisconnect != nil {
						go m.OnWsDisconnect(errors.New("connection alive check failed"))
					}

					m.recoverWs(ctx, ws)
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
//
//nolint:funlen,gocognit,gocyclo
func (m *Client) WsReceiver(ctx context.Context) {
	m.logger.Debug("starting WsReceiver")

	ticker := time.NewTicker(time.Second * 10)
	defer ticker.Stop()

	for {
		// Snapshot: recoverWs may swap m.WsClient, and the select must not mix channels of two clients.
		ws, _ := m.wsSnapshot()

		select {
		case event := <-ws.EventChannel:
			if event == nil {
				if m.OnWsDisconnect != nil {
					go m.OnWsDisconnect(errors.New("websocket closed"))
				}

				// The reader closes its channels when the connection dies.
				if m.recoverWs(ctx, ws) {
					continue
				}

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

					_ = json.Unmarshal([]byte(userStr), &summary)
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

			msg := &Message{
				Raw:  event,
				Team: m.Credentials.Team,
			}

			if !Matterircd {
				m.parseMessage(ctx, msg)
			}

			// parseMessage can write to the event data (team_id), only read it from another goroutine after that.
			go m.maintainUsersCache(ctx, event)

			select {
			case m.MessageChan <- msg:
				// Message sent successfully
			default:
				// Message channel buffer full, drop/discard
				m.logger.Errorf("CRITICAL: MessageChan is blocked! Downstream processor is hung. Dropping event: %s", event.EventType())
			}
		case response := <-ws.ResponseChannel:
			if response == nil || !response.IsValid() {
				continue
			}

			m.lastWsActivity.Store(time.Now().Unix())

			text, ok := response.Data["text"].(string)
			if ok && text == "pong" {
				m.logger.Tracef("WsReceiver response: %#v", response)

				m.pingChans.Range(func(key, value any) bool {
					if ch, ok := value.(chan struct{}); ok {
						select {
						case ch <- struct{}{}:
						default:
						}
					}

					return true
				})

				continue
			}

			m.logger.Debugf("WsReceiver response: %#v", response)
			m.parseResponse(response)
		case <-ws.PingTimeoutChannel:
			m.logger.Error("got a ping timeout")

			if m.OnWsDisconnect != nil {
				go m.OnWsDisconnect(errors.New("websocket ping timeout"))
			}

			if m.recoverWs(ctx, ws) {
				continue
			}

			return
		case <-ticker.C:
			if ws.ListenError != nil {
				m.logger.Errorf("%#v", ws.ListenError)

				if m.OnWsDisconnect != nil {
					go m.OnWsDisconnect(ws.ListenError)
				}

				if m.recoverWs(ctx, ws) {
					continue
				}

				return
			}
		case <-ctx.Done():
			m.logger.Debugf("wsReceiver: ctx.Done() triggered")

			return
		}
	}
}

// drainChannel receives from ch, discarding everything, until it is closed.
func drainChannel[T any](ch <-chan T) {
	if ch == nil {
		return
	}

	for range ch {
	}
}

// reconnectLogout disconnects the client from the chat server for an internal reconnect.
func (m *Client) reconnectLogout(ctx context.Context) {
	err := m.Logout(ctx)

	// Always reset WsQuit so the subsequent Login() doesn't instantly abort
	m.setWsQuit(false)

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

	m.logger.Debugf("logout as %s (team: %s) on %s", m.Credentials.Login, m.Credentials.Team, m.Server)
	m.wsMu.Lock()
	m.WsQuit = true
	ws := m.WsClient
	m.wsMu.Unlock()

	// close the websocket
	if ws != nil {
		m.logger.Debug("closing websocket")

		// The websocket client's reader blocks on full event/response channels, even after Close(),
		// once WsReceiver is stopped. discardWs keeps receiving until it has closed them, or it leaks.
		discardWs(ws)
	}

	// NOTE: We no longer wipe the m.Users maps here.
	// In Bouncer mode, the cache survives for fast reconnects and is validated by Login().
	// In Dynamic mode, this entire Client struct will be Garbage Collected once the IRC session dies.

	if strings.Contains(m.Pass, model.SessionCookieToken) {
		m.logger.Debug("Not invalidating session in logout, credential is a token")
		return nil
	}

	// actually log out
	m.logger.Debug("running m.Client.Logout")

	m.apiLogger.Info("Logout")

	_, err := m.Client.Logout(ctx)
	if err != nil {
		// Ignore context/network errors on teardown so reconnects can proceed
		m.logger.Debugf("HTTP logout failed (ignored): %v", err)
	}

	m.logger.Debug("exiting Logout()")

	return nil
}

func (m *Client) HandleRatelimit(ctx context.Context, name string, resp *model.Response) error {
	if resp == nil {
		return fmt.Errorf("got a nil model response from %s", name)
	}

	if resp.StatusCode != http.StatusTooManyRequests {
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

// HttpPing sends an HTTP API GET /api/v4/system/ping request and returns the server status.
func (m *Client) HttpPing(ctx context.Context) (string, error) {
	if m.Client == nil {
		return "", errors.New("client not initialized")
	}

	m.logger.Trace("HttpPing: sending ping")

	status, _, err := m.Client.GetPing(ctx)
	if err != nil {
		m.apiLogger.Warnf("HttpPing: GetPing: %#v", err)

		return "", err
	}

	m.apiLogger.Infof("HttpPing: GetPing: %s", status)

	return status, nil
}

// IsAborted checks if the user disconnected, logout was called, or the context died.
func (m *Client) IsAborted(ctx context.Context) bool {
	m.wsMu.RLock()
	quit := m.WsQuit
	m.wsMu.RUnlock()

	if quit {
		return true
	}

	if ctx != nil && ctx.Err() != nil {
		return true
	}

	return false
}

// RemoveUserFromTeamChannelsCache removes a user from all cached channels belonging to teamID,
// cleans up the team user cache, and returns the channel IDs the user was removed from.
//
//nolint:gocyclo
func (m *Client) RemoveUserFromTeamChannelsCache(teamID, userID string) []string {
	if teamID == "" || userID == "" {
		return nil
	}

	m.RLock()
	isCurrentUser := m.User != nil && userID == m.User.Id
	m.RUnlock()

	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	if m.Users.teams != nil {
		if userMap, exists := m.Users.teams[teamID]; exists {
			delete(userMap, userID)
		}
	}

	var removedChannels []string

	if isCurrentUser && m.Users.joinedChannels != nil {
		removedChannels = make([]string, 0, len(m.Users.joinedChannels))
	} else {
		removedChannels = make([]string, 0, 16)
	}

	if m.Users.channels != nil {
		for channelID, userMap := range m.Users.channels {
			ch, exists := m.Users.channelData[channelID]
			if !exists || ch == nil || ch.TeamId != teamID {
				continue
			}

			if _, isMember := userMap[userID]; isMember {
				delete(userMap, userID)

				removedChannels = append(removedChannels, channelID)
			}
		}
	}

	if isCurrentUser && m.Users.joinedChannels != nil {
		for chID := range m.Users.joinedChannels {
			ch, exists := m.Users.channelData[chID]
			if !exists || ch == nil || ch.TeamId != teamID {
				continue
			}

			m.Users.markLeftLocked(chID)

			if !slices.Contains(removedChannels, chID) {
				removedChannels = append(removedChannels, chID)
			}
		}
	}

	m.Users.lastUpdated.Store(time.Now().Unix())

	return removedChannels
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

// SyncActiveContextStatuses refreshes statuses for DM peers and users in joined
// channels, skipping broadcast, archived, dormant, or deactivated accounts.
//
//nolint:funlen,gocyclo,gocognit
func (m *Client) SyncActiveContextStatuses() {
	if ws, connected := m.wsSnapshot(); ws == nil || !connected {
		return
	}

	m.Users.mu.RLock()

	totalUsers := len(m.Users.users)
	if totalUsers == 0 {
		m.Users.mu.RUnlock()

		return
	}

	// Cap users to help filter out broad team-wide channels (e.g. Town Square / Off Topic)
	maxChannelSize := min(int(float64(totalUsers)*0.25), 250)
	if totalUsers > 50 && maxChannelSize < 40 {
		maxChannelSize = 40
	}

	// Filter out dormant channels with no posts in the last 2 days (timestamps in milliseconds)
	dormantThresholdMs := time.Now().Add(-2 * 24 * time.Hour).UnixMilli()

	seen := make(map[string]struct{}, 500)

	// Collect users from active project / small channels
	for chanID := range m.Users.joinedChannels {
		ch, hasData := m.Users.channelData[chanID]

		chanName := chanID
		if hasData && ch.Name != "" {
			chanName = ch.Name
		}

		if hasData {
			// Skip archived / deleted channels
			if ch.DeleteAt > 0 {
				m.logger.Tracef("SyncActiveContext: skipping archived channel %s", chanName)

				continue
			}

			// Skip dormant channels with no recent activity
			if ch.LastPostAt > 0 && ch.LastPostAt < dormantThresholdMs {
				m.logger.Tracef("SyncActiveContext: skipping dormant channel %s (LastPostAt: %v)", chanName, time.UnixMilli(ch.LastPostAt))

				continue
			}
		}

		uMap, ok := m.Users.channels[chanID]
		if !ok || (totalUsers > 50 && len(uMap) >= maxChannelSize) {
			if ok {
				m.logger.Tracef("SyncActiveContext: skipping broadcast channel %s (%d users >= %d max)", chanName, len(uMap), maxChannelSize)
			}

			continue
		}

		m.logger.Tracef("SyncActiveContext: including channel %s (%d users)", chanName, len(uMap))

		for uID := range uMap {
			// Skip deactivated / suspended users
			if user, userExists := m.Users.users[uID]; userExists && user.DeleteAt > 0 {
				continue
			}

			seen[uID] = struct{}{}
		}
	}

	// Always collect DM partners
	myID := ""
	if m.User != nil {
		myID = m.User.Id
	}

	for _, ch := range m.Users.channelData {
		if ch.Type != model.ChannelTypeDirect {
			continue
		}

		// Skip dormant DMs with no recent activity
		if ch.LastPostAt > 0 && ch.LastPostAt < dormantThresholdMs {
			continue
		}

		if otherID, ok := m.GetDMOtherUserID(ch.Name, myID); ok && otherID != "" {
			// Skip deactivated / suspended DM partner
			if user, userExists := m.Users.users[otherID]; userExists && user.DeleteAt > 0 {
				continue
			}

			seen[otherID] = struct{}{}
		}
	}

	m.Users.mu.RUnlock()

	if len(seen) == 0 {
		return
	}

	userIDs := make([]string, 0, len(seen))
	for id := range seen {
		userIDs = append(userIDs, id)
	}

	m.WsGetStatusesByIds(userIDs)
}

func (m *Client) UpdateTeamUsersCache(teamID string, user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.UpdateUser(user, true)

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

// wsSafely runs fn, which sends on the websocket client. Once the connection is gone (also when it
// dropped by itself) the Mattermost websocket client panics with "send on closed channel", return
// that as an error instead of crashing the process.
func wsSafely(fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("websocket connection closed: %v", r)
		}
	}()

	fn()

	return nil
}

// WsPing sends a WebSocket ping and blocks until the server responds or ctx times out.
func (m *Client) WsPing(ctx context.Context) error {
	ws, connected := m.wsSnapshot()
	if ws == nil || !connected {
		return errors.New("websocket not connected")
	}

	seq := m.pingSeq.Add(1)
	ch := make(chan struct{}, 1)

	m.pingChans.Store(seq, ch)
	defer func() {
		m.pingChans.Delete(seq)
	}()

	data := map[string]any{
		"seq": seq,
	}

	m.logger.Trace("WsPing: sending ping")

	err := wsSafely(func() { ws.SendMessage("ping", data) })
	if err != nil {
		return err
	}

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WsSendTyping broadcasts a user_typing event to Mattermost with a 4-second debounce per channel.
func (m *Client) WsSendTyping(channelID, parentID string) error {
	ws, connected := m.wsSnapshot()
	if ws == nil || !connected {
		return errors.New("websocket not connected")
	}

	m.typingLock.Lock()
	lastSent, exists := m.lastTypingSent[channelID]
	now := time.Now()

	if exists && now.Sub(lastSent) < 4*time.Second {
		m.typingLock.Unlock()
		return nil
	}

	if len(m.lastTypingSent) > 1024 {
		for id, t := range m.lastTypingSent {
			if now.Sub(t) >= 4*time.Second {
				delete(m.lastTypingSent, id)
			}
		}
	}

	m.lastTypingSent[channelID] = now
	m.typingLock.Unlock()

	if parentID != "" {
		m.logger.Tracef("WsSendTyping: sending user_typing for channel %s (parent %s)", channelID, parentID)
	} else {
		m.logger.Tracef("WsSendTyping: sending user_typing for channel %s", channelID)
	}

	return wsSafely(func() { ws.UserTyping(channelID, parentID) })
}

func (m *Client) antiIdle(ctx context.Context, channelID string, interval int) {
	if interval == 0 {
		interval = 60
	}

	m.logger.Debugf("starting antiIdle for %s every %d secs", channelID, interval)

	ticker := time.NewTicker(time.Second * time.Duration(interval))
	defer ticker.Stop()

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

//nolint:gocognit,gocyclo,funlen
func (m *Client) maintainUsersCache(ctx context.Context, event *model.WebSocketEvent) {
	switch event.EventType() {
	case model.WebsocketEventNewUser, model.WebsocketEventUserUpdated, model.WebsocketEventUserAdded, model.WebsocketEventAddedToTeam:
		var u *model.User

		if userVal, ok := event.GetData()["user"]; ok { //nolint:nestif
			switch v := userVal.(type) {
			case *model.User:
				u = m.UpdateUser(v)
			case string:
				if v != "" {
					var summary UserSummary

					err := json.Unmarshal([]byte(v), &summary)
					if err == nil {
						u = m.UpdateUserSummary(&summary)
					}
				}
			case map[string]any:
				b, err := json.Marshal(v)
				if err == nil {
					var summary UserSummary

					err = json.Unmarshal(b, &summary)
					if err == nil {
						u = m.UpdateUserSummary(&summary)
					}
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

		if eventType == model.WebsocketEventUserAdded { //nolint:nestif
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
				var tzLoc *time.Location
				if m.User != nil {
					tzLoc = m.User.GetTimezoneLocation()
				}

				m.Users.SetUserCustomStatus(u.Id, u.Props["customStatus"], tzLoc)
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
		channelID := broadcastChannelID(event)
		if userID, ok := event.GetData()["user_id"].(string); ok && channelID != "" {
			m.UpdateChannelUsersCacheRemove(channelID, userID)
			m.Users.lastUpdated.Store(time.Now().Unix())
		}

	case model.WebsocketEventPosted:
		channelID := broadcastChannelID(event)
		if channelID == "" {
			break
		}

		var post *model.Post

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			post = postPtr
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			// Fallback path: Driver left it as a JSON string
			post = &model.Post{}
			_ = json.Unmarshal([]byte(postStr), post)
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

		m.Users.RecordUserActivity(post.UserId)

	case model.WebsocketEventPostEdited:
		var post *model.Post

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			post = postPtr
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			post = &model.Post{}
			_ = json.Unmarshal([]byte(postStr), post)
		}

		if post != nil && post.Id != "" && m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
			m.postCache.Add(post.Id, post)
		}

		if post != nil && post.UserId != "" && !strings.HasPrefix(post.Type, "system_") {
			m.Users.RecordUserActivity(post.UserId)
		}

	case model.WebsocketEventPostDeleted:
		var (
			post   *model.Post
			postID string
		)

		if postPtr, ok := event.GetData()["post"].(*model.Post); ok {
			post = postPtr
			postID = postPtr.Id
		} else if postStr, ok := event.GetData()["post"].(string); ok && postStr != "" {
			post = &model.Post{}
			_ = json.Unmarshal([]byte(postStr), post)
			postID = post.Id
		} else if id, ok := event.GetData()["post_id"].(string); ok {
			postID = id
		}

		if postID != "" && m.postCache != nil {
			m.postCache.Remove(postID)
		}

		if post != nil && post.UserId != "" && !strings.HasPrefix(post.Type, "system_") {
			m.Users.RecordUserActivity(post.UserId)
		}

	case model.WebsocketEventChannelCreated, model.WebsocketEventChannelUpdated, model.WebsocketEventDirectAdded, model.WebsocketEventGroupAdded:
		var channel *model.Channel

		if chPtr, ok := event.GetData()["channel"].(*model.Channel); ok {
			channel = chPtr
		} else if chStr, ok := event.GetData()["channel"].(string); ok && chStr != "" {
			var summary ChannelSummary

			_ = json.Unmarshal([]byte(chStr), &summary)
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
		if channel == nil || channel.Id == "" { //nolint:nestif
			if event.EventType() != model.WebsocketEventChannelUpdated {
				channelID, _ := event.GetData()["channel_id"].(string)
				if channelID == "" && event.GetBroadcast() != nil {
					channelID = event.GetBroadcast().ChannelId
				}

				if channelID != "" {
					channel = m.GetChannel(ctx, channelID)
				}
			}
		}

		if channel == nil || channel.Id == "" {
			break
		}

		// We have a valid channel object, cache it
		m.Users.mu.Lock()
		m.Users.channelData[channel.Id] = channel
		m.Users.mu.Unlock()
		m.Users.lastUpdated.Store(time.Now().Unix())

		// Pre-fetch members for new Group DMs so getGroupChannelName works immediately
		if channel.Type == model.ChannelTypeGroup {
			go func(cID string) {
				_, _ = m.GetChannelUsers(ctx, cID)
			}(channel.Id)
		}

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
		if channelTimes, ok := event.GetData()["channel_times"].(map[string]any); ok {
			m.Users.mu.Lock()
			for chanID, timeVal := range channelTimes {
				if ts, ok := timeVal.(float64); ok {
					m.Users.channelLastViewedAt[chanID] = int64(ts)
				}
			}
			m.Users.mu.Unlock()
		}

	// model.WebsocketEventChannelViewed deprecated per https://github.com/mattermost/mattermost/pull/24003
	// but let's continue to support it for now.
	case "channel_viewed":
		if channelID, ok := event.GetData()["channel_id"].(string); ok && channelID != "" {
			m.Users.mu.Lock()
			// channel_viewed doesn't always contain a timestamp in the payload.
			// If it doesn't, we can just use the current time to update it locally!
			m.Users.channelLastViewedAt[channelID] = time.Now().UnixNano() / int64(time.Millisecond)
			m.Users.mu.Unlock()
		}

	case model.WebsocketEventReactionAdded:
		if reactionStr, ok := event.GetData()["reaction"].(string); ok && reactionStr != "" {
			var reaction model.Reaction

			err := json.Unmarshal([]byte(reactionStr), &reaction)
			if err == nil && reaction.UserId != "" {
				m.Users.RecordUserActivity(reaction.UserId)
			}
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

		var lastActivity int64

		if laVal, ok := event.GetData()["last_activity_at"]; ok {
			switch v := laVal.(type) {
			case float64:
				lastActivity = int64(v) / 1000
			case int64:
				lastActivity = v / 1000
			}
		}

		if lastActivity > 0 {
			m.Users.mu.Lock()
			m.setUserStatus(userID, statusRaw, true, "websocket "+string(model.WebsocketEventStatusChange), lastActivity)
			m.markStatusVerifiedLocked(userID, lastActivity)
			m.Users.mu.Unlock()
		} else {
			m.SetUserStatus(userID, statusRaw, false)
		}

	case model.WebsocketEventTyping:
		userID, _ := event.GetData()["user_id"].(string)
		if userID == "" && event.GetBroadcast() != nil {
			userID = event.GetBroadcast().UserId
		}

		if userID != "" {
			m.Users.RecordUserActivity(userID)
		}
	}
}

// broadcastChannelID returns the channel the event was sent to, the broadcast is nil when the event has none.
func broadcastChannelID(event *model.WebSocketEvent) string {
	if broadcast := event.GetBroadcast(); broadcast != nil {
		return broadcast.ChannelId
	}

	return ""
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

// syncJoinedChannelsCache synchronously updates the joined channels cache
// to prevent race conditions before handing off to background processes.
//
//nolint:funlen,gocyclo
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
			m.Users.markJoinedLocked(broadcast.ChannelId)
		} else {
			m.Users.markLeftLocked(broadcast.ChannelId)
		}
		m.Users.mu.Unlock()

	case model.WebsocketEventChannelCreated, model.WebsocketEventDirectAdded, model.WebsocketEventGroupAdded:
		var (
			chID   string
			chType model.ChannelType
		)

		if chPtr, ok := event.GetData()["channel"].(*model.Channel); ok {
			chID = chPtr.Id
			chType = chPtr.Type
		} else if chStr, ok := event.GetData()["channel"].(string); ok && chStr != "" {
			// Fallback path: Fast partial unmarshal
			var ch struct {
				ID   string            `json:"id"`
				Type model.ChannelType `json:"type"`
			}

			err := json.Unmarshal([]byte(chStr), &ch)
			if err == nil {
				chID = ch.ID
				chType = ch.Type
			}
		} else if groupChannelID, ok := event.GetData()["channel_id"].(string); ok && groupChannelID != "" {
			chID = groupChannelID
			chType = model.ChannelTypeGroup
		} else if event.GetBroadcast() != nil && event.GetBroadcast().ChannelId != "" {
			chID = event.GetBroadcast().ChannelId
			chType = model.ChannelTypeGroup
		}

		if chID == "" {
			break
		}

		if chType == model.ChannelTypeDirect || chType == model.ChannelTypeGroup {
			m.Users.mu.Lock()
			m.Users.markJoinedLocked(chID)
			m.Users.mu.Unlock()
		}
	}
}

func (m *Client) syncPeriodicStatus(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.SyncActiveContextStatuses()
		}
	}
}

//nolint:unused
func (m *Client) syncSingleUser(ctx context.Context, event *model.WebSocketEvent) {
	userID, ok := event.GetData()["user_id"].(string)
	if !ok {
		return
	}

	userName := m.GetCachedUserName(userID)
	m.apiLogger.Warnf("syncSingleUser: GetUser: User: %s (%s)", userName, userID)

	user, _, err := m.Client.GetUser(ctx, userID, "")
	if err != nil {
		m.logger.Errorf("syncSingleUser failed to get user %s: %v", userID, err)
		return
	}

	m.logger.Debugf("dynamically caching updated/new user: %s", user.Username)

	teamID, _ := event.GetData()["team_id"].(string)

	m.UpdateTeamUsersCache(teamID, user)
}
