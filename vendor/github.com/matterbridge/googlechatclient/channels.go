package googlechatclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	worldPageSize   = 50
	maxWorldPages   = 20
	membersPageSize = 100
	maxMemberPages  = 20
)

// UpdateChannels fetches the user's Spaces, 1:1 DMs, and Group DMs via
// paginated_world and populates the local channel and user caches.
func (c *Client) UpdateChannels(ctx context.Context) error {
	var (
		anchorUsec int64
		pageToken  string
		missingIDs []string
	)

	seenMissing := make(map[string]struct{})
	seenConvs := make(map[string]struct{})

	for range maxWorldPages {
		reqBytes := encodePaginatedWorldRequest(worldPageSize, anchorUsec, pageToken)

		respBytes, err := c.doProto(ctx, "paginated_world", reqBytes)
		if err != nil {
			return fmt.Errorf("googlechatclient: paginated_world: %w", err)
		}

		resp, err := decodePaginatedWorldResponse(respBytes)
		if err != nil {
			return fmt.Errorf("googlechatclient: decode paginated_world: %w", err)
		}

		for _, item := range resp.Items {
			conv := c.upsertWorldItem(item)
			if conv == nil {
				continue
			}

			seenConvs[conv.ID] = struct{}{}
			c.collectMissingUserIDs(conv.Members, seenMissing, &missingIDs)
		}

		if !resp.HasMore || (resp.NextPageToken == "" && resp.NextAnchorUsec == anchorUsec) {
			break
		}

		anchorUsec = resp.NextAnchorUsec
		pageToken = resp.NextPageToken
	}

	c.Users.lastUpdated.Store(time.Now().UnixNano())

	if len(missingIDs) > 0 {
		_, resolveErr := c.ResolveUsers(ctx, missingIDs)
		if resolveErr != nil {
			c.logger.Debugf("Failed resolving some DM/space member profiles: %v", resolveErr)
		}
	}

	// Best-effort member roster fetch for Spaces that did not include full member lists in paginated_world.
	c.syncSpaceMembersIfNeeded(ctx, seenConvs)

	return nil
}

func (c *Client) collectMissingUserIDs(memberIDs []string, seen map[string]struct{}, missing *[]string) {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, uid := range memberIDs {
		if uid == "" {
			continue
		}

		if _, ok := seen[uid]; ok {
			continue
		}

		if u := c.Users.users[uid]; u != nil && u.DisplayName != "" {
			continue
		}

		seen[uid] = struct{}{}
		*missing = append(*missing, uid)
	}
}

func (c *Client) syncSpaceMembersIfNeeded(ctx context.Context, convIDs map[string]struct{}) {
	for id := range convIDs {
		conv := c.GetChannel(id)
		if conv == nil {
			continue
		}

		isGroupOrSpace := conv.Type == ConversationSpace || conv.Type == ConversationGroupDM
		needsMembers := len(conv.Members) == 0 || (conv.DisplayName == "" && len(conv.Members) <= 1)

		if !isGroupOrSpace || !needsMembers {
			continue
		}

		err := c.fetchSpaceMembers(ctx, id)
		if err != nil {
			c.logger.Debugf("Failed listing members for space %s: %v", id, err)
		}
	}
}

// SyncConversation fetches up-to-date metadata and members for a single Space or DM.
func (c *Client) SyncConversation(ctx context.Context, convID string) (*Conversation, error) {
	if convID == "" {
		return nil, errors.New("googlechatclient: empty conversation ID")
	}

	ref := c.protoGroupRefFor(convID)

	respBytes, err := c.doProto(ctx, "get_group", encodeGetGroupRequest(ref))
	if err != nil {
		// Retry with opposite Space/DM wire tag if the group was not previously cached.
		if !c.hasCachedConversation(convID) {
			ref.IsDM = !ref.IsDM

			respBytes, err = c.doProto(ctx, "get_group", encodeGetGroupRequest(ref))
		}

		if err != nil {
			return nil, fmt.Errorf("googlechatclient: get_group(%s): %w", convID, err)
		}
	}

	resp, err := decodeGetGroupResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode get_group(%s): %w", convID, err)
	}

	conv := c.mergeGetGroupResponse(convID, ref, resp)
	if conv == nil {
		return nil, fmt.Errorf("googlechatclient: get_group(%s) returned empty group", convID)
	}

	if conv.Type == ConversationSpace {
		_ = c.fetchSpaceMembers(ctx, conv.ID)
		conv = c.GetChannel(conv.ID)
	}

	if conv != nil && !conv.IsHidden {
		c.subscribeGroup(ctx, protoGroupRef{ID: conv.ID, IsDM: conv.IsDMGroup})
	}

	return conv, nil
}

