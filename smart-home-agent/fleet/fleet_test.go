package fleet

import (
	"os"
	"path/filepath"
	"testing"
)

// The embedded default manifest must parse and satisfy the invariants the CLI
// relies on — a broken release.json should fail the build's tests, not a unit
// in the field.
func TestDefaultManifestValid(t *testing.T) {
	m, err := Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}

	if m.ReleaseID == "" {
		t.Error("embedded manifest has no release_id")
	}

	if m.Agent() == nil {
		t.Fatal("embedded manifest has no agent add-on")
	}

	if m.Agent().Repository == "" {
		t.Error("agent add-on should carry the add-on repository to install from")
	}
}

// Load with an empty path returns the embedded default; Load with a path parses
// that file instead.
func TestLoadOverride(t *testing.T) {
	def, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if def.ReleaseID == "" {
		t.Error("empty path should return the embedded default")
	}

	path := filepath.Join(t.TempDir(), "candidate.json")
	if err := os.WriteFile(path, []byte(`{
		"release_id": "test-r9",
		"populated": true,
		"addon_repository": "https://example.test/repo",
		"addons": [{"name": "Agent", "match": "smart_home_agent", "version": "9.9.9"}]
	}`), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}

	m, err := Load(path)
	if err != nil {
		t.Fatalf("Load(path): %v", err)
	}

	if m.ReleaseID != "test-r9" || !m.Populated {
		t.Errorf("override not loaded: %+v", m)
	}

	if !m.Agent().Pinned() || m.Agent().Version != "9.9.9" {
		t.Errorf("agent version not parsed: %+v", m.Agent())
	}
}

// Validate rejects manifests missing the pieces onboarding cannot proceed
// without.
func TestValidateRejectsIncomplete(t *testing.T) {
	cases := map[string]Manifest{
		"no release id": {
			AddonRepository: "https://x.test",
			Addons:          []Addon{{Match: "smart_home_agent"}},
		},
		"no repository": {
			ReleaseID: "r1",
			Addons:    []Addon{{Match: "smart_home_agent"}},
		},
		"no agent add-on": {
			ReleaseID:       "r1",
			AddonRepository: "https://x.test",
			Addons:          []Addon{{Match: "mosquitto", Slug: "core_mosquitto"}},
		},
		"addon without match or slug": {
			ReleaseID:       "r1",
			AddonRepository: "https://x.test",
			Addons:          []Addon{{Name: "broken"}, {Match: "smart_home_agent"}},
		},
	}

	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			if err := m.Validate(); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

// Resolves handles the three slug shapes the CLI meets: an exact configured
// slug, a bare match, and the repo-hash-prefixed community-add-on tail.
func TestAddonResolves(t *testing.T) {
	core := Addon{Match: "mosquitto", Slug: "core_mosquitto"}
	if !core.Resolves("core_mosquitto") {
		t.Error("exact slug should resolve")
	}
	if core.Resolves("mosquitto") {
		t.Error("configured exact slug must not resolve a bare match")
	}

	agent := Addon{Match: "smart_home_agent"}
	if !agent.Resolves("smart_home_agent") {
		t.Error("bare match should resolve (local add-on)")
	}
	if !agent.Resolves("a1b2c3d4_smart_home_agent") {
		t.Error("repo-hash-prefixed slug should resolve by suffix")
	}
	if agent.Resolves("smart_home_agent_extra") {
		t.Error("must not resolve an unrelated slug that merely contains the match")
	}
}
