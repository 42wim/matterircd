package matterclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func (m *Client) GetNickName(userID string) string {
	if user := m.GetUser(context.TODO(), userID); user != nil {
		return user.Nickname
	}

	return ""
}

func (m *Client) GetStatus(userID string) string {
	m.Users.mu.RLock()
	status, ok := m.Users.statuses[userID]
	m.Users.mu.RUnlock()

	if !ok {
		m.apiLogger.Warnf("GetStatus: GetUserStatus: UserID: %s", userID)
		res, _, err := m.Client.GetUserStatus(context.TODO(), userID, "")
		if err != nil {
			status = "offline"
		} else {
			status = m.SetUserStatus(userID, res.Status)
		}
	}

	m.Users.mu.RLock()
	customStatus, tracked := m.Users.customStatuses[userID]
	m.Users.mu.RUnlock()

	if !tracked {
		user := m.GetUser(context.TODO(), userID)
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
	}

	if customStatus != "" {
		if status != "online" && status != "" {
			return status + ": " + customStatus
		}
		return customStatus
	}

	return status
}

func (m *Client) GetStatuses() map[string]string {
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
		m.apiLogger.Warnf("GetStatuses: GetUsersStatusesByIds: Batch: %d #%d", len(batch), i)
		res, _, err := m.Client.GetUsersStatusesByIds(context.TODO(), batch)
		if err != nil {
			continue
		}

		for _, st := range res {
			cleanID := strings.Clone(st.UserId)
			statuses[cleanID] = m.SetUserStatus(cleanID, st.Status)
		}
	}

	for _, id := range missingIDs {
		if _, ok := statuses[id]; !ok {
			statuses[id] = "offline"
		}
	}

	return statuses
}

func (m *Client) GetTeamID() string {
	return m.Team.ID
}

// GetTeamName returns the name of the specified teamId
func (m *Client) GetTeamName(teamID string) string {
	m.RLock()
	defer m.RUnlock()

	for _, t := range m.OtherTeams {
		if t.ID == teamID {
			return t.Team.Name
		}
	}

	return ""
}

func (m *Client) GetUser(ctx context.Context, userID string) *model.User {
	m.Users.mu.RLock()
	user, exists := m.Users.users[userID]
	m.Users.mu.RUnlock()

	if exists {
		return user
	}

	m.apiLogger.Warnf("GetUser: UserID: %s", userID)
	res, _, err := m.Client.GetUser(ctx, userID, "")
	if err != nil {
		m.logger.Debugf("GetUser failed to fetch missing user %s: %s", userID, err)
		return nil
	}

	res.Id = strings.Clone(res.Id)
	res.Username = strings.Clone(res.Username)
	res.FirstName = strings.Clone(res.FirstName)
	res.LastName = strings.Clone(res.LastName)
	res.Nickname = strings.Clone(res.Nickname)
	res.Roles = strings.Clone(res.Roles)

	m.UpdateUser(res)

	return res
}

func (c *UsersCache) GetUserCustomStatus(userID string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.customStatuses[userID]
}

func (m *Client) GetUserName(userID string) string {
	if user := m.GetUser(context.TODO(), userID); user != nil {
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

	if err := json.NewDecoder(strings.NewReader(rawJSON)).Decode(&status); err != nil {
		c.customStatuses[userID] = ""
		return
	}

	if status.Text == "" {
		c.customStatuses[userID] = ""
		return
	}

	if status.Emoji != "" {
		c.customStatuses[userID] = ":" + status.Emoji + ": " + status.Text
	} else {
		c.customStatuses[userID] = status.Text
	}
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

func (m *Client) UpdateUsers() error {
	const batchSize = 200

	idx := 0
	retryCount := 0
	for {
		if m.IsAborted() {
			return errors.New("login aborted")
		}

		query := "/users?page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
		m.apiLogger.Warnf("UpdateUsers: DoAPIGet: query %s #%d", query, retryCount)
		resp, err := m.Client.DoAPIGet(context.TODO(), query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}
			shouldRetry, hErr := m.HandleRetry("GetUsers", retryCount, 10, mResp)
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

func (m *Client) UpdateUserNick(nick string) error {
	m.RLock()
	if m.User == nil {
		m.RUnlock()
		return fmt.Errorf("current user profile is not loaded")
	}
	userClone := *m.User
	m.RUnlock()
	userClone.Nickname = nick

	m.apiLogger.Warnf("UpdateUserNick: nick: %s", nick)
	updatedUser, _, err := m.Client.UpdateUser(context.TODO(), &userClone)
	if err != nil {
		return err
	}

	m.Lock()
	m.User = updatedUser
	m.Unlock()
	m.UpdateUser(updatedUser)

	return nil
}

func (m *Client) UsernamesInChannel(channelID string) []string {
	const batchSize = 200

	allusers := m.GetUsers()
	result := make([]string, 0, batchSize)

	idx := 0
	retryCount := 0
	for {
		m.apiLogger.Warnf("UsernamesInChannel: GetChannelMembers: ChannelID: %s, Page: %d, PerPage: %d #%d", channelID, idx, batchSize, retryCount)
		res, resp, err := m.Client.GetChannelMembers(context.TODO(), channelID, idx, batchSize, "")
		if err != nil {
			shouldRetry, hErr := m.HandleRetry("UsernamesInChannel", retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			m.logger.Errorf("UsernamesInChannel(%s) failed: %s", channelID, err)
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

func (m *Client) UpdateStatus(userID string, status string) error {
	m.apiLogger.Warnf("UpdateStatus: UserID: %s, Status: %s", userID, status)
	_, _, err := m.Client.UpdateUserStatus(context.TODO(), userID, &model.Status{Status: status})
	if err != nil {
		return err
	}

	m.SetUserStatus(userID, status)

	return nil
}

func (m *Client) UpdateUser(user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.Users.users[user.Id] = user
	m.Users.lastUpdated.Store(time.Now().Unix())
}
