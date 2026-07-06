package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/smart-home/edge/agent/fleet"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return New(srv.URL, testLog())
}

func TestWaitForCoreReturnsWhenReachable(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))

	if err := c.WaitForCore(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("WaitForCore: %v", err)
	}
}

// A fully-onboarded device (onboarding views removed → 404) still counts as
// "usably up": WaitForCore must return so a re-run against a finished device can
// proceed to log in.
func TestWaitForCoreReadyOnOnboardedDevice(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))

	if err := c.WaitForCore(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("WaitForCore should treat 404 (onboarded) as ready: %v", err)
	}
}

// The regression this guards: a Core that has bound its socket but is still
// bringing up the onboarding/auth subsystem answers /api/onboarding with a
// transient 401. That is NOT "ready" — WaitForCore must keep waiting (and time
// out) rather than declaring the device up and letting the next step fail on a
// spurious 401.
func TestWaitForCoreDoesNotAcceptWarmupStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable} {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		err := c.WaitForCore(context.Background(), 200*time.Millisecond)
		if err == nil {
			t.Fatalf("status %d: WaitForCore should not treat a warm-up status as ready", status)
		}
		if !strings.Contains(err.Error(), "still initializing") {
			t.Errorf("status %d: expected a 'still initializing' message, got %v", status, err)
		}
	}
}

func TestOnboardingStatus(t *testing.T) {
	t.Run("user not done", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"step":"user","done":false},{"step":"core_config","done":false}]`))
		}))

		st, err := c.OnboardingStatus(context.Background())
		if err != nil {
			t.Fatalf("OnboardingStatus: %v", err)
		}
		if st.UserDone || st.AllDone {
			t.Errorf("expected nothing done, got %+v", st)
		}
	})

	t.Run("user done, wizard incomplete", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`[{"step":"user","done":true},{"step":"analytics","done":false}]`))
		}))

		st, _ := c.OnboardingStatus(context.Background())
		if !st.UserDone || st.AllDone {
			t.Errorf("expected user done but not all done, got %+v", st)
		}
	})

	t.Run("endpoint gone means fully onboarded", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))

		st, err := c.OnboardingStatus(context.Background())
		if err != nil {
			t.Fatalf("OnboardingStatus: %v", err)
		}
		if !st.UserDone || !st.AllDone {
			t.Errorf("404 should map to fully onboarded, got %+v", st)
		}
	})

	t.Run("401 during warm-up is a clear, actionable error", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))

		_, err := c.OnboardingStatus(context.Background())
		if err == nil || !strings.Contains(err.Error(), "still initializing") {
			t.Errorf("401 should surface a 'still initializing' hint, got %v", err)
		}
	})
}

func TestCreateOwnerReturnsAuthCode(t *testing.T) {
	var gotBody string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/onboarding/users" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		_, _ = w.Write([]byte(`{"auth_code":"AC-123"}`))
	}))

	code, err := c.CreateOwner(context.Background(), OwnerConfig{Name: "N", Username: "admin", Password: "pw"})
	if err != nil {
		t.Fatalf("CreateOwner: %v", err)
	}
	if code != "AC-123" {
		t.Errorf("expected auth code AC-123, got %q", code)
	}
	if !strings.Contains(gotBody, `"username":"admin"`) || !strings.Contains(gotBody, `"client_id"`) {
		t.Errorf("owner request missing fields: %s", gotBody)
	}
}

func TestExchangeCodePostsForm(t *testing.T) {
	var ct, form string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		form = string(raw)
		_, _ = w.Write([]byte(`{"access_token":"AT-9","refresh_token":"RT","expires_in":1800}`))
	}))

	token, err := c.ExchangeCode(context.Background(), "AC-123")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if token != "AT-9" {
		t.Errorf("expected access token AT-9, got %q", token)
	}
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("expected form content type, got %q", ct)
	}
	if !strings.Contains(form, "grant_type=authorization_code") || !strings.Contains(form, "code=AC-123") {
		t.Errorf("unexpected form body: %s", form)
	}
}

func TestLoginFlow(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/auth/login_flow":
				_, _ = w.Write([]byte(`{"flow_id":"F1","type":"form","step_id":"init"}`))
			case strings.HasPrefix(r.URL.Path, "/auth/login_flow/F1"):
				_, _ = w.Write([]byte(`{"type":"create_entry","result":"AC-777"}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		}))

		code, err := c.Login(context.Background(), "admin", "pw")
		if err != nil {
			t.Fatalf("Login: %v", err)
		}
		if code != "AC-777" {
			t.Errorf("expected AC-777, got %q", code)
		}
	})

	t.Run("bad credentials", func(t *testing.T) {
		c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/auth/login_flow" {
				_, _ = w.Write([]byte(`{"flow_id":"F1","type":"form","step_id":"init"}`))

				return
			}
			_, _ = w.Write([]byte(`{"type":"form","errors":{"base":"invalid_auth"}}`))
		}))

		if _, err := c.Login(context.Background(), "admin", "wrong"); err == nil {
			t.Error("expected an error on invalid credentials")
		}
	})
}

