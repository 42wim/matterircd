package msteamsclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	teamsChannelSuffix     = "@thread.tacv2"
	teamsDMUnqSuffix       = "@unq.gbl.spaces"
	teamsGroupThreadSuffix = "@thread.v2"
	selfNotesConvID        = "48:notes"
	maxConversationPages   = 10
	conversationsPageSize  = 200
)

type rawConversationsResponse struct {
	Conversations []rawConversation `json:"conversations"`
	Metadata      struct {
		BackwardLink string `json:"backwardLink"`
		SyncState    string `json:"syncState"`
	} `json:"_metadata"`
}

type rawConversation struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Version          any    `json:"version"`
	ThreadProperties struct {
		Topic           string `json:"topic"`
		ThreadType      string `json:"threadType"`
		ProductThreadID string `json:"productThreadType"`
		SpaceThreadID   string `json:"spaceThreadTopic"`
		FixedRoster     string `json:"fixedRoster"`
		IsArchived      string `json:"isArchived"`
		MemberCount     string `json:"memberCount"`
	} `json:"threadProperties"`
	Properties struct {
		ConsumptionHorizon string `json:"consumptionhorizon"`
		ClearedAt          string `json:"clearedat"`
		EmptyRoom          string `json:"emptyroom"`
		OneOnOne           string `json:"oneOnOne"`
		IsFollowed         string `json:"isfollowed"`
	} `json:"properties"`
	LastMessage struct {
		ID                  string `json:"id"`
		OriginalArrivalTime string `json:"originalarrivaltime"`
		ComposeTime         string `json:"composetime"`
		From                string `json:"from"`
	} `json:"lastMessage"`
	MemberProperties struct {
		Role string `json:"role"`
	} `json:"memberProperties"`
}

type rawCSATeamsResponse struct {
	Teams []rawCSATeam `json:"teams"`
	Chats []rawCSAChat `json:"chats"`
}

type rawCSATeam struct {
	ID          string          `json:"id"`
	DisplayName string          `json:"displayName"`
	Description string          `json:"description"`
	IsArchived  bool            `json:"isArchived"`
	Channels    []rawCSAChannel `json:"channels"`
}

type rawCSAChannel struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	Description    string `json:"description"`
	IsGeneral      bool   `json:"isGeneral"`
	IsArchived     bool   `json:"isArchived"`
	MembershipType string `json:"membershipType"`
}

type rawCSAChat struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Members []struct {
		MRI          string `json:"mri"`
		ObjectID     string `json:"objectId"`
		FriendlyName string `json:"friendlyName"`
	} `json:"members"`
}

type rawThreadResponse struct {
	ID         string `json:"id"`
	Properties struct {
		Topic      string `json:"topic"`
		ThreadType string `json:"threadType"`
	} `json:"properties"`
	Members []struct {
		ID           string `json:"id"`
		FriendlyName string `json:"friendlyName"`
		Role         string `json:"role"`
	} `json:"members"`
}

// IsTeamsChannel reports whether conversationID refers to a Teams channel
// (which uses ;messageid= sub-threading rather than flat chat quote replies).
func IsTeamsChannel(conversationID string) bool {
	return strings.HasSuffix(conversationID, teamsChannelSuffix)
}

