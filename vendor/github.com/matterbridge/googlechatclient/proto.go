package googlechatclient

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

// Dynamite protobuf enum constants verified against purple-googlechat/googlechat.proto.
const (
	protoUserTypeHuman = 0
	protoUserTypeBot   = 1

	protoClientTypeIOS            = 2
	protoClientVersion            = int64(2440378181258)
	protoCapabilityFullySupported = 2

	protoPlatformMobile  = 2
	protoOriginIOSProd   = 114
	protoPingStateActive = 1
	protoPingStateIdle   = 2
	protoFocusForeground = 1
	protoFocusBackground = 2
	protoInteractive     = 1

	protoMembershipJoined     = 2
	protoMembershipNotAMember = 3

	protoRoleOwner   = 4
	protoRoleManager = 6

	protoPresenceActive          = 1
	protoPresenceInactive        = 2
	protoPresenceUnknown         = 3
	protoPresenceSharingDisabled = 4

	protoDndAvailable = 1
	protoDndActive    = 2

	protoTypingStarted = 1
	protoTypingStopped = 2

	protoReactionAdd    = 1
	protoReactionRemove = 2

	protoAttrOneToOneHumanDM = 1
	protoAttrOneToOneBotDM   = 2
	protoAttrGroupDM         = 3
	protoAttrFlatRoom        = 4
	protoAttrThreadedRoom    = 5

	protoMsgTypeSystem = 2

	protoAnnotationURL         = 1
	protoAnnotationUserMention = 6
	protoAnnotationFormatData  = 8

	protoMentionUser = 3
	protoMentionAll  = 4

	protoChipDoNotRender = 3

	protoFormatBold             = 1
	protoFormatItalic           = 2
	protoFormatStrike           = 3
	protoFormatSourceCode       = 4
	protoFormatMonospace        = 5
	protoFormatHidden           = 6
	protoFormatMonospaceBlock   = 7
	protoFormatUnderline        = 8
	protoFormatFontColor        = 9
	protoFormatBulletedList     = 10
	protoFormatBulletedListItem = 11
	protoFormatClientHidden     = 12
	protoFormatSearchHighlight  = 13
	protoFormatBlockquote       = 15

	protoEventGroupViewed       = 3
	protoEventGroupUpdated      = 5
	protoEventMessagePosted     = 6
	protoEventMessageUpdated    = 7
	protoEventMessageDeleted    = 8
	protoEventMembershipChanged = 15
	protoEventTopicCreated      = 20
	protoEventMessageReacted    = 24
	protoEventUserStatusUpdated = 25
	protoEventTypingChanged     = 29
	protoEventBatchReactions    = 58
	protoEventGroupReadState    = 83

	protoGroupFetchMembers      = 0
	protoGroupFetchMemberIDs    = 2
	protoListTopicsFetchUser    = 1
	protoRetentionPermanent     = 1
	protoUpdateMaskSpaceDetails = 4
	protoHeartbeatUserActive    = 1
	protoHeartbeatUserInactive  = 2
)

var errInvalidProto = errors.New("googlechatclient: invalid protobuf payload")

// protoGroupRef identifies a Google Chat group along with whether its wire
// representation uses DmId (field 3) or SpaceId (field 1).
type protoGroupRef struct {
	ID   string
	IsDM bool
}

func appendVarintField(b []byte, num protowire.Number, v uint64) []byte {
	b = protowire.AppendTag(b, num, protowire.VarintType)

	return protowire.AppendVarint(b, v)
}

func appendBoolField(b []byte, num protowire.Number, v bool) []byte {
	return appendVarintField(b, num, protowire.EncodeBool(v))
}

func appendStringField(b []byte, num protowire.Number, s string) []byte {
	if s == "" {
		return b
	}

	b = protowire.AppendTag(b, num, protowire.BytesType)

	return protowire.AppendString(b, s)
}

