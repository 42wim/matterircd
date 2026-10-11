package matrix

import (
	"sync"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type User struct {
	*event.MemberEventContent

	ID id.UserID
}

type Channel struct {
	sync.RWMutex

	ID         id.RoomID
	Alias      id.RoomAlias
	AltAliases []id.RoomAlias
	Members    map[id.UserID]*User
	IsDirect   bool
	Encrypted  bool
	Topic      string
}
