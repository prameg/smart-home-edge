package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/smart-home/edge/agent/fleet"
)

// wsUpgrader upgrades the test server's /api/websocket route. The default
// (empty) upgrader accepts same-origin httptest requests.
var wsUpgrader = websocket.Upgrader{}

// serveHAWebSocket plays Home Assistant's WebSocket API for tests: it runs the
// auth_required -> auth -> auth_ok handshake, then answers
// auth/long_lived_access_token and supervisor/api commands. dispatch returns the
// Supervisor `data` for an endpoint, or an error to relay as a Supervisor-side
// failure (success:false) — the shape the real supervisor/api command uses.
func serveHAWebSocket(w http.ResponseWriter, r *http.Request, dispatch func(endpoint, method string, data json.RawMessage) (any, error)) {
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = conn.WriteJSON(map[string]any{"type": "auth_required"})
	var auth map[string]any
	if err := conn.ReadJSON(&auth); err != nil {
		return
	}
	_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})

	for {
		var msg struct {
			ID       int             `json:"id"`
			Type     string          `json:"type"`
			Endpoint string          `json:"endpoint"`
			Method   string          `json:"method"`
			Data     json.RawMessage `json:"data"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}

		switch msg.Type {
		case "auth/long_lived_access_token":
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "type": "result", "success": true, "result": "LLT-e2e"})
		case "supervisor/api":
			data, derr := dispatch(msg.Endpoint, msg.Method, msg.Data)
			if derr != nil {
				_ = conn.WriteJSON(map[string]any{"id": msg.ID, "type": "result", "success": false, "error": map[string]any{"code": "unknown_error", "message": derr.Error()}})

				continue
			}
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "type": "result", "success": true, "result": data})
		default:
			_ = conn.WriteJSON(map[string]any{"id": msg.ID, "type": "result", "success": false, "error": map[string]any{"code": "unknown_command", "message": msg.Type}})
		}
	}
}

// mockHA is a faithful-enough stand-in for a fresh HAOS gateway: the onboarding
// + auth REST API and the Supervisor /api/hassio proxy, backed by mutable state.
// It lets the whole real stack (Client + Engine + BuildSteps) run end-to-end in
// process — the automated equivalent of the on-hardware onboarding run, and the
// place the kill-and-resume behavior is proven against the real client.
type mockHA struct {
	mu           sync.Mutex
	userDone     bool
	repos        []string
	installed    map[string]bool
	started      map[string]bool
	options      map[string]map[string]any
	installCount map[string]int
}

const mockAgentSlug = "e2e_smart_home_agent"

func newMockHA() *mockHA {
	return &mockHA{
		installed:    map[string]bool{},
		started:      map[string]bool{},
		options:      map[string]map[string]any{},
		installCount: map[string]int{},
	}
}

func (m *mockHA) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/onboarding", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		done := m.userDone
		m.mu.Unlock()
		writeRaw(w, `[{"step":"user","done":`+boolStr(done)+`},{"step":"core_config","done":false}]`)
	})
	mux.HandleFunc("/api/onboarding/users", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		m.userDone = true
		m.mu.Unlock()
		writeRaw(w, `{"auth_code":"AC-fresh"}`)
	})
	mux.HandleFunc("/api/onboarding/core_config", okRaw)
	mux.HandleFunc("/api/onboarding/analytics", okRaw)
	mux.HandleFunc("/api/onboarding/integration", func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, `{"auth_code":"AC-integration"}`)
	})
	mux.HandleFunc("/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, `{"access_token":"AT-e2e","refresh_token":"RT","expires_in":1800}`)
	})
	mux.HandleFunc("/auth/login_flow", func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, `{"flow_id":"F1","type":"form","step_id":"init"}`)
	})
	mux.HandleFunc("/auth/login_flow/", func(w http.ResponseWriter, _ *http.Request) {
		writeRaw(w, `{"type":"create_entry","result":"AC-relogin"}`)
	})

	// Supervisor management now rides the websocket supervisor/api command (Core
	// no longer proxies /store or /addons through /api/hassio). Add-on logs are
	// still read through the allowlisted /api/hassio proxy.
	mux.HandleFunc("/api/websocket", func(w http.ResponseWriter, r *http.Request) {
		serveHAWebSocket(w, r, m.supervisorDispatch)
	})
	mux.HandleFunc("/api/hassio/", m.addonLogsHTTP)

	return mux
}

// supervisorDispatch models the Supervisor endpoints the onboarding flow drives,
// returning the same `data` payloads the real Supervisor returns. A not-installed
// add-on's /info is a Supervisor error (mirroring the real 404-equivalent), which
// AddonInfo treats as "not installed".
func (m *mockHA) supervisorDispatch(endpoint, method string, data json.RawMessage) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch {
	case endpoint == "/store/repositories" && method == "get":
		items := make([]map[string]string, 0, len(m.repos))
		for _, u := range m.repos {
			items = append(items, map[string]string{"url": u})
		}

		return items, nil

	case endpoint == "/store/repositories" && method == "post":
		var body struct {
			Repository string `json:"repository"`
		}
		_ = json.Unmarshal(data, &body)
		m.repos = append(m.repos, body.Repository)

		return nil, nil

	case endpoint == "/store/reload":
		return nil, nil

	case endpoint == "/store":
		return map[string]any{"addons": []map[string]string{{"slug": mockAgentSlug}}}, nil

	case strings.HasPrefix(endpoint, "/addons/") && strings.HasSuffix(endpoint, "/info"):
		slug := middle(endpoint, "/addons/", "/info")
		if !m.installed[slug] {
			return nil, fmt.Errorf("Addon %s is not installed", slug)
		}
		state := "stopped"
		if m.started[slug] {
			state = "started"
		}

		return map[string]any{"version": "1.0.0", "state": state, "auto_update": false, "options": m.options[slug]}, nil

	case strings.HasPrefix(endpoint, "/store/addons/") && strings.HasSuffix(endpoint, "/install"):
		slug := middle(endpoint, "/store/addons/", "/install")
		m.installed[slug] = true
		m.installCount[slug]++

		return nil, nil

	case strings.HasSuffix(endpoint, "/options"):
		slug := middle(endpoint, "/addons/", "/options")
		var body struct {
			Options map[string]any `json:"options"`
		}
		_ = json.Unmarshal(data, &body)
		if body.Options != nil {
			m.options[slug] = body.Options
		}

		return nil, nil

	case strings.HasSuffix(endpoint, "/start"):
		slug := middle(endpoint, "/addons/", "/start")
		m.started[slug] = true

		return nil, nil

	default:
		// Tolerate unmodeled calls (e.g. /addons/{slug}/update on a pinned run).
		return nil, nil
	}
}

// addonLogsHTTP serves the one Supervisor call still routed through the
// /api/hassio proxy: add-on logs (plain text, allowlisted for admins).
func (m *mockHA) addonLogsHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/api/hassio")
	if !strings.HasSuffix(p, "/logs") {
		w.WriteHeader(http.StatusNotFound)

		return
	}

	slug := middle(p, "/addons/", "/logs")

	m.mu.Lock()
	started := m.started[slug]
	m.mu.Unlock()

	if started {
		writeRaw(w, "msg=provisioned uid=e2e-uid-0001 claim_status=unclaimed mqtt_username=gw_e2e\nmsg=\"gateway is UNCLAIMED\" claim_code=MOCK-CODE\n")
	} else {
		writeRaw(w, "msg=starting\n")
	}
}

// runFullOnboarding runs the real Client + Engine + steps against the mock, with
// a fresh client (as a new process would have), and returns the final state.
func runFullOnboarding(t *testing.T, baseURL string) *State {
	t.Helper()

	client := New(baseURL, testLog())
	st := &State{
		Client:       client,
		Manifest:     e2eManifest(),
		Owner:        OwnerConfig{Name: "N", Username: "admin", Password: "pw"},
		AgentOptions: map[string]any{"cloud_base_url": "https://cloud.test", "factory_key": "fk", "mqtt_host": "b.test", "mqtt_port": 8883, "mqtt_tls": true},
		Timeouts:     Timeouts{WaitCore: 5 * time.Second, WaitProvision: 5 * time.Second},
	}

	eng := NewEngine(BuildSteps(&captureReporter{}), nil)
	if err := eng.Run(context.Background(), st); err != nil {
		t.Fatalf("onboarding run: %v", err)
	}

	return st
}

func e2eManifest() *fleet.Manifest {
	return &fleet.Manifest{
		ReleaseID:       "e2e",
		AddonRepository: "https://example.test/edge-repo",
		Addons: []fleet.Addon{
			{Name: "Smart Home Agent", Match: "smart_home_agent", Repository: "https://example.test/edge-repo"},
		},
	}
}

// The whole stack runs against the mock and reaches the claim code — the
// in-process equivalent of a clean on-hardware onboarding run.
func TestEndToEndOnboarding(t *testing.T) {
	mock := newMockHA()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	st := runFullOnboarding(t, srv.URL)

	if st.Claim.UID != "e2e-uid-0001" {
		t.Errorf("expected provisioned uid captured, got %q", st.Claim.UID)
	}
	if st.Claim.ClaimCode != "MOCK-CODE" {
		t.Errorf("expected claim code captured, got %q", st.Claim.ClaimCode)
	}

	if mock.installCount[mockAgentSlug] != 1 {
		t.Errorf("agent should have installed exactly once, got %d", mock.installCount[mockAgentSlug])
	}
}

// Kill-and-resume: a first run is interrupted after the install step (only the
// first four steps run); re-running the full sequence against the same device
// resumes — already-done steps are skipped (the agent is not re-installed) and
// the run still reaches the claim code.
func TestEndToEndResumeAfterInterruption(t *testing.T) {
	mock := newMockHA()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	// First run: simulate a crash right after install by running only the first
	// four steps (connect, owner-and-token, addon-repository, install-addons).
	partialClient := New(srv.URL, testLog())
	partialState := &State{
		Client:   partialClient,
		Manifest: e2eManifest(),
		Owner:    OwnerConfig{Username: "admin", Password: "pw"},
		Timeouts: Timeouts{WaitCore: 5 * time.Second, WaitProvision: 5 * time.Second},
	}
	partial := BuildSteps(&captureReporter{})[:4]
	if err := NewEngine(partial, nil).Run(context.Background(), partialState); err != nil {
		t.Fatalf("partial run: %v", err)
	}
	if !mock.installed[mockAgentSlug] {
		t.Fatal("agent should be installed after the partial run")
	}

	// Second run: a fresh process (new client, empty token) re-runs everything.
	st := runFullOnboarding(t, srv.URL)

	if st.Claim.ClaimCode != "MOCK-CODE" {
		t.Errorf("resume run should reach the claim code, got %q", st.Claim.ClaimCode)
	}
	if mock.installCount[mockAgentSlug] != 1 {
		t.Errorf("resume must not reinstall the agent (install count=%d)", mock.installCount[mockAgentSlug])
	}
}

// --- small helpers for the mock ---

func middle(path, prefix, suffix string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
}

func writeRaw(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

func okRaw(w http.ResponseWriter, _ *http.Request) { writeRaw(w, `{}`) }

func boolStr(b bool) string {
	if b {
		return "true"
	}

	return "false"
}
