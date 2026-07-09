package agent

import (
	"context"
	"encoding/json"
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

	// releaseCallTimeout / releaseInstallTimeout bound a Supervisor call and a
	// slow mutation (add-on/OS/Core update pulls an image or reboots) the agent
	// makes while converging to a release.
	releaseCallTimeout    = 90 * time.Second
	releaseInstallTimeout = 20 * time.Minute

	releaseStateFile = "applied-release.json"
)

// releaseStore persists the release the agent has fully converged to, so the
// reported release_id survives a reboot (a self-update / OS update restarts the
// unit) and a stale retained redelivery with a lower seq is ignored. It mirrors
// versionStore's on-disk, atomic-write shape.
type releaseStore struct {
	mu    sync.Mutex
	path  string
	state releaseState
}

type releaseState struct {
	AppliedReleaseID string `json:"applied_release_id"`
	AppliedSeq       int    `json:"applied_seq"`
}

func newReleaseStore(dataDir string) *releaseStore {
	s := &releaseStore{path: filepath.Join(dataDir, releaseStateFile)}

	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, &s.state)
	}

	return s
}

// current returns the release_id the agent has converged to ("" when it has
// never been given a target).
func (s *releaseStore) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.state.AppliedReleaseID
}

// alreadyApplied reports whether a release with this seq (or newer) has already
// fully converged, so a retained redelivery is a no-op.
func (s *releaseStore) alreadyApplied(seq int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return seq <= s.state.AppliedSeq
}

// markApplied records a fully-converged release (monotonic on seq) and persists.
func (s *releaseStore) markApplied(releaseID string, seq int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if seq < s.state.AppliedSeq {
		return
	}

	s.state = releaseState{AppliedReleaseID: releaseID, AppliedSeq: seq}

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

// handleRelease applies the retained target-release doc: the cloud's desired
// version set for this gateway. It is monotonic on release_seq (a stale
// redelivery is ignored) and runs the actual convergence off the broker-callback
// goroutine (Supervisor updates take minutes and can reboot the unit), guarded
// so overlapping redeliveries never double-run.
func (a *Agent) handleRelease(_ string, raw []byte) {
	if a.sup == nil {
		// Not running inside HAOS (no Supervisor token) — cloud-orchestrated
		// self-update is unavailable; nothing to converge.
		return
	}

	var rel contract.ReleasePayload
	if err := json.Unmarshal(raw, &rel); err != nil {
		a.log.Warn("release decode failed", "error", err)

		return
	}

	if rel.ReleaseID == "" {
		return
	}

	if a.release.alreadyApplied(rel.ReleaseSeq) {
		// Already converged to this (or a newer) release; just re-assert the
		// reported versions so the cloud self-heals if it missed the last report.
		go a.publishVersionsAsync()

		return
	}

	a.releaseMu.Lock()
	if a.converging {
		a.releaseMu.Unlock()

		return
	}
	a.converging = true
	a.releaseMu.Unlock()

	go func() {
		defer func() {
			a.releaseMu.Lock()
			a.converging = false
			a.releaseMu.Unlock()
		}()

		a.convergeRelease(rel)
	}()
}

// convergeRelease drives the gateway to the target release via the Supervisor
// API: dependency add-ons first, then OS, then Core, and the agent's OWN add-on
// LAST — because a self-update restarts the agent mid-flight. Every step is
// skipped when already at target, so the whole routine is idempotent and
// resume-safe: an OS update or the self-update reboots the unit, and on restart
// the retained release doc drives this again, picking up where it left off.
// Only once every component matches (including the agent, after its restart) is
// the release marked applied and reported.
func (a *Agent) convergeRelease(rel contract.ReleasePayload) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseInstallTimeout+5*time.Minute)
	defer cancel()

	a.log.Info("converging to release", "release_id", rel.ReleaseID, "release_seq", rel.ReleaseSeq)

	// 1. Dependency add-ons (everything but the agent itself).
	for _, dep := range rel.Addons {
		if dep.Match == "smart_home_agent" {
			continue
		}
		if err := a.convergeAddon(ctx, dep); err != nil {
			a.log.Error("release addon converge failed", "addon", dep.Match, "error", err)

			return
		}
	}

	// 2. OS, then 3. Core — each reboots/restarts the unit, so a mismatch stops
	// the run here; the restart re-drives convergence from the retained doc.
	if rel.HAOSVersion != "" {
		if changed, err := a.convergeOS(ctx, rel.HAOSVersion); err != nil {
			a.log.Error("release OS converge failed", "error", err)

			return
		} else if changed {
			a.log.Info("OS update triggered; convergence resumes after reboot")

			return
		}
	}

	if rel.CoreVersion != "" {
		if changed, err := a.convergeCore(ctx, rel.CoreVersion); err != nil {
			a.log.Error("release Core converge failed", "error", err)

			return
		} else if changed {
			a.log.Info("Core update triggered; convergence resumes after restart")

			return
		}
	}

	// 4. The agent itself, last. A self-update restarts this process; on restart
	// handleRelease runs again, this branch is a no-op (versions match), and the
	// release is marked applied + reported below.
	if rel.AgentVersion != "" {
		info, err := a.sup.AddonInfo(ctx, agentSelfSlug)
		if err != nil {
			a.log.Error("release agent info failed", "error", err)

			return
		}
		if info.Installed && !supervisor.VersionsEqual(info.Version, rel.AgentVersion) {
			a.log.Info("self-updating agent add-on", "from", info.Version, "to", rel.AgentVersion)
			if err := a.sup.UpdateAddon(ctx, agentSelfSlug, rel.AgentVersion); err != nil {
				a.log.Error("agent self-update failed", "error", err)
			}
			// Whether or not Supervisor returned before restarting us, the update
			// is in flight; stop here and let the restart re-drive convergence.
			return
		}
	}

	a.release.markApplied(rel.ReleaseID, rel.ReleaseSeq)
	a.log.Info("release converged", "release_id", rel.ReleaseID)
	a.publishVersions(ctx)
}

