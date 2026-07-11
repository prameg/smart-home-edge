package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/provision"
	"github.com/smart-home/edge/agent/internal/supervisor"
)

// errFakeTransport is an injected transport-level failure (distinct from a
// Supervisor-side *supervisor.Error) so a test can force a hard update failure.
var errFakeTransport = errors.New("fake transport failure")

// fakeAddon is one add-on's install state in the fake Supervisor.
type fakeAddon struct {
	version         string
	autoUpdate      bool
	updateAvailable bool
}

// fakeSupervisor is a canned Supervisor transport that models add-on / OS / Core
// install state in memory, applies update posts to it, and records every call so
// a test can assert the update ORDER (add-ons -> OS -> Core -> self).
type fakeSupervisor struct {
	mu          sync.Mutex
	addons      map[string]*fakeAddon
	storeSlugs  []string
	osVersion   string
	osLatest    string
	coreVersion string
	coreLatest  string
	failOSInfo  bool
	calls       []string
}

func (f *fakeSupervisor) Call(_ context.Context, method, endpoint string, payload any, _ time.Duration) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method+" "+endpoint)

	if f.failOSInfo && method == "get" && endpoint == "/os/info" {
		return nil, errFakeTransport
	}

	switch {
	case method == "get" && endpoint == "/store":
		addons := make([]map[string]string, 0, len(f.storeSlugs))
		for _, s := range f.storeSlugs {
			addons = append(addons, map[string]string{"slug": s})
		}

		return json.Marshal(map[string]any{"addons": addons})

	case method == "get" && endpoint == "/os/info":
		return json.Marshal(map[string]any{"version": f.osVersion, "version_latest": f.osLatest})

	case method == "get" && endpoint == "/core/info":
		return json.Marshal(map[string]any{"version": f.coreVersion, "version_latest": f.coreLatest})

	case method == "post" && endpoint == "/os/update":
		f.osVersion = f.osLatest

	case method == "post" && endpoint == "/core/update":
		f.coreVersion = f.coreLatest

	case method == "get" && strings.HasPrefix(endpoint, "/addons/") && strings.HasSuffix(endpoint, "/info"):
		slug := strings.TrimSuffix(strings.TrimPrefix(endpoint, "/addons/"), "/info")
		if a := f.addons[slug]; a != nil {
			return json.Marshal(map[string]any{
				"version":          a.version,
				"state":            "started",
				"auto_update":      a.autoUpdate,
				"update_available": a.updateAvailable,
			})
		}

		return json.Marshal(map[string]any{"version": ""})

	case method == "post" && strings.HasSuffix(endpoint, "/options"):
		slug := f.slugFromOptions(endpoint)
		if a := f.addons[slug]; a != nil {
			if m, ok := payload.(map[string]any); ok {
				if au, ok := m["auto_update"].(bool); ok {
					a.autoUpdate = au
				}
			}
		}

	case method == "post" && strings.HasSuffix(endpoint, "/update"):
		if a := f.addons[f.slugFromUpdate(endpoint)]; a != nil {
			a.updateAvailable = false
			a.version = "latest"
		}
	}

	return json.RawMessage("null"), nil
}

// slugFromOptions extracts the slug from "/addons/{slug}/options".
func (f *fakeSupervisor) slugFromOptions(endpoint string) string {
	return strings.TrimSuffix(strings.TrimPrefix(endpoint, "/addons/"), "/options")
}

// slugFromUpdate extracts the slug from either the "/store/addons/{slug}/update"
// or the "/addons/{slug}/update" mutation form.
func (f *fakeSupervisor) slugFromUpdate(endpoint string) string {
	endpoint = strings.TrimSuffix(endpoint, "/update")
	endpoint = strings.TrimPrefix(endpoint, "/store")

	return strings.TrimPrefix(endpoint, "/addons/")
}

func (f *fakeSupervisor) called(target string) bool {
	return f.callIndex(target) >= 0
}

func (f *fakeSupervisor) callIndex(target string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	for i, c := range f.calls {
		if c == target {
			return i
		}
	}

	return -1
}

