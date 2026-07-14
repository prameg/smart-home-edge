package onboard

import (
	"context"
	"strings"
	"time"

	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/supervisor"
)

// DeviceAPI is everything the onboarding steps do to a gateway, expressed as one
// narrow seam so the steps are testable against a fake and the real HTTP client
// (client.go) is the only place that knows Home Assistant's wire formats.
//
// Authentication is stateful: the connectivity/onboarding calls run before any
// token exists; once the owner+token step obtains a token it calls SetToken, and
// every subsequent Supervisor call authenticates with that stored token. Keeping
// the token on the client (rather than threading it through every method) keeps
// the interface and the step code readable.
type DeviceAPI interface {
	// WaitForCore blocks until Home Assistant Core's HTTP API answers or timeout
	// elapses (first HAOS boot downloads Core, which can take minutes).
	WaitForCore(ctx context.Context, timeout time.Duration) error

	// OnboardingStatus reports where the device is in the HA onboarding wizard.
	OnboardingStatus(ctx context.Context) (OnboardingStatus, error)
	// CreateOwner creates the owner account via the onboarding API and returns
	// an authorization code to exchange for tokens.
	CreateOwner(ctx context.Context, o OwnerConfig) (authCode string, err error)
	// Login runs the username/password auth flow (the re-run path when the owner
	// already exists) and returns an authorization code.
	Login(ctx context.Context, username, password string) (authCode string, err error)
	// ExchangeCode swaps an authorization code for a short-lived access token.
	ExchangeCode(ctx context.Context, authCode string) (accessToken string, err error)
	// CreateLongLivedToken upgrades a short-lived access token into a long-lived
	// one that outlasts a slow add-on install / Core download.
	CreateLongLivedToken(ctx context.Context, accessToken, clientName string) (token string, err error)
	// FinishOnboarding best-effort completes the remaining wizard steps
	// (core_config/analytics/integration) so the frontend does not reappear at
	// the wizard; failures are non-fatal.
	FinishOnboarding(ctx context.Context) error
	// UpdateCoreConfig sets Home Assistant's core configuration (country/time
	// zone/…) that the headless onboarding flow never sets, so the device is not
	// left warning that no country is configured. An empty config is a no-op.
	UpdateCoreConfig(ctx context.Context, cfg CoreConfig) error
	// SetToken stores the token used to authenticate all later Supervisor calls.
	SetToken(token string)
	// Token returns the stored token (empty before authentication).
	Token() string

	// StoreRepositories lists the add-on store repository URLs already
	// registered.
	StoreRepositories(ctx context.Context) ([]string, error)
	// AddStoreRepository registers an add-on store repository by URL.
	AddStoreRepository(ctx context.Context, url string) error
	// ResolveAddonSlug maps a manifest add-on to its full Supervisor slug (an
	// exact configured slug, or by matching the store when it carries a
	// repo-hash prefix). Returns found=false when the add-on is not yet in the
	// store (e.g. its repository has not finished loading).
	ResolveAddonSlug(ctx context.Context, a fleet.Addon) (slug string, found bool, err error)
	// AddonInfo returns an add-on's install state and versions.
	AddonInfo(ctx context.Context, slug string) (AddonInfo, error)
	// InstallAddon installs an add-on (latest available version).
	InstallAddon(ctx context.Context, slug string) error
	// SetAddonOptions writes an add-on's user options (the add-on's config.yaml
	// schema keys).
	SetAddonOptions(ctx context.Context, slug string, options map[string]any) error
	// StartAddon starts an installed add-on.
	StartAddon(ctx context.Context, slug string) error
	// RestartAddon restarts an installed add-on so option changes written while
	// it was running take effect (HA loads add-on options only at start).
	RestartAddon(ctx context.Context, slug string) error

	// ClaimInfo reads the agent's provisioned identity and short claim code by
	// inspecting the agent add-on (its logs, falling back to the HA persistent
	// notification). found is false until the agent has provisioned.
	ClaimInfo(ctx context.Context, agentSlug string) (ClaimInfo, error)
}

// OnboardingStatus is where a device sits in the HA onboarding wizard.
type OnboardingStatus struct {
	// UserDone is true once the owner account exists.
	UserDone bool
	// AllDone is true once the whole wizard is complete (the onboarding
	// endpoints are gone).
	AllDone bool
}

// AddonInfo is an add-on's install state as Supervisor reports it. Aliased to
// the shared supervisor package's type so the onboarding steps and the agent's
// self-update path speak one shape.
type AddonInfo = supervisor.AddonInfo

// ClaimInfo is what the agent surfaces once it has provisioned: its cloud uid,
// the hardware serial it enrolled under (auto-derived on the device unless the
// operator overrode it), whether it is already claimed, and (while unclaimed)
// the short claim code the installer reads off to bind the gateway to a home.
type ClaimInfo struct {
	UID       string
	Serial    string
	Claimed   bool
	ClaimCode string
}

