package onboard

import (
	"context"
	"testing"
	"time"

	"github.com/smart-home/edge/agent/fleet"
)

// fakeDevice is an in-memory DeviceAPI for exercising step logic without HTTP.
// Only the behavior the step tests touch is modeled; the rest returns zero
// values so the fake satisfies the whole interface without panicking.
type fakeDevice struct {
	token string

	onboarding OnboardingStatus

	createdOwner bool
	loggedIn     bool

	knownSlugs []string
	addons     map[string]*AddonInfo

	installs map[string]int
	starts   map[string]int
	restarts map[string]int
	options  map[string]map[string]any
	autoOff  map[string]bool

	claim ClaimInfo
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{
		addons:   map[string]*AddonInfo{},
		installs: map[string]int{},
		starts:   map[string]int{},
		restarts: map[string]int{},
		options:  map[string]map[string]any{},
		autoOff:  map[string]bool{},
	}
}

func (f *fakeDevice) WaitForCore(context.Context, time.Duration) error { return nil }

func (f *fakeDevice) OnboardingStatus(context.Context) (OnboardingStatus, error) {
	return f.onboarding, nil
}

func (f *fakeDevice) CreateOwner(context.Context, OwnerConfig) (string, error) {
	f.createdOwner = true
	f.onboarding.UserDone = true

	return "auth-code", nil
}

func (f *fakeDevice) Login(context.Context, string, string) (string, error) {
	f.loggedIn = true

	return "auth-code", nil
}

func (f *fakeDevice) ExchangeCode(context.Context, string) (string, error) {
	return "access-token", nil
}

func (f *fakeDevice) CreateLongLivedToken(context.Context, string, string) (string, error) {
	return "long-lived", nil
}

func (f *fakeDevice) FinishOnboarding(context.Context) error { return nil }

func (f *fakeDevice) SetToken(t string) { f.token = t }
func (f *fakeDevice) Token() string     { return f.token }

func (f *fakeDevice) StoreRepositories(context.Context) ([]string, error) { return nil, nil }
func (f *fakeDevice) AddStoreRepository(context.Context, string) error    { return nil }

func (f *fakeDevice) ResolveAddonSlug(_ context.Context, a fleet.Addon) (string, bool, error) {
	if a.Slug != "" {
		return a.Slug, true, nil
	}
	for _, s := range f.knownSlugs {
		if a.Resolves(s) {
			return s, true, nil
		}
	}

	return "", false, nil
}

func (f *fakeDevice) AddonInfo(_ context.Context, slug string) (AddonInfo, error) {
	if info, ok := f.addons[slug]; ok {
		return *info, nil
	}

	return AddonInfo{Slug: slug, Installed: false}, nil
}

func (f *fakeDevice) InstallAddon(_ context.Context, slug string) error {
	f.installs[slug]++
	f.addons[slug] = &AddonInfo{Slug: slug, Installed: true, Version: "9.9.9", State: "stopped"}

	return nil
}

func (f *fakeDevice) UpdateAddon(_ context.Context, slug, version string) error {
	if info, ok := f.addons[slug]; ok {
		info.Version = version
	}

	return nil
}

func (f *fakeDevice) SetAddonOptions(_ context.Context, slug string, options map[string]any) error {
	f.options[slug] = options
	if info, ok := f.addons[slug]; ok {
		info.Options = options
	}

	return nil
}

func (f *fakeDevice) SetAddonAutoUpdate(_ context.Context, slug string, enabled bool) error {
	f.autoOff[slug] = !enabled
	if info, ok := f.addons[slug]; ok {
		info.AutoUpdate = enabled
	}

	return nil
}

func (f *fakeDevice) StartAddon(_ context.Context, slug string) error {
	f.starts[slug]++
	if info, ok := f.addons[slug]; ok {
		info.State = "started"
	}

	return nil
}

func (f *fakeDevice) RestartAddon(_ context.Context, slug string) error {
	f.restarts[slug]++
	if info, ok := f.addons[slug]; ok {
		info.State = "started"
	}

	return nil
}

func (f *fakeDevice) OSInfo(context.Context) (VersionInfo, error)   { return VersionInfo{}, nil }
func (f *fakeDevice) CoreInfo(context.Context) (VersionInfo, error) { return VersionInfo{}, nil }
func (f *fakeDevice) UpdateOS(context.Context, string) error        { return nil }
func (f *fakeDevice) UpdateCore(context.Context, string) error      { return nil }

func (f *fakeDevice) ClaimInfo(context.Context, string) (ClaimInfo, error) { return f.claim, nil }

