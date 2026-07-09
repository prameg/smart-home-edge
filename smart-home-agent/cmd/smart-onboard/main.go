// Command smart-onboard takes a freshly flashed Home Assistant OS gateway from
// first boot to "agent provisioned, claim code in hand" over Home Assistant's
// stable public APIs. It is meant to run from a companion machine (a
// technician's laptop or a factory station) pointed at the device, though it
// works equally well run on the device itself.
//
// It is a resumable state machine: every step checks the device before acting,
// so if a run is interrupted — network blip, killed process, a slow add-on
// install — you recover by re-running the exact same command. Completed steps
// are skipped and the run continues where it left off.
//
// Usage — zero-config: run it with no flags and it discovers the gateway on the
// LAN over mDNS, then guides you through the remaining settings with sensible
// defaults:
//
//	smart-onboard
//
// Usage — fully specified (scripts / CI, pair with --yes to disable prompts):
//
//	smart-onboard \
//	  --host http://homeassistant.local:8123 \
//	  --cloud-base-url https://app.example.com \
//	  --factory-key <key> \
//	  --mqtt-host broker.example.com --mqtt-port 8883 --mqtt-tls \
//	  --owner-password <password> --yes
//
// Secrets can be supplied by flag, by environment variable
// (SMART_ONBOARD_FACTORY_KEY, SMART_ONBOARD_OWNER_PASSWORD), or interactively.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/onboard"
)

// errAborted signals the operator declined the confirmation. It is a clean exit,
// not a failure, so main reports it plainly and exits 0.
var errAborted = errors.New("cancelled")

func main() {
	err := run()
	if err == nil {
		return
	}

	if errors.Is(err, errAborted) {
		fmt.Fprintln(os.Stderr, "Cancelled — nothing was changed on the gateway.")

		return
	}

	fmt.Fprintln(os.Stderr, "\nOnboarding failed:", err)

	var stepErr *onboard.StepError
	if errors.As(err, &stepErr) {
		fmt.Fprintf(os.Stderr, "The failure was in step %q. Fix the cause shown above, then re-run the same command — it resumes where it stopped.\n", stepErr.Step)
	}

	os.Exit(1)
}

// discoverTimeout bounds the mDNS browse so a quiet network does not stall the
// guided flow; 4s is comfortably longer than the sub-second an on-LAN HAOS
// takes to answer its service query.
const discoverTimeout = 4 * time.Second

// options is the fully-resolved CLI input.
type options struct {
	host          string
	cloudBaseURL  string
	factoryKey    string
	serial        string
	mqttHost      string
	mqttPort      int
	mqttTLS       bool
	mqttInsecure  bool
	logLevel      string
	ownerName     string
	ownerUsername string
	ownerPassword string
	ownerLanguage string
	country       string
	timezone      string
	currency      string
	unitSystem    string
	manifestPath  string
	waitCore      time.Duration
	waitProvision time.Duration
	assumeYes     bool
	dev           bool
}

func run() error {
	opts, provided, err := parseFlags()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	opts, err = resolveOptions(ctx, opts, provided, os.Stdout)
	if err != nil {
		return err
	}

	manifest, err := fleet.Load(opts.manifestPath)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	reporter := newReporter(os.Stdout)

	printHeader(os.Stdout, opts, manifest)

	client := onboard.New(opts.host, log)

	st := &onboard.State{
		Client:       client,
		Manifest:     manifest,
		Owner:        opts.owner(),
		CoreConfig:   opts.coreConfig(),
		AgentOptions: opts.agentOptions(),
		Timeouts: onboard.Timeouts{
			WaitCore:      opts.waitCore,
			WaitProvision: opts.waitProvision,
		},
	}

	engine := onboard.NewEngine(onboard.BuildSteps(reporter), reporter)
	if err := engine.Run(ctx, st); err != nil {
		return err
	}

	printDone(os.Stdout, st)

	return nil
}

