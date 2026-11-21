package rocketchatclient

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// User presence statuses used by Rocket.Chat and normalized callbacks.
const (
	StatusOnline  = "online"
	StatusAway    = "away"
	StatusBusy    = "busy"
	StatusOffline = "offline"
)

// Rocket.Chat room types.
const (
	RoomTypeChannel = "c"
	RoomTypePrivate = "p"
	RoomTypeDirect  = "d"
)

// Credentials holds the connection and authentication parameters for a Rocket.Chat server.
type Credentials struct {
	Server        string
	Login         string
	Pass          string
	Token         string // "<userId>:<authToken>" or "token=<userId>:<authToken>"
	TwoFactorCode string
	NoTLS         bool
	SkipTLSVerify bool
}

// Timestamp decodes both REST ISO-8601 timestamp strings and Meteor DDP EJSON {"$date": ms} objects.
type Timestamp struct {
	Time time.Time
}

// UnmarshalJSON parses a Rocket.Chat timestamp from JSON.
func (t *Timestamp) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		t.Time = time.Time{}

		return nil
	}

	if s[0] == '"' {
		return t.unmarshalString(data)
	}

	if s[0] == '{' {
		var ejson struct {
			Date int64 `json:"$date"`
		}

		err := json.Unmarshal(data, &ejson)
		if err != nil {
			return err
		}

		if ejson.Date > 0 {
			t.Time = time.UnixMilli(ejson.Date).UTC()
		}

		return nil
	}

	return fmt.Errorf("rocketchatclient: unsupported timestamp format %s", s)
}

func (t *Timestamp) unmarshalString(data []byte) error {
	var str string

	err := json.Unmarshal(data, &str)
	if err != nil {
		return err
	}

	if str == "" {
		t.Time = time.Time{}

		return nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, str)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, str)
		if err != nil {
			return fmt.Errorf("rocketchatclient: invalid timestamp %q: %w", str, err)
		}
	}

	t.Time = parsed

	return nil
}

