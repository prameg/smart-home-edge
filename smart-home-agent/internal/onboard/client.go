package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/supervisor"
)

// Client is the real DeviceAPI: it speaks Home Assistant's public onboarding +
// auth REST API and reaches the Supervisor through Core's authenticated
// "supervisor/api" WebSocket command (current HA no longer proxies general
// Supervisor calls over the /api/hassio REST endpoint — that proxy is locked to
// backups/logs; see supervisorAPI). Add-on logs are the one Supervisor read
// still taken over the allowlisted /api/hassio proxy. It is deliberately the
// only place that knows HA's wire formats, so the steps stay declarative and the
// whole surface is swappable for a fake in tests.
type Client struct {
	baseURL  string
	clientID string
	http     *http.Client
	log      *slog.Logger

	// token authenticates the Supervisor WebSocket command and every /api/hassio
	// and /api/states call; set once the owner+token step authenticates
	// (SetToken).
	token string

	// sup runs the Supervisor operations over this client's WS transport (Call),
	// so add-on/OS/Core management is the one shared implementation the agent
	// add-on also uses (see internal/supervisor).
	sup *supervisor.Client
}

// claimCode / uid / claim_status parsing off the agent add-on's log stream and
// the fallback HA persistent notification. The agent logs logfmt (slog text
// handler) and posts a "…enter claim code **XXXX-XXXX**…" notification.
var (
	reClaimLogfmt = regexp.MustCompile(`claim_code=([A-Za-z0-9]{4}-[A-Za-z0-9]{4})`)
	reClaimNotif  = regexp.MustCompile(`\*\*([A-Za-z0-9]{4}-[A-Za-z0-9]{4})\*\*`)
	reUID         = regexp.MustCompile(`\buid=([0-9a-zA-Z_-]{8,})`)
	reClaimStatus = regexp.MustCompile(`claim_status=([a-z]+)`)
	// reSerial parses the hardware serial the agent logs alongside uid on its
	// "provisioned" line (and at startup). Real serials (pi-<cpuserial>,
	// host-<hostname>, unknown-serial, or a CLI override) carry no spaces, so a
	// non-space capture is enough; slog only quotes values with spaces/specials,
	// so any stray surrounding quotes are trimmed by the caller.
	reSerial = regexp.MustCompile(`\bserial=(\S+)`)
)

// New builds a client for a gateway reachable at baseURL (e.g.
// http://homeassistant.local:8123). It has no global HTTP timeout on purpose —
// some Supervisor calls (add-on install, OS/Core update) legitimately take
// minutes — so callers bound each call with the context instead.
func New(baseURL string, log *slog.Logger) *Client {
	base := strings.TrimRight(baseURL, "/")

	c := &Client{
		baseURL:  base,
		clientID: base + "/",
		http:     &http.Client{},
		log:      log,
	}
	c.sup = supervisor.New(c, supervisorCallTimeout, installTimeout)

	return c
}

// SetToken stores the token used for all later authenticated calls.
func (c *Client) SetToken(token string) { c.token = token }

// Token returns the stored token.
func (c *Client) Token() string { return c.token }

