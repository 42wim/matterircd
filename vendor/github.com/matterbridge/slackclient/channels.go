package slackclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func (c *Client) Invite(channelID, username string) error {
	_, err := c.sc.InviteUsersToConversation(strings.ToUpper(channelID), username)
	return err
}

const (
	channelsCacheTTL       = 5 * time.Minute
	channelMembersCacheTTL = 5 * time.Minute
	userChannelsCacheTTL   = 5 * time.Minute
)

func (c *Client) IsChannelMember(channelID string) bool {
	if channelID == "" {
		return false
	}

	c.RLock()
	member, ok := c.joinedChannels[channelID]
	c.RUnlock()

	if ok {
		return member
	}

	if c.sc == nil {
		return false
	}

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return false
	}

	c.cacheChannel(*info)

	return info.IsMember
}

func (c *Client) cacheChannel(ch slack.Channel) {
	if ch.ID == "" {
		return
	}

	c.Lock()
	if ch.Name != "" {
		c.channels[ch.ID] = "#" + ch.Name
	}

	c.channelData[ch.ID] = ch
	c.joinedChannels[ch.ID] = ch.IsMember
	c.Unlock()
}

func (c *Client) JoinChannel(channelName string) (string, string, error) {
	mychan, _, _, err := c.sc.JoinConversation(channelName)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i): %w", err)
	}

	mychan.IsMember = true
	c.cacheChannel(*mychan)

	return mychan.ID, mychan.Topic.Value, nil
}

func (c *Client) GetConversations() ([]slack.Channel, error) {
	now := time.Now().Unix()

	c.RLock()

	fresh := c.channelsFetchedAt > 0 && now-c.channelsFetchedAt < int64(channelsCacheTTL.Seconds())
	if (fresh || c.sc == nil) && len(c.channelData) > 0 {
		channels := make([]slack.Channel, 0, len(c.channelData))
		for _, ch := range c.channelData {
			channels = append(channels, ch)
		}

		c.RUnlock()

		return channels, nil
	}

	c.RUnlock()

	if c.sc == nil {
		return nil, nil
	}

	return c.RefreshConversations()
}

func (c *Client) RefreshConversations() ([]slack.Channel, error) {
	if c.sc == nil {
		return nil, nil
	}

	var channels []slack.Channel

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: true,
		Limit:           100,
		Types:           []string{"public_channel", "private_channel", "mpim"},
	}

	for {
		conversations, nextCursor, err := c.sc.GetConversations(&params)
		if err != nil {
			c.logger.Error("GetConversations", err)
			break
		}

		params.Cursor = nextCursor

		channels = append(channels, conversations...)

		if nextCursor == "" {
			break
		}
	}

	c.Lock()
	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}

		if ch.Name != "" {
			c.channels[ch.ID] = "#" + ch.Name
		}

		c.channelData[ch.ID] = ch
		c.joinedChannels[ch.ID] = ch.IsMember
	}

	c.channelsFetchedAt = time.Now().Unix()
	c.Unlock()

	return channels, nil
}

func (c *Client) GetChannels() []slack.Channel {
	var channels []slack.Channel

	conversations, _ := c.GetConversations()
	for _, mmchannel := range conversations {
		if !mmchannel.IsMember {
			continue
		}

		channels = append(channels, mmchannel)
	}

	return channels
}

func (c *Client) LeaveChannel(channelID string) error {
	upperID := strings.ToUpper(channelID)

	_, err := c.sc.LeaveConversation(upperID)
	if err == nil {
		c.SetChannelJoined(upperID, false)
	}

	return err
}

func (c *Client) Kick(channelID, username string) error {
	return c.sc.KickUserFromConversation(strings.ToUpper(channelID), username)
}

func (c *Client) Topic(channelID string) string {
	upperID := strings.ToUpper(channelID)
	now := time.Now().Unix()

	c.RLock()
	ch, ok := c.channelData[upperID]
	fresh := c.channelsFetchedAt > 0 && now-c.channelsFetchedAt < int64(channelsCacheTTL.Seconds())
	c.RUnlock()

	if ok && fresh {
		return ch.Topic.Value
	}

	if c.sc == nil {
		return ""
	}

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: upperID,
	})
	if err != nil {
		c.logger.Errorf("error getting topic of %s: %s", channelID, err)
		return ""
	}

	if info != nil {
		c.cacheChannel(*info)
		return info.Topic.Value
	}

	return ""
}

func (c *Client) UpdateTopic(channelID, text string) {
	if channelID == "" {
		return
	}

	upperID := strings.ToUpper(channelID)

	c.Lock()
	if ch, ok := c.channelData[upperID]; ok {
		ch.Topic.Value = text
		c.channelData[upperID] = ch
	}
	c.Unlock()
}

func (c *Client) SetTopic(channelID, text string) error {
	upperID := strings.ToUpper(channelID)

	_, err := c.sc.SetTopicOfConversation(upperID, text)
	if err == nil {
		c.UpdateTopic(upperID, text)
	}

	return err
}