// UpdateChannels refreshes Teams, channels, and chat conversations from
// chatsvc (and CSA for Work/School teams & channels) into UsersCache.
func (c *Client) UpdateChannels(ctx context.Context) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	convs, err := c.fetchConversations(ctx, ep.ChatService)
	if err != nil {
		return err
	}

	teams, csaChans, csaMembers := c.fetchCSATeams(ctx, ep)

	c.Users.mu.Lock()

	for _, t := range teams {
		c.Users.teams[t.ID] = t
	}

	for id, ch := range convs {
		if csaCh, ok := csaChans[id]; ok {
			mergeCSAChannel(ch, csaCh)
		}

		c.Users.conversations[id] = ch
		c.Users.joinedChannels[id] = struct{}{}

		if ch.LastViewedAt > 0 {
			c.Users.channelLastViewedAt[id] = ch.LastViewedAt
		}

		if len(ch.Members) > 0 {
			c.setMembersLocked(id, ch.Members)
		}
	}

	for id, ch := range csaChans {
		if _, exists := c.Users.conversations[id]; !exists {
			c.Users.conversations[id] = ch
			c.Users.joinedChannels[id] = struct{}{}
		}
	}

	for convID, members := range csaMembers {
		c.setMembersLocked(convID, members)

		if conv, ok := c.Users.conversations[convID]; ok && len(conv.Members) == 0 {
			conv.Members = members
			reclassifyConversation(conv)
		}
	}

	c.Users.lastUpdated.Store(time.Now().Unix())
	c.Users.mu.Unlock()

	return nil
}

func (c *Client) fetchConversations(ctx context.Context, chatService string) (map[string]*Conversation, error) {
	result := make(map[string]*Conversation)
	nextURL := fmt.Sprintf(
		"%s/v1/users/ME/conversations?view=msnp24Equivalent%%7CsupportsMessageProperties&pageSize=%d&targetType=Passport%%7CSkype%%7CLync%%7CThread%%7CPSTN%%7CAgent",
		chatService,
		conversationsPageSize,
	)

	for range maxConversationPages {
		if nextURL == "" {
			break
		}

		var resp rawConversationsResponse

		err := c.doJSON(ctx, http.MethodGet, nextURL, authSkypeToken, "", nil, &resp)
		if err != nil {
			if len(result) > 0 {
				c.logger.Warnf("Partial conversation sync (%d loaded): %v", len(result), err)

				break
			}

			return nil, fmt.Errorf("msteamsclient: list conversations: %w", err)
		}

		for i := range resp.Conversations {
			conv := parseRawConversation(&resp.Conversations[i])
			if conv == nil {
				continue
			}

			result[conv.ID] = conv
		}

		if resp.Metadata.BackwardLink == "" || resp.Metadata.BackwardLink == nextURL || len(resp.Conversations) == 0 {
			break
		}

		nextURL = resp.Metadata.BackwardLink
	}

	return result, nil
}

func (c *Client) fetchCSATeams(ctx context.Context, ep RegionEndpoints) (map[string]*Team, map[string]*Conversation, map[string][]string) {
	teams := make(map[string]*Team)
	channels := make(map[string]*Conversation)
	chatMembers := make(map[string][]string)

	if ep.ChatsvcAgg == "" {
		return teams, channels, chatMembers
	}

	csaURL := ep.ChatsvcAgg + "/api/v1/teams/users/me?isPrefetch=false&enableMembershipSummary=true"

	scope := ScopeChatsvcAgg
	if c.effectiveAccountType() == AccountPersonal {
		scope = ScopeSpacesPersonal
	}

	var resp rawCSATeamsResponse

	err := c.doJSON(ctx, http.MethodGet, csaURL, authSkypeAndBearer, scope, nil, &resp)
	if err != nil {
		c.logger.Debugf("CSA teams/users/me sync skipped or unavailable: %v", err)

		return teams, channels, chatMembers
	}

	c.collectCSATeams(resp.Teams, teams, channels)
	c.collectCSAChatMembers(resp.Chats, chatMembers)

	return teams, channels, chatMembers
}

func (c *Client) collectCSATeams(rawTeams []rawCSATeam, teams map[string]*Team, channels map[string]*Conversation) {
	for i := range rawTeams {
		rt := &rawTeams[i]
		team := &Team{
			ID:          rt.ID,
			DisplayName: rt.DisplayName,
			Description: rt.Description,
		}

		for j := range rt.Channels {
			rc := &rt.Channels[j]
			conv := &Conversation{
				ID:          rc.ID,
				Type:        ConversationChannel,
				TeamID:      rt.ID,
				TeamName:    rt.DisplayName,
				DisplayName: rc.DisplayName,
				Topic:       rc.Description,
				IsGeneral:   rc.IsGeneral,
				IsPrivate:   strings.EqualFold(rc.MembershipType, "private") || strings.EqualFold(rc.MembershipType, "shared"),
				IsArchived:  rc.IsArchived || rt.IsArchived,
			}

			team.Channels = append(team.Channels, conv)
			channels[conv.ID] = conv
		}

		teams[team.ID] = team
	}
}

