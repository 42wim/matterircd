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

	query := fmt.Sprintf("/channels/%v", channelID)
	resp, err := m.Client.DoAPIGet(query, "")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	var summary ChannelSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return nil
	}

	mmchannel := &model.Channel{
		Id:          summary.Id,
		TeamId:      summary.TeamId,
		Type:        model.ChannelType(summary.Type),
		DisplayName: summary.DisplayName,
		Name:        summary.Name,
		Header:      summary.Header,
		Purpose:     summary.Purpose,
		CreatorId:   summary.CreatorId,
	}

	m.Users.mu.Lock()
	if m.Users.channelData == nil {
		m.Users.channelData = make(map[string]*model.Channel)
	}
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
	// This also lets us join private channels.
	query := fmt.Sprintf("/teams/%v/channels/name/%v", teamID, name)
	resp, err := m.Client.DoAPIGet(query, "")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()

	var summary ChannelSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		return ""
	}

	channel := &model.Channel{
		Id:          summary.Id,
		TeamId:      summary.TeamId,
		Type:        model.ChannelType(summary.Type),
		DisplayName: summary.DisplayName,
		Name:        summary.Name,
		Header:      summary.Header,
		Purpose:     summary.Purpose,
		CreatorId:   summary.CreatorId,
	}

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
			// Intern common roles to prevent massive string duplication
			roles := u.Roles
			if roles == "system_user" {
				roles = "system_user"
			} else if roles == "system_admin system_user" {
				roles = "system_admin system_user"
			}

			cachedUser = &model.User{
				Id:        u.Id,
				Username:  u.Username,
				FirstName: u.FirstName,
				LastName:  u.LastName,
				Nickname:  u.Nickname,
				Roles:     roles,
			}
			m.Users.users[u.Id] = cachedUser
		} else {
			// Only update string fields if they actually changed!
			// This prevents tenured strings from being replaced by newly allocated
			// JSON strings, saving massive GC churn on cache refresh.
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
			if cachedUser.Roles != u.Roles {
				cachedUser.Roles = u.Roles
			}
		}

		allUsers = append(allUsers, cachedUser)
		m.Users.channels[channelID][cachedUser.Id] = struct{}{}
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

	var joinedSummaries []ChannelSummary
	retryCount := 0
	for {
		query := fmt.Sprintf("/users/%v/teams/%v/channels", m.User.Id, teamID)
		resp, err := m.Client.DoAPIGet(query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}
			shouldRetry, hErr := m.HandleRetry("GetChannelsForTeamForUser", retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}
			return err
		}

		if err := json.NewDecoder(resp.Body).Decode(&joinedSummaries); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
		break
	}

	publicSummaries := make([]ChannelSummary, 0, batchSize)
	var list []ChannelSummary

	idx := 0
	retryCount = 0
	for {
		query := fmt.Sprintf("/teams/%v/channels?page=%v&per_page=%v", teamID, idx, batchSize)
		resp, err := m.Client.DoAPIGet(query, "")
		if err != nil {
			var mResp *model.Response
			if resp != nil {
				mResp = model.BuildResponse(resp)
			}
			shouldRetry, hErr := m.HandleRetry("GetPublicChannelsForTeam", retryCount, 10, mResp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}
			return err
		}
		retryCount = 0

		list = list[:0]
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()

		publicSummaries = append(publicSummaries, list...)

		if len(list) < batchSize {
			break
		}
		idx++
	}

	// Helper to intern highly repetitive channel types
	internType := func(t string) model.ChannelType {
		switch t {
		case "O":
			return model.ChannelType("O")
		case "P":
			return model.ChannelType("P")
		case "D":
			return model.ChannelType("D")
		}
		return model.ChannelType(t)
	}

	m.Users.mu.Lock()
	if m.Users.channelData == nil {
		totalChannels := len(joinedSummaries) + len(publicSummaries)
		m.Users.channelData = make(map[string]*model.Channel, totalChannels)

		// Allocate the map buckets exactly once on startup
		m.Users.joinedChannels = make(map[string]struct{}, len(joinedSummaries))
	} else {
		// This instantly empties the map while preserving the underlying memory buckets,
		// completely eliminating allocation churn for subsequent syncs!
		// clear(m.Users.joinedChannels) // rquires Go 1.21+
		for k := range m.Users.joinedChannels {
			delete(m.Users.joinedChannels, k)
		}
	}

	for _, ch := range joinedSummaries {
		cached, exists := m.Users.channelData[ch.Id]
		if !exists { //nolint:nestif
			cached = &model.Channel{
				Id:          ch.Id,
				TeamId:      teamID,
				Type:        internType(ch.Type),
				DisplayName: ch.DisplayName,
				Name:        ch.Name,
				Header:      ch.Header,
				Purpose:     ch.Purpose,
				CreatorId:   ch.CreatorId,
			}
			m.Users.channelData[cached.Id] = cached
		} else {
			// Save tenured GC strings by conditionally updating
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
			// It's rare for type to change, but check it safely using our interner
			if newType := internType(ch.Type); cached.Type != newType {
				cached.Type = newType
			}
		}
		m.Users.joinedChannels[cached.Id] = struct{}{}
	}

	for _, ch := range publicSummaries {
		cached, exists := m.Users.channelData[ch.Id]
		if !exists { //nolint:nestif
			cached = &model.Channel{
				Id:          ch.Id,
				TeamId:      teamID,
				Type:        internType(ch.Type),
				DisplayName: ch.DisplayName,
				Name:        ch.Name,
				Header:      ch.Header,
				Purpose:     ch.Purpose,
				CreatorId:   ch.CreatorId,
			}
			m.Users.channelData[cached.Id] = cached
		} else {
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
			if newType := internType(ch.Type); cached.Type != newType {
				cached.Type = newType
			}
		}
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
