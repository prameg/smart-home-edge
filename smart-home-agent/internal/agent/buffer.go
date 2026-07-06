package agent

import "sync"

// pending is one uplink message held while the broker is unreachable.
type pending struct {
	topic   string
	payload []byte
	qos     byte
	retain  bool
}

// uplinkBuffer is a bounded, drop-oldest ring for uplink messages produced while
// offline. This is the app-level guarantee the plan calls for ("bounded
// drop-oldest buffer holds recent uplinks"): the newest telemetry always wins
// over the oldest when the WAN is down for a long time, so memory stays bounded
// and reconnect flushes the most recent state rather than an unbounded backlog.
//
// It complements (does not replace) the Paho persistent-session store used for
// QoS-1 downlink buffering at the broker.
type uplinkBuffer struct {
	mu    sync.Mutex
	items []pending
	limit int
}

func newUplinkBuffer(limit int) *uplinkBuffer {
	return &uplinkBuffer{
		items: make([]pending, 0, limit),
		limit: limit,
	}
}

// push appends a message, dropping the oldest when at capacity.
func (b *uplinkBuffer) push(p pending) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) >= b.limit {
		// Drop oldest.
		b.items = b.items[1:]
	}

	b.items = append(b.items, p)
}

// drain returns all buffered messages and empties the buffer.
func (b *uplinkBuffer) drain() []pending {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.items) == 0 {
		return nil
	}

	out := b.items
	b.items = make([]pending, 0, b.limit)

	return out
}