// parseFlags declares and parses the flags and reports which were explicitly set
// (so the guided flow knows whether to auto-discover the host and which values
// still need prompting). It does no validation or prompting itself — that is
// resolveOptions' job, once we know whether we can talk to a terminal.
func parseFlags() (options, map[string]bool, error) {
	var opts options

	flag.StringVar(&opts.host, "host", "http://homeassistant.local:8123", "gateway base URL (auto-discovered over mDNS when omitted)")
	flag.StringVar(&opts.cloudBaseURL, "cloud-base-url", "", "cloud origin the agent provisions against (required)")
	flag.StringVar(&opts.factoryKey, "factory-key", os.Getenv("SMART_ONBOARD_FACTORY_KEY"), "provisioning factory key (or SMART_ONBOARD_FACTORY_KEY; prompted if absent)")
	flag.StringVar(&opts.serial, "serial", "", "override the gateway hardware serial (default: agent auto-derives it)")
	flag.StringVar(&opts.mqttHost, "mqtt-host", "", "cloud broker host (required)")
	flag.IntVar(&opts.mqttPort, "mqtt-port", 8883, "cloud broker port")
	flag.BoolVar(&opts.mqttTLS, "mqtt-tls", true, "use TLS to the broker")
	flag.BoolVar(&opts.mqttInsecure, "mqtt-tls-insecure", false, "skip broker certificate verification (dev only)")
	flag.StringVar(&opts.logLevel, "agent-log-level", "info", "agent add-on log level (debug|info|warning|error)")
	flag.StringVar(&opts.ownerName, "owner-name", "Smart Home", "HA owner display name")
	flag.StringVar(&opts.ownerUsername, "owner-username", "admin", "HA owner username")
	flag.StringVar(&opts.ownerPassword, "owner-password", os.Getenv("SMART_ONBOARD_OWNER_PASSWORD"), "HA owner password (or SMART_ONBOARD_OWNER_PASSWORD; prompted if absent)")
	flag.StringVar(&opts.ownerLanguage, "owner-language", "en", "HA owner + instance language")
	flag.StringVar(&opts.country, "country", "SA", "ISO-3166 country code for HA core config (clears the 'no country configured' warning)")
	flag.StringVar(&opts.timezone, "timezone", "", "IANA time zone for HA core config (e.g. Asia/Riyadh; default: HA's own)")
	flag.StringVar(&opts.currency, "currency", "", "ISO-4217 currency for HA core config (e.g. SAR; default: HA's own)")
	flag.StringVar(&opts.unitSystem, "unit-system", "", "HA unit system: metric|us_customary (default: HA's own)")
	flag.StringVar(&opts.manifestPath, "manifest", "", "path to a fleet release manifest (default: the version embedded in this binary)")
	flag.DurationVar(&opts.waitCore, "wait-core", 10*time.Minute, "how long to wait for Core to come up on a fresh flash")
	flag.DurationVar(&opts.waitProvision, "wait-provision", 5*time.Minute, "how long to wait for the agent to provision")
	flag.BoolVar(&opts.assumeYes, "yes", false, "do not prompt; discover the host non-interactively and fail if a required value is missing")
	flag.BoolVar(&opts.dev, "dev", false, "developer mode: also look for a local HA (e.g. a VM on 127.0.0.1 that mDNS can't see) and default the broker to plaintext")

	flag.Parse()

	provided := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	// Developer defaults: a local dev broker is almost always plaintext. Only
	// applied when the operator did not set --mqtt-tls explicitly.
	if opts.dev && !provided["mqtt-tls"] {
		opts.mqttTLS = false
	}

	return opts, provided, nil
}

// resolveOptions turns parsed flags into a runnable configuration. When a
// terminal is attached and --yes is not set it discovers the host over mDNS and
// prompts for any unset value with sensible defaults; otherwise it fills what it
// can non-interactively. Either way it ends by validating the required inputs.
func resolveOptions(ctx context.Context, opts options, provided map[string]bool, out io.Writer) (options, error) {
	interactive := !opts.assumeYes && stdinIsInteractive()

	var p *prompter
	if interactive {
		p = newPrompter(os.Stdin, out)
	}

	if !provided["host"] {
		def := opts.host
		if opts.dev {
			def = devDefaultHost
		}

		host, err := resolveHost(ctx, def, opts.dev, interactive, p, out)
		if err != nil {
			return opts, err
		}
		opts.host = host
	}

	if interactive {
		if err := promptMissing(&opts, provided, p, out); err != nil {
			return opts, err
		}
	}

	if err := validate(opts); err != nil {
		return opts, err
	}

	if interactive {
		ok, err := confirmSummary(opts, p, out)
		if err != nil {
			return opts, err
		}
		if !ok {
			return opts, errAborted
		}
	}

	return opts, nil
}

