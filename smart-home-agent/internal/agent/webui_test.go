package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/entitymap"
	"github.com/smart-home/edge/agent/internal/ha"
	"github.com/smart-home/edge/agent/internal/provision"
)

func testAgent(claimed bool) *Agent {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Point the HA REST base at a closed port so Version() fails fast (returns
	// "") instead of blocking the test on a dial timeout.
	cfg := &config.Config{
		AgentVersion:  "1.2.3",
		CloudBaseURL:  "http://cloud.example",
		MQTTHost:      "broker.example",
		MQTTPort:      8883,
		MQTTTLS:       true,
		HARestBaseURL: "http://127.0.0.1:1/api",
	}

	return &Agent{
		cfg: cfg,
		log: log,
		ha:  ha.New(cfg, log),
		creds: &provision.Credentials{
			UID:                "gw-uid-1",
			Serial:             "serial-xyz",
			ClaimStatus:        "unclaimed",
			ClaimCode:          "ABCD-1234",
			ClaimCodeExpiresAt: "2099-01-01T00:00:00Z",
			MQTTUsername:       "mqtt-user",
			MQTTPassword:       "super-secret-password",
			ProvisionToken:     "provision-token-secret",
		},
		emap:          entitymap.New([]entitymap.Entry{{DeviceUID: "d1", EntityID: "light.a"}}),
		claimed:       claimed,
		configVersion: 5,
	}
}

func TestStatusReportsAgentState(t *testing.T) {
	s := testAgent(false).Status(context.Background())

	if s.UID != "gw-uid-1" || s.Serial != "serial-xyz" || s.AgentVersion != "1.2.3" {
		t.Errorf("unexpected identity fields: %+v", s)
	}

	if s.Claimed || s.ClaimStatus != "unclaimed" {
		t.Errorf("expected unclaimed, got claimed=%v status=%q", s.Claimed, s.ClaimStatus)
	}

	if s.ClaimCode != "ABCD-1234" {
		t.Errorf("expected claim code surfaced while unclaimed, got %q", s.ClaimCode)
	}

	if s.BrokerConnected {
		t.Error("expected broker_connected=false when no broker is set")
	}

	if s.ConfigVersion != 5 || s.DeviceCount != 1 {
		t.Errorf("expected config_version=5 device_count=1, got version=%d count=%d", s.ConfigVersion, s.DeviceCount)
	}

	if s.MQTTHost != "broker.example" || s.MQTTPort != 8883 || !s.MQTTTLS {
		t.Errorf("unexpected broker target: %+v", s)
	}
}

func TestStatusHidesClaimCodeWhenClaimed(t *testing.T) {
	s := testAgent(true).Status(context.Background())

	if !s.Claimed {
		t.Fatal("expected claimed=true")
	}

	if s.ClaimCode != "" || s.ClaimCodeExpiresAt != "" {
		t.Errorf("expected no claim code once claimed, got code=%q expires=%q", s.ClaimCode, s.ClaimCodeExpiresAt)
	}
}

func TestStatusNeverLeaksCredentials(t *testing.T) {
	s := testAgent(false).Status(context.Background())

	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}

	body := string(raw)
	for _, secret := range []string{"super-secret-password", "provision-token-secret", "mqtt-user"} {
		if strings.Contains(body, secret) {
			t.Errorf("status leaks credential %q: %s", secret, body)
		}
	}
}
