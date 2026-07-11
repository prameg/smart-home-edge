// Package supervisor is the single implementation of the Home Assistant
// Supervisor operations both halves of the fleet use: the smart-onboard CLI
// (which reaches Supervisor through Core's authenticated "supervisor/api"
// WebSocket command from a companion machine) and the agent add-on itself
// (which reaches Supervisor directly over http://supervisor with its injected
// SUPERVISOR_TOKEN, to update itself and the unit to the latest available).
//
// The two callers differ only in TRANSPORT (WS-via-Core vs direct HTTP), so the
// transport is an interface and every endpoint path / payload shape / result
// parse lives here exactly once. That is the whole point: onboarding and fleet
// updates drive Supervisor through one code path, so a Supervisor API change is
// fixed in one place and can never drift between the CLI and the agent.
package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/smart-home/edge/agent/fleet"
)

// Transport performs one Supervisor REST call and returns the unwrapped
// Supervisor `data` payload. Implementations own auth and framing; a
// Supervisor-side rejection (HTTP/WS success=false) must be returned as *Error
// so callers can treat an expected "not installed"/"not found" as a soft signal.
type Transport interface {
	Call(ctx context.Context, method, endpoint string, payload any, timeout time.Duration) (json.RawMessage, error)
}

// Error is a Supervisor-side failure (the call returned success=false / a 4xx),
// kept distinct from a transport/auth error so callers can treat an expected
// "not installed"/"not found" as a soft signal while still propagating real
// failures.
type Error struct {
	Method, Endpoint, Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("supervisor %s %s: %s", e.Method, e.Endpoint, e.Message)
}

// AddonInfo is an add-on's install state as Supervisor reports it.
type AddonInfo struct {
	Slug      string
	Installed bool
	// Version is the installed version ("" when not installed).
	Version string
	// State is "started" / "stopped" (empty when not installed).
	State string
	// AutoUpdate is the current auto-update flag.
	AutoUpdate bool
	// UpdateAvailable reports whether Supervisor has a newer version than the
	// installed one, so the agent can update-to-latest only when it matters.
	UpdateAvailable bool
	// Options is the add-on's current user options, used to skip a redundant
	// re-write when the desired options already match.
	Options map[string]any
}

// VersionInfo is a running-vs-latest version pair for OS / Core.
type VersionInfo struct {
	Version string
	Latest  string
}

// Client exposes the typed Supervisor operations over a Transport. callTimeout
// bounds a normal call server-side; installTimeout bounds slow mutations
// (add-on install/update, OS/Core update) that pull an image or reboot the unit.
type Client struct {
	t              Transport
	callTimeout    time.Duration
	installTimeout time.Duration
}

// New builds a Supervisor operations client over t.
func New(t Transport, callTimeout, installTimeout time.Duration) *Client {
	return &Client{t: t, callTimeout: callTimeout, installTimeout: installTimeout}
}