func (f *fakeSupervisor) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = nil
}

// newUpdateTestFake returns a fake with the bootstrap add-on set installed and a
// resolvable community slug for Zigbee2MQTT (the manifest's only non-core dep).
func newUpdateTestFake() *fakeSupervisor {
	return &fakeSupervisor{
		addons: map[string]*fakeAddon{
			"core_mosquitto":     {version: "1.0"},
			"core_matter_server": {version: "1.0"},
			"abc123_zigbee2mqtt": {version: "1.0"},
			"self":               {version: "0.1.0"},
		},
		storeSlugs:  []string{"abc123_zigbee2mqtt"},
		osVersion:   "13.0",
		osLatest:    "13.0",
		coreVersion: "2026.6.0",
		coreLatest:  "2026.6.0",
	}
}

func newUpdateTestAgent(t *testing.T, fake *fakeSupervisor) *Agent {
	t.Helper()

	return &Agent{
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:         &config.Config{AgentVersion: "0.1.0"},
		creds:       &provision.Credentials{UID: "gw1"},
		updateState: newUpdateStore(t.TempDir()),
		buffer:      newUplinkBuffer(uplinkBufferLimit),
		sup:         supervisor.New(fake, time.Second, time.Second),
	}
}

// updateAll converges everything to the latest, ordered add-ons -> OS -> Core ->
// self, stopping after each reboot-prone phase and resuming on the next pass.
func TestUpdateAllOrdersAddonsOSCoreThenSelfResumeSafe(t *testing.T) {
	fake := newUpdateTestFake()
	fake.addons["abc123_zigbee2mqtt"].updateAvailable = true
	fake.osLatest = "13.1"
	fake.coreLatest = "2026.7.0"
	fake.addons["self"].updateAvailable = true

	a := newUpdateTestAgent(t, fake)

	// Pass 1: add-ons update to latest, then OS is triggered and the pass stops
	// (OS reboots) — Core and self must not be touched yet.
	a.updateAll("u1")
	addonIdx := fake.callIndex("post /store/addons/abc123_zigbee2mqtt/update")
	osIdx := fake.callIndex("post /os/update")
	if addonIdx < 0 || osIdx < 0 {
		t.Fatalf("expected an add-on update then an OS update, got %v", fake.calls)
	}
	if addonIdx > osIdx {
		t.Fatalf("add-ons must update before OS, got %v", fake.calls)
	}
	if fake.called("post /core/update") || fake.called("post /store/addons/self/update") {
		t.Fatalf("core/self must wait until after the OS reboot, got %v", fake.calls)
	}
	if _, ok := a.updateState.inProgress(); !ok {
		t.Fatal("marker must stay in-progress across the OS reboot")
	}

	// Pass 2 (post-reboot: OS current): Core is triggered and the pass stops.
	fake.reset()
	a.updateAll("u1")
	if !fake.called("post /core/update") {
		t.Fatalf("expected Core update after OS, got %v", fake.calls)
	}
	if fake.called("post /store/addons/self/update") {
		t.Fatalf("self must wait until after Core, got %v", fake.calls)
	}

	// Pass 3 (post-restart: Core current): the agent self-updates last.
	fake.reset()
	a.updateAll("u1")
	if !fake.called("post /store/addons/self/update") {
		t.Fatalf("expected self-update last, got %v", fake.calls)
	}
	if _, ok := a.updateState.inProgress(); !ok {
		t.Fatal("marker must stay in-progress across the self-update restart")
	}

	// Pass 4 (post-restart: everything current): converged, marker cleared, ok.
	fake.reset()
	a.updateAll("u1")
	if _, ok := a.updateState.inProgress(); ok {
		t.Fatal("marker must be cleared once fully converged")
	}
	if got := lastUpdateStatus(t, a); got != contract.UpdateOK {
		t.Fatalf("expected a terminal ok report, got %q", got)
	}
}

