package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeOptions renders an options.json into a fresh data dir and points
// AGENT_DATA_DIR at it, mirroring how Supervisor feeds the add-on.
func writeOptions(t *testing.T, body string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "options.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write options: %v", err)
	}

	t.Setenv("AGENT_DATA_DIR", dir)

	return dir
}

func TestOptionsFileDisablesTLS(t *testing.T) {
	// The env default for MQTT_TLS is true; an explicit `false` in the options
	// file must win (the bug this guards: `opts || env` made false unreachable).
	writeOptions(t, `{"mqtt_host":"broker.local","mqtt_tls":false,"mqtt_tls_insecure":true}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MQTTTLS {
		t.Error("expected mqtt_tls=false from options to disable TLS")
	}

	if !cfg.MQTTTLSInsecure {
		t.Error("expected mqtt_tls_insecure=true from options to enable insecure")
	}
}

func TestBooleanFallsBackToEnvWhenAbsent(t *testing.T) {
	// No options.json: the env fallback (default true) applies.
	dir := t.TempDir()
	t.Setenv("AGENT_DATA_DIR", dir)
	t.Setenv("MQTT_HOST", "broker.local")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.MQTTTLS {
		t.Error("expected mqtt_tls to default to true via env fallback when unset")
	}
}

func TestConfigDirDefaultsToDataDir(t *testing.T) {
	dir := writeOptions(t, `{"mqtt_host":"broker.local"}`)
	os.Unsetenv("AGENT_CONFIG_DIR")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ConfigDir != dir {
		t.Errorf("ConfigDir = %q, want data dir %q when AGENT_CONFIG_DIR unset", cfg.ConfigDir, dir)
	}
}

func TestConfigDirFromEnv(t *testing.T) {
	writeOptions(t, `{"mqtt_host":"broker.local"}`)
	t.Setenv("AGENT_CONFIG_DIR", "/config")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ConfigDir != "/config" {
		t.Errorf("ConfigDir = %q, want /config", cfg.ConfigDir)
	}
}
