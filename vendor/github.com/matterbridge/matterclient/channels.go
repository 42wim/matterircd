package matterclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func (m *Client) CreateChannel(ctx context.Context, teamID string, channelName string, channelType model.ChannelType) (*model.Channel, error) {
	if channelType == "" {
		channelType = model.ChannelTypeOpen
	}

	m.Users.mu.RLock()

	// Check existing channelData cache first
	for _, ch := range m.Users.channelData {
		if ch.TeamId == teamID && ch.Name == channelName {
			m.Users.mu.RUnlock()
			return ch, nil
		}
	}

	m.Users.mu.RUnlock()

	// Fallback to creating the channel via REST API
	channelReq := &model.Channel{
		Name:        channelName,
		DisplayName: channelName,
		Type:        channelType,
		TeamId:      teamID,
	}

	retryCount := 0
	for {
		m.apiLogger.Warnf("CreateChannel: CreateChannel for %s #%d", channelName, retryCount)

		mmchannel, resp, err := m.Client.CreateChannel(ctx, channelReq)
		if err == nil {
			// Cache it so subsequent GetChannel calls are fast
			m.Users.mu.Lock()
			m.Users.channelData[mmchannel.Id] = mmchannel
			m.Users.mu.Unlock()

			return mmchannel, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreateChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("CreateChannel failed for %s: %v", channelName, err)

		return nil, err
	}
}

func (m *Client) CreateDirectChannel(ctx context.Context, userID1, userID2 string) (*model.Channel, error) {
	dmName := model.GetDMNameFromIds(userID1, userID2)

	m.Users.mu.RLock()

	// Check existing channelData cache first
	for _, ch := range m.Users.channelData {
		if ch.Type == model.ChannelTypeDirect && ch.Name == dmName {
			m.Users.mu.RUnlock()
			return ch, nil
		}
	}

	m.Users.mu.RUnlock()

	// Fallback to REST API
	retryCount := 0
	for {
		m.apiLogger.Warnf("CreateDirectChannel for users %s and %s (%s) #%d", userID1, userID2, dmName, retryCount)

		mmchannel, resp, err := m.Client.CreateDirectChannel(ctx, userID1, userID2)
		if err == nil {
			// Cache the DM channel so GetChannel is fast
			m.Users.mu.Lock()
			m.Users.channelData[mmchannel.Id] = mmchannel
			m.Users.mu.Unlock()

			return mmchannel, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreateDirectChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("CreateDirectChannel failed for %s, %s (%s): %v", userID1, userID2, dmName, err)

		return nil, err
	}
}

// GetCachedChannel returns the channel from local cache without triggering a REST API lookup.
func (m *Client) GetCachedChannel(channelID string) *model.Channel {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	return m.Users.channelData[channelID]
}

// GetCachedChannelName returns the normalised channel name if cached, or empty string.
func (m *Client) GetCachedChannelName(channelID string) string {
	// getNormalisedName reads the users/channels caches, which are modified concurrently.
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	if ch := m.Users.channelData[channelID]; ch != nil {
		return m.getNormalisedName(ch)
	}

	return ""
}

// GetCachedChannelID returns the channel ID for a channel name and team ID from cache.
func (m *Client) GetCachedChannelID(channelName, teamID string) string {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	for _, ch := range m.Users.channelData {
		if (ch.TeamId == teamID || ch.Type == model.ChannelTypeGroup) && (ch.Name == channelName || (ch.Type == model.ChannelTypeGroup && m.getNormalisedName(ch) == channelName)) {
			return ch.Id
		}
	}

	return ""
}

// GetCachedDirectChannelID returns the channel ID for a DM with the given username from cache.
func (m *Client) GetCachedDirectChannelID(username string) string {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	var targetUser *model.User

	for _, u := range m.Users.users {
		if u.Username == username {
			targetUser = u
			break
		}
	}

	if targetUser == nil {
		return ""
	}

	dmName := m.GetDMChannelName(m.User.Id, targetUser.Id)
	for _, ch := range m.Users.channelData {
		if ch.Type == model.ChannelTypeDirect && ch.Name == dmName {
			return ch.Id
		}
	}

	return ""
}

//nolint:funlen
func (m *Client) GetChannel(ctx context.Context, channelID string) *model.Channel {
	m.Users.mu.RLock()
	ch, exists := m.Users.channelData[channelID]
	m.Users.mu.RUnlock()

	if exists {
		return ch
	}

	query := "/channels/" + channelID
	retryCount := 0

	for {
		m.apiLogger.Warnf("GetChannel: DoAPIGet: query %s #%d", query, retryCount)

		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err == nil {
			var summary ChannelSummary

			decodeErr := json.NewDecoder(resp.Body).Decode(&summary)
			resp.Body.Close()

			if decodeErr == nil {
				mmchannel := &model.Channel{
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

				m.Users.mu.Lock()
				m.Users.channelData[channelID] = mmchannel
				m.Users.mu.Unlock()

				return mmchannel
			}

			m.logger.Errorf("GetChannel decode failed for %s: %v", channelID, decodeErr)

			return nil
		}

		var mResp *model.Response
		if resp != nil {
			mResp = model.BuildResponse(resp)
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetChannel", err, retryCount, 10, mResp)

		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}

		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("GetChannel failed for %s: %v", channelID, err)

		return nil
	}
}

// GetChannelCount returns the number of active joined channels for the current user.
func (m *Client) GetChannelCount() int {
	if m == nil || m.Users == nil {
		return 0
	}

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	var count int

	for id, ch := range m.Users.channelData {
		if ch.DeleteAt != 0 {
			continue
		}

		if _, joined := m.Users.joinedChannels[id]; joined {
			count++
		}
	}

	return count
}

// GetChannels returns all channels we're members off
func (m *Client) GetChannels() []*model.Channel {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	channels := make([]*model.Channel, 0, len(m.Users.joinedChannels))
	for id := range m.Users.joinedChannels {
		if ch, exists := m.Users.channelData[id]; exists {
			channels = append(channels, ch)
		}
	}

	return channels
}

func (m *Client) GetChannelHeader(ctx context.Context, channelID string) string {
	if ch := m.GetChannel(ctx, channelID); ch != nil {
		return ch.Header
	}

	return ""
}

func (m *Client) getNormalisedName(channel *model.Channel) string {
	if channel.Type == model.ChannelTypeGroup {
		if channel.DisplayName != "" {
			res := strings.ReplaceAll(channel.DisplayName, ", ", "-")
			res = strings.ReplaceAll(res, " ", "_")

			return res
		}

		if groupName := m.getGroupChannelName(channel.Id); groupName != "" {
			return groupName
		}
	}

	return channel.Name
}

func (m *Client) GetChannelID(ctx context.Context, name string, teamID string) string {
	if teamID != "" {
		return m.getChannelIDTeam(ctx, name, teamID)
	}

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	for _, ch := range m.Users.channelData {
		if ch.Name == name || (ch.Type == model.ChannelTypeGroup && m.getNormalisedName(ch) == name) {
			return ch.Id
		}
	}

	return ""
}

//nolint:funlen,gocognit,gocyclo
func (m *Client) getChannelIDTeam(ctx context.Context, name string, teamID string) string {
	m.Users.mu.RLock()

	for _, ch := range m.Users.channelData {
		if ch.TeamId != teamID && ch.Type != model.ChannelTypeDirect && ch.Type != model.ChannelTypeGroup {
			continue
		}

		// Check raw channel name first
		if strings.EqualFold(ch.Name, name) {
			m.Users.mu.RUnlock()

			return ch.Id
		}

		// Check normalized name only if type is group
		if ch.Type == model.ChannelTypeGroup {
			if strings.EqualFold(m.getNormalisedName(ch), name) {
				m.Users.mu.RUnlock()

				return ch.Id
			}

			// Renamed Group DM fallback: match by member usernames
			groupName := m.getGroupChannelName(ch.Id)
			if groupName != "" && strings.EqualFold(groupName, name) {
				m.Users.mu.RUnlock()

				return ch.Id
			}
		}
	}

	m.Users.mu.RUnlock()

	query := "/teams/" + teamID + "/channels/name/" + name
	retryCount := 0

	for {
		m.apiLogger.Warnf("getChannelIDTeam: DoAPIGet: query %s #%d", query, retryCount)

		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err == nil {
			var summary ChannelSummary

			decodeErr := json.NewDecoder(resp.Body).Decode(&summary)
			resp.Body.Close()

			if decodeErr == nil {
				channel := &model.Channel{
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

				m.Users.mu.Lock()
				m.Users.channelData[channel.Id] = channel
				m.Users.mu.Unlock()

				return channel.Id
			}

			m.logger.Errorf("getChannelIDTeam decode failed for %s: %v", name, decodeErr)

			return ""
		}

		var mResp *model.Response
		if resp != nil {
			mResp = model.BuildResponse(resp)
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "getChannelIDTeam", err, retryCount, 10, mResp)

		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}

		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		if mResp != nil && mResp.StatusCode == http.StatusNotFound {
			m.logger.Warnf("getChannelIDTeam failed for %s: %v", name, err)
		} else {
			m.logger.Errorf("getChannelIDTeam failed for %s: %v", name, err)
		}

		return ""
	}
}

func (m *Client) getGroupChannelName(channelID string) string {
	usersMap, ok := m.Users.channels[channelID]
	if !ok || len(usersMap) == 0 {
		return ""
	}

	nicks := make([]string, 0, len(usersMap))
	for userID := range usersMap {
		if u, exists := m.Users.users[userID]; exists && u.Username != "" {
			nicks = append(nicks, u.Username)
		}
	}

	if len(nicks) == 0 {
		return ""
	}

	sort.Strings(nicks)

	return strings.Join(nicks, "-")
}

func (m *Client) GetChannelName(ctx context.Context, channelID string) string {
	if ch := m.GetChannel(ctx, channelID); ch != nil {
		myID := m.currentUserID()

		m.Users.mu.RLock()
		name := m.getNormalisedName(ch)
		unresolved := m.channelNameUnresolvedLocked(ch, name, myID)
		channelID := ch.Id

		m.Users.mu.RUnlock()

		if unresolved {
			m.healChannelName(ctx, channelID)
		}

		return name
	}

	return ""
}

const (
	nameHealInitialDelay = 5 * time.Second
	nameHealMaxDelay     = 5 * time.Minute
	nameHealMaxAttempts  = 12
)

// currentUserID returns the current user's ID under the client lock, which guards m.User.
// It must not be called with the users cache lock held.
func (m *Client) currentUserID() string {
	m.RLock()
	defer m.RUnlock()

	if m.User == nil {
		return ""
	}

	return m.User.Id
}

// channelNameUnresolvedLocked reports whether name is still only an ID (or empty), i.e. the
// data needed to build a readable name was missing. The users cache has to be locked.
func (m *Client) channelNameUnresolvedLocked(ch *model.Channel, name, myID string) bool {
	switch ch.Type {
	case model.ChannelTypeGroup:
		if name == "" || name == ch.Name {
			return true
		}

		if ch.DisplayName != "" {
			return false
		}

		// getGroupChannelName omits members without a username, so every member must be known.
		for userID := range m.Users.channels[ch.Id] {
			if u := m.Users.users[userID]; u == nil || u.Username == "" {
				return true
			}
		}

		return false
	case model.ChannelTypeDirect:
		if myID == "" {
			return false
		}

		otherID, ok := m.GetDMOtherUserID(ch.Name, myID)
		if !ok {
			return ch.Name == ""
		}

		u := m.Users.users[otherID]

		return u == nil || u.Username == ""
	default:
		return name == ""
	}
}

// healChannelName starts, at most once per channel, a background retry with exponential backoff
// that refetches the data needed to name the channel. It stops on success, on a 404 (channel
// is gone), after nameHealMaxAttempts, or when the client context is canceled.
func (m *Client) healChannelName(ctx context.Context, channelID string) {
	if _, running := m.nameHealing.LoadOrStore(channelID, struct{}{}); running {
		return
	}

	// Detach from the caller's cancellation (it may be a short-lived request context) but
	// still stop on Logout, which cancels the login context.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	stop := func() bool { return false }

	if lc := m.loginCtx.Load(); lc != nil {
		//nolint:contextcheck // Tied to the long-lived login session, not the caller's context
		stop = context.AfterFunc(*lc, cancel)
	}

	go func() {
		defer stop()
		defer cancel()
		defer m.nameHealing.Delete(channelID)

		delay := nameHealInitialDelay

		for attempt := 1; attempt <= nameHealMaxAttempts; attempt++ {
			resolved, final := m.refreshChannelName(ctx, channelID)
			if final {
				return
			}

			if resolved {
				m.logger.Infof("channel name for %s resolved after %d attempt(s)", channelID, attempt)

				if cb := m.OnChannelNameResolved; cb != nil {
					cb(channelID, m.resolvedChannelName(channelID))
				}

				return
			}

			m.logger.Debugf("channel name for %s still unresolved (attempt %d/%d), retrying in %s",
				channelID, attempt, nameHealMaxAttempts, delay)

			if ok, _ := m.sleepWithContext(ctx, delay); !ok {
				return
			}

			delay = min(delay*2, nameHealMaxDelay)
		}

		m.logger.Warnf("giving up resolving channel name for %s", channelID)
	}()
}

// resolvedChannelName returns the readable name of a channel: the peer's username for a DM,
// otherwise the normalised name. GetCachedChannelName keeps returning the raw DM name.
func (m *Client) resolvedChannelName(channelID string) string {
	myID := m.currentUserID()

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	ch := m.Users.channelData[channelID]
	if ch == nil {
		return ""
	}

	if ch.Type == model.ChannelTypeDirect && myID != "" {
		if otherID, ok := m.GetDMOtherUserID(ch.Name, myID); ok {
			if u := m.Users.users[otherID]; u != nil && u.Username != "" {
				return u.Username
			}
		}
	}

	return m.getNormalisedName(ch)
}

// fetchUserWithName fetches the user from the server unless a cached copy already has a username.
func (m *Client) fetchUserWithName(ctx context.Context, userID string) {
	m.Users.mu.RLock()
	u := m.Users.users[userID]
	hasName := u != nil && u.Username != ""

	m.Users.mu.RUnlock()

	if hasName {
		return
	}

	user, _, err := m.Client.GetUser(ctx, userID, "")
	if err != nil {
		m.logger.Debugf("fetching user %s for channel name failed: %v", userID, err)

		return
	}

	m.UpdateUser(user)
}

// refreshChannelName refetches the channel and the users needed for its name. It returns
// whether the name is now resolved, and whether retrying is pointless (channel gone, or
// context canceled).
func (m *Client) refreshChannelName(ctx context.Context, channelID string) (bool, bool) {
	resp, err := m.Client.DoAPIGet(ctx, "/channels/"+channelID, "")
	if err != nil {
		var statusCode int
		if resp != nil {
			statusCode = resp.StatusCode
			resp.Body.Close()
		}

		return false, statusCode == http.StatusNotFound || ctx.Err() != nil
	}

	var summary ChannelSummary

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err == nil {
		err = json.Unmarshal(body, &summary)
	}

	if err != nil {
		return false, ctx.Err() != nil
	}

	m.Users.mu.Lock()
	ch := m.Users.cacheChannelLocked(summary.TeamId, &summary)
	chType, chName := ch.Type, ch.Name
	m.Users.mu.Unlock()

	myID := m.currentUserID()

	switch chType {
	case model.ChannelTypeGroup:
		m.refreshGroupMembers(ctx, channelID)
	case model.ChannelTypeDirect:
		if myID != "" {
			if otherID, ok := m.GetDMOtherUserID(chName, myID); ok {
				m.fetchUserWithName(ctx, otherID)
			}
		}
	}

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	return !m.channelNameUnresolvedLocked(ch, m.getNormalisedName(ch), myID), ctx.Err() != nil
}

// refreshGroupMembers makes sure the users of a group channel are cached with their usernames.
func (m *Client) refreshGroupMembers(ctx context.Context, channelID string) {
	m.Users.mu.Lock()
	ids := make([]string, 0, len(m.Users.channels[channelID]))

	for id := range m.Users.channels[channelID] {
		ids = append(ids, id)
	}

	// GetChannelUsers returns an existing (even empty) entry without a request.
	if len(ids) == 0 {
		delete(m.Users.channels, channelID)
	}

	m.Users.mu.Unlock()

	if len(ids) == 0 {
		_, _ = m.GetChannelUsers(ctx, channelID)

		return
	}

	for _, id := range ids {
		m.fetchUserWithName(ctx, id)
	}
}

func (m *Client) GetChannelTeamID(ctx context.Context, id string) string {
	if ch := m.GetChannel(ctx, id); ch != nil {
		return ch.TeamId
	}

	return ""
}

func (m *Client) GetDMChannelName(userID1 string, userID2 string) string {
	return model.GetDMNameFromIds(userID1, userID2)
}

// GetDMUserIDs extracts the two user IDs from a direct message channel name (userID1__userID2).
// It returns ok as false if the channel name does not conform to the DM naming format.
func (m *Client) GetDMUserIDs(channelName string) (string, string, bool) {
	userID1, userID2, ok := strings.Cut(channelName, "__")
	if !ok || len(userID1) != mattermostIDLen || len(userID2) != mattermostIDLen {
		return "", "", false
	}

	return userID1, userID2, true
}

// GetDMOtherUserID extracts the opposite participant's user ID from a DM channel name.
func (m *Client) GetDMOtherUserID(channelName, myUserID string) (string, bool) {
	userID1, userID2, ok := m.GetDMUserIDs(channelName)
	if !ok {
		return "", false
	}

	if userID1 == myUserID {
		return userID2, true
	}

	if userID2 == myUserID {
		return userID1, true
	}

	return "", false
}

//nolint:funlen
func (m *Client) GetChannelUsers(ctx context.Context, channelID string) ([]*model.User, error) {
	m.Users.mu.RLock()

	if userIDs, exists := m.Users.channels[channelID]; exists {
		users := make([]*model.User, 0, len(userIDs))
		for uid := range userIDs {
			if user, ok := m.Users.users[uid]; ok {
				users = append(users, user)
			}
		}

		m.Users.mu.RUnlock()

		return users, nil
	}

	m.Users.mu.RUnlock()

	const batchSize = mattermostPerPageMax

	allUsers := make([]*model.User, 0, batchSize)

	channelName := m.GetCachedChannelName(channelID)
	idx := 0
	retryCount := 0

	for {
		if m.IsAborted(ctx) {
			return nil, errors.New("login aborted")
		}

		query := "/users?in_channel=" + channelID + "&page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
		m.apiLogger.Warnf("GetChannelUsers: DoAPIGet: %s: query %s #%d", channelName, query, retryCount)

		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}

			shouldRetry, hErr := m.HandleRetry(ctx, "GetUsersInChannel", err, retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			return nil, err
		}

		retryCount = 0

		count, jsonErr := m.updateUsersFromJSON(resp.Body, func(u *model.User) {
			allUsers = append(allUsers, u)
		})
		resp.Body.Close()

		if jsonErr != nil {
			return nil, jsonErr
		}

		if count < batchSize {
			break
		}

		idx++
	}

	m.Users.mu.Lock()
	if m.Users.channels[channelID] == nil {
		m.Users.channels[channelID] = make(map[string]struct{}, len(allUsers))
	}

	for _, u := range allUsers {
		m.Users.channels[channelID][u.Id] = struct{}{}
	}

	m.Users.lastUpdated.Store(time.Now().Unix())
	m.Users.mu.Unlock()

	return allUsers, nil
}

func (m *Client) GetLastViewedAt(ctx context.Context, channelID string) int64 {
	m.Users.mu.RLock()
	// Check if the channel is deleted
	ch, channelExists := m.Users.channelData[channelID]
	if channelExists && ch.DeleteAt > 0 {
		m.Users.mu.RUnlock()
		return 0
	}

	// Check our view cache
	if viewedAt, ok := m.Users.channelLastViewedAt[channelID]; ok {
		m.Users.mu.RUnlock()
		return viewedAt
	}

	// Get CreateAt while we still have the read lock, just in case we need it
	var createAt int64
	if channelExists {
		createAt = ch.CreateAt
	}

	m.Users.mu.RUnlock()

	m.RLock()
	userID := m.User.Id
	m.RUnlock()

	channelName := m.GetCachedChannelName(channelID)
	userName := m.GetCachedUserName(userID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("GetLastViewedAt: Channel %s (%s), User: %s (%s) #%d", channelName, channelID, userName, userID, retryCount)

		res, resp, err := m.Client.GetChannelMember(ctx, channelID, userID, "")
		if err == nil {
			viewedAt := res.LastViewedAt
			if viewedAt == 0 && createAt > 0 {
				viewedAt = createAt
			}

			m.Users.mu.Lock()
			m.Users.channelLastViewedAt[channelID] = viewedAt
			m.Users.mu.Unlock()

			return viewedAt
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetChannelMember", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetChannelMember failed for %s (%s): %v", channelName, channelID, err)

		// Fallback on error: Return CreateAt if we know it, otherwise current time
		if createAt > 0 {
			return createAt
		}

		return model.GetMillis()
	}
}

// GetMoreChannels returns existing channels where we're not a member of.
func (m *Client) GetMoreChannels() []*model.Channel {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	capSize := max(0, len(m.Users.channelData)-len(m.Users.joinedChannels))
	channels := make([]*model.Channel, 0, capSize)

	for id, ch := range m.Users.channelData {
		if _, joined := m.Users.joinedChannels[id]; !joined {
			channels = append(channels, ch)
		}
	}

	return channels
}

// GetTeamFromChannel returns teamId belonging to channel (DM channels have no teamId).
func (m *Client) GetTeamFromChannel(ctx context.Context, channelID string) string {
	if ch := m.GetChannel(ctx, channelID); ch != nil {
		if ch.Type == model.ChannelTypeGroup {
			return "G"
		}

		return ch.TeamId
	}

	return ""
}

// IsChannelMember returns true if the user is a member of the given channel ID.
func (m *Client) IsChannelMember(channelID string) bool {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	if m.Users.joinedChannels == nil {
		return false
	}

	_, exists := m.Users.joinedChannels[channelID]

	return exists
}

// IsDMChannelName reports whether the given channel name matches the Mattermost
// direct message naming convention.
func (m *Client) IsDMChannelName(channelName string) bool {
	_, _, ok := m.GetDMUserIDs(channelName)

	return ok
}

func (m *Client) JoinChannel(ctx context.Context, channelID string) error {
	m.Users.mu.RLock()
	_, joined := m.Users.joinedChannels[channelID]
	m.Users.mu.RUnlock()

	channelName := m.GetCachedChannelName(channelID)
	if joined {
		m.logger.Debugf("Not joining %s (%s) already joined.", channelName, channelID)
		return nil
	}

	m.logger.Debugf("Joining %s (%s)", channelName, channelID)

	_, err := m.AddChannelMember(ctx, channelID, m.User.Id)
	if err == nil {
		m.Users.mu.Lock()
		m.Users.markJoinedLocked(channelID)
		m.Users.mu.Unlock()

		return nil
	}

	return err
}

func (m *Client) AddChannelMember(ctx context.Context, channelID, userID string) (*model.ChannelMember, error) {
	channelName := m.GetCachedChannelName(channelID)
	userName := m.GetCachedUserName(userID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("AddChannelMember: Channel: %s (%s), User: %s (%s) #%d", channelName, channelID, userName, userID, retryCount)

		member, resp, err := m.Client.AddChannelMember(ctx, channelID, userID)
		if err == nil {
			return member, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "AddChannelMember", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("AddChannelMember failed for channel %s (%s) (user %s (%s)): %v", channelName, channelID, userName, userID, err)

		return nil, err
	}
}

func (m *Client) RemoveUserFromChannel(ctx context.Context, channelID, userID string) error {
	channelName := m.GetCachedChannelName(channelID)
	userName := m.GetCachedUserName(userID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("RemoveUserFromChannel: Channel: %s (%s), User: %s (%s) #%d", channelName, channelID, userName, userID, retryCount)

		resp, err := m.Client.RemoveUserFromChannel(ctx, channelID, userID)
		if err == nil {
			// If we are removing ourselves, clean up our joinedChannels cache
			m.RLock()
			isCurrentUser := m.User != nil && userID == m.User.Id
			m.RUnlock()

			if isCurrentUser {
				m.Users.mu.Lock()
				m.Users.markLeftLocked(channelID)
				m.Users.mu.Unlock()
			}

			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "RemoveUserFromChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("RemoveUserFromChannel failed for channel %s (%s) (user %s (%s)): %v", channelName, channelID, userName, userID, err)

		return err
	}
}

func (m *Client) PatchChannel(ctx context.Context, channelID string, patch *model.ChannelPatch) (*model.Channel, error) {
	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("PatchChannel: Channel: %s (%s) #%d", channelName, channelID, retryCount)

		channel, resp, err := m.Client.PatchChannel(ctx, channelID, patch)
		if err == nil {
			// Update local cache so GetChannel immediately reflects the updated topic/header
			m.Users.mu.Lock()
			m.Users.channelData[channelID] = channel
			m.Users.mu.Unlock()

			return channel, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "PatchChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("PatchChannel failed for %s (%s): %v", channelName, channelID, err)

		return nil, err
	}
}

//nolint:funlen,gocognit,gocyclo
func (m *Client) UpdateChannelsTeam(ctx context.Context, teamID string) error {
	var joinedSummaries []ChannelSummary

	// WebSocket events applied while the request is in flight are newer than its response.
	deltas, stopWatching := m.Users.watchMembership()
	defer stopWatching()

	retryCount := 0

	for {
		if m.IsAborted(ctx) {
			return errors.New("login aborted")
		}

		joinedSummaries = joinedSummaries[:0]

		query := "/users/" + m.User.Id + "/teams/" + teamID + "/channels"
		m.apiLogger.Warnf("UpdateChannelsTeam: DoAPIGet: query %s #%d", query, retryCount)

		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}

			shouldRetry, hErr := m.HandleRetry(ctx, "GetChannelsForTeamForUser", err, retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			return err
		}

		_, jsonErr := decodeJSONArray(resp.Body, func(c *ChannelSummary) {
			joinedSummaries = append(joinedSummaries, *c)
		})
		resp.Body.Close()

		if jsonErr != nil {
			return jsonErr
		}

		break
	}

	m.Users.mu.Lock()

	if m.Users.channelData == nil {
		m.Users.channelData = make(map[string]*model.Channel, len(joinedSummaries))
		m.Users.joinedChannels = make(map[string]struct{}, len(joinedSummaries))
	} else {
		for chanID := range m.Users.joinedChannels {
			if joined, changed := deltas[chanID]; changed && joined {
				// Joined by an event during the request, the response can't know about it.
				continue
			}

			if ch, ok := m.Users.channelData[chanID]; ok && ch.TeamId == teamID {
				delete(m.Users.joinedChannels, chanID)
			}
		}
	}

	for i := range joinedSummaries {
		id := m.Users.cacheChannelLocked(teamID, &joinedSummaries[i]).Id

		if joined, changed := deltas[id]; changed && !joined {
			// Left by an event during the request, don't resurrect it from the stale response.
			continue
		}

		m.Users.joinedChannels[id] = struct{}{}
	}

	_, hasCache := m.Users.publicChannelsSynced[teamID]
	m.Users.mu.Unlock()

	// Skip the heavy public channel crawl if we have a cache and aren't forcing a sync.
	// We rely on WebSocket events to keep public channels updated organically.
	if hasCache && !m.ForceSyncOnReconnect {
		m.logger.Debug("UpdateChannelsTeam: skipping public channel fetch (using cache)")
	} else {
		m.logger.Trace("UpdateChannelsTeam: fetching public channels (cache empty or ForceSyncOnReconnect enabled)")

		err := m.updatePublicChannels(ctx, teamID)
		if err != nil {
			return err
		}
	}

	m.Users.lastUpdated.Store(time.Now().Unix())

	m.Lock()
	if team, exists := m.OtherTeams[teamID]; exists {
		team.LastChannelSync = time.Now()
	}
	m.Unlock()

	return nil
}

// updatePublicChannels caches the public channels of the team one page at a time. Every page is decoded into the same
// slice, and cached before the next page is fetched, so only a page of channels is held on to besides the cache itself.
// The team is only marked as synced once its last page is cached, so a crawl that fails part way is retried.
//
//nolint:funlen
func (m *Client) updatePublicChannels(ctx context.Context, teamID string) error {
	const batchSize = mattermostPerPageMax

	page := make([]ChannelSummary, 0, batchSize)

	idx := 0
	retryCount := 0

	for {
		if m.IsAborted(ctx) {
			return errors.New("login aborted")
		}

		query := "/teams/" + teamID + "/channels?page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
		m.apiLogger.Warnf("UpdateChannelsTeam: DoAPIGet: query %s #%d", query, retryCount)

		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}

			shouldRetry, hErr := m.HandleRetry(ctx, "GetPublicChannelsForTeam", err, retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++

				continue
			}

			return err
		}

		retryCount = 0

		page = page[:0]

		count, jsonErr := decodeJSONArray(resp.Body, func(c *ChannelSummary) {
			page = append(page, *c)
		})
		resp.Body.Close()

		if jsonErr != nil {
			return jsonErr
		}

		m.Users.mu.Lock()
		for i := range page {
			m.Users.cacheChannelLocked(teamID, &page[i])
		}

		if count < batchSize {
			if m.Users.publicChannelsSynced == nil {
				m.Users.publicChannelsSynced = make(map[string]struct{})
			}

			m.Users.publicChannelsSynced[teamID] = struct{}{}
		}
		m.Users.mu.Unlock()

		if count < batchSize {
			return nil
		}

		idx++
	}
}

// internChannelType returns the constant for the highly repetitive channel types, instead of a copy of the string.
func internChannelType(t string) model.ChannelType {
	switch t {
	case "O":
		return model.ChannelTypeOpen
	case "P":
		return model.ChannelTypePrivate
	case "D":
		return model.ChannelTypeDirect
	}

	return model.ChannelType(t)
}

// cacheChannelLocked adds the channel to the channels of the team in the cache, or brings the cached channel up to date
// if there already is one, and returns it. The users cache has to be locked.
func (c *UsersCache) cacheChannelLocked(teamID string, ch *ChannelSummary) *model.Channel {
	cached, exists := c.channelData[ch.Id]
	if !exists {
		cached = &model.Channel{
			Id:          ch.Id,
			UpdateAt:    ch.UpdateAt,
			DeleteAt:    ch.DeleteAt,
			TeamId:      teamID,
			Type:        internChannelType(ch.Type),
			DisplayName: ch.DisplayName,
			Name:        ch.Name,
			Header:      ch.Header,
			Purpose:     ch.Purpose,
			LastPostAt:  ch.LastPostAt,
			CreatorId:   c.userIDKey(ch.CreatorId),
		}
		c.channelData[cached.Id] = cached

		return cached
	}

	if cached.DisplayName != ch.DisplayName {
		cached.DisplayName = ch.DisplayName
	}

	if cached.Name != ch.Name {
		cached.Name = ch.Name
	}

	if cached.Header != ch.Header {
		cached.Header = ch.Header
	}

	if cached.Purpose != ch.Purpose {
		cached.Purpose = ch.Purpose
	}

	if cached.UpdateAt < ch.UpdateAt {
		cached.UpdateAt = ch.UpdateAt
	}

	if cached.DeleteAt != ch.DeleteAt {
		cached.DeleteAt = ch.DeleteAt
	}

	if cached.LastPostAt < ch.LastPostAt {
		cached.LastPostAt = ch.LastPostAt
	}

	if newType := internChannelType(ch.Type); cached.Type != newType {
		cached.Type = newType
	}

	return cached
}

func (m *Client) UpdateChannels(ctx context.Context) error {
	if m.Team == nil {
		m.logger.Errorf("cannot update channels: primary team is nil")
		return errors.New("cannot update channels: primary team is nil")
	}

	err := m.UpdateChannelsTeam(ctx, m.Team.ID)
	if err != nil {
		return err
	}

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)

	for _, t := range m.OtherTeams {
		// We've already populated users/channels for team in the above.
		if t.ID == m.Team.ID {
			continue
		}

		wg.Add(1)

		go func(teamID string) {
			defer wg.Done()

			err := m.UpdateChannelsTeam(ctx, teamID)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		}(t.ID)
	}

	wg.Wait()

	return firstErr
}

