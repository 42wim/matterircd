package slackclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	clientCountsTimeout    = 15 * time.Second
	edgeChannelsTimeout    = 10 * time.Second
	edgeChannelsSearchMax  = 30
	conversationPageLimit  = 200
	maxIMInfoFallback      = 50
	convTypePublicChannel  = "public_channel"
)

func isActiveConversation(ch slack.Channel) bool {
	return ch.IsMember || (ch.IsIM && ch.IsOpen && !ch.IsUserDeleted)
}

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
	if ch.Name != "" && !ch.IsIM {
		c.channels[ch.ID] = "#" + ch.Name
	}

	if ch.IsIM && ch.User != "" {
		c.dmChannels[ch.User] = ch.ID
	}

	c.channelData[ch.ID] = ch
	c.joinedChannels[ch.ID] = isActiveConversation(ch)
	c.Unlock()
}

// InvalidateChannels clears cached conversation freshness so the next
// GetConversations/GetChannels call re-fetches current channel metadata
// (such as latest timestamps) on reconnect without flushing membership caches.
func (c *Client) InvalidateChannels() {
	c.Lock()
	c.channelsFetchedAt = 0
	c.Unlock()
}

// InvalidateCaches clears cached conversation freshness, public channel sync
// state, and membership TTL maps so subsequent lookups re-fetch from the Slack
// API after a long disconnect.
func (c *Client) InvalidateCaches() {
	c.Lock()
	c.channelsFetchedAt = 0
	c.publicChannelsSynced = false
	clear(c.channelMembers)
	clear(c.userChannels)
	c.Unlock()
}

func isSlackChannelID(s string) bool {
	if len(s) < 9 {
		return false
	}

	if s[0] != 'C' && s[0] != 'G' {
		return false
	}

	hasDigit := false

	for i := 1; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= '0' && ch <= '9':
			hasDigit = true
		case ch >= 'A' && ch <= 'Z':
		default:
			return false
		}
	}

	return hasDigit
}

func (c *Client) JoinChannel(ctx context.Context, channelName string) (string, string, error) {
	channelID := strings.TrimSpace(channelName)
	if resolved := c.GetCachedChannelID(channelID); resolved != "" {
		channelID = resolved
	} else if !isSlackChannelID(channelID) {
		if resolved := c.GetChannelID(ctx, channelID); resolved != "" {
			channelID = resolved
		}
	}

	c.RLock()
	existing, hasExisting := c.channelData[channelID]
	alreadyJoined := c.joinedChannels[channelID]
	c.RUnlock()

	if hasExisting && (alreadyJoined || existing.IsMember) && (existing.IsPrivate || existing.IsGroup || existing.IsMpIM) {
		return existing.ID, existing.Topic.Value, nil
	}

	if c.sc == nil {
		if hasExisting {
			c.SetChannelJoined(existing.ID, true)
			return existing.ID, existing.Topic.Value, nil
		}

		return "", "", errors.New("cannot join channel (+i): slackclient not connected")
	}

	mychan, _, _, err := c.sc.JoinConversationContext(ctx, channelID)
	if err != nil {
		return "", "", fmt.Errorf("cannot join channel (+i): %w", err)
	}

	mychan.IsMember = true
	c.cacheChannel(*mychan)

	return mychan.ID, mychan.Topic.Value, nil
}

func (c *Client) GetConversations() ([]slack.Channel, error) {
	return c.getConversations(context.Background())
}

func (c *Client) getConversations(ctx context.Context) ([]slack.Channel, error) {
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

	return c.refreshConversations(ctx)
}

type clientCountsEntry struct {
	ID         string          `json:"id"`
	LastRead   string          `json:"last_read"`
	Latest     json.RawMessage `json:"latest"`
	HasUnreads bool            `json:"has_unreads"`
}

type clientCountsResponse struct {
	slack.SlackResponse

	Channels []clientCountsEntry `json:"channels"`
	MPIMs    []clientCountsEntry `json:"mpims"`
	IMs      []clientCountsEntry `json:"ims"`
}

