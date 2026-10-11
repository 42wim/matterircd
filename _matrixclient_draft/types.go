package matrixclient

import (
	"sync"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Credentials used to log in to a homeserver.
type Credentials struct {
	Server string
	Login  string
	Pass   string
}

// User is a Matrix user as seen in room state.
type User struct {
	*event.MemberEventContent

	ID id.UserID
}

// Channel is a Matrix room.
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

// Message is an incoming room or direct message.
type Message struct {
	RoomID    id.RoomID
	EventID   id.EventID
	ParentID  id.EventID
	Sender    id.UserID
	Text      string
	Direct    bool
	Timestamp int64
}

// Reaction is an incoming reaction to an event.
type Reaction struct {
	RoomID   id.RoomID
	ParentID id.EventID
	Sender   id.UserID
	Key      string
	Direct   bool
}

// Handler receives events from the client. All methods are called from the
// sync goroutine and must not block for long.
type Handler interface {
	// Ready is called once, after the initial sync completed.
	Ready()
	OnMessage(msg *Message)
	OnReaction(r *Reaction)
}
