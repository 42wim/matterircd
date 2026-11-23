package googlechatclient

import (
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

type protoUser struct {
	ID             string
	IsBot          bool
	Name           string
	AvatarURL      string
	Email          string
	FirstName      string
	LastName       string
	Deleted        bool
	OrganizationID string
	IsConsumer     bool
}

type protoUserStatus struct {
	UserID            string
	DND               bool
	DNDExpiryUsec     int64
	PresenceShared    bool
	HasPresenceShared bool
	CustomText        string
	CustomEmoji       string
	CustomExpiryUsec  int64
}

type protoUserPresence struct {
	UserID          string
	Presence        int32
	DND             bool
	ActiveUntilUsec int64
	Status          *protoUserStatus
}

type protoMembership struct {
	UserID string
	Email  string
	Group  protoGroupRef
	State  int32
	Role   int32
}

type protoAnnotation struct {
	Type           int32
	StartIndex     int32
	Length         int32
	MentionUserID  string
	MentionName    string
	MentionType    int32
	FormatType     int32
	URL            string
	URLTitle       string
	UploadToken    string
	UploadName     string
	UploadMIME     string
	DriveID        string
	DriveTitle     string
	DriveMIME      string
	DriveURL       string
	MeetURL        string
	RoomNewName    string
	RoomNewDesc    string
	RoomInitiator  string
	MemberAction   int32
	MemberActorID  string
	MemberTargetID []string
	CustomEmoji    string
}

type protoReaction struct {
	Emoji   string
	Count   int32
	Own     bool
	Created int64
}

type protoQuotedMessage struct {
	Group       protoGroupRef
	TopicID     string
	MessageID   string
	CreatorID   string
	Creator     *protoUser
	TextBody    string
	Annotations []protoAnnotation
	Deleted     bool
}

type protoMessage struct {
	Group          protoGroupRef
	TopicID        string
	MessageID      string
	Creator        *protoUser
	CreateTimeUsec int64
	UpdateTimeUsec int64
	DeleteTimeUsec int64
	EditTimeUsec   int64
	TextBody       string
	LocalID        string
	Annotations    []protoAnnotation
	Reactions      []protoReaction
	Quoted         *protoQuotedMessage
	MessageType    int32
	IsInlineReply  bool
}

type protoTopic struct {
	Group          protoGroupRef
	TopicID        string
	SortTimeUsec   int64
	CreateTimeUsec int64
	Replies        []*protoMessage
	IsSystem       bool
}

type protoGroupReadState struct {
	LastReadTimeUsec int64
	HideTimeUsec     int64
	Blocked          bool
	MembershipState  int32
	MembershipRole   int32
	JoinedUsers      []string
	LastHeadMsgUsec  int64
}

type protoGroup struct {
	Group          protoGroupRef
	Name           string
	SortTimeUsec   int64
	ReadState      *protoGroupReadState
	GroupType      int32
	NameUserIDs    []string
	NameGroupName  string
	Description    string
	OrganizationID string
	IsConsumer     bool
	AttrGroupType  int32
}

type protoWorldItem struct {
	Group          protoGroupRef
	SortTimeUsec   int64
	ReadState      *protoGroupReadState
	RoomName       string
	DMMembers      []string
	NameUserIDs    []string
	NameGroupName  string
	Description    string
	OrganizationID string
	IsConsumer     bool
	Snippet        *protoMessage
	AttrGroupType  int32
	UserProfiles   []*protoUser
}

type protoPaginatedWorldResp struct {
	Items          []*protoWorldItem
	HasMore        bool
	NextAnchorUsec int64
	NextPageToken  string
}

type protoGetGroupResp struct {
	Group           *protoGroup
	Memberships     []protoMembership
	JoinedMemberIDs []string
	NameProfiles    []*protoUser
}

type protoListMembersResp struct {
	Memberships   []protoMembership
	Members       []*protoUser
	NextPageToken string
}

type protoReactionSummary struct {
	Emoji        string
	Reactors     []string
	UsersAdded   []string
	UsersRemoved []string
}

type protoEventBody struct {
	EventType      int32
	GroupViewed    protoGroupRef
	ViewTimeUsec   int64
	GroupUpdated   *protoGroup
	GroupUpdateOp  int32
	MessagePosted  *protoMessage
	MessageGroup   *protoGroup
	TopicCreated   *protoTopic
	DeletedGroup   protoGroupRef
	DeletedTopicID string
	DeletedMsgID   string
	DeletedLocalID string
	Tombstone      *protoMessage
	ReactGroup     protoGroupRef
	ReactTopicID   string
	ReactMsgID     string
	ReactEmoji     string
	ReactUserID    string
	ReactOption    int32
	ReactTimeUsec  int64
	BatchGroup     protoGroupRef
	BatchTopicID   string
	BatchMsgID     string
	BatchSummaries []protoReactionSummary
	BatchTimeUsec  int64
	TypingState    int32
	TypingUserID   string
	TypingGroup    protoGroupRef
	TypingTopicID  string
	Membership     *protoMembership
	PriorState     int32
	HiddenGroup    protoGroupRef
	Hidden         bool
	DeletedGroups  []protoGroupRef
	UserStatus     *protoUserStatus
	ReadStateGroup protoGroupRef
	ReadTimeUsec   int64
}

type protoEvent struct {
	Group       protoGroupRef
	Type        int32
	UserID      string
	Bodies      []protoEventBody
	WorldItem   *protoWorldItem
	RevisionTs  int64
	HasRevision bool
}

func walkFields(b []byte, fn func(num protowire.Number, typ protowire.Type, val []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return fmt.Errorf("%w: %v", errInvalidProto, protowire.ParseError(n))
		}

		b = b[n:]

		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return fmt.Errorf("%w: %v", errInvalidProto, protowire.ParseError(m))
		}

		err := fn(num, typ, b[:m])
		if err != nil {
			return err
		}

		b = b[m:]
	}

	return nil
}