// UserRef identifies a user embedded inside a Rocket.Chat room, subscription, or message.
type UserRef struct {
	ID       string `json:"_id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// UserEmail holds an email entry on a Rocket.Chat user profile.
type UserEmail struct {
	Address  string `json:"address"`
	Verified bool   `json:"verified"`
}

// User represents a normalized Rocket.Chat user.
type User struct {
	ID         string      `json:"_id"`
	Username   string      `json:"username"`
	Name       string      `json:"name"`
	Emails     []UserEmail `json:"emails,omitempty"`
	Status     string      `json:"status"`
	StatusConn string      `json:"statusConnection,omitempty"`
	StatusText string      `json:"statusText,omitempty"`
	Type       string      `json:"type,omitempty"`
	Active     bool        `json:"active"`
	Roles      []string    `json:"roles,omitempty"`
	UTCOffset  float64     `json:"utcOffset,omitempty"`
}

// Room represents a Rocket.Chat public channel, private group, or direct message room.
type Room struct {
	ID           string    `json:"_id"`
	Type         string    `json:"t"`
	Name         string    `json:"name"`
	FName        string    `json:"fname"`
	Topic        string    `json:"topic"`
	Announcement string    `json:"announcement"`
	Description  string    `json:"description"`
	ReadOnly     bool      `json:"ro"`
	Default      bool      `json:"default"`
	UIDs         []string  `json:"uids"`
	Usernames    []string  `json:"usernames"`
	UsersCount   int       `json:"usersCount"`
	Msgs         int       `json:"msgs"`
	Owner        *UserRef  `json:"u"`
	UpdatedAt    Timestamp `json:"_updatedAt"`
	LastMessage  Timestamp `json:"lm"`
}

// Subscription represents the logged-in user's subscription state for a room.
type Subscription struct {
	ID        string    `json:"_id"`
	RoomID    string    `json:"rid"`
	Name      string    `json:"name"`
	FName     string    `json:"fname"`
	Type      string    `json:"t"`
	Open      bool      `json:"open"`
	Alert     bool      `json:"alert"`
	Unread    int       `json:"unread"`
	Roles     []string  `json:"roles"`
	User      *UserRef  `json:"u"`
	LastSeen  Timestamp `json:"ls"`
	Timestamp Timestamp `json:"ts"`
	UpdatedAt Timestamp `json:"_updatedAt"`
}

// File describes a file attached to a Rocket.Chat message.
type File struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// AttachmentField describes a structured field inside a rich attachment.
type AttachmentField struct {
	Title string `json:"title"`
	Value string `json:"value"`
	Short bool   `json:"short"`
}

// Attachment describes a rich or quoted attachment on a Rocket.Chat message.
type Attachment struct {
	Color       string            `json:"color"`
	Title       string            `json:"title"`
	TitleLink   string            `json:"title_link"`
	Description string            `json:"description"`
	Text        string            `json:"text"`
	AuthorName  string            `json:"author_name"`
	AuthorIcon  string            `json:"author_icon"`
	ImageURL    string            `json:"image_url"`
	ImageSize   int64             `json:"image_size"`
	AudioURL    string            `json:"audio_url"`
	VideoURL    string            `json:"video_url"`
	MessageLink string            `json:"message_link"`
	Fields      []AttachmentField `json:"fields"`
}

// ReactionSet holds the usernames that reacted with a given emoji.
type ReactionSet struct {
	Usernames []string `json:"usernames"`
}

// ChannelRef identifies a channel mentioned in a message.
type ChannelRef struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
}

// Message represents a normalized Rocket.Chat message.
type Message struct {
	ID          string                 `json:"_id"`
	RoomID      string                 `json:"rid"`
	Text        string                 `json:"msg"`
	Timestamp   Timestamp              `json:"ts"`
	User        UserRef                `json:"u"`
	UpdatedAt   Timestamp              `json:"_updatedAt"`
	EditedAt    *Timestamp             `json:"editedAt"`
	EditedBy    *UserRef               `json:"editedBy"`
	ThreadID    string                 `json:"tmid"`
	ThreadShow  bool                   `json:"tshow"`
	Type        string                 `json:"t"`
	File        *File                  `json:"file"`
	Files       []File                 `json:"files"`
	Attachments []Attachment           `json:"attachments"`
	Reactions   map[string]ReactionSet `json:"reactions"`
	Mentions    []UserRef              `json:"mentions"`
	Channels    []ChannelRef           `json:"channels"`
}

// ReactionEvent represents a single added or removed emoji reaction on a message.
type ReactionEvent struct {
	RoomID    string
	MessageID string
	UserID    string
	Username  string
	Emoji     string
	Added     bool
}

// Invitation represents a room invitation or newly surfaced unjoined room.
type Invitation struct {
	RoomID   string
	RoomName string
	RoomType string
	Sender   string
	Direct   bool
}

// MemberEvent represents a user joining, leaving, or changing roles in a room.
type MemberEvent struct {
	RoomID   string
	UserID   string
	Username string
	Sender   string
	Action   string
}

// SubscriptionsResponse is the payload returned by GET /api/v1/subscriptions.get.
type SubscriptionsResponse struct {
	Update  []Subscription `json:"update"`
	Remove  []Subscription `json:"remove"`
	Success bool           `json:"success"`
}

// RoomsResponse is the payload returned by GET /api/v1/rooms.get.
type RoomsResponse struct {
	Update  []Room `json:"update"`
	Remove  []Room `json:"remove"`
	Success bool   `json:"success"`
}

// UsersListResponse is the paginated payload returned by GET /api/v1/users.list.
type UsersListResponse struct {
	Users   []User `json:"users"`
	Count   int    `json:"count"`
	Offset  int    `json:"offset"`
	Total   int    `json:"total"`
	Success bool   `json:"success"`
}

// RoomRole represents a user's roles within a specific Rocket.Chat room.
type RoomRole struct {
	ID     string   `json:"_id"`
	RoomID string   `json:"rid"`
	User   UserRef  `json:"u"`
	Roles  []string `json:"roles"`
}

// MembersResponse is the paginated payload returned by channels.members, groups.members, and im.members.
type MembersResponse struct {
	Members []User `json:"members"`
	Count   int    `json:"count"`
	Offset  int    `json:"offset"`
	Total   int    `json:"total"`
	Success bool   `json:"success"`
}

// SyncMessagesResponse is the payload returned by GET /api/v1/chat.syncMessages.
type SyncMessagesResponse struct {
	Result struct {
		Updated []Message `json:"updated"`
		Deleted []Message `json:"deleted"`
	} `json:"result"`
	Success bool `json:"success"`
}

// MessagesResponse is the payload returned by history, thread, and search endpoints.
type MessagesResponse struct {
	Messages []Message `json:"messages"`
	Success  bool      `json:"success"`
}
