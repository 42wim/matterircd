package matterclient

import (
	"errors"
	"time"

	"github.com/mattermost/mattermost-server/v5/model"
)

func (m *Client) GetNickName(userID string) string {
	user := m.GetUser(userID)
	if user != nil {
		return user.Nickname
	}

	return ""
}

func (m *Client) GetStatus(userID string) string {
	res, resp := m.Client.GetUserStatus(userID, "")
	if resp.Error != nil {
		return ""
	}

	if res.Status == model.STATUS_AWAY {
		return "away"
	}

	if res.Status == model.STATUS_ONLINE {
		return "online"
	}

	return "offline"
}

func (m *Client) GetStatuses() map[string]string {
	var ids []string
	statuses := make(map[string]string)

	for id := range m.Users {
		ids = append(ids, id)
	}

	res, resp := m.Client.GetUsersStatusesByIds(ids)
	if resp.Error != nil {
		return statuses
	}

	for _, status := range res {
		statuses[status.UserId] = "offline"
		if status.Status == model.STATUS_AWAY {
			statuses[status.UserId] = "away"
		}

		if status.Status == model.STATUS_ONLINE {
			statuses[status.UserId] = "online"
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
		res, resp := m.Client.GetUser(userID, "")
		if resp.Error != nil {
			return nil
		}

		m.Users[userID] = res
	}

	return m.Users[userID]
}

func (m *Client) GetUserName(userID string) string {
	user := m.GetUser(userID)
	if user != nil {
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

func (m *Client) UpdateUsers() error {
	idx := 0
	max := 200

	mmusers, resp := m.Client.GetUsers(idx, max, "")
	if resp.Error != nil {
		return errors.New(resp.Error.DetailedError)
	}

	for len(mmusers) > 0 {
		m.Lock()

		for _, user := range mmusers {
			m.Users[user.Id] = user
		}

		m.Unlock()

		mmusers, resp = m.Client.GetUsers(idx, max, "")

		time.Sleep(time.Millisecond * 300)

		if resp.Error != nil {
			return errors.New(resp.Error.DetailedError)
		}

		idx++
	}

	return nil
}

func (m *Client) UpdateUserNick(nick string) error {
	user := m.User
	user.Nickname = nick

	_, resp := m.Client.UpdateUser(user)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Client) UsernamesInChannel(channelID string) []string {
	res, resp := m.Client.GetChannelMembers(channelID, 0, 50000, "")
	if resp.Error != nil {
		m.logger.Errorf("UsernamesInChannel(%s) failed: %s", channelID, resp.Error)

		return []string{}
	}

	allusers := m.GetUsers()
	result := []string{}

	for _, member := range *res {
		result = append(result, allusers[member.UserId].Nickname)
	}

	return result
}

func (m *Client) UpdateStatus(userID string, status string) error {
	_, resp := m.Client.UpdateUserStatus(userID, &model.Status{Status: status})
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Client) UpdateUser(userID string) {
	m.Lock()
	defer m.Unlock()

	res, resp := m.Client.GetUser(userID, "")
	if resp.Error != nil {
		return
	}

	m.Users[userID] = res
}