func decodeVarint(val []byte) uint64 {
	v, _ := protowire.ConsumeVarint(val)

	return v
}

func decodeBytes(val []byte) []byte {
	v, _ := protowire.ConsumeBytes(val)

	return v
}

func decodeString(val []byte) string {
	v, _ := protowire.ConsumeString(val)

	return v
}

func decodeUserID(b []byte) (string, bool) {
	var (
		id    string
		isBot bool
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			id = decodeString(val)
		case num == 2 && typ == protowire.VarintType:
			isBot = decodeVarint(val) == protoUserTypeBot
		}

		return nil
	})

	return id, isBot
}

func decodeOrganizationInfo(b []byte) (string, bool) {
	var (
		orgID      string
		isConsumer bool
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			isConsumer = true
		case 2:
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 1 && t2 == protowire.BytesType {
					_ = walkFields(decodeBytes(v2), func(n3 protowire.Number, t3 protowire.Type, v3 []byte) error {
						if n3 == 1 && t3 == protowire.BytesType {
							orgID = decodeString(v3)
						}

						return nil
					})
				}

				return nil
			})
		}

		return nil
	})

	return orgID, isConsumer
}

func decodeUser(b []byte) *protoUser {
	u := &protoUser{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ == protowire.VarintType {
			if num == 7 {
				u.Deleted = decodeVarint(val) != 0
			}

			return nil
		}

		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			u.ID, u.IsBot = decodeUserID(decodeBytes(val))
		case 2:
			u.Name = decodeString(val)
		case 3:
			u.AvatarURL = decodeString(val)
		case 4:
			u.Email = decodeString(val)
		case 5:
			u.FirstName = decodeString(val)
		case 6:
			u.LastName = decodeString(val)
		case 9:
			u.IsBot = true
			decodeBotInfo(decodeBytes(val), u)
		case 12:
			u.OrganizationID, u.IsConsumer = decodeOrganizationInfo(decodeBytes(val))
		}

		return nil
	})

	return u
}

func decodeBotInfo(b []byte, u *protoUser) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 5:
			if u.AvatarURL == "" {
				u.AvatarURL = decodeString(val)
			}
		case 6:
			if u.Name == "" {
				u.Name = decodeString(val)
			}
		}

		return nil
	})
}

func decodeGroupID(b []byte) protoGroupRef {
	var ref protoGroupRef

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			ref.ID = decodeFirstString(decodeBytes(val))
			ref.IsDM = false
		case 3:
			ref.ID = decodeFirstString(decodeBytes(val))
			ref.IsDM = true
		}

		return nil
	})

	return ref
}

func decodeFirstString(b []byte) string {
	var s string

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			s = decodeString(val)
		}

		return nil
	})

	return s
}

func decodeTopicID(b []byte) (protoGroupRef, string) {
	var (
		ref     protoGroupRef
		topicID string
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 2:
			topicID = decodeString(val)
		case 3:
			ref = decodeGroupID(decodeBytes(val))
		}

		return nil
	})

	return ref, topicID
}

func decodeMessageParentID(b []byte) (protoGroupRef, string) {
	var (
		ref     protoGroupRef
		topicID string
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 4 && typ == protowire.BytesType {
			ref, topicID = decodeTopicID(decodeBytes(val))
		}

		return nil
	})

	return ref, topicID
}

func decodeMessageID(b []byte) (protoGroupRef, string, string) {
	var (
		ref     protoGroupRef
		topicID string
		msgID   string
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			ref, topicID = decodeMessageParentID(decodeBytes(val))
		case 2:
			msgID = decodeString(val)
		}

		return nil
	})

	return ref, topicID, msgID
}

func decodeEmoji(b []byte) string {
	var out string

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			out = decodeString(val)
		case 2:
			if out == "" {
				out = decodeCustomEmojiShortcode(decodeBytes(val))
			}
		}

		return nil
	})

	return out
}

