package slackclient

import (
	"context"
	"strings"
	"time"

	"github.com/slack-go/slack"
)

func (c *Client) createSlackMsgOption(text, threadTS string) []slack.MsgOption {
	np := slack.NewPostMessageParameters()
	np.AsUser = true
	np.Parse = "full"

	var opts []slack.MsgOption

	opts = append(opts,
		slack.MsgOptionPostMessageParameters(np),
		// provide regular text field (fallback used in Slack notifications, etc.)
		slack.MsgOptionText(text, false),
	)

	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}

	return opts
}

func (c *Client) OpenDMChannel(ctx context.Context, userID string) (string, error) {
	c.RLock()
	chID, ok := c.dmChannels[userID]
	c.RUnlock()

	if ok {
		return chID, nil
	}

	dchannel, _, _, err := c.sc.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users: []string{userID},
	})
	if err != nil {
		return "", err
	}

	c.Lock()
	c.dmChannels[userID] = dchannel.ID
	c.Unlock()

	return dchannel.ID, nil
}

func (c *Client) SendDirectMessage(username, text string) (string, error) {
	chID, err := c.OpenDMChannel(context.Background(), username)
	if err != nil {
		return "", err
	}

	return c.PostMessageThread(context.Background(), chID, "", text)
}

func (c *Client) PostMessage(channelID, text string) (string, error) {
	return c.PostMessageThread(context.Background(), channelID, "", text)
}

func (c *Client) PostMessageThread(ctx context.Context, channelID, threadTS, text string) (string, error) {
	chID := strings.ToUpper(channelID)
	opts := c.createSlackMsgOption(text, threadTS)

	_, msgID, err := c.sc.PostMessageContext(ctx, chID, opts...)
	if err != nil {
		return "", err
	}

	c.Lock()
	c.msgLast[chID] = msgID
	c.Unlock()

	return msgID, nil
}

func (c *Client) GetConversationReplies(ctx context.Context, channelID, ts string) ([]slack.Message, error) {
	chID := strings.ToUpper(channelID)

	var (
		all    []slack.Message
		cursor string
	)

	for {
		msgs, hasMore, nextCursor, err := c.sc.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: chID,
			Timestamp: ts,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return nil, err
		}

		all = append(all, msgs...)

		if !hasMore || nextCursor == "" {
			break
		}

		cursor = nextCursor
	}

	return all, nil
}

func (c *Client) AddReaction(ctx context.Context, channelID, ts, emoji string) error {
	return c.sc.AddReactionContext(ctx, strings.Trim(emoji, ":"), slack.NewRefToMessage(strings.ToUpper(channelID), ts))
}

func (c *Client) RemoveReaction(ctx context.Context, channelID, ts, emoji string) error {
	return c.sc.RemoveReactionContext(ctx, strings.Trim(emoji, ":"), slack.NewRefToMessage(strings.ToUpper(channelID), ts))
}

func (c *Client) UpdateMessage(ctx context.Context, channelID, ts, text string) error {
	//nolint:dogsled
	_, _, _, err := c.sc.UpdateMessageContext(ctx, strings.ToUpper(channelID), ts,
		slack.MsgOptionText(text, false),
		slack.MsgOptionParse(true),
	)

	return err
}

func (c *Client) DeleteMessage(ctx context.Context, channelID, ts string) error {
	_, _, err := c.sc.DeleteMessageContext(ctx, strings.ToUpper(channelID), ts)
	return err
}

func (c *Client) GetPermalink(ctx context.Context, channelID, ts string) (string, error) {
	return c.sc.GetPermalinkContext(ctx, &slack.PermalinkParameters{
		Channel: strings.ToUpper(channelID),
		Ts:      ts,
	})
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
