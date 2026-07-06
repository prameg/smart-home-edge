package main

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	full := options{cloudBaseURL: "https://c", mqttHost: "b", factoryKey: "k", ownerPassword: "pw"}
	if err := validate(full); err != nil {
		t.Errorf("a complete config should validate, got %v", err)
	}

	cases := map[string]options{
		"missing cloud":    {mqttHost: "b", factoryKey: "k", ownerPassword: "pw"},
		"missing broker":   {cloudBaseURL: "https://c", factoryKey: "k", ownerPassword: "pw"},
		"missing factory":  {cloudBaseURL: "https://c", mqttHost: "b", ownerPassword: "pw"},
		"missing password": {cloudBaseURL: "https://c", mqttHost: "b", factoryKey: "k"},
	}
	for name, o := range cases {
		if err := validate(o); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestDefaultMQTTHost(t *testing.T) {
	if got := defaultMQTTHost("https://app.example.com:443/path"); got != "app.example.com" {
		t.Errorf("expected the host without scheme/port, got %q", got)
	}
	if got := defaultMQTTHost(""); got != "" {
		t.Errorf("an empty cloud URL offers no default, got %q", got)
	}
}

func TestMaskedAndOrNone(t *testing.T) {
	if masked("") != "(missing)" {
		t.Errorf("empty secret should read as missing")
	}
	if masked("secret") == "secret" {
		t.Errorf("a set secret must never be echoed back")
	}
	if orNone("") != "(unset)" || orNone("SA") != "SA" {
		t.Errorf("orNone rendering wrong")
	}
}

// Non-interactively (the test harness has no TTY on stdin), resolveOptions must
// not prompt: with the host provided it neither discovers nor blocks, and simply
// validates. A complete config passes; an incomplete one errors.
func TestResolveOptionsNonInteractive(t *testing.T) {
	provided := map[string]bool{"host": true}

	complete := options{host: "http://ha.local:8123", cloudBaseURL: "https://c", mqttHost: "b", factoryKey: "k", ownerPassword: "pw", assumeYes: true}
	got, err := resolveOptions(context.Background(), complete, provided, io.Discard)
	if err != nil {
		t.Fatalf("complete config should resolve: %v", err)
	}
	if got.host != "http://ha.local:8123" {
		t.Errorf("provided host should be untouched, got %q", got.host)
	}

	incomplete := options{host: "http://ha.local:8123", cloudBaseURL: "https://c", assumeYes: true}
	if _, err := resolveOptions(context.Background(), incomplete, provided, io.Discard); err == nil {
		t.Error("an incomplete config must fail validation non-interactively")
	}
}

// A safety net for the summary wording: masked secrets must never leak the value.
func TestConfirmSummaryMasksSecrets(t *testing.T) {
	out := &strings.Builder{}
	p := newPrompter(strings.NewReader("y\n"), out)

	o := options{host: "h", cloudBaseURL: "c", mqttHost: "b", mqttPort: 8883, mqttTLS: true, ownerUsername: "admin", country: "SA", factoryKey: "SUPERSECRET", ownerPassword: "PWSECRET"}
	if _, err := confirmSummary(o, p, out); err != nil {
		t.Fatalf("confirmSummary: %v", err)
	}
	if strings.Contains(out.String(), "SUPERSECRET") || strings.Contains(out.String(), "PWSECRET") {
		t.Errorf("summary must not print secret values:\n%s", out.String())
	}
}
