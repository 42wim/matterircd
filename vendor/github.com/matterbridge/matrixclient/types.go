package matrixclient

import (
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Credentials used to log in to a homeserver.
type Credentials struct {
	Server string
	Login  string
	Pass   string

	// Token logs in with an existing access token instead of Login and Pass.
	// The token cannot be renewed, so it is not refreshed once it expires.
	Token string
	// DeviceID is the device the token belongs to. Optional, looked up when empty,
	// and a mismatch with the device the server reports for the token is an error.
	// With e2ee, the key database is bound to this device: a different device
	// fails to start instead of reusing or overwriting the stored keys.
	DeviceID id.DeviceID
	// DeviceName is the session name other clients list for this device, instead of
	// the generated device ID. Optional. It is also applied to an existing device.
	DeviceName string
}

// User is a Matrix user as seen in room state.
type User struct {
	*event.MemberEventContent

	ID id.UserID

	Bot   bool // see Client.IsBot
	human bool
}

// Channel is a Matrix room.
type Channel struct {
	sync.RWMutex

	ID         id.RoomID
	Name       string
	RawName    string // the room name as set, unlike Name without spaces and with a '#'
	Alias      id.RoomAlias
	AltAliases []id.RoomAlias
	Members    map[id.UserID]*User
	IsDirect   bool
	Encrypted  bool
	Topic      string

	nameChecked bool // the server confirmed the room has no name or alias, see RefreshRoomName
	// nameRev and aliasRev count the sync events for the name and alias, clears included,
	// so a lookup does not restore a value a newer sync event removed.
	nameRev, aliasRev uint64

	// joined and invited member counts from the sync summary, which omits each when unchanged
	joinedCount, invitedCount int
	joinedSeen, invitedSeen   bool

	joinRule event.JoinRule // from the join_rules state, "" until seen
	// worldReadable is set by the history_visibility state, anyone can read such a room without joining.
	worldReadable bool

	heroes       []id.UserID // members the server suggests to name the room after, from the sync summary
	reportedName string      // channel name last reported to the consumer, see RoomNameHandler
	delivering   bool        // a name callback is running, see reportNameLocked
	renameAgain  bool        // the name was reported while delivering, re-evaluate it afterwards
	nameGaveUp   bool        // healing ended without a name (forbidden, or out of attempts), see healRoomName
	selfJoined   bool        // we are joined, as last seen in our member event
	nameEpoch    uint64      // incremented when we join or leave, lookups of an older epoch are discarded

	LastActivity int64      // timestamp in milliseconds of the newest message seen, 0 if none
	LastEventID  id.EventID // newest message event seen, set during the initial sync as well

	nextSeq, lastSeq uint64                    // sync timeline positions, lastSeq belongs to LastEventID
	pruneAt          int                       // eventSeq size that triggers the next prune
	eventSeq         map[id.EventID]pendingSeq // positions of messages not yet delivered to handleMessage
}

// pendingSeq is the sync timeline position of a message and when it was seen.
type pendingSeq struct {
	seq uint64
	at  time.Time
}

// pendingAssoc is a direct room association not yet seen in a synced m.direct.
type pendingAssoc struct {
	user    id.UserID
	written bool // the write succeeded, it stays pending until a sync confirms it
}

// Message is an incoming room or direct message. Messages sent by ourselves
// from other sessions are delivered too, compare Sender with Client.UserID.
type Message struct {
	RoomID    id.RoomID
	EventID   id.EventID
	ParentID  id.EventID
	Sender    id.UserID
	Text      string
	Direct    bool
	Timestamp int64
	MsgType   event.MessageType
	URL       id.ContentURIString // mxc:// URI of m.file, m.image, m.audio and m.video messages
	FileName  string
	Info      *event.FileInfo
	File      *event.EncryptedFileInfo // set instead of URL for encrypted media, see Client.DownloadEncrypted
}

// Edit is an incoming edit of a message.
type Edit struct {
	RoomID    id.RoomID
	EventID   id.EventID
	TargetID  id.EventID // the edited message
	Sender    id.UserID
	Text      string
	Direct    bool
	Timestamp int64
}

// Redaction is an incoming deletion of an event, which can be a message or a
// reaction. Redactions from ourselves are delivered too, compare Sender with
// Client.UserID.
type Redaction struct {
	RoomID   id.RoomID
	TargetID id.EventID
	Sender   id.UserID
	Direct   bool
}

// Typing is the full list of users currently typing in a room, excluding ourselves.
type Typing struct {
	RoomID id.RoomID
	Users  []id.UserID
	Direct bool
}

// MemberChange is a membership change of a user in a room.
type MemberChange struct {
	RoomID     id.RoomID
	UserID     id.UserID
	Sender     id.UserID // who caused the change, differs from UserID on invites, kicks and bans
	Membership event.Membership
	Reason     string
}

// Invitation is a pending invitation for Client.UserID. Name, Alias and Reason
// may be empty; RoomID is the authoritative target to pass to Client.Join.
type Invitation struct {
	RoomID id.RoomID
	Sender id.UserID
	Name   string
	Alias  id.RoomAlias
	Reason string
	Direct bool
}

// TopicChange is a room topic change.
type TopicChange struct {
	RoomID id.RoomID
	Sender id.UserID
	Topic  string
}

// Reaction is an incoming reaction to an event. Reactions from ourselves are
// delivered too, compare Sender with Client.UserID.
type Reaction struct {
	RoomID   id.RoomID
	ParentID id.EventID
	Sender   id.UserID
	Key      string
	Direct   bool
}

// Handler receives events from the client. All methods, including Ready, are
// called from the sync goroutine and must not block for long. The exception
// are encrypted messages whose room key arrives late: they are delivered from
// another goroutine once decrypted, possibly after Ready, so a handler must
// be safe for concurrent use. VerificationHandler has separate concurrency and
// reentrancy rules; see its documentation.
type Handler interface {
	// Ready is called once, after the events of the initial sync were processed.
	// The messages, reactions, redactions and typing events of the initial sync
	// are not delivered, as a handler typically only starts consuming them once
	// Ready was called. Use the history methods to fetch earlier messages.
	Ready()
	OnMessage(msg *Message)
	OnReaction(r *Reaction)
}

// EditHandler can optionally be implemented by a Handler to receive edits.
// Without it, edits are delivered to OnMessage as a message replying to the
// edited event.
type EditHandler interface {
	OnEdit(e *Edit)
}

// RedactionHandler can optionally be implemented by a Handler to receive deletions.
type RedactionHandler interface {
	OnRedaction(r *Redaction)
}

// MemberHandler can optionally be implemented by a Handler to receive
// membership changes. Changes from the initial sync are not delivered.
type MemberHandler interface {
	OnMember(m *MemberChange)
}

// InviteHandler can optionally be implemented by a Handler to receive new or
// changed invitations after the initial sync. Initial invitations are available
// through Client.Invitations when Ready is called. Callbacks must not block.
type InviteHandler interface {
	OnInvite(invite *Invitation)
}

// RoomRename is a change of the channel name of a room, as listed by Client.Rooms.
// Like matterircd's ChannelUpdateEvent for Mattermost it carries the room ID, the
// new channel name and the display name. A consumer is expected to handle it the
// same way: PART the channel of OldName with "Channel renamed to <Name>", JOIN
// Name and send a server notice. Direct rooms are never reported.
type RoomRename struct {
	RoomID      id.RoomID
	OldName     string
	Name        string
	DisplayName string // the room name as set, empty if it has none
}

// RoomNameHandler can optionally be implemented by a Handler to be told when the
// channel name of a room changes through the sync, the counterpart of the channel
// update event of Mattermost. Only rooms whose name was reported through
// Client.Rooms or Client.ResolveRoomName are delivered. A name healed in the
// background is reported through the WithRoomNameResolved callback instead.
type RoomNameHandler interface {
	OnRoomName(r *RoomRename)
}

// TopicHandler can optionally be implemented by a Handler to receive topic
// changes. Changes from the initial sync are not delivered.
type TopicHandler interface {
	OnTopic(t *TopicChange)
}

// TypingHandler can optionally be implemented by a Handler to receive typing notifications.
type TypingHandler interface {
	OnTyping(t *Typing)
}