func (c *Client) collectCSAChatMembers(rawChats []rawCSAChat, chatMembers map[string][]string) {
	for i := range rawChats {
		rc := &rawChats[i]
		if rc.ID == "" {
			continue
		}

		members := make([]string, 0, len(rc.Members))
		for _, m := range rc.Members {
			if m.MRI == "" {
				continue
			}

			members = append(members, m.MRI)

			if m.FriendlyName != "" {
				c.CacheUser(&User{
					MRI:         m.MRI,
					ObjectID:    m.ObjectID,
					DisplayName: m.FriendlyName,
				})
			}
		}

		if len(members) > 0 {
			chatMembers[rc.ID] = members
		}
	}
}

func parseRawConversation(rc *rawConversation) *Conversation {
	if rc.ID == "" {
		return nil
	}

	// Skip internal system/notification pseudo-conversations.
	if strings.HasPrefix(rc.ID, "48:") && rc.ID != selfNotesConvID {
		return nil
	}

	conv := &Conversation{
		ID:           rc.ID,
		DisplayName:  strings.TrimSpace(rc.ThreadProperties.Topic),
		Topic:        strings.TrimSpace(rc.ThreadProperties.Topic),
		LastPostAt:   parseTeamsTimeMillis(rc.LastMessage.ComposeTime, rc.LastMessage.OriginalArrivalTime),
		LastViewedAt: parseConsumptionHorizon(rc.Properties.ConsumptionHorizon),
		IsArchived:   strings.EqualFold(rc.ThreadProperties.IsArchived, "true"),
	}

	if conv.DisplayName == "" && rc.ThreadProperties.SpaceThreadID != "" {
		conv.DisplayName = strings.TrimSpace(rc.ThreadProperties.SpaceThreadID)
	}

	classifyRawConversation(conv, rc)

	return conv
}

func classifyRawConversation(conv *Conversation, rc *rawConversation) {
	switch {
	case rc.ID == selfNotesConvID:
		conv.Type = ConversationSelfNote
		if conv.DisplayName == "" {
			conv.DisplayName = "You"
		}
	case IsTeamsChannel(rc.ID):
		conv.Type = ConversationChannel
	case strings.HasSuffix(rc.ID, teamsDMUnqSuffix):
		conv.Type = ConversationDM
		conv.Members = extractUnqDMMembers(rc.ID)
	case strings.EqualFold(rc.ThreadProperties.ThreadType, "meeting"):
		conv.Type = ConversationMeeting
	case strings.EqualFold(rc.Properties.OneOnOne, "true") || strings.EqualFold(rc.ThreadProperties.ProductThreadID, "OneToOne"):
		conv.Type = ConversationDM
	default:
		count, err := strconv.Atoi(rc.ThreadProperties.MemberCount)
		if err == nil && count == 2 && conv.Topic == "" {
			conv.Type = ConversationDM
		} else {
			conv.Type = ConversationGroup
		}
	}
}

func mergeCSAChannel(dst, src *Conversation) {
	dst.Type = ConversationChannel
	dst.TeamID = src.TeamID
	dst.TeamName = src.TeamName
	dst.IsGeneral = src.IsGeneral
	dst.IsPrivate = src.IsPrivate
	dst.IsArchived = dst.IsArchived || src.IsArchived

	if src.DisplayName != "" {
		dst.DisplayName = src.DisplayName
	}

	if dst.Topic == "" && src.Topic != "" {
		dst.Topic = src.Topic
	}
}

func reclassifyConversation(conv *Conversation) {
	if conv.Type == ConversationGroup && conv.Topic == "" && len(conv.Members) == 2 && strings.HasSuffix(conv.ID, teamsGroupThreadSuffix) {
		conv.Type = ConversationDM
	}
}

