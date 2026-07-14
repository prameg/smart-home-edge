package agent

import (
	"context"
	"encoding/json"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/smart-home/edge/agent/fleet"
	"github.com/smart-home/edge/agent/internal/contract"
	"github.com/smart-home/edge/agent/internal/supervisor"
)

const (
	// agentSelfSlug is the Supervisor alias for the calling add-on, so the agent
	// updates ITSELF without having to resolve its repo-hash-prefixed full slug.
	agentSelfSlug = "self"

	// agentMatch is the inner slug of the agent add-on in the bootstrap set; the
	// agent updates itself LAST (out of band from the dependency add-ons) because
	// a self-update restarts this process mid-run.
	agentMatch = "smart_home_agent"

	// updateCallTimeout / updateInstallTimeout bound a Supervisor call and a slow
	// mutation (add-on/OS/Core update pulls an image or reboots) the agent makes
	// while bringing the unit to the latest.
	updateCallTimeout    = 90 * time.Second
	updateInstallTimeout = 20 * time.Minute

	// updateAllTimeout caps a single updateAll pass. A phase that reboots the
	// unit stops the pass early (it resumes on boot), so this only has to cover
	// the non-rebooting add-on phase plus one OS/Core/self trigger.
	updateAllTimeout = updateInstallTimeout + 5*time.Minute

	// updateCheckInterval is the cadence of the agent's own self-check: it brings
	// the unit to the latest roughly daily even with no fleet command, so an
	// offline gateway that missed a command still converges. Jittered per-device
	// so a large fleet does not check in lock-step.
	updateCheckInterval = 24 * time.Hour

	updateStateFile = "update-in-progress.json"
)

// updateStore persists that an update is mid-flight so a reboot (an OS/Core or
// self update restarts the unit) resumes it on boot instead of leaving the unit
// half-updated. It mirrors versionStore's on-disk, atomic-write shape.
type updateStore struct {
	mu    sync.Mutex
	path  string
	state updateMarker
}

// updateMarker is the on-disk resume record. InProgress is the presence flag;
// UpdateID carries the triggering command's id ("" for a self-check) so the
// terminal status report on resume ties back to the same command.
type updateMarker struct {
	InProgress bool   `json:"in_progress"`
	UpdateID   string `json:"update_id"`
}

func newUpdateStore(dataDir string) *updateStore {
	s := &updateStore{path: filepath.Join(dataDir, updateStateFile)}

	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.state)
	}

	return s
}

// inProgress reports whether an update was left mid-flight (and its update_id),
// so the agent resumes it on boot.
func (s *updateStore) inProgress() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state.UpdateID, s.state.InProgress
}

// begin records that an update has started and persists the marker.
func (s *updateStore) begin(updateID string) {
	s.write(updateMarker{InProgress: true, UpdateID: updateID})
}

// clear records that no update is in flight and persists the marker.
func (s *updateStore) clear() {
	s.write(updateMarker{})
}

func (s *updateStore) write(m updateMarker) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.state = m

	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}

	_ = os.Rename(tmp, s.path)
}

// handleUpdate is the downlink homes/{uid}/update command: bring the unit to the
// latest. It runs the (slow, reboot-prone) work off the broker-callback
// goroutine, guarded so overlapping commands never double-run.
func (a *Agent) handleUpdate(_ string, raw []byte) {
	if a.sup == nil {
		// Not running inside HAOS (no Supervisor token) — Supervisor-driven
		// updates are unavailable; nothing to do.
		return
	}

	var cmd contract.UpdatePayload
	if err := json.Unmarshal(raw, &cmd); err != nil {
		a.log.Warn("update decode failed", "error", err)

		return
	}

	a.startUpdate(cmd.UpdateID)
}

// startUpdate launches updateAll on a background goroutine, guarded so only one
// runs at a time (a command, the daily self-check, and a boot-resume all funnel
// through here). A second trigger while one is running is dropped — the running
// pass already converges to the latest.
func (a *Agent) startUpdate(updateID string) {
	if a.sup == nil {
		return
	}

	a.updateMu.Lock()
	if a.updating {
		a.updateMu.Unlock()

		return
	}
	a.updating = true
	a.updateMu.Unlock()

	go func() {
		defer func() {
			a.updateMu.Lock()
			a.updating = false
			a.updateMu.Unlock()
		}()

		a.updateAll(updateID)
	}()
}

// resumeOrCheckUpdates runs on each claimed (re)connect. If an update was left
// mid-flight (a reboot interrupted it), it resumes that update; otherwise it
// runs a throttled self-check so a just-claimed / just-booted unit converges to
// the latest without waiting a full day for the ticker. Reconnects within the
// check interval are a no-op so a flapping connection never re-checks in a loop.
func (a *Agent) resumeOrCheckUpdates() {
	if a.sup == nil {
		return
	}

	if id, ok := a.updateState.inProgress(); ok {
		a.markAutoChecked()
		a.startUpdate(id)

		return
	}

	if a.autoCheckDue() {
		a.startUpdate("")
	}
}

