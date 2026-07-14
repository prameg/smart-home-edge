package pairing

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMapBridgeEvent(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantPhase string
		wantOK    bool
	}{
		{
			name:      "device joined",
			raw:       `{"type":"device_joined","data":{"friendly_name":"0x00124b0022334455","ieee_address":"0x00124b0022334455"}}`,
			wantPhase: "device_found",
			wantOK:    true,
		},
		{
			name:      "interview started",
			raw:       `{"type":"device_interview","data":{"friendly_name":"0xabc","ieee_address":"0xabc","status":"started"}}`,
			wantPhase: "interviewing",
			wantOK:    true,
		},
		{
			name:      "interview successful",
			raw:       `{"type":"device_interview","data":{"friendly_name":"0xabc","ieee_address":"0xabc","status":"successful","supported":true,"definition":{"model":"ZBMINIL2","vendor":"SONOFF"}}}`,
			wantPhase: "completed",
			wantOK:    true,
		},
		{
			name:      "interview failed",
			raw:       `{"type":"device_interview","data":{"friendly_name":"0xabc","ieee_address":"0xabc","status":"failed"}}`,
			wantPhase: "failed",
			wantOK:    true,
		},
		{
			name:   "irrelevant event",
			raw:    `{"type":"device_leave","data":{"ieee_address":"0xabc"}}`,
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok := MapBridgeEvent([]byte(tc.raw))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && ev.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", ev.Phase, tc.wantPhase)
			}
		})
	}

	ev, _ := MapBridgeEvent([]byte(`{"type":"device_interview","data":{"friendly_name":"0xabc","ieee_address":"0xabc","status":"successful","definition":{"model":"ZBMINIL2","vendor":"SONOFF"}}}`))
	if ev.Device == nil || ev.Device.Model != "ZBMINIL2" || ev.Device.Vendor != "SONOFF" {
		t.Fatalf("device detail not mapped: %+v", ev.Device)
	}
}

type fakeBackend struct{ started, stopped int }

func (f *fakeBackend) Start(seconds int) error { f.started++; return nil }
func (f *fakeBackend) Stop() error             { f.stopped++; return nil }

func TestManagerSingleSession(t *testing.T) {
	backend := &fakeBackend{}
	var events []string
	m := NewManager(backend, func(sessionID string, ev Event) {
		events = append(events, sessionID+":"+ev.Phase)
	}, testLogger())

	if err := m.Start("s1", 120); err != nil {
		t.Fatal(err)
	}
	if err := m.Start("s2", 120); err != ErrBusy {
		t.Fatalf("expected ErrBusy, got %v", err)
	}
	if backend.started != 1 {
		t.Fatalf("backend started %d times", backend.started)
	}
	if len(events) != 1 || events[0] != "s1:started" {
		t.Fatalf("unexpected events %v", events)
	}
}

func TestManagerCompletionFreesSlotAndStopsBackend(t *testing.T) {
	backend := &fakeBackend{}
	var events []string
	m := NewManager(backend, func(sessionID string, ev Event) {
		events = append(events, sessionID+":"+ev.Phase)
	}, testLogger())

	_ = m.Start("s1", 120)
	m.HandleBackendEvent(Event{Phase: "completed"})

	if backend.stopped != 1 {
		t.Fatalf("backend not stopped on completion")
	}
	if err := m.Start("s2", 120); err != nil {
		t.Fatalf("slot not freed: %v", err)
	}
	_ = events
}

func TestManagerTimeout(t *testing.T) {
	backend := &fakeBackend{}
	done := make(chan Event, 4)
	m := NewManager(backend, func(_ string, ev Event) { done <- ev }, testLogger())

	m.timeoutFor = func(int) time.Duration { return 10 * time.Millisecond }
	_ = m.Start("s1", 120)
	<-done // started

	select {
	case ev := <-done:
		if ev.Phase != "stopped" || ev.Reason != "timeout" {
			t.Fatalf("unexpected terminal event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout event never emitted")
	}
}