func appendBytesField(b []byte, num protowire.Number, v []byte) []byte {
	b = protowire.AppendTag(b, num, protowire.BytesType)

	return protowire.AppendBytes(b, v)
}

func randomTraceID() int64 {
	var raw [8]byte

	_, _ = rand.Read(raw[:])

	return int64(binary.LittleEndian.Uint64(raw[:]) & 0x7fffffffffffffff)
}

func encodeClientFeatureCapabilities() []byte {
	var b []byte

	b = appendBoolField(b, 1, true) // enable_all_features
	for _, field := range []protowire.Number{2, 3, 5, 7, 8, 18, 22, 25, 26, 31, 43, 44} {
		b = appendVarintField(b, field, protoCapabilityFullySupported)
	}

	return b
}

func encodeRequestHeader() []byte {
	var b []byte

	b = appendVarintField(b, 1, uint64(randomTraceID())) //nolint:gosec // masked non-negative
	b = appendVarintField(b, 2, protoClientTypeIOS)
	b = appendVarintField(b, 3, uint64(protoClientVersion))
	b = appendStringField(b, 4, "en")
	b = appendBytesField(b, 5, encodeClientFeatureCapabilities())

	return b
}

func encodeUserID(id string) []byte {
	var b []byte

	b = appendStringField(b, 1, id)
	b = appendVarintField(b, 2, protoUserTypeHuman)

	return b
}

func encodeGroupID(ref protoGroupRef) []byte {
	if ref.ID == "" {
		return nil
	}

	inner := appendStringField(nil, 1, ref.ID)
	if ref.IsDM {
		return appendBytesField(nil, 3, inner)
	}

	return appendBytesField(nil, 1, inner)
}

func encodeTopicID(ref protoGroupRef, topicID string) []byte {
	var b []byte

	b = appendStringField(b, 2, topicID)
	if gid := encodeGroupID(ref); len(gid) > 0 {
		b = appendBytesField(b, 3, gid)
	}

	return b
}

func encodeMessageParentID(ref protoGroupRef, topicID string) []byte {
	return appendBytesField(nil, 4, encodeTopicID(ref, topicID))
}

func encodeMessageID(ref protoGroupRef, topicID, msgID string) []byte {
	if topicID == "" {
		topicID = msgID
	}

	if msgID == "" {
		msgID = topicID
	}

	var b []byte

	b = appendBytesField(b, 1, encodeMessageParentID(ref, topicID))
	b = appendStringField(b, 2, msgID)

	return b
}

func encodeEmoji(unicodeStr string) []byte {
	return appendStringField(nil, 1, unicodeStr)
}

func encodeMemberID(userID, email string) []byte {
	var b []byte

	if userID != "" {
		b = appendBytesField(b, 1, encodeUserID(userID))
	}

	if email != "" {
		b = appendStringField(b, 3, email)
	}

	return b
}

func encodeInviteeMemberInfo(userID, email string) []byte {
	var invitee []byte

	if userID != "" {
		invitee = appendBytesField(invitee, 1, encodeUserID(userID))
	}

	if email != "" {
		invitee = appendStringField(invitee, 2, email)
	}

	return appendBytesField(nil, 1, invitee)
}

func encodeMessageInfo() []byte {
	return appendBoolField(nil, 1, true)
}

func encodeOutgoingAnnotation(ann outgoingAnnotation) []byte {
	var b []byte

	b = appendVarintField(b, 1, ann.Type)
	if ann.StartIndex >= 0 {
		b = appendVarintField(b, 2, uint64(ann.StartIndex))
	}

	if ann.Length > 0 {
		b = appendVarintField(b, 3, uint64(ann.Length))
	}

	switch ann.Type {
	case protoAnnotationUserMention:
		var um []byte

		if ann.MentionAll {
			um = appendVarintField(um, 2, protoMentionAll)
		} else {
			um = appendBytesField(um, 1, encodeUserID(ann.UserID))
			um = appendVarintField(um, 2, protoMentionUser)
			um = appendStringField(um, 4, ann.DisplayName)
		}

		b = appendBytesField(b, 5, um)
	case protoAnnotationFormatData:
		fm := appendVarintField(nil, 1, ann.FormatType)
		b = appendBytesField(b, 8, fm)
		b = appendVarintField(b, 20, protoChipDoNotRender)
	}

	return b
}

