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

	coreConfig    CoreConfig
	coreConfigSet bool

	claim ClaimInfo
}

func newFakeDevice() *fakeDevice {
	return &fakeDevice{
		addons:   map[string]*AddonInfo{},
		installs: map[string]int{},
		starts:   map[string]int{},
		restarts: map[string]int{},
		options:  map[string]map[string]any{},
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

func (f *fakeDevice) UpdateCoreConfig(_ context.Context, cfg CoreConfig) error {
	f.coreConfig = cfg
	f.coreConfigSet = true

	return nil
}

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

func (f *fakeDevice) SetAddonOptions(_ context.Context, slug string, options map[string]any) error {
	f.options[slug] = options
	if info, ok := f.addons[slug]; ok {
		info.Options = options
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

func (f *fakeDevice) ClaimInfo(context.Context, string) (ClaimInfo, error) { return f.claim, nil }

// testManifest is a minimal single-add-on bootstrap set used by the step tests.
func testManifest() *fleet.Manifest {
	return &fleet.Manifest{
		AddonRepository: "https://repo.test",
		Addons: []fleet.Addon{
			{Name: "Smart Home Agent", Match: "smart_home_agent", Repository: "https://repo.test"},
		},
	}
}

// On a fresh device the owner+token step creates the owner; on a re-run where the
// owner already exists it logs in instead. Both end with a usable token.
func TestOwnerAndTokenStepBranches(t *testing.T) {
	step := ownerAndTokenStep(&captureReporter{})

	t.Run("fresh device creates owner", func(t *testing.T) {
		dev := newFakeDevice()
		st := &State{Client: dev, Manifest: testManifest(), Owner: OwnerConfig{Username: "admin", Password: "pw"}, CoreConfig: CoreConfig{Country: "SA"}}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if !dev.createdOwner || dev.loggedIn {
			t.Errorf("fresh device should create the owner, not log in (created=%v loggedIn=%v)", dev.createdOwner, dev.loggedIn)
		}
		if dev.Token() != "long-lived" {
			t.Errorf("expected long-lived token stored, got %q", dev.Token())
		}
		if !dev.coreConfigSet || dev.coreConfig.Country != "SA" {
			t.Errorf("owner step should apply the core config (country), got set=%v cfg=%+v", dev.coreConfigSet, dev.coreConfig)
		}
		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify: %v", err)
		}
	})

	t.Run("re-run logs in", func(t *testing.T) {
		dev := newFakeDevice()
		dev.onboarding.UserDone = true
		st := &State{Client: dev, Manifest: testManifest(), Owner: OwnerConfig{Username: "admin", Password: "pw"}}

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

	st := &State{Client: dev, Manifest: testManifest()}

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

// The install step installs the latest available add-on and never pins: an
// already-installed add-on (any version) short-circuits Check, so the CLI leaves
// version selection entirely to the cloud after claim.
func TestInstallAddonsStepDoesNotPin(t *testing.T) {
	step := installAddonsStep(&captureReporter{})

	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}
	// Already installed at some version — the CLI must treat this as done and
	// must not touch the version.
	dev.addons["hash_smart_home_agent"] = &AddonInfo{Slug: "hash_smart_home_agent", Installed: true, Version: "0.0.1"}

	st := &State{Client: dev, Manifest: testManifest()}

	done, err := step.Check(context.Background(), st)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatal("an already-installed add-on should be considered done (no pinning)")
	}

	if err := step.Act(context.Background(), st); err != nil {
		t.Fatalf("Act: %v", err)
	}
	if dev.installs["hash_smart_home_agent"] != 0 {
		t.Errorf("must not reinstall an already-installed add-on, got %d", dev.installs["hash_smart_home_agent"])
	}
	if dev.addons["hash_smart_home_agent"].Version != "0.0.1" {
		t.Errorf("CLI must not change the add-on version, got %q", dev.addons["hash_smart_home_agent"].Version)
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
			Manifest:     testManifest(),
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

// start-broker starts the Mosquitto add-on (its slug is a stable core slug), is
// idempotent once started, and skips cleanly when the manifest carries no broker.
func TestStartBrokerStep(t *testing.T) {
	withBroker := func() *fleet.Manifest {
		return &fleet.Manifest{
			AddonRepository: "https://repo.test",
			Addons: []fleet.Addon{
				{Name: "Mosquitto broker", Match: "mosquitto", Slug: "core_mosquitto", Core: true},
				{Name: "Smart Home Agent", Match: "smart_home_agent", Repository: "https://repo.test"},
			},
		}
	}

	t.Run("starts a stopped broker and is idempotent", func(t *testing.T) {
		step := startBrokerStep(&captureReporter{})
		dev := newFakeDevice()
		dev.addons["core_mosquitto"] = &AddonInfo{Slug: "core_mosquitto", Installed: true, State: "stopped"}
		st := &State{Client: dev, Manifest: withBroker()}

		done, err := step.Check(context.Background(), st)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if done {
			t.Fatal("a stopped broker must not be considered started")
		}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if dev.starts["core_mosquitto"] != 1 {
			t.Errorf("broker must be started once, got %d", dev.starts["core_mosquitto"])
		}
		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify: %v", err)
		}

		done, err = step.Check(context.Background(), st)
		if err != nil || !done {
			t.Errorf("Check should short-circuit once started: done=%v err=%v", done, err)
		}
	})

	t.Run("skips when the manifest has no broker", func(t *testing.T) {
		step := startBrokerStep(&captureReporter{})
		dev := newFakeDevice()
		st := &State{Client: dev, Manifest: testManifest()} // agent-only manifest

		done, err := step.Check(context.Background(), st)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !done {
			t.Fatal("a manifest without a broker must skip the step (done=true)")
		}
		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify must no-op when there is no broker: %v", err)
		}
		if dev.starts["core_mosquitto"] != 0 {
			t.Errorf("nothing should be started when no broker is in the manifest")
		}
	})
}

// configure-zigbee points Z2M at the coordinator and starts it, is idempotent on
// a resumed run, restarts a Z2M already running with stale radio options, and is
// skipped entirely when no coordinator is configured (the dev-VM / fake-light case).
func TestConfigureZigbeeStep(t *testing.T) {
	manifest := func() *fleet.Manifest {
		return &fleet.Manifest{
			AddonRepository: "https://repo.test",
			Addons: []fleet.Addon{
				{Name: "Zigbee2MQTT", Match: "zigbee2mqtt", Repository: "https://z2m.test"},
				{Name: "Smart Home Agent", Match: "smart_home_agent", Repository: "https://repo.test"},
			},
		}
	}
	zcfg := ZigbeeConfig{Port: "/dev/ttyACM0", Adapter: "ember"}

	t.Run("configures a stopped add-on, preserving its other options, and starts it", func(t *testing.T) {
		step := configureZigbeeStep(&captureReporter{})
		dev := newFakeDevice()
		dev.knownSlugs = []string{"hash_zigbee2mqtt"}
		// Seed the add-on's default options as a fresh install returns them. The
		// step must send these back untouched — Supervisor rejects a write that
		// drops a required root key (the real "Missing option 'socat'" failure).
		dev.addons["hash_zigbee2mqtt"] = &AddonInfo{
			Slug: "hash_zigbee2mqtt", Installed: true, State: "stopped",
			Options: map[string]any{
				"data_path": "/config/zigbee2mqtt",
				"socat":     map[string]any{"enabled": false},
				"serial":    map[string]any{},
			},
		}
		st := &State{Client: dev, Manifest: manifest(), Zigbee: zcfg}

		done, err := step.Check(context.Background(), st)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if done {
			t.Fatal("Check should report not-done before the radio is configured")
		}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}

		posted := dev.options["hash_zigbee2mqtt"]
		serial, _ := posted["serial"].(map[string]any)
		if serial["port"] != "/dev/ttyACM0" || serial["adapter"] != "ember" {
			t.Errorf("Z2M serial options not written: %+v", posted)
		}
		// The required root keys the add-on already had MUST survive the write.
		if posted["data_path"] != "/config/zigbee2mqtt" {
			t.Errorf("data_path must be preserved, got %+v", posted)
		}
		if _, ok := posted["socat"]; !ok {
			t.Errorf("socat must be preserved (dropping it is the bug), got %+v", posted)
		}
		if dev.starts["hash_zigbee2mqtt"] != 1 {
			t.Errorf("a stopped Z2M must be started once, got %d", dev.starts["hash_zigbee2mqtt"])
		}
		if dev.restarts["hash_zigbee2mqtt"] != 0 {
			t.Errorf("a stopped Z2M must not be restarted, got %d", dev.restarts["hash_zigbee2mqtt"])
		}

		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify: %v", err)
		}

		done, err = step.Check(context.Background(), st)
		if err != nil || !done {
			t.Errorf("Check should short-circuit once configured+started: done=%v err=%v", done, err)
		}
	})

	t.Run("restarts an add-on already running with stale radio options", func(t *testing.T) {
		step := configureZigbeeStep(&captureReporter{})
		dev := newFakeDevice()
		dev.knownSlugs = []string{"hash_zigbee2mqtt"}
		dev.addons["hash_zigbee2mqtt"] = &AddonInfo{
			Slug: "hash_zigbee2mqtt", Installed: true, State: "started",
			Options: map[string]any{"serial": map[string]any{"port": "/dev/ttyUSB0", "adapter": "zstack"}},
		}
		st := &State{Client: dev, Manifest: manifest(), Zigbee: zcfg}

		done, err := step.Check(context.Background(), st)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if done {
			t.Fatal("stale radio options must not satisfy Check")
		}

		if err := step.Act(context.Background(), st); err != nil {
			t.Fatalf("Act: %v", err)
		}
		if dev.restarts["hash_zigbee2mqtt"] != 1 {
			t.Errorf("a running Z2M with changed options must be restarted once, got %d", dev.restarts["hash_zigbee2mqtt"])
		}
	})

	t.Run("skipped when no coordinator configured", func(t *testing.T) {
		step := configureZigbeeStep(&captureReporter{})
		dev := newFakeDevice() // no zigbee slug known — resolve would fail if the step tried
		st := &State{Client: dev, Manifest: manifest(), Zigbee: ZigbeeConfig{}}

		done, err := step.Check(context.Background(), st)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !done {
			t.Fatal("an unconfigured coordinator must skip the step (done=true)")
		}
		if err := step.Verify(context.Background(), st); err != nil {
			t.Errorf("Verify must no-op when unconfigured: %v", err)
		}
	})
}

// The await-provision step captures the uid + claim code onto the State.
func TestAwaitProvisionStepCapturesClaim(t *testing.T) {
	step := awaitProvisionStep(&captureReporter{})

	dev := newFakeDevice()
	dev.knownSlugs = []string{"hash_smart_home_agent"}
	dev.claim = ClaimInfo{UID: "uid-1", ClaimCode: "ABCD-EFGH"}

	st := &State{Client: dev, Manifest: testManifest(), Timeouts: Timeouts{WaitProvision: time.Second}}

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
	st := &State{Client: dev, Manifest: testManifest()}

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
