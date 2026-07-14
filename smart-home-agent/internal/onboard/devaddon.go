package onboard

import "github.com/smart-home/edge/agent/fleet"

// LocalAgentSlug is the Supervisor slug of the agent when installed as a LOCAL
// add-on (from a source drop in /addons) rather than from the store. Supervisor
// prefixes a local add-on's own slug (config.yaml `slug: smart_home_agent`) with
// "local_". The dev flow (`smart-onboard --dev`) targets this add-on, which the
// developer installs from their checkout via scripts/sync-addon-to-vm.sh.
const LocalAgentSlug = "local_smart_home_agent"

// agentAddonMatch is the manifest `match` of the agent add-on (config.yaml
// `slug:`), used to single it out for the dev local-install path.
const agentAddonMatch = "smart_home_agent"

// isAgentAddon reports whether a manifest add-on is our agent.
func isAgentAddon(a fleet.Addon) bool {
	return a.Match == agentAddonMatch
}

// devAgentNote is the reminder printed in --dev when the agent add-on is skipped:
// smart-onboard no longer delivers the source itself, so the developer installs
// the local add-on out-of-band from their checkout.
const devAgentNote = "--dev: skipping the store agent add-on. Install it from your checkout instead — run `scripts/sync-addon-to-vm.sh serve` (serves the source + Laravel), then in Home Assistant: Settings → Add-ons → Add-on store → ⋮ → Check for updates → Local add-ons → Smart Home Agent → Install."