// resolveHost picks the gateway URL: it browses the LAN over mDNS (plus a probe
// of well-known local URLs under --dev) and, when a terminal is attached,
// auto-uses a single hit, offers a picker for several, or prompts (with the
// default) when none answer. Non-interactively it uses the one hit if there is
// exactly one and otherwise keeps the default.
func resolveHost(ctx context.Context, def string, dev, interactive bool, p *prompter, out io.Writer) (string, error) {
	fmt.Fprintln(out, "Looking for a Home Assistant gateway on the network…")

	gws, err := discoverGateways(ctx, discoverTimeout)
	if err != nil {
		fmt.Fprintf(out, "  (mDNS discovery unavailable: %v)\n", err)
	}
	if dev {
		gws = dedupeGateways(append(gws, probeGateways(ctx, devCandidates)...))
	}

	switch {
	case len(gws) == 1:
		fmt.Fprintf(out, "  found %s at %s\n", gws[0].name, gws[0].url)

		return gws[0].url, nil

	case len(gws) > 1 && interactive:
		labels := make([]string, len(gws))
		for i, g := range gws {
			labels[i] = fmt.Sprintf("%s — %s", g.name, g.url)
		}
		idx, perr := p.pick("Found more than one gateway — pick which to onboard:", labels)
		if perr != nil {
			return "", perr
		}
		if idx < 0 {
			return p.text("Gateway URL", def)
		}

		return gws[idx].url, nil

	case len(gws) > 1:
		fmt.Fprintf(out, "  found %d gateways; using the first (%s). Pass --host to pick one.\n", len(gws), gws[0].url)

		return gws[0].url, nil

	case interactive:
		if !dev {
			fmt.Fprintln(out, "  none found. A local VM won't show up over mDNS — re-run with --dev, or enter the URL below.")
		} else {
			fmt.Fprintln(out, "  none found. Enter the gateway URL below.")
		}

		return p.text("Gateway URL", def)

	default:
		fmt.Fprintf(out, "  none found; using %s (pass --host to set it).\n", def)

		return def, nil
	}
}

// promptMissing fills each still-unset value from a guided prompt. Values that
// arrived by flag are left untouched; secrets already present in the environment
// are not re-asked.
func promptMissing(opts *options, provided map[string]bool, p *prompter, out io.Writer) error {
	if opts.cloudBaseURL == "" {
		v, err := p.required("Cloud base URL (e.g. https://app.example.com)")
		if err != nil {
			return err
		}
		opts.cloudBaseURL = v
	}

	if opts.factoryKey == "" {
		v, err := p.required("Factory key")
		if err != nil {
			return err
		}
		opts.factoryKey = v
	}

	if opts.mqttHost == "" {
		v, err := p.text("Broker host", defaultMQTTHost(opts.cloudBaseURL))
		if err != nil {
			return err
		}
		opts.mqttHost = v
	}

	if !provided["mqtt-port"] {
		v, err := p.intVal("Broker port", opts.mqttPort)
		if err != nil {
			return err
		}
		opts.mqttPort = v
	}

	if !provided["mqtt-tls"] {
		v, err := p.boolVal("Use TLS to the broker", opts.mqttTLS)
		if err != nil {
			return err
		}
		opts.mqttTLS = v
	}

	// Username is prompted (not just defaulted) because it is also the login
	// identity on a re-run against a device whose owner already exists.
	if !provided["owner-username"] {
		v, err := p.text("HA owner username (used to log in on a re-run)", opts.ownerUsername)
		if err != nil {
			return err
		}
		opts.ownerUsername = v
	}

	if opts.ownerPassword == "" {
		v, err := p.required("HA owner password (set on a fresh device, or the existing one on a re-run)")
		if err != nil {
			return err
		}
		opts.ownerPassword = v
	}

	if !provided["country"] {
		v, err := p.text("Country code", opts.country)
		if err != nil {
			return err
		}
		opts.country = v
	}

	// Serial is an optional override: blank means the agent auto-derives a
	// stable hardware serial on the device (Pi CPU serial / hostname), which is
	// the common case, so the prompt makes "leave blank" the obvious default.
	if !provided["serial"] {
		v, err := p.text("Gateway serial (blank = auto-derive on the device)", opts.serial)
		if err != nil {
			return err
		}
		opts.serial = v
	}

	_ = out

	return nil
}

