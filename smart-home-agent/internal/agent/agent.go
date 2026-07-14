// Package agent is the orchestration layer: it wires the HA bridge and the MQTT
// client to the frozen contract, implementing both directions.
//
// Uplink  (HA -> cloud): state_changed events for mapped entities become
//
//	homes/{uid}/state/{device_uid} messages carrying the applied version;
//	liveness is the retained status topic + last-will.
//
// Downlink (cloud -> agent):
//   - homes/{uid}/cmd          one-shot HA service call, TTL-gated, cmd/ack'd.
//   - homes/{uid}/shadow/desired retained desired state, applied then reported
//     back by version on the state topic (no separate ack).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/smart-home/edge/agent/internal/config"
	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/entitymap"
	"github.com/smart-home/edge/agent/internal/ha"
	"github.com/smart-home/edge/agent/internal/localmqtt"
	"github.com/smart-home/edge/agent/internal/mqtt"
	"github.com/smart-home/edge/agent/internal/pairing"
	"github.com/smart-home/edge/agent/internal/provision"
	"github.com/smart-home/edge/agent/internal/supervisor"
	"github.com/smart-home/edge/agent/internal/webui"
)

const (
	uplinkBufferLimit = 512
	cmdDedupeLimit    = 1024
	// inventoryRefreshInterval is the backstop that re-checks the HA inventory
	// even if a registry-change event was missed; the hash-diff makes an
	// unchanged refresh a no-op, so it is cheap.
	inventoryRefreshInterval = 5 * time.Minute

	// connectMaxBackoff caps the reconnect backoff for transient broker/network
	// failures.
	connectMaxBackoff = 30 * time.Second
	// recoverCooldown throttles credential recovery so a cloud outage (which can
	// look like an auth failure on some brokers) cannot trigger a re-provision
	// storm — at most one recovery attempt per cool-down.
	recoverCooldown = 30 * time.Second

	// claimNotificationID is the stable id of the HA persistent notification the
	// agent posts while unclaimed, so it can update or dismiss the same one
	// rather than stacking duplicates.
	claimNotificationID = "smart_home_claim_code"
)

// Agent binds the HA bridge and broker connection to the contract.
//
// A gateway starts UNCLAIMED: it maintains only its liveness `status` and
// listens on the retained `config` topic. The cloud pushes the config doc on
// claim (claimed=true + the entity_map), at which point the agent subscribes to
// command/shadow downlink and begins device uplink. `mu` guards the claim-driven
// runtime state, which the config handler (a broker-callback goroutine) mutates
// while the HA/run loop reads it.
type Agent struct {
	cfg  *config.Config
	log  *slog.Logger
	ha   *ha.Client
	prov *provision.Client

	broker   *mqtt.Client
	versions *versionStore
	buffer   *uplinkBuffer
	dedupe   *cmdDedupe

	// sup drives the Supervisor API for cloud-orchestrated self-update; nil when
	// the agent is not running inside HAOS (no SUPERVISOR_TOKEN). release tracks
	// the release the agent has converged to; releaseMu/converging serialize the
	// (slow, reboot-prone) convergence so overlapping retained redeliveries never
	// double-run it.
	sup        *supervisor.Client
	release    *releaseStore
	releaseMu  sync.Mutex
	converging bool

	// reconnect asks the run loop to cycle the broker connection (buffered,
	// depth 1, coalesced). The manual re-provision path signals it after
	// rotating the password so the run loop reconnects with the new credential;
	// Paho's connection-lost handler does NOT fire on a deliberate Disconnect(),
	// so this is the only way to drive an intentional reconnect.
	reconnect chan struct{}

	configPath string

	// credsMu guards the mutable credential fields on creds (mqtt username /
	// password / provision token / claim code), which recovery rotates while
	// the broker credentials-provider and the claim-code surfacing read them.
	// creds.UID is immutable across recovery (a recovered unit keeps its uid),
	// so UID is read without the lock.
	credsMu sync.RWMutex
	creds   *provision.Credentials

	// recoverMu serializes credential recovery and holds the last-attempt time
	// for the cool-down.
	recoverMu   sync.Mutex
	lastRecover time.Time

	mu            sync.RWMutex
	emap          entitymap.Map
	claimed       bool
	configVersion int
	inventoryHash string

	// pairing serializes device-pairing sessions (Zigbee2MQTT over the
	// gateway-local broker); nil when no local broker is reachable, in which
	// case pairing commands ack failed with a `failed` pairing event.
	pairing *pairing.Manager
}

