// Package slackclient is a small Slack client library used by matterircd
// (and usable by matterbridge), the equivalent of matterclient for Mattermost.
package slackclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	prefixed "github.com/matterbridge/logrus-prefixed-formatter"
	"github.com/sirupsen/logrus"
	"github.com/slack-go/slack"
)

type Credentials struct {
	Login    string
	Team     string
	Pass     string
	Token    string
	MFAToken string
}

type presenceEntry struct {
	presence   slack.UserPresence
	fetchedAt  int64
	verifiedAt int64
}

type userChannelsEntry struct {
	channels  []string
	fetchedAt int64
}

type channelMembersEntry struct {
	members   []string
	fetchedAt int64
}

type Client struct {
	sync.RWMutex

	cred              Credentials
	token             string
	httpClient        *httpClient
	sc                *slack.Client
	rtm               *slack.RTM
	info              *slack.Info
	users             map[string]slack.User
	presences         map[string]presenceEntry
	channels          map[string]string
	channelData       map[string]slack.Channel
	channelsFetchedAt int64
	joinedChannels    map[string]bool
	channelMembers    map[string]channelMembersEntry
	userChannels      map[string]userChannelsEntry
	dmChannels        map[string]string
	recentSent        map[string][]string
	sending           map[string]int
	sentCh            chan struct{}
	pongCh            chan struct{}
	connected         atomic.Bool
	usersLoaded       atomic.Bool
	initUsersMu       sync.Mutex
	initUsersDone     chan struct{}
	initUsersCancel   context.CancelFunc

	logger        *logrus.Entry
	rootLogger    *logrus.Logger
	apiLogger     *logrus.Entry
	rootAPILogger *logrus.Logger
}

func New(cred Credentials) *Client {
	// Logger for the rest of slackclient
	rootLogger := logrus.New()
	rootLogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 13,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Logger for Slack API calls
	rootAPILogger := logrus.New()
	rootAPILogger.SetFormatter(&prefixed.TextFormatter{
		PrefixPadding: 18,
		DisableColors: false,
		FullTimestamp: true,
	})
	// Default to higher than "warn" to not log anything
	rootAPILogger.SetLevel(logrus.ErrorLevel)

	return &Client{
		cred:           cred,
		users:          make(map[string]slack.User),
		presences:      make(map[string]presenceEntry),
		channels:       make(map[string]string),
		channelData:    make(map[string]slack.Channel),
		joinedChannels: make(map[string]bool),
		channelMembers: make(map[string]channelMembersEntry),
		userChannels:   make(map[string]userChannelsEntry),
		dmChannels:     make(map[string]string),
		recentSent:     make(map[string][]string),
		sending:        make(map[string]int),
		sentCh:         make(chan struct{}),
		pongCh:         make(chan struct{}, 1),
		rootLogger:     rootLogger,
		logger:         rootLogger.WithFields(logrus.Fields{"prefix": "slackclient"}),
		rootAPILogger:  rootAPILogger,
		apiLogger:      rootAPILogger.WithFields(logrus.Fields{"prefix": "slackclient: API"}),
	}
}

func (c *Client) Login() error {
	var err error

	if c.cred.Token == "" {
		c.cred.Token, err = c.getSlackToken()
		if err != nil {
			return err
		}
	}

	var cookie string

	token := c.cred.Token

	if strings.HasPrefix(c.cred.Token, "xoxc") {
		token, cookie, err = passwordToTokenAndCookie(c.cred.Token)
		if err != nil {
			return err
		}
	}

	hc := &httpClient{
		cookie:    cookie,
		logger:    c.logger,
		apiLogger: c.apiLogger,
	}

	opts := []slack.Option{
		slack.OptionDebug(true),
		slack.OptionLog(slackLogAdapter{logger: c.apiLogger}),
		slack.OptionHTTPClient(hc),
	}

	var rtmOpts []slack.RTMOption

	if cookie != "" {
		rtmOpts = append(rtmOpts, slack.RTMOptionDialer(newCookieDialer(cookie)))
	}

	c.Lock()
	c.token = token
	c.httpClient = hc
	c.sc = slack.New(token, opts...)
	c.rtm = c.sc.NewRTM(rtmOpts...)
	c.users = make(map[string]slack.User)
	c.presences = make(map[string]presenceEntry)
	c.channels = make(map[string]string)
	c.userChannels = make(map[string]userChannelsEntry)
	c.Unlock()
	c.resetUserPrefetch()

	go c.rtm.ManageConnection()

	err = c.waitForInfo()
	if err != nil {
		return err
	}

	c.connected.Store(true)

	return nil
}

