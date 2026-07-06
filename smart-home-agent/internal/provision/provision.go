// Package provision implements the device-facing first-boot / recovery
// provisioning client and the local credential store.
//
// First boot (ENROLL) and recovery (RE-PROVISION) are the SAME cloud operation —
// an idempotent upsert on the hardware serial (POST /api/v1/provisioning/
// gateways) — but they authenticate DIFFERENTLY, and that split is the whole
// point of the "one place to be wrong" model:
//
//   - ENROLL (serial unknown): the shared factory key (Bearer) authorizes
//     creating a fleet row. The cloud mints and returns, exactly once, this
//     unit's per-gateway provision token + a short claim code.
//   - RECOVER (serial known): the unit presents its own provision token in the
//     body — the TRUSTED path — and the cloud rotates just this row's MQTT
//     password. A /data-wiped unit that lost its token recovers AUTOMATICALLY on
//     the factory key alone (the token is rotated too); if that unit was already
//     claimed the cloud quarantines it (control frozen) until an owner confirms,
//     and an admin-SUSPENDED unit is refused until restored. So a factory reset
//     self-heals with no operator action in the normal case.
//
// The agent provisions when it has no locally-usable credentials (first ever
// boot, or after a factory reset wiped /data) and also re-provisions in place
// when the broker rejects stale credentials. A normal reboot reuses the stored
// credentials and never touches the endpoint (the MQTT session self-heals via
// retained topics). Re-provisioning the same serial returns the same `uid`, a
// rotated MQTT password, and preserves the existing home/claim binding, so
// pulling and reflashing a unit never orphans it.
package provision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/smart-home/edge/agent/internal/config"
)

const credentialsFile = "agent-creds.json"

const (
	// provisionMaxBackoff caps the exponential retry so a unit on a flaky WAN
	// keeps trying roughly every few minutes forever rather than fatally
	// exiting on the first failure — first boot must eventually succeed with no
	// manual restart.
	provisionInitialBackoff = 2 * time.Second
	provisionMaxBackoff     = 5 * time.Minute
)

// Credentials is the persisted result of a successful provision. The MQTT
// password and the provision token are returned by the cloud (plaintext) and
// stored here on the add-on's persistent volume; they never leave the device.
type Credentials struct {
	UID            string `json:"uid"`
	TopicNamespace string `json:"topic_namespace"`
	ClaimStatus    string `json:"claim_status"`
	MQTTUsername   string `json:"mqtt_username"`
	MQTTPassword   string `json:"mqtt_password"`
	// ProvisionToken is the per-gateway RECOVERY credential — the TRUSTED secret
	// that re-provisions this serial (rotating its MQTT password) without
	// tripping quarantine. Issued at enrollment (and re-issued whenever the
	// factory key is used to recover) and sent back on every recovery so a live
	// unit re-provisions on its own token rather than the shared factory key.
	ProvisionToken string `json:"provision_token,omitempty"`
	// ClaimCode is the short, human-typeable code shown while unclaimed; the
	// user reads it off the device and enters it to bind the gateway to a home.
	// Empty once claimed; reissued on demand while unclaimed.
	ClaimCode string `json:"claim_code,omitempty"`
	// ClaimCodeExpiresAt is the ISO-8601 expiry of ClaimCode, so the agent can
	// proactively reissue an expired code instead of surfacing a dead one.
	ClaimCodeExpiresAt string    `json:"claim_code_expires_at,omitempty"`
	Serial             string    `json:"serial"`
	ProvisionedAt      time.Time `json:"provisioned_at"`
}

