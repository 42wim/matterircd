package matterclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

// GetCachedUser returns the user from local cache without triggering a REST API lookup.
func (m *Client) GetCachedUser(userID string) *model.User {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	return m.Users.users[userID]
}

// GetCachedUserName returns the cached username if available, or empty string.
func (m *Client) GetCachedUserName(userID string) string {
	if u := m.GetCachedUser(userID); u != nil {
		return u.Username
	}

	return ""
}

func (m *Client) GetNickName(ctx context.Context, userID string) string {
	if user := m.GetUser(ctx, userID); user != nil {
		return user.Nickname
	}

	return ""
}

func (m *Client) GetStatus(ctx context.Context, userID string) string {
	const activeThreshold = 20 * time.Minute

	m.Users.mu.RLock()
	lastActive := m.Users.lastUserActivity[userID]
	status, ok := m.Users.statuses[userID]
	m.Users.mu.RUnlock()

	if lastActive > 0 && time.Since(time.Unix(lastActive, 0)) < activeThreshold {
		// Recent activity overrides cached/offline status as "online"
		return "online"
	}
	userName := m.GetCachedUserName(userID)

	if !ok {
		retryCount := 0
		for {
			m.apiLogger.Warnf("GetStatus: GetUserStatus: User: %s (%s) #%d", userName, userID, retryCount)

			res, resp, err := m.Client.GetUserStatus(ctx, userID, "")
			if err == nil {
				status = m.SetUserStatus(userID, res.Status)

				break
			}

			shouldRetry, hErr := m.HandleRetry(ctx, "GetUserStatus", err, retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++

				continue
			}

			// Fallback to existing cached status if HTTP API fails completely
			m.Users.mu.RLock()
			status = m.Users.statuses[userID]
			m.Users.mu.RUnlock()

			// If it completely fails, we assume offline and break the loop
			if status == "" {
				status = model.StatusOffline
			}

			break
		}
	}

	if status == model.StatusOnline {
		return status
	}

	m.Users.mu.RLock()
	customStatus, tracked := m.Users.customStatuses[userID]
	m.Users.mu.RUnlock()

	if !tracked {
		user := m.GetUser(ctx, userID)

		var rawJSON string
		if user != nil && user.Props != nil {
			if val, propOk := user.Props["customStatus"]; propOk {
				rawJSON = val
			}
		}

		// Parse & store in cache (permanently marks user as tracked)
		m.Users.SetUserCustomStatus(userID, rawJSON)

		m.Users.mu.RLock()
		customStatus = m.Users.customStatuses[userID]
		m.Users.mu.RUnlock()
	} else if customStatus != "" {
		// Re-validate against user props in cache to catch time-expired statuses
		if user := m.GetUser(ctx, userID); user != nil && user.Props != nil {
			if rawJSON, ok := user.Props["customStatus"]; ok {
				m.Users.SetUserCustomStatus(userID, rawJSON)

				m.Users.mu.RLock()
				customStatus = m.Users.customStatuses[userID]
				m.Users.mu.RUnlock()
			}
		}
	}

	if customStatus != "" {
		if status != model.StatusOnline && status != "" {
			return status + ": " + customStatus
		}

		return customStatus
	}

	return status
}

func (m *Client) GetStatuses(ctx context.Context) map[string]string {
	statuses := make(map[string]string, len(m.Users.users))
	var missingIDs []string

	m.Users.mu.RLock()
	for id := range m.Users.users {
		if status, ok := m.Users.statuses[id]; ok {
			statuses[id] = status
		} else {
			missingIDs = append(missingIDs, id)
		}
	}
	m.Users.mu.RUnlock()

	if len(missingIDs) == 0 {
		return statuses
	}

	const batchSize = 5000

	for i := 0; i < len(missingIDs); i += batchSize {
		end := i + batchSize
		if end > len(missingIDs) {
			end = len(missingIDs)
		}
		batch := missingIDs[i:end]

		retryCount := 0
		for {
			m.apiLogger.Warnf("GetStatuses: GetUsersStatusesByIds: Batch: %d #%d", len(batch), retryCount)

			res, resp, err := m.Client.GetUsersStatusesByIds(ctx, batch)
			if err == nil {
				for _, st := range res {
					cleanID := strings.Clone(st.UserId)
					statuses[cleanID] = m.SetUserStatus(cleanID, st.Status)
				}

				break // Break the retry loop on success to move to the next batch
			}

			shouldRetry, hErr := m.HandleRetry(ctx, "GetUsersStatusesByIds", err, retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++

				continue
			}

			// If it completely fails after retries, break the loop and continue to the next batch
			m.logger.Errorf("GetStatuses failed for batch starting at index %d: %v", i, err)

			break
		}
	}

	for _, id := range missingIDs {
		if _, ok := statuses[id]; !ok {
			statuses[id] = "offline"
		}
	}

	return statuses
}

