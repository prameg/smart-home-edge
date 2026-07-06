package agent

import "sync"

// cmdDedupe is a bounded set of recently-processed command ids so a QoS-1
// redelivery (broker resends an un-PUBACK'd cmd on reconnect) never re-invokes
// the HA service. The cloud is also idempotent on cmd_id, so this is a
// double-actuation guard, not a correctness dependency; it is intentionally
// in-memory and best-effort (a reboot may allow one re-apply, which is safe).
type cmdDedupe struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
	limit int
}

func newCmdDedupe(limit int) *cmdDedupe {
	return &cmdDedupe{
		seen:  make(map[string]struct{}, limit),
		order: make([]string, 0, limit),
		limit: limit,
	}
}

// firstSee returns true the first time it sees an id, false on repeats.
func (d *cmdDedupe) firstSee(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if _, ok := d.seen[id]; ok {
		return false
	}

	if len(d.order) >= d.limit {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}

	d.seen[id] = struct{}{}
	d.order = append(d.order, id)

	return true
}
