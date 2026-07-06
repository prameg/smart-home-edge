// Package webui is the agent's small embedded status/actions web server. Home
// Assistant renders it as an authenticated sidebar panel via Ingress: Supervisor
// proxies already-authenticated requests to this server on the ingress port (see
// `ingress_port` in config.yaml and config.WebUIPort), so it needs no auth of
// its own and binds only the internal port.
//
// It is intentionally dependency-free: a single embedded HTML page plus a narrow
// JSON API. The Backend interface is the whole coupling to the agent, so the
// handlers are testable against a fake without a live broker or Home Assistant.
package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	_ "embed"
)

//go:embed index.html
var indexHTML []byte

// Status is the read-only snapshot rendered by the panel. It deliberately
// carries NO secrets — never the MQTT password or the provision token — because
// it is serialized straight to the browser.
type Status struct {
	UID                string `json:"uid"`
	Serial             string `json:"serial"`
	AgentVersion       string `json:"agent_version"`
	HAVersion          string `json:"ha_version"`
	CloudBaseURL       string `json:"cloud_base_url"`
	MQTTHost           string `json:"mqtt_host"`
	MQTTPort           int    `json:"mqtt_port"`
	MQTTTLS            bool   `json:"mqtt_tls"`
	Claimed            bool   `json:"claimed"`
	ClaimStatus        string `json:"claim_status"`
	ClaimCode          string `json:"claim_code,omitempty"`
	ClaimCodeExpiresAt string `json:"claim_code_expires_at,omitempty"`
	BrokerConnected    bool   `json:"broker_connected"`
	ConfigVersion      int    `json:"config_version"`
	DeviceCount        int    `json:"device_count"`
}

// DeviceRow is one mapped device_uid <-> entity_id binding plus the entity's
// current HA state. State/LastChanged are best-effort: a fetch failure leaves
// them empty and populates Error so the panel can show the binding regardless.
type DeviceRow struct {
	DeviceUID   string `json:"device_uid"`
	EntityID    string `json:"entity_id"`
	State       string `json:"state,omitempty"`
	LastChanged string `json:"last_changed,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Backend is the narrow slice of the agent the panel drives. Keeping it an
// interface (rather than a *agent.Agent) is what lets webui_test exercise every
// route against a fake.
type Backend interface {
	// Status returns the current read-only snapshot. It takes a context because
	// the implementation reads the HA version best-effort over HTTP.
	Status(ctx context.Context) Status
	// Devices returns the mapped devices with their live HA state.
	Devices(ctx context.Context) []DeviceRow
	// ReissueClaimCode mints and surfaces a fresh claim code (unclaimed only).
	ReissueClaimCode(ctx context.Context) error
	// RepublishInventory forces a fresh inventory publish to the cloud.
	RepublishInventory(ctx context.Context) error
	// ReconcileNow re-reports HA's current state for every mapped device.
	ReconcileNow()
	// Reprovision recovers credentials (rotating the MQTT password) and
	// reconnects the broker.
	Reprovision(ctx context.Context) error
}

// actionResult is the uniform body every action route returns, so the frontend
// can toast a single { ok, message } shape.
type actionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

type server struct {
	backend Backend
	log     *slog.Logger
}

// Serve starts the web server on addr and blocks until ctx is cancelled, then
// shuts it down gracefully. It returns nil on a clean shutdown so a caller can
// launch it in a goroutine and ignore the result.
func Serve(ctx context.Context, addr string, b Backend, log *slog.Logger) error {
	s := &server{backend: b, log: log}

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(shutCtx)
	}()

	log.Info("web UI listening", "addr", addr)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

// routes wires the mux. Method-qualified patterns (Go 1.22+) give a free 405 for
// the wrong verb on a known path (e.g. POST /api/status); the "GET /" catch-all
// serves the page and 404s any other GET path via handleIndex's guard.
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/actions/claim-code", s.handleClaimCode)
	mux.HandleFunc("POST /api/actions/inventory", s.handleInventory)
	mux.HandleFunc("POST /api/actions/reconcile", s.handleReconcile)
	mux.HandleFunc("POST /api/actions/reprovision", s.handleReprovision)

	return mux
}

// handleIndex serves the embedded dashboard. "GET /" is the catch-all, so guard
// against serving the page for unknown sub-paths (they should 404).
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Status(r.Context()))
}

func (s *server) handleDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.backend.Devices(r.Context()))
}

func (s *server) handleClaimCode(w http.ResponseWriter, r *http.Request) {
	s.runAction(w, "Claim code reissued.", func() error {
		return s.backend.ReissueClaimCode(r.Context())
	})
}

func (s *server) handleInventory(w http.ResponseWriter, r *http.Request) {
	s.runAction(w, "Inventory republished to the cloud.", func() error {
		return s.backend.RepublishInventory(r.Context())
	})
}

func (s *server) handleReconcile(w http.ResponseWriter, _ *http.Request) {
	s.backend.ReconcileNow()
	writeJSON(w, http.StatusOK, actionResult{OK: true, Message: "Reconcile started; reported state is being re-published."})
}

func (s *server) handleReprovision(w http.ResponseWriter, r *http.Request) {
	s.runAction(w, "Re-provisioned; MQTT credentials rotated and broker reconnected.", func() error {
		return s.backend.Reprovision(r.Context())
	})
}

// runAction runs fn and always answers with a { ok, message } body (HTTP 200),
// so the frontend has one code path: a failed action surfaces its error in the
// message rather than as an HTTP error.
func (s *server) runAction(w http.ResponseWriter, success string, fn func() error) {
	if err := fn(); err != nil {
		s.log.Warn("web UI action failed", "error", err)
		writeJSON(w, http.StatusOK, actionResult{OK: false, Message: err.Error()})

		return
	}

	writeJSON(w, http.StatusOK, actionResult{OK: true, Message: success})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
