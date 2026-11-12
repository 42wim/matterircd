package slackclient

import (
	"errors"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

func (c *Client) Invite(channelID, username string) error {
	_, err := c.sc.InviteUsersToConversation(strings.ToUpper(channelID), username)
	return err
}

func (c *Client) IsChannelMember(channelID string) bool {
	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return false
	}

	return info.IsMember
}

func (c *Client) JoinChannel(channelName string) (string, string, error) {
	mychan, _, _, err := c.sc.JoinConversation(channelName)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i): %w", err)
	}

	return mychan.ID, mychan.Topic.Value, nil
}

func (c *Client) GetConversations() ([]slack.Channel, error) {
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
		}

		params.Cursor = nextCursor

		channels = append(channels, conversations...)

		if nextCursor == "" {
			break
		}
	}

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
	_, err := c.sc.LeaveConversation(strings.ToUpper(channelID))
	return err
}

func (c *Client) Kick(channelID, username string) error {
	return c.sc.KickUserFromConversation(strings.ToUpper(channelID), username)
}

func (c *Client) Topic(channelID string) string {
	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: strings.ToUpper(channelID),
	})
	if err != nil {
		c.logger.Errorf("error getting topic of %s: %s", channelID, err)
		return ""
	}

	return info.Topic.Value
}

func (c *Client) SetTopic(channelID, text string) error {
	_, err := c.sc.SetTopicOfConversation(strings.ToUpper(channelID), text)
	return err
}

func (c *Client) GetChannelName(channelID string) string {
	var name string

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		name = channelID
	} else {
		name = "#" + info.Name
	}

	return name
}

func (c *Client) GetChannelUsers(channelID string) ([]*slack.User, error) {
	var users []*slack.User

	limit := 100

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil {
		return nil, err
	}

	if info == nil {
		return nil, errors.New("Unknown channel seen (" + channelID + ")")
	}

	params := slack.GetUsersInConversationParameters{
		ChannelID: channelID,
		Cursor:    "",
		Limit:     limit,
	}

	for {
		members, nextCursor, _ := c.sc.GetUsersInConversation(&params)
		params.Cursor = nextCursor

		for _, user := range members {
			if c.info.User.ID == user {
				continue
			}

			users = append(users, c.GetUser(user))
		}

		if nextCursor == "" {
			break
		}
	}

	return users, nil
}

func (c *Client) GetUsersInConversation(channelID string) ([]string, error) {
	members, _, err := c.sc.GetUsersInConversation(&slack.GetUsersInConversationParameters{
		ChannelID: channelID,
	})

	return members, err
}
