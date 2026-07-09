package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/supervisor"
)

// fakeSupervisor is a canned Supervisor transport: it answers version reads from
// its current in-memory versions and applies update posts to them, recording
// every call so a test can assert the convergence ORDER.
type fakeSupervisor struct {
	mu          sync.Mutex
	selfVersion string
	osVersion   string
	coreVersion string
	calls       []string
}

func (f *fakeSupervisor) Call(_ context.Context, method, endpoint string, payload any, _ time.Duration) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method+" "+endpoint)

	version := func(p any) string {
		m, _ := p.(map[string]string)

		return m["version"]
	}

	switch {
	case method == "get" && endpoint == "/addons/self/info":
		return json.Marshal(map[string]any{"version": f.selfVersion, "state": "started"})
	case method == "get" && endpoint == "/os/info":
		return json.Marshal(map[string]any{"version": f.osVersion, "version_latest": f.osVersion})
	case method == "get" && endpoint == "/core/info":
		return json.Marshal(map[string]any{"version": f.coreVersion, "version_latest": f.coreVersion})
	case method == "post" && (endpoint == "/store/addons/self/update" || endpoint == "/addons/self/update"):
		f.selfVersion = version(payload)
	case method == "post" && endpoint == "/os/update":
		f.osVersion = version(payload)
	case method == "post" && endpoint == "/core/update":
		f.coreVersion = version(payload)
	}

	return json.RawMessage("null"), nil
}

func (f *fakeSupervisor) called(target string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, c := range f.calls {
		if c == target {
			return true
		}
	}

	return false
}

func (f *fakeSupervisor) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = nil
}

func newReleaseTestAgent(t *testing.T, fake *fakeSupervisor) *Agent {
	t.Helper()

	return &Agent{
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:     &config.Config{AgentVersion: fake.selfVersion},
		release: newReleaseStore(t.TempDir()),
		sup:     supervisor.New(fake, time.Second, time.Second),
	}
}

func TestConvergeReleaseSelfUpdateLastAndResumeSafe(t *testing.T) {
	fake := &fakeSupervisor{selfVersion: "0.1.0"}
	a := newReleaseTestAgent(t, fake)

	rel := contract.ReleasePayload{ReleaseID: "2026.07-r2", ReleaseSeq: 2, AgentVersion: "0.2.0"}

	// First pass: agent is behind, so it triggers its own update and stops
	// WITHOUT marking the release applied (the restart re-drives convergence).
	a.convergeRelease(rel)
	if !fake.called("post /store/addons/self/update") {
		t.Fatalf("expected a self-update call, got %v", fake.calls)
	}
	if got := a.release.current(); got != "" {
		t.Fatalf("release must not be marked applied before the self-update restart, got %q", got)
	}

	// Simulate the post-update restart: the new binary reports the target
	// version, so convergence completes and the release is marked applied.
	fake.reset()
	a.convergeRelease(rel)
	if fake.called("post /store/addons/self/update") {
		t.Fatalf("must not self-update again once at target, got %v", fake.calls)
	}
	if got := a.release.current(); got != "2026.07-r2" {
		t.Fatalf("expected converged release id, got %q", got)
	}
}

func TestConvergeReleaseOrdersDepsOSCoreThenSelf(t *testing.T) {
	fake := &fakeSupervisor{selfVersion: "0.1.0", osVersion: "13.0", coreVersion: "2026.6.0"}
	a := newReleaseTestAgent(t, fake)

	rel := contract.ReleasePayload{
		ReleaseID:    "2026.07-r3",
		ReleaseSeq:   3,
		AgentVersion: "0.2.0",
		HAOSVersion:  "13.1",
		CoreVersion:  "2026.7.0",
	}

	// OS is first and reboots the unit, so the run stops after triggering it —
	// Core and the agent must NOT be touched yet.
	a.convergeRelease(rel)
	if !fake.called("post /os/update") {
		t.Fatalf("expected OS update first, got %v", fake.calls)
	}
	if fake.called("post /core/update") || fake.called("post /store/addons/self/update") {
		t.Fatalf("core/self must wait until after the OS reboot, got %v", fake.calls)
	}

	// Post-reboot: OS matches, so Core converges next and stops.
	fake.reset()
	a.convergeRelease(rel)
	if !fake.called("post /core/update") {
		t.Fatalf("expected Core update after OS, got %v", fake.calls)
	}
	if fake.called("post /store/addons/self/update") {
		t.Fatalf("self must wait until after Core, got %v", fake.calls)
	}

	// Post-restart: OS+Core match, so the agent self-updates last.
	fake.reset()
	a.convergeRelease(rel)
	if !fake.called("post /store/addons/self/update") {
		t.Fatalf("expected self-update last, got %v", fake.calls)
	}
	if got := a.release.current(); got != "" {
		t.Fatalf("release must not be applied until the agent restart, got %q", got)
	}

	// Final restart at target: fully converged.
	fake.reset()
	a.convergeRelease(rel)
	if got := a.release.current(); got != "2026.07-r3" {
		t.Fatalf("expected converged release id, got %q", got)
	}
}

func TestReleaseStoreIgnoresStaleSeq(t *testing.T) {
	s := newReleaseStore(t.TempDir())
	s.markApplied("2026.07-r5", 5)

	if !s.alreadyApplied(5) {
		t.Fatal("seq 5 should count as applied")
	}
	if !s.alreadyApplied(4) {
		t.Fatal("a lower seq must be treated as already applied (stale)")
	}
	if s.alreadyApplied(6) {
		t.Fatal("a newer seq must NOT be treated as applied")
	}

	// A stale markApplied must not roll the converged release backwards.
	s.markApplied("old", 3)
	if got := s.current(); got != "2026.07-r5" {
		t.Fatalf("stale markApplied rolled the release back to %q", got)
	}
}
