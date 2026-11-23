package googlechatclient

import "time"

// AccountType selects or constrains the Google Chat account type.
type AccountType string

const (
	AccountAuto     AccountType = "auto"
	AccountWork     AccountType = "work"
	AccountPersonal AccountType = "personal"
)

// Event type constants emitted on Client.MessageChan.
// Naming follows matterclient's and msteamsclient's Event* conventions.
const (
	EventPosted             = "posted"
	EventPostEdited         = "post_edited"
	EventPostDeleted        = "post_deleted"
	EventReactionAdded      = "reaction_added"
	EventReactionRemoved    = "reaction_removed"
	EventTyping             = "typing"
	EventClearTyping        = "clear_typing"
	EventConversationUpdate = "conversation_update"
	EventChannelTopic       = "channel_topic"
	EventMemberAdded        = "member_added"
	EventMemberRemoved      = "member_removed"
	EventPresenceChange     = "presence_change"
	EventConnected          = "connected"
	EventDisconnected       = "disconnected"
)

// ConversationType distinguishes Google Chat Spaces, 1:1 DMs, Group DMs, and self notes.
type ConversationType string

const (
	ConversationSpace    ConversationType = "space"
	ConversationDM       ConversationType = "dm"
	ConversationGroupDM  ConversationType = "group"
	ConversationSelfNote ConversationType = "self"
)

// Credentials configures how Client authenticates to Google Chat.
type Credentials struct {
	AccountType   AccountType
	AuthUser      int
	Cookies       map[string]string
	RawCookies    string
	SkipTLSVerify bool
	TokenState    *TokenState
}

// TokenState is the serializable session snapshot passed to OnTokenUpdate
// so callers (such as matterircd) can persist rotated cookies and XSRF state
// without googlechatclient owning a database or file format.
type TokenState struct {
	AccountType    AccountType       `json:"account_type,omitempty"`
	AuthUser       int               `json:"auth_user,omitempty"`
	Cookies        map[string]string `json:"cookies,omitempty"`
	XSRFToken      string            `json:"xsrf_token,omitempty"`
	CSessionID     string            `json:"csessionid,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
	DisplayName    string            `json:"display_name,omitempty"`
	Email          string            `json:"email,omitempty"`
	OrganizationID string            `json:"organization_id,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at,omitzero"`
}

// AuthPrompt carries interactive authentication guidance surfaced to the caller.
type AuthPrompt struct {
	Message string
}

// Message is the normalized event delivered over Client.MessageChan.
type Message struct {
	Type      string
	Channel   string
	UserID    string
	Username  string
	Text      string
	Post      *ChatMessage
	Reaction  *ReactionEvent
	Presence  *Presence
	Members   []string
	Topic     string
	Timestamp time.Time
	Err       error
}

// ChatMessage represents a normalized Google Chat message (top-level or inline reply).
type ChatMessage struct {
	ID              string
	TopicID         string
	ClientMessageID string
	ConversationID  string
	RootMessageID   string
	FromID          string
	DisplayName     string
	ContentText     string
	CreateAt        int64
	EditAt          int64
	DeleteAt        int64
	IsSystem        bool
	QuotedMessage   *QuotedMessage
	Mentions        []Mention
	Attachments     []Attachment
	Reactions       map[string][]ReactionUser
}

// QuotedMessage describes the original message quoted or forwarded by a Google Chat message.
type QuotedMessage struct {
	ID             string
	ConversationID string
	TopicID        string
	FromID         string
	DisplayName    string
	Text           string
	Deleted        bool
	IsForwarded    bool
}

// Mention records an @mention target embedded in a Google Chat message.
type Mention struct {
	UserID      string
	DisplayName string
	All         bool
}

// Attachment describes a file, image, or Google Drive item attached to a message.
type Attachment struct {
	ID          string
	Name        string
	ContentType string
	URL         string
	Size        int64
}

// ReactionUser records a user who applied a reaction key at a given timestamp.
type ReactionUser struct {
	UserID string `json:"user_id"`
	Time   int64  `json:"time"`
}

// ReactionEvent describes a single added or removed emoji reaction.
type ReactionEvent struct {
	ConversationID string
	TopicID        string
	MessageID      string
	RootMessageID  string
	UserID         string
	Key            string
	Added          bool
	Timestamp      int64
}

// Conversation represents a Google Chat Space, 1:1 DM, or Group DM.
type Conversation struct {
	ID           string
	IsDMGroup    bool
	Type         ConversationType
	DisplayName  string
	Topic        string
	Members      []string
	Admins       []string
	LastPostAt   int64
	LastViewedAt int64
	IsPrivate    bool
	IsHidden     bool
}

// User represents a resolved Google Chat user profile.
type User struct {
	ID               string
	DisplayName      string
	FirstName        string
	LastName         string
	Email            string
	AvatarURL        string
	OrganizationID   string
	OrganizationName string
	IsBot            bool
	IsExternal       bool
	IsConsumer       bool
}

// Presence represents a user's availability, DND state, and custom status.
type Presence struct {
	UserID                string
	Availability          string
	DND                   bool
	CustomEmoji           string
	CustomStatus          string
	CustomStatusExpiresAt int64
	ActiveUntil           int64
	UpdatedAt             int64
}
