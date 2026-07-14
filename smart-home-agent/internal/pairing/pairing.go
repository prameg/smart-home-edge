// Package pairing runs at most one device-pairing session on the gateway,
// bridging cloud pairing.* commands to a protocol backend (Zigbee2MQTT today,
// Matter later) and emitting per-phase events the agent relays upstream.
package pairing

import (
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/smart-home/edge/agent/internal/contract"
)

// ErrBusy means a session is already active; mirrors the cloud's 409.
var ErrBusy = errors.New("pairing session already active")

// Event is one pairing-phase transition from the backend (or the manager's
// own timer). Phase uses the contract.PairingPhase* strings.
type Event struct {
	Phase     string
	Device    *contract.PairingDevice
	Reason    string
	ExpiresAt time.Time
}

// Backend opens/closes the protocol's join window. Implementations emit
// progress via the manager's HandleBackendEvent.
type Backend interface {
	Start(seconds int) error
	Stop() error
}

// Manager serializes sessions: one active at a time, a local timeout as the
// safety net (the backend also closes its window on-device).
type Manager struct {
	mu      sync.Mutex
	backend Backend
	emit    func(sessionID string, ev Event)
	log     *slog.Logger

	activeID string
	timer    *time.Timer

	// timeoutFor is overridable in tests.
	timeoutFor func(durationSec int) time.Duration
}

// NewManager builds a Manager. emit is called for every phase, including the
// synthetic started/stopped ones the manager itself produces.
func NewManager(backend Backend, emit func(sessionID string, ev Event), log *slog.Logger) *Manager {
	return &Manager{
		backend:    backend,
		emit:       emit,
		log:        log,
		timeoutFor: func(durationSec int) time.Duration { return time.Duration(durationSec) * time.Second },
	}
}

// Start opens the join window for sessionID. Emits `started` on success.
func (m *Manager) Start(sessionID string, durationSec int) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.activeID != "" {
		return ErrBusy
	}

	if err := m.backend.Start(durationSec); err != nil {
		return err
	}

	m.activeID = sessionID
	m.timer = time.AfterFunc(m.timeoutFor(durationSec), func() {
		m.close(sessionID, Event{Phase: contract.PairingPhaseStopped, Reason: "timeout"})
	})

	m.emit(sessionID, Event{
		Phase:     contract.PairingPhaseStarted,
		ExpiresAt: time.Now().Add(time.Duration(durationSec) * time.Second),
	})

	return nil
}

// Stop closes the window early (user cancel). No-op for a stale session id.
func (m *Manager) Stop(sessionID, reason string) {
	m.close(sessionID, Event{Phase: contract.PairingPhaseStopped, Reason: reason})
}

// HandleBackendEvent routes a backend progress event to the active session.
// Terminal phases (completed/failed) free the slot and close the window.
func (m *Manager) HandleBackendEvent(ev Event) {
	m.mu.Lock()
	sessionID := m.activeID
	m.mu.Unlock()

	if sessionID == "" {
		return // no active session; a neighbor's device joined outside a wizard
	}

	switch ev.Phase {
	case contract.PairingPhaseCompleted, contract.PairingPhaseFailed:
		m.close(sessionID, ev)
	default:
		m.emit(sessionID, ev)
	}
}

// close ends the active session (if it is sessionID), stops the backend, and
// emits the terminal event exactly once.
func (m *Manager) close(sessionID string, ev Event) {
	m.mu.Lock()
	if m.activeID != sessionID || sessionID == "" {
		m.mu.Unlock()

		return
	}
	m.activeID = ""
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.mu.Unlock()

	if err := m.backend.Stop(); err != nil {
		m.log.Warn("pairing backend stop failed", "error", err)
	}

	m.emit(sessionID, ev)
}
