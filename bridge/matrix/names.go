package matrix

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/matterbridge/matrixclient"
)

const (
	nameSuffixLen = 8

	// The synchronous room name lookups of all rooms share a budget per window, so many
	// unnamed rooms on a slow homeserver cannot stall the channel creation for long.
	roomNameResolveBudget = 30 * time.Second
	roomNameBudgetWindow  = 5 * time.Minute
	roomNameResolveRetry  = 5 * time.Minute
)

// nameBudget limits the time spent in synchronous room name lookups.
type nameBudget struct {
	mu          sync.Mutex
	windowStart time.Time
	spent       time.Duration
}

func (b *nameBudget) remaining() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	if time.Since(b.windowStart) > roomNameBudgetWindow {
		b.windowStart, b.spent = time.Now(), 0
	}

	return roomNameResolveBudget - b.spent
}

func (b *nameBudget) spend(d time.Duration) {
	b.mu.Lock()
	b.spent += d
	b.mu.Unlock()
}

// roomName is the channel name given to a room, and the name it was derived from.
type roomName struct {
	base string
	name string
}

// assignRoomNames returns the unique channel name, without '#', of every room. Names
// are compared case-insensitively as in IRC. An assignment is kept while the base name
// of its room is unchanged, so a room never loses its name to another one and only a
// room whose own name changed is renamed. Rooms that need a name take it in ID order,
// a taken one gets a suffix of its room ID, so the result does not depend on the map
// order of rooms. The rooms are sorted by ID.
func (m *Matrix) assignRoomNames(rooms []matrixclient.RoomInfo) map[id.RoomID]string {
	slices.SortFunc(rooms, func(a, b matrixclient.RoomInfo) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})

	m.roomNamesMu.Lock()
	defer m.roomNamesMu.Unlock()

	if m.roomNames == nil {
		m.roomNames = make(map[id.RoomID]roomName)
	}

	present := make(map[id.RoomID]struct{}, len(rooms))
	names := make(map[id.RoomID]string, len(rooms))
	used := make(map[string]struct{}, len(rooms))

	for _, room := range rooms {
		present[room.ID] = struct{}{}

		kept, ok := m.roomNames[room.ID]
		if ok && kept.base == strings.TrimPrefix(room.Name, "#") {
			names[room.ID] = kept.name
			used[strings.ToLower(kept.name)] = struct{}{}
		}
	}

	for roomID := range m.roomNames {
		if _, ok := present[roomID]; !ok {
			delete(m.roomNames, roomID)
		}
	}

	for _, room := range rooms {
		if _, done := names[room.ID]; done {
			continue
		}

		base := strings.TrimPrefix(room.Name, "#")
		name := freeRoomName(base, room.ID, used)

		used[strings.ToLower(name)] = struct{}{}
		names[room.ID] = name
		m.roomNames[room.ID] = roomName{base: base, name: name}
	}

	return names
}

// freeRoomName returns base, or base with a suffix of the room ID, that is not in used.
// The suffix grows up to the whole ID and then gets a counter, so a free name is always found.
func freeRoomName(base string, roomID id.RoomID, used map[string]struct{}) string {
	free := func(name string) bool {
		_, taken := used[strings.ToLower(name)]

		return !taken
	}

	if free(base) {
		return base
	}

	local, _, _ := strings.Cut(strings.TrimPrefix(roomID.String(), "!"), ":")

	for n := min(nameSuffixLen, len(local)); n <= len(local); n++ {
		if name := base + "-" + local[:n]; free(name) {
			return name
		}
	}

	for n := 2; ; n++ {
		if name := base + "-" + local + "-" + strconv.Itoa(n); free(name) {
			return name
		}
	}
}

// roomsWithNames returns the rooms sorted by ID and their unique channel names.
func (m *Matrix) roomsWithNames() ([]matrixclient.RoomInfo, map[id.RoomID]string) {
	rooms := m.mc.Rooms()

	return rooms, m.assignRoomNames(rooms)
}

// roomLabel describes a room for logs as "#name (ID)", or just the ID while it has no
// name yet. It only reads the cache, so it never blocks on the network.
func (m *Matrix) roomLabel(roomID id.RoomID) string {
	m.roomNamesMu.Lock()
	rn, ok := m.roomNames[roomID]
	m.roomNamesMu.Unlock()

	if !ok {
		return roomID.String()
	}

	return "#" + rn.name + " (" + roomID.String() + ")"
}

// nameResolveDue reports whether the room should be looked up now, and records it.
// A room is looked up again after roomNameResolveRetry, as long as it has no name.
func (m *Matrix) nameResolveDue(roomID id.RoomID) bool {
	if v, ok := m.nameResolveTried.Load(roomID); ok {
		last, isTime := v.(time.Time)
		if isTime && time.Since(last) < roomNameResolveRetry {
			return false
		}
	}

	m.nameResolveTried.Store(roomID, time.Now())

	return true
}
