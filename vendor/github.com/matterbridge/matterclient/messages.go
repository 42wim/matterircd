package matterclient

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

func (m *Client) CreatePost(ctx context.Context, post *model.Post) (*model.Post, error) {
	if post.UserId == "" && m.User != nil {
		post.UserId = m.User.Id
	}

	// Proactive RootId resolution: if RootId points to a reply, flatten to root post
	if post.RootId != "" && m.postCache != nil {
		if cached, ok := m.postCache.Get(post.RootId); ok && cached.RootId != "" {
			post.RootId = cached.RootId
		}
	}

	channelName := m.GetCachedChannelName(post.ChannelId)
	userName := m.GetCachedUserName(post.UserId)
	retryCount := 0

	for {
		m.apiLogger.Warnf("CreatePost: User: %s (%s), Channel: %s (%s) #%d", userName, post.UserId, channelName, post.ChannelId, retryCount)

		res, resp, err := m.Client.CreatePost(ctx, post)
		if err == nil {
			if m.postCache != nil {
				m.postCache.Add(res.Id, res)
			}

			return res, nil
		}

		// Reactive RootId resolution: if the API rejected it because RootId wasn't the root post
		if post.RootId != "" && resp != nil && resp.StatusCode == http.StatusBadRequest {
			rootPost, gErr := m.GetPost(ctx, post.RootId)
			if gErr == nil && rootPost.RootId != "" {
				post.RootId = rootPost.RootId
				continue
			}
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreatePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return nil, err
	}
}

func (m *Client) DeleteMessage(ctx context.Context, postID string) error {
	retryCount := 0
	for {
		m.apiLogger.Warnf("DeleteMessage: PostID: %s #%d", postID, retryCount)

		resp, err := m.Client.DeletePost(ctx, postID)
		if err == nil {
			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "DeletePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("DeleteMessage failed for %s: %v", postID, err)
		return err
	}
}

func (m *Client) EditMessage(ctx context.Context, postID string, text string) (string, error) {
	post := &model.Post{Message: text, Id: postID}

	retryCount := 0
	for {
		m.apiLogger.Warnf("EditMessage: PostID: %s #%d", postID, retryCount)

		res, resp, err := m.Client.UpdatePost(ctx, postID, post)
		if err == nil {
			return res.Id, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "UpdatePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("EditMessage failed for %s: %v", postID, err)

		return "", err
	}
}

// GetFileInfo fetches a single file's metadata from the Mattermost API with retry logic
func (m *Client) GetFileInfo(ctx context.Context, fileID string) (*model.FileInfo, error) {
	retryCount := 0
	for {
		m.apiLogger.Warnf("GetFileInfo: FileID: %s #%d", fileID, retryCount)

		res, resp, err := m.Client.GetFileInfo(ctx, fileID)
		if err == nil {
			return res, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetFileInfo", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetFileInfo failed for %s: %v", fileID, err)

		return nil, err
	}
}

// GetFilesInfo replaces GetFileLinks, retrieving both the URL and file metadata
func (m *Client) GetFilesInfo(ctx context.Context, fileIDs []string) []*FileInfo {
	uriScheme := schemeHTTPS
	if m.NoTLS {
		uriScheme = schemeHTTP
	}

	var output []*FileInfo

	for _, f := range fileIDs {
		info := &FileInfo{}

		link := m.GetPublicLink(ctx, f)
		if link != "" {
			info.URL = link
		} else {
			// public links is probably disabled or failed, create the link ourselves
			info.URL = uriScheme + m.Server + model.APIURLSuffix + "/files/" + f
		}

		// Fetch the file metadata using the Mattermost API client with retry logic
		fileMeta, err := m.GetFileInfo(ctx, f)
		if err == nil && fileMeta != nil {
			info.Name = fileMeta.Name
			info.Size = fileMeta.Size
		} else {
			info.Name = info.URL // Fallback to URL if the API fails
		}

		output = append(output, info)
	}

	return output
}

// GetFilesInfoFromPost extracts file information using embedded metadata from the post,
// falling back to GetFilesInfo only when the server omits post.Metadata.Files.
func (m *Client) GetFilesInfoFromPost(ctx context.Context, p *model.Post) []*FileInfo {
	if p == nil || len(p.FileIds) == 0 {
		return nil
	}

	uriScheme := schemeHTTPS
	if m.NoTLS {
		uriScheme = schemeHTTP
	}

	// Use metadata already populated by the Mattermost server
	if p.Metadata != nil && len(p.Metadata.Files) > 0 {
		output := make([]*FileInfo, 0, len(p.Metadata.Files))

		for _, f := range p.Metadata.Files {
			if f == nil {
				continue
			}

			output = append(output, &FileInfo{
				Name: f.Name,
				Size: f.Size,
				URL:  uriScheme + m.Server + model.APIURLSuffix + "/files/" + f.Id,
			})
		}

		if len(output) == len(p.FileIds) {
			return output
		}
	}

	// Fallback if server did not populate post.Metadata.Files
	return m.GetFilesInfo(ctx, p.FileIds)
}

func (m *Client) GetFileLinks(ctx context.Context, filenames []string) []string {
	uriScheme := schemeHTTPS
	if m.NoTLS {
		uriScheme = schemeHTTP
	}

	var output []string

	for _, f := range filenames {
		link := m.GetPublicLink(ctx, f)
		if link != "" {
			output = append(output, link)
		} else {
			// public links is probably disabled or failed, create the link ourselves
			output = append(output, uriScheme+m.Credentials.Server+model.APIURLSuffix+"/files/"+f)
		}
	}

	return output
}

func (m *Client) GetPost(ctx context.Context, postID string) (*model.Post, error) {
	// Check LRU cache first
	if p, ok := m.postCache.Get(postID); ok {
		return p, nil
	}

	// Fallback to REST API
	retryCount := 0
	for {
		m.apiLogger.Warnf("GetPost: PostID: %s #%d", postID, retryCount)

		post, resp, err := m.Client.GetPost(ctx, postID, "")
		if err == nil {
			if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
				m.postCache.Add(post.Id, post)
			}

			return post, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetPost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPost failed for %s: %v", postID, err)
		return nil, err
	}
}

func (m *Client) GetPosts(ctx context.Context, channelID string, limit int) *model.PostList {
	const batchSize = mattermostPerPageMax

	if limit <= 0 {
		limit = 60
	}

	finalPostList := &model.PostList{
		Order: []string{},
		Posts: make(map[string]*model.Post),
	}

	channelName := m.GetCachedChannelName(channelID)
	idx := 0
	retryCount := 0
	fetched := 0

	for fetched < limit {
		// Figure out how many posts to fetch in this batch.
		// It will be 200, unless we are close to the limit.
		currentBatchSize := batchSize
		if limit-fetched < batchSize {
			currentBatchSize = limit - fetched
		}

		m.apiLogger.Warnf("GetPosts: Channel: %s (%s), Page: %d, PerPage: %d #%d", channelName, channelID, idx, currentBatchSize, retryCount)
		res, resp, err := m.Client.GetPostsForChannel(ctx, channelID, idx, currentBatchSize, "", false, false)
		if err != nil {
			shouldRetry, hErr := m.HandleRetry(ctx, "GetPostsForChannel", err, retryCount, 10, resp)
			if hErr == nil && shouldRetry {
				retryCount++
				continue
			}

			m.logger.Errorf("GetPostsForChannel failed for %s (%s) at page %d: %v", channelName, channelID, idx, err)
			if len(finalPostList.Order) == 0 {
				return nil
			}
			return finalPostList
		}
		retryCount = 0

		if res != nil {
			finalPostList.Order = append(finalPostList.Order, res.Order...)
			for postID, post := range res.Posts {
				finalPostList.Posts[postID] = post
				if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
					m.postCache.Add(postID, post)
				}
			}

			fetched += len(res.Order)

			if len(res.Order) < currentBatchSize {
				break
			}
		} else {
			break
		}

		idx++
	}

	return finalPostList
}

func (m *Client) GetPostThread(ctx context.Context, postID string) *model.PostList {
	opts := model.GetPostsOptions{
		CollapsedThreads: false,
		Direction:        "up",
	}

	retryCount := 0
	for {
		m.apiLogger.Warnf("GetPostThread: PostID: %s #%d", postID, retryCount)
		res, resp, err := m.Client.GetPostThreadWithOpts(ctx, postID, "", opts)
		if err == nil {
			if res == nil || m.postCache == nil {
				return res
			}

			for _, post := range res.Posts {
				if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
					m.postCache.Add(post.Id, post)
				}
			}

			return res
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetPostThread", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPostThread failed for %s: %v", postID, err)
		return nil
	}
}

func (m *Client) GetPostsSince(ctx context.Context, channelID string, time int64) *model.PostList {
	m.Users.mu.RLock()
	ch, ok := m.Users.channelData[channelID]
	m.Users.mu.RUnlock()

	if ok && ch != nil {
		// If channel is archived, or hasn't had new posts, bail early!
		if ch.DeleteAt > 0 || (ch.LastPostAt > 0 && ch.LastPostAt <= time) {
			return &model.PostList{
				Order: []string{},
				Posts: make(map[string]*model.Post),
			}
		}
	}

	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("GetPostsSince: Channel: %s (%s), Since: %d #%d", channelName, channelID, time, retryCount)
		res, resp, err := m.Client.GetPostsSince(ctx, channelID, time, false)
		if err == nil {
			if res == nil || m.postCache == nil {
				return res
			}

			for _, post := range res.Posts {
				if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
					m.postCache.Add(post.Id, post)
				}
			}

			return res
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "GetPostsSince", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("GetPostsSince failed for %s (%s): %v", channelName, channelID, err)
		return nil
	}
}