func (c *Client) fetchSpaceMembers(ctx context.Context, convID string) error {
	ref := c.protoGroupRefFor(convID)

	var (
		pageToken string
		members   []string
		admins    []string
	)

	for range maxMemberPages {
		reqBytes := encodeListMembersRequest(ref, membersPageSize, pageToken)

		respBytes, err := c.doProto(ctx, "list_members", reqBytes)
		if err != nil {
			return err
		}

		resp, err := decodeListMembersResponse(respBytes)
		if err != nil {
			return err
		}

		for _, pu := range resp.Members {
			if u := protoUserToUser(pu); u != nil {
				c.CacheUser(u)
			}
		}

		for _, m := range resp.Memberships {
			if m.UserID == "" || m.State == protoMembershipNotAMember {
				continue
			}

			members = append(members, m.UserID)

			if m.Role == protoRoleOwner || m.Role == protoRoleManager {
				admins = append(admins, m.UserID)
			}
		}

		if resp.NextPageToken == "" || resp.NextPageToken == pageToken {
			break
		}

		pageToken = resp.NextPageToken
	}

	if len(members) == 0 {
		return nil
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	if conv := c.Users.conversations[convID]; conv != nil {
		conv.Members = dedupStrings(members)
		conv.Admins = dedupStrings(admins)
		c.storeConversationLocked(conv)
	}

	return nil
}

func (c *Client) upsertWorldItem(item *protoWorldItem) *Conversation {
	if item == nil || item.Group.ID == "" {
		return nil
	}

	for _, pu := range item.UserProfiles {
		if u := protoUserToUser(pu); u != nil {
			c.CacheUser(u)
		}
	}

	conv := c.worldItemToConversation(item)

	c.Users.mu.Lock()
	c.storeConversationLocked(conv)
	c.Users.mu.Unlock()

	return cloneConversation(conv)
}

func (c *Client) worldItemToConversation(item *protoWorldItem) *Conversation {
	members := collectWorldMembers(item)
	myID := c.selfUserID()
	convType := classifyConversationType(item.Group.IsDM, item.AttrGroupType, members, myID)

	name := strings.TrimSpace(item.RoomName)
	if name == "" {
		name = strings.TrimSpace(item.NameGroupName)
	}

	conv := &Conversation{
		ID:          item.Group.ID,
		IsDMGroup:   item.Group.IsDM,
		Type:        convType,
		DisplayName: name,
		Topic:       strings.TrimSpace(item.Description),
		Members:     members,
		LastPostAt:  usecToMillis(item.SortTimeUsec),
		IsPrivate:   true,
	}

	applyReadStateToConv(conv, item.ReadState)

	return conv
}

func collectWorldMembers(item *protoWorldItem) []string {
	var raw []string

	raw = append(raw, item.DMMembers...)
	raw = append(raw, item.NameUserIDs...)

	if item.ReadState != nil {
		raw = append(raw, item.ReadState.JoinedUsers...)
	}

	for _, pu := range item.UserProfiles {
		if pu != nil && pu.ID != "" {
			raw = append(raw, pu.ID)
		}
	}

	return dedupStrings(raw)
}

func applyReadStateToConv(conv *Conversation, rs *protoGroupReadState) {
	if rs == nil {
		return
	}

	if rs.LastReadTimeUsec > 0 {
		conv.LastViewedAt = usecToMillis(rs.LastReadTimeUsec)
	}

	if rs.LastHeadMsgUsec > 0 && usecToMillis(rs.LastHeadMsgUsec) > conv.LastPostAt {
		conv.LastPostAt = usecToMillis(rs.LastHeadMsgUsec)
	}

	latestUsec := max(rs.LastHeadMsgUsec, conv.LastPostAt*1000)
	if rs.HideTimeUsec > 0 && rs.HideTimeUsec >= latestUsec {
		conv.IsHidden = true
	} else if latestUsec > rs.HideTimeUsec {
		conv.IsHidden = false
	}
}

func (c *Client) touchConversation(convID string, postAtMs int64) {
	if convID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	if conv := c.Users.conversations[convID]; conv != nil {
		conv.IsHidden = false
		if postAtMs > conv.LastPostAt {
			conv.LastPostAt = postAtMs
		}
	}
}

func classifyConversationType(isDM bool, attrType int32, members []string, myID string) ConversationType {
	switch attrType {
	case protoAttrOneToOneHumanDM, protoAttrOneToOneBotDM:
		return classifyDMByMembers(members, myID)
	case protoAttrGroupDM:
		return ConversationGroupDM
	case protoAttrFlatRoom, protoAttrThreadedRoom:
		return ConversationSpace
	}

	if !isDM {
		return ConversationSpace
	}

	return classifyDMByMembers(members, myID)
}

func classifyDMByMembers(members []string, myID string) ConversationType {
	otherCount := 0

	for _, id := range members {
		if id != "" && id != myID {
			otherCount++
		}
	}

	switch {
	case otherCount > 1:
		return ConversationGroupDM
	case otherCount == 0 && len(members) > 0:
		return ConversationSelfNote
	default:
		return ConversationDM
	}
}

func (c *Client) mergeGetGroupResponse(convID string, ref protoGroupRef, resp *protoGetGroupResp) *Conversation {
	if resp == nil {
		return nil
	}

	for _, pu := range resp.NameProfiles {
		if u := protoUserToUser(pu); u != nil {
			c.CacheUser(u)
		}
	}

	g := resp.Group
	if g != nil && g.Group.ID != "" {
		ref = g.Group
		convID = g.Group.ID
	}

	members, admins := collectGroupRespMembers(g, resp)

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	existing := c.Users.conversations[convID]

	conv := buildMergedConversation(convID, ref, g, existing, members, admins, c.selfUserID())
	c.storeConversationLocked(conv)

	return cloneConversation(conv)
}

func collectGroupRespMembers(g *protoGroup, resp *protoGetGroupResp) ([]string, []string) {
	var (
		members []string
		admins  []string
	)

	members = append(members, resp.JoinedMemberIDs...)

	for _, m := range resp.Memberships {
		if m.UserID == "" || m.State == protoMembershipNotAMember {
			continue
		}

		members = append(members, m.UserID)

		if m.Role == protoRoleOwner || m.Role == protoRoleManager {
			admins = append(admins, m.UserID)
		}
	}

	if g != nil {
		members = append(members, g.NameUserIDs...)
		if g.ReadState != nil {
			members = append(members, g.ReadState.JoinedUsers...)
		}
	}

	return dedupStrings(members), dedupStrings(admins)
}

func buildMergedConversation(
	convID string,
	ref protoGroupRef,
	g *protoGroup,
	existing *Conversation,
	members, admins []string,
	myID string,
) *Conversation {
	conv := &Conversation{
		ID:        convID,
		IsDMGroup: ref.IsDM,
		IsPrivate: true,
	}

	if existing != nil {
		*conv = *cloneConversation(existing)
		conv.IsDMGroup = ref.IsDM
	}

	if len(members) > 0 {
		conv.Members = members
	}

	if len(admins) > 0 {
		conv.Admins = admins
	}

	var attrType int32

	if g != nil {
		attrType = g.AttrGroupType

		if name := strings.TrimSpace(g.Name); name != "" {
			conv.DisplayName = name
		} else if conv.DisplayName == "" && strings.TrimSpace(g.NameGroupName) != "" {
			conv.DisplayName = strings.TrimSpace(g.NameGroupName)
		}

		if desc := strings.TrimSpace(g.Description); desc != "" {
			conv.Topic = desc
		}

		if g.SortTimeUsec > 0 {
			conv.LastPostAt = usecToMillis(g.SortTimeUsec)
		}

		applyReadStateToConv(conv, g.ReadState)
	}

	conv.Type = classifyConversationType(conv.IsDMGroup, attrType, conv.Members, myID)

	return conv
}

func (c *Client) storeConversationLocked(conv *Conversation) {
	if conv == nil || conv.ID == "" {
		return
	}

	cp := cloneConversation(conv)
	c.Users.conversations[cp.ID] = cp

	if cp.IsDMGroup {
		c.Users.dmGroupIDs[cp.ID] = struct{}{}
	} else {
		delete(c.Users.dmGroupIDs, cp.ID)
	}

	if !cp.IsHidden {
		c.Users.joinedChannels[cp.ID] = struct{}{}
	}

	if cp.LastViewedAt > c.Users.channelLastViewedAt[cp.ID] {
		c.Users.channelLastViewedAt[cp.ID] = cp.LastViewedAt
	}

	if len(cp.Members) > 0 {
		memberSet := make(map[string]struct{}, len(cp.Members))
		for _, id := range cp.Members {
			memberSet[id] = struct{}{}
		}

		c.Users.channelMembers[cp.ID] = memberSet
	}

	if len(cp.Admins) > 0 {
		adminSet := make(map[string]struct{}, len(cp.Admins))
		for _, id := range cp.Admins {
			adminSet[id] = struct{}{}
		}

		c.Users.channelAdmins[cp.ID] = adminSet
	}
}

func (c *Client) upsertConversationFromProto(g *protoGroup) *Conversation {
	if g == nil || g.Group.ID == "" {
		return nil
	}

	resp := &protoGetGroupResp{Group: g}

	return c.mergeGetGroupResponse(g.Group.ID, g.Group, resp)
}

func (c *Client) hasCachedConversation(convID string) bool {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	_, ok := c.Users.conversations[convID]

	return ok
}

func (c *Client) protoGroupRefFor(convID string) protoGroupRef {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	_, isDM := c.Users.dmGroupIDs[convID]

	return protoGroupRef{
		ID:   convID,
		IsDM: isDM,
	}
}

func (c *Client) allGroupRefs() []protoGroupRef {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	refs := make([]protoGroupRef, 0, len(c.Users.conversations))

	for id, conv := range c.Users.conversations {
		if conv == nil || conv.IsHidden {
			continue
		}

		refs = append(refs, protoGroupRef{
			ID:   id,
			IsDM: conv.IsDMGroup,
		})
	}

	return refs
}

// GetChannels returns copies of all cached Spaces and DMs.
func (c *Client) GetChannels() []*Conversation {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	out := make([]*Conversation, 0, len(c.Users.conversations))

	for _, conv := range c.Users.conversations {
		if conv != nil {
			out = append(out, cloneConversation(conv))
		}
	}

	return out
}

// GetChannel returns a copy of the cached conversation for id, or nil.
func (c *Client) GetChannel(id string) *Conversation {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return cloneConversation(c.Users.conversations[id])
}

// GetChannelCount returns the number of cached conversations.
func (c *Client) GetChannelCount() int {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return len(c.Users.conversations)
}

// IsChannelMember reports whether userID is cached as a member of convID.
func (c *Client) IsChannelMember(convID, userID string) bool {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	members := c.Users.channelMembers[convID]
	if members == nil {
		return false
	}

	_, ok := members[userID]

	return ok
}

// IsChannelAdmin reports whether userID is cached as an owner or manager of convID.
func (c *Client) IsChannelAdmin(convID, userID string) bool {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	admins := c.Users.channelAdmins[convID]
	if admins == nil {
		return false
	}

	_, ok := admins[userID]

	return ok
}

// GetChannelLastViewedAt returns the cached last-viewed timestamp in Unix milliseconds.
func (c *Client) GetChannelLastViewedAt(convID string) int64 {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	return c.Users.channelLastViewedAt[convID]
}

// GetChannelMembers returns the cached member user IDs for convID.
func (c *Client) GetChannelMembers(convID string) []string {
	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	conv := c.Users.conversations[convID]
	if conv == nil {
		return nil
	}

	return slices.Clone(conv.Members)
}

// GetDMConversationID returns the cached 1:1 DM conversation ID with userID, or "".
func (c *Client) GetDMConversationID(userID string) string {
	if userID == "" {
		return ""
	}

	myID := c.selfUserID()

	c.Users.mu.RLock()
	defer c.Users.mu.RUnlock()

	for _, conv := range c.Users.conversations {
		if conv == nil {
			continue
		}

		if userID == myID && conv.Type == ConversationSelfNote {
			return conv.ID
		}

		if conv.Type != ConversationDM {
			continue
		}

		if slices.Contains(conv.Members, userID) {
			return conv.ID
		}
	}

	return ""
}

// GetOrCreateDM returns an existing 1:1 or Group DM with memberIDs, or creates one.
func (c *Client) GetOrCreateDM(ctx context.Context, memberIDs []string) (*Conversation, error) {
	myID := c.selfUserID()

	var (
		userIDs []string
		emails  []string
	)

	for _, raw := range memberIDs {
		id := strings.TrimSpace(raw)
		if id == "" || id == myID {
			continue
		}

		if strings.Contains(id, "@") {
			emails = append(emails, id)
		} else {
			userIDs = append(userIDs, id)
		}
	}

	userIDs = dedupStrings(userIDs)
	emails = dedupStrings(emails)

	if len(userIDs) == 0 && len(emails) == 0 {
		return nil, errors.New("googlechatclient: no target users specified for DM")
	}

	if len(userIDs) == 1 && len(emails) == 0 {
		if existingID := c.GetDMConversationID(userIDs[0]); existingID != "" {
			if conv := c.GetChannel(existingID); conv != nil {
				return conv, nil
			}
		}
	}

	respBytes, err := c.doProto(ctx, "create_dm", encodeCreateDMRequest(userIDs, emails))
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: create_dm: %w", err)
	}

	group, memberships, err := decodeCreateDMResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode create_dm: %w", err)
	}

	if group == nil || group.Group.ID == "" {
		return nil, errors.New("googlechatclient: create_dm returned empty group")
	}

	group.Group.IsDM = true

	resp := &protoGetGroupResp{
		Group:       group,
		Memberships: memberships,
	}

	conv := c.mergeGetGroupResponse(group.Group.ID, group.Group, resp)
	c.subscribeGroup(ctx, group.Group)

	return conv, nil
}

