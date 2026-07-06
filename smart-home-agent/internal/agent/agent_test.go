package agent

import (
	"testing"
	"time"

	"github.com/smart-home/edge/agent/internal/contract"
)

func TestSplitAction(t *testing.T) {
	cases := []struct {
		action  string
		domain  string
		service string
		ok      bool
	}{
		{"light.turn_on", "light", "turn_on", true},
		{"climate.set_temperature", "climate", "set_temperature", true},
		{"noservice", "", "", false},
		{".leading", "", "", false},
		{"trailing.", "", "", false},
		{"", "", "", false},
	}

	for _, c := range cases {
		domain, service, ok := splitAction(c.action)
		if ok != c.ok || domain != c.domain || service != c.service {
			t.Errorf("splitAction(%q) = (%q,%q,%v), want (%q,%q,%v)", c.action, domain, service, ok, c.domain, c.service, c.ok)
		}
	}
}

func TestCommandExpired(t *testing.T) {
	a := &Agent{}

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	now := time.Now().UTC().Format(time.RFC3339)

	if !a.commandExpired(contract.CommandPayload{TS: past, TTLSec: 30}) {
		t.Error("expected command issued an hour ago with 30s TTL to be expired")
	}

	if a.commandExpired(contract.CommandPayload{TS: now, TTLSec: 300}) {
		t.Error("expected fresh command within TTL to be live")
	}

	if a.commandExpired(contract.CommandPayload{TS: now, TTLSec: 0}) {
		t.Error("expected TTL<=0 to never expire")
	}

	if a.commandExpired(contract.CommandPayload{TS: "not-a-time", TTLSec: 30}) {
		t.Error("expected unparseable ts to be applied, not dropped")
	}
}

func onlyCall(t *testing.T, calls []serviceCall) serviceCall {
	t.Helper()

	if len(calls) != 1 {
		t.Fatalf("expected exactly one service call, got %d: %+v", len(calls), calls)
	}

	return calls[0]
}

func TestTranslateDesired(t *testing.T) {
	// HA-native desired: `state` string drives the primary target, `attributes`
	// pass through as service params.
	light := onlyCall(t, translateDesired("light.kitchen", map[string]any{
		"state":      "on",
		"attributes": map[string]any{"brightness": float64(128)},
	}))
	if light.domain != "light" || light.service != "turn_on" {
		t.Fatalf("unexpected light translation: %+v", light)
	}

	if light.data["brightness"] != float64(128) {
		t.Errorf("expected brightness passthrough, got %v", light.data["brightness"])
	}

	off := onlyCall(t, translateDesired("switch.kettle", map[string]any{"state": "off"}))
	if off.domain != "switch" || off.service != "turn_off" {
		t.Errorf("unexpected off translation: %+v", off)
	}

	// An input_boolean (not a first-class domain) falls back to homeassistant.
	generic := onlyCall(t, translateDesired("input_boolean.bed_lights", map[string]any{"state": "on"}))
	if generic.domain != "homeassistant" || generic.service != "turn_on" {
		t.Errorf("unexpected generic translation: %+v", generic)
	}

	if calls := translateDesired("light.kitchen", map[string]any{"attributes": map[string]any{}}); len(calls) != 0 {
		t.Errorf("expected no translation for a document without a state string, got %+v", calls)
	}
}

func TestTranslateDesiredRichDomains(t *testing.T) {
	// lock: state maps to the lock/unlock services.
	lock := onlyCall(t, translateDesired("lock.front", map[string]any{"state": "locked"}))
	if lock.domain != "lock" || lock.service != "lock" {
		t.Errorf("unexpected lock translation: %+v", lock)
	}

	// cover: an explicit position wins over open/closed.
	cover := onlyCall(t, translateDesired("cover.garage", map[string]any{
		"state":      "open",
		"attributes": map[string]any{"position": float64(40)},
	}))
	if cover.service != "set_cover_position" || cover.data["position"] != float64(40) {
		t.Errorf("unexpected cover translation: %+v", cover)
	}

	// climate: a mode + a target temperature expand to two ordered calls.
	climate := translateDesired("climate.living", map[string]any{
		"state":      "heat",
		"attributes": map[string]any{"temperature": float64(21)},
	})
	if len(climate) != 2 {
		t.Fatalf("expected two climate calls, got %+v", climate)
	}
	if climate[0].service != "set_hvac_mode" || climate[0].data["hvac_mode"] != "heat" {
		t.Errorf("unexpected climate mode call: %+v", climate[0])
	}
	if climate[1].service != "set_temperature" || climate[1].data["temperature"] != float64(21) {
		t.Errorf("unexpected climate temperature call: %+v", climate[1])
	}

	// fan: a bare percentage change with no state uses set_percentage.
	fan := onlyCall(t, translateDesired("fan.office", map[string]any{
		"attributes": map[string]any{"percentage": float64(66)},
	}))
	if fan.service != "set_percentage" || fan.data["percentage"] != float64(66) {
		t.Errorf("unexpected fan translation: %+v", fan)
	}

	// media_player: state play + a volume level expand to two calls.
	media := translateDesired("media_player.tv", map[string]any{
		"state":      "playing",
		"attributes": map[string]any{"volume_level": float64(0.3)},
	})
	if len(media) != 2 || media[0].service != "media_play" || media[1].service != "volume_set" {
		t.Errorf("unexpected media_player translation: %+v", media)
	}
}

func TestUplinkBufferDropOldest(t *testing.T) {
	b := newUplinkBuffer(2)
	b.push(pending{topic: "a"})
	b.push(pending{topic: "b"})
	b.push(pending{topic: "c"}) // should drop "a"

	drained := b.drain()
	if len(drained) != 2 || drained[0].topic != "b" || drained[1].topic != "c" {
		t.Errorf("expected [b c], got %+v", drained)
	}

	if len(b.drain()) != 0 {
		t.Error("expected buffer empty after drain")
	}
}

func TestCmdDedupe(t *testing.T) {
	d := newCmdDedupe(2)

	if !d.firstSee("x") || d.firstSee("x") {
		t.Error("expected x seen once")
	}

	d.firstSee("y")
	d.firstSee("z") // evicts x

	if !d.firstSee("x") {
		t.Error("expected x re-admitted after eviction")
	}
}