func (m *Client) GetPublicLink(ctx context.Context, filename string) string {
	retryCount := 0
	for {
		m.apiLogger.Infof("GetPublicLink: file: %s #%d", filename, retryCount)

		res, resp, err := m.Client.GetFileLink(ctx, filename)
		if err == nil {
			return res
		}

		// If public links are disabled, Mattermost returns a 501 Not Implemented or 400 Bad Request.
		// HandleRetry will correctly see the 4xx/501 error, return false, and break the loop gracefully.
		shouldRetry, hErr := m.HandleRetry(ctx, "GetFileLink", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		return ""
	}
}

func (m *Client) GetPublicLinks(ctx context.Context, filenames []string) []string {
	var output []string

	for _, f := range filenames {
		link := m.GetPublicLink(ctx, f)
		if link != "" {
			output = append(output, link)
		}
	}

	return output
}

func (m *Client) PatchPost(ctx context.Context, postID string, patch *model.PostPatch) (*model.Post, error) {
	retryCount := 0
	for {
		m.apiLogger.Warnf("PatchPost: PostID: %s #%d", postID, retryCount)

		post, resp, err := m.Client.PatchPost(ctx, postID, patch)
		if err == nil {
			if m.postCache != nil {
				m.postCache.Add(post.Id, post)
			}
			return post, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "PatchPost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("PatchPost failed for %s: %v", postID, err)
		return nil, err
	}
}

func (m *Client) PostMessage(ctx context.Context, channelID string, text string, rootID string) (string, error) {
	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    rootID,
	}

	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("PostMessage: Channel: %s (%s), RootID: %s #%d", channelName, channelID, rootID, retryCount)
		res, resp, err := m.Client.CreatePost(ctx, post)
		if err == nil {
			if m.postCache != nil {
				m.postCache.Add(res.Id, res)
			}
			return res.Id, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreatePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("PostMessage failed for %s (%s) %s: %v", channelName, channelID, rootID, err)

		return "", err
	}
}

func (m *Client) PostMessageWithFiles(ctx context.Context, channelID string, text string, rootID string, fileIds []string) (string, error) {
	post := &model.Post{
		ChannelId: channelID,
		Message:   text,
		RootId:    rootID,
		FileIds:   fileIds,
	}

	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("PostMessageWithFiles: Channel: %s (%s), RootID %s #%d", channelName, channelID, rootID, retryCount)
		res, resp, err := m.Client.CreatePost(ctx, post)
		if err == nil {
			if m.postCache != nil {
				m.postCache.Add(res.Id, res)
			}
			return res.Id, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreatePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("PostMessageWithFiles failed for %s (%s) %s: %v", channelName, channelID, rootID, err)

		return "", err
	}
}

func (m *Client) SearchPosts(ctx context.Context, query string) *model.PostList {
	retryCount := 0
	for {
		m.apiLogger.Warnf("SearchPosts: query: %s #%d", query, retryCount)
		res, resp, err := m.Client.SearchPosts(ctx, m.Team.ID, query, false)
		if err == nil {
			if res == nil || m.postCache == nil {
				return res
			}

			for _, post := range res.Posts {
				if m.postCache != nil && !strings.HasPrefix(post.Type, model.PostSystemMessagePrefix) {
					m.postCache.Add(post.Id, post)
				}
			}

			return res
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "SearchPosts", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		return nil
	}
}

// SendDirectMessage sends a direct message to specified user
func (m *Client) SendDirectMessage(ctx context.Context, toUserID string, msg string, rootID string) error {
	return m.SendDirectMessageProps(ctx, toUserID, msg, rootID, nil)
}

func (m *Client) SendDirectMessageProps(ctx context.Context, toUserID string, msg string, rootID string, props map[string]interface{}) error {
	m.logger.Debugf("SendDirectMessage to %s, msg %s", toUserID, msg)

	dchannel, err := m.CreateDirectChannel(ctx, m.User.Id, toUserID)
	if err != nil {
		m.logger.Errorf("CreateDirectChannel to %s failed: %v", toUserID, err)
		return err
	}

	// build & send the message
	msg = strings.ReplaceAll(msg, "\r", "")
	post := &model.Post{
		ChannelId: dchannel.Id,
		Message:   msg,
		RootId:    rootID,
	}
	post.SetProps(props)

	channelName := m.GetCachedChannelName(post.ChannelId)
	userName := m.GetCachedUserName(post.UserId)
	retryCount := 0

	for {
		m.apiLogger.Warnf("SendDirectMessageProps: CreatePost: User: %s (%s), Channel: %s (%s) #%d", userName, post.UserId, channelName, post.ChannelId, retryCount)

		res, resp, err := m.Client.CreatePost(ctx, post)
		if err == nil {
			if m.postCache != nil {
				m.postCache.Add(res.Id, res)
			}
			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "CreatePost", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++
			continue
		}

		m.logger.Errorf("CreatePost failed for channel %s: %v", post.ChannelId, err)
		return err
	}
}

func (m *Client) SaveReaction(ctx context.Context, reaction *model.Reaction) (*model.Reaction, error) {
	retryCount := 0
	for {
		m.apiLogger.Warnf("SaveReaction: PostID: %s, Emoji: %s #%d", reaction.PostId, reaction.EmojiName, retryCount)

		res, resp, err := m.Client.SaveReaction(ctx, reaction)
		if err == nil {
			return res, nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "SaveReaction", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("SaveReaction failed for %s (emoji: %s): %v", reaction.PostId, reaction.EmojiName, err)

		return nil, err
	}
}

func (m *Client) DeleteReaction(ctx context.Context, reaction *model.Reaction) error {
	retryCount := 0
	for {
		m.apiLogger.Warnf("DeleteReaction: PostID: %s, Emoji: %s #%d", reaction.PostId, reaction.EmojiName, retryCount)

		resp, err := m.Client.DeleteReaction(ctx, reaction)
		if err == nil {
			return nil
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "DeleteReaction", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("DeleteReaction failed for %s (emoji: %s): %v", reaction.PostId, reaction.EmojiName, err)

		return err
	}
}

func (m *Client) UploadFile(ctx context.Context, data []byte, channelID string, filename string) (string, error) {
	channelName := m.GetCachedChannelName(channelID)
	retryCount := 0

	for {
		m.apiLogger.Warnf("UploadFile: Channel: %s (%s), filename: %s #%d", channelName, channelID, filename, retryCount)

		f, resp, err := m.Client.UploadFile(ctx, data, channelID, filename)
		if err == nil && f != nil && len(f.FileInfos) > 0 {
			return f.FileInfos[0].Id, nil
		}

		if err == nil {
			err = errors.New("file upload succeeded but FileInfos was empty")
		}

		shouldRetry, hErr := m.HandleRetry(ctx, "UploadFile", err, retryCount, 10, resp)
		if hErr == nil && shouldRetry {
			retryCount++

			continue
		}

		m.logger.Errorf("UploadFile failed for %s: %v", filename, err)

		return "", err
	}
}

func (m *Client) parseActionPost(ctx context.Context, rmsg *Message) {
	var data *model.Post
	var postStr string

	if pPtr, ok := rmsg.Raw.GetData()["post"].(*model.Post); ok {
		data = pPtr
	} else if pStr, ok := rmsg.Raw.GetData()["post"].(string); ok && pStr != "" {
		postStr = pStr
		data = &model.Post{}

		err := json.Unmarshal([]byte(postStr), data)
		if err != nil {
			m.logger.Errorf("failed to unmarshal post: %v", err)
			return
		}
	} else {
		m.logger.Error("payload 'post' was missing or invalid")
		return
	}

	// We combine EventType, ID, and UpdateAt.
	// This uniquely separates creations, edits, and deletions without any slow hashing!
	var dedupKey string
	if data != nil && data.Id != "" {
		dedupKey = string(rmsg.Raw.EventType()) + ":" + data.Id + ":" + strconv.FormatInt(data.UpdateAt, 10)
	} else if postStr != "" {
		// Absolute last resort fallback
		dedupKey = digestString(postStr)
	}

	if ok, _ := m.lruCache.ContainsOrAdd(dedupKey, true); ok {
		m.logger.Debugf("message %s in cache, not processing again", dedupKey)
		rmsg.Text = ""
		return
	}

	// we don't have the user, refresh the userlist
	if m.GetUser(ctx, data.UserId) == nil {
		m.logger.Infof("User '%v' is not known, ignoring message '%#v'", data.UserId, data)
		return
	}

	rmsg.Username = m.GetUserName(ctx, data.UserId)
	rmsg.Channel = m.GetChannelName(ctx, data.ChannelId)
	rmsg.UserID = data.UserId
	rmsg.Type = data.Type

	teamid, _ := rmsg.Raw.GetData()["team_id"].(string)
	// edit messsages have no team_id for some reason
	if teamid == "" {
		// we can find the team_id from the channelid
		teamid = m.GetChannelTeamID(ctx, data.ChannelId)
		rmsg.Raw.GetData()["team_id"] = teamid
	}

	if teamid != "" {
		rmsg.Team = m.GetTeamName(ctx, teamid)
	}

	// direct message
	if rmsg.Raw.GetData()["channel_type"] == "D" {
		rmsg.Channel = m.GetUser(ctx, data.UserId).Username
	}

	rmsg.Text = data.Message
	rmsg.Post = data
}

func (m *Client) parseMessage(ctx context.Context, rmsg *Message) {
	switch rmsg.Raw.EventType() {
	case model.WebsocketEventPosted, model.WebsocketEventPostEdited, model.WebsocketEventPostDeleted:
		m.parseActionPost(ctx, rmsg)
	case "user_updated":
		if user, ok := rmsg.Raw.GetData()["user"].(*model.User); ok {
			m.UpdateUser(user)
		}
	case "group_added":
		if err := m.UpdateChannels(ctx); err != nil {
			m.logger.Errorf("failed to update channels: %#v", err)
		}
	}
}

func (m *Client) parseResponse(rmsg *model.WebSocketResponse) {
	if rmsg == nil || rmsg.Data == nil {
		return
	}

	text, ok := rmsg.Data["text"].(string)
	if ok && text == "pong" {
		m.logger.Tracef("getting response: %#v", rmsg)

		return
	}

	m.logger.Debugf("getting response: %#v", rmsg)

	for userID, val := range rmsg.Data {
		statusStr, isStr := val.(string)
		if !isStr || statusStr == "" {
			continue
		}

		switch statusStr {
		case model.StatusOnline, model.StatusAway, model.StatusDnd, model.StatusOutOfOffice, model.StatusOffline:
			if len(userID) == 26 {
				m.SetUserStatus(userID, statusStr)
			}
		}
	}
}

func digestString(s string) string {
	if len(s) == 0 {
		return ""
	}

	sum := md5.Sum([]byte(s)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}