func decodeCustomEmojiShortcode(b []byte) string {
	var shortcode string

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 3 && typ == protowire.BytesType {
			shortcode = decodeString(val)
		}

		return nil
	})

	return shortcode
}

func decodeMemberID(b []byte) (string, string) {
	var (
		userID string
		email  string
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			userID, _ = decodeUserID(decodeBytes(val))
		case 3:
			email = decodeString(val)
		}

		return nil
	})

	return userID, email
}

func decodeMembership(b []byte) protoMembership {
	var m protoMembership

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			decodeMembershipID(decodeBytes(val), &m)
		case num == 3 && typ == protowire.VarintType:
			m.State = int32(decodeVarint(val)) //nolint:gosec // enum value
		case num == 5 && typ == protowire.VarintType:
			m.Role = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return nil
	})

	return m
}

func decodeMembershipID(b []byte, m *protoMembership) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			m.UserID, m.Email = decodeMemberID(decodeBytes(val))
		case 2:
			m.Group = protoGroupRef{ID: decodeFirstString(decodeBytes(val)), IsDM: false}
		case 3:
			m.Group = decodeGroupID(decodeBytes(val))
		}

		return nil
	})
}

func decodeMember(b []byte) *protoUser {
	var u *protoUser

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			u = decodeUser(decodeBytes(val))
		}

		return nil
	})

	return u
}

func decodeUserStatus(b []byte) *protoUserStatus {
	st := &protoUserStatus{PresenceShared: true}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				st.UserID, _ = decodeUserID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.BytesType {
				decodeDndSettings(decodeBytes(val), st)
			}
		case 5:
			if typ == protowire.VarintType {
				st.PresenceShared = decodeVarint(val) != 0
				st.HasPresenceShared = true
			}
		case 6:
			if typ == protowire.BytesType {
				decodeCustomStatus(decodeBytes(val), st)
			}
		}

		return nil
	})

	return st
}

func decodeDndSettings(b []byte, st *protoUserStatus) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.VarintType {
			return nil
		}

		switch num {
		case 1:
			st.DND = decodeVarint(val) == protoDndActive
		case 2:
			st.DNDExpiryUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		}

		return nil
	})
}

func decodeCustomStatus(b []byte, st *protoUserStatus) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				st.CustomText = decodeString(val)
			}
		case 2:
			if typ == protowire.BytesType && st.CustomEmoji == "" {
				st.CustomEmoji = decodeString(val)
			}
		case 3:
			if typ == protowire.VarintType {
				st.CustomExpiryUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		case 4:
			if typ == protowire.BytesType {
				if e := decodeEmoji(decodeBytes(val)); e != "" {
					st.CustomEmoji = e
				}
			}
		}

		return nil
	})
}

func decodeGetSelfUserStatusResponse(b []byte) (*protoUserStatus, error) {
	var st *protoUserStatus

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			st = decodeUserStatus(decodeBytes(val))
		}

		return nil
	})

	return st, err
}

func decodeGetUserPresenceResponse(b []byte) ([]*protoUserPresence, error) {
	var out []*protoUserPresence

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			out = append(out, decodeUserPresence(decodeBytes(val)))
		}

		return nil
	})

	return out, err
}

func decodeUserPresence(b []byte) *protoUserPresence {
	p := &protoUserPresence{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				p.UserID, _ = decodeUserID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.VarintType {
				p.Presence = int32(decodeVarint(val)) //nolint:gosec // enum value
			}
		case 3:
			if typ == protowire.VarintType {
				p.DND = decodeVarint(val) == protoDndActive
			}
		case 4:
			if typ == protowire.VarintType {
				p.ActiveUntilUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		case 5:
			if typ == protowire.BytesType {
				p.Status = decodeUserStatus(decodeBytes(val))
			}
		}

		return nil
	})

	return p
}

func decodeGetMembersResponse(b []byte) ([]*protoUser, error) {
	var users []*protoUser

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			if u := decodeMember(decodeBytes(val)); u != nil && u.ID != "" {
				users = append(users, u)
			}
		case 2:
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 2 && t2 == protowire.BytesType {
					if u := decodeMember(decodeBytes(v2)); u != nil && u.ID != "" {
						users = append(users, u)
					}
				}

				return nil
			})
		}

		return nil
	})

	return users, err
}

func decodeListMembersResponse(b []byte) (*protoListMembersResp, error) {
	resp := &protoListMembersResp{}

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			resp.Memberships = append(resp.Memberships, decodeMembership(decodeBytes(val)))
		case 2:
			if u := decodeMember(decodeBytes(val)); u != nil && u.ID != "" {
				resp.Members = append(resp.Members, u)
			}
		case 3:
			resp.NextPageToken = decodeString(val)
		}

		return nil
	})

	return resp, err
}

func decodeAnnotation(b []byte) protoAnnotation {
	var a protoAnnotation

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeAnnotationField(num, typ, val, &a)

		return nil
	})

	return a
}