// testManifest is a minimal single-add-on release used by the step tests.
func testManifest(agentVersion string) *fleet.Manifest {
	return &fleet.Manifest{
		ReleaseID:       "test",
		AddonRepository: "https://repo.test",
		Addons: []fleet.Addon{
			{Name: "Smart Home Agent", Match: "smart_home_agent", Version: agentVersion, Repository: "https://repo.test"},
		},
	}
}

// On a fresh device the owner+token step creates the owner; on a re-run where the
// owner already exists it logs in instead. Both end with a usable token.
func TestOwnerAndTokenStepBranches(t *testing.T) {
	step := ownerAndTokenStep(&captureReporter{})

	t.Run("fresh device creates owner", func(t *testing.T) {
		dev := newFakeDevice()
		st := &State{Client: dev, Manifest: testManifest(""), Owner: OwnerConfig{Username: "admin", Password: "pw"}}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if !dev.createdOwner || dev.loggedIn {
			t.Errorf("fresh device should create the owner, not log in (created=%v loggedIn=%v)", dev.createdOwner, dev.loggedIn)
		}
		if dev.Token() != "long-lived" {
			t.Errorf("expected long-lived token stored, got %q", dev.Token())
		}
		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})

	t.Run("re-run logs in", func(t *testing.T) {
		dev := newFakeDevice()
		dev.onboarding.UserDone = true
		st := &State{Client: dev, Manifest: testManifest(""), Owner: OwnerConfig{Username: "admin", Password: "pw"}}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if dev.createdOwner || !dev.loggedIn {
			t.Errorf("existing owner should log in, not re-create (created=%v loggedIn=%v)", dev.createdOwner, dev.loggedIn)
		}
	})
}

// The install step resolves each add-on's full slug, installs the missing ones,
// caches the agent slug, and is skipped when everything is already installed.
func TestInstallAddonsStep(t *testing.T) {
	rep := &captureReporter{}
	step := installAddonsStep(rep)

	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}

	st := &State{Client: dev, Manifest: testManifest("")}

	done, err := step.Check(context.Background(), st)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("Check should report not-done before install")
	}
	if st.AgentSlug != "hash_smart_home_agent" {
		t.Errorf("agent slug should be cached during Check, got %q", st.AgentSlug)
	}

	if err := step.Act(context.Background(), st); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if dev.installs["hash_smart_home_agent"] != 1 {
		t.Errorf("expected one install, got %d", dev.installs["hash_smart_home_agent"])
	}

	if err := step.Verify(context.Background(), st); err != nil {
		t.Errorf("Verify: %v", err)
	}

	// A second Check now short-circuits (already installed) — the resume path.
	done, err = step.Check(context.Background(), st)
	if err != nil || !done {
		t.Errorf("Check should skip once installed: done=%v err=%v", done, err)
	}
}

// A pinned version that does not match the installed one triggers an update.
func TestInstallAddonsStepPins(t *testing.T) {
	step := installAddonsStep(&captureReporter{})

	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}
	// Already installed, but at the wrong version.
	dev.addons["hash_smart_home_agent"] = &AddonInfo{Slug: "hash_smart_home_agent", Installed: true, Version: "0.0.1"}

	st := &State{Client: dev, Manifest: testManifest("1.2.3")}

	done, err := step.Check(context.Background(), st)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("a version mismatch should not be considered done")
	}

	if err := step.Act(context.Background(), st); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if dev.addons["hash_smart_home_agent"].Version != "1.2.3" {
		t.Errorf("expected pin to 1.2.3, got %q", dev.addons["hash_smart_home_agent"].Version)
	}
}

// configure-agent writes the options and, on a resumed run where the add-on is
// already running, restarts it so the corrected config actually loads (HA reads
// options only at container start). A first run whose add-on is still stopped
// only writes options — start-agent starts it fresh, so no restart is needed.
func TestConfigureAgentStepRestartsRunningAddon(t *testing.T) {
	step := configureAgentStep(&captureReporter{})

	newState := func(dev *fakeDevice) *State {
		return &State{
			Client:       dev,
			Manifest:     testManifest(""),
			AgentSlug:    "hash_smart_home_agent",
			AgentOptions: map[string]any{"cloud_base_url": "http://192.168.100.73:8090"},
		}
	}

	t.Run("running add-on is restarted", func(t *testing.T) {
		dev := newFakeDevice()
		dev.addons["hash_smart_home_agent"] = &AddonInfo{
			Slug: "hash_smart_home_agent", Installed: true, Version: "9.9.9", State: "started",
			Options: map[string]any{"cloud_base_url": "http://10.0.2.2:8090"},
		}

		if err := step.Act(context.Background(), newState(dev)); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if dev.restarts["hash_smart_home_agent"] != 1 {
			t.Errorf("a running add-on must be restarted to apply new options, got %d restarts", dev.restarts["hash_smart_home_agent"])
		}
	})

	t.Run("stopped add-on is not restarted", func(t *testing.T) {
		dev := newFakeDevice()
		dev.addons["hash_smart_home_agent"] = &AddonInfo{
			Slug: "hash_smart_home_agent", Installed: true, Version: "9.9.9", State: "stopped",
		}

		if err := step.Act(context.Background(), newState(dev)); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if dev.restarts["hash_smart_home_agent"] != 0 {
			t.Errorf("a stopped add-on must not be restarted (start-agent starts it fresh), got %d restarts", dev.restarts["hash_smart_home_agent"])
		}
	})
}

