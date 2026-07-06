// Package config loads the agent's runtime configuration from the two sources a
// Home Assistant add-on has: the Supervisor-rendered options file
// (/data/options.json, shaped by the add-on's config.yaml schema) and the
// Supervisor-injected environment (SUPERVISOR_TOKEN). Every value falls back to
// an environment variable so the agent is runnable outside HAOS for local
// iteration (see DOCS.md).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// CloudBaseURL is the Laravel cloud origin, e.g. https://app.example.com.
	CloudBaseURL string
	// FactoryKey is the provisioning shared secret (SMART_HOME_PROVISIONING_
	// FACTORY_KEY on the cloud). Presented as a bearer token on first boot /
	// recovery. Injected at the factory; on a scripted-onboarding unit it is the
	// same key baked into the add-on options.
	FactoryKey string
	// Serial is the immutable hardware identity the provisioning upsert keys on.
	// When blank it is derived from the machine (see resolveSerial).
	Serial string

	// Broker connection. The username/password come from provisioning, not here.
	MQTTHost        string
	MQTTPort        int
	MQTTTLS         bool
	MQTTTLSInsecure bool // dev only: skip broker cert verification

	// Supervisor-proxied Home Assistant API. In an add-on the host is literally
	// "supervisor" and the token is injected; overridable for local dev.
	SupervisorToken string
	HARestBaseURL   string // e.g. http://supervisor/core/api
	HAWebsocketURL  string // e.g. ws://supervisor/core/websocket

	// DataDir is the add-on's persistent, private volume (/data in HAOS).
	// Credentials and applied-version state live here so they survive restarts
	// and are never exposed to the user.
	DataDir string

	// ConfigDir is the user-facing config folder (addon_config, mounted at
	// /config in HAOS; host path /addon_configs/<repo>_<slug>/). The optional
	// entity-map override lives here so a tester can edit it without a rebuild.
	// Outside HAOS it falls back to DataDir so local runs keep working.
	ConfigDir string

	// AgentVersion is stamped into provisioning metadata; set via -ldflags at
	// build time, defaults to "dev".
	AgentVersion string

	// WebUIPort is the TCP port the embedded status/actions web server binds.
	// In an add-on it must match `ingress_port` in config.yaml so Supervisor's
	// ingress proxy can reach the sidebar panel; overridable via WEBUI_PORT for
	// local runs outside HAOS.
	WebUIPort int

	LogLevel string
}

// options is the shape Supervisor writes to /data/options.json from the add-on
// schema in config.yaml. Booleans are pointers so we can tell "user set it to
// false" apart from "absent" (running outside HAOS, no options file) — a plain
// bool would make an explicit `false` indistinguishable from the zero value and
// let the env default silently win (e.g. mqtt_tls could never be turned off).
type options struct {
	CloudBaseURL    string `json:"cloud_base_url"`
	FactoryKey      string `json:"factory_key"`
	Serial          string `json:"serial"`
	MQTTHost        string `json:"mqtt_host"`
	MQTTPort        int    `json:"mqtt_port"`
	MQTTTLS         *bool  `json:"mqtt_tls"`
	MQTTTLSInsecure *bool  `json:"mqtt_tls_insecure"`
	LogLevel        string `json:"log_level"`
}

// AgentVersion is overridden at build time via
// -ldflags "-X .../config.AgentVersion=x.y.z".
var AgentVersion = "dev"

// Load resolves configuration from the options file (if present) with an
// environment-variable fallback for every field, then validates the essentials.
func Load() (*Config, error) {
	dataDir := envOr("AGENT_DATA_DIR", "/data")

	opts := loadOptions(dataDir + "/options.json")

	cfg := &Config{
		CloudBaseURL:    firstNonEmpty(opts.CloudBaseURL, os.Getenv("CLOUD_BASE_URL")),
		FactoryKey:      firstNonEmpty(opts.FactoryKey, os.Getenv("FACTORY_KEY")),
		Serial:          firstNonEmpty(opts.Serial, os.Getenv("GATEWAY_SERIAL")),
		MQTTHost:        firstNonEmpty(opts.MQTTHost, os.Getenv("MQTT_HOST")),
		MQTTPort:        firstPositive(opts.MQTTPort, envInt("MQTT_PORT", 8883)),
		MQTTTLS:         boolPref(opts.MQTTTLS, envBool("MQTT_TLS", true)),
		MQTTTLSInsecure: boolPref(opts.MQTTTLSInsecure, envBool("MQTT_TLS_INSECURE", false)),
		SupervisorToken: os.Getenv("SUPERVISOR_TOKEN"),
		HARestBaseURL:   envOr("HA_REST_BASE_URL", "http://supervisor/core/api"),
		HAWebsocketURL:  envOr("HA_WEBSOCKET_URL", "ws://supervisor/core/websocket"),
		DataDir:         dataDir,
		ConfigDir:       envOr("AGENT_CONFIG_DIR", dataDir),
		AgentVersion:    AgentVersion,
		WebUIPort:       envInt("WEBUI_PORT", 8099),
		LogLevel:        firstNonEmpty(opts.LogLevel, envOr("LOG_LEVEL", "info")),
	}

	if cfg.Serial == "" {
		cfg.Serial = resolveSerial()
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// CloudURL joins the cloud base URL with an API path.
func (c *Config) CloudURL(path string) string {
	return strings.TrimRight(c.CloudBaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// validate only hard-requires what is needed to even connect to a broker. The
// cloud URL + factory key are checked at provisioning time (provision.Recover),
// because a local test with pre-seeded credentials never provisions and should
// run without them. A missing SUPERVISOR_TOKEN is left to surface as an HA
// bridge error so an MQTT-only smoke test can still start.
func (c *Config) validate() error {
	if c.MQTTHost == "" {
		return fmt.Errorf("config: missing required setting: mqtt_host")
	}

	return nil
}

func loadOptions(path string) options {
	var opts options

	raw, err := os.ReadFile(path)
	if err != nil {
		return opts
	}

	_ = json.Unmarshal(raw, &opts)

	return opts
}

// resolveSerial derives a stable hardware identity when none is configured. On a
// Raspberry Pi the CPU serial is stable across reflashes; the hostname is the
// portable fallback for non-Pi / dev environments.
func resolveSerial() string {
	if serial := readPiSerial(); serial != "" {
		return "pi-" + serial
	}

	if host, err := os.Hostname(); err == nil && host != "" {
		return "host-" + host
	}

	return "unknown-serial"
}

func readPiSerial() string {
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "Serial") {
			if _, value, ok := strings.Cut(line, ":"); ok {
				return strings.TrimSpace(value)
			}
		}
	}

	return ""
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}

	return 0
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}

	return fallback
}

// boolPref returns the options value when it was present (non-nil), otherwise the
// environment fallback. This keeps the options file authoritative in an add-on
// (where every schema key is always written, including explicit `false`) while
// preserving the env fallback for local runs that have no options file.
func boolPref(opt *bool, fallback bool) bool {
	if opt != nil {
		return *opt
	}

	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}

	return fallback
}
