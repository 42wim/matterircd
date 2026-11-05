package matrix

import (
	"slices"
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

// uniqueRoomNames returns the channel name, without '#', of every room. Names are
// compared case-insensitively as in IRC. The room with the smallest ID keeps a shared
// name, the others get a suffix of their room ID, so the result does not depend on
// the map order of rooms. rooms is sorted by ID.
func uniqueRoomNames(rooms []matrixclient.RoomInfo) map[id.RoomID]string {
	slices.SortFunc(rooms, func(a, b matrixclient.RoomInfo) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})

	names := make(map[id.RoomID]string, len(rooms))
	used := make(map[string]struct{}, len(rooms))

	for _, room := range rooms {
		base := strings.TrimPrefix(room.Name, "#")
		local, _, _ := strings.Cut(strings.TrimPrefix(room.ID.String(), "!"), ":")
		name := base

		for n := nameSuffixLen; ; n++ {
			if _, taken := used[strings.ToLower(name)]; !taken {
				break
			}

			if n >= len(local) {
				name = base + "-" + local

				break
			}

			name = base + "-" + local[:n]
		}

		used[strings.ToLower(name)] = struct{}{}
		names[room.ID] = name
	}

	return names
}

// roomsWithNames returns the rooms sorted by ID and their unique channel names.
func (m *Matrix) roomsWithNames() ([]matrixclient.RoomInfo, map[id.RoomID]string) {
	rooms := m.mc.Rooms()

	return rooms, uniqueRoomNames(rooms)
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