// CreateSpace creates a new modern inline-threaded Google Chat Space.
func (c *Client) CreateSpace(ctx context.Context, name string) (*Conversation, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, errors.New("googlechatclient: space name must not be empty")
	}

	respBytes, err := c.doProto(ctx, "create_group", encodeCreateGroupRequest(trimmed))
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: create_group: %w", err)
	}

	group, err := decodeCreateGroupResponse(respBytes)
	if err != nil {
		return nil, fmt.Errorf("googlechatclient: decode create_group: %w", err)
	}

	if group == nil || group.Group.ID == "" {
		return nil, errors.New("googlechatclient: create_group returned empty group")
	}

	conv := c.upsertConversationFromProto(group)
	c.subscribeGroup(ctx, group.Group)

	return conv, nil
}

// AddChannelMember adds a user (by numeric user ID or email) to a Space.
func (c *Client) AddChannelMember(ctx context.Context, convID, userIDOrEmail string) error {
	if convID == "" || strings.TrimSpace(userIDOrEmail) == "" {
		return errors.New("googlechatclient: conversation ID and user ID are required")
	}

	ref := c.protoGroupRefFor(convID)

	var userID, email string
	if strings.Contains(userIDOrEmail, "@") {
		email = strings.TrimSpace(userIDOrEmail)
	} else {
		userID = strings.TrimSpace(userIDOrEmail)
	}

	_, err := c.doProto(ctx, "create_membership", encodeCreateMembershipRequest(ref, userID, email))
	if err != nil {
		return fmt.Errorf("googlechatclient: create_membership: %w", err)
	}

	if userID != "" {
		c.addChannelMemberLocal(convID, userID, false)
	}

	return nil
}

