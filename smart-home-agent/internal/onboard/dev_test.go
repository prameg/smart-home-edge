package onboard

import (
	"context"
	"testing"

	"github.com/smart-home/edge/agent/fleet"
)

// devManifest is a bootstrap set with the agent plus one core add-on, so a dev
// test can assert the agent is SKIPPED while the rest still install from the
// store.
func devManifest() *fleet.Manifest {
	return &fleet.Manifest{
		AddonRepository: "https://repo.test",
		Addons: []fleet.Addon{
			{Name: "Mosquitto", Match: "mosquitto", Slug: "core_mosquitto", Core: true},
			{Name: "Smart Home Agent", Match: "smart_home_agent", Repository: "https://repo.test"},
		},
	}
}

func devState(dev *fakeDevice) *State {
	return &State{
		Client:   dev,
		Manifest: devManifest(),
		Dev:      true,
	}
}

// In --dev the agent add-on is never installed by smart-onboard: the developer
// installs local_smart_home_agent from their checkout. Once it is present the
// install-addons step installs the other add-ons from the store and treats the
// agent as satisfied.
func TestInstallAddonsStepDevSkipsAgentInstall(t *testing.T) {
	step := installAddonsStep(&captureReporter{})

	dev := newFakeDevice()
	dev.addons[LocalAgentSlug] = &AddonInfo{Slug: LocalAgentSlug, Installed: true, State: "stopped"}
	st := devState(dev)

	if err := step.Act(context.Background(), st); err != nil {
		t.Fatalf("Act: %v", err)
	}

	if dev.installs[LocalAgentSlug] != 0 {
		t.Errorf("dev must NOT install the agent add-on, got %d installs", dev.installs[LocalAgentSlug])
	}
	if dev.installs["core_mosquitto"] != 1 {
		t.Errorf("expected the core add-on to install from the store, got %d", dev.installs["core_mosquitto"])
	}

	if err := step.Verify(context.Background(), st); err != nil {
		t.Errorf("Verify: %v", err)
	}
	done, err := step.Check(context.Background(), st)
	if err != nil || !done {
		t.Errorf("Check should be satisfied once the local agent is present: done=%v err=%v", done, err)
	}
}

// When the developer has not installed the local agent yet, the step fails with
// actionable guidance (install it via scripts/sync-addon-to-vm.sh, then re-run).
func TestInstallAddonsStepDevRequiresLocalAgent(t *testing.T) {
	step := installAddonsStep(&captureReporter{})

	dev := newFakeDevice()
	st := devState(dev)

	err := step.Act(context.Background(), st)
	if err == nil {
		t.Fatal("expected an error when the local agent add-on is not installed")
	}
	if dev.installs[LocalAgentSlug] != 0 {
		t.Errorf("dev must never install the agent add-on, got %d installs", dev.installs[LocalAgentSlug])
	}
}

// resolveAgentSlug fixes the slug to local_smart_home_agent in dev mode without
// touching the store.
func TestResolveAgentSlugDev(t *testing.T) {
	dev := newFakeDevice()
	st := devState(dev)

	if err := resolveAgentSlug(context.Background(), st); err != nil {
		t.Fatalf("resolveAgentSlug: %v", err)
	}
	if st.AgentSlug != LocalAgentSlug {
		t.Errorf("dev agent slug should be %q, got %q", LocalAgentSlug, st.AgentSlug)
	}
}