// provisionResponse mirrors GatewayProvisionController::store's JSON body.
type provisionResponse struct {
	UID            string `json:"uid"`
	TopicNamespace string `json:"topic_namespace"`
	ClaimStatus    string `json:"claim_status"`
	MQTT           struct {
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"mqtt"`
	// ProvisionToken is non-empty only when freshly issued (enroll / re-enroll);
	// on a normal token recovery the device already holds it and it is not
	// resent, so an empty value here must NOT clobber the stored token.
	ProvisionToken     string `json:"provision_token"`
	ClaimCode          string `json:"claim_code"`
	ClaimCodeExpiresAt string `json:"claim_code_expires_at"`
}

// claimCodeResponse mirrors GatewayProvisionController::claimCode.
type claimCodeResponse struct {
	ClaimCode          string `json:"claim_code"`
	ClaimCodeExpiresAt string `json:"claim_code_expires_at"`
}

// Metadata is the optional gateway software inventory reported at provision time.
type Metadata struct {
	HAVersion    string
	AgentVersion string
	OSVersion    string
}

// Error is a structured cloud rejection carrying the machine-readable error
// code (see App\Exceptions\ProvisioningException) so the agent can branch on
// WHY it was rejected rather than treating every non-2xx the same.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("provision: cloud rejected request (%d %s): %s", e.Status, e.Code, e.Message)
	}

	return fmt.Sprintf("provision: unexpected status %d", e.Status)
}

// NeedsOperator reports the codes that will not clear on their own no matter how
// many times the agent retries — they need a human: a wrong/absent factory key
// (config), a serial with neither a valid token nor the factory key, or a unit
// an admin has suspended (killswitch). The agent still keeps retrying (an
// operator fix makes a later attempt succeed) but logs these prominently so the
// wait is explained, not silent.
func (e *Error) NeedsOperator() bool {
	return e.Code == "factory_key_required" ||
		e.Code == "recovery_not_authorized" ||
		e.Code == "gateway_suspended"
}

// Client provisions against the cloud and owns the on-disk credential store.
type Client struct {
	cfg  *config.Config
	http *http.Client
	log  *slog.Logger
}

// New builds a provisioning client with a bounded HTTP timeout.
func New(cfg *config.Config, log *slog.Logger) *Client {
	return &Client{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second},
		log:  log,
	}
}

// EnsureProvisioned returns locally-stored credentials when present, otherwise
// provisions and persists the result, retrying with backoff until it succeeds
// or ctx is cancelled. This is the normal boot path: a unit that has valid
// /data creds returns instantly; a fresh unit blocks here (never fatally exits)
// until the cloud/network lets it enroll.
func (c *Client) EnsureProvisioned(ctx context.Context, meta Metadata) (*Credentials, error) {
	if creds, err := c.load(); err == nil && creds.isUsable() {
		return creds, nil
	}

	return c.provisionWithRetry(ctx, meta)
}

// provisionWithRetry loops Recover with capped exponential backoff. It returns
// only on success or ctx cancellation; every failure is logged with the parsed
// reason so an operator watching the add-on log sees exactly what is blocking
// first boot.
func (c *Client) provisionWithRetry(ctx context.Context, meta Metadata) (*Credentials, error) {
	backoff := provisionInitialBackoff

	for attempt := 1; ; attempt++ {
		creds, err := c.Recover(ctx, meta)
		if err == nil {
			return creds, nil
		}

		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		c.logProvisionFailure(attempt, backoff, err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}

		if backoff < provisionMaxBackoff {
			backoff *= 2
			if backoff > provisionMaxBackoff {
				backoff = provisionMaxBackoff
			}
		}
	}
}

func (c *Client) logProvisionFailure(attempt int, backoff time.Duration, err error) {
	var provErr *Error
	if errors.As(err, &provErr) && provErr.NeedsOperator() {
		// A human must act (fix the factory key, or restore a suspended unit on
		// the fleet page). Keep retrying so it self-heals once they do.
		c.log.Warn("provisioning is waiting on an operator action; will keep retrying",
			"code", provErr.Code,
			"message", provErr.Message,
			"attempt", attempt,
			"retry_in", backoff.String(),
		)

		return
	}

	c.log.Warn("provisioning attempt failed; will retry",
		"error", err,
		"attempt", attempt,
		"retry_in", backoff.String(),
	)
}

// Recover performs a single (re)provision against the cloud and overwrites local
// credentials on success. It is the one-shot primitive behind both first-boot
// enrollment (via EnsureProvisioned's retry loop) and the broker-rejected
// self-recovery path in the agent. It sends the stored provision token (when we
// have one) so the cloud rotates THIS row rather than demanding the factory key,
// and preserves the token/claim code the cloud does not resend.
func (c *Client) Recover(ctx context.Context, meta Metadata) (*Credentials, error) {
	if c.cfg.CloudBaseURL == "" {
		return nil, fmt.Errorf("provision: cloud_base_url is required to provision (or pre-seed %s in the data dir for a local test)", credentialsFile)
	}

	// Best-effort: an existing store gives us this unit's provision token (and
	// the claim code to preserve on a recovery that does not reissue one).
	existing, _ := c.load()

	payload := map[string]string{"serial": c.cfg.Serial}
	if existing != nil && existing.ProvisionToken != "" {
		payload["provision_token"] = existing.ProvisionToken
	}
	if meta.HAVersion != "" {
		payload["ha_version"] = meta.HAVersion
	}
	if meta.AgentVersion != "" {
		payload["agent_version"] = meta.AgentVersion
	}
	if meta.OSVersion != "" {
		payload["os_version"] = meta.OSVersion
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("provision: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.CloudURL("/api/v1/provisioning/gateways"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provision: build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The factory key is always presented when configured; the cloud consults it
	// to ENROLL a new serial or to recover a token-less one, and ignores it on a
	// normal token recovery — so sending it is safe and lets a first-boot or
	// factory-reset unit (which has no token yet) provision through this path.
	if c.cfg.FactoryKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.FactoryKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provision: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}

	var decoded provisionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("provision: decode response: %w", err)
	}

	creds := &Credentials{
		UID:                decoded.UID,
		TopicNamespace:     decoded.TopicNamespace,
		ClaimStatus:        decoded.ClaimStatus,
		MQTTUsername:       decoded.MQTT.Username,
		MQTTPassword:       decoded.MQTT.Password,
		ProvisionToken:     decoded.ProvisionToken,
		ClaimCode:          decoded.ClaimCode,
		ClaimCodeExpiresAt: decoded.ClaimCodeExpiresAt,
		Serial:             c.cfg.Serial,
		ProvisionedAt:      time.Now().UTC(),
	}

	// The cloud only resends the provision token when it (re)issued one; on a
	// normal recovery it stays on the device, so carry the stored value forward
	// rather than blanking it (which would strand us with no recovery secret).
	if creds.ProvisionToken == "" && existing != nil {
		creds.ProvisionToken = existing.ProvisionToken
	}

	// Likewise the claim code is only reissued while unclaimed; if this recovery
	// did not carry a fresh one but the unit is still unclaimed, keep the last
	// known code so the surfacing path still has something to show.
	if creds.ClaimCode == "" && existing != nil && creds.ClaimStatus != "claimed" {
		creds.ClaimCode = existing.ClaimCode
		creds.ClaimCodeExpiresAt = existing.ClaimCodeExpiresAt
	}

	if !creds.isUsable() {
		return nil, fmt.Errorf("provision: response missing uid or mqtt credentials")
	}

	if err := c.save(creds); err != nil {
		return nil, fmt.Errorf("provision: persist credentials: %w", err)
	}

	return creds, nil
}

// ReissueClaimCode mints a fresh short claim code for a still-unclaimed unit,
// authenticated by the stored provision token, WITHOUT rotating MQTT creds. The
// agent calls it when the on-device code is missing or expired (e.g. after an
// unclaim cleared it cloud-side). It updates and persists the stored credentials
// in place.
func (c *Client) ReissueClaimCode(ctx context.Context) (*Credentials, error) {
	existing, err := c.load()
	if err != nil || existing == nil || existing.ProvisionToken == "" {
		return nil, fmt.Errorf("provision: cannot reissue claim code without a stored provision token")
	}

	body, err := json.Marshal(map[string]string{
		"serial":          existing.Serial,
		"provision_token": existing.ProvisionToken,
	})
	if err != nil {
		return nil, fmt.Errorf("provision: encode claim-code request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.CloudURL("/api/v1/provisioning/gateways/claim-code"), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("provision: build claim-code request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provision: claim-code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}

	var decoded claimCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("provision: decode claim-code response: %w", err)
	}

	existing.ClaimCode = decoded.ClaimCode
	existing.ClaimCodeExpiresAt = decoded.ClaimCodeExpiresAt

	if err := c.save(existing); err != nil {
		return nil, fmt.Errorf("provision: persist reissued claim code: %w", err)
	}

	return existing, nil
}

// decodeError turns a non-2xx provisioning response into a structured *Error,
// reading the cloud's `{error, message}` body when present.
func decodeError(resp *http.Response) error {
	var body struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	return &Error{Status: resp.StatusCode, Code: body.Error, Message: body.Message}
}

func (c *Client) path() string {
	return filepath.Join(c.cfg.DataDir, credentialsFile)
}

func (c *Client) load() (*Credentials, error) {
	raw, err := os.ReadFile(c.path())
	if err != nil {
		return nil, err
	}

	var creds Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, err
	}

	return &creds, nil
}

// save writes credentials atomically (write-temp-then-rename) with 0600 perms so
// a crash mid-write never leaves a half-written credential file.
func (c *Client) save(creds *Credentials) error {
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(c.cfg.DataDir, 0o755); err != nil {
		return err
	}

	tmp := c.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, c.path())
}

func (creds *Credentials) isUsable() bool {
	return creds != nil && creds.UID != "" && creds.MQTTUsername != "" && creds.MQTTPassword != ""
}
