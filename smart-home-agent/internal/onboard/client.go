package onboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
}

// claimCode / uid / claim_status parsing off the agent add-on's log stream and
// the fallback HA persistent notification. The agent logs logfmt (slog text
// handler) and posts a "…enter claim code **XXXX-XXXX**…" notification.
var (
	reClaimLogfmt = regexp.MustCompile(`claim_code=([A-Za-z0-9]{4}-[A-Za-z0-9]{4})`)
	reClaimNotif  = regexp.MustCompile(`\*\*([A-Za-z0-9]{4}-[A-Za-z0-9]{4})\*\*`)
	reUID         = regexp.MustCompile(`\buid=([0-9a-zA-Z_-]{8,})`)
	reClaimStatus = regexp.MustCompile(`claim_status=([a-z]+)`)
)

// New builds a client for a gateway reachable at baseURL (e.g.
// http://homeassistant.local:8123). It has no global HTTP timeout on purpose —
// some Supervisor calls (add-on install, OS/Core update) legitimately take
// minutes — so callers bound each call with the context instead.
func New(baseURL string, log *slog.Logger) *Client {
	base := strings.TrimRight(baseURL, "/")

	return &Client{
		baseURL:  base,
		clientID: base + "/",
		http:     &http.Client{},
		log:      log,
	}
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

	if err := conn.WriteJSON(map[string]any{
		"id":          1,
		"type":        "auth/long_lived_access_token",
		"client_name": clientName,
		"lifespan":    3650,
	}); err != nil {
		return "", fmt.Errorf("long-lived token: request: %w", err)
	}

	for {
		var frame struct {
			ID      int             `json:"id"`
			Type    string          `json:"type"`
			Success bool            `json:"success"`
			Result  json.RawMessage `json:"result"`
			Error   struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := conn.ReadJSON(&frame); err != nil {
			return "", fmt.Errorf("long-lived token: read result: %w", err)
		}
		if frame.Type != "result" || frame.ID != 1 {
			continue
		}
		if !frame.Success {
			return "", fmt.Errorf("long-lived token: %s", frame.Error.Message)
		}

		var token string
		if err := json.Unmarshal(frame.Result, &token); err != nil {
			return "", fmt.Errorf("long-lived token: decode: %w", err)
		}

		return token, nil
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

// supervisorError is a Supervisor-side failure (the supervisor/api command
// returned success:false). It is kept distinct from a transport/auth error so
// callers can treat an expected Supervisor "not installed"/"not found" as a soft
// signal (the way the old /api/hassio proxy surfaced it as a 404) while still
// propagating real failures.
type supervisorError struct {
	method, endpoint, message string
}

func (e *supervisorError) Error() string {
	return fmt.Sprintf("supervisor %s %s: %s", e.method, e.endpoint, e.message)
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
			return nil, &supervisorError{method: method, endpoint: endpoint, message: frame.Error.Message}
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

// StoreRepositories lists the registered add-on store repository URLs.
func (c *Client) StoreRepositories(ctx context.Context) ([]string, error) {
	data, err := c.supervisorAPI(ctx, "get", "/store/repositories", nil, supervisorCallTimeout)
	if err != nil {
		return nil, err
	}

	var repos []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &repos); err != nil {
		return nil, fmt.Errorf("store repositories: decode: %w", err)
	}

	urls := make([]string, 0, len(repos))
	for _, r := range repos {
		if r.URL != "" {
			urls = append(urls, r.URL)
		}
	}

	return urls, nil
}

// AddStoreRepository registers a repository and reloads the store so its add-ons
// become resolvable.
func (c *Client) AddStoreRepository(ctx context.Context, repoURL string) error {
	if _, err := c.supervisorAPI(ctx, "post", "/store/repositories", map[string]string{"repository": repoURL}, supervisorCallTimeout); err != nil {
		return err
	}

	// Reload is best-effort; the repository add itself already refreshes.
	if _, err := c.supervisorAPI(ctx, "post", "/store/reload", nil, supervisorCallTimeout); err != nil {
		c.log.Debug("store reload failed (non-fatal)", "error", err)
	}

	return nil
}

// ResolveAddonSlug returns the full Supervisor slug for a manifest add-on. Exact
// slugs (built-in core_* add-ons) bypass the store lookup; otherwise it matches
// the store, which is how a repo-hash-prefixed community/agent slug is found.
func (c *Client) ResolveAddonSlug(ctx context.Context, a fleet.Addon) (string, bool, error) {
	if a.Slug != "" {
		return a.Slug, true, nil
	}

	data, err := c.supervisorAPI(ctx, "get", "/store", nil, supervisorCallTimeout)
	if err != nil {
		return "", false, err
	}

	var store struct {
		Addons []struct {
			Slug string `json:"slug"`
		} `json:"addons"`
	}
	if err := json.Unmarshal(data, &store); err != nil {
		return "", false, fmt.Errorf("resolve %q: decode store: %w", a.Match, err)
	}

	for _, sa := range store.Addons {
		if a.Resolves(sa.Slug) {
			return sa.Slug, true, nil
		}
	}

	return "", false, nil
}

// AddonInfo returns an add-on's install state. A Supervisor-side error on the
// info endpoint means the add-on is known to the store but not installed (the
// old /api/hassio proxy surfaced this as a 404); a transport/auth error is a
// real failure and propagates.
func (c *Client) AddonInfo(ctx context.Context, slug string) (AddonInfo, error) {
	data, err := c.supervisorAPI(ctx, "get", "/addons/"+slug+"/info", nil, supervisorCallTimeout)
	if err != nil {
		var se *supervisorError
		if errors.As(err, &se) {
			return AddonInfo{Slug: slug, Installed: false}, nil
		}

		return AddonInfo{}, err
	}

	var info struct {
		Version    string         `json:"version"`
		State      string         `json:"state"`
		AutoUpdate bool           `json:"auto_update"`
		Options    map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return AddonInfo{}, fmt.Errorf("addon info %q: decode: %w", slug, err)
	}

	return AddonInfo{
		Slug:       slug,
		Installed:  info.Version != "",
		Version:    info.Version,
		State:      info.State,
		AutoUpdate: info.AutoUpdate,
		Options:    info.Options,
	}, nil
}

// InstallAddon installs an add-on's latest version, tolerating the two path
// shapes different Supervisor versions expose.
func (c *Client) InstallAddon(ctx context.Context, slug string) error {
	return c.addonMutate(ctx, slug, "install", nil)
}

// UpdateAddon moves an installed add-on to a specific version (used to pin).
func (c *Client) UpdateAddon(ctx context.Context, slug, version string) error {
	return c.addonMutate(ctx, slug, "update", map[string]string{"version": version})
}

// addonMutate posts an install/update, trying the /store/addons form first then
// the /addons form (different Supervisor versions expose one or the other). It
// forwards installTimeout as the server-side timeout because a first install
// pulls a container image.
func (c *Client) addonMutate(ctx context.Context, slug, action string, payload any) error {
	_, err := c.supervisorAPI(ctx, "post", "/store/addons/"+slug+"/"+action, payload, installTimeout)
	if err == nil {
		return nil
	}

	// Only a Supervisor-side rejection (e.g. the store form not existing on this
	// version) warrants trying the legacy form; a transport error is terminal.
	var se *supervisorError
	if !errors.As(err, &se) {
		return err
	}

	if _, ferr := c.supervisorAPI(ctx, "post", "/addons/"+slug+"/"+action, payload, installTimeout); ferr != nil {
		return fmt.Errorf("addon %s %q: %w", action, slug, ferr)
	}

	return nil
}

// SetAddonOptions writes an add-on's user options.
func (c *Client) SetAddonOptions(ctx context.Context, slug string, options map[string]any) error {
	_, err := c.supervisorAPI(ctx, "post", "/addons/"+slug+"/options", map[string]any{"options": options}, supervisorCallTimeout)

	return err
}

// SetAddonAutoUpdate toggles an add-on's auto-update flag.
func (c *Client) SetAddonAutoUpdate(ctx context.Context, slug string, enabled bool) error {
	_, err := c.supervisorAPI(ctx, "post", "/addons/"+slug+"/options", map[string]any{"auto_update": enabled}, supervisorCallTimeout)

	return err
}

// StartAddon starts an installed add-on.
func (c *Client) StartAddon(ctx context.Context, slug string) error {
	_, err := c.supervisorAPI(ctx, "post", "/addons/"+slug+"/start", nil, supervisorCallTimeout)

	return err
}

// RestartAddon restarts an installed add-on. Home Assistant loads an add-on's
// options only at container start, so a config change written to an already
// running add-on takes effect only after a restart.
func (c *Client) RestartAddon(ctx context.Context, slug string) error {
	_, err := c.supervisorAPI(ctx, "post", "/addons/"+slug+"/restart", nil, supervisorCallTimeout)

	return err
}

// OSInfo reports the running vs latest Home Assistant OS version.
func (c *Client) OSInfo(ctx context.Context) (VersionInfo, error) {
	return c.versionInfo(ctx, "/os/info")
}

// CoreInfo reports the running vs latest Home Assistant Core version.
func (c *Client) CoreInfo(ctx context.Context) (VersionInfo, error) {
	return c.versionInfo(ctx, "/core/info")
}

func (c *Client) versionInfo(ctx context.Context, path string) (VersionInfo, error) {
	data, err := c.supervisorAPI(ctx, "get", path, nil, supervisorCallTimeout)
	if err != nil {
		return VersionInfo{}, err
	}

	var info struct {
		Version string `json:"version"`
		Latest  string `json:"version_latest"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return VersionInfo{}, fmt.Errorf("version info %q: decode: %w", path, err)
	}

	return VersionInfo{Version: info.Version, Latest: info.Latest}, nil
}

// UpdateOS converges Home Assistant OS to a version (slow: forwards
// installTimeout server-side).
func (c *Client) UpdateOS(ctx context.Context, version string) error {
	_, err := c.supervisorAPI(ctx, "post", "/os/update", map[string]string{"version": version}, installTimeout)

	return err
}

// UpdateCore converges Home Assistant Core to a version (slow: forwards
// installTimeout server-side).
func (c *Client) UpdateCore(ctx context.Context, version string) error {
	_, err := c.supervisorAPI(ctx, "post", "/core/update", map[string]string{"version": version}, installTimeout)

	return err
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