// RemoveChannelMember removes userID from a Space.
func (c *Client) RemoveChannelMember(ctx context.Context, convID, userID string) error {
	if convID == "" || userID == "" {
		return errors.New("googlechatclient: conversation ID and user ID are required")
	}

	ref := c.protoGroupRefFor(convID)

	_, err := c.doProto(ctx, "remove_memberships", encodeRemoveMembershipsRequest(ref, userID))
	if err != nil {
		return fmt.Errorf("googlechatclient: remove_memberships: %w", err)
	}

	c.removeChannelMemberLocal(convID, userID)

	return nil
}

// LeaveChannel leaves a Space (via remove_memberships) or hides a DM (via hide_group).
func (c *Client) LeaveChannel(ctx context.Context, convID string) error {
	if convID == "" {
		return errors.New("googlechatclient: empty conversation ID")
	}

	ref := c.protoGroupRefFor(convID)

	if ref.IsDM {
		_, err := c.doProto(ctx, "hide_group", encodeHideGroupRequest(ref, true))
		if err != nil {
			return fmt.Errorf("googlechatclient: hide_group: %w", err)
		}
	} else {
		myID := c.selfUserID()
		if myID == "" {
			return errors.New("googlechatclient: self user ID not known")
		}

		_, err := c.doProto(ctx, "remove_memberships", encodeRemoveMembershipsRequest(ref, myID))
		if err != nil {
			return fmt.Errorf("googlechatclient: leave space: %w", err)
		}
	}

	c.Users.mu.Lock()
	delete(c.Users.joinedChannels, convID)

	if conv := c.Users.conversations[convID]; conv != nil {
		conv.IsHidden = true
	}

	c.Users.mu.Unlock()

	return nil
}