func slackLatestTS(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}

	if trimmed[0] == '"' {
		var ts string

		err := json.Unmarshal(raw, &ts)
		if err == nil {
			return ts
		}

		return ""
	}

	if trimmed[0] == '{' {
		var msg struct {
			Timestamp string `json:"ts"`
		}

		err := json.Unmarshal(raw, &msg)
		if err == nil {
			return msg.Timestamp
		}
	}

	return ""
}

func (c *Client) fetchClientCounts(ctx context.Context) (map[string]clientCountsEntry, bool) {
	c.RLock()
	hc := c.httpClient
	token := c.token
	c.RUnlock()

	if hc == nil || token == "" {
		return nil, false
	}

	form := url.Values{"token": {token}}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, slack.APIURL+"client.counts", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, false
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := hc.Do(req)
	if err != nil {
		c.logger.Debugf("client.counts: %s", err)

		return nil, false
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false
	}

	var parsed clientCountsResponse

	err = json.Unmarshal(body, &parsed)
	if err != nil || !parsed.Ok {
		c.logger.Debugf("client.counts unavailable: err=%v slack_err=%s", err, parsed.Error)

		return nil, false
	}

	counts := make(map[string]clientCountsEntry, len(parsed.Channels)+len(parsed.MPIMs)+len(parsed.IMs))
	for _, list := range [][]clientCountsEntry{parsed.Channels, parsed.MPIMs, parsed.IMs} {
		for _, entry := range list {
			if entry.ID != "" {
				counts[entry.ID] = entry
			}
		}
	}

	return counts, true
}

func applyClientCounts(channels []slack.Channel, counts map[string]clientCountsEntry) {
	for i := range channels {
		entry, ok := counts[channels[i].ID]
		if !ok {
			continue
		}

		if entry.LastRead != "" && channels[i].LastRead == "" {
			channels[i].LastRead = entry.LastRead
		}

		if ts := slackLatestTS(entry.Latest); ts != "" {
			if channels[i].Latest == nil {
				channels[i].Latest = &slack.Message{}
			}

			channels[i].Latest.Timestamp = ts
		}

		if channels[i].IsIM && !channels[i].IsUserDeleted && (entry.HasUnreads || (channels[i].Latest != nil && channels[i].Latest.Timestamp != "")) {
			channels[i].IsOpen = true
		}
	}
}

func (c *Client) enrichConversationLatest(parentCtx context.Context, channels []slack.Channel) {
	ctx, cancel := context.WithTimeout(parentCtx, clientCountsTimeout)
	defer cancel()

	if counts, ok := c.fetchClientCounts(ctx); ok {
		applyClientCounts(channels, counts)

		return
	}

	// Fallback for non-cookie tokens where client.counts is unavailable: hydrate
	// open IMs via conversations.info so offline DMs still carry Latest timestamps.
	fetched := 0

	for i := range channels {
		if fetched >= maxIMInfoFallback {
			break
		}

		ch := &channels[i]
		if !ch.IsIM || !ch.IsOpen || ch.IsUserDeleted || (ch.Latest != nil && ch.Latest.Timestamp != "") {
			continue
		}

		info, err := c.sc.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
			ChannelID: ch.ID,
		})
		if err != nil {
			c.logger.Debugf("GetConversationInfo %s: %s", ch.ID, err)

			break
		}

		fetched++

		if info != nil {
			if info.Latest != nil && info.Latest.Timestamp != "" {
				ch.Latest = info.Latest
			}

			if info.LastRead != "" {
				ch.LastRead = info.LastRead
			}
		}
	}
}

