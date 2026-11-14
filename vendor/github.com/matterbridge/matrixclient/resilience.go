package matrixclient

import (
	"context"
	"errors"
	"net/http"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

const (
	maxSyncBackoff   = time.Minute
	maxSyncRetryWait = 30 * time.Second
	maxLoginRetries  = 10
	maxAPIRetries    = 10
	defaultRateLimit = 3 * time.Second

	// Users not seen active for this long are reported offline, matching matterclient.
	activeThreshold = 20 * time.Minute
	maintainEvery   = time.Minute

	// Bounds and rate-limits per-user status and timezone lookups, matching matterclient.
	inactiveFetchTimeout   = 3 * time.Second
	freshStatusMinInterval = 30 * time.Second
	timezoneTTL            = 5 * time.Minute
	maxSharedTimezones     = 1024

	pingSkipWindow = 30 * time.Second // /sync long polls return at least this often

	lastViewedDebounce = 3 * time.Second
	typingDebounce     = 4 * time.Second
	debounceMapLimit   = 1024
)

// viewedMark is the last read receipt we sent for a room.
type viewedMark struct {
	eventID id.EventID
	at      time.Time
}

func loginReq(cred Credentials, deviceID id.DeviceID) *mautrix.ReqLogin {
	return &mautrix.ReqLogin{
		Type:             mautrix.AuthTypePassword,
		Identifier:       mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: cred.Login},
		Password:         cred.Pass,
		DeviceID:         deviceID,
		StoreCredentials: true,

		InitialDeviceDisplayName: cred.DeviceName,
	}
}

// applyDeviceName renames our device when it differs, as the login name only
// applies to newly created devices. Failure is not fatal.
func (c *Client) applyDeviceName(ctx context.Context) {
	if c.cred.DeviceName == "" || c.mc.DeviceID == "" {
		return
	}

	info, err := c.mc.GetDeviceInfo(ctx, c.mc.DeviceID)
	if err == nil && info.DisplayName == c.cred.DeviceName {
		return
	}

	err = c.mc.SetDeviceInfo(ctx, c.mc.DeviceID, &mautrix.ReqDeviceInfo{DisplayName: c.cred.DeviceName})
	if err != nil {
		c.logger.Warnf("could not set device name: %s", err)
	}
}

func (c *Client) onSynced() (bool, time.Duration) {
	now := time.Now()
	prevSync := c.lastSync.Swap(now.UnixNano())
	wasConnected := c.connected.Swap(true)
	wasSynced := c.synced.Swap(true)

	if !wasSynced || wasConnected {
		return false, 0
	}

	var downtime time.Duration
	if prevSync > 0 {
		downtime = now.Sub(time.Unix(0, prevSync))
	}

	return true, downtime
}

func (c *Client) notifyReconnect(downtime time.Duration) {
	if h, ok := c.handler.(ReconnectHandler); ok {
		h.OnReconnect(downtime)
	}
}

func (c *Client) onSyncFailed(err error, wait time.Duration) {
	c.connected.Store(false)
	c.apiLogger.Warnf("sync failed: %s, retrying in %s", err, wait.Round(time.Millisecond))
}

