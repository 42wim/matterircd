package irckit

import (
	"net"
	"strings"

	"github.com/sorcix/irc"
)

// Conn abstracts the encoding/decoding and sending/receiving when speaking IRC.
type Conn interface {
	Close() error
	Encode(*irc.Message) error
	Decode() (*irc.Message, error)

	// ResolveHost returns the resolved host of the RemoteAddr
	ResolveHost() string
}

type conn struct {
	net.Conn
	*irc.Encoder
	*irc.Decoder
}

// resolveHost will convert an IP to a Hostname, but fall back to IP on error.
func (c *conn) ResolveHost() string {
	addr := c.RemoteAddr()

	s := addr.String()
	ip, _, err := net.SplitHostPort(s)
	if err != nil {
		return s
	}

	names, err := net.LookupAddr(ip)
	if err != nil {
		return ip
	}

	return strings.TrimSuffix(names[0], ".")
}
