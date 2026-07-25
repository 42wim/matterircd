package matterclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mattermost/mattermost-server/v6/model"
)

func (m *Client) GetChannel(channelID string) *model.Channel {
	m.Users.mu.RLock()
	ch, exists := m.Users.channelData[channelID]
	m.Users.mu.RUnlock()

	if exists {
		return ch
	}

	mmchannel, _, err := m.Client.GetChannel(channelID, "")
	if err != nil {
		return nil
	}

	mmchannel.Id = strings.Clone(mmchannel.Id)
	mmchannel.TeamId = strings.Clone(mmchannel.TeamId)
	mmchannel.Type = model.ChannelType(strings.Clone(string(mmchannel.Type)))
	mmchannel.DisplayName = strings.Clone(mmchannel.DisplayName)
	mmchannel.Name = strings.Clone(mmchannel.Name)
	mmchannel.Header = strings.Clone(mmchannel.Header)
	mmchannel.Purpose = strings.Clone(mmchannel.Purpose)

	m.Users.mu.Lock()
	m.Users.channelData[channelID] = mmchannel
	m.Users.mu.Unlock()

	return mmchannel
}

// GetChannels returns all channels we're members off
func (m *Client) GetChannels() []*model.Channel {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	channels := make([]*model.Channel, 0, 200)
	for id := range m.Users.joinedChannels {
		if ch, exists := m.Users.channelData[id]; exists {
			channels = append(channels, ch)
		}
	}

	return channels
}

func (m *Client) GetChannelHeader(channelID string) string {
	if ch := m.GetChannel(channelID); ch != nil {
		return ch.Header
	}
	return ""
}

func getNormalisedName(channel *model.Channel) string {
	if channel.Type == model.ChannelTypeGroup {
		res := strings.ReplaceAll(channel.DisplayName, ", ", "-")
		res = strings.ReplaceAll(res, " ", "_")

		return res
	}

	return channel.Name
}

func (m *Client) GetChannelID(name string, teamID string) string {
	if teamID != "" {
		return m.getChannelIDTeam(name, teamID)
	}

	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	for _, ch := range m.Users.channelData {
		if getNormalisedName(ch) == name {
			return ch.Id
		}
	}

	return ""
}

func (m *Client) getChannelIDTeam(name string, teamID string) string {
	m.Users.mu.RLock()
	for _, ch := range m.Users.channelData {
		if ch.TeamId == teamID && getNormalisedName(ch) == name {
			m.Users.mu.RUnlock()
			return ch.Id
		}
	}
	m.Users.mu.RUnlock()

	// Fallback if it's not found in the t.Channels or t.MoreChannels cache.
	// This also let's us join private channels.
	channel, _, err := m.Client.GetChannelByName(name, teamID, "")
	if err != nil {
		return ""
	}

	channel.Id = strings.Clone(channel.Id)
	channel.TeamId = strings.Clone(channel.TeamId)
	channel.Type = model.ChannelType(strings.Clone(string(channel.Type)))
	channel.DisplayName = strings.Clone(channel.DisplayName)
	channel.Name = strings.Clone(channel.Name)
	channel.Header = strings.Clone(channel.Header)
	channel.Purpose = strings.Clone(channel.Purpose)

	m.Users.mu.Lock()
	if m.Users.channelData == nil {
		m.Users.channelData = make(map[string]*model.Channel)
	}
	m.Users.channelData[channel.Id] = channel
	m.Users.mu.Unlock()

	return channel.Id
}

func (m *Client) GetChannelName(channelID string) string {
	if ch := m.GetChannel(channelID); ch != nil {
		return getNormalisedName(ch)
	}
	return ""
}

func (m *Client) GetChannelTeamID(id string) string {
	if ch := m.GetChannel(id); ch != nil {
		return ch.TeamId
	}
	return ""
}