func decodeAnnotationField(num protowire.Number, typ protowire.Type, val []byte, a *protoAnnotation) {
	if typ == protowire.VarintType {
		switch num {
		case 1:
			a.Type = int32(decodeVarint(val)) //nolint:gosec // enum value
		case 2:
			a.StartIndex = int32(decodeVarint(val)) //nolint:gosec // text offset
		case 3:
			a.Length = int32(decodeVarint(val)) //nolint:gosec // text length
		}

		return
	}

	if typ != protowire.BytesType {
		return
	}

	data := decodeBytes(val)

	switch num {
	case 4:
		decodeDriveMetadata(data, a)
	case 5:
		decodeUserMentionMetadata(data, a)
	case 7:
		decodeURLMetadata(data, a)
	case 8:
		decodeFormatMetadata(data, a)
	case 10:
		decodeUploadMetadata(data, a)
	case 11:
		decodeMembershipChangedMetadata(data, a)
	case 12:
		decodeVideoCallMetadata(data, a)
	case 14:
		decodeRoomUpdatedMetadata(data, a)
	case 26:
		decodeCustomEmojiMetadata(data, a)
	}
}

func decodeUserMentionMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				a.MentionUserID, _ = decodeUserID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.VarintType {
				a.MentionType = int32(decodeVarint(val)) //nolint:gosec // enum value
			}
		case 4:
			if typ == protowire.BytesType {
				a.MentionName = decodeString(val)
			}
		}

		return nil
	})
}

func decodeFormatMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.VarintType {
			a.FormatType = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return nil
	})
}

func decodeUploadMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			a.UploadToken = decodeString(val)
		case 3:
			a.UploadName = decodeString(val)
		case 4:
			a.UploadMIME = decodeString(val)
		}

		return nil
	})
}

func decodeDriveMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			a.DriveID = decodeString(val)
		case 2:
			a.DriveTitle = decodeString(val)
		case 4:
			a.DriveMIME = decodeString(val)
		case 16:
			a.DriveURL = decodeString(val)
		}

		return nil
	})
}

func decodeURLMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			a.URLTitle = decodeString(val)
		case 7:
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 3 && t2 == protowire.BytesType {
					a.URL = decodeString(v2)
				}

				return nil
			})
		}

		return nil
	})
}

func decodeVideoCallMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 3 && t2 == protowire.BytesType {
					a.MeetURL = decodeString(v2)
				}

				return nil
			})
		}

		return nil
	})
}

func decodeRoomUpdatedMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 2:
			a.RoomNewName = decodeString(val)
		case 4:
			if u := decodeUser(decodeBytes(val)); u != nil {
				a.RoomInitiator = u.ID
			}
		case 6:
			if name := decodeFirstString(decodeBytes(val)); name != "" {
				a.RoomNewName = name
			}
		case 7:
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 1 && t2 == protowire.BytesType {
					a.RoomNewDesc = decodeFirstString(decodeBytes(v2))
				}

				return nil
			})
		}

		return nil
	})
}

func decodeMembershipChangedMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.VarintType {
				a.MemberAction = int32(decodeVarint(val)) //nolint:gosec // enum value
			}
		case 2:
			if typ == protowire.BytesType {
				a.MemberActorID, _ = decodeUserID(decodeBytes(val))
			}
		case 3:
			if typ == protowire.BytesType {
				uid, _ := decodeMemberID(decodeBytes(val))
				if uid != "" {
					a.MemberTargetID = append(a.MemberTargetID, uid)
				}
			}
		case 4:
			if typ == protowire.BytesType && a.MemberActorID == "" {
				if u := decodeUser(decodeBytes(val)); u != nil {
					a.MemberActorID = u.ID
				}
			}
		}

		return nil
	})
}

func decodeCustomEmojiMetadata(b []byte, a *protoAnnotation) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			a.CustomEmoji = decodeCustomEmojiShortcode(decodeBytes(val))
		}

		return nil
	})
}

func decodeReaction(b []byte) protoReaction {
	var r protoReaction

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				r.Emoji = decodeEmoji(decodeBytes(val))
			}
		case 2:
			if typ == protowire.VarintType {
				r.Count = int32(decodeVarint(val)) //nolint:gosec // reaction count
			}
		case 3:
			if typ == protowire.VarintType {
				r.Own = decodeVarint(val) != 0
			}
		case 4:
			if typ == protowire.VarintType {
				r.Created = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		}

		return nil
	})

	return r
}

func decodeMessage(b []byte) *protoMessage {
	m := &protoMessage{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeMessageField(num, typ, val, m)

		return nil
	})

	return m
}

