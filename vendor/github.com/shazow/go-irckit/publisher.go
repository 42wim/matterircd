package irckit

import (
	"sync"

	"github.com/sorcix/irc"
)

//go:generate stringer -type=EventKind
type EventKind int

const (
	_ EventKind = iota
	// ConnectEvent is emitted when a User successfully connects and handshakes with a Server.
	ConnectEvent
	// QuitEvent is emitted when a User is disconnected from a Server.
	QuitEvent
	// JoinEvent is emitted when a User joins a Channel.
	JoinEvent
	// PartEvent is emitted when a User leaves a Channel.
	PartEvent
	// UserMsgEvent is emitted when a User sends a message to another User.
	UserMsgEvent
	// ChanMsgEvent is emitted when a User sends a message to a Channel.
	ChanMsgEvent
	// EmptyChanEvent is emitted when the last User leaves a Channel.
	EmptyChanEvent
	// NewChanEvent is emitted when a new Channel is created.
	NewChanEvent
	// ShutdownEvent is emitted when the server shuts down.
	ShutdownEvent
)

type event struct {
	kind    EventKind
	server  Server
	channel Channel
	user    *User
	message *irc.Message
}

func (evt event) Kind() EventKind       { return evt.kind }
func (evt event) Server() Server        { return evt.server }
func (evt event) Channel() Channel      { return evt.channel }
func (evt event) User() *User           { return evt.user }
func (evt event) Message() *irc.Message { return evt.message }
func (evt event) String() string {
	r := evt.Kind().String()
	if u := evt.User(); u != nil {
		r += " from " + u.String()
	}
	if ch := evt.Channel(); ch != nil {
		r += " in " + ch.String()
	}
	return r
}

// Event is emitted by a Publisher.
type Event interface {
	// String returns a user-friendly presentation of the event.
	String() string
	// Kind is the event kind.
	Kind() EventKind
	// Server associated with the event (or nil).
	Server() Server
	// Channel associated with the event (or nil).
	Channel() Channel
	// User associated with the event (or nil).
	User() *User
	// Message returns the original irc.Message that triggered the event (or nil).
	Message() *irc.Message
}

// Publisher emits Events to existing subscribers.
type Publisher interface {
	// Subscribe registers channel to receive events. Will skip events if channel is full.
	Subscribe(chan<- Event)

	// Unsubscribe stops the channel from receiving further events. Returns false if channel was not subscribed to start with.
	// TODO: Unsubcribe(chan<- Event) bool

	// Publish emits the Event to all the subscribers.
	Publish(Event)

	// Close will close all the subscribing channels.
	Close() error
}

// SyncPublisher creates a Publisher which blocks on all operations.
func SyncPublisher() Publisher {
	return &publisher{}
}

type publisher struct {
	// TODO: Could make a lock-free version of this with a goroutine+broadcast channel. Not sure it would be worthwhile though.
	mu          sync.Mutex
	subscribers []chan<- Event
}

func (pub *publisher) Subscribe(sub chan<- Event) {
	pub.mu.Lock()
	pub.subscribers = append(pub.subscribers, sub)
	pub.mu.Unlock()
}

func (pub *publisher) Publish(evt Event) {
	// TODO: Should this be non-blocking? Could do that with the broadcast channel.
	pub.mu.Lock()
	for _, sub := range pub.subscribers {
		select {
		case sub <- evt:
		default: // Skip if full.
		}
	}
	pub.mu.Unlock()
}

func (pub *publisher) Close() error {
	pub.mu.Lock()
	for _, sub := range pub.subscribers {
		close(sub)
	}
	pub.subscribers = []chan<- Event{}
	pub.mu.Unlock()
	return nil
}