func (c *Client) GetChannelName(channelID string) string {
	c.RLock()

	if name, ok := c.channels[channelID]; ok {
		c.RUnlock()
		return name
	}

	c.RUnlock()

	if c.sc == nil {
		return channelID
	}

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return channelID
	}

	c.cacheChannel(*info)

	if info.Name == "" {
		return channelID
	}

	return "#" + info.Name
}

func (c *Client) CreateChannel(ctx context.Context, name string, isPrivate bool) (*slack.Channel, error) {
	ch, err := c.sc.CreateConversationContext(ctx, slack.CreateConversationParams{
		ChannelName: name,
		IsPrivate:   isPrivate,
	})
	if err != nil {
		return nil, err
	}

	ch.IsMember = true
	c.cacheChannel(*ch)

	return ch, nil
}

func (c *Client) GetChannelID(ctx context.Context, name string) string {
	target := "#" + strings.TrimPrefix(name, "#")

	c.RLock()

	for id, chName := range c.channels {
		if strings.EqualFold(chName, target) {
			c.RUnlock()
			return id
		}
	}

	c.RUnlock()

	channels, _ := c.GetConversations()
	for _, ch := range channels {
		if strings.EqualFold(ch.Name, strings.TrimPrefix(name, "#")) {
			return ch.ID
		}
	}

	return ""
}

func (c *Client) GetConversationInfo(ctx context.Context, channelID string) (*slack.Channel, error) {
	return c.sc.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
}

func (c *Client) MarkConversation(ctx context.Context, channelID, ts string) error {
	return c.sc.MarkConversationContext(ctx, channelID, ts)
}

func (c *Client) resolveChannelUsers(members []string) []*slack.User {
	var users []*slack.User

	selfID := ""
	if c.info != nil && c.info.User != nil {
		selfID = c.info.User.ID
	}

	for _, user := range members {
		if selfID != "" && selfID == user {
			continue
		}

		if u := c.GetUser(user); u != nil {
			users = append(users, u)
		}
	}

	return users
}

func (c *Client) fetchChannelMembers(channelID string) ([]string, error) {
	const limit = 100

	params := slack.GetUsersInConversationParameters{
		ChannelID: channelID,
		Cursor:    "",
		Limit:     limit,
	}

	var allMembers []string

	for {
		members, nextCursor, err := c.sc.GetUsersInConversation(&params)
		if err != nil {
			return nil, err
		}

		params.Cursor = nextCursor

		allMembers = append(allMembers, members...)

		if nextCursor == "" {
			break
		}
	}

	c.Lock()
	c.channelMembers[channelID] = channelMembersEntry{
		members:   slices.Clone(allMembers),
		fetchedAt: time.Now().Unix(),
	}
	c.Unlock()

	return allMembers, nil
}

func (c *Client) GetChannelUsers(channelID string) ([]*slack.User, error) {
	if channelID == "" {
		return nil, nil
	}

	now := time.Now().Unix()

	c.RLock()
	entry, hasMembers := c.channelMembers[channelID]
	_, hasChannel := c.channelData[channelID]
	c.RUnlock()

	if hasMembers && ((entry.fetchedAt > 0 && now-entry.fetchedAt < int64(channelMembersCacheTTL.Seconds())) || c.sc == nil) {
		return c.resolveChannelUsers(entry.members), nil
	}

	if c.sc == nil {
		return nil, nil
	}

	if !hasChannel {
		info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
			ChannelID: channelID,
		})
		if err != nil {
			return nil, err
		}

		if info == nil {
			return nil, errors.New("Unknown channel seen (" + channelID + ")")
		}

		c.cacheChannel(*info)
	}

	allMembers, err := c.fetchChannelMembers(channelID)
	if err != nil {
		return nil, err
	}

	return c.resolveChannelUsers(allMembers), nil
}

func (c *Client) GetUsersInConversation(channelID string) ([]string, error) {
	if c.sc == nil {
		return nil, errors.New("slackclient: not connected")
	}

	members, _, err := c.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{
		ChannelID: channelID,
	})

	return members, err
}

func (c *Client) cachedDMOtherUserID(upperID, selfID string) (string, bool) {
	c.RLock()
	defer c.RUnlock()

	if ch, ok := c.channelData[upperID]; ok && ch.User != "" && ch.User != selfID {
		return ch.User, true
	}

	for uid, dmChID := range c.dmChannels {
		if strings.EqualFold(dmChID, upperID) && uid != "" && uid != selfID {
			return uid, true
		}
	}

	if entry, ok := c.channelMembers[upperID]; ok {
		for _, m := range entry.members {
			if m != "" && m != selfID {
				return m, true
			}
		}
	}

	return "", false
}

func (c *Client) CacheDMChannel(userID, channelID string) {
	if userID == "" || channelID == "" {
		return
	}

	c.Lock()
	c.dmChannels[userID] = strings.ToUpper(channelID)
	c.Unlock()
}

