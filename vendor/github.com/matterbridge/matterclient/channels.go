package matterclient

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/mattermost/mattermost/server/public/model"
)

func (m *Client) GetChannel(ctx context.Context, channelID string) *model.Channel {
	m.Users.mu.RLock()
	ch, exists := m.Users.channelData[channelID]
	m.Users.mu.RUnlock()

	if exists {
		return ch
	}

	query := "/channels/" + channelID
	m.apiLogger.Warnf("GetChannel: DoAPIGet: query %s", query)
	resp, err := m.Client.DoAPIGet(ctx, query, "")
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

func (m *Client) GetChannelHeader(ctx context.Context, channelID string) string {
	if ch := m.GetChannel(ctx, channelID); ch != nil {
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

func (m *Client) GetChannelID(ctx context.Context, name string, teamID string) string {
	if teamID != "" {
		return m.getChannelIDTeam(ctx, name, teamID)
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

func (m *Client) getChannelIDTeam(ctx context.Context, name string, teamID string) string {
	m.Users.mu.RLock()
	for _, ch := range m.Users.channelData {
		if ch.TeamId == teamID && getNormalisedName(ch) == name {
			m.Users.mu.RUnlock()
			return ch.Id
		}
	}
	m.Users.mu.RUnlock()

	query := "/teams/" + teamID + "/channels/name/" + name
	m.apiLogger.Warnf("getChannelIDTeam: DoAPIGet: query %s", query)
	resp, err := m.Client.DoAPIGet(ctx, query, "")
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

func (m *Client) GetChannelName(ctx context.Context, channelID string) string {
	if ch := m.GetChannel(ctx, channelID); ch != nil {
		return getNormalisedName(ch)
	}
	return ""
}

func (m *Client) GetChannelTeamID(ctx context.Context, id string) string {
	if ch := m.GetChannel(ctx, id); ch != nil {
		return ch.TeamId
	}
	return ""
}

//nolint:funlen,gocognit,gocyclo
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

	const batchSize = 200
	fetchedUsers := make([]UserSummary, 0, batchSize)

	idx := 0
	retryCount := 0
	for {
		if m.IsAborted(ctx) {
			return nil, errors.New("login aborted")
		}

		query := "/users?in_channel=" + channelID + "&page=" + strconv.Itoa(idx) + "&per_page=" + strconv.Itoa(batchSize)
		m.apiLogger.Warnf("GetChannelUsers: DoAPIGet: query %s #%d", query, retryCount)
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

		roles := u.Roles
		if roles == "system_user" { //nolint:goconst
			roles = "system_user"
		} else if roles == "system_admin system_user" { //nolint:goconst
			roles = "system_admin system_user"
		}

		if !exists { //nolint:nestif
			cachedUser = &model.User{
				Id:        u.Id,
				Username:  u.Username,
				FirstName: u.FirstName,
				LastName:  u.LastName,
				Nickname:  u.Nickname,
				Roles:     roles,
				Props:     u.Props,
			}
			m.Users.users[u.Id] = cachedUser
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

		allUsers = append(allUsers, cachedUser)
		m.Users.channels[channelID][cachedUser.Id] = struct{}{}
	}
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

	retryCount := 0
	for {
		m.apiLogger.Warnf("GetLastViewedAt: ChannelID: %s, UserID: %s #%d", channelID, userID, retryCount)
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

		m.logger.Errorf("GetChannelMember failed for %s: %v", channelID, err)

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

	channels := make([]*model.Channel, 0, 200)
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

func (m *Client) JoinChannel(ctx context.Context, channelID string) error {
	m.Users.mu.RLock()
	_, joined := m.Users.joinedChannels[channelID]
	m.Users.mu.RUnlock()

	if joined {
		m.logger.Debug("Not joining ", channelID, " already joined.")
		return nil
	}

	m.logger.Debug("Joining ", channelID)

	m.apiLogger.Warnf("JoinChannel: ChannelID: %s, UserID: %s", channelID, m.User.Id)
	_, _, err := m.Client.AddChannelMember(ctx, channelID, m.User.Id)
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

//nolint:funlen,gocognit,gocyclo
func (m *Client) UpdateChannelsTeam(ctx context.Context, teamID string) error {
	m.RLock()
	if team, exists := m.OtherTeams[teamID]; exists {
		if time.Since(team.LastChannelSync) < 15*time.Minute {
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
		if m.IsAborted(ctx) {
			return errors.New("login aborted")
		}

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

		if err := json.NewDecoder(resp.Body).Decode(&joinedSummaries); err != nil {
			resp.Body.Close()
			return err
		}
		resp.Body.Close()
		break
	}

	publicSummaries := make([]ChannelSummary, 0, batchSize)
	var list []ChannelSummary

	m.Users.mu.RLock()
	skipPublic := !m.ForceSyncOnReconnect && m.Users.channelData != nil
	m.Users.mu.RUnlock()

	if !skipPublic {
		idx := 0
		retryCount = 0
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
	} else {
		m.logger.Debug("UpdateChannelsTeam: skipping public channel fetch (ForceSyncOnReconnect is disabled)")
	}

	// Helper to intern highly repetitive channel types
	internType := func(t string) model.ChannelType {
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

	m.Users.mu.Lock()
	if m.Users.channelData == nil {
		totalChannels := len(joinedSummaries) + len(publicSummaries)
		m.Users.channelData = make(map[string]*model.Channel, totalChannels)
		m.Users.joinedChannels = make(map[string]struct{}, len(joinedSummaries))
	} else {
		for chanID := range m.Users.joinedChannels {
			if ch, ok := m.Users.channelData[chanID]; ok && ch.TeamId == teamID {
				delete(m.Users.joinedChannels, chanID)
			}
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

func (m *Client) UpdateChannels(ctx context.Context) error {
	if m.Team == nil {
		m.logger.Errorf("cannot update channels: primary team is nil")
		return errors.New("cannot update channels: primary team is nil")
	}

	if err := m.UpdateChannelsTeam(ctx, m.Team.ID); err != nil {
		return err
	}

	for _, t := range m.OtherTeams {
		// We've already populated users/channels for team in the above.
		if t.ID == m.Team.ID {
			continue
		}
		if err := m.UpdateChannelsTeam(ctx, t.ID); err != nil {
			return err
		}
	}

	return nil
}

func (m *Client) UpdateChannelHeader(ctx context.Context, channelID string, header string) {
	channel := &model.Channel{Id: channelID, Header: header}

	m.logger.Debugf("updating channelheader %#v, %#v", channelID, header)

	m.apiLogger.Warnf("UpdateChannelHeader: ChannelID: %s", channelID)
	_, _, err := m.Client.UpdateChannel(ctx, channel)
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

func (m *Client) UpdateLastViewed(ctx context.Context, channelID string) error {
	m.logger.Debugf("posting lastview %#v", channelID)

	view := &model.ChannelView{ChannelId: channelID}

	retryCount := 0
	for {
		m.apiLogger.Warnf("UpdateLastViewed: ChannelID: %s, UserID: %s #%d", channelID, m.User.Id, retryCount)
		_, resp, err := m.Client.ViewChannel(ctx, m.User.Id, view)
		if err == nil {
			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "ViewChannel", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("ChannelView update for %s failed: %v", channelID, err)
		return err
	}
}
