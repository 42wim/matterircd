package irckit

import (
	"reflect"
	"testing"

	"github.com/sorcix/irc"
)

type mockConn struct {
	send    chan *irc.Message
	receive chan *irc.Message
	host    string
}

func (conn *mockConn) Close() error {
	return nil
}

func (conn *mockConn) Encode(msg *irc.Message) error {
	conn.send <- msg
	return nil
}

func (conn *mockConn) Decode() (*irc.Message, error) {
	return <-conn.receive, nil
}

func (conn *mockConn) ResolveHost() string {
	return conn.host
}

func NewConnMock(host string, capacity int) *mockConn {
	return &mockConn{
		send:    make(chan *irc.Message, capacity),
		receive: make(chan *irc.Message, capacity),
		host:    host,
	}
}

func NewUserMock(send chan *irc.Message, receive chan *irc.Message) *User {
	return NewUser(&mockConn{
		send:    send,
		receive: receive,
		host:    "mockhost.local",
	})
}

func TestUserMock(t *testing.T) {
	send, receive := make(chan *irc.Message, 1), make(chan *irc.Message, 1)
	u := NewUserMock(send, receive)

	expect := &irc.Message{Command: irc.RPL_WELCOME}
	u.Encode(expect)
	msg := <-send
	if !reflect.DeepEqual(msg, expect) {
		t.Errorf("got %v; want %v", msg, expect)
	}

	expect = &irc.Message{Command: irc.QUIT}
	receive <- expect
	msg, _ = u.Decode()
	if !reflect.DeepEqual(msg, expect) {
		t.Errorf("got %v; want %v", msg, expect)
	}
}