// extractUnqDMMembers parses "19:<id1>_<id2>@unq.gbl.spaces" into two user MRIs.
func extractUnqDMMembers(convID string) []string {
	trimmed := strings.TrimPrefix(convID, "19:")
	trimmed = strings.TrimSuffix(trimmed, teamsDMUnqSuffix)

	left, right, ok := strings.Cut(trimmed, "_")
	if !ok || left == "" || right == "" {
		return nil
	}

	return []string{normalizeMemberID(left), normalizeMemberID(right)}
}

func normalizeMemberID(raw string) string {
	if strings.HasPrefix(raw, "8:") || strings.HasPrefix(raw, "28:") {
		return raw
	}

	return "8:orgid:" + raw
}

func parseConsumptionHorizon(raw string) int64 {
	if raw == "" {
		return 0
	}

	first, _, _ := strings.Cut(raw, ";")

	ms, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0
	}

	return ms
}

func (c *Client) setMembersLocked(convID string, members []string) {
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		if m != "" {
			set[m] = struct{}{}
		}
	}

	c.Users.channelMembers[convID] = set
}

// GetChannels returns copies of all cached conversations and channels.
func (c *Client) GetChannels() []*Conversation {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make([]*Conversation, 0, len(c.Users.conversations))
	for _, ch := range c.Users.conversations {
		cp := *ch
		cp.Members = slices.Clone(ch.Members)
		out = append(out, &cp)
	}

	return out
}

// GetChannel returns a copy of a single cached conversation by ID, or nil.
func (c *Client) GetChannel(channelID string) *Conversation {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	ch, ok := c.Users.conversations[channelID]
	if !ok {
		return nil
	}

	cp := *ch
	cp.Members = slices.Clone(ch.Members)

	return &cp
}

// GetChannelName returns the display name/topic of a conversation, or its ID if unknown.
func (c *Client) GetChannelName(channelID string) string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	if ch, ok := c.Users.conversations[channelID]; ok && ch.DisplayName != "" {
		return ch.DisplayName
	}

	return channelID
}

// GetTeams returns copies of all cached Teams.
func (c *Client) GetTeams() []*Team {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make([]*Team, 0, len(c.Users.teams))
	for _, t := range c.Users.teams {
		cp := *t
		out = append(out, &cp)
	}

	return out
}

// GetTeamName returns the display name of teamID, or "" if unknown.
func (c *Client) GetTeamName(teamID string) string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	if t, ok := c.Users.teams[teamID]; ok {
		return t.DisplayName
	}

	return ""
}

// GetChannelLastViewedAt returns the cached consumption horizon (epoch ms) for channelID.
func (c *Client) GetChannelLastViewedAt(channelID string) int64 {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return c.Users.channelLastViewedAt[channelID]
}

// IsChannelMember reports whether the current user belongs to channelID.
func (c *Client) IsChannelMember(channelID string) bool {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	_, ok := c.Users.joinedChannels[channelID]

	return ok
}

// GetChannelMembers returns the member MRIs of conversationID, fetching them
// from chatsvc if not already cached.
func (c *Client) GetChannelMembers(ctx context.Context, conversationID string) ([]string, error) {
	if members := c.cachedChannelMembers(conversationID); len(members) > 0 {
		return members, nil
	}

	if conversationID == selfNotesConvID {
		if me := c.Me(); me != nil {
			return []string{me.MRI}, nil
		}

		return nil, nil
	}

	ep := c.Endpoints()
	if ep.ChatService == "" {
		return nil, errors.New("msteamsclient: not authenticated")
	}

	threadURL := fmt.Sprintf("%s/v1/threads/%s?view=msnp24Equivalent", ep.ChatService, url.PathEscape(conversationID))

	var resp rawThreadResponse

	err := c.doJSON(ctx, http.MethodGet, threadURL, authSkypeToken, "", nil, &resp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: get thread members for %s: %w", conversationID, err)
	}

	members := make([]string, 0, len(resp.Members))
	for _, m := range resp.Members {
		if m.ID == "" {
			continue
		}

		members = append(members, m.ID)

		if m.FriendlyName != "" {
			c.CacheUser(&User{
				MRI:         m.ID,
				DisplayName: m.FriendlyName,
			})
		}
	}

	c.updateThreadMembersCache(conversationID, members, resp.Properties.Topic)

	return members, nil
}