func (c *Client) fetchConversationPages(ctx context.Context, types []string) ([]slack.Channel, error) {
	var channels []slack.Channel

	params := slack.GetConversationsParameters{
		Cursor:          "",
		ExcludeArchived: true,
		Limit:           conversationPageLimit,
		Types:           types,
	}

	for {
		if ctx.Err() != nil {
			return channels, ctx.Err()
		}

		conversations, nextCursor, err := c.sc.GetConversationsContext(ctx, &params)
		if err != nil {
			if rlErr, ok := errors.AsType[*slack.RateLimitedError](err); ok {
				if !sleepContext(ctx, rlErr.RetryAfter) {
					return channels, ctx.Err()
				}

				continue
			}

			return channels, err
		}

		params.Cursor = nextCursor

		channels = append(channels, conversations...)

		if nextCursor == "" {
			break
		}
	}

	return channels, nil
}

func (c *Client) fetchJoinedConversationPages(ctx context.Context, types []string) ([]slack.Channel, error) {
	var channels []slack.Channel

	params := slack.GetConversationsForUserParameters{
		Cursor:          "",
		ExcludeArchived: true,
		Limit:           conversationPageLimit,
		Types:           types,
	}

	for {
		if ctx.Err() != nil {
			return channels, ctx.Err()
		}

		conversations, nextCursor, err := c.sc.GetConversationsForUserContext(ctx, &params)
		if err != nil {
			if rlErr, ok := errors.AsType[*slack.RateLimitedError](err); ok {
				if !sleepContext(ctx, rlErr.RetryAfter) {
					return channels, ctx.Err()
				}

				continue
			}

			return channels, err
		}

		params.Cursor = nextCursor

		channels = append(channels, conversations...)

		if nextCursor == "" {
			break
		}
	}

	return channels, nil
}

func (c *Client) RefreshConversations() ([]slack.Channel, error) {
	return c.refreshConversations(context.Background())
}

func (c *Client) refreshConversations(ctx context.Context) ([]slack.Channel, error) {
	if c.sc == nil {
		return nil, nil
	}

	if c.LazyPublicChannels {
		return c.refreshJoinedConversations(ctx)
	}

	return c.refreshAllConversations(ctx)
}

func (c *Client) refreshAllConversations(ctx context.Context) ([]slack.Channel, error) {
	channels, err := c.fetchConversationPages(ctx, []string{convTypePublicChannel, "private_channel", "mpim"})
	if err != nil {
		c.logger.Error("GetConversations", err)
	}

	// Fetch 1-on-1 DMs separately so tokens lacking im:read still load channels.
	ims, imErr := c.fetchConversationPages(ctx, []string{"im"})
	if imErr != nil {
		c.logger.Debugf("GetConversations(im): %s", imErr)
	} else {
		channels = append(channels, ims...)
	}

	c.enrichConversationLatest(ctx, channels)

	c.Lock()
	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}

		if ch.Name != "" && !ch.IsIM {
			c.channels[ch.ID] = "#" + ch.Name
		}

		if ch.IsIM && ch.User != "" {
			c.dmChannels[ch.User] = ch.ID
		}

		c.channelData[ch.ID] = ch
		c.joinedChannels[ch.ID] = isActiveConversation(ch)
	}

	if err == nil {
		c.publicChannelsSynced = true
	}

	c.channelsFetchedAt = time.Now().Unix()
	c.Unlock()

	return channels, nil
}

func (c *Client) refreshJoinedConversations(ctx context.Context) ([]slack.Channel, error) {
	channels, err := c.fetchJoinedConversationPages(ctx, []string{convTypePublicChannel, "private_channel", "mpim"})
	if err != nil {
		c.logger.Error("GetConversationsForUser", err)
	}

	// users.conversations omits is_member on some responses; every non-IM
	// channel returned for the authenticated user is joined.
	for i := range channels {
		if !channels[i].IsIM {
			channels[i].IsMember = true
		}
	}

	// Fetch 1-on-1 DMs separately so tokens lacking im:read still load channels.
	ims, imErr := c.fetchJoinedConversationPages(ctx, []string{"im"})
	if imErr != nil {
		c.logger.Debugf("GetConversationsForUser(im): %s", imErr)
	} else {
		channels = append(channels, ims...)
	}

	c.enrichConversationLatest(ctx, channels)

	return c.applyJoinedConversations(channels, err == nil, imErr == nil), nil
}

