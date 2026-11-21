package msteamsclient

import "time"

// AccountType selects the Microsoft Teams authentication and backend cloud.
type AccountType string

const (
	AccountAuto     AccountType = "auto"
	AccountWork     AccountType = "work"
	AccountPersonal AccountType = "personal"
)

// Event type constants emitted on Client.MessageChan.
// Naming follows matterclient's Event* conventions wherever applicable.
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

// ConversationType distinguishes Teams channels, 1:1 chats, and group/meeting chats.
type ConversationType string

const (
	ConversationChannel  ConversationType = "channel"
	ConversationDM       ConversationType = "dm"
	ConversationGroup    ConversationType = "group"
	ConversationMeeting  ConversationType = "meeting"
	ConversationSelfNote ConversationType = "self"
)

// Credentials configures how Client authenticates to Microsoft Teams.
type Credentials struct {
	AccountType    AccountType
	TenantID       string
	ClientID       string
	RefreshToken   string
	SkypeToken     string
	ChatServiceURL string
	Region         string
	SkipTLSVerify  bool
	TokenState     *TokenState
}

// TokenState is the serializable session snapshot passed to OnTokenUpdate
// so callers (such as matterircd) can persist rotated refresh tokens without
// msteamsclient owning a database or file format.
type TokenState struct {
	AccountType      AccountType     `json:"account_type,omitempty"`
	TenantID         string          `json:"tenant_id,omitempty"`
	ClientID         string          `json:"client_id,omitempty"`
	RefreshToken     string          `json:"refresh_token,omitempty"`
	SkypeToken       string          `json:"skype_token,omitempty"`
	SkypeTokenExpiry time.Time       `json:"skype_token_expiry,omitzero"`
	UserMRI          string          `json:"user_mri,omitempty"`
	DisplayName      string          `json:"display_name,omitempty"`
	Email            string          `json:"email,omitempty"`
	Region           string          `json:"region,omitempty"`
	Endpoints        RegionEndpoints `json:"endpoints,omitzero"`
}

// AuthPrompt carries the OAuth2 device-code details shown to the user during
// interactive login.
type AuthPrompt struct {
	VerificationURI string
	UserCode        string
	Message         string
	ExpiresIn       int
}

// RegionEndpoints holds the regional Teams backend base URLs discovered via authsvc.
type RegionEndpoints struct {
	Region         string `json:"region,omitempty"`
	ChatService    string `json:"chat_service,omitempty"`
	ChatServiceAfd string `json:"chat_service_afd,omitempty"`
	ChatsvcAgg     string `json:"chatsvc_agg,omitempty"`
	MiddleTier     string `json:"middle_tier,omitempty"`
	Trouter        string `json:"trouter,omitempty"`
	Presence       string `json:"presence,omitempty"`
	AMS            string `json:"ams,omitempty"`
	Substrate      string `json:"substrate,omitempty"`
}

// Message is the normalized event delivered over Client.MessageChan.
type Message struct {
	Type      string
	Channel   string
	Team      string
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

// ChatMessage represents a normalized Teams chat or channel message.
type ChatMessage struct {
	ID              string
	ClientMessageID string
	ConversationID  string
	RootMessageID   string
	FromMRI         string
	DisplayName     string
	MessageType     string
	ContentType     string
	ContentHTML     string
	ContentText     string
	CreateAt        int64
	EditAt          int64
	DeleteAt        int64
	Version         string
	Mentions        []Mention
	Attachments     []Attachment
	Emotions        map[string][]ReactionUser
}

// Mention records an @mention target embedded in a Teams message.
type Mention struct {
	MRI         string
	DisplayName string
	Type        string
}

// Attachment describes a file, image, or card attached to a Teams message.
type Attachment struct {
	ID          string
	Name        string
	ContentType string
	URL         string
	Size        int64
}

// ReactionUser records a user who applied a reaction key at a given timestamp.
type ReactionUser struct {
	MRI  string `json:"mri"`
	Time int64  `json:"time"`
}

// ReactionEvent describes a single added or removed reaction derived from
// message emotion diffs.
type ReactionEvent struct {
	ConversationID string
	MessageID      string
	RootMessageID  string
	UserMRI        string
	Key            string
	Added          bool
	Timestamp      int64
}

// Conversation represents a Teams channel, 1:1 DM, group chat, or meeting chat.
type Conversation struct {
	ID           string
	Type         ConversationType
	TeamID       string
	TeamName     string
	DisplayName  string
	Topic        string
	Members      []string
	LastPostAt   int64
	LastViewedAt int64
	IsGeneral    bool
	IsPrivate    bool
	IsArchived   bool
}

// Team represents a Microsoft Teams workspace (Work/School) or Community.
type Team struct {
	ID          string
	DisplayName string
	Description string
	Channels    []*Conversation
}

// User represents a resolved Microsoft Teams user profile.
type User struct {
	MRI         string
	ObjectID    string
	DisplayName string
	GivenName   string
	Surname     string
	Email       string
	UPN         string
	JobTitle    string
	Department  string
	Location    string
	TimeZone    string
	TenantID    string
	IsBot       bool
	IsExternal  bool
}

// Presence represents a user's availability and status message.
type Presence struct {
	MRI                 string
	Availability        string
	Activity            string
	StatusNote          string
	StatusNoteExpiresAt int64
	LastActiveAt        int64
	UpdatedAt           int64
}
