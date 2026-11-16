package rocketchatclient

import (
	"slices"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

type stateCache struct {
	mu sync.RWMutex

	usersByID    map[string]*User
	usersByName  map[string]*User
	roomsByID    map[string]*Room
	subsByRoomID map[string]*Subscription
	invitations  map[string]Invitation

	sentIDs       *lru.Cache[string, struct{}]
	msgCache      *lru.Cache[string, *Message]
	reactionCache *lru.Cache[string, map[string][]string]
}

func newStateCache(sentSize, msgSize, reactionSize int) (*stateCache, error) {
	sentIDs, err := lru.New[string, struct{}](sentSize)
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
		invitations:   make(map[string]Invitation),
		sentIDs:       sentIDs,
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
	delete(sc.invitations, roomID)
	sc.mu.Unlock()
}

func (sc *stateCache) upsertSubscription(s *Subscription) {
	if s == nil || s.RoomID == "" {
		return
	}

	cp := cloneSubscription(s)

	sc.mu.Lock()
	sc.subsByRoomID[cp.RoomID] = cp
	delete(sc.invitations, cp.RoomID)
	sc.mu.Unlock()
}

func (sc *stateCache) removeSubscription(roomID string) {
	if roomID == "" {
		return
	}

	sc.mu.Lock()
	delete(sc.subsByRoomID, roomID)
	sc.mu.Unlock()
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

func (sc *stateCache) invitationsSnapshot() []Invitation {
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	out := make([]Invitation, 0, len(sc.invitations))
	for _, inv := range sc.invitations {
		out = append(out, inv)
	}

	return out
}