func (m *Client) GetChannelUsers(channelID string) ([]*model.User, error) {
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

	const batchSize = 200
	fetchedUsers := make([]UserSummary, 0, batchSize)

	idx := 0
	retryCount := 0
	for {
		query := fmt.Sprintf("/users?in_channel=%v&page=%v&per_page=%v", channelID, idx, batchSize)
		resp, err := m.Client.DoAPIGet(query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}
			shouldRetry, hErr := m.HandleRetry("GetUsersInChannel", retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}
			return nil, err
		}
		retryCount = 0

		var list []UserSummary
		if jsonErr := json.NewDecoder(resp.Body).Decode(&list); jsonErr != nil {
			resp.Body.Close()
			return nil, jsonErr
		}
		resp.Body.Close()

		fetchedUsers = append(fetchedUsers, list...)

		if len(list) < batchSize {
			break
		}
		idx++
	}

	allUsers := make([]*model.User, 0, len(fetchedUsers))

	m.Users.mu.Lock()
	if m.Users.channels[channelID] == nil {
		m.Users.channels[channelID] = make(map[string]struct{}, len(fetchedUsers))
	}

	for _, u := range fetchedUsers {
		cachedUser, exists := m.Users.users[u.Id]
		if !exists {
			cachedUser = &model.User{
				Id:        strings.Clone(u.Id),
				Username:  strings.Clone(u.Username),
				FirstName: strings.Clone(u.FirstName),
				LastName:  strings.Clone(u.LastName),
				Nickname:  strings.Clone(u.Nickname),
				Roles:     strings.Clone(u.Roles),
			}
			m.Users.users[u.Id] = cachedUser
		} else {
			// Ensure updated string fields are also cloned
			cachedUser.Username = strings.Clone(u.Username)
			cachedUser.FirstName = strings.Clone(u.FirstName)
			cachedUser.LastName = strings.Clone(u.LastName)
			cachedUser.Nickname = strings.Clone(u.Nickname)
			cachedUser.Roles = strings.Clone(u.Roles)
		}

		allUsers = append(allUsers, cachedUser)
		m.Users.channels[channelID][u.Id] = struct{}{}
	}
	m.Users.mu.Unlock()

	return allUsers, nil
}