func (c *Client) waitForInfo() error {
	c.info = c.rtm.GetInfo()
	for count := 0; c.info == nil; count++ {
		if count == 20 {
			return errors.New("couldn't connect in 10 seconds. Check your credentials")
		}

		time.Sleep(time.Millisecond * 500)
		c.logger.Debug("still waiting for sinfo")

		c.info = c.rtm.GetInfo()
	}

	return nil
}

func (c *Client) Disconnect() error {
	if c.rtm == nil {
		return nil
	}

	return c.rtm.Disconnect()
}

func (c *Client) Logout() error {
	c.logger.Debug("calling logout from slack")

	err := c.Disconnect()
	if err != nil {
		c.logger.Debug("logoutfrom slack", err)
		return err
	}

	c.connected.Store(false)
	c.resetUserPrefetch()

	c.Lock()
	c.sc = nil
	c.token = ""
	c.httpClient = nil
	c.recentSent = nil
	c.sending = nil
	c.broadcastSentLocked()
	c.Unlock()

	c.logger.Info("logout succeeded")

	return nil
}

func (c *Client) Connected() bool {
	return c.connected.Load()
}

func (c *Client) NotifyPong() {
	if c.pongCh == nil {
		return
	}

	select {
	case c.pongCh <- struct{}{}:
	default:
	}
}

func (c *Client) HttpPing(ctx context.Context) error {
	if c.sc == nil || !c.connected.Load() {
		return errors.New("slackclient: not connected")
	}

	_, err := c.sc.AuthTestContext(ctx)

	return err
}

