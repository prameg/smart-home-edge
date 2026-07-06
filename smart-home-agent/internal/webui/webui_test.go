package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeBackend records what the handlers invoke and returns canned data, so the
// route wiring can be exercised without a live agent, broker, or Home Assistant.
type fakeBackend struct {
	status  Status
	devices []DeviceRow

	claimErr     error
	inventoryErr error
	reprovErr    error

	claimCalls     int
	inventoryCalls int
	reconcileCalls int
	reprovCalls    int
}

func (f *fakeBackend) Status(context.Context) Status       { return f.status }
func (f *fakeBackend) Devices(context.Context) []DeviceRow { return f.devices }

func (f *fakeBackend) ReissueClaimCode(context.Context) error {
	f.claimCalls++

	return f.claimErr
}

func (f *fakeBackend) RepublishInventory(context.Context) error {
	f.inventoryCalls++

	return f.inventoryErr
}

func (f *fakeBackend) ReconcileNow() { f.reconcileCalls++ }

func (f *fakeBackend) Reprovision(context.Context) error {
	f.reprovCalls++

	return f.reprovErr
}

func newTestServer(b Backend) *httptest.Server {
	s := &server{backend: b, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	return httptest.NewServer(s.routes())
}

func TestStatusEndpoint(t *testing.T) {
	fake := &fakeBackend{status: Status{UID: "gw-1", Claimed: true, DeviceCount: 3, BrokerConnected: true}}
	srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.UID != "gw-1" || !got.Claimed || got.DeviceCount != 3 || !got.BrokerConnected {
		t.Errorf("unexpected status: %+v", got)
	}
}

func TestStatusNeverLeaksSecretKeys(t *testing.T) {
	// The Status wire shape must not carry any credential-ish field, since it is
	// serialized straight to the browser.
	srv := newTestServer(&fakeBackend{status: Status{UID: "gw-1"}})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	body := strings.ToLower(string(raw))

	for _, forbidden := range []string{"password", "provision_token", "token", "secret"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("status body leaks %q: %s", forbidden, raw)
		}
	}
}

func TestDevicesEndpoint(t *testing.T) {
	fake := &fakeBackend{devices: []DeviceRow{
		{DeviceUID: "d1", EntityID: "light.a", State: "on"},
		{DeviceUID: "d2", EntityID: "switch.b", Error: "unavailable"},
	}}
	srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/devices")
	if err != nil {
		t.Fatalf("GET /api/devices: %v", err)
	}
	defer resp.Body.Close()

	var got []DeviceRow
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(got) != 2 || got[0].State != "on" || got[1].Error != "unavailable" {
		t.Errorf("unexpected devices: %+v", got)
	}
}

func TestActionRoutesDispatch(t *testing.T) {
	fake := &fakeBackend{}
	srv := newTestServer(fake)
	defer srv.Close()

	cases := []struct {
		path string
		want func() int
	}{
		{"claim-code", func() int { return fake.claimCalls }},
		{"inventory", func() int { return fake.inventoryCalls }},
		{"reconcile", func() int { return fake.reconcileCalls }},
		{"reprovision", func() int { return fake.reprovCalls }},
	}

	for _, c := range cases {
		resp, err := http.Post(srv.URL+"/api/actions/"+c.path, "application/json", nil)
		if err != nil {
			t.Fatalf("POST %s: %v", c.path, err)
		}

		var res actionResult
		_ = json.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()

		if !res.OK {
			t.Errorf("action %s: ok=false, message=%q", c.path, res.Message)
		}

		if c.want() != 1 {
			t.Errorf("action %s: backend called %d times, want 1", c.path, c.want())
		}
	}
}

func TestActionSurfacesError(t *testing.T) {
	fake := &fakeBackend{inventoryErr: errors.New("broker is not connected")}
	srv := newTestServer(fake)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/actions/inventory", "application/json", nil)
	if err != nil {
		t.Fatalf("POST inventory: %v", err)
	}
	defer resp.Body.Close()

	var res actionResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if res.OK || res.Message != "broker is not connected" {
		t.Errorf("expected ok=false with the backend error, got %+v", res)
	}
}

func TestIndexServed(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestWrongMethodIs405(t *testing.T) {
	srv := newTestServer(&fakeBackend{})
	defer srv.Close()

	// /api/status is GET-only and has no catch-all shadowing it for POST, so the
	// method-qualified route rejects a POST with 405.
	resp, err := http.Post(srv.URL+"/api/status", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