// The await-provision step captures the uid + claim code onto the State.
func TestAwaitProvisionStepCapturesClaim(t *testing.T) {
	step := awaitProvisionStep(&captureReporter{})

	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}
	dev.claim = ClaimInfo{UID: "uid-1", ClaimCode: "ABCD-EFGH"}

	st := &State{Client: dev, Manifest: testManifest(""), Timeouts: Timeouts{WaitProvision: time.Second}}

	done, err := step.Check(context.Background(), st)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatal("Check should report done once the agent has a uid")
	}
	if st.Claim.UID != "uid-1" || st.Claim.ClaimCode != "ABCD-EFGH" {
		t.Errorf("claim not captured onto state: %+v", st.Claim)
	}
}

// resolveAgentSlug caches the resolved slug and only resolves once.
func TestResolveAgentSlugCaches(t *testing.T) {
	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}
	st := &State{Client: dev, Manifest: testManifest("")}

	if err := resolveAgentSlug(context.Background(), st); err != nil {
		t.Fatalf("resolveAgentSlug: %v", err)
	}
	if st.AgentSlug != "hash_smart_home_agent" {
		t.Errorf("unexpected slug %q", st.AgentSlug)
	}

	// With the slug cached, a device that no longer knows it must not be
	// re-queried (cache short-circuits).
	dev.knownSlugs = nil
	if err := resolveAgentSlug(context.Background(), st); err != nil {
		t.Errorf("cached resolve should not error: %v", err)
	}
}

func TestOptionsSatisfied(t *testing.T) {
	desired := map[string]any{"mqtt_port": 8883, "mqtt_tls": true, "factory_key": "k"}

	// Current from Supervisor decodes numbers as float64 — the JSON compare must
	// still treat 8883 and 8883.0 as equal.
	current := map[string]any{"mqtt_port": float64(8883), "mqtt_tls": true, "factory_key": "k", "extra": "ignored"}
	if !optionsSatisfied(current, desired) {
		t.Error("expected satisfied when all desired keys match (numeric encoding aside)")
	}

	if optionsSatisfied(map[string]any{"mqtt_port": float64(1883)}, desired) {
		t.Error("a differing value must not be satisfied")
	}
	if optionsSatisfied(nil, desired) {
		t.Error("nil current cannot satisfy a non-empty desired")
	}
	if !optionsSatisfied(nil, nil) {
		t.Error("empty desired is trivially satisfied")
	}
}

func TestRequiredRepositoriesAndPresence(t *testing.T) {
	m := &fleet.Manifest{
		AddonRepository: "https://repo.test",
		Addons: []fleet.Addon{
			{Match: "mosquitto", Core: true},                             // core: no repo
			{Match: "zigbee2mqtt", Repository: "https://z2m.test"},       // community repo
			{Match: "smart_home_agent", Repository: "https://repo.test"}, // dup of edge repo
		},
	}

	repos := requiredRepositories(m)
	if len(repos) != 2 {
		t.Fatalf("expected 2 distinct repos, got %v", repos)
	}

	// Presence compares ignoring trailing slash / .git.
	if !repoPresent([]string{"https://repo.test/"}, "https://repo.test") {
		t.Error("trailing slash should still match")
	}
	if !repoPresent([]string{"https://z2m.test.git"}, "https://z2m.test") {
		t.Error(".git suffix should still match")
	}
	if repoPresent([]string{"https://other.test"}, "https://repo.test") {
		t.Error("unrelated repo must not match")
	}
}

func TestPinningNeeded(t *testing.T) {
	if pinningNeeded(&fleet.Manifest{Addons: []fleet.Addon{{Match: "x"}}}) {
		t.Error("a template with no versions needs no pinning")
	}
	if !pinningNeeded(&fleet.Manifest{Addons: []fleet.Addon{{Match: "x", Version: "1.0"}}}) {
		t.Error("a pinned add-on needs pinning")
	}
	if !pinningNeeded(&fleet.Manifest{Core: fleet.Component{Version: "2026.7"}, Addons: []fleet.Addon{{Match: "x"}}}) {
		t.Error("a pinned Core version needs pinning")
	}
}