// convergeAddon installs a dependency add-on when missing (registering its
// community repository first) and updates it to the pinned version when set and
// different.
func (a *Agent) convergeAddon(ctx context.Context, dep contract.ReleaseAddon) error {
	fa := fleet.Addon{Match: dep.Match, Slug: dep.Slug, Version: dep.Version, Repository: dep.Repository, Core: dep.Core}

	slug, found, err := a.sup.ResolveAddonSlug(ctx, fa)
	if err != nil {
		return err
	}

	if !found {
		if !dep.Core && dep.Repository != "" {
			if err := a.sup.AddStoreRepository(ctx, dep.Repository); err != nil {
				return err
			}
			slug, found, err = a.sup.ResolveAddonSlug(ctx, fa)
			if err != nil {
				return err
			}
		}
		if !found {
			a.log.Warn("release addon not resolvable in store yet", "addon", dep.Match)

			return nil
		}
	}

	info, err := a.sup.AddonInfo(ctx, slug)
	if err != nil {
		return err
	}

	if !info.Installed {
		a.log.Info("installing release addon", "addon", dep.Match, "slug", slug)
		if err := a.sup.InstallAddon(ctx, slug); err != nil {
			return err
		}
		info, err = a.sup.AddonInfo(ctx, slug)
		if err != nil {
			return err
		}
	}

	if dep.Version != "" && !supervisor.VersionsEqual(info.Version, dep.Version) {
		a.log.Info("updating release addon", "addon", dep.Match, "from", info.Version, "to", dep.Version)
		if err := a.sup.UpdateAddon(ctx, slug, dep.Version); err != nil {
			return err
		}
	}

	return nil
}

// convergeOS updates HA OS to want when it differs; changed reports whether an
// update was triggered (which reboots the unit).
func (a *Agent) convergeOS(ctx context.Context, want string) (bool, error) {
	info, err := a.sup.OSInfo(ctx)
	if err != nil {
		return false, err
	}
	if supervisor.VersionsEqual(info.Version, want) {
		return false, nil
	}

	return true, a.sup.UpdateOS(ctx, want)
}

// convergeCore updates HA Core to want when it differs; changed reports whether
// an update was triggered (which restarts Core).
func (a *Agent) convergeCore(ctx context.Context, want string) (bool, error) {
	info, err := a.sup.CoreInfo(ctx)
	if err != nil {
		return false, err
	}
	if supervisor.VersionsEqual(info.Version, want) {
		return false, nil
	}

	return true, a.sup.UpdateCore(ctx, want)
}

// publishVersions reports the gateway's running software inventory + converged
// release on the retained versions topic, so the cloud's gateway versions +
// rollout convergence reflect reality independent of provisioning. Claimed-only:
// the broker ACL confines an unclaimed gateway to availability/config, and the
// cloud drops home-scoped uplink from an unclaimed gateway anyway.
func (a *Agent) publishVersions(ctx context.Context) {
	if !a.isClaimed() || a.broker == nil || !a.broker.IsConnected() {
		return
	}

	payload := contract.VersionsPayload{
		AgentVersion: a.cfg.AgentVersion,
		HAVersion:    a.ha.Version(ctx),
		OSVersion:    a.osVersion(ctx),
		ReleaseID:    a.release.current(),
		TS:           time.Now().UTC().Format(time.RFC3339),
	}

	a.publishUplink(contract.VersionsTopic(a.creds.UID), payload, true)
}

// publishVersionsAsync is the goroutine-friendly version-report used off the
// broker-callback path (a bounded context, best-effort).
func (a *Agent) publishVersionsAsync() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a.publishVersions(ctx)
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
