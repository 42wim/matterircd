package matrix

import (
	"sync"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type User struct {
	ID id.UserID
	*event.MemberEventContent
}

type Channel struct {
	ID         id.RoomID
	Alias      id.RoomAlias
	AltAliases []id.RoomAlias
	Members    map[id.UserID]*User
	IsDirect   bool
	Encrypted  bool
	Topic      string
	sync.RWMutex
}