func (c *Client) cachedChannelMembers(conversationID string) []string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	set, ok := c.Users.channelMembers[conversationID]
	if !ok || len(set) == 0 {
		return nil
	}

	members := make([]string, 0, len(set))
	for m := range set {
		members = append(members, m)
	}

	return members
}

func (c *Client) updateThreadMembersCache(conversationID string, members []string, topic string) {
	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	c.setMembersLocked(conversationID, members)

	if conv, ok := c.Users.conversations[conversationID]; ok {
		conv.Members = slices.Clone(members)
		if conv.Topic == "" && topic != "" {
			conv.Topic = topic
			conv.DisplayName = topic
		}

		reclassifyConversation(conv)
	}
}

// GetDMConversationID returns the cached 1:1 DM conversation ID with otherMRI, or "".
func (c *Client) GetDMConversationID(otherMRI string) string {
	me := c.Me()

	myMRI := ""
	if me != nil {
		myMRI = me.MRI
	}

	if otherMRI == myMRI && myMRI != "" {
		return selfNotesConvID
	}

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, conv := range c.Users.conversations {
		if conv.Type != ConversationDM {
			continue
		}

		if slices.Contains(conv.Members, otherMRI) {
			return conv.ID
		}
	}

	return ""
}

// GetOrCreateDM returns an existing 1:1 DM conversation ID with otherMRI, or
// creates a new 1:1 chat thread via chatsvc.
func (c *Client) GetOrCreateDM(ctx context.Context, otherMRI string) (string, error) {
	if existing := c.GetDMConversationID(otherMRI); existing != "" {
		return existing, nil
	}

	me := c.Me()
	if me == nil || me.MRI == "" {
		return "", errors.New("msteamsclient: current user MRI unknown")
	}

	conv, err := c.createThread(ctx, []string{me.MRI, otherMRI}, "", true)
	if err != nil {
		return "", err
	}

	return conv.ID, nil
}

// CreateGroupChat creates a new group chat thread with memberMRIs and an optional topic.
func (c *Client) CreateGroupChat(ctx context.Context, memberMRIs []string, topic string) (*Conversation, error) {
	me := c.Me()
	if me == nil || me.MRI == "" {
		return nil, errors.New("msteamsclient: current user MRI unknown")
	}

	members := make([]string, 0, len(memberMRIs)+1)
	members = append(members, me.MRI)

	for _, mri := range memberMRIs {
		if mri != "" && !slices.Contains(members, mri) {
			members = append(members, mri)
		}
	}

	return c.createThread(ctx, members, topic, false)
}

func (c *Client) createThread(ctx context.Context, memberMRIs []string, topic string, oneOnOne bool) (*Conversation, error) {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return nil, errors.New("msteamsclient: not authenticated")
	}

	payload := buildCreateThreadPayload(memberMRIs, topic, oneOnOne)
	reqURL := ep.ChatService + "/v1/threads"

	var resp struct {
		ID string `json:"id"`
	}

	err := c.doJSON(ctx, http.MethodPost, reqURL, authSkypeToken, "", payload, &resp)
	if err != nil {
		return nil, fmt.Errorf("msteamsclient: create thread: %w", err)
	}

	if resp.ID == "" {
		_ = c.UpdateChannels(ctx)

		if oneOnOne && len(memberMRIs) == 2 {
			if id := c.GetDMConversationID(memberMRIs[1]); id != "" {
				return c.GetChannel(id), nil
			}
		}

		return nil, errors.New("msteamsclient: create thread succeeded without returning a thread ID")
	}

	convType := ConversationGroup
	if oneOnOne {
		convType = ConversationDM
	}

	conv := &Conversation{
		ID:          resp.ID,
		Type:        convType,
		DisplayName: topic,
		Topic:       topic,
		Members:     slices.Clone(memberMRIs),
	}

	c.Users.mu.Lock()
	c.Users.conversations[conv.ID] = conv
	c.Users.joinedChannels[conv.ID] = struct{}{}
	c.setMembersLocked(conv.ID, memberMRIs)
	c.Users.mu.Unlock()

	return conv, nil
}