func (c *Client) applyJoinedConversations(channels []slack.Channel, reconcileNonIM, reconcileIM bool) []slack.Channel {
	seenNonIM, seenIM := collectSeenConversationIDs(channels)

	c.Lock()
	c.reconcileJoinedMembershipLocked(seenNonIM, seenIM, reconcileNonIM, reconcileIM)

	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}

		if ch.Name != "" && !ch.IsIM {
			c.channels[ch.ID] = "#" + ch.Name
		}

		if ch.IsIM && ch.User != "" {
			c.dmChannels[ch.User] = ch.ID
		}

		c.channelData[ch.ID] = ch
		c.joinedChannels[ch.ID] = isActiveConversation(ch)
	}

	if reconcileNonIM || reconcileIM {
		c.channelsFetchedAt = time.Now().Unix()
	}

	snapshot := make([]slack.Channel, 0, len(c.channelData))
	for _, ch := range c.channelData {
		snapshot = append(snapshot, ch)
	}

	c.Unlock()

	return snapshot
}

func collectSeenConversationIDs(channels []slack.Channel) (map[string]struct{}, map[string]struct{}) {
	seenNonIM := make(map[string]struct{}, len(channels))
	seenIM := make(map[string]struct{})

	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}

		if ch.IsIM {
			seenIM[ch.ID] = struct{}{}
		} else {
			seenNonIM[ch.ID] = struct{}{}
		}
	}

	return seenNonIM, seenIM
}

func (c *Client) reconcileJoinedMembershipLocked(seenNonIM, seenIM map[string]struct{}, reconcileNonIM, reconcileIM bool) {
	for id, joined := range c.joinedChannels {
		if !joined {
			continue
		}

		ch, hasCh := c.channelData[id]
		isIM := (hasCh && ch.IsIM) || strings.HasPrefix(id, "D")

		if isIM {
			if !reconcileIM {
				continue
			}

			if _, ok := seenIM[id]; !ok {
				c.joinedChannels[id] = false
			}

			continue
		}

		if !reconcileNonIM {
			continue
		}

		if _, ok := seenNonIM[id]; ok {
			continue
		}

		c.joinedChannels[id] = false

		if hasCh {
			ch.IsMember = false
			c.channelData[id] = ch
		}
	}
}

// EnsurePublicChannels populates the local channel cache with workspace public
// channels if they have not been synced yet. Already-synced caches return
// immediately so subsequent /LIST calls reuse in-memory state (kept up to date
// by RTM lifecycle events).
func (c *Client) EnsurePublicChannels(ctx context.Context) error {
	c.RLock()
	synced := c.publicChannelsSynced
	sc := c.sc
	c.RUnlock()

	if synced || sc == nil {
		return nil
	}

	c.publicChannelsMu.Lock()
	defer c.publicChannelsMu.Unlock()

	c.RLock()
	synced = c.publicChannelsSynced
	sc = c.sc
	c.RUnlock()

	if synced || sc == nil {
		return nil
	}

	channels, err := c.fetchConversationPages(ctx, []string{convTypePublicChannel})
	if err != nil {
		return err
	}

	c.mergePublicChannels(channels)

	return nil
}

func (c *Client) mergePublicChannels(channels []slack.Channel) {
	c.Lock()
	for _, ch := range channels {
		if ch.ID == "" {
			continue
		}

		if ch.Name != "" && !ch.IsIM {
			c.channels[ch.ID] = "#" + ch.Name
		}

		if existing, ok := c.channelData[ch.ID]; ok {
			if ch.Latest == nil && existing.Latest != nil {
				ch.Latest = existing.Latest
			}

			if ch.LastRead == "" && existing.LastRead != "" {
				ch.LastRead = existing.LastRead
			}

			if c.joinedChannels[ch.ID] {
				ch.IsMember = true
			}
		}

		c.channelData[ch.ID] = ch
		if _, known := c.joinedChannels[ch.ID]; !known {
			c.joinedChannels[ch.ID] = isActiveConversation(ch)
		}
	}

	c.publicChannelsSynced = true
	c.Unlock()
}