func decodeMessageField(num protowire.Number, typ protowire.Type, val []byte, m *protoMessage) {
	if typ == protowire.VarintType {
		decodeMessageVarintField(num, val, m)

		return
	}

	if typ != protowire.BytesType {
		return
	}

	switch num {
	case 1:
		m.Group, m.TopicID, m.MessageID = decodeMessageID(decodeBytes(val))
	case 2:
		m.Creator = decodeUser(decodeBytes(val))
	case 10:
		m.TextBody = decodeString(val)
	case 11:
		m.Annotations = append(m.Annotations, decodeAnnotation(decodeBytes(val)))
	case 14:
		m.LocalID = decodeString(val)
	case 21:
		m.Reactions = append(m.Reactions, decodeReaction(decodeBytes(val)))
	case 37:
		m.Quoted = decodeQuotedMessageMetadata(decodeBytes(val))
	}
}

func decodeQuotedMessageMetadata(b []byte) *protoQuotedMessage {
	q := &protoQuotedMessage{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				q.Group, q.TopicID, q.MessageID = decodeMessageID(decodeBytes(val))
			}
		case 3:
			if typ == protowire.VarintType {
				q.Deleted = decodeVarint(val) == 2
			}
		case 4:
			if typ == protowire.BytesType {
				q.CreatorID, _ = decodeUserID(decodeBytes(val))
			}
		case 5:
			if typ == protowire.BytesType {
				q.Creator = decodeUser(decodeBytes(val))
			}
		case 6:
			if typ == protowire.BytesType {
				q.TextBody = decodeString(val)
			}
		case 7:
			if typ == protowire.BytesType {
				q.Annotations = append(q.Annotations, decodeAnnotation(decodeBytes(val)))
			}
		}

		return nil
	})

	return q
}

func decodeMessageVarintField(num protowire.Number, val []byte, m *protoMessage) {
	switch num {
	case 3:
		m.CreateTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
	case 4:
		m.UpdateTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
	case 8:
		m.DeleteTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
	case 17:
		m.EditTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
	case 28:
		m.MessageType = int32(decodeVarint(val)) //nolint:gosec // enum value
	case 34:
		m.IsInlineReply = decodeVarint(val) != 0
	}
}

func decodeTopic(b []byte) *protoTopic {
	t := &protoTopic{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				t.Group, t.TopicID = decodeTopicID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.VarintType {
				t.SortTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		case 7:
			if typ == protowire.BytesType {
				t.Replies = append(t.Replies, decodeMessage(decodeBytes(val)))
			}
		case 12:
			if typ == protowire.VarintType {
				t.IsSystem = decodeVarint(val) != 0
			}
		case 15:
			if typ == protowire.VarintType {
				t.CreateTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		}

		return nil
	})

	return t
}

func decodeCreateTopicResponse(b []byte) (*protoTopic, error) {
	var t *protoTopic

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			t = decodeTopic(decodeBytes(val))
		}

		return nil
	})

	return t, err
}

func decodeSingleMessageResponse(b []byte) (*protoMessage, error) {
	var m *protoMessage

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			m = decodeMessage(decodeBytes(val))
		}

		return nil
	})

	return m, err
}

func decodeListTopicsResponse(b []byte) ([]*protoTopic, error) {
	var topics []*protoTopic

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			topics = append(topics, decodeTopic(decodeBytes(val)))
		}

		return nil
	})

	return topics, err
}

func decodeListMessagesResponse(b []byte) ([]*protoMessage, error) {
	var msgs []*protoMessage

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			msgs = append(msgs, decodeMessage(decodeBytes(val)))
		}

		return nil
	})

	return msgs, err
}

func decodeGroupReadState(b []byte) *protoGroupReadState {
	rs := &protoGroupReadState{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ == protowire.BytesType {
			if num == 23 {
				if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
					rs.JoinedUsers = append(rs.JoinedUsers, uid)
				}
			}

			return nil
		}

		if typ != protowire.VarintType {
			return nil
		}

		switch num {
		case 2:
			rs.LastReadTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		case 9:
			rs.HideTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		case 15:
			rs.Blocked = decodeVarint(val) != 0
		case 16:
			rs.MembershipState = int32(decodeVarint(val)) //nolint:gosec // enum value
		case 22:
			rs.MembershipRole = int32(decodeVarint(val)) //nolint:gosec // enum value
		case 29:
			rs.LastHeadMsgUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		}

		return nil
	})

	return rs
}

func decodeNameUsers(b []byte) ([]string, string) {
	var (
		ids  []string
		name string
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
				ids = append(ids, uid)
			}
		case 3:
			name = decodeString(val)
		}

		return nil
	})

	return ids, name
}

func decodeGroup(b []byte) *protoGroup {
	g := &protoGroup{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeGroupField(num, typ, val, g)

		return nil
	})

	return g
}

