package matterclient

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

func (m *Client) parseResponse(rmsg *model.WebSocketResponse) {
	m.logger.Debugf("getting response: %#v", rmsg)
}

func (m *Client) CreatePost(post *model.Post) (*model.Post, error) {
	retryCount := 0
	for {
		res, resp, err := m.Client.CreatePost(context.TODO(), post)
		if err == nil {
			return res, nil
		}

		shouldRetry, hErr := m.HandleRetry("CreatePost", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return nil, err
	}
}

func (m *Client) DeleteMessage(postID string) error {
	_, err := m.Client.DeletePost(context.TODO(), postID)
	if err != nil {
		return err
	}

	return nil
}

func (m *Client) EditMessage(postID string, text string) (string, error) {
	post := &model.Post{Message: text, Id: postID}

	res, _, err := m.Client.UpdatePost(context.TODO(), postID, post)
	if err != nil {
		return "", err
	}

	return res.Id, nil
}

func (m *Client) GetFileLinks(filenames []string) []string {
	uriScheme := "https://"
	if m.NoTLS {
		uriScheme = "http://"
	}

	var output []string

	for _, f := range filenames {
		res, _, err := m.Client.GetFileLink(context.TODO(), f)
		if err != nil {
			// public links is probably disabled, create the link ourselves
			output = append(output, uriScheme+m.Credentials.Server+model.APIURLSuffix+"/files/"+f)

			continue
		}

		output = append(output, res)
	}

	return output
}

func (m *Client) GetPost(postID string) (*model.Post, error) {
	retryCount := 0
	for {
		res, resp, err := m.Client.GetPost(context.TODO(), postID, "")
		if err == nil {
			return res, nil
		}

		shouldRetry, hErr := m.HandleRetry("GetPost", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPost failed for %s: %v", postID, err)
		return nil, err
	}
}

func (m *Client) GetPosts(channelID string, limit int) *model.PostList {
	retryCount := 0
	for {
		res, resp, err := m.Client.GetPostsForChannel(context.TODO(), channelID, 0, limit, "", false, false)
		if err == nil {
			return res
		}

		shouldRetry, hErr := m.HandleRetry("GetPostsForChannel", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPostsForChannel failed for %s: %v", channelID, err)
		return nil
	}
}

func (m *Client) GetPostThread(postID string) *model.PostList {
	opts := model.GetPostsOptions{
		CollapsedThreads: false,
		Direction:        "up",
	}

	retryCount := 0
	for {
		res, resp, err := m.Client.GetPostThreadWithOpts(context.TODO(), postID, "", opts)
		if err == nil {
			return res
		}

		shouldRetry, hErr := m.HandleRetry("GetPostThread", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPostThread failed for %s: %v", postID, err)
		return nil
	}
}

func (m *Client) GetPostsSince(channelID string, time int64) *model.PostList {
	retryCount := 0
	for {
		res, resp, err := m.Client.GetPostsSince(context.TODO(), channelID, time, false)
		if err == nil {
			return res
		}

		shouldRetry, hErr := m.HandleRetry("GetPostsSince", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPostsSince failed for %s: %v", channelID, err)
		return nil
	}
}

func (m *Client) GetPublicLink(filename string) string {
	res, _, err := m.Client.GetFileLink(context.TODO(), filename)
	if err != nil {
		return ""
	}

	return res
}

func (m *Client) GetPublicLinks(filenames []string) []string {
	var output []string

	for _, f := range filenames {
		res, _, err := m.Client.GetFileLink(context.TODO(), f)
		if err != nil {
			continue
		}

		output = append(output, res)
	}

	return output
}

func (m *Client) PostMessage(channelID string, text string, rootID string) (string, error) {
	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    rootID,
	}

	retryCount := 0
	for {
		res, resp, err := m.Client.CreatePost(context.TODO(), post)
		if err == nil {
			return res.Id, nil
		}

		shouldRetry, hErr := m.HandleRetry("CreatePost", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return "", err
	}
}

func (m *Client) PostMessageWithFiles(channelID string, text string, rootID string, fileIds []string) (string, error) {
	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    rootID,
		FileIds:   fileIds,
	}

	retryCount := 0
	for {
		res, resp, err := m.Client.CreatePost(context.TODO(), post)
		if err == nil {
			return res.Id, nil
		}

		shouldRetry, hErr := m.HandleRetry("CreatePost", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return "", err
	}
}

func (m *Client) SearchPosts(query string) *model.PostList {
	retryCount := 0
	for {
		res, resp, err := m.Client.SearchPosts(context.TODO(), m.Team.ID, query, false)
		if err == nil {
			return res
		}

		shouldRetry, hErr := m.HandleRetry("SearchPosts", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return nil
	}
}

// SendDirectMessage sends a direct message to specified user
func (m *Client) SendDirectMessage(toUserID string, msg string, rootID string) error {
	return m.SendDirectMessageProps(toUserID, msg, rootID, nil)
}

func (m *Client) SendDirectMessageProps(toUserID string, msg string, rootID string, props map[string]interface{}) error {
	m.logger.Debugf("SendDirectMessage to %s, msg %s", toUserID, msg)

	retryCount := 0
	for {
		// create DM channel (only happens on first message)
		_, resp, err := m.Client.CreateDirectChannel(context.TODO(), m.User.Id, toUserID)
		if err == nil {
			break
		}

		shouldRetry, hErr := m.HandleRetry("CreateDirectChannel", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("CreateDirectChannel to %s failed: %v", toUserID, err)
		return err
	}

	channelName := model.GetDMNameFromIds(toUserID, m.User.Id)

	// update our channels
	if err := m.UpdateChannels(); err != nil {
		m.logger.Errorf("failed to update channels: %#v", err)
	}

	// build & send the message
	msg = strings.ReplaceAll(msg, "\r", "")
	post := &model.Post{
		ChannelId: m.GetChannelID(channelName, m.Team.ID),
		Message:   msg,
		RootId:    rootID,
	}

	post.SetProps(props)

	retryCount = 0
	for {
		_, resp, err := m.Client.CreatePost(context.TODO(), post)
		if err == nil {
			return nil
		}

		shouldRetry, hErr := m.HandleRetry("CreatePost", retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("CreatePost failed for channel %s: %v", post.ChannelId, err)
		return err
	}
}

func (m *Client) UploadFile(data []byte, channelID string, filename string) (string, error) {
	f, _, err := m.Client.UploadFile(context.TODO(), data, channelID, filename)
	if err != nil {
		return "", err
	}

	return f.FileInfos[0].Id, nil
}

func (m *Client) parseActionPost(rmsg *Message) {
	// add post to cache, if it already exists don't relay this again.
	// this should fix reposts
	if ok, _ := m.lruCache.ContainsOrAdd(digestString(rmsg.Raw.GetData()["post"].(string)), true); ok && rmsg.Raw.EventType() != model.WebsocketEventPostDeleted {
		m.logger.Debugf("message %#v in cache, not processing again", rmsg.Raw.GetData()["post"].(string))
		rmsg.Text = ""

		return
	}

	var data model.Post
	postStr, ok := rmsg.Raw.GetData()["post"].(string)
	if !ok {
		m.logger.Error("payload 'post' was missing or not a string")
		return
	}
	if err := json.Unmarshal([]byte(postStr), &data); err != nil {
		m.logger.Errorf("failed to unmarshal post: %v", err)
		return
	}
	// we don't have the user, refresh the userlist
	if m.GetUser(context.TODO(), data.UserId) == nil {
		m.logger.Infof("User '%v' is not known, ignoring message '%#v'",
			data.UserId, data)
		return
	}

	rmsg.Username = m.GetUserName(data.UserId)
	rmsg.Channel = m.GetChannelName(data.ChannelId)
	rmsg.UserID = data.UserId
	rmsg.Type = data.Type
	teamid, _ := rmsg.Raw.GetData()["team_id"].(string)
	// edit messsages have no team_id for some reason
	if teamid == "" {
		// we can find the team_id from the channelid
		teamid = m.GetChannelTeamID(data.ChannelId)
		rmsg.Raw.GetData()["team_id"] = teamid
	}

	if teamid != "" {
		rmsg.Team = m.GetTeamName(teamid)
	}
	// direct message
	if rmsg.Raw.GetData()["channel_type"] == "D" {
		rmsg.Channel = m.GetUser(context.TODO(), data.UserId).Username
	}

	rmsg.Text = data.Message
	rmsg.Post = &data
}

func (m *Client) parseMessage(rmsg *Message) {
	switch rmsg.Raw.EventType() {
	case model.WebsocketEventPosted, model.WebsocketEventPostEdited, model.WebsocketEventPostDeleted:
		m.parseActionPost(rmsg)
	case "user_updated":
		if user, ok := rmsg.Raw.GetData()["user"].(*model.User); ok {
			m.UpdateUser(user)
		}
	case "group_added":
		if err := m.UpdateChannels(); err != nil {
			m.logger.Errorf("failed to update channels: %#v", err)
		}
	}
}

func digestString(s string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(s))) //nolint:gosec
}