// validate enforces the four inputs the run cannot proceed without, phrased so
// the message tells the operator both the flag and the env fallback.
func validate(o options) error {
	switch {
	case o.cloudBaseURL == "":
		return errors.New("--cloud-base-url is required")
	case o.mqttHost == "":
		return errors.New("--mqtt-host is required")
	case o.factoryKey == "":
		return errors.New("a factory key is required (--factory-key or SMART_ONBOARD_FACTORY_KEY)")
	case o.ownerPassword == "":
		return errors.New("an owner password is required (--owner-password or SMART_ONBOARD_OWNER_PASSWORD)")
	default:
		return nil
	}
}

// confirmSummary shows the resolved configuration (secrets masked) and asks to
// proceed, so a guided operator sees exactly what will run before it starts.
func confirmSummary(o options, p *prompter, out io.Writer) (bool, error) {
	fmt.Fprintln(out, "\nReady to onboard with:")
	fmt.Fprintf(out, "  gateway:  %s\n", o.host)
	fmt.Fprintf(out, "  cloud:    %s\n", o.cloudBaseURL)
	fmt.Fprintf(out, "  broker:   %s:%d (tls=%v)\n", o.mqttHost, o.mqttPort, o.mqttTLS)
	fmt.Fprintf(out, "  owner:    %s\n", o.ownerUsername)
	fmt.Fprintf(out, "  country:  %s\n", orNone(o.country))
	fmt.Fprintf(out, "  serial:   %s\n", serialOrAuto(o.serial))
	fmt.Fprintf(out, "  secrets:  factory key %s, owner password %s\n", masked(o.factoryKey), masked(o.ownerPassword))

	return p.confirm("Proceed?", true)
}

func (o options) owner() onboard.OwnerConfig {
	return onboard.OwnerConfig{
		Name:     o.ownerName,
		Username: o.ownerUsername,
		Password: o.ownerPassword,
		Language: o.ownerLanguage,
	}
}

// coreConfig is the HA core configuration the run sets after onboarding so the
// finished device is not left warning about missing location data. The instance
// language tracks the owner language.
func (o options) coreConfig() onboard.CoreConfig {
	return onboard.CoreConfig{
		Country:    o.country,
		TimeZone:   o.timezone,
		Currency:   o.currency,
		UnitSystem: o.unitSystem,
		Language:   o.ownerLanguage,
	}
}

// agentOptions builds the add-on option map matching the agent's config.yaml
// schema. The serial key is included only when overridden (the agent otherwise
// derives it from the hardware).
func (o options) agentOptions() map[string]any {
	opts := map[string]any{
		"cloud_base_url":    o.cloudBaseURL,
		"factory_key":       o.factoryKey,
		"mqtt_host":         o.mqttHost,
		"mqtt_port":         o.mqttPort,
		"mqtt_tls":          o.mqttTLS,
		"mqtt_tls_insecure": o.mqttInsecure,
		"log_level":         o.logLevel,
	}

	if o.serial != "" {
		opts["serial"] = o.serial
	}

	return opts
}

// defaultMQTTHost proposes the cloud host as the broker host, the common case
// where the cloud and broker share a domain — an empty string when the cloud URL
// is not yet a parseable URL, which just means no default is offered.
func defaultMQTTHost(cloudBaseURL string) string {
	u, err := url.Parse(cloudBaseURL)
	if err != nil {
		return ""
	}

	return u.Hostname()
}

func masked(secret string) string {
	if secret == "" {
		return "(missing)"
	}

	return "•••• set"
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}

	return s
}

// serialOrAuto renders a serial override for the summary, making clear that a
// blank value is not "missing" but "the agent derives it on the device".
func serialOrAuto(s string) string {
	if s == "" {
		return "(auto-derived on the device)"
	}

	return s
}
