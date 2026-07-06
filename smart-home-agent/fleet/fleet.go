// Package fleet is the machine-readable twin of docs/fleet-release.md: one
// pinned, tested set of versions for everything smart-onboard puts on a gateway
// (HAOS, HA Core, the add-ons we depend on, and our agent). The onboarding CLI
// reads a Manifest to know which add-on repository to add, which add-ons to
// install (and at which exact versions), and what to pin the OS/Core to so a
// unit never drifts off a validated release.
//
// A default manifest is embedded at build time (release.json in this package),
// so the shipped binary always has a self-contained release definition; a caller
// can override it with an on-disk file (smart-onboard --manifest ...) for
// testing a candidate release before it is committed.
//
// This is also the artifact Phase 4 fleet rollout will push, and where the
// future Jetson AI-node section (JetPack version, container image digests, model
// versions) will live.
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

// Manifest is one fleet release: the exact versions that define "what is on this
// unit". A release is either populated (Populated=true — every version pinned and
// validated) or a template (Populated=false — versions still blank, used on a
// bring-up unit where the CLI installs latest and skips pinning).
type Manifest struct {
	// ReleaseID is the human release tag, e.g. "2026.07-r1".
	ReleaseID string `json:"release_id"`
	// Populated reports whether this release's versions have been filled in and
	// validated. When false the CLI treats every blank version as "install
	// latest / do not pin" and warns rather than failing.
	Populated bool `json:"populated"`
	// Notes is free-form context carried alongside the machine-readable fields
	// (JSON has no comments); not consumed programmatically.
	Notes string `json:"notes,omitempty"`

	// HAOS / Core are the OS and Home Assistant Core versions to converge to.
	HAOS Component `json:"haos"`
	Core Component `json:"core"`

	// AddonRepository is the add-on store repo (this edge repo) the CLI ensures
	// is registered so our agent add-on becomes installable.
	AddonRepository string `json:"addon_repository"`

	// Addons is the ordered set of add-ons the gateway depends on.
	Addons []Addon `json:"addons"`
}

// Component is a single pinned version (blank in a template release).
type Component struct {
	Version string `json:"version"`
}

// Addon is one add-on in the release. Community add-ons carry a repo-hash prefix
// in their full Supervisor slug that we cannot know ahead of time, so the CLI
// resolves the full slug at runtime by matching Match against the store; Slug is
// an optional exact override for add-ons whose full slug is stable (the built-in
// "core_*" add-ons).
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
	// Version is the pinned add-on version; blank means "install latest and do
	// not pin" (template release).
	Version string `json:"version"`
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

// Validate checks the invariants the CLI relies on: a release id, an add-on
// repository to register, and an agent add-on to install (the whole point of
// onboarding). Blank versions are allowed on purpose (template releases).
func (m *Manifest) Validate() error {
	if strings.TrimSpace(m.ReleaseID) == "" {
		return fmt.Errorf("release_id is required")
	}

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

// Pinned reports whether this add-on has an explicit version to install/pin.
func (a Addon) Pinned() bool {
	return strings.TrimSpace(a.Version) != ""
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
