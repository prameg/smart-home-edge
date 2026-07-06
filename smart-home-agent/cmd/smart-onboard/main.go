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
// Usage:
//
//	smart-onboard \
//	  --host http://homeassistant.local:8123 \
//	  --cloud-base-url https://app.example.com \
//	  --factory-key <key> \
//	  --mqtt-host broker.example.com --mqtt-port 8883 --mqtt-tls \
//	  --owner-password <password>
//
// Secrets can be supplied by flag, by environment variable
// (SMART_ONBOARD_FACTORY_KEY, SMART_ONBOARD_OWNER_PASSWORD), or interactively.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/onboard"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nonboarding failed:", err)

		var stepErr *onboard.StepError
		if errors.As(err, &stepErr) {
			fmt.Fprintf(os.Stderr, "the failure was in step %q — fix the cause above and re-run the same command to resume.\n", stepErr.Step)
		}

		os.Exit(1)
	}
}

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
	manifestPath  string
	waitCore      time.Duration
	waitProvision time.Duration
	assumeYes     bool
}

func run() error {
	opts, err := parseFlags()
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
		AgentOptions: opts.agentOptions(),
		Timeouts: onboard.Timeouts{
			WaitCore:      opts.waitCore,
			WaitProvision: opts.waitProvision,
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	engine := onboard.NewEngine(onboard.BuildSteps(reporter), reporter)
	if err := engine.Run(ctx, st); err != nil {
		return err
	}

	printDone(os.Stdout, st)

	return nil
}

func parseFlags() (options, error) {
	var opts options

	flag.StringVar(&opts.host, "host", "http://homeassistant.local:8123", "gateway base URL (scheme://host:port)")
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
	flag.StringVar(&opts.ownerLanguage, "owner-language", "en", "HA owner language")
	flag.StringVar(&opts.manifestPath, "manifest", "", "path to a fleet release manifest (default: the version embedded in this binary)")
	flag.DurationVar(&opts.waitCore, "wait-core", 10*time.Minute, "how long to wait for Core to come up on a fresh flash")
	flag.DurationVar(&opts.waitProvision, "wait-provision", 5*time.Minute, "how long to wait for the agent to provision")
	flag.BoolVar(&opts.assumeYes, "yes", false, "do not prompt; fail if a required secret is missing")

	flag.Parse()

	if opts.cloudBaseURL == "" {
		return opts, fmt.Errorf("--cloud-base-url is required")
	}
	if opts.mqttHost == "" {
		return opts, fmt.Errorf("--mqtt-host is required")
	}

	if opts.factoryKey == "" {
		v, err := prompt(opts.assumeYes, "Factory key: ")
		if err != nil {
			return opts, err
		}
		opts.factoryKey = v
	}
	if opts.ownerPassword == "" {
		v, err := prompt(opts.assumeYes, "HA owner password to set: ")
		if err != nil {
			return opts, err
		}
		opts.ownerPassword = v
	}

	if opts.factoryKey == "" {
		return opts, fmt.Errorf("a factory key is required")
	}
	if opts.ownerPassword == "" {
		return opts, fmt.Errorf("an owner password is required")
	}

	return opts, nil
}

func (o options) owner() onboard.OwnerConfig {
	return onboard.OwnerConfig{
		Name:     o.ownerName,
		Username: o.ownerUsername,
		Password: o.ownerPassword,
		Language: o.ownerLanguage,
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

// prompt reads one line from stdin. In --yes mode there is no interactive
// fallback, so a missing secret is a hard error instead of a hang.
func prompt(assumeYes bool, label string) (string, error) {
	if assumeYes {
		return "", fmt.Errorf("missing required input for %q and --yes disables prompting", strings.TrimSpace(label))
	}

	fmt.Print(label)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}

	return strings.TrimSpace(line), nil
}