// WaitForCore polls the onboarding endpoint until Core is *usably* up, or the
// timeout elapses. "Usably up" is deliberately stricter than "the socket
// answered": we wait for a definitive onboarding signal — HTTP 200 (the wizard
// is being served) or 404 (onboarding is fully finished, endpoint removed).
//
// A fresh HAOS boot binds Core's HTTP socket well before the auth provider,
// onboarding views, and the Supervisor link have finished setting up. In that
// warm-up window the socket answers with a transient 401/403/5xx, which does
// NOT mean the device is ready to be onboarded — treating it as "up" (the old
// behavior) let connect short-circuit and the next step then hit a spurious 401
// on /api/onboarding or /api/hassio. So any non-definitive status is treated as
// "still coming up" and we keep polling. Each poll is individually short so a
// hung socket does not swallow the whole budget.
func (c *Client) WaitForCore(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	var lastStatus int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		attemptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, _, err := c.get(attemptCtx, "/api/onboarding")
		cancel()

		if err == nil {
			if onboardingReady(status) {
				return nil
			}
			lastStatus = status
		}

		if time.Now().After(deadline) {
			switch {
			case err != nil:
				return fmt.Errorf("core did not answer within %s: %w", timeout, err)
			case lastStatus != 0:
				return fmt.Errorf("core answered but was still initializing within %s (last onboarding status %d); re-run to resume", timeout, lastStatus)
			default:
				return fmt.Errorf("core did not answer within %s", timeout)
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// onboardingReady reports whether a GET /api/onboarding status means Core is
// ready to be driven: 200 (wizard active) or 404 (onboarding already complete).
// Every other status (a warm-up 401/403, a starting 5xx) means "not yet".
func onboardingReady(status int) bool {
	return status == http.StatusOK || status == http.StatusNotFound
}

// OnboardingStatus reads GET /api/onboarding. A 200 carries the per-step wizard
// state; once onboarding is fully complete HA removes those views and the
// endpoint 404s, which we map to AllDone.
func (c *Client) OnboardingStatus(ctx context.Context) (OnboardingStatus, error) {
	status, body, err := c.get(ctx, "/api/onboarding")
	if err != nil {
		return OnboardingStatus{}, fmt.Errorf("onboarding status: %w", err)
	}

	if status == http.StatusNotFound {
		return OnboardingStatus{UserDone: true, AllDone: true}, nil
	}

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// The onboarding views are (temporarily) absent: Core's onboarding/auth
		// subsystem is still initializing after boot. This is transient — the
		// connect step should normally hold us until Core is usably up, so
		// seeing it here means we raced a slow warm-up. Re-running resumes.
		return OnboardingStatus{}, fmt.Errorf("onboarding status: Core is still initializing (status %d); wait for the Home Assistant onboarding screen to load, then re-run", status)
	}

	if status != http.StatusOK {
		return OnboardingStatus{}, fmt.Errorf("onboarding status: unexpected status %d", status)
	}

	var steps []struct {
		Step string `json:"step"`
		Done bool   `json:"done"`
	}
	if err := json.Unmarshal(body, &steps); err != nil {
		return OnboardingStatus{}, fmt.Errorf("onboarding status: decode: %w", err)
	}

	out := OnboardingStatus{AllDone: true}
	for _, s := range steps {
		if s.Step == "user" && s.Done {
			out.UserDone = true
		}
		if !s.Done {
			out.AllDone = false
		}
	}

	return out, nil
}

// CreateOwner creates the owner account and returns an authorization code.
func (c *Client) CreateOwner(ctx context.Context, o OwnerConfig) (string, error) {
	lang := o.Language
	if lang == "" {
		lang = "en"
	}

	status, body, err := c.postJSON(ctx, "/api/onboarding/users", map[string]string{
		"client_id": c.clientID,
		"name":      o.Name,
		"username":  o.Username,
		"password":  o.Password,
		"language":  lang,
	})
	if err != nil {
		return "", fmt.Errorf("create owner: %w", err)
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("create owner: unexpected status %d: %s", status, truncate(body))
	}

	var out struct {
		AuthCode string `json:"auth_code"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("create owner: decode: %w", err)
	}

	if out.AuthCode == "" {
		return "", fmt.Errorf("create owner: response carried no auth_code")
	}

	return out.AuthCode, nil
}

// Login runs the username/password auth flow (re-run path) and returns an
// authorization code. It fails clearly on bad credentials or an MFA prompt the
// CLI does not drive.
func (c *Client) Login(ctx context.Context, username, password string) (string, error) {
	status, body, err := c.postJSON(ctx, "/auth/login_flow", map[string]any{
		"client_id":    c.clientID,
		"handler":      []any{"homeassistant", nil},
		"redirect_uri": c.clientID,
	})
	if err != nil {
		return "", fmt.Errorf("login: start flow: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("login: start flow: unexpected status %d: %s", status, truncate(body))
	}

	var flow struct {
		FlowID string `json:"flow_id"`
		Type   string `json:"type"`
		StepID string `json:"step_id"`
	}
	if err := json.Unmarshal(body, &flow); err != nil {
		return "", fmt.Errorf("login: decode flow: %w", err)
	}
	if flow.FlowID == "" {
		return "", fmt.Errorf("login: auth provider returned no flow id")
	}

	status, body, err = c.postJSON(ctx, "/auth/login_flow/"+flow.FlowID, map[string]any{
		"client_id": c.clientID,
		"username":  username,
		"password":  password,
	})
	if err != nil {
		return "", fmt.Errorf("login: submit credentials: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("login: submit credentials: unexpected status %d: %s", status, truncate(body))
	}

	var result struct {
		Type   string `json:"type"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("login: decode result: %w", err)
	}

	if result.Type != "create_entry" || result.Result == "" {
		return "", fmt.Errorf("login failed: check the username/password (MFA is not supported by this tool)")
	}

	return result.Result, nil
}

// ExchangeCode swaps an authorization code for a short-lived access token via the
// OAuth2 token endpoint (form-encoded).
func (c *Client) ExchangeCode(ctx context.Context, authCode string) (string, error) {
	status, body, err := c.postForm(ctx, "/auth/token", url.Values{
		"grant_type": {"authorization_code"},
		"code":       {authCode},
		"client_id":  {c.clientID},
	})
	if err != nil {
		return "", fmt.Errorf("exchange code: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("exchange code: unexpected status %d: %s", status, truncate(body))
	}

	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("exchange code: decode: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("exchange code: response carried no access_token")
	}

	return out.AccessToken, nil
}

// CreateLongLivedToken upgrades a short-lived access token into a long-lived one
// over the WebSocket API (the only place HA mints them), so the token outlasts a
// slow add-on install / Core download.
//
// A previous run leaves a long-lived token registered under clientName, and HA
// refuses to mint a second one with the same client_name — its handler raises a
// bare ValueError that surfaces as a generic "unknown_error", so a naive re-run
// could NEVER mint one again. We therefore delete any stale token with our name
// first (see purgeLongLivedTokens), which makes minting idempotent across the
// resumable re-runs this tool is built around.
func (c *Client) CreateLongLivedToken(ctx context.Context, accessToken, clientName string) (string, error) {
	conn, err := c.dialAuthedWS(ctx, accessToken)
	if err != nil {
		return "", fmt.Errorf("long-lived token: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	c.purgeLongLivedTokens(conn, clientName)

	// A high, distinct id keeps this out of the way of the purge round-trips
	// above (list=1, deletes=2…), which share this connection.
	result, err := wsRoundTrip(conn, 1000, map[string]any{
		"type":        "auth/long_lived_access_token",
		"client_name": clientName,
		"lifespan":    3650,
	})
	if err != nil {
		return "", fmt.Errorf("long-lived token: %w", err)
	}

	var token string
	if err := json.Unmarshal(result, &token); err != nil {
		return "", fmt.Errorf("long-lived token: decode: %w", err)
	}

	return token, nil
}

// purgeLongLivedTokens deletes any existing long-lived token whose client_name
// matches, so CreateLongLivedToken can re-mint under the same name on a re-run.
// It is best-effort on the same authenticated connection: any failure here just
// risks the mint hitting "already exists", which the caller already tolerates by
// falling back to the short-lived access token.
func (c *Client) purgeLongLivedTokens(conn *websocket.Conn, clientName string) {
	result, err := wsRoundTrip(conn, 1, map[string]any{"type": "auth/refresh_tokens"})
	if err != nil {
		c.log.Debug("list refresh tokens failed (non-fatal)", "error", err)

		return
	}

	var tokens []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		ClientName string `json:"client_name"`
	}
	if err := json.Unmarshal(result, &tokens); err != nil {
		c.log.Debug("decode refresh tokens failed (non-fatal)", "error", err)

		return
	}

	id := 2
	for _, t := range tokens {
		if t.Type != "long_lived_access_token" || t.ClientName != clientName {
			continue
		}

		if _, err := wsRoundTrip(conn, id, map[string]any{
			"type":             "auth/delete_refresh_token",
			"refresh_token_id": t.ID,
		}); err != nil {
			c.log.Debug("delete stale long-lived token failed (non-fatal)", "id", t.ID, "error", err)
		}
		id++
	}
}

// UpdateCoreConfig sets Home Assistant's core configuration (country, time zone,
// currency, unit system, language) over the WS config/core/update command.
//
// The headless onboarding flow never sets a location — the REST core_config
// onboarding step only marks itself done — so Core is left warning "No country
// has been configured". This fills in whatever the operator supplied. Only
// non-empty fields are sent; an empty config is a no-op (no connection opened).
func (c *Client) UpdateCoreConfig(ctx context.Context, cfg CoreConfig) error {
	fields := cfg.fields()
	if len(fields) == 0 {
		return nil
	}

	conn, err := c.dialAuthedWS(ctx, c.token)
	if err != nil {
		return fmt.Errorf("core config: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	cmd := map[string]any{"type": "config/core/update"}
	for k, v := range fields {
		cmd[k] = v
	}

	if _, err := wsRoundTrip(conn, 1, cmd); err != nil {
		return fmt.Errorf("core config: %w", err)
	}

	return nil
}

// wsResultErr is a Home Assistant WebSocket command failure (success:false),
// carrying HA's error code + message.
type wsResultErr struct {
	code    string
	message string
}

func (e *wsResultErr) Error() string {
	if e.code == "" {
		return e.message
	}

	return fmt.Sprintf("%s: %s", e.code, e.message)
}

// wsRoundTrip sends cmd tagged with id on an already-authenticated WebSocket
// connection and returns the result payload for that id, skipping any unrelated
// frames. Callers own dialing/closing the connection and must use distinct ids
// for concurrent-in-flight commands on the same connection.
func wsRoundTrip(conn *websocket.Conn, id int, cmd map[string]any) (json.RawMessage, error) {
	cmd["id"] = id
	if err := conn.WriteJSON(cmd); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	for {
		var frame struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			return nil, fmt.Errorf("read: %w", err)
		}
		if frame.Type != "result" || frame.ID != id {
			continue
		}
		if !frame.Success {
			return nil, &wsResultErr{code: frame.Error.Code, message: frame.Error.Message}
		}

		return frame.Result, nil
	}
}

// dialAuthedWS opens the Home Assistant WebSocket API and completes the
// auth_required -> auth -> auth_ok handshake with accessToken. The caller owns
// closing the returned connection.
func (c *Client) dialAuthedWS(ctx context.Context, accessToken string) (*websocket.Conn, error) {
	wsURL, err := c.websocketURL()
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, _, err := websocket.DefaultDialer.DialContext(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	var hello struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&hello); err != nil {
		_ = conn.Close()

		return nil, fmt.Errorf("read hello: %w", err)
	}
	if hello.Type == "auth_required" {
		if err := conn.WriteJSON(map[string]string{"type": "auth", "access_token": accessToken}); err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("send auth: %w", err)
		}
		var authResult struct {
			Type string `json:"type"`
		}
		if err := conn.ReadJSON(&authResult); err != nil {
			_ = conn.Close()

			return nil, fmt.Errorf("read auth result: %w", err)
		}
		if authResult.Type != "auth_ok" {
			_ = conn.Close()

			return nil, fmt.Errorf("auth rejected (%s)", authResult.Type)
		}
	}

	return conn, nil
}

// supervisorCallTimeout bounds a normal Supervisor call server-side. It is
// generous because a store reload can clone a repository; slow mutations
// (install/update) pass installTimeout instead.
const supervisorCallTimeout = 90 * time.Second

// Call implements supervisor.Transport: it reaches a Supervisor REST endpoint
// through Core's authenticated "supervisor/api" WebSocket command. This is the
// WS half of the shared Supervisor operations (the agent add-on supplies a
// direct-HTTP half), so onboarding and fleet updates drive Supervisor through
// one code path in internal/supervisor.
func (c *Client) Call(ctx context.Context, method, endpoint string, payload any, timeout time.Duration) (json.RawMessage, error) {
	return c.supervisorAPI(ctx, method, endpoint, payload, timeout)
}

// supervisorAPI calls a Supervisor REST endpoint through Core's authenticated
// "supervisor/api" WebSocket command and returns the unwrapped Supervisor `data`
// payload.
//
// Current Home Assistant no longer proxies general Supervisor calls through the
// /api/hassio REST endpoint: HassIOView is locked to an allowlist (backups,
// add-on logs, changelog/docs) and answers 401 for everything else — including
// all of /store and /addons management. supervisor/api is the WebSocket command
// the HA frontend itself uses for those calls, and it works across HA versions,
// so it is the stable contract for driving Supervisor from Core.
//
// serverTimeout is forwarded as the command's own timeout and must exceed slow
// operations, because Supervisor defaults the command to only 10s.
func (c *Client) supervisorAPI(ctx context.Context, method, endpoint string, payload any, serverTimeout time.Duration) (json.RawMessage, error) {
	conn, err := c.dialAuthedWS(ctx, c.token)
	if err != nil {
		return nil, fmt.Errorf("supervisor %s %s: %w", method, endpoint, err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	cmd := map[string]any{
		"id":       1,
		"type":     "supervisor/api",
		"endpoint": endpoint,
		"method":   strings.ToLower(method),
	}
	if payload != nil {
		cmd["data"] = payload
	}
	if serverTimeout > 0 {
		cmd["timeout"] = int(serverTimeout.Seconds())
	}

	if err := conn.WriteJSON(cmd); err != nil {
		return nil, fmt.Errorf("supervisor %s %s: send: %w", method, endpoint, err)
	}

	for {
		var frame struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			return nil, fmt.Errorf("supervisor %s %s: read: %w", method, endpoint, err)
		}
		if frame.Type != "result" || frame.ID != 1 {
			continue
		}
		if !frame.Success {
			return nil, &supervisor.Error{Method: method, Endpoint: endpoint, Message: frame.Error.Message}
		}

		return frame.Result, nil
	}
}

// FinishOnboarding best-effort completes the remaining wizard steps so the
// frontend does not reappear at the wizard. Each call is non-fatal: the owner +
// token is all onboarding strictly needs for Supervisor access.
func (c *Client) FinishOnboarding(ctx context.Context) error {
	c.tryFinishStep(ctx, "/api/onboarding/core_config", map[string]any{})
	c.tryFinishStep(ctx, "/api/onboarding/analytics", map[string]any{})
	c.tryFinishStep(ctx, "/api/onboarding/integration", map[string]any{
		"client_id":    c.clientID,
		"redirect_uri": c.clientID,
	})

	return nil
}

func (c *Client) tryFinishStep(ctx context.Context, path string, payload any) {
	stepCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if _, _, err := c.postJSON(stepCtx, path, payload); err != nil {
		c.log.Debug("finish onboarding step failed (non-fatal)", "path", path, "error", err)
	}
}

// The Supervisor operations below all delegate to the shared internal/supervisor
// client over this Client's WS transport (see Call), so the CLI and the agent
// add-on drive Supervisor through exactly one implementation. The thin wrappers
// remain because they satisfy the DeviceAPI seam the onboarding steps test
// against.

// StoreRepositories lists the registered add-on store repository URLs.
func (c *Client) StoreRepositories(ctx context.Context) ([]string, error) {
	return c.sup.StoreRepositories(ctx)
}

// AddStoreRepository registers a repository and reloads the store so its add-ons
// become resolvable.
func (c *Client) AddStoreRepository(ctx context.Context, repoURL string) error {
	return c.sup.AddStoreRepository(ctx, repoURL)
}

// ResolveAddonSlug returns the full Supervisor slug for a manifest add-on.
func (c *Client) ResolveAddonSlug(ctx context.Context, a fleet.Addon) (string, bool, error) {
	return c.sup.ResolveAddonSlug(ctx, a)
}

// AddonInfo returns an add-on's install state.
func (c *Client) AddonInfo(ctx context.Context, slug string) (AddonInfo, error) {
	return c.sup.AddonInfo(ctx, slug)
}

// InstallAddon installs an add-on's latest version.
func (c *Client) InstallAddon(ctx context.Context, slug string) error {
	return c.sup.InstallAddon(ctx, slug)
}

// SetAddonOptions writes an add-on's user options.
func (c *Client) SetAddonOptions(ctx context.Context, slug string, options map[string]any) error {
	return c.sup.SetAddonOptions(ctx, slug, options)
}

// StartAddon starts an installed add-on.
func (c *Client) StartAddon(ctx context.Context, slug string) error {
	return c.sup.StartAddon(ctx, slug)
}

// RestartAddon restarts an installed add-on so option changes written while it
// was running take effect (HA loads add-on options only at start).
func (c *Client) RestartAddon(ctx context.Context, slug string) error {
	return c.sup.RestartAddon(ctx, slug)
}

// ClaimInfo reads the agent's identity + claim code from its add-on log stream,
// falling back to the HA persistent notification for the code. found is false
// until the agent has provisioned (no uid yet).
func (c *Client) ClaimInfo(ctx context.Context, agentSlug string) (ClaimInfo, error) {
	logs, err := c.addonLogs(ctx, agentSlug)
	if err != nil {
		return ClaimInfo{}, err
	}

	info := ClaimInfo{}
	if m := reUID.FindStringSubmatch(logs); m != nil {
		info.UID = m[1]
	}
	if m := reSerial.FindStringSubmatch(logs); m != nil {
		info.Serial = strings.Trim(m[1], `"`)
	}
	if m := reClaimStatus.FindStringSubmatch(logs); m != nil {
		info.Claimed = m[1] == "claimed"
	}
	if m := reClaimLogfmt.FindStringSubmatch(logs); m != nil {
		info.ClaimCode = m[1]
	}

	// Fall back to the persistent notification for the code (the log line that
	// carries it may have scrolled out of the returned window).
	if info.ClaimCode == "" && !info.Claimed {
		if code := c.claimCodeFromNotification(ctx); code != "" {
			info.ClaimCode = code
		}
	}

	return info, nil
}

func (c *Client) addonLogs(ctx context.Context, slug string) (string, error) {
	// Logs are plain text, not the wrapped {result,data} envelope.
	status, body, err := c.get(ctx, "/api/hassio/addons/"+slug+"/logs")
	if err != nil {
		return "", fmt.Errorf("addon logs %q: %w", slug, err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("addon logs %q: unexpected status %d", slug, status)
	}

	return string(body), nil
}

func (c *Client) claimCodeFromNotification(ctx context.Context) string {
	status, body, err := c.get(ctx, "/api/states/persistent_notification.smart_home_claim_code")
	if err != nil || status != http.StatusOK {
		return ""
	}

	var state struct {
		Attributes struct {
			Message string `json:"message"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal(body, &state); err != nil {
		return ""
	}

	if m := reClaimNotif.FindStringSubmatch(state.Attributes.Message); m != nil {
		return m[1]
	}

	return ""
}

// --- low-level HTTP helpers -----------------------------------------------

func (c *Client) get(ctx context.Context, path string) (int, []byte, error) {
	return c.do(ctx, http.MethodGet, path, nil, "")
}

func (c *Client) postJSON(ctx context.Context, path string, payload any) (int, []byte, error) {
	return c.do(ctx, http.MethodPost, path, payload, "application/json")
}

func (c *Client) postForm(ctx context.Context, path string, values url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	return c.send(req)
}

// do performs a request against the gateway, JSON-encoding payload when the
// content type is JSON, and attaching the bearer token when one is set.
func (c *Client) do(ctx context.Context, method, path string, payload any, contentType string) (int, []byte, error) {
	var bodyReader io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request: %w", err)
		}
		bodyReader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}

	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	return c.send(req)
}

// send attaches auth + Accept, executes the request, and reads the full body.
func (c *Client) send(req *http.Request) (int, []byte, error) {
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response body: %w", err)
	}

	return resp.StatusCode, body, nil
}

// websocketURL derives the ws(s) URL for the WebSocket API from the base URL.
func (c *Client) websocketURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse base url: %w", err)
	}

	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}

	u.Path = "/api/websocket"

	return u.String(), nil
}

func truncate(body []byte) string {
	const max = 200
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "…"
	}

	return s
}
