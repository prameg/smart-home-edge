package agent

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
)

func newTestAgent(t *testing.T) *Agent {
	t.Helper()

	dir := t.TempDir()

	return &Agent{
		cfg:        &config.Config{DataDir: dir},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		configPath: configStatePath(dir),
	}
}

func TestHandleConfigAppliesClaimAndMap(t *testing.T) {
	a := newTestAgent(t)

	raw, _ := json.Marshal(contract.ConfigPayload{
		Claimed:       true,
		ConfigVersion: 3,
		EntityMap:     []contract.ConfigEntityMapEntry{{DeviceUID: "d1", EntityID: "light.k"}},
	})

	a.handleConfig("", raw)

	if !a.isClaimed() {
		t.Fatal("expected claimed after config")
	}

	if got := a.entityForDevice("d1"); got != "light.k" {
		t.Errorf("entityForDevice(d1) = %q, want light.k", got)
	}

	if got := a.deviceForEntity("light.k"); got != "d1" {
		t.Errorf("deviceForEntity(light.k) = %q, want d1", got)
	}
}

func TestHandleConfigIgnoresStaleVersion(t *testing.T) {
	a := newTestAgent(t)

	current, _ := json.Marshal(contract.ConfigPayload{Claimed: true, ConfigVersion: 5})
	a.handleConfig("", current)

	stale, _ := json.Marshal(contract.ConfigPayload{Claimed: false, ConfigVersion: 4})
	a.handleConfig("", stale)

	if !a.isClaimed() {
		t.Error("expected stale (lower config_version) doc to be ignored")
	}
}

func TestConfigStatePersistsAcrossRestart(t *testing.T) {
	a := newTestAgent(t)

	raw, _ := json.Marshal(contract.ConfigPayload{
		Claimed:       true,
		ConfigVersion: 2,
		EntityMap:     []contract.ConfigEntityMapEntry{{DeviceUID: "d9", EntityID: "switch.x"}},
	})
	a.handleConfig("", raw)

	// Simulate a restart: a fresh agent reading the same data dir.
	restarted := &Agent{
		cfg:        a.cfg,
		log:        a.log,
		configPath: a.configPath,
	}
	restarted.restoreConfigState()

	if !restarted.isClaimed() {
		t.Fatal("expected restored agent to boot claimed")
	}

	if got := restarted.entityForDevice("d9"); got != "switch.x" {
		t.Errorf("restored entityForDevice(d9) = %q, want switch.x", got)
	}
}
