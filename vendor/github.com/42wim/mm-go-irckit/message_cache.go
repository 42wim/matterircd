package irckit

import (
	"sync"
	"testing"
)

// represents the number of times a message has been sent
// (in case a user submits the same message multiple times
// before receiving it back)
type messageCounts map[string]int

// keeps track of message counts per channel
type channelMessages map[string]messageCounts

// our structure for keeping track of messages a user sent,
// per-channel
type messageCache struct {
	cache channelMessages
	m     sync.Mutex
}

// creates/instantiates a new message cache
func newMessageCache() *messageCache {
	return &messageCache{
		cache: make(channelMessages),
	}
}

// increments the message's count in our cache, for the given channel.
// creates the messageCounts for that channel structure as needed.
func (c *messageCache) addMessage(channel, message string) {
	c.m.Lock()
	defer c.m.Unlock()
	if _, ok := c.cache[channel]; !ok {
		// add this channel since it doesn't exist in the cache yet
		c.cache[channel] = make(messageCounts)
	}
	c.cache[channel][message]++
}

// checks whether the given message is in our cache for the given channel.
// if not, returns false.
// if it is, decrements the message count for that channel and returns true.
// deletes cache map and messageCounts entries as needed to save memory.
func (c *messageCache) messageIn(channel, message string) bool {
	c.m.Lock()
	defer c.m.Unlock()

	if _, ok := c.cache[channel]; !ok {
		// if the channel doesn't even exist, the user definitely didn't send the message
		return false
	}

	_, ok := c.cache[channel][message]
	if ok {
		// user sent this message to this channel. decrement our count.
		c.cache[channel][message]--

		if c.cache[channel][message] <= 0 {
			// if the count for this message in this channel reaches 0, we
			// can evict that message rather than leaving it in our messageCounts map
			delete(c.cache[channel], message)

			if len(c.cache[channel]) == 0 {
				// if there are no messages remaining in this channel, we can
				// delete the entry for it
				delete(c.cache, channel)
			}
		}
		return true
	}
	return false
}

// TestMessageCache provides some basic tests for our sent message cache
func TestMessageCache(t *testing.T) {
	u := newMessageCache()
	c, nc, s, ns := "test", "notest", "hello", "goodbye"
	u.addMessage(c, s)
	if u.messageIn(nc, s) {
		t.Errorf("message [%s] should not be in but is!", s)
	}
	if u.messageIn(nc, ns) {
		t.Errorf("message [%s] should not be in but is!", s)
	}
	if !u.messageIn(c, s) {
		t.Errorf("message [%s] should be in but is not!", s)
	}
	if u.messageIn(c, s) {
		t.Errorf("message [%s] should not be in but is!", s)
	}
	if u.messageIn(c, ns) {
		t.Errorf("message [%s] should not be in but is!", s)
	}
	u.addMessage(c, s)
	u.addMessage(c, s)
	if !u.messageIn(c, s) || !u.messageIn(c, ns) {
		t.Errorf("message [%s] should be in but is not!", s)
	}
}