// New builds an agent from resolved credentials and dependencies. It restores
// the last cloud config from disk so an already-claimed gateway boots straight
// into claimed mode (and offline boots keep their entity_map) before the
// retained config doc re-arrives on connect. The provisioning client is kept so
// the agent can re-provision itself when the broker rejects a stale password.
func New(cfg *config.Config, log *slog.Logger, creds *provision.Credentials, haClient *ha.Client, prov *provision.Client) *Agent {
	a := &Agent{
		cfg:        cfg,
		log:        log,
		creds:      creds,
		ha:         haClient,
		prov:       prov,
		emap:       entitymap.Load(cfg.ConfigDir),
		versions:   newVersionStore(cfg.DataDir),
		release:    newReleaseStore(cfg.DataDir),
		buffer:     newUplinkBuffer(uplinkBufferLimit),
		dedupe:     newCmdDedupe(cmdDedupeLimit),
		reconnect:  make(chan struct{}, 1),
		configPath: configStatePath(cfg.DataDir),
	}

	// Inside HAOS the Supervisor injects a token; that (plus hassio_role:
	// manager) is what lets the agent self-update to a cloud-pushed release.
	// Outside HAOS (local dev) there is no token and release handling is inert.
	if cfg.SupervisorToken != "" {
		a.sup = supervisor.New(
			&supervisor.HTTPTransport{BaseURL: cfg.SupervisorBaseURL, Token: cfg.SupervisorToken},
			releaseCallTimeout, releaseInstallTimeout,
		)
	}

	a.restoreConfigState()

	return a
}

// Run connects to the broker, starts the HA event pump, and blocks until ctx is
// cancelled, at which point it disconnects gracefully. Reconnection is driven
// here (not by Paho) so a broker credential rejection can trigger a
// re-provision before the next attempt rather than looping on a dead password.
func (a *Agent) Run(ctx context.Context) error {
	// Serve the Ingress status/actions panel. It comes up as soon as
	// provisioning completes (this Run is reached only after EnsureProvisioned),
	// and shuts down with ctx. A failure to bind is logged but never fatal — the
	// panel is an operator convenience, not part of the device contract.
	go func() {
		if err := webui.Serve(ctx, fmt.Sprintf(":%d", a.cfg.WebUIPort), a, a.log); err != nil {
			a.log.Error("web UI server stopped", "error", err)
		}
	}()

	// Best-effort: pairing needs the gateway-local broker (Z2M's bus); its
	// absence must never block the cloud bridge.
	a.initPairing(ctx)

	a.broker = mqtt.New(mqtt.Options{
		Host:                a.cfg.MQTTHost,
		Port:                a.cfg.MQTTPort,
		TLS:                 a.cfg.MQTTTLS,
		TLSInsecure:         a.cfg.MQTTTLSInsecure,
		GatewayUID:          a.creds.UID,
		CredentialsProvider: a.brokerCredentials,
		OnConnect:           a.onBrokerConnect,
	}, a.log)

	if err := a.connectBroker(ctx); err != nil {
		return err
	}
	defer a.broker.Disconnect()

	events := a.ha.SubscribeStateChanges(ctx)
	registryChanges := a.ha.SubscribeRegistryChanges(ctx)

	ticker := time.NewTicker(inventoryRefreshInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			a.log.Info("shutting down agent")

			return nil
		case err := <-a.broker.Lost():
			a.log.Warn("broker connection lost; reconnecting", "error", err)

			if rerr := a.connectBroker(ctx); rerr != nil {
				return rerr
			}
		case <-a.reconnect:
			a.log.Info("reconnect requested; cycling broker connection")
			a.broker.Disconnect()

			if rerr := a.connectBroker(ctx); rerr != nil {
				return rerr
			}
		case sc, ok := <-events:
			if !ok {
				return nil
			}

			a.handleStateChange(sc)
		case _, ok := <-registryChanges:
			if !ok {
				return nil
			}

			a.publishInventory(ctx)
		case <-ticker.C:
			a.publishInventory(ctx)
		}
	}
}

