package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/pairing"
	"github.com/smart-home/edge/agent/internal/provision"
)

type nopBackend struct{}

func (nopBackend) Start(int) error { return nil }
func (nopBackend) Stop() error     { return nil }

// newPairingTestAgent builds a minimal agent whose broker is nil, so every
// uplink lands in the drain-able buffer.
func newPairingTestAgent(t *testing.T, withPairing bool) *Agent {
	t.Helper()

	a := &Agent{
		log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		creds:  &provision.Credentials{UID: "gw-test"},
		buffer: newUplinkBuffer(64),
		dedupe: newCmdDedupe(64),
	}

	if withPairing {
		a.pairing = pairing.NewManager(nopBackend{}, a.emitPairingEvent, a.log)
	}

	return a
}

func pairingStartCmd(cmdID, sessionID string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"cmd_id":  cmdID,
		"action":  "pairing.start",
		"params":  map[string]any{"protocol": "zigbee", "duration_sec": 60, "session_id": sessionID},
		"ts":      time.Now().UTC().Format(time.RFC3339),
		"ttl_sec": 60,
	})

	return raw
}

// drainUplinks partitions the buffered uplinks into pairing events and acks.
func drainUplinks(t *testing.T, a *Agent) (events []contract.PairingEventPayload, acks []contract.CommandAckPayload) {
	t.Helper()

	for _, p := range a.buffer.drain() {
		switch p.topic {
		case contract.EventTopic("gw-test", contract.PairingEventType):
			var ev contract.PairingEventPayload
			if err := json.Unmarshal(p.payload, &ev); err != nil {
				t.Fatal(err)
			}
			events = append(events, ev)
		case contract.CommandAckTopic("gw-test"):
			var ack contract.CommandAckPayload
			if err := json.Unmarshal(p.payload, &ack); err != nil {
				t.Fatal(err)
			}
			acks = append(acks, ack)
		}
	}

	return events, acks
}

func TestPairingCommandRoutesToManager(t *testing.T) {
	a := newPairingTestAgent(t, true)

	a.handleCommand("", pairingStartCmd("c1", "s1"))

	events, acks := drainUplinks(t, a)
	if len(events) != 1 || events[0].SessionID != "s1" || events[0].Phase != contract.PairingPhaseStarted {
		t.Fatalf("expected started event for s1, got %+v", events)
	}
	if len(acks) != 1 || acks[0].CmdID != "c1" || acks[0].Status != contract.AckAcked {
		t.Fatalf("expected acked c1, got %+v", acks)
	}
}

func TestPairingStartWhileBusyAcksFailed(t *testing.T) {
	a := newPairingTestAgent(t, true)

	a.handleCommand("", pairingStartCmd("c1", "s1"))
	a.handleCommand("", pairingStartCmd("c2", "s2"))

	events, acks := drainUplinks(t, a)

	var s2Failed bool
	for _, ev := range events {
		if ev.SessionID == "s2" && ev.Phase == contract.PairingPhaseFailed {
			s2Failed = true
		}
	}
	if !s2Failed {
		t.Fatalf("expected failed event for s2, got %+v", events)
	}

	var c2Status contract.CommandAckStatus
	for _, ack := range acks {
		if ack.CmdID == "c2" {
			c2Status = ack.Status
		}
	}
	if c2Status != contract.AckFailed {
		t.Fatalf("expected c2 ack failed, got %q", c2Status)
	}
}

func TestPairingUnavailableAcksFailed(t *testing.T) {
	a := newPairingTestAgent(t, false)

	a.handleCommand("", pairingStartCmd("c1", "s1"))

	events, acks := drainUplinks(t, a)
	if len(events) != 1 || events[0].SessionID != "s1" || events[0].Phase != contract.PairingPhaseFailed {
		t.Fatalf("expected failed event for s1, got %+v", events)
	}
	if len(acks) != 1 || acks[0].Status != contract.AckFailed {
		t.Fatalf("expected failed ack, got %+v", acks)
	}
}

func TestPairingStopAcksAcked(t *testing.T) {
	a := newPairingTestAgent(t, true)

	a.handleCommand("", pairingStartCmd("c1", "s1"))

	stop, _ := json.Marshal(map[string]any{
		"cmd_id":  "c2",
		"action":  "pairing.stop",
		"params":  map[string]any{"protocol": "zigbee", "session_id": "s1"},
		"ts":      time.Now().UTC().Format(time.RFC3339),
		"ttl_sec": 60,
	})
	a.handleCommand("", stop)

	events, acks := drainUplinks(t, a)

	var stopped bool
	for _, ev := range events {
		if ev.SessionID == "s1" && ev.Phase == contract.PairingPhaseStopped && ev.Reason == "user" {
			stopped = true
		}
	}
	if !stopped {
		t.Fatalf("expected stopped(user) event, got %+v", events)
	}
	if len(acks) != 2 || acks[1].Status != contract.AckAcked {
		t.Fatalf("expected second ack acked, got %+v", acks)
	}
}
