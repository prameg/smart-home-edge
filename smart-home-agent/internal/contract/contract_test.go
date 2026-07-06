package contract

import (
	"encoding/json"
	"testing"
)

func TestTopics(t *testing.T) {
	uid := "gw-uid-123"

	cases := map[string]string{
		StateTopic(uid, "dev-1"):         "homes/gw-uid-123/state/dev-1",
		EventTopic(uid, "alarm"):         "homes/gw-uid-123/event/alarm",
		CommandAckTopic(uid):             "homes/gw-uid-123/cmd/ack",
		AvailabilityTopic(uid):           "homes/gw-uid-123/availability",
		CommandTopic(uid):                "homes/gw-uid-123/cmd",
		DesiredShadowTopic(uid, "dev-1"): "homes/gw-uid-123/shadow/desired/dev-1",
		DesiredShadowFilter(uid):         "homes/gw-uid-123/shadow/desired/#",
	}

	for got, want := range cases {
		if got != want {
			t.Errorf("topic mismatch: got %q want %q", got, want)
		}
	}
}

// TestStatePayloadOmitsVersionWhenNil verifies the "still converged" case
// encodes without a version key (the cloud reads absent version as telemetry).
func TestStatePayloadOmitsVersionWhenNil(t *testing.T) {
	raw, err := json.Marshal(StatePayload{State: map[string]any{"on": true}, TS: "2026-07-04T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}

	if string(raw) == "" || contains(raw, "version") {
		t.Errorf("expected no version key, got %s", raw)
	}
}

// TestStatePayloadIncludesVersionWhenSet verifies an applied version is emitted.
func TestStatePayloadIncludesVersionWhenSet(t *testing.T) {
	v := 7
	raw, err := json.Marshal(StatePayload{State: map[string]any{}, Version: &v, TS: "2026-07-04T00:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}

	if !contains(raw, "\"version\":7") {
		t.Errorf("expected version:7, got %s", raw)
	}
}

func contains(haystack []byte, needle string) bool {
	return len(needle) > 0 && indexOf(string(haystack), needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}

	return -1
}
