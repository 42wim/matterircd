package bridge

import (
	"context"
	"time"

	"github.com/42wim/matterircd/config"
)

type Bridger interface {
	Invite(ctx context.Context, channelID, username string) error
	Join(ctx context.Context, channelName string) (string, string, error)
	List(ctx context.Context) (map[string]string, error)
	Part(ctx context.Context, channel string) error
	SetTopic(ctx context.Context, channelID, text string) error
	Topic(ctx context.Context, channelID string) string
	Kick(ctx context.Context, channelID, username string) error
	Nick(ctx context.Context, name string) error

	UpdateChannels(ctx context.Context) error
	Logout(ctx context.Context) error
	Connected() bool

	MsgUser(ctx context.Context, userID, text string) (string, error)
	MsgUserThread(ctx context.Context, userID, parentID, text string) (string, error)
	MsgChannel(ctx context.Context, channelID, text string) (string, error)
	MsgChannelThread(ctx context.Context, channelID, parentID, text string) (string, error)

	AddReaction(ctx context.Context, msgID, emoji string) error
	RemoveReaction(ctx context.Context, msgID, emoji string) error

	StatusUser(ctx context.Context, userID string) (string, error)
	StatusUsers(ctx context.Context) (map[string]string, error)
	SetStatus(ctx context.Context, status string) error

	Protocol() string

	CreateChannel(ctx context.Context, channelName string, channelType string) (*ChannelInfo, error)
	GetChannels() []*ChannelInfo
	GetChannel(ctx context.Context, channelID string) (*ChannelInfo, error)
	GetChannelCount() int
	GetChannelName(ctx context.Context, channelID string) string
	GetLastViewedAt(ctx context.Context, channelID string) int64
	UpdateLastViewed(ctx context.Context, channelID string)
	UpdateLastViewedUser(ctx context.Context, userID string) error
	GetChannelID(ctx context.Context, name, teamID string) string

	GetChannelUsers(ctx context.Context, channelID string) ([]*UserInfo, error)
	GetUsers() []*UserInfo
	GetUser(ctx context.Context, userID string) *UserInfo
	GetUserChannels(ctx context.Context, userID string) ([]string, error)
	GetUserCount() int
	GetMe() *UserInfo
	GetUserByUsername(ctx context.Context, username string) *UserInfo
	SearchUsers(ctx context.Context, query string) ([]*UserInfo, error)

	GetDMChannelName(userID1 string, userID2 string) string
	GetDMUser(ctx context.Context, channelName string) *UserInfo
	GetDMUserIDs(channelName string) (string, string, bool)
	GetDMOtherUserID(channelName, myUserID string) (string, bool)
	IsDMChannelName(channelName string) bool

	GetTeamName(ctx context.Context, teamID string) string

	GetPostsSince(ctx context.Context, channelID string, since int64) []*Event
	GetPosts(ctx context.Context, channelID string, limit int) []*Event
	GetPostThread(ctx context.Context, postID string) []*Event
	GetReplayEvents(ctx context.Context, channelID string, since int64) []*Event
	SearchPosts(ctx context.Context, search string) []*Event

	ModifyPost(ctx context.Context, msgID, text string) error
	GetFilesInfo(ctx context.Context, fileIDs []string) []*File

	GetLastSentMsgs() []string

	Config() any
	BridgeConfig() *config.BridgeConfig
	FormatterConfig() *config.FormatterConfig

	IsChannelMember(channelID string) bool

	Ping(ctx context.Context) error
}

type ChannelInfo struct {
	Name        string
	ID          string
	TeamID      string
	DM          bool
	Private     bool
	DisplayName string
	LastPostAt  int64
	DeleteAt    int64
}

type UserInfo struct {
	Nick        string   // From NICK command
	User        string   // From USER command
	Real        string   // From USER command
	Pass        []string // From PASS command
	Host        string
	Roles       string
	DisplayName string
	Ghost       bool
	Me          bool
	Username    string
	TeamID      string
	FirstName   string
	LastName    string
	MentionKeys []string
}

type Credentials struct {
	Login    string
	Team     string
	Pass     string
	Server   string
	Token    string
	MFAToken string
}

type BannerChangeEvent struct {
	Text string
}

type Event struct {
	Type string
	Data interface{}
}

type ChannelAddEvent struct {
	Adder     *UserInfo
	Added     []*UserInfo
	ChannelID string
	Text      string
	CreateAt  int64
}

type ChannelRemoveEvent struct {
	Remover   *UserInfo
	Removed   []*UserInfo
	ChannelID string
	Text      string
	CreateAt  int64
}

type ChannelCreateEvent struct {
	ChannelID string
}

type ChannelDeleteEvent struct {
	ChannelID string
}

type ChannelMessageEvent struct {
	Text        string
	ChannelID   string
	Sender      *UserInfo
	MessageType string
	ChannelType string
	Files       []*File
	MessageID   string
	Event       string
	ParentID    string
	CreateAt    int64
}

type ChannelTopicEvent struct {
	Text      string
	ChannelID string
	UserID    string
}

type ChannelUpdateEvent struct {
	ChannelID   string
	Name        string
	DisplayName string
}

type DirectMessageEvent struct {
	Text      string
	ChannelID string
	Receiver  *UserInfo
	Sender    *UserInfo
	Files     []*File
	MessageID string
	Event     string
	ParentID  string
	CreateAt  int64
}

type FileEvent struct {
	Receiver    *UserInfo
	Sender      *UserInfo
	ChannelID   string
	ChannelType string
	Files       []*File
	MessageID   string
	ParentID    string
}

type ReactionAddEvent struct {
	Receiver    *UserInfo
	Sender      *UserInfo
	ChannelID   string
	MessageID   string
	Reaction    string
	ChannelType string
	ParentUser  *UserInfo
	Message     string
	ParentID    string
}

type ReactionRemoveEvent ReactionAddEvent

type UserUpdateEvent struct {
	User *UserInfo
}

type StatusChangeEvent struct {
	UserID string
	Status string
}

type TypingEvent struct {
	ChannelID   string
	ChannelType string
	Receiver    *UserInfo
	Sender      *UserInfo
}

type LogoutEvent struct{}

type File struct {
	Name string
	Size int64
	URL  string
}

type Message struct {
	Text      string    `json:"text"`
	Channel   string    `json:"channel"`
	Username  string    `json:"username"`
	UserID    string    `json:"userid"` // userid on the bridge
	Account   string    `json:"account"`
	Event     string    `json:"event"`
	Protocol  string    `json:"protocol"`
	ParentID  string    `json:"parent_id"`
	Timestamp time.Time `json:"timestamp"`
	ID        string    `json:"id"`
	Extra     map[string][]interface{}
}