// newSupervisorClient wires a client to a test server that speaks the HA
// websocket auth handshake and dispatches supervisor/api commands to dispatch.
// A token is pre-set because supervisor/api authenticates the socket.
func newSupervisorClient(t *testing.T, dispatch func(endpoint, method string, data json.RawMessage) (any, error)) *Client {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/websocket", func(w http.ResponseWriter, r *http.Request) {
		serveHAWebSocket(w, r, dispatch)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.URL, testLog())
	c.SetToken("test-token")

	return c
}

func TestStoreRepositories(t *testing.T) {
	c := newSupervisorClient(t, func(endpoint, method string, _ json.RawMessage) (any, error) {
		if endpoint != "/store/repositories" || method != "get" {
			t.Errorf("unexpected call %s %s", method, endpoint)
		}

		return []map[string]string{{"slug": "core", "url": "https://a.test"}, {"slug": "x", "url": "https://b.test"}}, nil
	})

	repos, err := c.StoreRepositories(context.Background())
	if err != nil {
		t.Fatalf("StoreRepositories: %v", err)
	}
	if len(repos) != 2 || repos[0] != "https://a.test" {
		t.Errorf("unexpected repos: %v", repos)
	}
}

func TestResolveAddonSlug(t *testing.T) {
	c := newSupervisorClient(t, func(endpoint, _ string, _ json.RawMessage) (any, error) {
		if endpoint != "/store" {
			t.Errorf("unexpected endpoint %s", endpoint)
		}

		return map[string]any{"addons": []map[string]string{{"slug": "core_mosquitto"}, {"slug": "a1b2c3_smart_home_agent"}}}, nil
	})

	slug, found, err := c.ResolveAddonSlug(context.Background(), fleet.Addon{Match: "smart_home_agent"})
	if err != nil || !found {
		t.Fatalf("ResolveAddonSlug: found=%v err=%v", found, err)
	}
	if slug != "a1b2c3_smart_home_agent" {
		t.Errorf("expected repo-prefixed slug, got %q", slug)
	}

	// An exact configured slug bypasses the store entirely (no supervisor call).
	exact, found, err := c.ResolveAddonSlug(context.Background(), fleet.Addon{Match: "mosquitto", Slug: "core_mosquitto"})
	if err != nil || !found || exact != "core_mosquitto" {
		t.Errorf("exact slug should resolve directly: %q found=%v err=%v", exact, found, err)
	}
}