// StoreRepositories lists the registered add-on store repository URLs.
func (c *Client) StoreRepositories(ctx context.Context) ([]string, error) {
	data, err := c.t.Call(ctx, "get", "/store/repositories", nil, c.callTimeout)
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

// AddStoreRepository registers a repository and reloads the store so its
// add-ons become resolvable. The reload is best-effort (the add already
// refreshes); a reload failure is not fatal.
func (c *Client) AddStoreRepository(ctx context.Context, repoURL string) error {
	if _, err := c.t.Call(ctx, "post", "/store/repositories", map[string]string{"repository": repoURL}, c.callTimeout); err != nil {
		return err
	}

	_, _ = c.t.Call(ctx, "post", "/store/reload", nil, c.callTimeout)

	return nil
}

// ResolveAddonSlug returns the full Supervisor slug for a manifest add-on. Exact
// slugs (built-in core_* add-ons) bypass the store lookup; otherwise it matches
// the store, which is how a repo-hash-prefixed community/agent slug is found.
func (c *Client) ResolveAddonSlug(ctx context.Context, a fleet.Addon) (string, bool, error) {
	if a.Slug != "" {
		return a.Slug, true, nil
	}

	data, err := c.t.Call(ctx, "get", "/store", nil, c.callTimeout)
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
// info endpoint means the add-on is known to the store but not installed; a
// transport/auth error is a real failure and propagates.
func (c *Client) AddonInfo(ctx context.Context, slug string) (AddonInfo, error) {
	data, err := c.t.Call(ctx, "get", "/addons/"+slug+"/info", nil, c.callTimeout)
	if err != nil {
		var se *Error
		if errors.As(err, &se) {
			return AddonInfo{Slug: slug, Installed: false}, nil
		}

		return AddonInfo{}, err
	}

	var info struct {
		Version         string         `json:"version"`
		State           string         `json:"state"`
		AutoUpdate      bool           `json:"auto_update"`
		UpdateAvailable bool           `json:"update_available"`
		Options         map[string]any `json:"options"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return AddonInfo{}, fmt.Errorf("addon info %q: decode: %w", slug, err)
	}

	return AddonInfo{
		Slug:            slug,
		Installed:       info.Version != "",
		Version:         info.Version,
		State:           info.State,
		AutoUpdate:      info.AutoUpdate,
		UpdateAvailable: info.UpdateAvailable,
		Options:         info.Options,
	}, nil
}

// InstallAddon installs an add-on's latest version.
func (c *Client) InstallAddon(ctx context.Context, slug string) error {
	return c.addonMutate(ctx, slug, "install", nil)
}

// UpdateAddonLatest updates an installed add-on to the latest available version
// (Supervisor picks it when no version is supplied).
func (c *Client) UpdateAddonLatest(ctx context.Context, slug string) error {
	return c.addonMutate(ctx, slug, "update", nil)
}

// addonMutate posts an install/update, trying the /store/addons form first then
// the /addons form (different Supervisor versions expose one or the other). It
// forwards installTimeout server-side because a first install pulls an image.
func (c *Client) addonMutate(ctx context.Context, slug, action string, payload any) error {
	_, err := c.t.Call(ctx, "post", "/store/addons/"+slug+"/"+action, payload, c.installTimeout)
	if err == nil {
		return nil
	}

	// Only a Supervisor-side rejection (e.g. the store form not existing on this
	// version) warrants trying the legacy form; a transport error is terminal.
	var se *Error
	if !errors.As(err, &se) {
		return err
	}

	if _, ferr := c.t.Call(ctx, "post", "/addons/"+slug+"/"+action, payload, c.installTimeout); ferr != nil {
		return fmt.Errorf("addon %s %q: %w", action, slug, ferr)
	}

	return nil
}

// SetAddonOptions writes an add-on's user options.
func (c *Client) SetAddonOptions(ctx context.Context, slug string, options map[string]any) error {
	_, err := c.t.Call(ctx, "post", "/addons/"+slug+"/options", map[string]any{"options": options}, c.callTimeout)

	return err
}

// SetAddonAutoUpdate toggles an add-on's auto-update flag.
func (c *Client) SetAddonAutoUpdate(ctx context.Context, slug string, enabled bool) error {
	_, err := c.t.Call(ctx, "post", "/addons/"+slug+"/options", map[string]any{"auto_update": enabled}, c.callTimeout)

	return err
}

// StartAddon starts an installed add-on.
func (c *Client) StartAddon(ctx context.Context, slug string) error {
	_, err := c.t.Call(ctx, "post", "/addons/"+slug+"/start", nil, c.callTimeout)

	return err
}

// RestartAddon restarts an installed add-on so option changes written while it
// was running take effect (HA loads add-on options only at start).
func (c *Client) RestartAddon(ctx context.Context, slug string) error {
	_, err := c.t.Call(ctx, "post", "/addons/"+slug+"/restart", nil, c.callTimeout)

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
	data, err := c.t.Call(ctx, "get", path, nil, c.callTimeout)
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

// UpdateOSLatest updates Home Assistant OS to the latest available version
// (slow: forwards installTimeout server-side; reboots the unit).
func (c *Client) UpdateOSLatest(ctx context.Context) error {
	_, err := c.t.Call(ctx, "post", "/os/update", nil, c.installTimeout)

	return err
}

// UpdateCoreLatest updates Home Assistant Core to the latest available version
// (slow: forwards installTimeout server-side; restarts Core).
func (c *Client) UpdateCoreLatest(ctx context.Context) error {
	_, err := c.t.Call(ctx, "post", "/core/update", nil, c.installTimeout)

	return err
}

// AgentVersion reads the installed version of the agent add-on itself, used to
// report the running version and to decide whether a self-update is still
// pending. Returns "" (no error) when the add-on is not installed.
func (c *Client) AgentVersion(ctx context.Context, agentSlug string) (string, error) {
	info, err := c.AddonInfo(ctx, agentSlug)
	if err != nil {
		return "", err
	}

	return info.Version, nil
}

// normalizeVersion trims a leading "v" so tag-style and bare versions compare
// equal (the manifest and Supervisor both report bare versions, but a caller
// may hold a tag).
func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// VersionsEqual reports whether two version strings denote the same version,
// tolerating a leading "v".
func VersionsEqual(a, b string) bool {
	return normalizeVersion(a) == normalizeVersion(b)
}