func decodeGroupField(num protowire.Number, typ protowire.Type, val []byte, g *protoGroup) {
	if typ == protowire.VarintType {
		switch num {
		case 7:
			g.SortTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		case 22:
			g.GroupType = int32(decodeVarint(val)) //nolint:gosec // enum value
		case 33:
			g.AttrGroupType = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return
	}

	if typ != protowire.BytesType {
		return
	}

	switch num {
	case 1:
		g.Group = decodeGroupID(decodeBytes(val))
	case 2:
		g.Name = decodeString(val)
	case 12:
		g.ReadState = decodeGroupReadState(decodeBytes(val))
	case 21:
		g.OrganizationID, g.IsConsumer = decodeOrganizationInfo(decodeBytes(val))
	case 34:
		g.NameUserIDs, g.NameGroupName = decodeNameUsers(decodeBytes(val))
	case 37:
		g.Description = decodeFirstString(decodeBytes(val))
	}
}

func decodeGetGroupResponse(b []byte) (*protoGetGroupResp, error) {
	resp := &protoGetGroupResp{}

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			resp.Group = decodeGroup(decodeBytes(val))
		case 4:
			resp.Memberships = append(resp.Memberships, decodeMembership(decodeBytes(val)))
		case 7:
			if uid, _ := decodeMemberID(decodeBytes(val)); uid != "" {
				resp.JoinedMemberIDs = append(resp.JoinedMemberIDs, uid)
			}
		case 16:
			if u := decodeUser(decodeBytes(val)); u != nil && u.ID != "" {
				resp.NameProfiles = append(resp.NameProfiles, u)
			}
		}

		return nil
	})

	return resp, err
}

func decodeCreateGroupResponse(b []byte) (*protoGroup, error) {
	var g *protoGroup

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			g = decodeGroup(decodeBytes(val))
		}

		return nil
	})

	return g, err
}

func decodeCreateDMResponse(b []byte) (*protoGroup, []protoMembership, error) {
	var (
		dm          *protoGroup
		memberships []protoMembership
	)

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			dm = decodeGroup(decodeBytes(val))
		case 4:
			memberships = append(memberships, decodeMembership(decodeBytes(val)))
		}

		return nil
	})

	return dm, memberships, err
}

func decodeDMMembers(b []byte) []string {
	var out []string

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
				out = append(out, uid)
			}
		case 2:
			_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
				if n2 == 1 && t2 == protowire.BytesType {
					if uid, _ := decodeUserID(decodeBytes(v2)); uid != "" {
						out = append(out, uid)
					}
				}

				return nil
			})
		}

		return nil
	})

	return out
}

func decodeWorldItemLite(b []byte) *protoWorldItem {
	item := &protoWorldItem{}

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeWorldItemField(num, typ, val, item)

		return nil
	})

	return item
}

func decodeWorldItemField(num protowire.Number, typ protowire.Type, val []byte, item *protoWorldItem) {
	if typ == protowire.VarintType {
		switch num {
		case 3:
			item.SortTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		case 19:
			item.AttrGroupType = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return
	}

	if typ != protowire.BytesType {
		return
	}

	switch num {
	case 1:
		item.Group = decodeGroupID(decodeBytes(val))
	case 4:
		item.ReadState = decodeGroupReadState(decodeBytes(val))
	case 5:
		item.RoomName = decodeString(val)
	case 6:
		item.DMMembers = append(item.DMMembers, decodeDMMembers(decodeBytes(val))...)
	case 7:
		decodeGroupLite(decodeBytes(val), item)
	case 11:
		item.OrganizationID, item.IsConsumer = decodeOrganizationInfo(decodeBytes(val))
	case 13:
		item.Snippet = decodeMessage(decodeBytes(val))
	case 20:
		item.NameUserIDs, item.NameGroupName = decodeNameUsers(decodeBytes(val))
	case 32:
		decodeUserProfile(decodeBytes(val), item)
	case 37:
		decodeGroupNameDetails(decodeBytes(val), item)
	}
}

func decodeGroupLite(b []byte, item *protoWorldItem) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 8 && typ == protowire.BytesType {
			item.Description = decodeFirstString(decodeBytes(val))
		}

		return nil
	})
}

func decodeUserProfile(b []byte, item *protoWorldItem) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			if u := decodeUser(decodeBytes(val)); u != nil && u.ID != "" {
				item.UserProfiles = append(item.UserProfiles, u)
			}
		}

		return nil
	})
}

func decodeGroupNameDetails(b []byte, item *protoWorldItem) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			if item.RoomName == "" {
				item.RoomName = decodeString(val)
			}
		case 2:
			item.DMMembers = append(item.DMMembers, decodeDMMembers(decodeBytes(val))...)
		case 3:
			ids, name := decodeNameUsers(decodeBytes(val))
			if len(item.NameUserIDs) == 0 {
				item.NameUserIDs = ids
			}

			if item.NameGroupName == "" {
				item.NameGroupName = name
			}
		}

		return nil
	})
}

func decodePaginatedWorldResponse(b []byte) (*protoPaginatedWorldResp, error) {
	resp := &protoPaginatedWorldResp{}

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			decodeWorldSectionResponse(decodeBytes(val), resp)
		case 4:
			resp.Items = append(resp.Items, decodeWorldItemLite(decodeBytes(val)))
		}

		return nil
	})

	return resp, err
}