func buildCreateThreadPayload(memberMRIs []string, topic string, oneOnOne bool) map[string]any {
	type threadMember struct {
		ID   string `json:"id"`
		Role string `json:"role"`
	}

	members := make([]threadMember, 0, len(memberMRIs))
	for _, mri := range memberMRIs {
		members = append(members, threadMember{ID: mri, Role: "Admin"})
	}

	props := map[string]string{
		"threadType": "chat",
	}

	if oneOnOne {
		props["fixedRoster"] = "true"
		props["uniqueRosterThread"] = "true"
	}

	if topic != "" {
		props["topic"] = topic
	}

	return map[string]any{
		"members":    members,
		"properties": props,
	}
}

// AddChannelMember invites userMRI to conversationID.
func (c *Client) AddChannelMember(ctx context.Context, conversationID, userMRI string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf("%s/v1/threads/%s/members/%s", ep.ChatService, url.PathEscape(conversationID), url.PathEscape(userMRI))
	payload := map[string]string{"role": "Admin"}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: add member %s to %s: %w", userMRI, conversationID, err)
	}

	c.Users.mu.Lock()
	if set, ok := c.Users.channelMembers[conversationID]; ok {
		set[userMRI] = struct{}{}
	}
	c.Users.mu.Unlock()

	return nil
}

// RemoveChannelMember removes userMRI from conversationID.
func (c *Client) RemoveChannelMember(ctx context.Context, conversationID, userMRI string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf("%s/v1/threads/%s/members/%s", ep.ChatService, url.PathEscape(conversationID), url.PathEscape(userMRI))

	err := c.doJSON(ctx, http.MethodDelete, reqURL, authSkypeToken, "", nil, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: remove member %s from %s: %w", userMRI, conversationID, err)
	}

	c.Users.mu.Lock()
	if set, ok := c.Users.channelMembers[conversationID]; ok {
		delete(set, userMRI)
	}
	c.Users.mu.Unlock()

	return nil
}

// LeaveChannel removes the current user from conversationID.
func (c *Client) LeaveChannel(ctx context.Context, conversationID string) error {
	me := c.Me()
	if me == nil || me.MRI == "" {
		return errors.New("msteamsclient: current user MRI unknown")
	}

	err := c.RemoveChannelMember(ctx, conversationID, me.MRI)
	if err != nil {
		return err
	}

	c.Users.mu.Lock()
	delete(c.Users.joinedChannels, conversationID)
	c.Users.mu.Unlock()

	return nil
}

// SetChannelTopic updates the topic of a group chat or conversation.
func (c *Client) SetChannelTopic(ctx context.Context, conversationID, topic string) error {
	ep := c.Endpoints()
	if ep.ChatService == "" {
		return errors.New("msteamsclient: not authenticated")
	}

	reqURL := fmt.Sprintf("%s/v1/threads/%s/properties?name=topic", ep.ChatService, url.PathEscape(conversationID))
	payload := map[string]string{"topic": topic}

	err := c.doJSON(ctx, http.MethodPut, reqURL, authSkypeToken, "", payload, nil)
	if err != nil {
		return fmt.Errorf("msteamsclient: set topic on %s: %w", conversationID, err)
	}

	c.Users.mu.Lock()
	if conv, ok := c.Users.conversations[conversationID]; ok {
		conv.Topic = topic
		conv.DisplayName = topic
	}
	c.Users.mu.Unlock()

	return nil
}