// autoCheckDue reports whether the self-check interval has elapsed since the
// last auto-check, stamping "now" when it has so callers race-free at most once
// per interval.
func (a *Agent) autoCheckDue() bool {
	a.autoMu.Lock()
	defer a.autoMu.Unlock()

	if time.Since(a.lastAutoCheck) < updateCheckInterval {
		return false
	}

	a.lastAutoCheck = time.Now()

	return true
}

func (a *Agent) markAutoChecked() {
	a.autoMu.Lock()
	a.lastAutoCheck = time.Now()
	a.autoMu.Unlock()
}

// updateAll brings the gateway to the latest across managed add-ons, then HAOS,
// then Core, then the agent itself (in that order — the agent last because a
// self-update restarts this process). It is idempotent and resume-safe: an
// OS/Core/self update reboots/restarts the unit, so the pass stops after
// triggering it and an on-disk marker re-drives it on boot, where every
// already-current phase is a no-op until the unit is fully converged.
//
// updateID is the triggering command's id, "" for the agent's own self-check.
// Progress is reported on update/status: `started` (command-triggered only, so a
// silent daily check doesn't flap the fleet badge), then a terminal `ok` when
// everything is current or `failed` with the error on a hard failure.
func (a *Agent) updateAll(updateID string) {
	if a.sup == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), updateAllTimeout)
	defer cancel()

	if updateID != "" {
		a.reportUpdateStatus(contract.UpdateStarted, updateID, "")
	}

	// Persist the marker BEFORE any reboot-prone phase so an interrupted pass
	// resumes on boot.
	a.updateState.begin(updateID)

	if err := a.updateAddons(ctx); err != nil {
		a.failUpdate(updateID, err)

		return
	}

	// OS, then Core, then self — each reboots/restarts the unit, so a triggered
	// update stops the pass here; the restart re-drives updateAll from the
	// marker, skipping the now-current phases.
	if triggered, err := a.updateOSLatest(ctx); err != nil {
		a.failUpdate(updateID, err)

		return
	} else if triggered {
		a.log.Info("OS update triggered; resumes after reboot")

		return
	}

	if triggered, err := a.updateCoreLatest(ctx); err != nil {
		a.failUpdate(updateID, err)

		return
	} else if triggered {
		a.log.Info("Core update triggered; resumes after restart")

		return
	}

	if triggered, err := a.updateSelf(ctx); err != nil {
		a.failUpdate(updateID, err)

		return
	} else if triggered {
		a.log.Info("agent self-update triggered; resumes after restart")

		return
	}

	// Everything is current — the pass is done.
	a.updateState.clear()
	a.log.Info("update converged; everything on latest")
	a.reportUpdateStatus(contract.UpdateOK, updateID, "")
	a.publishVersions(ctx)
}

// failUpdate clears the resume marker (a hard failure is retried by the next
// command or self-check, not by a boot-resume loop) and reports it.
func (a *Agent) failUpdate(updateID string, err error) {
	a.log.Error("update failed", "error", err)
	a.updateState.clear()
	a.reportUpdateStatus(contract.UpdateFailed, updateID, err.Error())
}

// updateAddons brings every managed dependency add-on to the latest and turns
// HA's per-add-on auto_update ON, so Supervisor keeps them fresh between
// self-checks. The agent's own add-on is handled by updateSelf (last).
func (a *Agent) updateAddons(ctx context.Context) error {
	manifest, err := fleet.Default()
	if err != nil {
		return err
	}

	for _, dep := range manifest.Addons {
		if dep.Match == agentMatch {
			continue
		}
		if err := a.updateAddon(ctx, dep); err != nil {
			return err
		}
	}

	return nil
}

// updateAddon installs a dependency add-on when missing (registering its
// community repository first), enables auto_update, and moves it to the latest
// when an update is available.
func (a *Agent) updateAddon(ctx context.Context, dep fleet.Addon) error {
	slug, found, err := a.sup.ResolveAddonSlug(ctx, dep)
	if err != nil {
		return err
	}

	if !found {
		if !dep.Core && dep.Repository != "" {
			if err := a.sup.AddStoreRepository(ctx, dep.Repository); err != nil {
				return err
			}
			slug, found, err = a.sup.ResolveAddonSlug(ctx, dep)
			if err != nil {
				return err
			}
		}
		if !found {
			a.log.Warn("addon not resolvable in store yet", "addon", dep.Match)

			return nil
		}
	}

	info, err := a.sup.AddonInfo(ctx, slug)
	if err != nil {
		return err
	}

	if !info.Installed {
		a.log.Info("installing addon", "addon", dep.Match, "slug", slug)
		if err := a.sup.InstallAddon(ctx, slug); err != nil {
			return err
		}
		info, err = a.sup.AddonInfo(ctx, slug)
		if err != nil {
			return err
		}
	}

	a.enableAutoUpdate(ctx, slug, info.AutoUpdate)

	if info.UpdateAvailable {
		a.log.Info("updating addon to latest", "addon", dep.Match, "from", info.Version)
		if err := a.sup.UpdateAddonLatest(ctx, slug); err != nil {
			return err
		}
	}

	return nil
}