// connectBroker (re)establishes the broker connection. On a credential
// rejection (a stale password after a cloud-side rotation) it re-provisions via
// the provision token and retries immediately with the rotated password; on a
// transient failure it backs off. It returns a non-nil error only when ctx is
// cancelled, so the run loop treats any return as shutdown.
func (a *Agent) connectBroker(ctx context.Context) error {
	backoff := time.Second

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := a.broker.Connect()
		if err == nil {
			return nil
		}

		if mqtt.IsAuthError(err) {
			a.log.Warn("broker rejected credentials; attempting recovery", "error", err)

			if a.recoverCredentials(ctx) {
				// Retry immediately with the rotated password.
				backoff = time.Second

				continue
			}
		} else {
			a.log.Warn("broker connect failed; will retry", "error", err, "backoff", backoff.String())
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}

		if backoff < connectMaxBackoff {
			backoff *= 2
			if backoff > connectMaxBackoff {
				backoff = connectMaxBackoff
			}
		}
	}
}

// triggerReconnect asks the run loop to cycle the broker connection. Coalesced:
// if a request is already pending it is a no-op, so repeated manual
// re-provisions never queue a backlog of reconnects.
func (a *Agent) triggerReconnect() {
	select {
	case a.reconnect <- struct{}{}:
	default:
	}
}

// recoverCredentials re-provisions this gateway by its stored provision token,
// rotating the MQTT password so the next connect uses fresh credentials. A
// cool-down bounds how often it runs so a cloud outage cannot cause a
// re-provision storm. Returns true when credentials were refreshed. Safe to
// call concurrently; the recoverMu serializes attempts.
func (a *Agent) recoverCredentials(ctx context.Context) bool {
	a.recoverMu.Lock()
	defer a.recoverMu.Unlock()

	if wait := recoverCooldown - time.Since(a.lastRecover); wait > 0 {
		a.log.Info("recovery cool-down active; delaying re-provision", "wait", wait.String())

		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
	}

	a.lastRecover = time.Now()

	creds, err := a.prov.Recover(ctx, a.provisionMeta(ctx))
	if err != nil {
		a.log.Error("credential recovery failed", "error", err)

		return false
	}

	a.applyRecoveredCredentials(creds)
	a.log.Info("recovered gateway credentials; reconnecting", "mqtt_username", creds.MQTTUsername)

	// A recovery while unclaimed may reissue the claim code — resurface it so a
	// user watching HA sees the current code.
	if creds.ClaimStatus != "claimed" {
		a.surfaceClaimCode(ctx)
	}

	return true
}

// provisionMeta assembles the software inventory reported on (re)provision.
func (a *Agent) provisionMeta(ctx context.Context) provision.Metadata {
	return provision.Metadata{
		HAVersion:    a.ha.Version(ctx),
		AgentVersion: a.cfg.AgentVersion,
	}
}

// brokerCredentials returns the current MQTT username/password for Paho's
// credentials provider, read under the lock so a concurrent recovery is safe.
func (a *Agent) brokerCredentials() (string, string) {
	a.credsMu.RLock()
	defer a.credsMu.RUnlock()

	return a.creds.MQTTUsername, a.creds.MQTTPassword
}

// applyRecoveredCredentials copies the rotated secrets from a recovery into the
// live credentials in place (UID is unchanged), under the lock. The provision
// client has already persisted them to /data.
func (a *Agent) applyRecoveredCredentials(nc *provision.Credentials) {
	a.credsMu.Lock()
	defer a.credsMu.Unlock()

	a.creds.MQTTUsername = nc.MQTTUsername
	a.creds.MQTTPassword = nc.MQTTPassword
	a.creds.ProvisionToken = nc.ProvisionToken
	a.creds.ClaimStatus = nc.ClaimStatus
	a.creds.ClaimCode = nc.ClaimCode
	a.creds.ClaimCodeExpiresAt = nc.ClaimCodeExpiresAt
}