// SetChannelTopic updates the description of a Google Chat Space.
func (c *Client) SetChannelTopic(ctx context.Context, convID, topic string) error {
	if convID == "" {
		return errors.New("googlechatclient: empty conversation ID")
	}

	ref := c.protoGroupRefFor(convID)
	if ref.IsDM {
		return errors.New("googlechatclient: cannot set description on a DM")
	}

	_, err := c.doProto(ctx, "update_group", encodeUpdateGroupDescriptionRequest(convID, topic))
	if err != nil {
		return fmt.Errorf("googlechatclient: update_group: %w", err)
	}

	c.Users.mu.Lock()

	if conv := c.Users.conversations[convID]; conv != nil {
		conv.Topic = topic
	}

	c.Users.mu.Unlock()

	return nil
}

// MarkConversationRead updates Google Chat's read watermark for convID.
func (c *Client) MarkConversationRead(ctx context.Context, convID string, ts time.Time) error {
	if convID == "" {
		return errors.New("googlechatclient: empty conversation ID")
	}

	if ts.IsZero() {
		ts = time.Now()
	}

	ref := c.protoGroupRefFor(convID)

	_, err := c.doProto(ctx, "mark_group_readstate", encodeMarkGroupReadstateRequest(ref, ts.UnixMicro()))
	if err != nil {
		return fmt.Errorf("googlechatclient: mark_group_readstate: %w", err)
	}

	c.updateChannelLastViewed(convID, ts.UnixMilli())
	c.subscribeGroup(ctx, ref)

	return nil
}