func TestAddonInfo(t *testing.T) {
	t.Run("installed", func(t *testing.T) {
		c := newSupervisorClient(t, func(string, string, json.RawMessage) (any, error) {
			return map[string]any{"version": "1.2.3", "state": "started", "auto_update": true, "options": map[string]any{"log_level": "info"}}, nil
		})

		info, err := c.AddonInfo(context.Background(), "slug")
		if err != nil {
			t.Fatalf("AddonInfo: %v", err)
		}
		if !info.Installed || info.Version != "1.2.3" || info.State != "started" || !info.AutoUpdate {
			t.Errorf("unexpected info: %+v", info)
		}
		if info.Options["log_level"] != "info" {
			t.Errorf("options not parsed: %+v", info.Options)
		}
	})

	t.Run("not installed (supervisor error)", func(t *testing.T) {
		c := newSupervisorClient(t, func(string, string, json.RawMessage) (any, error) {
			return nil, fmt.Errorf("Addon does not exist")
		})

		info, err := c.AddonInfo(context.Background(), "slug")
		if err != nil {
			t.Fatalf("AddonInfo: %v", err)
		}
		if info.Installed {
			t.Errorf("a supervisor error should mean not installed, got %+v", info)
		}
	})
}

func TestSetAddonOptionsWrapsPayload(t *testing.T) {
	bodyCh := make(chan string, 1)
	c := newSupervisorClient(t, func(_, _ string, data json.RawMessage) (any, error) {
		bodyCh <- string(data)

		return nil, nil
	})

	if err := c.SetAddonOptions(context.Background(), "slug", map[string]any{"factory_key": "k"}); err != nil {
		t.Fatalf("SetAddonOptions: %v", err)
	}

	body := <-bodyCh
	if !strings.Contains(body, `"options"`) || !strings.Contains(body, `"factory_key":"k"`) {
		t.Errorf("options not wrapped under an \"options\" key: %s", body)
	}
}

func TestSupervisorErrorSurfacesMessage(t *testing.T) {
	c := newSupervisorClient(t, func(string, string, json.RawMessage) (any, error) {
		return nil, fmt.Errorf("addon is not installed")
	})

	err := c.StartAddon(context.Background(), "slug")
	if err == nil || !strings.Contains(err.Error(), "addon is not installed") {
		t.Errorf("expected the supervisor error message surfaced, got %v", err)
	}
}

func TestClaimInfoFromLogs(t *testing.T) {
	logs := `time=2026-07-06T10:00:00Z level=INFO msg=provisioned uid=9f8c2b10-0000-4a1b-9c3d-abcdef012345 claim_status=unclaimed mqtt_username=gw_x
time=2026-07-06T10:00:01Z level=WARN msg="gateway is UNCLAIMED — enter this claim code to add it to a home" claim_code=WXYZ-2345`

	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/logs") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(logs))
	}))

	info, err := c.ClaimInfo(context.Background(), "a1b2_smart_home_agent")
	if err != nil {
		t.Fatalf("ClaimInfo: %v", err)
	}
	if info.UID != "9f8c2b10-0000-4a1b-9c3d-abcdef012345" {
		t.Errorf("uid not parsed: %+v", info)
	}
	if info.Claimed {
		t.Errorf("should be unclaimed: %+v", info)
	}
	if info.ClaimCode != "WXYZ-2345" {
		t.Errorf("claim code not parsed: %+v", info)
	}
}

func TestClaimInfoFallsBackToNotification(t *testing.T) {
	// Logs carry the uid but no claim code (it scrolled off); the code must come
	// from the persistent notification instead.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/logs"):
			_, _ = w.Write([]byte(`msg=provisioned uid=uid-9 claim_status=unclaimed`))
		case r.URL.Path == "/api/states/persistent_notification.smart_home_claim_code":
			_, _ = w.Write([]byte(`{"attributes":{"message":"Open the Smart Home app and enter claim code **QRST-6789** to add this gateway to a home."}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	info, err := c.ClaimInfo(context.Background(), "slug")
	if err != nil {
		t.Fatalf("ClaimInfo: %v", err)
	}
	if info.ClaimCode != "QRST-6789" {
		t.Errorf("expected code from notification fallback, got %+v", info)
	}
}