// updateAll turns HA's per-add-on auto-update ON (so Supervisor keeps managed
// add-ons fresh between the agent's self-checks) — the inverse of the old
// pin-and-disable behavior.
func TestUpdateAllEnablesAutoUpdate(t *testing.T) {
	fake := newUpdateTestFake()
	a := newUpdateTestAgent(t, fake)

	a.updateAll("")

	for slug, addon := range fake.addons {
		if !addon.autoUpdate {
			t.Fatalf("auto-update should be enabled on %q after an update pass", slug)
		}
	}
}

// A command-triggered update reports `started`; the agent's own daily self-check
// (empty update_id) stays silent until the terminal `ok`, so it never flaps the
// fleet badge.
func TestUpdateAllStartedReportOnlyForCommand(t *testing.T) {
	fake := newUpdateTestFake()

	a := newUpdateTestAgent(t, fake)
	a.updateAll("cmd-1")
	if !hasStatus(t, a, contract.UpdateStarted) {
		t.Fatal("a command-triggered update must report started")
	}

	fake2 := newUpdateTestFake()
	b := newUpdateTestAgent(t, fake2)
	b.updateAll("")
	if hasStatus(t, b, contract.UpdateStarted) {
		t.Fatal("a silent self-check must not report started")
	}
	if got := lastUpdateStatus(t, b); got != contract.UpdateOK {
		t.Fatalf("a self-check that converges must report ok, got %q", got)
	}
}

// A hard failure is reported as `failed` (with the error) and clears the resume
// marker, so recovery is a re-sent command or the next self-check — not a
// boot-resume loop.
func TestUpdateAllReportsFailureAndClearsMarker(t *testing.T) {
	fake := newUpdateTestFake()
	// A transport-level failure reading OS info is a hard failure the pass
	// cannot skip past.
	fake.failOSInfo = true

	a := newUpdateTestAgent(t, fake)
	a.updateAll("cmd-err")

	if got := lastUpdateStatus(t, a); got != contract.UpdateFailed {
		t.Fatalf("expected a failed report, got %q", got)
	}
	if _, ok := a.updateState.inProgress(); ok {
		t.Fatal("a hard failure must clear the marker (retry via command/self-check)")
	}
}

func TestUpdateStoreResumeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newUpdateStore(dir)

	if _, ok := s.inProgress(); ok {
		t.Fatal("a fresh store must not be in-progress")
	}

	s.begin("u9")
	// A new store reading the same dir must see the persisted marker (survives a
	// process restart / reboot).
	if id, ok := newUpdateStore(dir).inProgress(); !ok || id != "u9" {
		t.Fatalf("expected persisted in-progress u9, got %q ok=%v", id, ok)
	}

	s.clear()
	if _, ok := newUpdateStore(dir).inProgress(); ok {
		t.Fatal("cleared marker must not persist as in-progress")
	}
}

// lastUpdateStatus drains the agent's uplink buffer and returns the status of
// the last update/status report (empty when none was published).
func lastUpdateStatus(t *testing.T, a *Agent) contract.UpdateStatus {
	t.Helper()

	var last contract.UpdateStatus
	for _, p := range a.buffer.drain() {
		if !strings.HasSuffix(p.topic, "/update/status") {
			continue
		}

		var payload contract.UpdateStatusPayload
		if err := json.Unmarshal(p.payload, &payload); err != nil {
			t.Fatalf("decode status payload: %v", err)
		}
		last = payload.Status
	}

	return last
}

// hasStatus reports whether any update/status report with the given status was
// buffered. It re-buffers what it drains so a later assertion still sees it.
func hasStatus(t *testing.T, a *Agent, want contract.UpdateStatus) bool {
	t.Helper()

	found := false
	for _, p := range a.buffer.drain() {
		if strings.HasSuffix(p.topic, "/update/status") {
			var payload contract.UpdateStatusPayload
			if err := json.Unmarshal(p.payload, &payload); err != nil {
				t.Fatalf("decode status payload: %v", err)
			}
			if payload.Status == want {
				found = true
			}
		}
		a.buffer.push(p)
	}

	return found
}