// onBrokerConnect runs on every (re)connect: announce online and subscribe to
// the always-readable config topic. Command/shadow downlink and device uplink
// are gated on the claim state — an unclaimed gateway does nothing but keep its
// liveness and wait for the retained config doc. With a persistent session the
// broker redelivers any QoS-1 downlink buffered while we were gone, which our
// handlers reconcile idempotently.
func (a *Agent) onBrokerConnect(c *mqtt.Client) {
	c.PublishAvailability(contract.StatusOnline)

	if err := c.Subscribe(contract.ConfigTopic(a.creds.UID), 1, a.handleConfig); err != nil {
		a.log.Error("subscribe config failed", "error", err)
	}

	if a.isClaimed() {
		a.activateClaimed(c)
	}

	for _, p := range a.buffer.drain() {
		_ = c.Publish(p.topic, p.payload, p.qos, p.retain)
	}
}

// activateClaimed subscribes to the command/shadow downlink and (re)publishes
// the device inventory. Safe to call on every claimed (re)connect. The shadow
// subscription is the per-device wildcard; the handler routes each retained
// document by its device_uid.
func (a *Agent) activateClaimed(c *mqtt.Client) {
	if err := c.Subscribe(contract.CommandTopic(a.creds.UID), 1, a.handleCommand); err != nil {
		a.log.Error("subscribe cmd failed", "error", err)
	}

	if err := c.Subscribe(contract.DesiredShadowFilter(a.creds.UID), 1, a.handleDesiredShadow); err != nil {
		a.log.Error("subscribe shadow failed", "error", err)
	}

	// Cloud-orchestrated fleet updates: the retained target-release doc drives
	// the agent's self-update. Claimed-only (matches the broker ACL), so it lives
	// here rather than in the always-on config subscription.
	if err := c.Subscribe(contract.ReleaseDesiredTopic(a.creds.UID), 1, a.handleRelease); err != nil {
		a.log.Error("subscribe release failed", "error", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a.publishInventory(ctx)

	// Report the running software inventory + converged release so the cloud's
	// gateway versions reflect what is actually running after any update/reboot.
	a.publishVersions(ctx)

	// Re-assert HA's current state for every mapped device so reported_state
	// self-heals after a reconnect/restart. Off the callback goroutine: it does
	// one HA GET per device and must not block the broker connect handler.
	go a.reconcileReportedState()
}

// deactivateClaimed reverses activateClaimed when the gateway is unclaimed
// (its cloud home was deleted): unsubscribe from the command/shadow downlink so
// the agent stops all device work and reverts to availability-only. The
// inventory hash is reset so a later re-claim republishes a fresh inventory
// even if the process never restarted. The entity map itself is already cleared
// by the (empty) config doc that drove the unclaim.
func (a *Agent) deactivateClaimed(c *mqtt.Client) {
	if err := c.Unsubscribe(contract.CommandTopic(a.creds.UID), contract.DesiredShadowFilter(a.creds.UID)); err != nil {
		a.log.Error("unsubscribe on unclaim failed", "error", err)
	}

	a.mu.Lock()
	a.inventoryHash = ""
	a.mu.Unlock()
}

// handleStateChange turns an HA state_changed for a mapped entity into an uplink
// state message. The version echoes the last desired_version we applied for that
// device (omitted when we have never applied one — pure telemetry).
func (a *Agent) handleStateChange(sc ha.StateChange) {
	if !a.isClaimed() {
		return
	}

	deviceUID := a.deviceForEntity(sc.EntityID)
	if deviceUID == "" {
		return
	}

	var version *int
	if v, ok := a.versions.get(deviceUID); ok {
		version = &v
	}

	payload := contract.StatePayload{
		State: map[string]any{
			"state":      sc.State,
			"attributes": sc.Attributes,
		},
		Version: version,
		// RFC3339Nano (sub-second), not RFC3339: `ts` is the cloud's tiebreaker
		// for ordering same-version telemetry, and rapid toggles land inside the
		// same wall-clock second. Second precision made them collide, so the
		// cloud dropped every toggle after the first. See docs/mqtt-contract.md.
		TS: time.Now().UTC().Format(time.RFC3339Nano),
	}

	a.publishUplink(contract.StateTopic(a.creds.UID, deviceUID), payload, false)
}

// handleCommand applies a one-shot downlink command: TTL-gate it, actuate via
// HA, and ack. Idempotent on cmd_id.
func (a *Agent) handleCommand(_ string, raw []byte) {
	var cmd contract.CommandPayload
	if err := json.Unmarshal(raw, &cmd); err != nil {
		a.log.Warn("cmd decode failed", "error", err)

		return
	}

	if cmd.CmdID == "" {
		return
	}

	if !a.dedupe.firstSee(cmd.CmdID) {
		a.log.Debug("cmd already processed, ignoring redelivery", "cmd_id", cmd.CmdID)

		return
	}

	// TTL gate: the MQTT-3.1.1 substitute for message expiry. Past the TTL we
	// drop it; the cloud independently marks it expired.
	if a.commandExpired(cmd) {
		a.log.Info("cmd expired before apply, dropping", "cmd_id", cmd.CmdID)

		return
	}

	// Gateway-scoped pairing commands carry no device_uid and never touch the
	// entity map — route them before the device lookup.
	if strings.HasPrefix(cmd.Action, "pairing.") {
		a.handlePairingCommand(cmd)

		return
	}

	entityID := a.entityForDevice(cmd.DeviceUID)
	if entityID == "" {
		a.log.Warn("cmd for unmapped device", "device_uid", cmd.DeviceUID, "cmd_id", cmd.CmdID)
		a.ackCommand(cmd.CmdID, contract.AckFailed)

		return
	}

	domain, service, ok := splitAction(cmd.Action)
	if !ok {
		a.ackCommand(cmd.CmdID, contract.AckFailed)

		return
	}

	data := map[string]any{"entity_id": entityID}
	for k, v := range cmd.Params {
		data[k] = v
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := a.ha.CallService(ctx, domain, service, data); err != nil {
		a.log.Error("cmd service call failed", "cmd_id", cmd.CmdID, "action", cmd.Action, "error", err)
		a.ackCommand(cmd.CmdID, contract.AckFailed)

		return
	}

	a.ackCommand(cmd.CmdID, contract.AckAcked)
}

// handleDesiredShadow applies the retained desired state (converging by version)
// and then reports HA's ACTUAL resulting state back on the state topic, carrying
// the applied version. It deliberately does NOT echo the desired document as the
// reported state: reported_state must always be HA truth, never a copy of what
// the cloud wanted. Convergence is still by version (reported_version ==
// desired_version); the state topic just now carries the real HA state alongside
// that version so desired and reported are directly comparable.
func (a *Agent) handleDesiredShadow(_ string, raw []byte) {
	var shadow contract.DesiredShadowPayload
	if err := json.Unmarshal(raw, &shadow); err != nil {
		a.log.Warn("shadow decode failed", "error", err)

		return
	}

	if shadow.DeviceUID == "" {
		return
	}

	if applied, ok := a.versions.get(shadow.DeviceUID); ok && shadow.DesiredVersion <= applied {
		// Already converged; re-report HA's actual state so the cloud self-heals
		// if it missed the last report (never re-echo the desired document).
		a.reportDeviceState(shadow.DeviceUID)

		return
	}

	entityID := a.entityForDevice(shadow.DeviceUID)
	if entityID == "" {
		a.log.Warn("shadow for unmapped device", "device_uid", shadow.DeviceUID)

		return
	}

	if calls := translateDesired(entityID, shadow.State); len(calls) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		for _, call := range calls {
			if err := a.ha.CallService(ctx, call.domain, call.service, call.data); err != nil {
				// Leave the device unconverged (do not record the version) so the
				// divergence stays visible rather than being papered over. A
				// partial multi-call apply (e.g. mode set, temperature failed)
				// also stays unconverged and is retried on the next delivery.
				a.log.Error("shadow apply failed", "device_uid", shadow.DeviceUID, "service", call.domain+"."+call.service, "error", err)

				return
			}
		}
	}

	// Record convergence only after a successful apply, then report HA's real
	// resulting state carrying the applied version. The state_changed the apply
	// triggers may also arrive; it carries the same real state, so the cloud's
	// monotonic ingest treats the pair idempotently.
	a.versions.set(shadow.DeviceUID, shadow.DesiredVersion)
	a.reportDeviceState(shadow.DeviceUID)
}

// reportDeviceState publishes HA's ACTUAL current state for a mapped device on
// the uplink state topic, stamped with the applied desired_version (if any) so
// the cloud can declare convergence. This is the read-back that replaces echoing
// the desired document, so reported_state is always HA truth. Best-effort: a
// fetch failure is logged and skipped (the next state_changed re-reports it).
func (a *Agent) reportDeviceState(deviceUID string) {
	entityID := a.entityForDevice(deviceUID)
	if entityID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := a.ha.State(ctx, entityID)
	if err != nil {
		a.log.Warn("report state fetch failed", "device_uid", deviceUID, "entity_id", entityID, "error", err)

		return
	}

	var version *int
	if v, ok := a.versions.get(deviceUID); ok {
		version = &v
	}

	payload := contract.StatePayload{
		State: map[string]any{
			"state":      st.State,
			"attributes": st.Attributes,
		},
		Version: version,
		// RFC3339Nano (sub-second) so the cloud's same-version `ts` tiebreaker can
		// order reports that fall inside the same second (see handleStateChange).
		TS: time.Now().UTC().Format(time.RFC3339Nano),
	}

	a.publishUplink(contract.StateTopic(a.creds.UID, deviceUID), payload, false)
}

// reconcileReportedState publishes HA's current state for every mapped device.
// Run on each claimed (re)connect: the state pump only fires on state CHANGES,
// so without this a device that has not changed since the agent (re)started
// would never re-assert its current value and the cloud's reported_state could
// stay stale. Runs off the broker-callback goroutine (one HA GET per device).
func (a *Agent) reconcileReportedState() {
	a.mu.RLock()
	devices := a.emap.Devices()
	a.mu.RUnlock()

	for _, deviceUID := range devices {
		a.reportDeviceState(deviceUID)
	}
}

// PublishEvent emits an uplink event/{type} (deduped cloud-side on event_id). A
// boot event exercises the event path end-to-end during the spike.
func (a *Agent) PublishEvent(eventType, eventID string, severity contract.Severity, deviceUID string) {
	if !a.isClaimed() {
		// Events from an unclaimed gateway have nowhere to attach (and the
		// broker ACL would reject them); drop rather than buffer indefinitely.
		return
	}

	payload := contract.EventPayload{
		EventID:   eventID,
		Severity:  severity,
		DeviceUID: deviceUID,
		TS:        time.Now().UTC().Format(time.RFC3339),
	}

	a.publishUplink(contract.EventTopic(a.creds.UID, eventType), payload, false)
}

// handlePairingCommand routes pairing.start/stop to the manager. Gateway-
// scoped: no device_uid, no entity map. Every failure emits a `failed`
// pairing event (so the wizard sees why) in addition to the command ack.
func (a *Agent) handlePairingCommand(cmd contract.CommandPayload) {
	sessionID, _ := cmd.Params["session_id"].(string)
	if sessionID == "" {
		a.ackCommand(cmd.CmdID, contract.AckFailed)

		return
	}

	if a.pairing == nil {
		a.emitPairingEvent(sessionID, pairing.Event{Phase: contract.PairingPhaseFailed, Reason: "pairing unavailable on this gateway"})
		a.ackCommand(cmd.CmdID, contract.AckFailed)

		return
	}

	switch cmd.Action {
	case "pairing.start":
		durationSec := 120
		if v, ok := cmd.Params["duration_sec"].(float64); ok && v > 0 {
			durationSec = int(v)
		}

		if err := a.pairing.Start(sessionID, durationSec); err != nil {
			reason := "pairing start failed"
			if errors.Is(err, pairing.ErrBusy) {
				reason = "another pairing session is active"
			}
			a.emitPairingEvent(sessionID, pairing.Event{Phase: contract.PairingPhaseFailed, Reason: reason})
			a.ackCommand(cmd.CmdID, contract.AckFailed)

			return
		}
		a.ackCommand(cmd.CmdID, contract.AckAcked)
	case "pairing.stop":
		a.pairing.Stop(sessionID, "user")
		a.ackCommand(cmd.CmdID, contract.AckAcked)
	default:
		a.ackCommand(cmd.CmdID, contract.AckFailed)
	}
}

// emitPairingEvent publishes one pairing phase upstream.
func (a *Agent) emitPairingEvent(sessionID string, ev pairing.Event) {
	payload := contract.PairingEventPayload{
		SessionID: sessionID,
		Phase:     ev.Phase,
		Device:    ev.Device,
		Reason:    ev.Reason,
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
	}
	if !ev.ExpiresAt.IsZero() {
		payload.ExpiresAt = ev.ExpiresAt.UTC().Format(time.RFC3339)
	}

	a.publishUplink(contract.EventTopic(a.creds.UID, contract.PairingEventType), payload, false)
}

// initPairing wires the pairing manager to Zigbee2MQTT over the gateway-local
// broker. Pairing is optional: without local-broker access (Mac dev track, or
// a gateway with no mqtt service) the agent runs fine and pairing commands
// ack failed with reason "unavailable".
func (a *Agent) initPairing(ctx context.Context) {
	opts, err := localmqtt.Resolve(ctx)
	if err != nil {
		a.log.Info("pairing disabled: no local broker", "error", err)

		return
	}

	client, err := localmqtt.Connect(opts, a.log)
	if err != nil {
		a.log.Warn("pairing disabled: local broker connect failed", "error", err)

		return
	}

	backend := pairing.NewZ2M(client, os.Getenv("Z2M_BASE_TOPIC"), a.log)
	manager := pairing.NewManager(backend, a.emitPairingEvent, a.log)

	if err := client.Subscribe(backend.BridgeEventTopic(), func(_ string, raw []byte) {
		if ev, ok := pairing.MapBridgeEvent(raw); ok {
			manager.HandleBackendEvent(ev)
		}
	}); err != nil {
		a.log.Warn("pairing disabled: bridge/event subscribe failed", "error", err)
		client.Close()

		return
	}

	a.pairing = manager
	a.log.Info("pairing enabled", "broker", opts.Host, "bridge_topic", backend.BridgeEventTopic())
}

// ackCommand publishes a cmd/ack.
func (a *Agent) ackCommand(cmdID string, status contract.CommandAckStatus) {
	payload := contract.CommandAckPayload{CmdID: cmdID, Status: status}

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	if a.broker != nil && a.broker.IsConnected() {
		_ = a.broker.Publish(contract.CommandAckTopic(a.creds.UID), raw, 1, false)

		return
	}

	a.buffer.push(pending{topic: contract.CommandAckTopic(a.creds.UID), payload: raw, qos: 1, retain: false})
}

// publishUplink publishes a JSON uplink payload, buffering (bounded, drop-oldest)
// when the broker is offline so reconnect flushes the most recent telemetry.
func (a *Agent) publishUplink(topic string, payload any, retain bool) {
	raw, err := json.Marshal(payload)
	if err != nil {
		a.log.Warn("uplink encode failed", "topic", topic, "error", err)

		return
	}

	if a.broker != nil && a.broker.IsConnected() {
		if err := a.broker.Publish(topic, raw, 1, retain); err != nil {
			a.log.Warn("uplink publish failed, buffering", "topic", topic, "error", err)
			a.buffer.push(pending{topic: topic, payload: raw, qos: 1, retain: retain})
		}

		return
	}

	a.buffer.push(pending{topic: topic, payload: raw, qos: 1, retain: retain})
}

func (a *Agent) commandExpired(cmd contract.CommandPayload) bool {
	if cmd.TTLSec <= 0 {
		return false
	}

	issued, err := time.Parse(time.RFC3339, cmd.TS)
	if err != nil {
		// Try the broader ISO-8601 layout the cloud emits.
		issued, err = time.Parse("2006-01-02T15:04:05-07:00", cmd.TS)
		if err != nil {
			return false // cannot judge expiry; apply rather than silently drop
		}
	}

	return time.Now().After(issued.Add(time.Duration(cmd.TTLSec) * time.Second))
}

// splitAction splits a "domain.service" action into its parts. Returns ok=false
// (with empty parts) for anything that is not a non-empty domain and service
// separated by a single dot.
func splitAction(action string) (domain, service string, ok bool) {
	for i := 0; i < len(action); i++ {
		if action[i] == '.' {
			if i == 0 || i == len(action)-1 {
				return "", "", false
			}

			return action[:i], action[i+1:], true
		}
	}

	return "", "", false
}
