package matterclient

import (
	"github.com/mattermost/mattermost-server/v6/model"
)

func (m *Client) GetNickName(userID string) string {
	if user := m.GetUser(userID); user != nil {
		return user.Nickname
	}

	return ""
}

func (m *Client) GetStatus(userID string) string {
	m.UserStatusMutex.RLock()
	status, ok := m.UserStatuses[userID]
	m.UserStatusMutex.RUnlock()
	if ok {
		return status
	}

	res, _, err := m.Client.GetUserStatus(userID, "")
	if err != nil {
		return "offline"
	}

	return m.SetUserStatus(userID, res.Status)
}

func (m *Client) GetStatuses() map[string]string {
	statuses := make(map[string]string, len(m.Users))
	var missingIDs []string

	m.UserStatusMutex.RLock()
	for id := range m.Users {
		if status, ok := m.UserStatuses[id]; ok {
			statuses[id] = status
		} else {
			missingIDs = append(missingIDs, id)
		}
	}
	m.UserStatusMutex.RUnlock()

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
		res, _, err := m.Client.GetUsersStatusesByIds(batch)
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

func (m *Client) GetUser(userID string) *model.User {
	m.Lock()
	defer m.Unlock()

	_, ok := m.Users[userID]
	if !ok {
		res, _, err := m.Client.GetUser(userID, "")
		if err != nil {
			return nil
		}

		m.Users[userID] = res
	}

	return m.Users[userID]
}

func (m *Client) GetUserName(userID string) string {
	if user := m.GetUser(userID); user != nil {
		return user.Username
	}

	return ""
}

func (m *Client) GetUsers() map[string]*model.User {
	users := make(map[string]*model.User)

	m.RLock()
	defer m.RUnlock()

	for k, v := range m.Users {
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

	m.UserStatusMutex.Lock()
	if m.UserStatuses == nil {
		m.UserStatuses = make(map[string]string)
	}
	m.UserStatuses[userID] = statusStr
	m.UserStatusMutex.Unlock()

	return statusStr
}

func (m *Client) UpdateUsers() error {
	var (
		resp *model.Response
		err  error
	)

	const batchSize = 200

	idx := 0
	mmusers := make([]*model.User, 0, batchSize)
	for {
		mmusers, resp, err = m.Client.GetUsers(idx, batchSize, "")
		if err != nil {
			if rlErr := m.HandleRatelimit("GetUsers", resp); rlErr != nil {
				return rlErr
			}
			continue
		}

		m.Lock()
		for _, user := range mmusers {
			m.Users[user.Id] = user
		}
		m.Unlock()

		if len(mmusers) < batchSize {
			break
		}

		idx++
	}

	return nil
}

func (m *Client) UpdateUserNick(nick string) error {
	user := m.User
	user.Nickname = nick

	_, _, err := m.Client.UpdateUser(user)
	if err != nil {
		return err
	}

	return nil
}

func (m *Client) UsernamesInChannel(channelID string) []string {
	res, _, err := m.Client.GetChannelMembers(channelID, 0, 50000, "")
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
	_, _, err := m.Client.UpdateUserStatus(userID, &model.Status{Status: status})
	if err != nil {
		return err
	}

	return nil
}

func (m *Client) UpdateUser(userID string) {
	m.Lock()
	defer m.Unlock()

	res, _, err := m.Client.GetUser(userID, "")
	if err != nil {
		return
	}

	m.Users[userID] = res
}