func (m *Client) GetLastViewedAt(channelID string) int64 {
	m.RLock()
	userID := m.User.Id
	m.RUnlock()

	retryCount := 0
	for {
		res, resp, err := m.Client.GetChannelMember(channelID, userID, "")
		if err == nil {
			return res.LastViewedAt
		}

		shouldRetry, hErr := m.HandleRetry("GetChannelMember", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetChannelMember failed for %s: %v", channelID, err)
		return model.GetMillis()
	}
}

// GetMoreChannels returns existing channels where we're not a member of.
func (m *Client) GetMoreChannels() []*model.Channel {
	m.Users.mu.RLock()
	defer m.Users.mu.RUnlock()

	channels := make([]*model.Channel, 0, 200)
	for id, ch := range m.Users.channelData {
		if _, joined := m.Users.joinedChannels[id]; !joined {
			channels = append(channels, ch)
		}
	}

	return channels
}

// GetTeamFromChannel returns teamId belonging to channel (DM channels have no teamId).
func (m *Client) GetTeamFromChannel(channelID string) string {
	if ch := m.GetChannel(channelID); ch != nil {
		if ch.Type == model.ChannelTypeGroup {
			return "G"
		}
		return ch.TeamId
	}
	return ""
}

func (m *Client) JoinChannel(channelID string) error {
	m.Users.mu.RLock()
	_, joined := m.Users.joinedChannels[channelID]
	m.Users.mu.RUnlock()

	if joined {
		m.logger.Debug("Not joining ", channelID, " already joined.")
		return nil
	}

	m.logger.Debug("Joining ", channelID)

	_, _, err := m.Client.AddChannelMember(channelID, m.User.Id)
	if err != nil {
		return err
	}

	m.Users.mu.Lock()
	if m.Users.joinedChannels == nil {
		m.Users.joinedChannels = make(map[string]struct{})
	}
	m.Users.joinedChannels[channelID] = struct{}{}
	m.Users.mu.Unlock()

	return nil
}

func (m *Client) UpdateChannelsTeam(teamID string) error {
	m.RLock()
	if team, exists := m.OtherTeams[teamID]; exists {
		if time.Since(team.LastChannelSync) < 30*time.Minute {
			m.RUnlock()
			m.logger.Debugf("skipping channel fetch for team %s: cache is only %v old", teamID, time.Since(team.LastChannelSync).Round(time.Second))
			return nil
		}
	}
	m.RUnlock()

	const batchSize = 200
	var mmchannels []*model.Channel

	retryCount := 0
	for {
		var resp *model.Response
		var err error

		mmchannels, resp, err = m.Client.GetChannelsForTeamForUser(teamID, m.User.Id, false, "")
		if err == nil {
			break
		}

		shouldRetry, hErr := m.HandleRetry("GetChannelsForTeamForUser", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return err
	}

	moreChannels := make([]*model.Channel, 0, batchSize)

	idx := 0
	retryCount = 0
	for {
		channels, resp, err := m.Client.GetPublicChannelsForTeam(teamID, idx, batchSize, "")
		if err != nil {
			shouldRetry, hErr := m.HandleRetry("GetPublicChannelsForTeam", retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}
			return err
		}
		retryCount = 0

		moreChannels = append(moreChannels, channels...)
		if len(channels) < batchSize {
			break
		}
		idx++
	}

	m.Users.mu.Lock()
	if m.Users.channelData == nil {
		m.Users.channelData = make(map[string]*model.Channel)
		m.Users.joinedChannels = make(map[string]struct{})
	}

	// Helper function to sever JSON backing arrays from a channel's strings
	cloneChannelStrings := func(ch *model.Channel) {
		ch.Id = strings.Clone(ch.Id)
		ch.TeamId = strings.Clone(ch.TeamId)
		ch.Type = model.ChannelType(strings.Clone(string(ch.Type)))
		ch.DisplayName = strings.Clone(ch.DisplayName)
		ch.Name = strings.Clone(ch.Name)
		ch.Header = strings.Clone(ch.Header)
		ch.Purpose = strings.Clone(ch.Purpose)
		ch.CreatorId = strings.Clone(ch.CreatorId)
	}

	for _, ch := range mmchannels {
		cloneChannelStrings(ch)
		m.Users.channelData[ch.Id] = ch
		m.Users.joinedChannels[ch.Id] = struct{}{}
	}

	for _, ch := range moreChannels {
		if _, exists := m.Users.channelData[ch.Id]; exists {
			continue
		}
		cloneChannelStrings(ch)
		m.Users.channelData[ch.Id] = ch
	}
	m.Users.mu.Unlock()

	m.Users.lastUpdated.Store(time.Now().Unix())

	m.Lock()
	if team, exists := m.OtherTeams[teamID]; exists {
		team.LastChannelSync = time.Now()
	}
	m.Unlock()

	return nil
}

func (m *Client) UpdateChannels() error {
	if m.Team == nil {
		m.logger.Errorf("cannot update channels: primary team is nil")
		return errors.New("cannot update channels: primary team is nil")
	}

	if err := m.UpdateChannelsTeam(m.Team.ID); err != nil {
		return err
	}

	for _, t := range m.OtherTeams {
		// We've already populated users/channels for team in the above.
		if t.ID == m.Team.ID {
			continue
		}
		if err := m.UpdateChannelsTeam(t.ID); err != nil {
			return err
		}
	}

	return nil
}

func (m *Client) UpdateChannelHeader(channelID string, header string) {
	channel := &model.Channel{Id: channelID, Header: header}

	m.logger.Debugf("updating channelheader %#v, %#v", channelID, header)

	_, _, err := m.Client.UpdateChannel(channel)
	if err != nil {
		m.logger.Error(err)
	}
}

func (m *Client) UpdateChannelUsersCache(channelID string, user *model.User) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	m.Users.users[user.Id] = user

	if channelID != "" {
		if m.Users.channels[channelID] != nil {
			m.Users.channels[channelID][user.Id] = struct{}{}
		}
	}
}

func (m *Client) UpdateChannelUsersCacheRemove(channelID string, userID string) {
	m.Users.mu.Lock()
	defer m.Users.mu.Unlock()

	if m.Users.channels != nil && m.Users.channels[channelID] != nil {
		delete(m.Users.channels[channelID], userID)
	}
}

func (m *Client) UpdateLastViewed(channelID string) error {
	m.logger.Debugf("posting lastview %#v", channelID)

	view := &model.ChannelView{ChannelId: channelID}

	retryCount := 0
	for {
		_, resp, err := m.Client.ViewChannel(m.User.Id, view)
		if err == nil {
			return nil
		}

		shouldRetry, hErr := m.HandleRetry("ViewChannel", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("ChannelView update for %s failed: %v", channelID, err)
		return err
	}
}