func (c *Client) WsPing(ctx context.Context) error {
	if c.rtm == nil || !c.connected.Load() || c.rtm.GetInfo() == nil {
		return errors.New("slackclient: websocket not connected")
	}

	if c.pongCh != nil {
		select {
		case <-c.pongCh:
		default:
		}
	}

	c.rtm.SendMessage(&slack.OutgoingMessage{Type: "ping"})

	if c.pongCh == nil {
		return nil
	}

	select {
	case <-c.pongCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) Ping(ctx context.Context) error {
	return c.HttpPing(ctx)
}

func (c *Client) IncomingEvents() <-chan slack.RTMEvent {
	return c.rtm.IncomingEvents
}

// SetLogLevel sets the log level of slackclient.
func (c *Client) SetLogLevel(level string) {
	setLevel(c.rootLogger, level)
}

// SetLogAPICalls sets the log level of the Slack API calls.
func (c *Client) SetLogAPICalls(level string) {
	setLevel(c.rootAPILogger, level)
}

func setLevel(l *logrus.Logger, level string) {
	lvl, err := logrus.ParseLevel(level)
	if err != nil {
		l.Warnf("invalid log level %q, keeping %s", level, l.GetLevel())

		return
	}

	l.SetLevel(lvl)
}

//nolint:unused
func (c *Client) ratelimitCheck(err error) {
	if rateLimitedError, ok := err.(*slack.RateLimitedError); ok {
		time.Sleep(rateLimitedError.RetryAfter)
	}
}

//nolint:funlen
func (c *Client) getSlackToken() (string, error) {
	type findTeamResponseFull struct {
		slack.SlackResponse

		SSO    bool   `json:"sso"`
		TeamID string `json:"team_id"`
	}

	type loginResponseFull struct {
		slack.SlackResponse

		Token string `json:"token"`
	}

	resp, err := http.PostForm("https://slack.com/api/auth.findTeam", url.Values{"domain": {c.cred.Team}})
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var findTeamResponse findTeamResponseFull

	err = json.Unmarshal(body, &findTeamResponse)
	if err != nil {
		return "", err
	}

	if findTeamResponse.SSO {
		return "", errors.New("SSO teams not yet supported")
	}

	signinValues := url.Values{
		"team":     {findTeamResponse.TeamID},
		"email":    {c.cred.Login},
		"password": {c.cred.Pass},
	}
	if c.cred.MFAToken != "" {
		signinValues.Set("pin", c.cred.MFAToken)
	}

	resp, err = http.PostForm("https://slack.com/api/auth.signin", signinValues)
	if err != nil {
		return "", err
	}

	defer resp.Body.Close()

	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var loginResponse loginResponseFull

	err = json.Unmarshal(body, &loginResponse)
	if err != nil {
		return "", err
	}

	if !loginResponse.Ok {
		return "", errors.New(loginResponse.Error)
	}

	return loginResponse.Token, nil
}

// passwordToTokenAndCookie parses the password specified by the user into a
// Slack token and optionally a cookie Auth cookies can be specified by
// appending a "|" symbol and the base64-encoded auth cookie to the Slack token.
// taken from https://github.com/insomniacslk/irc-slack/blob/master/pkg/ircslack/irc_server.go
func passwordToTokenAndCookie(p string) (string, string, error) {
	parts := strings.Split(p, "|")

	switch len(parts) {
	case 1:
		// XXX should check that the token starts with xoxp- ?
		return parts[0], "", nil
	case 2:
		if !strings.HasPrefix(parts[0], "xoxc-") {
			return "", "", errors.New("auth cookie is set, but token does not start with xoxc-")
		}

		if parts[1] == "" {
			return "", "", errors.New("auth cookie is empty")
		}

		if !strings.HasPrefix(parts[1], "d=") || !strings.HasSuffix(parts[1], ";") {
			return "", "", errors.New("auth cookie must have the format 'd=XXX;'")
		}

		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("failed to parse password into token and cookie, got %d components, want 1 or 2", len(parts))
	}
}

// custom HTTP client used to set the auth cookie if requested, and only over
// TLS.
// taken from https://github.com/insomniacslk/irc-slack/blob/master/pkg/ircslack/irc_server.go
type httpClient struct {
	c         http.Client
	cookie    string
	logger    *logrus.Entry
	apiLogger *logrus.Entry
}

// taken from https://github.com/insomniacslk/irc-slack/blob/master/pkg/ircslack/irc_server.go
func (hc *httpClient) Do(req *http.Request) (*http.Response, error) {
	if hc.cookie != "" {
		hc.logger.Trace("Setting auth cookie")

		if strings.ToLower(req.URL.Scheme) == "https" {
			req.Header.Add("Cookie", hc.cookie)
		} else {
			hc.logger.Warning("Cookie is set but connection is not HTTPS, skipping")
		}
	}

	start := time.Now()

	resp, err := hc.c.Do(req)
	duration := time.Since(start).Round(time.Millisecond)

	if err != nil {
		hc.apiLogger.Warnf("%s %s: %s (%s)", req.Method, req.URL.Path, err, duration)
		return resp, err
	}

	// Only the path is logged; the query string or body can carry credentials.
	hc.apiLogger.Warnf("%s %s: %d (%s)", req.Method, req.URL.Path, resp.StatusCode, duration)

	return resp, nil
}

func newCookieDialer(cookie string) *websocket.Dialer {
	val := strings.TrimSuffix(strings.TrimPrefix(cookie, "d="), ";")
	dialer := *websocket.DefaultDialer
	dialer.Jar = &staticCookieJar{
		cookies: []*http.Cookie{{
			Name:     "d",
			Value:    val,
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
		}},
	}

	return &dialer
}

type slackLogAdapter struct {
	logger *logrus.Entry
}

func (l slackLogAdapter) Output(_ int, s string) error {
	l.logger.Trace(strings.TrimRight(s, "\r\n"))

	return nil
}

type staticCookieJar struct {
	cookies []*http.Cookie
}

func (j *staticCookieJar) SetCookies(_ *url.URL, _ []*http.Cookie) {}

func (j *staticCookieJar) Cookies(u *url.URL) []*http.Cookie {
	if strings.EqualFold(u.Scheme, "https") {
		return j.cookies
	}

	return nil
}