// OwnerConfig is the HA owner account the CLI creates on a fresh device.
type OwnerConfig struct {
	Name     string
	Username string
	Password string
	Language string
}

// CoreConfig is the subset of Home Assistant's core configuration the onboarding
// flow sets so the finished device is not left warning about missing location
// data (notably "No country has been configured"). Every field is optional; an
// empty one is not sent, and an all-empty config is a no-op.
type CoreConfig struct {
	// Country is an ISO-3166 alpha-2 code (e.g. "SA"). Setting it clears the
	// "No country has been configured" repair Core surfaces after a headless
	// onboarding.
	Country string
	// TimeZone is an IANA name (e.g. "Asia/Riyadh"); empty leaves HA's default.
	TimeZone string
	// Currency is an ISO-4217 code (e.g. "SAR"); empty leaves HA's default.
	Currency string
	// UnitSystem is "metric" or "us_customary"; empty leaves HA's default.
	UnitSystem string
	// Language is the instance UI language (e.g. "en"); empty leaves HA's
	// default.
	Language string
}

// fields returns only the set core-config keys, in the shape config/core/update
// expects.
func (c CoreConfig) fields() map[string]any {
	out := map[string]any{}
	if c.Country != "" {
		out["country"] = c.Country
	}
	if c.TimeZone != "" {
		out["time_zone"] = c.TimeZone
	}
	if c.Currency != "" {
		out["currency"] = c.Currency
	}
	if c.UnitSystem != "" {
		out["unit_system"] = c.UnitSystem
	}
	if c.Language != "" {
		out["language"] = c.Language
	}

	return out
}

// Timeouts bounds the two waits that can legitimately take minutes on real
// hardware: Core coming up after a fresh flash, and the agent provisioning
// against the cloud after it starts.
type Timeouts struct {
	WaitCore      time.Duration
	WaitProvision time.Duration
}

// ZigbeeConfig is the coordinator radio written to the Zigbee2MQTT add-on so a
// freshly onboarded gateway can pair devices immediately, instead of installing
// Z2M and sitting broken until a human points it at the dongle — which is exactly
// what makes the setup team's *first* action (pair a sensor) fail on a fresh
// unit. Port and Adapter are the Z2M add-on's `serial.*` options (its schema:
// adapter is match(zstack|deconz|zigate|ezsp|ember|zboss)).
type ZigbeeConfig struct {
	// Port is the coordinator's serial device. For the golden image it MUST be
	// serial-agnostic: a `dd` clone shares one Z2M options.json across every unit,
	// so a /dev/serial/by-id/… path that embeds the reference dongle's serial
	// number would break every other unit. "/dev/ttyACM0" (the ZBDongle-E's CDC
	// node) is stable on a factory Pi whose only serial device is the coordinator;
	// a unit with extra serial hardware can override with a by-id path.
	Port string
	// Adapter is the zigbee-herdsman driver: "ember" for the EFR32-based Sonoff
	// ZBDongle-E (the locked dongle), "zstack" for the CC2652-based ZBDongle-P.
	Adapter string
	// Baudrate overrides the driver default when non-zero; ember's default is
	// correct, so it is normally left 0 (omitted).
	Baudrate int
}

// Configured reports whether a coordinator was specified. An empty Port disables
// the Zigbee step — the dev VM has no real radio and validates pairing against a
// fake light entity instead.
func (z ZigbeeConfig) Configured() bool { return strings.TrimSpace(z.Port) != "" }

// serialOptions renders the Zigbee2MQTT add-on's `serial` option object. Omitted
// keys fall back to the add-on's own schema defaults (SetAddonOptions replaces,
// not merges), so sending just port+adapter is safe.
func (z ZigbeeConfig) serialOptions() map[string]any {
	serial := map[string]any{"port": z.Port}
	if z.Adapter != "" {
		serial["adapter"] = z.Adapter
	}
	if z.Baudrate > 0 {
		serial["baudrate"] = z.Baudrate
	}

	return serial
}

// State is the shared, mutating context threaded through every step: the fixed
// inputs (client, manifest, owner, agent options, timeouts) plus values
// accumulated as the run progresses (token, resolved agent slug, provisioned
// identity + claim code). Steps read earlier steps' output from here.
type State struct {
	Client       DeviceAPI
	Manifest     *fleet.Manifest
	Owner        OwnerConfig
	CoreConfig   CoreConfig
	AgentOptions map[string]any
	Zigbee       ZigbeeConfig
	Timeouts     Timeouts

	// Dev switches the run to developer mode: the install-addons step SKIPS the
	// agent add-on (the developer installs it from their local checkout via
	// scripts/sync-addon-to-vm.sh) and the agent slug resolves to the local
	// add-on. The other add-ons still install from the store at latest.
	Dev bool

	// Accumulated during the run.
	AccessToken string
	AgentSlug   string
	Claim       ClaimInfo
}