func (c *Client) GetChannels() []slack.Channel {
	var channels []slack.Channel

	conversations, _ := c.GetConversations()
	for _, mmchannel := range conversations {
		if !isActiveConversation(mmchannel) {
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
	if channelID == "" || strings.HasPrefix(channelID, "&") || strings.HasPrefix(channelID, "#") {
		return channelID
	}

	c.RLock()

	if name, ok := c.channels[channelID]; ok {
		c.RUnlock()
		return name
	}

	if ch, ok := c.channelData[channelID]; ok && (ch.IsIM || ch.Name == "") {
		c.RUnlock()
		return channelID
	}

	c.RUnlock()

	if strings.HasPrefix(strings.ToUpper(channelID), "D") || c.sc == nil {
		return channelID
	}

	info, err := c.sc.GetConversationInfo(&slack.GetConversationInfoInput{
		ChannelID: channelID,
	})
	if err != nil || info == nil {
		return channelID
	}

	c.cacheChannel(*info)

	if info.IsIM || info.Name == "" {
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

// GetCachedChannelID resolves a channel ID or name from the local cache without
// triggering any network requests.
func (c *Client) GetCachedChannelID(name string) string {
	clean := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if clean == "" {
		return ""
	}

	target := "#" + clean
	upper := strings.ToUpper(clean)

	c.RLock()
	defer c.RUnlock()

	if _, ok := c.channels[upper]; ok {
		return upper
	}

	if _, ok := c.channelData[upper]; ok {
		return upper
	}

	for id, chName := range c.channels {
		if strings.EqualFold(chName, target) {
			return id
		}
	}

	return ""
}

func buildEdgeChannelSearchPayload(token, cleanName string) ([]byte, error) {
	payload := struct {
		Token           string `json:"token"`
		Query           string `json:"query"`
		Count           int    `json:"count"`
		Fuzz            int    `json:"fuzz"`
		CheckMembership bool   `json:"check_membership"`
	}{
		Token:           token,
		Query:           cleanName,
		Count:           edgeChannelsSearchMax,
		Fuzz:            0,
		CheckMembership: true,
	}

	return json.Marshal(payload)
}

// searchEdgeChannelID resolves an unjoined channel name via Slack's edgeapi
// channels/search endpoint for xoxc- cookie sessions, caching the matched
// channel on exact hit.
func (c *Client) searchEdgeChannelID(ctx context.Context, cleanName string) string {
	c.RLock()
	hc := c.httpClient
	token := c.token
	c.RUnlock()

	teamID := c.teamID()
	if hc == nil || hc.cookie == "" || !strings.HasPrefix(token, "xoxc-") || teamID == "" || cleanName == "" {
		return ""
	}

	reqBody, err := buildEdgeChannelSearchPayload(token, cleanName)
	if err != nil {
		return ""
	}

	reqCtx, cancel := context.WithTimeout(ctx, edgeChannelsTimeout)
	defer cancel()

	endpoint := edgeAPIUsersPrefix + teamID + "/channels/search"

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return ""
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		c.logger.Debugf("edgeapi channels/search: %s", err)

		return ""
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}

	if resp.StatusCode != http.StatusOK {
		c.logger.Debugf("edgeapi channels/search: HTTP %d", resp.StatusCode)

		return ""
	}

	ch, ok := c.parseEdgeChannelSearchMatch(body, cleanName)
	if !ok {
		return ""
	}

	c.cacheChannel(ch)

	return ch.ID
}

func (c *Client) parseEdgeChannelSearchMatch(body []byte, cleanName string) (slack.Channel, bool) {
	var parsed struct {
		Results  []slack.Channel `json:"results"`
		Channels []slack.Channel `json:"channels"`
	}

	err := json.Unmarshal(body, &parsed)
	if err != nil {
		c.logger.Debugf("edgeapi channels/search decode: %s", err)

		return slack.Channel{}, false
	}

	candidates := parsed.Results
	if len(candidates) == 0 {
		candidates = parsed.Channels
	}

	for _, ch := range candidates {
		if ch.ID == "" || ch.IsArchived {
			continue
		}

		if strings.EqualFold(ch.Name, cleanName) || strings.EqualFold(ch.NameNormalized, cleanName) {
			return ch, true
		}
	}

	return slack.Channel{}, false
}

func (c *Client) GetChannelID(ctx context.Context, name string) string {
	if id := c.GetCachedChannelID(name); id != "" {
		return id
	}

	clean := strings.TrimPrefix(strings.TrimSpace(name), "#")
	if clean == "" {
		return ""
	}

	c.RLock()
	sc := c.sc
	fetchedAt := c.channelsFetchedAt
	publicSynced := c.publicChannelsSynced
	lazy := c.LazyPublicChannels
	c.RUnlock()

	if sc == nil {
		return ""
	}

	if !lazy || fetchedAt == 0 {
		channels, _ := c.getConversations(ctx)
		for _, ch := range channels {
			if strings.EqualFold(ch.Name, clean) {
				return ch.ID
			}
		}

		if !lazy {
			return ""
		}
	}

	if !publicSynced {
		if chID := c.searchEdgeChannelID(ctx, clean); chID != "" {
			return chID
		}

		err := c.EnsurePublicChannels(ctx)
		if err == nil {
			return c.GetCachedChannelID(clean)
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
	selfID := ""
	if c.info != nil && c.info.User != nil {
		selfID = c.info.User.ID
	}

	users, missing := c.cachedChannelUsers(members, selfID)
	if len(missing) == 0 {
		return users
	}

	if c.isCookieSession() {
		ctx, cancel := context.WithTimeout(context.Background(), edgeUsersTimeout)
		c.fetchEdgeUsersInfo(ctx, missing)
		cancel()

		users, missing = c.cachedChannelUsers(members, selfID)
	}

	// Avoid firing N sequential users.info calls while background users.list
	// is still running; missing passive members will be backfilled once loaded.
	if len(missing) == 0 || !c.usersLoaded.Load() {
		return users
	}

	for _, uid := range missing {
		if u := c.GetUser(uid); u != nil {
			users = append(users, u)
		}
	}

	return users
}

func (c *Client) cachedChannelUsers(members []string, selfID string) ([]*slack.User, []string) {
	users := make([]*slack.User, 0, len(members))

	var missing []string

	c.RLock()
	defer c.RUnlock()

	for _, uid := range members {
		if uid == "" || (selfID != "" && selfID == uid) {
			continue
		}

		if u, ok := c.users[uid]; ok {
			cp := u
			users = append(users, &cp)
		} else {
			missing = append(missing, uid)
		}
	}

	return users, missing
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
	isIM := strings.HasPrefix(channelID, "D")

	c.Lock()
	if !isIM {
		c.channels[channelID] = "#" + cleanName
	}

	ch, ok := c.channelData[channelID]
	if !ok {
		ch.ID = channelID
		ch.IsIM = isIM
		ch.IsGroup = strings.HasPrefix(channelID, "G")
		ch.IsPrivate = ch.IsGroup
		ch.IsMember = c.joinedChannels[channelID]
	}

	ch.Name = cleanName
	c.channelData[channelID] = ch
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

// GetChannelCount returns the number of active joined non-DM channels for the current user.
func (c *Client) GetChannelCount() int {
	c.RLock()
	defer c.RUnlock()

	count := 0

	for id, joined := range c.joinedChannels {
		if !joined {
			continue
		}

		if ch, ok := c.channelData[id]; ok {
			if ch.IsArchived || ch.IsIM {
				continue
			}
		}

		count++
	}

	return count
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
		Types:           []string{convTypePublicChannel},
		Limit:           conversationPageLimit,
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
