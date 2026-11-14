package slackclient

import (
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func (c *Client) createSlackMsgOption(text string) []slack.MsgOption {
	np := slack.NewPostMessageParameters()
	np.AsUser = true
	np.Parse = "full"

	var opts []slack.MsgOption

	opts = append(opts,
		slack.MsgOptionPostMessageParameters(np),
		// provide regular text field (fallback used in Slack notifications, etc.)
		slack.MsgOptionText(text, false),
	)

	return opts
}

func (c *Client) SendDirectMessage(username, text string) (string, error) {
	dchannel, _, _, err := c.sc.OpenConversation(&slack.OpenConversationParameters{
		Users: []string{username},
	})
	if err != nil {
		return "", err
	}

	opts := c.createSlackMsgOption(text)

	_, msgID, err := c.sc.PostMessage(dchannel.ID, opts...)
	if err != nil {
		return "", err
	}

	c.Lock()
	c.msgLast[dchannel.ID] = msgID
	c.Unlock()

	return msgID, nil
}

func (c *Client) PostMessage(channelID, text string) (string, error) {
	chID := strings.ToUpper(channelID)
	opts := c.createSlackMsgOption(text)

	_, msgID, err := c.sc.PostMessage(chID, opts...)
	if err != nil {
		return "", err
	}

	c.Lock()
	c.msgLast[chID] = msgID
	c.Unlock()

	return msgID, nil
}

func (c *Client) IsOwnMessageEcho(userID, channelID, timestamp string) bool {
	if userID != c.info.User.ID {
		return false
	}

	c.RLock()
	lastmsg := c.msgLast[channelID]
	c.RUnlock()

	// Slack can be faster in sending new message than replying to POST
	if lastmsg < timestamp {
		time.Sleep(100 * time.Millisecond)

		c.RLock()
		lastmsg = c.msgLast[channelID]
		c.RUnlock()
	}

	return lastmsg == timestamp
}
