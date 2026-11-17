package slackclient

import (
	"context"
	"errors"
	"slices"
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

const (
	maxRecentSent      = 32
	ownEchoWaitTimeout = 500 * time.Millisecond
)

func (c *Client) recordSentLocked(chID, msgID string) {
	if c.recentSent == nil {
		c.recentSent = make(map[string][]string)
	}

	sent := c.recentSent[chID]
	if len(sent) >= maxRecentSent {
		copy(sent, sent[1:])
		sent[len(sent)-1] = msgID
		c.recentSent[chID] = sent

		return
	}

	c.recentSent[chID] = append(sent, msgID)
}

func (c *Client) broadcastSentLocked() {
	if c.sentCh != nil {
		close(c.sentCh)
		c.sentCh = make(chan struct{})
	}
}

func (c *Client) PostMessageThread(ctx context.Context, channelID, threadTS, text string) (string, error) {
	chID := strings.ToUpper(channelID)
	opts := c.createSlackMsgOption(text, threadTS)

	c.Lock()
	if c.sending == nil {
		c.sending = make(map[string]int)
	}

	c.sending[chID]++
	c.Unlock()

	_, msgID, err := c.sc.PostMessageContext(ctx, chID, opts...)

	c.Lock()

	if c.sending[chID] <= 1 {
		delete(c.sending, chID)
	} else {
		c.sending[chID]--
	}

	if err == nil && msgID != "" {
		c.recordSentLocked(chID, msgID)
	}

	c.broadcastSentLocked()
	c.Unlock()

	if err != nil {
		return "", err
	}

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

// IsOwnMessageEcho reports whether a Slack RTM message from ourselves echoes a
// post sent via PostMessageThread. If a send in the same channel is still in
// flight, it waits briefly for the API response so fast RTM echoes aren't missed.
func (c *Client) IsOwnMessageEcho(userID, channelID, timestamp string) bool {
	if c.info == nil || c.info.User == nil || userID == "" || userID != c.info.User.ID || timestamp == "" {
		return false
	}

	chID := strings.ToUpper(channelID)

	c.RLock()
	matched := slices.Contains(c.recentSent[chID], timestamp)
	inFlight := c.sending[chID] > 0
	c.RUnlock()

	if matched {
		return true
	}

	if !inFlight {
		return false
	}

	return c.waitOwnEcho(chID, timestamp)
}

func (c *Client) waitOwnEcho(chID, timestamp string) bool {
	timer := time.NewTimer(ownEchoWaitTimeout)
	defer timer.Stop()

	for {
		c.Lock()

		if slices.Contains(c.recentSent[chID], timestamp) {
			c.Unlock()

			return true
		}

		if c.sending[chID] == 0 {
			c.Unlock()

			return false
		}

		if c.sentCh == nil {
			c.sentCh = make(chan struct{})
		}

		waitCh := c.sentCh
		c.Unlock()

		select {
		case <-waitCh:
		case <-timer.C:
			return false
		}
	}
}

func (c *Client) GetConversationHistory(ctx context.Context, channelID, oldest string, limit int) ([]slack.Message, error) {
	var all []slack.Message

	cursor := ""

	for {
		pageLimit := 100
		if limit > 0 && limit-len(all) < pageLimit {
			pageLimit = limit - len(all)
		}

		resp, err := c.sc.GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: channelID,
			Oldest:    oldest,
			Limit:     pageLimit,
			Cursor:    cursor,
		})
		if err != nil {
			return nil, err
		}

		all = append(all, resp.Messages...)

		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" || (limit > 0 && len(all) >= limit) {
			break
		}

		cursor = resp.ResponseMetaData.NextCursor
	}

	slices.Reverse(all)

	return all, nil
}

func (c *Client) SearchMessages(ctx context.Context, query string, count int) ([]slack.SearchMessage, error) {
	if count <= 0 {
		count = 100
	}

	res, err := c.sc.SearchMessagesContext(ctx, query, slack.SearchParameters{
		Sort:          "timestamp",
		SortDirection: "desc",
		Count:         count,
	})
	if err != nil {
		return nil, err
	}

	matches := slices.Clone(res.Matches)
	slices.Reverse(matches)

	return matches, nil
}

func (c *Client) SendTyping(channelID string) {
	if c.rtm == nil || channelID == "" {
		return
	}

	c.rtm.SendMessage(c.rtm.NewTypingMessage(strings.ToUpper(channelID)))
}

func (c *Client) GetFileInfo(ctx context.Context, fileID string) (*slack.File, error) {
	if c.sc == nil || fileID == "" {
		return nil, errors.New("slackclient: file info unavailable")
	}

	file, _, _, err := c.sc.GetFileInfoContext(ctx, fileID, 0, 0)
	if err != nil {
		return nil, err
	}

	return file, nil
}
