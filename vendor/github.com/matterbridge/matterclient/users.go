package matterclient

import (
	"context"
	"fmt"
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
	if ok {
		return status
	}

	res, _, err := m.Client.GetUserStatus(context.TODO(), userID, "")
	if err != nil {
		return "offline"
	}

	return m.SetUserStatus(userID, res.Status)
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
		res, _, err := m.Client.GetUsersStatusesByIds(context.TODO(), batch)
		if err != nil {
			continue
		}

		for _, st := range res {
			statuses[st.UserId] = m.SetUserStatus(st.UserId, st.Status)
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

	res, _, err := m.Client.GetUser(ctx, userID, "")
	if err != nil {
		m.logger.Debugf("GetUser failed to fetch missing user %s: %s", userID, err)
		return nil
	}

	m.UpdateUser(res)

	return res
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
		mmusers, resp, err := m.Client.GetUsers(context.TODO(), idx, batchSize, "")
		if err != nil {
			shouldRetry, hErr := m.HandleRetry("GetUsers", retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			m.logger.Errorf("UpdateUsers failed at batch %d: %v", idx, err)
			return err
		}
		retryCount = 0

		m.Users.mu.Lock()
		for _, user := range mmusers {
			m.Users.users[user.Id] = user
		}
		m.Users.lastUpdated.Store(time.Now().Unix())
		m.Users.mu.Unlock()

		if len(mmusers) < batchSize {
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
	res, _, err := m.Client.GetChannelMembers(context.TODO(), channelID, 0, 50000, "")
	if err != nil {
		m.logger.Errorf("UsernamesInChannel(%s) failed: %s", channelID, err)

		return []string{}
	}

	allusers := m.GetUsers()
	result := []string{}

	for _, member := range res {
		result = append(result, allusers[member.UserId].Nickname)
	}

	return result
}

func (m *Client) UpdateStatus(userID string, status string) error {
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