// enableAutoUpdate turns HA's per-add-on auto-update ON so Supervisor keeps a
// managed add-on fresh between the agent's self-checks. No-op when already on,
// and best-effort: a failure here must not fail the whole pass (the self-check
// re-applies it next time).
func (a *Agent) enableAutoUpdate(ctx context.Context, slug string, currentlyOn bool) {
	if currentlyOn {
		return
	}

	if err := a.sup.SetAddonAutoUpdate(ctx, slug, true); err != nil {
		a.log.Warn("enable addon auto-update failed", "slug", slug, "error", err)
	}
}

// updateOSLatest updates HA OS to the latest when one is available; triggered
// reports whether an update was started (which reboots the unit).
func (a *Agent) updateOSLatest(ctx context.Context) (bool, error) {
	info, err := a.sup.OSInfo(ctx)
	if err != nil {
		return false, err
	}
	if !updateAvailable(info) {
		return false, nil
	}

	a.log.Info("updating HA OS to latest", "from", info.Version, "to", info.Latest)

	return true, a.sup.UpdateOSLatest(ctx)
}

// updateCoreLatest updates HA Core to the latest when one is available;
// triggered reports whether an update was started (which restarts Core).
func (a *Agent) updateCoreLatest(ctx context.Context) (bool, error) {
	info, err := a.sup.CoreInfo(ctx)
	if err != nil {
		return false, err
	}
	if !updateAvailable(info) {
		return false, nil
	}

	a.log.Info("updating HA Core to latest", "from", info.Version, "to", info.Latest)

	return true, a.sup.UpdateCoreLatest(ctx)
}

// updateSelf updates the agent's OWN add-on to the latest when available. It
// enables auto_update too, so even a unit that never gets a fleet command stays
// current. triggered reports whether the self-update was started (which restarts
// this process); on the restart updateAll re-runs and this is a no-op.
func (a *Agent) updateSelf(ctx context.Context) (bool, error) {
	info, err := a.sup.AddonInfo(ctx, agentSelfSlug)
	if err != nil {
		return false, err
	}
	if !info.Installed {
		return false, nil
	}

	a.enableAutoUpdate(ctx, agentSelfSlug, info.AutoUpdate)

	if !info.UpdateAvailable {
		return false, nil
	}

	a.log.Info("self-updating agent add-on", "from", info.Version)

	return true, a.sup.UpdateAddonLatest(ctx, agentSelfSlug)
}

// updateAvailable reports whether Supervisor's reported latest differs from the
// running version for OS/Core (it does not expose an update_available flag on
// those the way it does for add-ons).
func updateAvailable(info supervisor.VersionInfo) bool {
	return info.Latest != "" && !supervisor.VersionsEqual(info.Version, info.Latest)
}

// reportUpdateStatus publishes an update/status progress report.
func (a *Agent) reportUpdateStatus(status contract.UpdateStatus, updateID, errMsg string) {
	payload := contract.UpdateStatusPayload{
		UpdateID: updateID,
		Status:   status,
		Error:    errMsg,
		TS:       time.Now().UTC().Format(time.RFC3339),
	}

	a.publishUplink(contract.UpdateStatusTopic(a.creds.UID), payload, false)
}

// publishVersions reports the gateway's running software inventory on the
// retained versions topic, so the cloud's gateway versions reflect reality
// independent of provisioning. Claimed-only: the broker ACL confines an
// unclaimed gateway to availability/config, and the cloud drops home-scoped
// uplink from an unclaimed gateway anyway.
func (a *Agent) publishVersions(ctx context.Context) {
	if !a.isClaimed() || a.broker == nil || !a.broker.IsConnected() {
		return
	}

	payload := contract.VersionsPayload{
		AgentVersion: a.cfg.AgentVersion,
		HAVersion:    a.ha.Version(ctx),
		OSVersion:    a.osVersion(ctx),
		TS:           time.Now().UTC().Format(time.RFC3339),
	}

	a.publishUplink(contract.VersionsTopic(a.creds.UID), payload, true)
}

// osVersion reads the running HA OS version via Supervisor (best-effort: empty
// when the agent is not running inside HAOS or the call fails).
func (a *Agent) osVersion(ctx context.Context) string {
	if a.sup == nil {
		return ""
	}

	info, err := a.sup.OSInfo(ctx)
	if err != nil {
		return ""
	}

	return info.Version
}

// jitteredInterval spreads a fleet's periodic self-checks so a large fleet does
// not hit Supervisor / the update servers in lock-step: base minus up to 10%.
func jitteredInterval(base time.Duration) time.Duration {
	return base - time.Duration(rand.Int63n(int64(base/10)))
}