func (c *Client) GetDMOtherUserID(channelID, myUserID string) (string, bool) {
	upperID := strings.ToUpper(strings.TrimSpace(channelID))
	if !strings.HasPrefix(upperID, "D") {
		return "", false
	}

	selfID := myUserID
	if selfID == "" {
		selfID = c.UserID()
	}

	if uid, ok := c.cachedDMOtherUserID(upperID, selfID); ok {
		return uid, true
	}

	if c.sc == nil {
		return "", false
	}

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: upperID,
	})
	if err == nil && info != nil {
		c.cacheChannel(*info)

		if info.User != "" && info.User != selfID {
			c.CacheDMChannel(info.User, upperID)
			return info.User, true
		}
	}

	members, err := c.GetUsersInConversation(upperID)
	if err != nil {
		return "", false
	}

	for _, m := range members {
		if m != "" && m != selfID {
			c.CacheDMChannel(m, upperID)
			return m, true
		}
	}

	return "", false
}

func (c *Client) UpdateChannel(channelID, name string) {
	if channelID == "" || name == "" {
		return
	}

	cleanName := strings.TrimPrefix(name, "#")

	c.Lock()
	c.channels[channelID] = "#" + cleanName

	if ch, ok := c.channelData[channelID]; ok {
		ch.Name = cleanName
		c.channelData[channelID] = ch
	}
	c.Unlock()
}

func (c *Client) RemoveChannel(channelID string) {
	if channelID == "" {
		return
	}

	c.Lock()
	delete(c.channels, channelID)
	delete(c.channelData, channelID)
	delete(c.joinedChannels, channelID)
	delete(c.channelMembers, channelID)
	c.Unlock()
}

func (c *Client) SetChannelJoined(channelID string, joined bool) {
	if channelID == "" {
		return
	}

	c.Lock()
	c.joinedChannels[channelID] = joined

	if ch, ok := c.channelData[channelID]; ok {
		ch.IsMember = joined
		c.channelData[channelID] = ch
	}
	c.Unlock()
}

func (c *Client) AddChannelMember(channelID, userID string) {
	if channelID == "" || userID == "" {
		return
	}

	c.Lock()
	if entry, ok := c.channelMembers[channelID]; ok && !slices.Contains(entry.members, userID) {
		entry.members = append(entry.members, userID)
		c.channelMembers[channelID] = entry
	}

	delete(c.userChannels, userID)
	c.Unlock()
}

func (c *Client) RemoveChannelMember(channelID, userID string) {
	if channelID == "" || userID == "" {
		return
	}

	c.Lock()
	if entry, ok := c.channelMembers[channelID]; ok {
		entry.members = slices.DeleteFunc(entry.members, func(id string) bool {
			return id == userID
		})
		c.channelMembers[channelID] = entry
	}

	delete(c.userChannels, userID)
	c.Unlock()
}

func (c *Client) GetChannelCount() int {
	c.RLock()
	defer c.RUnlock()

	return len(c.channels)
}

func (c *Client) InvalidateUserChannels(userID string) {
	c.Lock()
	if userID == "" {
		clear(c.userChannels)
	} else {
		delete(c.userChannels, userID)
	}
	c.Unlock()
}

func (c *Client) fetchUserPublicChannels(ctx context.Context, userID string) ([]slack.Channel, []string, error) {
	params := slack.GetConversationsForUserParameters{
		UserID:          userID,
		Types:           []string{"public_channel"},
		Limit:           200,
		ExcludeArchived: true,
	}

	var (
		names  []string
		chMeta []slack.Channel
	)

	for {
		channels, nextCursor, err := c.sc.GetConversationsForUserContext(ctx, &params)
		if err != nil {
			return nil, nil, err
		}

		for _, ch := range channels {
			if ch.Name != "" {
				names = append(names, ch.Name)
			}
		}

		chMeta = append(chMeta, channels...)

		if nextCursor == "" {
			break
		}

		params.Cursor = nextCursor
	}

	return chMeta, names, nil
}

func (c *Client) GetUserChannels(ctx context.Context, userID string) ([]string, error) {
	if userID == "" {
		return []string{}, nil
	}

	now := time.Now().Unix()

	c.RLock()
	entry, ok := c.userChannels[userID]
	c.RUnlock()

	if ok && entry.fetchedAt > 0 && now-entry.fetchedAt < int64(userChannelsCacheTTL.Seconds()) {
		return slices.Clone(entry.channels), nil
	}

	if c.sc == nil {
		if ok {
			return slices.Clone(entry.channels), nil
		}

		return []string{}, nil
	}

	chMeta, names, err := c.fetchUserPublicChannels(ctx, userID)
	if err != nil {
		if ok {
			return slices.Clone(entry.channels), nil
		}

		return nil, err
	}

	c.Lock()
	for _, ch := range chMeta {
		if ch.ID != "" && ch.Name != "" {
			c.channels[ch.ID] = "#" + ch.Name
		}
	}

	c.userChannels[userID] = userChannelsEntry{
		channels:  slices.Clone(names),
		fetchedAt: time.Now().Unix(),
	}
	c.Unlock()

	return names, nil
}
