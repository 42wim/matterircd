package matrixclient

import (
	"context"
	"slices"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	historyPageSize = 100
	historyMaxPages = 20 // bounds GetMessagesSince on busy rooms
)

// UserInfo is a user found in the homeserver user directory.
type UserInfo struct {
	ID          id.UserID
	DisplayName string
}

// messageFromEvent converts a fetched event, returning nil for anything that
// is not a plain message (edits are folded into their original by the server).
func (c *Client) messageFromEvent(ev *event.Event) *Message {
	if ev == nil || ev.Type != event.EventMessage {
		return nil
	}

	err := ev.Content.ParseRaw(ev.Type)
	if err != nil {
		return nil
	}

	content := ev.Content.AsMessage()
	if content == nil || content.RelatesTo.GetReplaceID() != "" {
		return nil
	}

	msg := &Message{
		RoomID:    ev.RoomID,
		EventID:   ev.ID,
		Sender:    ev.Sender,
		Text:      content.Body,
		Direct:    c.IsDirect(ev.RoomID),
		Timestamp: ev.Timestamp,
		MsgType:   content.MsgType,
		URL:       content.URL,
		FileName:  content.GetFileName(),
		Info:      content.Info,
		File:      content.File,
	}
	if content.RelatesTo != nil {
		msg.ParentID = content.RelatesTo.EventID
	}

	return msg
}

func (c *Client) messagesFromEvents(events []*event.Event) []*Message {
	msgs := make([]*Message, 0, len(events))

	for _, ev := range events {
		msg := c.messageFromEvent(ev)
		if msg != nil {
			msgs = append(msgs, msg)
		}
	}

	return msgs
}

// GetMessages returns up to limit of the latest messages of a room, oldest first.
func (c *Client) GetMessages(ctx context.Context, roomID id.RoomID, limit int) ([]*Message, error) {
	msgs := make([]*Message, 0, limit)
	token := ""

	for len(msgs) < limit {
		page, next, err := c.messagesPage(ctx, roomID, token, min(limit-len(msgs), historyPageSize))
		if err != nil {
			return nil, err
		}

		msgs = append(msgs, c.messagesFromEvents(page)...)

		if next == "" || len(page) == 0 {
			break
		}

		token = next
	}

	slices.Reverse(msgs)

	return msgs, nil
}

// GetMessagesSince returns the messages of a room newer than since (unix
// milliseconds), oldest first. At most historyMaxPages pages are fetched.
func (c *Client) GetMessagesSince(ctx context.Context, roomID id.RoomID, since int64) ([]*Message, error) {
	var msgs []*Message

	token := ""

	for range historyMaxPages {
		page, next, err := c.messagesPage(ctx, roomID, token, historyPageSize)
		if err != nil {
			return nil, err
		}

		done := next == "" || len(page) == 0

		for _, msg := range c.messagesFromEvents(page) {
			if msg.Timestamp < since {
				done = true

				break
			}

			msgs = append(msgs, msg)
		}

		if done {
			break
		}

		token = next
	}

	slices.Reverse(msgs)

	return msgs, nil
}

// messagesPage returns a page of events, newest first, and the token of the next page.
func (c *Client) messagesPage(ctx context.Context, roomID id.RoomID, from string, limit int) ([]*event.Event, string, error) {
	filter := &mautrix.FilterPart{Types: []event.Type{event.EventMessage}}

	resp, err := c.mc.Messages(ctx, roomID, from, "", mautrix.DirectionBackward, filter, limit)
	if err != nil {
		return nil, "", err
	}

	return resp.Chunk, resp.End, nil
}

// GetThread returns the root message of a thread followed by its replies, oldest first.
func (c *Client) GetThread(ctx context.Context, roomID id.RoomID, rootID id.EventID) ([]*Message, error) {
	root, err := c.Event(ctx, roomID, rootID)
	if err != nil {
		return nil, err
	}

	if root.RoomID == "" {
		root.RoomID = roomID
	}

	msgs := c.messagesFromEvents([]*event.Event{root})
	token := ""

	for range historyMaxPages {
		resp, err := c.mc.GetRelations(ctx, roomID, rootID, &mautrix.ReqGetRelations{
			RelationType: event.RelThread,
			Dir:          mautrix.DirectionForward,
			From:         token,
			Limit:        historyPageSize,
		})
		if err != nil {
			return nil, err
		}

		for _, ev := range resp.Chunk {
			if ev.RoomID == "" {
				ev.RoomID = roomID
			}
		}

		msgs = append(msgs, c.messagesFromEvents(resp.Chunk)...)

		if resp.NextBatch == "" {
			break
		}

		token = resp.NextBatch
	}

	return msgs, nil
}

// SearchMessages does a full text search over the messages of the joined rooms, best match first.
func (c *Client) SearchMessages(ctx context.Context, query string) ([]*Message, error) {
	resp, err := c.mc.Search(ctx, &mautrix.ReqSearch{SearchTerm: query, OrderBy: "rank"})
	if err != nil {
		return nil, err
	}

	if resp == nil {
		return nil, nil
	}

	events := make([]*event.Event, 0, len(resp.Results))
	for _, r := range resp.Results {
		events = append(events, r.Event)
	}

	return c.messagesFromEvents(events), nil
}

// SearchUsers searches the homeserver user directory.
func (c *Client) SearchUsers(ctx context.Context, query string, limit int) ([]UserInfo, error) {
	resp, err := c.mc.SearchUserDirectory(ctx, query, limit)
	if err != nil {
		return nil, err
	}

	users := make([]UserInfo, 0, len(resp.Results))
	for _, u := range resp.Results {
		users = append(users, UserInfo{ID: u.UserID, DisplayName: u.DisplayName})
	}

	return users, nil
}
