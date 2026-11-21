package rocketchatclient

import "time"

// Handler receives normalized Rocket.Chat events from the client.
// All callbacks run sequentially on the client's event consumer goroutine and must never block.
type Handler interface {
	Ready()
	OnMessage(msg *Message)
	OnEdit(msg *Message)
	OnDelete(roomID, msgID string)
	OnReaction(ev *ReactionEvent)
	OnRoomChange(room *Room)
	OnPresence(userID, status string)
}

// InviteHandler can optionally be implemented by a Handler to receive room invitations.
type InviteHandler interface {
	OnInvite(invite *Invitation)
}

// MemberHandler can optionally be implemented by a Handler to receive room membership events.
type MemberHandler interface {
	OnMember(ev *MemberEvent)
}

// ReconnectHandler can optionally be implemented by a Handler to be notified when the
// DDP stream recovers after a disconnection.
type ReconnectHandler interface {
	OnReconnect(downtime time.Duration)
}
