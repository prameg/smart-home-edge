// Package fleet is the static bootstrap add-on set both halves of the fleet
// share: the repository to register and the add-ons (repo + slugs, NO versions)
// to install so a fresh unit can start the agent and provision. It is
// deliberately version-free — everything runs the LATEST available: the CLI
// installs latest at onboarding, and the agent keeps the unit fresh afterwards
// (auto_update on the add-ons + its own updateAll on command / daily check).
//
// A default bootstrap set is embedded at build time (release.json in this
// package), so the shipped binary is self-contained; a caller can override it
// with an on-disk file (smart-onboard --manifest ...) for bringing up a unit
// against a different add-on repository.
//
// The agent reads this same set at runtime to know WHICH add-ons it manages
// (updateAll installs/updates them to latest); there are no pinned versions
// anywhere.
package fleet

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// release.json lives beside this file so it is embeddable (go:embed cannot reach
// outside the package directory) and versioned together with the loader.
//
//go:embed release.json
var embedded []byte

// Manifest is the managed add-on set: which add-on store repository to register
// and which add-ons to install (latest available, no pinning). It carries NO
// versions on purpose — everything runs the latest, kept fresh by the agent's
// updateAll and HA's per-add-on auto_update.
type Manifest struct {
	// AddonRepository is the add-on store repo (this edge repo) the CLI ensures
	// is registered so our agent add-on becomes installable.
	AddonRepository string `json:"addon_repository"`

	// Addons is the ordered set of add-ons the gateway needs at bootstrap.
	Addons []Addon `json:"addons"`
}

// Addon is one add-on in the bootstrap set. Community add-ons carry a repo-hash
// prefix in their full Supervisor slug that we cannot know ahead of time, so the
// CLI resolves the full slug at runtime by matching Match against the store;
// Slug is an optional exact override for add-ons whose full slug is stable (the
// built-in "core_*" add-ons).
type Addon struct {
	// Name is the human label shown in CLI output.
	Name string `json:"name"`
	// Match is the stable inner slug used to resolve the full store slug: an
	// add-on matches when its full slug equals Match or ends with "_"+Match
	// (e.g. Match "mosquitto" resolves "core_mosquitto"; "smart_home_agent"
	// resolves "<repohash>_smart_home_agent").
	Match string `json:"match"`
	// Slug, when set, is the exact full Supervisor slug and bypasses Match.
	Slug string `json:"slug,omitempty"`
	// Repository is the add-on store repo URL to register before installing a
	// community add-on; blank for built-in ("core") add-ons.
	Repository string `json:"repository,omitempty"`
	// Core marks a built-in Home Assistant add-on (no repository to add).
	Core bool `json:"core"`
}

// Default returns the manifest embedded in the binary at build time.
func Default() (*Manifest, error) {
	return parse(embedded, "embedded release.json")
}

// Load reads a manifest from disk, or returns the embedded default when path is
// empty. This is the one entry point the CLI uses so the --manifest override and
// the built-in default share identical parsing and validation.
func Load(path string) (*Manifest, error) {
	if path == "" {
		return Default()
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fleet: read manifest %q: %w", path, err)
	}

	return parse(raw, path)
}

func parse(raw []byte, source string) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("fleet: parse %s: %w", source, err)
	}

	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", source, err)
	}

	return &m, nil
}

// Validate checks the invariants the CLI relies on: an add-on repository to
// register and an agent add-on to install (the whole point of onboarding).
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.AddonRepository) == "" {
		return fmt.Errorf("addon_repository is required")
	}

	if len(m.Addons) == 0 {
		return fmt.Errorf("at least one add-on is required")
	}

	for i, a := range m.Addons {
		if strings.TrimSpace(a.Match) == "" && strings.TrimSpace(a.Slug) == "" {
			return fmt.Errorf("addons[%d] (%q): one of match or slug is required", i, a.Name)
		}
	}

	if m.Agent() == nil {
		return fmt.Errorf("an add-on with match %q is required", agentMatch)
	}

	return nil
}

// agentMatch is the inner slug of our agent add-on (config.yaml `slug:`); the
// manifest must contain it because onboarding without the agent is meaningless.
const agentMatch = "smart_home_agent"

// Agent returns the agent add-on entry, or nil when absent.
func (m *Manifest) Agent() *Addon {
	for i := range m.Addons {
		if m.Addons[i].Match == agentMatch {
			return &m.Addons[i]
		}
	}

	return nil
}

// Resolves reports whether fullSlug is this add-on: an exact Slug match when one
// is configured, otherwise Match as the whole slug or as a "_"-suffixed tail
// (the repo-hash-prefixed community-add-on case).
func (a Addon) Resolves(fullSlug string) bool {
	if a.Slug != "" {
		return fullSlug == a.Slug
	}

	return fullSlug == a.Match || strings.HasSuffix(fullSlug, "_"+a.Match)
}
