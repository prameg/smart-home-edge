package main

import (
	"io"
	"strings"
	"testing"
)

func newTestPrompter(input string) (*prompter, *strings.Builder) {
	out := &strings.Builder{}

	return newPrompter(strings.NewReader(input), out), out
}

func TestPrompterText(t *testing.T) {
	p, _ := newTestPrompter("\ncustom\n")

	got, err := p.text("Host", "default")
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	if got != "default" {
		t.Errorf("empty line should accept the default, got %q", got)
	}

	got, err = p.text("Host", "default")
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	if got != "custom" {
		t.Errorf("a typed value should override the default, got %q", got)
	}
}

func TestPrompterRequiredLoopsUntilNonEmpty(t *testing.T) {
	p, out := newTestPrompter("\n\nfinally\n")

	got, err := p.required("Cloud URL")
	if err != nil {
		t.Fatalf("required: %v", err)
	}
	if got != "finally" {
		t.Errorf("expected the first non-empty value, got %q", got)
	}
	if strings.Count(out.String(), "(required)") != 2 {
		t.Errorf("should re-prompt on each empty line, output=%q", out.String())
	}
}

func TestPrompterIntAndBool(t *testing.T) {
	p, _ := newTestPrompter("\nnope\n1884\n")

	n, err := p.intVal("Port", 8883)
	if err != nil {
		t.Fatalf("intVal: %v", err)
	}
	if n != 8883 {
		t.Errorf("empty line should keep the default, got %d", n)
	}

	n, err = p.intVal("Port", 8883)
	if err != nil {
		t.Fatalf("intVal: %v", err)
	}
	if n != 1884 {
		t.Errorf("should reject non-numbers and take the next valid value, got %d", n)
	}

	pb, _ := newTestPrompter("\nn\n")
	b, err := pb.boolVal("TLS", true)
	if err != nil {
		t.Fatalf("boolVal: %v", err)
	}
	if !b {
		t.Errorf("empty line should keep the default true")
	}
	b, err = pb.boolVal("TLS", true)
	if err != nil {
		t.Fatalf("boolVal: %v", err)
	}
	if b {
		t.Errorf("'n' should be false")
	}
}

func TestPrompterPick(t *testing.T) {
	p, _ := newTestPrompter("2\n")
	idx, err := p.pick("Pick:", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != 1 {
		t.Errorf("choice 2 should be index 1, got %d", idx)
	}

	// The trailing "enter manually" option (len+1) returns -1.
	pm, _ := newTestPrompter("4\n")
	idx, err = pm.pick("Pick:", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if idx != -1 {
		t.Errorf("the manual-entry option should return -1, got %d", idx)
	}

	// Empty selects the first option.
	pd, _ := newTestPrompter("\n")
	idx, _ = pd.pick("Pick:", []string{"a", "b"})
	if idx != 0 {
		t.Errorf("empty should select index 0, got %d", idx)
	}
}

func TestPromptMissingFillsUnsetValues(t *testing.T) {
	// cloud URL, factory key, broker host (accept default), port (default),
	// tls (default), username (default), owner password, country (default),
	// serial (typed override).
	input := strings.Join([]string{
		"https://app.example.com", // cloud base URL
		"FK-123",                  // factory key
		"",                        // broker host -> default (app.example.com)
		"",                        // port -> default 8883
		"",                        // tls -> default true
		"",                        // username -> default admin
		"s3cret",                  // owner password
		"",                        // country -> default SA
		"SN-9",                    // serial override
	}, "\n") + "\n"

	p := newPrompter(strings.NewReader(input), io.Discard)

	opts := options{mqttPort: 8883, mqttTLS: true, ownerUsername: "admin", country: "SA"}
	if err := promptMissing(&opts, map[string]bool{}, p, io.Discard); err != nil {
		t.Fatalf("promptMissing: %v", err)
	}

	if opts.cloudBaseURL != "https://app.example.com" {
		t.Errorf("cloud URL: %q", opts.cloudBaseURL)
	}
	if opts.factoryKey != "FK-123" {
		t.Errorf("factory key: %q", opts.factoryKey)
	}
	if opts.mqttHost != "app.example.com" {
		t.Errorf("broker host should default to the cloud host, got %q", opts.mqttHost)
	}
	if opts.mqttPort != 8883 || !opts.mqttTLS {
		t.Errorf("port/tls defaults not kept: %d %v", opts.mqttPort, opts.mqttTLS)
	}
	if opts.ownerUsername != "admin" {
		t.Errorf("username default not kept: %q", opts.ownerUsername)
	}
	if opts.ownerPassword != "s3cret" {
		t.Errorf("owner password: %q", opts.ownerPassword)
	}
	if opts.country != "SA" {
		t.Errorf("country default not kept: %q", opts.country)
	}
	if opts.serial != "SN-9" {
		t.Errorf("serial override should be captured, got %q", opts.serial)
	}
}

func TestPromptMissingSkipsProvidedAndEnvValues(t *testing.T) {
	// A factory key already present (e.g. from env) and a flag-provided port
	// must not be re-asked; only the still-empty required values are prompted.
	input := strings.Join([]string{
		"https://c.example.com", // cloud base URL
		"broker.example.com",    // broker host
		"pw",                    // owner password
	}, "\n") + "\n"

	p := newPrompter(strings.NewReader(input), io.Discard)

	opts := options{factoryKey: "from-env", mqttPort: 1884, mqttTLS: false, ownerUsername: "admin", country: "SA", serial: "SN-flag"}
	provided := map[string]bool{"mqtt-port": true, "mqtt-tls": true, "owner-username": true, "country": true, "serial": true}

	if err := promptMissing(&opts, provided, p, io.Discard); err != nil {
		t.Fatalf("promptMissing: %v", err)
	}

	if opts.factoryKey != "from-env" {
		t.Errorf("an existing factory key must not be overwritten, got %q", opts.factoryKey)
	}
	if opts.serial != "SN-flag" {
		t.Errorf("a flag-provided serial must not be re-asked, got %q", opts.serial)
	}
	if opts.mqttPort != 1884 || opts.mqttTLS {
		t.Errorf("flag-provided broker settings must be left alone: %d %v", opts.mqttPort, opts.mqttTLS)
	}
	if opts.mqttHost != "broker.example.com" || opts.cloudBaseURL != "https://c.example.com" || opts.ownerPassword != "pw" {
		t.Errorf("required values not filled: %+v", opts)
	}
}
