package history

import (
	"fmt"
	"io"
	"sync"
)

// Message is an interface for an entry stored in History.
type Message fmt.Stringer

// History is an interface for providing interface storage.
type History interface {
	Add(Message)
	Get(int) []Message
	Len() int
}

// History contains the history entries
type memHistory struct {
	sync.RWMutex
	entries []Message
	head    int
	size    int
	out     io.Writer
}

// MemoryHistory returns a History implementation which stores messages in
// memory. When an out writer is provided, each message will be written to it
// on arrival.
func MemoryHistory(size int, out io.Writer) History {
	return &memHistory{
		entries: make([]Message, size),
		out:     out,
	}
}

// Add adds the given entry to the entries in the history
func (h *memHistory) Add(entry Message) {
	h.Lock()
	defer h.Unlock()

	max := cap(h.entries)
	h.head = (h.head + 1) % max
	h.entries[h.head] = entry
	if h.size < max {
		h.size++
	}

	if h.out != nil {
		fmt.Fprint(h.out, entry.String())
	}
}

// Len returns the number of entries in the history
func (h *memHistory) Len() int {
	return h.size
}

// Get returns the latest num of messages in chronological order.
func (h *memHistory) Get(num int) []Message {
	h.RLock()
	defer h.RUnlock()

	max := cap(h.entries)
	if num > h.size {
		num = h.size
	}

	r := make([]Message, num)
	for i := 0; i < num; i++ {
		idx := (h.head - i) % max
		if idx < 0 {
			idx += max
		}
		r[num-i-1] = h.entries[idx]
	}

	return r
}
