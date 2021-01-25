package matrix

import (
	"fmt"
	"time"

	"github.com/davecgh/go-spew/spew"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Syncer struct {
	m *Matrix
}

func NewSyncer(m *Matrix) *Syncer {
	return &Syncer{
		m: m,
	}
}

func (s *Syncer) ProcessResponse(resp *mautrix.RespSync, since string) error {
	for room, sync := range resp.Rooms.Join {
		fmt.Println(room)

		for _, ev := range append(append(
			append(sync.State.Events, sync.Timeline.Events...),
			sync.Ephemeral.Events...),
			sync.AccountData.Events...) {
			ev.Content.ParseRaw(ev.Type)
			ev.RoomID = room
			spew.Dump(ev)

			switch ev.Type {
			case event.StateCanonicalAlias:
				s.m.handleCanonicalAlias(mautrix.EventSourceState, ev)
			case event.StateRoomName:
				s.m.handleRoomName(mautrix.EventSourceState, ev)
			case event.StateMember:
				s.m.handleMember(mautrix.EventSourceState, ev)
			case event.AccountDataDirectChats:
				s.m.handleDM(mautrix.EventSourceAccountData, ev)
			}
		}
		return nil
	}

	return nil
}

func (s *Syncer) OnFailedSync(res *mautrix.RespSync, err error) (time.Duration, error) {
	return 10 * time.Second, nil
}

func (s *Syncer) GetFilterJSON(userID id.UserID) *mautrix.Filter {
	return &mautrix.Filter{
		Room: mautrix.RoomFilter{
			Timeline: mautrix.FilterPart{
				Limit: 50,
			},
		},
	}
}
