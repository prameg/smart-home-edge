package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/smart-home/edge/agent/internal/webui"
)

// This file implements webui.Backend: the read models and actions the Ingress
// status panel drives. Everything here composes existing agent primitives — the
// panel adds no new device behavior, only a human-facing surface. Read models
// deliberately expose NO secrets (never the MQTT password or provision token).

// Status assembles the read-only snapshot the panel renders. The HA version is
// fetched best-effort per call (empty when Core is unreachable).
func (a *Agent) Status(ctx context.Context) webui.Status {
	a.credsMu.RLock()
	serial := a.creds.Serial
	claimStatus := a.creds.ClaimStatus
	claimCode := a.creds.ClaimCode
	claimExpires := a.creds.ClaimCodeExpiresAt
	a.credsMu.RUnlock()

	a.mu.RLock()
	claimed := a.claimed
	configVersion := a.configVersion
	deviceCount := a.emap.Len()
	a.mu.RUnlock()

	status := webui.Status{
		UID:             a.creds.UID, // immutable across recovery; safe to read unlocked
		Serial:          serial,
		AgentVersion:    a.cfg.AgentVersion,
		HAVersion:       a.ha.Version(ctx),
		CloudBaseURL:    a.cfg.CloudBaseURL,
		MQTTHost:        a.cfg.MQTTHost,
		MQTTPort:        a.cfg.MQTTPort,
		MQTTTLS:         a.cfg.MQTTTLS,
		Claimed:         claimed,
		ClaimStatus:     claimStatus,
		BrokerConnected: a.broker != nil && a.broker.IsConnected(),
		ConfigVersion:   configVersion,
		DeviceCount:     deviceCount,
	}

	// The claim code is only meaningful (and only present) while unclaimed.
	if !claimed {
		status.ClaimCode = claimCode
		status.ClaimCodeExpiresAt = claimExpires
	}

	return status
}

// Devices lists the mapped device<->entity bindings with each entity's live HA
// state. The entity_map is copied under the lock, then HA is queried per device
// off-lock so a slow Core never blocks the config/command handlers.
func (a *Agent) Devices(ctx context.Context) []webui.DeviceRow {
	a.mu.RLock()
	entries := a.emap.Entries()
	a.mu.RUnlock()

	rows := make([]webui.DeviceRow, 0, len(entries))
	for _, e := range entries {
		row := webui.DeviceRow{DeviceUID: e.DeviceUID, EntityID: e.EntityID}

		if st, err := a.ha.State(ctx, e.EntityID); err != nil {
			row.Error = "unavailable"
		} else {
			row.State = st.State
			if !st.LastChanged.IsZero() {
				row.LastChanged = st.LastChanged.UTC().Format(time.RFC3339)
			}
		}

		rows = append(rows, row)
	}

	return rows
}

// ReissueClaimCode mints a fresh claim code and resurfaces it (log + HA
// notification). Refused once claimed — a bound gateway has no claim code.
func (a *Agent) ReissueClaimCode(ctx context.Context) error {
	if a.isClaimed() {
		return fmt.Errorf("gateway is already claimed")
	}

	if a.prov == nil {
		return fmt.Errorf("provisioning client unavailable")
	}

	creds, err := a.prov.ReissueClaimCode(ctx)
	if err != nil {
		return err
	}

	a.applyRecoveredCredentials(creds)
	a.surfaceClaimCode(ctx)

	return nil
}

// RepublishInventory forces a fresh inventory publish. The inventory publisher
// hash-diffs to skip unchanged sets, so the manual path resets the hash first;
// otherwise a user-triggered refresh would be a silent no-op.
func (a *Agent) RepublishInventory(ctx context.Context) error {
	if !a.isClaimed() {
		return fmt.Errorf("gateway is not claimed; there is no home to publish to")
	}

	if a.broker == nil || !a.broker.IsConnected() {
		return fmt.Errorf("broker is not connected")
	}

	a.mu.Lock()
	a.inventoryHash = ""
	a.mu.Unlock()

	a.publishInventory(ctx)

	return nil
}

// ReconcileNow re-reports HA's current state for every mapped device. It runs
// off the request goroutine (one HA GET per device) so the HTTP response is not
// held open for the whole sweep.
func (a *Agent) ReconcileNow() {
	go a.reconcileReportedState()
}

// Reprovision recovers credentials (rotating the MQTT password, cool-down
// guarded) and cycles the broker connection so the rotated password takes
// effect. This is the manual escape hatch for a unit stuck on stale credentials.
func (a *Agent) Reprovision(ctx context.Context) error {
	if !a.recoverCredentials(ctx) {
		return fmt.Errorf("re-provision did not complete (cool-down active or cloud rejected); see the add-on log")
	}

	a.triggerReconnect()

	return nil
}