func encodeGetSelfUserStatusRequest() []byte {
	return appendBytesField(nil, 100, encodeRequestHeader())
}

func encodePaginatedWorldRequest(pageSize int32, anchorSortMicros int64, pageToken string) []byte {
	var section []byte

	section = appendVarintField(section, 1, uint64(pageSize)) //nolint:gosec // positive constant
	if anchorSortMicros > 0 {
		section = appendVarintField(section, 3, uint64(anchorSortMicros))
	}

	if pageToken != "" {
		section = appendStringField(section, 6, pageToken)
	}

	var b []byte

	// PaginatedWorldRequest places request_header at field 1 (not 100).
	b = appendBytesField(b, 1, encodeRequestHeader())
	b = appendBytesField(b, 2, section)
	b = appendVarintField(b, 4, 5) // FETCH_USER_PROFILES_FOR_GROUP_NAMING
	b = appendVarintField(b, 4, 6) // FETCH_SNIPPET_SENDER_PROFILES
	b = appendBoolField(b, 5, true)
	b = appendBoolField(b, 7, true)

	return b
}

func encodeGetGroupRequest(ref protoGroupRef) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeGroupID(ref))
	b = appendVarintField(b, 4, protoGroupFetchMembers)
	b = appendVarintField(b, 4, protoGroupFetchMemberIDs)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeGetMembersRequest(userIDs []string) []byte {
	var b []byte

	for _, id := range userIDs {
		if id == "" {
			continue
		}

		b = appendBytesField(b, 1, encodeMemberID(id, ""))
	}

	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeListMembersRequest(ref protoGroupRef, pageSize int32, pageToken string) []byte {
	var b []byte

	if !ref.IsDM {
		b = appendBytesField(b, 1, appendStringField(nil, 1, ref.ID))
	}

	b = appendVarintField(b, 3, uint64(pageSize)) //nolint:gosec // positive page size
	b = appendStringField(b, 4, pageToken)
	b = appendBytesField(b, 5, encodeGroupID(ref))
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeGetUserPresenceRequest(userIDs []string) []byte {
	var b []byte

	for _, id := range userIDs {
		if id == "" {
			continue
		}

		b = appendBytesField(b, 1, encodeUserID(id))
	}

	b = appendBoolField(b, 2, true) // include_active_until
	b = appendBoolField(b, 3, true) // include_user_status
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCreateTopicRequest(ref protoGroupRef, text, localID string, anns []outgoingAnnotation) []byte {
	var b []byte

	b = appendStringField(b, 2, text)
	for _, ann := range anns {
		b = appendBytesField(b, 3, encodeOutgoingAnnotation(ann))
	}

	b = appendStringField(b, 4, localID)
	b = appendBytesField(b, 5, encodeGroupID(ref))
	b = appendBoolField(b, 8, true) // history_v2
	b = appendBytesField(b, 9, encodeMessageInfo())
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCreateMessageRequest(ref protoGroupRef, topicID, text, localID string, anns []outgoingAnnotation) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeMessageParentID(ref, topicID))
	b = appendStringField(b, 2, text)

	for _, ann := range anns {
		b = appendBytesField(b, 3, encodeOutgoingAnnotation(ann))
	}

	b = appendStringField(b, 4, localID)
	b = appendBytesField(b, 7, encodeMessageInfo())
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeEditMessageRequest(ref protoGroupRef, topicID, msgID, text string, anns []outgoingAnnotation) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeMessageID(ref, topicID, msgID))
	b = appendStringField(b, 2, text)

	for _, ann := range anns {
		b = appendBytesField(b, 3, encodeOutgoingAnnotation(ann))
	}

	b = appendBytesField(b, 5, encodeMessageInfo())
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeDeleteMessageRequest(ref protoGroupRef, topicID, msgID string) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeMessageID(ref, topicID, msgID))
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeListTopicsRequest(ref protoGroupRef, topicLimit, replyLimit int32) []byte {
	var b []byte

	b = appendVarintField(b, 2, uint64(topicLimit)) //nolint:gosec // positive limit
	b = appendVarintField(b, 3, uint64(replyLimit)) //nolint:gosec // positive limit
	b = appendVarintField(b, 5, protoListTopicsFetchUser)
	b = appendBytesField(b, 8, encodeGroupID(ref))
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeListMessagesRequest(ref protoGroupRef, topicID string, pageSize int32) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeMessageParentID(ref, topicID))
	b = appendVarintField(b, 2, uint64(pageSize)) //nolint:gosec // positive page size
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCatchUpGroupRequest(ref protoGroupRef, fromUsec int64, pageSize int32) []byte {
	var cr []byte
	if fromUsec > 0 {
		cr = appendVarintField(cr, 1, uint64(fromUsec))
	}

	var b []byte

	b = appendBytesField(b, 1, encodeGroupID(ref))
	b = appendBytesField(b, 2, cr)
	b = appendVarintField(b, 3, uint64(pageSize)) //nolint:gosec // positive page size
	b = appendVarintField(b, 4, uint64(pageSize)) //nolint:gosec // positive cutoff
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeUpdateReactionRequest(ref protoGroupRef, topicID, msgID, emojiUnicode string, add bool) []byte {
	opt := uint64(protoReactionAdd)
	if !add {
		opt = protoReactionRemove
	}

	var b []byte

	b = appendBytesField(b, 1, encodeMessageID(ref, topicID, msgID))
	b = appendBytesField(b, 2, encodeEmoji(emojiUnicode))
	b = appendVarintField(b, 3, opt)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeSetTypingStateRequest(ref protoGroupRef, topicID string, typing bool) []byte {
	state := uint64(protoTypingStarted)
	if !typing {
		state = protoTypingStopped
	}

	var ctx []byte
	if topicID != "" {
		ctx = appendBytesField(ctx, 2, encodeTopicID(ref, topicID))
	} else {
		ctx = appendBytesField(ctx, 1, encodeGroupID(ref))
	}

	var b []byte

	b = appendVarintField(b, 1, state)
	b = appendBytesField(b, 2, ctx)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeMarkGroupReadstateRequest(ref protoGroupRef, lastReadUsec int64) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeGroupID(ref))
	if lastReadUsec > 0 {
		b = appendVarintField(b, 2, uint64(lastReadUsec))
	}

	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCreateGroupRequest(name string) []byte {
	var space []byte

	space = appendStringField(space, 1, name)
	// Modern inline-threaded spaces use FlatGroup (field 5) with inline threading enabled server-side.
	space = appendBytesField(space, 5, nil)

	var b []byte

	b = appendBytesField(b, 1, space)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCreateDMRequest(userIDs []string, emails []string) []byte {
	var b []byte

	for _, id := range userIDs {
		if id == "" {
			continue
		}

		b = appendBytesField(b, 1, encodeUserID(id))
		b = appendBytesField(b, 7, appendBytesField(nil, 1, encodeUserID(id)))
	}

	for _, email := range emails {
		if email == "" {
			continue
		}

		b = appendBytesField(b, 7, appendStringField(nil, 2, email))
	}

	b = appendBytesField(b, 3, appendVarintField(nil, 1, protoRetentionPermanent))
	b = appendBoolField(b, 10, true) // force_empty_dm_visible
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeCreateMembershipRequest(ref protoGroupRef, userID, email string) []byte {
	var b []byte

	b = appendBytesField(b, 2, encodeMemberID(userID, email))
	b = appendVarintField(b, 3, protoMembershipJoined)
	b = appendBytesField(b, 4, encodeGroupID(ref))
	b = appendBytesField(b, 8, encodeInviteeMemberInfo(userID, email))
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeRemoveMembershipsRequest(ref protoGroupRef, userID string) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeMemberID(userID, ""))
	b = appendBytesField(b, 2, encodeGroupID(ref))
	b = appendVarintField(b, 3, protoMembershipJoined)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeHideGroupRequest(ref protoGroupRef, hide bool) []byte {
	var b []byte

	b = appendBytesField(b, 1, encodeGroupID(ref))
	b = appendBoolField(b, 2, hide)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeUpdateGroupDescriptionRequest(spaceID, description string) []byte {
	details := appendStringField(nil, 1, description)

	var b []byte

	b = appendBytesField(b, 1, appendStringField(nil, 1, spaceID))
	b = appendVarintField(b, 4, protoUpdateMaskSpaceDetails)
	b = appendBytesField(b, 7, details)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeSetPresenceSharedRequest(shared bool) []byte {
	var b []byte

	b = appendBoolField(b, 1, shared)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeSetDndDurationRequest(dnd bool, durationUsec int64) []byte {
	state := uint64(protoDndAvailable)
	if dnd {
		state = protoDndActive
	}

	var b []byte

	if dnd && durationUsec > 0 {
		b = appendVarintField(b, 1, uint64(durationUsec))
	}

	b = appendVarintField(b, 2, state)
	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeSetCustomStatusRequest(emojiUnicode, text string, expiry time.Time) []byte {
	var cs []byte

	cs = appendStringField(cs, 1, text)
	if emojiUnicode != "" {
		cs = appendStringField(cs, 2, emojiUnicode)
		cs = appendBytesField(cs, 4, encodeEmoji(emojiUnicode))
	}

	var b []byte

	if text != "" || emojiUnicode != "" {
		b = appendBytesField(b, 1, cs)
	}

	if !expiry.IsZero() && expiry.After(time.Now()) {
		b = appendVarintField(b, 2, uint64(expiry.UnixMicro()))
	}

	b = appendBytesField(b, 100, encodeRequestHeader())

	return b
}

func encodeHeartbeatRequest(active bool) []byte {
	state := uint64(protoHeartbeatUserInactive)
	if active {
		state = protoHeartbeatUserActive
	}

	var b []byte

	// HeartbeatRequest places request_header at field 1 (not 100).
	b = appendBytesField(b, 1, encodeRequestHeader())
	b = appendBytesField(b, 2, appendVarintField(nil, 1, state))

	return b
}

func encodeStreamClientInfo() []byte {
	var b []byte

	b = appendVarintField(b, 1, protoPlatformMobile)
	b = appendVarintField(b, 2, protoOriginIOSProd)
	b = appendBytesField(b, 4, encodeClientFeatureCapabilities())

	return b
}

func encodeStreamPingRequest(active bool) []byte {
	state := uint64(protoPingStateIdle)
	focus := uint64(protoFocusBackground)

	if active {
		state = protoPingStateActive
		focus = protoFocusForeground
	}

	var ping []byte

	ping = appendVarintField(ping, 1, state)
	ping = appendVarintField(ping, 3, focus)
	ping = appendVarintField(ping, 5, protoInteractive)
	ping = appendBoolField(ping, 6, true)

	var b []byte

	b = appendBytesField(b, 2, ping)
	b = appendVarintField(b, 4, protoPlatformMobile)
	b = appendBytesField(b, 5, encodeStreamClientInfo())

	return b
}

func encodeStreamGroupSubscriptionRequest(refs []protoGroupRef) []byte {
	var sub []byte

	for _, ref := range refs {
		if gid := encodeGroupID(ref); len(gid) > 0 {
			sub = appendBytesField(sub, 1, gid)
		}
	}

	var b []byte

	b = appendVarintField(b, 4, protoPlatformMobile)
	b = appendBytesField(b, 5, encodeStreamClientInfo())
	b = appendBytesField(b, 8, sub)

	return b
}