func (m *Client) UpdateChannelHeader(ctx context.Context, channelID string, header string) {
	channel := &model.Channel{Id: channelID, Header: header}
	m.logger.Debugf("updating channelheader %#v, %#v", channelID, header)

	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("UpdateChannelHeader: Channel: %s (%s) #%d", channelName, channelID, retryCount)

		_, resp, err := m.Client.UpdateChannel(ctx, channel)
		if err == nil {
			return
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "UpdateChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("UpdateChannelHeader failed for %s (%s): %v", channelName, channelID, err)

		return
	}
}

func (m *Client) UpdateChannelUsersCache(channelID string, user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.UpdateUser(user, true)

	if channelID != "" {
		if m.Users.channels[channelID] != nil {
			m.Users.channels[channelID][user.Id] = struct{}{}
		}
	}

	m.Users.lastUpdated.Store(time.Now().Unix())
}

func (m *Client) UpdateChannelUsersCacheRemove(channelID string, userID string) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	if m.Users.channels != nil && m.Users.channels[channelID] != nil {
		delete(m.Users.channels[channelID], userID)
	}
}

func (m *Client) UpdateLastViewed(ctx context.Context, channelID string) error {
	m.Users.mu.RLock()
	lastViewed := m.Users.channelLastViewedAt[channelID]
	m.Users.mu.RUnlock()

	// Skip API call if this specific channel was marked viewed within the last 3 seconds
	if lastViewed > 0 && time.Since(time.UnixMilli(lastViewed)) < 3*time.Second {
		return nil
	}

	channelName := m.GetCachedChannelName(channelID)
	userName := m.GetCachedUserName(m.User.Id)

	m.logger.Debugf("posting lastview %s %#v", channelName, channelID)

	view := &model.ChannelView{ChannelId: channelID}

	retryCount := 0
	for {
		m.apiLogger.Infof("UpdateLastViewed: Channel: %s (%s), User: %s (%s) #%d", channelName, channelID, userName, m.User.Id, retryCount)

		_, resp, err := m.Client.ViewChannel(ctx, m.User.Id, view)
		if err == nil {
			m.Users.mu.Lock()
			m.Users.channelLastViewedAt[channelID] = time.Now().UnixMilli()
			m.Users.mu.Unlock()

			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "ViewChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("ChannelView update for %s (%s) failed: %v", channelName, channelID, err)

		return err
	}
}

// markJoinedLocked records that the user joined the channel. The users cache has to be locked.
func (c *UsersCache) markJoinedLocked(channelID string) {
	c.joinedChannels[channelID] = struct{}{}

	for _, deltas := range c.membershipWatchers {
		deltas[channelID] = true
	}
}

// markLeftLocked records that the user left the channel. The users cache has to be locked.
func (c *UsersCache) markLeftLocked(channelID string) {
	delete(c.joinedChannels, channelID)

	for _, deltas := range c.membershipWatchers {
		deltas[channelID] = false
	}
}

// watchMembership starts collecting the membership changes made from now on, including those of channels the cache
// doesn't know about yet. The returned map is only safe to read with the users cache locked, and the stop function
// has to be called to release it.
func (c *UsersCache) watchMembership() (map[string]bool, func()) {
	deltas := make(map[string]bool)

	c.mu.Lock()
	if c.membershipWatchers == nil {
		c.membershipWatchers = make(map[int]map[string]bool)
	}

	id := c.nextWatcherID
	c.nextWatcherID++
	c.membershipWatchers[id] = deltas
	c.mu.Unlock()

	return deltas, func() {
		c.mu.Lock()
		delete(c.membershipWatchers, id)
		c.mu.Unlock()
	}
}