// relogin obtains a new access token, reusing the device. It must only be
// called while the sync loop is not running a sync, as it replaces the token.
func (c *Client) relogin(ctx context.Context) error {
	if c.cred.Token != "" {
		return errors.New("matrixclient: access token expired and cannot be renewed")
	}

	for attempt := 0; ; attempt++ {
		_, err := c.mc.Login(ctx, loginReq(c.cred, c.mc.DeviceID))
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "login", err, attempt, maxLoginRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// HandleRetry decides whether a failed API call should be retried, sleeping
// before returning true. Rate limits respect the server's retry_after_ms and
// other 4xx errors are not retried. It returns an error only when ctx is done.
func (c *Client) HandleRetry(ctx context.Context, name string, err error, current, maxLimit int) (bool, error) {
	if ctx.Err() != nil {
		return false, ctx.Err()
	}

	if current >= maxLimit {
		return false, nil
	}

	wait := time.Duration(current+1) * time.Second

	var httpErr mautrix.HTTPError
	if errors.As(err, &httpErr) && httpErr.Response != nil {
		status := httpErr.Response.StatusCode

		switch {
		case status == http.StatusTooManyRequests:
			wait = defaultRateLimit

			if httpErr.RespError != nil {
				if ms, ok := httpErr.RespError.ExtraData["retry_after_ms"].(float64); ok && ms > 0 {
					wait = time.Duration(ms) * time.Millisecond
				}
			}
		case status < http.StatusInternalServerError:
			return false, nil
		}
	}

	c.logger.Warnf("%s failed: %s, retrying in %s (attempt %d/%d)", name, err, wait, current+1, maxLimit)

	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(wait):
		return true, nil
	}
}

// Reconnect restarts the sync and logs in again, reusing the device. It
// returns immediately, the sync goroutine does the work so the access token
// is never replaced while a request is using it.
func (c *Client) Reconnect() {
	c.forceRelogin.Store(true)
	c.mc.StopSync()
}

// Ping checks that the homeserver is reachable and our access token is valid.
// A recent sync response already proves that, so no request is made then.
func (c *Client) Ping(ctx context.Context) error {
	if c.connected.Load() && time.Since(time.Unix(0, c.lastSync.Load())) < pingSkipWindow {
		return nil
	}

	for attempt := 0; ; attempt++ {
		_, err := c.mc.Whoami(ctx)
		if err == nil {
			return nil
		}

		retry, herr := c.HandleRetry(ctx, "Ping", err, attempt, maxAPIRetries)
		if herr != nil {
			return herr
		}

		if !retry {
			return err
		}
	}
}

// ServerVersions returns the Matrix spec versions supported by the homeserver.
func (c *Client) ServerVersions(ctx context.Context) ([]string, error) {
	var (
		resp *mautrix.RespVersions
		err  error
	)

	for attempt := 0; ; attempt++ {
		resp, err = c.mc.Versions(ctx)
		if err == nil {
			break
		}

		retry, herr := c.HandleRetry(ctx, "ServerVersions", err, attempt, maxAPIRetries)
		if herr != nil {
			return nil, herr
		}

		if !retry {
			return nil, err
		}
	}

	versions := make([]string, 0, len(resp.Versions))
	for _, v := range resp.Versions {
		versions = append(versions, v.String())
	}

	return versions, nil
}

// LastSync returns when the last sync response was processed, the zero time if none yet.
func (c *Client) LastSync() time.Time {
	ns := c.lastSync.Load()
	if ns == 0 {
		return time.Time{}
	}

	return time.Unix(0, ns)
}

// maintainPresence reports users without recent activity as offline and drops
// presence of users we no longer share a room with.
func (c *Client) maintainPresence() {
	ticker := time.NewTicker(maintainEvery)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			c.expirePresence(time.Now())
		}
	}
}

func (c *Client) expirePresence(now time.Time) {
	cutoff := now.Add(-activeThreshold).UnixMilli()

	c.Lock()
	defer c.Unlock()

	for userID, p := range c.presence {
		if _, ok := c.users[userID]; !ok {
			delete(c.presence, userID)
			delete(c.presenceFetched, userID)

			continue
		}

		if p.Status != StatusOffline && p.LastActivityAt > 0 && p.LastActivityAt < cutoff {
			p.Status = StatusOffline
			c.presence[userID] = p
		}
	}
}

// logSyncSummary explains what woke a sync up, to tell short back to back syncs apart.
func (c *Client) logSyncSummary(res *mautrix.RespSync) {
	c.apiLogger.Tracef("sync content: joined=%d invited=%d left=%d presence=%d to_device=%d account_data=%d devices_changed=%d otk=%v",
		len(res.Rooms.Join), len(res.Rooms.Invite), len(res.Rooms.Leave), len(res.Presence.Events),
		len(res.ToDevice.Events), len(res.AccountData.Events), len(res.DeviceLists.Changed), res.DeviceOTKCount)
}
