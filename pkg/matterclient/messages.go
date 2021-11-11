package matterclient

import (
	"strings"

	"github.com/mattermost/mattermost-server/v5/model"
)

func (m *Client) parseResponse(rmsg *model.WebSocketResponse) {
	m.logger.Debugf("getting response: %#v", rmsg)
}

func (m *Client) DeleteMessage(postID string) error {
	_, resp := m.Client.DeletePost(postID)
	if resp.Error != nil {
		return resp.Error
	}

	return nil
}

func (m *Client) EditMessage(postID string, text string) (string, error) {
	post := &model.Post{Message: text, Id: postID}

	res, resp := m.Client.UpdatePost(postID, post)
	if resp.Error != nil {
		return "", resp.Error
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
		res, resp := m.Client.GetFileLink(f)
		if resp.Error != nil {
			// public links is probably disabled, create the link ourselves
			output = append(output, uriScheme+m.Credentials.Server+model.API_URL_SUFFIX_V4+"/files/"+f)

			continue
		}

		output = append(output, res)
	}

	return output
}

func (m *Client) GetPosts(channelID string, limit int) *model.PostList {
	for {
		res, resp := m.Client.GetPostsForChannel(channelID, 0, limit, "", true)
		if resp.Error == nil {
			return res
		}

		if err := m.HandleRatelimit("GetPostsForChannel", resp); err != nil {
			return nil
		}
	}
}

func (m *Client) GetPostsSince(channelID string, time int64) *model.PostList {
	for {
		res, resp := m.Client.GetPostsSince(channelID, time, true)
		if resp.Error == nil {
			return res
		}

		if err := m.HandleRatelimit("GetPostsSince", resp); err != nil {
			return nil
		}
	}
}

func (m *Client) GetPublicLink(filename string) string {
	res, resp := m.Client.GetFileLink(filename)
	if resp.Error != nil {
		return ""
	}

	return res
}

func (m *Client) GetPublicLinks(filenames []string) []string {
	var output []string

	for _, f := range filenames {
		res, resp := m.Client.GetFileLink(f)
		if resp.Error != nil {
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

	for {
		res, resp := m.Client.CreatePost(post)
		if resp.Error == nil {
			return res.Id, nil
		}

		if err := m.HandleRatelimit("CreatePost", resp); err != nil {
			return "", err
		}
	}
}

func (m *Client) PostMessageWithFiles(channelID string, text string, rootID string, fileIds []string) (string, error) {
	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    rootID,
		FileIds:   fileIds,
	}

	for {
		res, resp := m.Client.CreatePost(post)
		if resp.Error == nil {
			return res.Id, nil
		}

		if err := m.HandleRatelimit("CreatePost", resp); err != nil {
			return "", err
		}
	}
}

func (m *Client) SearchPosts(query string) *model.PostList {
	res, resp := m.Client.SearchPosts(m.Team.ID, query, false)
	if resp.Error != nil {
		return nil
	}

	return res
}

// SendDirectMessage sends a direct message to specified user
func (m *Client) SendDirectMessage(toUserID string, msg string, rootID string) error {
	return m.SendDirectMessageProps(toUserID, msg, rootID, nil)
}

func (m *Client) SendDirectMessageProps(toUserID string, msg string, rootID string, props map[string]interface{}) error {
	m.logger.Debugf("SendDirectMessage to %s, msg %s", toUserID, msg)

	for {
		// create DM channel (only happens on first message)
		_, resp := m.Client.CreateDirectChannel(m.User.Id, toUserID)
		if resp.Error == nil {
			break
		}

		if err := m.HandleRatelimit("CreateDirectChannel", resp); err != nil {
			m.logger.Debugf("SendDirectMessage to %#v failed: %s", toUserID, err)

			return err
		}
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

	for {
		_, resp := m.Client.CreatePost(post)
		if resp.Error == nil {
			return nil
		}

		if err := m.HandleRatelimit("CreatePost", resp); err != nil {
			return err
		}
	}
}

func (m *Client) UploadFile(data []byte, channelID string, filename string) (string, error) {
	f, resp := m.Client.UploadFile(data, channelID, filename)
	if resp.Error != nil {
		return "", resp.Error
	}

	return f.FileInfos[0].Id, nil
}
