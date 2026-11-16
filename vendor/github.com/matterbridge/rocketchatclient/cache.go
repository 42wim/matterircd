package rocketchatclient

import (
	"slices"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

type seenMessageMeta struct {
	text     string
	editedAt int64
}

type stateCache struct {
	mu sync.RWMutex

	usersByID    map[string]*User
	usersByName  map[string]*User
	roomsByID    map[string]*Room
	subsByRoomID map[string]*Subscription
	roomRoles    map[string]map[string][]string
	invitations  map[string]Invitation

	sentIDs       *lru.Cache[string, struct{}]
	seenMsgs      *lru.Cache[string, seenMessageMeta]
	msgCache      *lru.Cache[string, *Message]
	reactionCache *lru.Cache[string, map[string][]string]
}

func newStateCache(sentSize, msgSize, reactionSize int) (*stateCache, error) {
	sentIDs, err := lru.New[string, struct{}](sentSize)
	if err != nil {
		return nil, err
	}

	seenMsgs, err := lru.New[string, seenMessageMeta](msgSize)
	if err != nil {
		return nil, err
	}

	msgCache, err := lru.New[string, *Message](msgSize)
	if err != nil {
		return nil, err
	}

	reactionCache, err := lru.New[string, map[string][]string](reactionSize)
	if err != nil {
		return nil, err
	}

	return &stateCache{
		usersByID:     make(map[string]*User),
		usersByName:   make(map[string]*User),
		roomsByID:     make(map[string]*Room),
		subsByRoomID:  make(map[string]*Subscription),
		roomRoles:     make(map[string]map[string][]string),
		invitations:   make(map[string]Invitation),
		sentIDs:       sentIDs,
		seenMsgs:      seenMsgs,
		msgCache:      msgCache,
		reactionCache: reactionCache,
	}, nil
}

func cloneUser(u *User) *User {
	if u == nil {
		return nil
	}

	cp := *u
	if u.Roles != nil {
		cp.Roles = slices.Clone(u.Roles)
	}

	return &cp
}

func cloneRoom(r *Room) *Room {
	if r == nil {
		return nil
	}

	cp := *r
	if r.UIDs != nil {
		cp.UIDs = slices.Clone(r.UIDs)
	}

	if r.Usernames != nil {
		cp.Usernames = slices.Clone(r.Usernames)
	}

	if r.Owner != nil {
		owner := *r.Owner
		cp.Owner = &owner
	}

	return &cp
}

func cloneSubscription(s *Subscription) *Subscription {
	if s == nil {
		return nil
	}

	cp := *s
	if s.Roles != nil {
		cp.Roles = slices.Clone(s.Roles)
	}

	if s.User != nil {
		u := *s.User
		cp.User = &u
	}

	return &cp
}

func (sc *stateCache) upsertUser(u *User) {
	if u == nil || u.ID == "" {
		return
	}

	cp := cloneUser(u)

	sc.mu.Lock()
	defer sc.mu.Unlock()

	if prev, ok := sc.usersByID[cp.ID]; ok && prev.Username != "" && prev.Username != cp.Username {
		delete(sc.usersByName, prev.Username)
	}

	sc.usersByID[cp.ID] = cp

	if cp.Username != "" {
		sc.usersByName[cp.Username] = cp
	}
}

func (sc *stateCache) upsertUsers(users []User) {
	if len(users) == 0 {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	for i := range users {
		if users[i].ID == "" {
			continue
		}

		cp := cloneUser(&users[i])

		if prev, ok := sc.usersByID[cp.ID]; ok && prev.Username != "" && prev.Username != cp.Username {
			delete(sc.usersByName, prev.Username)
		}

		sc.usersByID[cp.ID] = cp

		if cp.Username != "" {
			sc.usersByName[cp.Username] = cp
		}
	}
}

func (sc *stateCache) seedUserRef(ref UserRef) {
	if ref.ID == "" || ref.Username == "" || ref.ID == "all" || ref.ID == "here" {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	if _, ok := sc.usersByID[ref.ID]; ok {
		return
	}

	u := &User{
		ID:       ref.ID,
		Username: ref.Username,
		Name:     ref.Name,
	}

	sc.usersByID[u.ID] = u
	sc.usersByName[u.Username] = u
}

func (sc *stateCache) updateUserPresence(userID, username, status, statusText string) (string, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	u, ok := sc.usersByID[userID]
	if !ok {
		if userID == "" {
			return "", false
		}

		u = &User{
			ID:         userID,
			Username:   username,
			Status:     status,
			StatusText: statusText,
			Active:     true,
		}
		sc.usersByID[userID] = u

		if username != "" {
			sc.usersByName[username] = u
		}

		return "", true
	}

	prev := u.Status
	u.Status = status

	if statusText != "" {
		u.StatusText = statusText
	}

	if username != "" && u.Username != username {
		if u.Username != "" {
			delete(sc.usersByName, u.Username)
		}

		u.Username = username
		sc.usersByName[username] = u
	}

	return prev, prev != status
}

func (sc *stateCache) updateUserName(userID, name, username string) {
	if userID == "" {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	u, ok := sc.usersByID[userID]
	if !ok {
		u = &User{
			ID:       userID,
			Name:     name,
			Username: username,
			Active:   true,
		}
		sc.usersByID[userID] = u

		if username != "" {
			sc.usersByName[username] = u
		}

		return
	}

	if name != "" {
		u.Name = name
	}

	if username != "" && u.Username != username {
		if u.Username != "" {
			delete(sc.usersByName, u.Username)
		}

		u.Username = username
		sc.usersByName[username] = u
	}
}

func (sc *stateCache) upsertRoom(r *Room) {
	if r == nil || r.ID == "" {
		return
	}

	cp := cloneRoom(r)

	sc.mu.Lock()
	sc.roomsByID[cp.ID] = cp
	sc.mu.Unlock()
}

func (sc *stateCache) removeRoom(roomID string) {
	if roomID == "" {
		return
	}

	sc.mu.Lock()
	delete(sc.roomsByID, roomID)
	delete(sc.subsByRoomID, roomID)
	delete(sc.roomRoles, roomID)
	delete(sc.invitations, roomID)
	sc.mu.Unlock()
}

func (sc *stateCache) upsertSubscription(s *Subscription) bool {
	if s == nil || s.RoomID == "" {
		return false
	}

	cp := cloneSubscription(s)

	sc.mu.Lock()
	_, existed := sc.subsByRoomID[cp.RoomID]
	sc.subsByRoomID[cp.RoomID] = cp
	delete(sc.invitations, cp.RoomID)
	sc.mu.Unlock()

	return existed
}

func (sc *stateCache) removeSubscription(roomID string) {
	if roomID == "" {
		return
	}

	sc.mu.Lock()
	delete(sc.subsByRoomID, roomID)
	sc.mu.Unlock()
}

func (sc *stateCache) markRoomRead(roomID string, seenAt time.Time) {
	if roomID == "" {
		return
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	sub, ok := sc.subsByRoomID[roomID]
	if !ok || sub == nil {
		return
	}

	if seenAt.After(sub.LastSeen.Time) {
		sub.LastSeen = Timestamp{Time: seenAt}
	}

	sub.Alert = false
	sub.Unread = 0
}

func (sc *stateCache) hydrateRoomsAndSubs(rooms []Room, subs []Subscription) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	for i := range rooms {
		if rooms[i].ID == "" {
			continue
		}

		sc.roomsByID[rooms[i].ID] = cloneRoom(&rooms[i])
	}

	for i := range subs {
		if subs[i].RoomID == "" {
			continue
		}

		sc.subsByRoomID[subs[i].RoomID] = cloneSubscription(&subs[i])
		delete(sc.invitations, subs[i].RoomID)
	}
}

func (sc *stateCache) user(id string) (*User, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	u, ok := sc.usersByID[id]
	if !ok {
		return nil, false
	}

	return cloneUser(u), true
}

func (sc *stateCache) userByUsername(username string) (*User, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	u, ok := sc.usersByName[username]
	if !ok {
		return nil, false
	}

	return cloneUser(u), true
}

func (sc *stateCache) users() []*User {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	out := make([]*User, 0, len(sc.usersByID))
	for _, u := range sc.usersByID {
		out = append(out, cloneUser(u))
	}

	return out
}

func (sc *stateCache) room(id string) (*Room, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	r, ok := sc.roomsByID[id]
	if !ok {
		return nil, false
	}

	return cloneRoom(r), true
}

func (sc *stateCache) rooms() []*Room {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	out := make([]*Room, 0, len(sc.roomsByID))
	for _, r := range sc.roomsByID {
		out = append(out, cloneRoom(r))
	}

	return out
}

func (sc *stateCache) subscription(roomID string) (*Subscription, bool) {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	s, ok := sc.subsByRoomID[roomID]
	if !ok {
		return nil, false
	}

	return cloneSubscription(s), true
}

func (sc *stateCache) subscriptions() []*Subscription {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	out := make([]*Subscription, 0, len(sc.subsByRoomID))
	for _, s := range sc.subsByRoomID {
		out = append(out, cloneSubscription(s))
	}

	return out
}

func (sc *stateCache) setRoomRoles(roomID string, roles []RoomRole) {
	if roomID == "" {
		return
	}

	byUser := make(map[string][]string, len(roles))
	for i := range roles {
		if roles[i].User.ID == "" || len(roles[i].Roles) == 0 {
			continue
		}

		byUser[roles[i].User.ID] = slices.Clone(roles[i].Roles)
	}

	sc.mu.Lock()
	sc.roomRoles[roomID] = byUser
	sc.mu.Unlock()
}

func (sc *stateCache) roomUserRoles(roomID, userID string) []string {
	if roomID == "" || userID == "" {
		return nil
	}

	sc.mu.RLock()
	defer sc.mu.RUnlock()

	if byUser, ok := sc.roomRoles[roomID]; ok {
		if roles, exists := byUser[userID]; exists {
			return slices.Clone(roles)
		}
	}

	// Fallback to the self subscription roles if roomRoles wasn't fetched separately.
	if sub, ok := sc.subsByRoomID[roomID]; ok && sub.User != nil && sub.User.ID == userID && len(sub.Roles) > 0 {
		return slices.Clone(sub.Roles)
	}

	return nil
}

func (sc *stateCache) updateRoomTopic(roomID, topic string) (*Room, bool) {
	if roomID == "" {
		return nil, false
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	r, ok := sc.roomsByID[roomID]
	if !ok {
		return nil, false
	}

	r.Topic = topic

	return cloneRoom(r), true
}

func (sc *stateCache) updateRoomName(roomID, name string) (*Room, bool) {
	if roomID == "" || name == "" {
		return nil, false
	}

	sc.mu.Lock()
	defer sc.mu.Unlock()

	r, ok := sc.roomsByID[roomID]
	if !ok {
		return nil, false
	}

	r.Name = name

	return cloneRoom(r), true
}

func (sc *stateCache) invitationsSnapshot() []Invitation {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	out := make([]Invitation, 0, len(sc.invitations))
	for _, inv := range sc.invitations {
		out = append(out, inv)
	}

	return out
}

func (sc *stateCache) markSent(key string) {
	sc.sentIDs.Add(key, struct{}{})
}

func (sc *stateCache) unmarkSent(key string) {
	sc.sentIDs.Remove(key)
}

func (sc *stateCache) consumeSent(key string) bool {
	if sc.sentIDs.Contains(key) {
		sc.sentIDs.Remove(key)

		return true
	}

	return false
}

func (sc *stateCache) cacheMessage(msg *Message) {
	if msg == nil || msg.ID == "" {
		return
	}

	cp := *msg
	sc.msgCache.Add(msg.ID, &cp)
}

func (sc *stateCache) cachedMessage(id string) (*Message, bool) {
	if id == "" {
		return nil, false
	}

	msg, ok := sc.msgCache.Get(id)
	if !ok || msg == nil {
		return nil, false
	}

	cp := *msg

	return &cp, true
}

func (sc *stateCache) recordSeenMessage(msg *Message) (seenMessageMeta, bool) {
	if msg == nil || msg.ID == "" {
		return seenMessageMeta{}, false
	}

	var editedAt int64
	if msg.EditedAt != nil && !msg.EditedAt.Time.IsZero() {
		editedAt = msg.EditedAt.Time.UnixNano()
	}

	prev, seen := sc.seenMsgs.Get(msg.ID)
	sc.seenMsgs.Add(msg.ID, seenMessageMeta{
		text:     msg.Text,
		editedAt: editedAt,
	})

	return prev, seen
}

func (sc *stateCache) forgetMessage(msgID string) {
	if msgID == "" {
		return
	}

	sc.msgCache.Remove(msgID)
	sc.seenMsgs.Remove(msgID)
	sc.reactionCache.Remove(msgID)
}

func snapshotReactions(incoming map[string]ReactionSet) map[string][]string {
	if len(incoming) == 0 {
		return nil
	}

	snap := make(map[string][]string, len(incoming))
	for emoji, set := range incoming {
		if len(set.Usernames) == 0 {
			continue
		}

		snap[emoji] = slices.Clone(set.Usernames)
	}

	if len(snap) == 0 {
		return nil
	}

	return snap
}

func (sc *stateCache) seedReactions(msgID string, incoming map[string]ReactionSet) {
	if msgID == "" {
		return
	}

	snap := snapshotReactions(incoming)
	if snap == nil {
		sc.reactionCache.Remove(msgID)

		return
	}

	sc.reactionCache.Add(msgID, snap)
}

func (sc *stateCache) appendReactionDiff(events []ReactionEvent, roomID, msgID string, from, against map[string][]string, added bool) []ReactionEvent {
	for emoji, users := range from {
		var other []string
		if against != nil {
			other = against[emoji]
		}

		for _, uname := range users {
			if slices.Contains(other, uname) {
				continue
			}

			uid := ""
			if u, ok := sc.userByUsername(uname); ok {
				uid = u.ID
			}

			events = append(events, ReactionEvent{
				RoomID:    roomID,
				MessageID: msgID,
				UserID:    uid,
				Username:  uname,
				Emoji:     emoji,
				Added:     added,
			})
		}
	}

	return events
}

func (sc *stateCache) diffReactions(roomID, msgID string, incoming map[string]ReactionSet) []ReactionEvent {
	if msgID == "" {
		return nil
	}

	prev, _ := sc.reactionCache.Get(msgID)
	next := snapshotReactions(incoming)

	if next == nil {
		sc.reactionCache.Remove(msgID)
	} else {
		sc.reactionCache.Add(msgID, next)
	}

	var events []ReactionEvent

	events = sc.appendReactionDiff(events, roomID, msgID, prev, next, false)
	events = sc.appendReactionDiff(events, roomID, msgID, next, prev, true)

	return events
}