func (m *Client) GetTeamByName(ctx context.Context, name string) (*model.Team, error) {
	retryCount := 0
	for {
		m.apiLogger.Warnf("GetTeamByName: TeamName: %s #%d", name, retryCount)

		team, resp, err := m.Client.GetTeamByName(ctx, name, "")
		if err == nil {
			return team, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetTeamByName", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("GetTeamByName failed for %s: %v", name, err)

		return nil, err
	}
}

func (m *Client) GetTeamID() string {
	return m.Team.ID
}

// GetTeamName returns the name of the specified teamId
func (m *Client) GetTeamName(ctx context.Context, teamID string) string {
	m.RLock()
	defer m.RUnlock()

	for _, t := range m.OtherTeams {
		if t.ID == teamID {
			return t.Team.Name
		}
	}

	return ""
}

// GetUser for backwards compatibility
func (m *Client) GetUser(ctx context.Context, userID string) *model.User {
	return m.GetUserByUserID(ctx, userID)
}

func (m *Client) GetUserByUserID(ctx context.Context, userID string) *model.User {
	m.Users.mu.RLock()
	user, exists := m.Users.users[userID]
	m.Users.mu.RUnlock()

	if exists {
		return user
	}

	var mmuser *model.User

	retryCount := 0
	for {
		m.apiLogger.Warnf("GetUser: UserID: %s #%d", userID, retryCount)

		res, resp, err := m.Client.GetUser(ctx, userID, "")
		if err == nil {
			mmuser = res
			break
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetUser", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Debugf("GetUser failed to fetch missing user %s: %s", userID, err)
		return nil
	}

	mmuser.Id = strings.Clone(mmuser.Id)
	mmuser.Username = strings.Clone(mmuser.Username)
	mmuser.FirstName = strings.Clone(mmuser.FirstName)
	mmuser.LastName = strings.Clone(mmuser.LastName)
	mmuser.Nickname = strings.Clone(mmuser.Nickname)
	mmuser.Roles = strings.Clone(mmuser.Roles)

	m.UpdateUser(mmuser)

	return mmuser
}

func (m *Client) GetUserByUsername(ctx context.Context, username string) *model.User {
	m.Users.mu.RLock()

	// Fast path: check if caller passed an already-cached User ID
	if u, ok := m.Users.users[username]; ok {
		m.Users.mu.RUnlock()
		return u
	}

	// Scan cached users by username
	for _, u := range m.Users.users {
		if u.Username == username {
			m.Users.mu.RUnlock()
			return u
		}
	}

	m.Users.mu.RUnlock()

	var mmuser *model.User

	// Query API by Username (handles genuine 26-character usernames)
	retryCount := 0
	for {
		m.apiLogger.Warnf("GetUserByUsername: User: %s #%d", username, retryCount)

		res, resp, err := m.Client.GetUserByUsername(ctx, username, "")
		if err == nil {
			mmuser = res
			break
		}

		// If user not found and the string looks like an ID, cascade to GetUser
		if resp != nil && resp.StatusCode == http.StatusNotFound && model.IsValidId(username) {
			m.logger.Debugf("GetUserByUsername: %s not found as username, attempting lookup by User ID", username)
			return m.GetUser(ctx, username)
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetUserByUsername", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		// Fallback attempt by ID if error was not retryable
		if model.IsValidId(username) {
			return m.GetUser(ctx, username)
		}

		m.logger.Debugf("GetUserByUsername failed to fetch missing user %s: %v", username, err)

		return nil
	}

	mmuser.Id = strings.Clone(mmuser.Id)
	mmuser.Username = strings.Clone(mmuser.Username)
	mmuser.FirstName = strings.Clone(mmuser.FirstName)
	mmuser.LastName = strings.Clone(mmuser.LastName)
	mmuser.Nickname = strings.Clone(mmuser.Nickname)
	mmuser.Roles = strings.Clone(mmuser.Roles)

	m.UpdateUser(mmuser)

	return mmuser
}

func (m *Client) GetUserChannels(ctx context.Context, userID string) ([]*model.Channel, error) {
	if m.Team == nil || userID == "" {
		return nil, nil
	}

	userName := m.GetCachedUserName(userID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("GetUserChannels: GetChannelsForTeamForUser: User: %s (%s) #%d", userName, userID, retryCount)

		channels, resp, err := m.Client.GetChannelsForTeamForUser(ctx, m.Team.ID, userID, false, "")
		if err == nil {
			return channels, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetUserChannels", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Debugf("GetUserChannels failed for %s (%s): %s", userName, userID, err)

		return nil, err
	}
}

// GetUserCount returns the total number of cached team users.
func (m *Client) GetUserCount() int {
	if m == nil || m.Users == nil {
		return 0
	}

	m.Users.mu.RLock()
	n := len(m.Users.users)
	m.Users.mu.RUnlock()

	return n
}

func (c *UsersCache) GetUserCustomStatus(userID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.customStatuses[userID]
}

func (m *Client) GetUserName(ctx context.Context, userID string) string {
	if user := m.GetUser(ctx, userID); user != nil {
		return user.Username
	}

	return ""
}

func (m *Client) GetUsers() map[string]*model.User {
	users := make(map[string]*model.User, len(m.Users.users))

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	for k, v := range m.Users.users {
		users[k] = v
	}

	return users
}

func (c *UsersCache) RecordUserActivity(userID string) {
	if userID == "" {
		return
	}

	c.mu.Lock()
	if c.lastUserActivity == nil {
		c.lastUserActivity = make(map[string]int64, 1000)
	}

	c.lastUserActivity[userID] = time.Now().Unix()
	c.mu.Unlock()
}

func (m *Client) SearchUsers(ctx context.Context, search *model.UserSearch) ([]*model.User, error) {
	retryCount := 0
	for {
		m.apiLogger.Warnf("SearchUsers: Term: %s #%d", search.Term, retryCount)

		users, resp, err := m.Client.SearchUsers(ctx, search)
		if err == nil {
			return users, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "SearchUsers", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("SearchUsers failed for %s: %v", search.Term, err)

		return nil, err
	}
}

func (c *UsersCache) SetUserCustomStatus(userID string, rawJSON string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.customStatuses == nil {
		c.customStatuses = make(map[string]string)
	}

	if rawJSON == "" || rawJSON == "{}" || rawJSON == "null" {
		c.customStatuses[userID] = ""
		return
	}

	var status CustomStatus

	err := json.Unmarshal([]byte(rawJSON), &status)
	if err != nil {
		c.customStatuses[userID] = ""
		return
	}

	var formattedStatus string

	switch {
	case status.Emoji != "" && status.Text != "":
		formattedStatus = ":" + status.Emoji + ": " + status.Text
	case status.Emoji != "":
		formattedStatus = ":" + status.Emoji + ":"
	default:
		formattedStatus = status.Text
	}

	if formattedStatus == "" {
		c.customStatuses[userID] = ""
		return
	}

	if status.ExpiresAt != "" {
		expiry, parseErr := time.Parse(time.RFC3339, status.ExpiresAt)
		if parseErr == nil {
			now := time.Now().Local()
			expLocal := expiry.Local()

			if !expLocal.After(now) {
				c.customStatuses[userID] = ""

				return
			}

			timeStr := expLocal.Format("15:04")
			nowDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
			expDate := time.Date(expLocal.Year(), expLocal.Month(), expLocal.Day(), 0, 0, 0, 0, expLocal.Location())
			daysDiff := int(expDate.Sub(nowDate).Hours() / 24)

			var dateStr string

			switch daysDiff {
			case 0:
				dateStr = "Today at " + timeStr
			case 1:
				dateStr = "Tomorrow at " + timeStr
			default:
				dateStr = expLocal.Format("Mon, 02 Jan 15:04")
			}

			formattedStatus += " (Until " + dateStr + ")"
		}
	}

	c.customStatuses[userID] = formattedStatus
}

func (m *Client) SetUserStatus(userID string, rawStatus string) string {
	statusStr := "offline"
	switch rawStatus {
	case model.StatusOnline:
		statusStr = "online"
	case model.StatusAway:
		statusStr = "away"
	}

	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.Users.statuses[userID] = statusStr
	m.Users.lastUpdated.Store(time.Now().Unix())

	return statusStr
}

//nolint:funlen,gocognit,gocyclo
func (m *Client) UpdateUsers(ctx context.Context) error {
	const batchSize = mattermostPerPageMax

	idx := 0
	retryCount := 0
	for {
		if m.IsAborted(ctx) {
			return errors.New("login aborted")
		}

		query := "/users?page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
		m.apiLogger.Warnf("UpdateUsers: DoAPIGet: query %s #%d", query, retryCount)
		resp, err := m.Client.DoAPIGet(ctx, query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}
			shouldRetry, hErr := m.HandleRetry(ctx, "GetUsers", err, retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}
			m.logger.Errorf("UpdateUsers failed at batch %d: %v", idx, err)
			return err
		}
		retryCount = 0

		var list []UserSummary
		if jsonErr := json.NewDecoder(resp.Body).Decode(&list); jsonErr != nil {
			resp.Body.Close()
			return jsonErr
		}
		resp.Body.Close()

		m.Users.mu.Lock()
		for _, u := range list {
			roles := u.Roles
			if roles == "system_user" {
				roles = "system_user"
			} else if roles == "system_admin system_user" {
				roles = "system_admin system_user"
			}

			cachedUser, exists := m.Users.users[u.Id]
			if !exists { //nolint:nestif
				m.Users.users[u.Id] = &model.User{
					Id:        u.Id,
					Username:  u.Username,
					FirstName: u.FirstName,
					LastName:  u.LastName,
					Nickname:  u.Nickname,
					Roles:     roles,
					Props:     u.Props,
				}
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
		}
		m.Users.lastUpdated.Store(time.Now().Unix())
		m.Users.mu.Unlock()

		if len(list) < batchSize {
			break
		}

		idx++
	}

	return nil
}

func (m *Client) UpdateUserNick(ctx context.Context, nick string) error {
	m.RLock()
	if m.User == nil {
		m.RUnlock()

		return fmt.Errorf("current user profile is not loaded")
	}

	userClone := *m.User
	m.RUnlock()
	userClone.Nickname = nick

	retryCount := 0
	for {
		m.apiLogger.Warnf("UpdateUserNick: nick: %s #%d", nick, retryCount)

		updatedUser, resp, err := m.Client.UpdateUser(ctx, &userClone)
		if err == nil {
			m.Lock()
			m.User = updatedUser
			m.Unlock()
			m.UpdateUser(updatedUser)

			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "UpdateUser", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("UpdateUserNick failed for %s: %v", nick, err)

		return err
	}
}

func (m *Client) UsernamesInChannel(ctx context.Context, channelID string) []string {
	const batchSize = mattermostPerPageMax

	allusers := m.GetUsers()
	result := make([]string, 0, batchSize)
	channelName := m.GetCachedChannelName(channelID)

	idx := 0
	retryCount := 0
	for {
		m.apiLogger.Warnf("UsernamesInChannel: GetChannelMembers: Channel: %s (%s), Page: %d, PerPage: %d #%d", channelName, channelID, idx, batchSize, retryCount)
		res, resp, err := m.Client.GetChannelMembers(ctx, channelID, idx, batchSize, "")
		if err != nil {
			shouldRetry, hErr := m.HandleRetry(ctx, "UsernamesInChannel", err, retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			m.logger.Errorf("UsernamesInChannel %s (%s) failed: %s", channelName, channelID, err)
			return result
		}
		retryCount = 0

		for _, member := range res {
			if user, ok := allusers[member.UserId]; ok {
				result = append(result, user.Nickname)
			}
		}

		if len(res) < batchSize {
			break
		}

		idx++
	}

	return result
}

func (m *Client) UpdateStatus(ctx context.Context, userID string, status string) error {
	userName := m.GetCachedUserName(userID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("UpdateStatus: User: %s (%s), Status: %s #%d", userName, userID, status, retryCount)

		_, resp, err := m.Client.UpdateUserStatus(ctx, userID, &model.Status{
			UserId: userID,
			Status: status,
		})
		if err == nil {
			m.SetUserStatus(userID, status)

			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "UpdateUserStatus", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("UpdateStatus failed for %s (%s): %v", userName, userID, err)

		return err
	}
}

func (m *Client) UpdateTeamUsersCacheRemove(teamID, userID string) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	if userMap, exists := m.Users.teams[teamID]; exists {
		delete(userMap, userID)
	}
}

func (m *Client) UpdateUser(user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.Users.users[user.Id] = user
	m.Users.lastUpdated.Store(time.Now().Unix())
}