func (c *Client) updateChannelLastViewed(convID string, ms int64) {
	if convID == "" || ms <= 0 {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	if ms > c.Users.channelLastViewedAt[convID] {
		c.Users.channelLastViewedAt[convID] = ms
	}

	if conv := c.Users.conversations[convID]; conv != nil && ms > conv.LastViewedAt {
		conv.LastViewedAt = ms
	}
}

func (c *Client) addChannelMemberLocal(convID, userID string, isAdmin bool) {
	if convID == "" || userID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	if c.Users.channelMembers[convID] == nil {
		c.Users.channelMembers[convID] = make(map[string]struct{})
	}

	c.Users.channelMembers[convID][userID] = struct{}{}

	if isAdmin {
		if c.Users.channelAdmins[convID] == nil {
			c.Users.channelAdmins[convID] = make(map[string]struct{})
		}

		c.Users.channelAdmins[convID][userID] = struct{}{}
	}

	if conv := c.Users.conversations[convID]; conv != nil {
		if !slices.Contains(conv.Members, userID) {
			conv.Members = append(conv.Members, userID)
		}

		if isAdmin && !slices.Contains(conv.Admins, userID) {
			conv.Admins = append(conv.Admins, userID)
		}
	}
}

func (c *Client) removeChannelMemberLocal(convID, userID string) {
	if convID == "" || userID == "" {
		return
	}

	c.Users.mu.Lock()
	defer c.Users.mu.Unlock()

	if members := c.Users.channelMembers[convID]; members != nil {
		delete(members, userID)
	}

	if admins := c.Users.channelAdmins[convID]; admins != nil {
		delete(admins, userID)
	}

	if conv := c.Users.conversations[convID]; conv != nil {
		conv.Members = slices.DeleteFunc(conv.Members, func(s string) bool { return s == userID })
		conv.Admins = slices.DeleteFunc(conv.Admins, func(s string) bool { return s == userID })
	}
}

func (c *Client) selfUserID() string {
	c.RLock()
	defer c.RUnlock()

	if c.User != nil {
		return c.User.ID
	}

	return ""
}

func cloneConversation(in *Conversation) *Conversation {
	if in == nil {
		return nil
	}

	cp := *in
	cp.Members = slices.Clone(in.Members)
	cp.Admins = slices.Clone(in.Admins)

	return &cp
}

func dedupStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if s == "" {
			continue
		}

		if _, ok := seen[s]; ok {
			continue
		}

		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}

func usecToMillis(usec int64) int64 {
	if usec <= 0 {
		return 0
	}

	return usec / 1000
}