func decodeWorldSectionResponse(b []byte, resp *protoPaginatedWorldResp) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 2:
			if typ == protowire.BytesType {
				resp.Items = append(resp.Items, decodeWorldItemLite(decodeBytes(val)))
			}
		case 3:
			if typ == protowire.VarintType {
				resp.NextAnchorUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		case 5:
			if typ == protowire.VarintType && decodeVarint(val) != 0 {
				resp.HasMore = true
			}
		case 6:
			if typ == protowire.BytesType {
				resp.NextPageToken = decodeString(val)
			}
		}

		return nil
	})
}

func decodeCatchUpResponse(b []byte) ([]*protoEvent, error) {
	var events []*protoEvent

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			if ev := decodeEvent(decodeBytes(val)); ev != nil {
				events = append(events, ev)
			}
		}

		return nil
	})

	return events, err
}

func decodeStreamEventsResponse(b []byte) (*protoEvent, error) {
	var ev *protoEvent

	err := walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			ev = decodeEvent(decodeBytes(val))
		}

		return nil
	})

	return ev, err
}

func decodeEvent(b []byte) *protoEvent {
	ev := &protoEvent{}

	var (
		firstBody   *protoEventBody
		extraBodies []protoEventBody
	)

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeEventField(num, typ, val, ev, &firstBody, &extraBodies)

		return nil
	})

	if firstBody != nil {
		if firstBody.EventType == 0 {
			firstBody.EventType = ev.Type
		}

		ev.Bodies = append(ev.Bodies, *firstBody)
	}

	ev.Bodies = append(ev.Bodies, extraBodies...)

	return ev
}

func decodeEventField(num protowire.Number, typ protowire.Type, val []byte, ev *protoEvent, firstBody **protoEventBody, extraBodies *[]protoEventBody) {
	if num == 3 && typ == protowire.VarintType {
		ev.Type = int32(decodeVarint(val)) //nolint:gosec // enum value

		return
	}

	if typ != protowire.BytesType {
		return
	}

	switch num {
	case 1:
		ev.Group = decodeGroupID(decodeBytes(val))
	case 4:
		body := decodeEventBody(decodeBytes(val))
		*firstBody = &body
	case 5:
		ev.UserID, _ = decodeUserID(decodeBytes(val))
	case 6, 7:
		if ts := decodeRevisionTimestamp(decodeBytes(val)); ts > 0 {
			ev.RevisionTs = ts
			ev.HasRevision = true
		}
	case 8:
		*extraBodies = append(*extraBodies, decodeEventBody(decodeBytes(val)))
	case 10:
		ev.WorldItem = decodeWorldItemLite(decodeBytes(val))
	}
}

func decodeRevisionTimestamp(b []byte) int64 {
	var ts int64

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.VarintType {
			ts = int64(decodeVarint(val)) //nolint:gosec // proto int64
		}

		return nil
	})

	return ts
}

func decodeEventBody(b []byte) protoEventBody {
	var body protoEventBody

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		decodeEventBodyField(num, typ, val, &body)

		return nil
	})

	return body
}

func decodeEventBodyField(num protowire.Number, typ protowire.Type, val []byte, body *protoEventBody) {
	if num == 12 && typ == protowire.VarintType {
		body.EventType = int32(decodeVarint(val)) //nolint:gosec // enum value

		return
	}

	if typ != protowire.BytesType {
		return
	}

	data := decodeBytes(val)

	if decodeEventBodyMessageField(num, data, body) {
		return
	}

	decodeEventBodyGroupField(num, data, body)
}

func decodeEventBodyMessageField(num protowire.Number, data []byte, body *protoEventBody) bool {
	switch num {
	case 6:
		decodeMessageEvent(data, body)
	case 18:
		decodeMessageDeletedEvent(data, body)
	case 21:
		decodeTopicCreatedEvent(data, body)
	case 22:
		decodeMessageReactionEvent(data, body)
	case 26:
		decodeTypingEvent(data, body)
	case 50:
		decodeReactionsSummaryEvent(data, body)
	default:
		return false
	}

	return true
}

func decodeEventBodyGroupField(num protowire.Number, data []byte, body *protoEventBody) {
	switch num {
	case 3:
		decodeGroupViewedEvent(data, body)
	case 5:
		decodeGroupUpdatedEvent(data, body)
	case 14:
		decodeMembershipChangedEvent(data, body)
	case 15:
		decodeGroupHideChangedEvent(data, body)
	case 23:
		decodeUserStatusUpdatedEvent(data, body)
	case 27:
		decodeGroupDeletedEvent(data, body)
	case 65:
		decodeGroupReadStateUpdatedEvent(data, body)
	}
}

func decodeGroupViewedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			body.GroupViewed = decodeGroupID(decodeBytes(val))
		case num == 2 && typ == protowire.VarintType:
			body.ViewTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		}

		return nil
	})
}

func decodeGroupUpdatedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			body.GroupUpdated = decodeGroup(decodeBytes(val))
		case num == 4 && typ == protowire.VarintType:
			body.GroupUpdateOp = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return nil
	})
}

func decodeMessageEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			body.MessagePosted = decodeMessage(decodeBytes(val))
		case 8:
			body.MessageGroup = decodeGroup(decodeBytes(val))
		}

		return nil
	})
}

func decodeTopicCreatedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			body.TopicCreated = decodeTopic(decodeBytes(val))
		}

		return nil
	})
}

func decodeMessageDeletedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			body.DeletedGroup, body.DeletedTopicID, body.DeletedMsgID = decodeMessageID(decodeBytes(val))
		case 4:
			body.DeletedLocalID = decodeString(val)
		case 5:
			body.Tombstone = decodeMessage(decodeBytes(val))
		}

		return nil
	})
}

func decodeMessageReactionEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				body.ReactGroup, body.ReactTopicID, body.ReactMsgID = decodeMessageID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.BytesType {
				body.ReactEmoji = decodeEmoji(decodeBytes(val))
			}
		case 3:
			if typ == protowire.BytesType {
				body.ReactUserID, _ = decodeUserID(decodeBytes(val))
			}
		case 4:
			if typ == protowire.VarintType {
				body.ReactTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		case 5:
			if typ == protowire.VarintType {
				body.ReactOption = int32(decodeVarint(val)) //nolint:gosec // enum value
			}
		}

		return nil
	})
}

func decodeReactionsSummaryEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.BytesType {
				body.BatchGroup, body.BatchTopicID, body.BatchMsgID = decodeMessageID(decodeBytes(val))
			}
		case 2:
			if typ == protowire.BytesType {
				body.BatchSummaries = append(body.BatchSummaries, decodeReactionSummary(decodeBytes(val)))
			}
		case 3:
			if typ == protowire.VarintType {
				body.BatchTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
			}
		}

		return nil
	})
}

func decodeReactionSummary(b []byte) protoReactionSummary {
	var rs protoReactionSummary

	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if typ != protowire.BytesType {
			return nil
		}

		switch num {
		case 1:
			rs.Emoji = decodeEmoji(decodeBytes(val))
		case 2:
			if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
				rs.Reactors = append(rs.Reactors, uid)
			}
		case 5:
			if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
				rs.UsersRemoved = append(rs.UsersRemoved, uid)
			}
		case 6:
			if uid, _ := decodeUserID(decodeBytes(val)); uid != "" {
				rs.UsersAdded = append(rs.UsersAdded, uid)
			}
		}

		return nil
	})

	return rs
}

func decodeTypingEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch num {
		case 1:
			if typ == protowire.VarintType {
				body.TypingState = int32(decodeVarint(val)) //nolint:gosec // enum value
			}
		case 2:
			if typ == protowire.BytesType {
				body.TypingUserID, _ = decodeUserID(decodeBytes(val))
			}
		case 3:
			if typ == protowire.BytesType {
				_ = walkFields(decodeBytes(val), func(n2 protowire.Number, t2 protowire.Type, v2 []byte) error {
					if t2 != protowire.BytesType {
						return nil
					}

					switch n2 {
					case 1:
						body.TypingGroup = decodeGroupID(decodeBytes(v2))
					case 2:
						body.TypingGroup, body.TypingTopicID = decodeTopicID(decodeBytes(v2))
					}

					return nil
				})
			}
		}

		return nil
	})
}

func decodeMembershipChangedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			m := decodeMembership(decodeBytes(val))
			body.Membership = &m
		case num == 2 && typ == protowire.VarintType:
			body.PriorState = int32(decodeVarint(val)) //nolint:gosec // enum value
		}

		return nil
	})
}

func decodeGroupHideChangedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			body.HiddenGroup = decodeGroupID(decodeBytes(val))
		case num == 2 && typ == protowire.VarintType:
			body.Hidden = decodeVarint(val) != 0
		}

		return nil
	})
}

func decodeGroupDeletedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			if ref := decodeGroupID(decodeBytes(val)); ref.ID != "" {
				body.DeletedGroups = append(body.DeletedGroups, ref)
			}
		}

		return nil
	})
}

func decodeUserStatusUpdatedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		if num == 1 && typ == protowire.BytesType {
			body.UserStatus = decodeUserStatus(decodeBytes(val))
		}

		return nil
	})
}

func decodeGroupReadStateUpdatedEvent(b []byte, body *protoEventBody) {
	_ = walkFields(b, func(num protowire.Number, typ protowire.Type, val []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			body.ReadStateGroup = decodeGroupID(decodeBytes(val))
		case num == 3 && typ == protowire.VarintType:
			body.ReadTimeUsec = int64(decodeVarint(val)) //nolint:gosec // proto int64
		}

		return nil
	})
}
