package provision

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smart-home/edge/agent/internal/config"
)

func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	return New(&config.Config{
		CloudBaseURL: baseURL,
		FactoryKey:   "factory-key",
		Serial:       "sn-123",
		DataDir:      t.TempDir(),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// On first boot (no stored token) enrollment sends the factory key and no
// provision token, and persists the token + claim code the cloud returns.
func TestRecoverEnrollsAndPersistsToken(t *testing.T) {
	var gotAuth, gotBody string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uid":                   "uid-1",
			"claim_status":          "unclaimed",
			"mqtt":                  map[string]string{"username": "gw_a", "password": "pw-1"},
			"provision_token":       "tok-1",
			"claim_code":            "ABCD-EFGH",
			"claim_code_expires_at": "2099-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)

	creds, err := c.Recover(context.Background(), Metadata{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if gotAuth != "Bearer factory-key" {
		t.Errorf("expected factory key bearer, got %q", gotAuth)
	}

	var payload map[string]string
	_ = json.Unmarshal([]byte(gotBody), &payload)
	if _, ok := payload["provision_token"]; ok {
		t.Error("first-boot enrollment must not send a provision token")
	}

	if creds.ProvisionToken != "tok-1" || creds.ClaimCode != "ABCD-EFGH" {
		t.Errorf("unexpected persisted creds: %+v", creds)
	}

	// The token must round-trip to disk for the next recovery.
	reloaded, err := c.load()
	if err != nil || reloaded.ProvisionToken != "tok-1" {
		t.Errorf("provision token not persisted: %+v (err %v)", reloaded, err)
	}
}

// A recovery with a stored token sends it and keeps the token/claim code the
// cloud does not resend, while adopting the rotated password.
func TestRecoverSendsAndPreservesToken(t *testing.T) {
	c := testClient(t, "")

	// Seed an already-provisioned unit.
	if err := c.save(&Credentials{
		UID:            "uid-1",
		MQTTUsername:   "gw_a",
		MQTTPassword:   "old-pw",
		ProvisionToken: "tok-1",
		ClaimCode:      "ABCD-EFGH",
		Serial:         "sn-123",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var sentToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		sentToken = payload["provision_token"]

		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uid":          "uid-1",
			"claim_status": "unclaimed",
			"mqtt":         map[string]string{"username": "gw_a", "password": "new-pw"},
			// provision_token + claim_code omitted (not resent on token recovery).
		})
	}))
	defer srv.Close()

	c.cfg.CloudBaseURL = srv.URL

	creds, err := c.Recover(context.Background(), Metadata{})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if sentToken != "tok-1" {
		t.Errorf("expected stored provision token sent, got %q", sentToken)
	}

	if creds.MQTTPassword != "new-pw" {
		t.Errorf("expected rotated password, got %q", creds.MQTTPassword)
	}

	if creds.ProvisionToken != "tok-1" {
		t.Errorf("expected preserved provision token, got %q", creds.ProvisionToken)
	}

	if creds.ClaimCode != "ABCD-EFGH" {
		t.Errorf("expected preserved claim code, got %q", creds.ClaimCode)
	}
}

// A structured cloud rejection surfaces as *Error with the machine code, and
// operator-actionable codes report NeedsOperator.
func TestRecoverDecodesStructuredError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "recovery_not_authorized",
			"message": "needs a provision token or an authorized window",
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)

	_, err := c.Recover(context.Background(), Metadata{})

	provErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}

	if provErr.Code != "recovery_not_authorized" || !provErr.NeedsOperator() {
		t.Errorf("unexpected error: %+v (needsOperator=%v)", provErr, provErr.NeedsOperator())
	}
}

// A suspended gateway is an operator-actionable rejection: retrying will not
// clear it until an admin restores the unit.
func TestSuspendedGatewayNeedsOperator(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":   "gateway_suspended",
			"message": "this gateway is suspended",
		})
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)

	_, err := c.Recover(context.Background(), Metadata{})

	provErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}

	if provErr.Code != "gateway_suspended" || !provErr.NeedsOperator() {
		t.Errorf("unexpected error: %+v (needsOperator=%v)", provErr, provErr.NeedsOperator())
	}
}

// ReissueClaimCode uses the stored provision token and persists the new code.
func TestReissueClaimCode(t *testing.T) {
	c := testClient(t, "")

	if err := c.save(&Credentials{
		UID:            "uid-1",
		MQTTUsername:   "gw_a",
		MQTTPassword:   "pw",
		ProvisionToken: "tok-1",
		Serial:         "sn-123",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["provision_token"] != "tok-1" || payload["serial"] != "sn-123" {
			t.Errorf("unexpected reissue payload: %+v", payload)
		}

		_ = json.NewEncoder(w).Encode(map[string]string{
			"claim_code":            "WXYZ-2345",
			"claim_code_expires_at": "2099-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	c.cfg.CloudBaseURL = srv.URL

	creds, err := c.ReissueClaimCode(context.Background())
	if err != nil {
		t.Fatalf("ReissueClaimCode: %v", err)
	}

	if creds.ClaimCode != "WXYZ-2345" {
		t.Errorf("expected reissued code, got %q", creds.ClaimCode)
	}
}

// Without a stored provision token there is nothing to authenticate a reissue.
func TestReissueClaimCodeRequiresToken(t *testing.T) {
	c := testClient(t, "http://unused")

	if err := c.save(&Credentials{UID: "uid-1", MQTTUsername: "gw_a", MQTTPassword: "pw", Serial: "sn-123"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := c.ReissueClaimCode(context.Background()); err == nil {
		t.Error("expected an error reissuing without a stored provision token")
	}
}
