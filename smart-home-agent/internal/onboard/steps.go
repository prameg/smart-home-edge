package onboard

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/smart-home/edge/agent/fleet"
)

// Step names are stable identifiers (used in output and, on failure, in the
// "re-run to resume" guidance). Keep them short and greppable.
const (
	StepConnect        = "connect"
	StepOwnerAndToken  = "owner-and-token"
	StepAddonRepos     = "addon-repository"
	StepInstallAddons  = "install-addons"
	StepConfigureAgent = "configure-agent"
	StepStartAgent     = "start-agent"
	StepAwaitProvision = "await-provision"
	StepPinRelease     = "pin-release"
)

// installTimeout bounds a single add-on install/update; a first install pulls a
// container image and can take several minutes on a Pi over a slow link.
const installTimeout = 20 * time.Minute

// BuildSteps returns the ordered onboarding steps. Every step follows the
// check -> act -> verify contract from the Engine so the whole sequence is
// resumable: re-running skips already-satisfied steps and continues at the
// first unfinished one. The steps read their inputs and publish their outputs
// through the shared *State.
func BuildSteps(reporter Reporter) []Step {
	return []Step{
		connectStep(reporter),
		ownerAndTokenStep(reporter),
		addonRepositoryStep(reporter),
		installAddonsStep(reporter),
		configureAgentStep(reporter),
		startAgentStep(reporter),
		awaitProvisionStep(reporter),
		pinReleaseStep(reporter),
	}
}

// connectStep waits for Core's HTTP API to answer (a fresh HAOS flash downloads
// Core on first boot, which can take minutes).
func connectStep(reporter Reporter) Step {
	return Step{
		Name: StepConnect,
		Check: func(ctx context.Context, st *State) (bool, error) {
			// A fast single probe: if Core already answers we skip the wait.
			if err := st.Client.WaitForCore(ctx, 2*time.Second); err == nil {
				return true, nil
			}

			return false, nil
		},
		Act: func(ctx context.Context, st *State) error {
			reporter.Info(fmt.Sprintf("waiting up to %s for Home Assistant to come up", st.Timeouts.WaitCore))

			return st.Client.WaitForCore(ctx, st.Timeouts.WaitCore)
		},
		Verify: func(ctx context.Context, st *State) error {
			return st.Client.WaitForCore(ctx, 10*time.Second)
		},
	}
}

// ownerAndTokenStep obtains a usable Supervisor token: on a fresh device it
// creates the owner via the onboarding API; on a re-run (owner already exists)
// it logs in with the same credentials. Either way it exchanges the auth code
// for an access token, upgrades it to a long-lived token, stores it on the
// client, and best-effort finishes the remaining wizard steps.
func ownerAndTokenStep(reporter Reporter) Step {
	return Step{
		Name: StepOwnerAndToken,
		Check: func(_ context.Context, st *State) (bool, error) {
			// Token presence is this run's done-signal; a fresh process re-auths
			// (idempotent: create-owner vs login is chosen from device state).
			return st.Client.Token() != "", nil
		},
		Act: func(ctx context.Context, st *State) error {
			status, err := st.Client.OnboardingStatus(ctx)
			if err != nil {
				return err
			}

			var authCode string
			if status.UserDone {
				reporter.Info("owner already exists; logging in with the provided credentials")
				authCode, err = st.Client.Login(ctx, st.Owner.Username, st.Owner.Password)
			} else {
				reporter.Info(fmt.Sprintf("creating owner account %q", st.Owner.Username))
				authCode, err = st.Client.CreateOwner(ctx, st.Owner)
			}
			if err != nil {
				return err
			}

			accessToken, err := st.Client.ExchangeCode(ctx, authCode)
			if err != nil {
				return err
			}
			st.AccessToken = accessToken

			token, err := st.Client.CreateLongLivedToken(ctx, accessToken, "smart-onboard")
			if err != nil {
				// A short-lived token still gets us through the run; warn and
				// proceed rather than failing onboarding on a token-lifespan
				// nicety.
				reporter.Info(fmt.Sprintf("could not mint a long-lived token (%v); using the short-lived access token", err))
				token = accessToken
			}

			st.Client.SetToken(token)

			if err := st.Client.FinishOnboarding(ctx); err != nil {
				reporter.Info(fmt.Sprintf("finishing the onboarding wizard reported: %v (continuing)", err))
			}

			return nil
		},
		Verify: func(ctx context.Context, st *State) error {
			if st.Client.Token() == "" {
				return fmt.Errorf("no usable token after authentication")
			}

			status, err := st.Client.OnboardingStatus(ctx)
			if err != nil {
				return err
			}
			if !status.UserDone {
				return fmt.Errorf("owner account was not created")
			}

			return nil
		},
	}
}

// addonRepositoryStep registers every add-on store repository the release needs
// (our edge repo plus any community add-on repos) so their add-ons resolve.
func addonRepositoryStep(reporter Reporter) Step {
	return Step{
		Name: StepAddonRepos,
		Check: func(ctx context.Context, st *State) (bool, error) {
			have, err := st.Client.StoreRepositories(ctx)
			if err != nil {
				return false, err
			}

			for _, want := range requiredRepositories(st.Manifest) {
				if !repoPresent(have, want) {
					return false, nil
				}
			}

			return true, nil
		},
		Act: func(ctx context.Context, st *State) error {
			have, err := st.Client.StoreRepositories(ctx)
			if err != nil {
				return err
			}

			for _, want := range requiredRepositories(st.Manifest) {
				if repoPresent(have, want) {
					continue
				}

				reporter.Info(fmt.Sprintf("adding add-on repository %s", want))
				if err := st.Client.AddStoreRepository(ctx, want); err != nil {
					return err
				}
			}

			return nil
		},
		Verify: func(ctx context.Context, st *State) error {
			// The store can take a moment to index a freshly added repo; give it
			// a few tries before failing.
			for attempt := 0; attempt < 10; attempt++ {
				have, err := st.Client.StoreRepositories(ctx)
				if err != nil {
					return err
				}

				missing := false
				for _, want := range requiredRepositories(st.Manifest) {
					if !repoPresent(have, want) {
						missing = true

						break
					}
				}
				if !missing {
					return nil
				}

				if err := sleep(ctx, 2*time.Second); err != nil {
					return err
				}
			}

			return fmt.Errorf("add-on repository did not appear in the store")
		},
	}
}

// installAddonsStep installs every add-on in the release (resolving each full
// slug from the store), pinning to the manifest version when one is set. It also
// caches the resolved agent slug for the later start/await steps.
func installAddonsStep(reporter Reporter) Step {
	return Step{
		Name: StepInstallAddons,
		Check: func(ctx context.Context, st *State) (bool, error) {
			// Resolving the agent slug here (not only in Act) means a resumed run
			// that skips this step still has the slug for later steps.
			if err := resolveAgentSlug(ctx, st); err != nil {
				return false, err
			}

			for _, a := range st.Manifest.Addons {
				slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
				if err != nil {
					return false, err
				}
				if !found {
					return false, nil
				}

				info, err := st.Client.AddonInfo(ctx, slug)
				if err != nil {
					return false, err
				}
				if !info.Installed {
					return false, nil
				}
				if a.Pinned() && info.Version != a.Version {
					return false, nil
				}
			}

			return true, nil
		},
		Act: func(ctx context.Context, st *State) error {
			for _, a := range st.Manifest.Addons {
				slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("add-on %q is not in the store yet (try re-running to resume)", a.Name)
				}

				info, err := st.Client.AddonInfo(ctx, slug)
				if err != nil {
					return err
				}

				if !info.Installed {
					reporter.Info(fmt.Sprintf("installing %s (%s)", a.Name, slug))
					if err := withTimeout(ctx, installTimeout, func(ctx context.Context) error {
						return st.Client.InstallAddon(ctx, slug)
					}); err != nil {
						return fmt.Errorf("install %s: %w", a.Name, err)
					}

					info, err = st.Client.AddonInfo(ctx, slug)
					if err != nil {
						return err
					}
				}

				if a.Pinned() && info.Version != a.Version {
					reporter.Info(fmt.Sprintf("pinning %s to %s (currently %s)", a.Name, a.Version, info.Version))
					if err := withTimeout(ctx, installTimeout, func(ctx context.Context) error {
						return st.Client.UpdateAddon(ctx, slug, a.Version)
					}); err != nil {
						return fmt.Errorf("pin %s to %s: %w", a.Name, a.Version, err)
					}
				}
			}

			return nil
		},
		Verify: func(ctx context.Context, st *State) error {
			for _, a := range st.Manifest.Addons {
				slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
				if err != nil {
					return err
				}
				if !found {
					return fmt.Errorf("add-on %q did not resolve after install", a.Name)
				}

				info, err := st.Client.AddonInfo(ctx, slug)
				if err != nil {
					return err
				}
				if !info.Installed {
					return fmt.Errorf("add-on %q is not installed", a.Name)
				}
			}

			return nil
		},
	}
}

// configureAgentStep writes the agent add-on's options (cloud URL, factory key,
// broker settings). It is idempotent: a re-run whose desired options already
// match is skipped.
func configureAgentStep(reporter Reporter) Step {
	return Step{
		Name: StepConfigureAgent,
		Check: func(ctx context.Context, st *State) (bool, error) {
			if err := resolveAgentSlug(ctx, st); err != nil {
				return false, err
			}

			info, err := st.Client.AddonInfo(ctx, st.AgentSlug)
			if err != nil {
				return false, err
			}

			return optionsSatisfied(info.Options, st.AgentOptions), nil
		},
		Act: func(ctx context.Context, st *State) error {
			if err := resolveAgentSlug(ctx, st); err != nil {
				return err
			}

			if err := st.Client.SetAddonOptions(ctx, st.AgentSlug, st.AgentOptions); err != nil {
				return err
			}

			// Home Assistant loads an add-on's options only at container start.
			// On a resumed run that corrects the config (e.g. a wrong
			// cloud_base_url / mqtt_host) the add-on is already running, so the
			// new options would never take effect: start-agent sees it
			// "started" and skips, and the agent keeps provisioning against the
			// stale config. Restart it here so the corrected options load.
			info, err := st.Client.AddonInfo(ctx, st.AgentSlug)
			if err != nil {
				return err
			}
			if info.State == "started" {
				reporter.Info("restarting the agent to apply the updated configuration")

				return st.Client.RestartAddon(ctx, st.AgentSlug)
			}

			return nil
		},
		Verify: func(ctx context.Context, st *State) error {
			info, err := st.Client.AddonInfo(ctx, st.AgentSlug)
			if err != nil {
				return err
			}
			if !optionsSatisfied(info.Options, st.AgentOptions) {
				return fmt.Errorf("agent options did not persist")
			}

			return nil
		},
	}
}

// startAgentStep starts the agent add-on and confirms it reaches the started
// state.
func startAgentStep(reporter Reporter) Step {
	return Step{
		Name: StepStartAgent,
		Check: func(ctx context.Context, st *State) (bool, error) {
			if err := resolveAgentSlug(ctx, st); err != nil {
				return false, err
			}

			info, err := st.Client.AddonInfo(ctx, st.AgentSlug)
			if err != nil {
				return false, err
			}

			return info.State == "started", nil
		},
		Act: func(ctx context.Context, st *State) error {
			reporter.Info("starting the Smart Home Agent add-on")

			return st.Client.StartAddon(ctx, st.AgentSlug)
		},
		Verify: func(ctx context.Context, st *State) error {
			for attempt := 0; attempt < 15; attempt++ {
				info, err := st.Client.AddonInfo(ctx, st.AgentSlug)
				if err != nil {
					return err
				}
				if info.State == "started" {
					return nil
				}

				if err := sleep(ctx, 2*time.Second); err != nil {
					return err
				}
			}

			return fmt.Errorf("agent add-on did not reach the started state")
		},
	}
}

// awaitProvisionStep waits for the agent to provision against the cloud and
// captures its uid + short claim code (published on the shared State for the
// done screen).
func awaitProvisionStep(reporter Reporter) Step {
	return Step{
		Name: StepAwaitProvision,
		Check: func(ctx context.Context, st *State) (bool, error) {
			if err := resolveAgentSlug(ctx, st); err != nil {
				return false, err
			}

			info, err := st.Client.ClaimInfo(ctx, st.AgentSlug)
			if err != nil {
				return false, err
			}
			if info.UID == "" {
				return false, nil
			}

			st.Claim = info

			return true, nil
		},
		Act: func(ctx context.Context, st *State) error {
			reporter.Info(fmt.Sprintf("waiting up to %s for the agent to provision", st.Timeouts.WaitProvision))

			deadline := time.Now().Add(st.Timeouts.WaitProvision)
			for {
				info, err := st.Client.ClaimInfo(ctx, st.AgentSlug)
				if err == nil && info.UID != "" {
					st.Claim = info

					return nil
				}

				if time.Now().After(deadline) {
					if err != nil {
						return fmt.Errorf("agent did not provision within %s: %w", st.Timeouts.WaitProvision, err)
					}

					return fmt.Errorf("agent did not provision within %s (still no uid in the add-on log)", st.Timeouts.WaitProvision)
				}

				if err := sleep(ctx, 3*time.Second); err != nil {
					return err
				}
			}
		},
		Verify: func(_ context.Context, st *State) error {
			if st.Claim.UID == "" {
				return fmt.Errorf("no provisioned uid captured")
			}

			return nil
		},
	}
}

// pinReleaseStep locks the unit to the release: it disables auto-update on the
// pinned add-ons and, when the manifest names explicit OS/Core versions that
// differ from what is running, converges them. On a template release (no pinned
// versions) it is a no-op that the engine reports as skipped.
func pinReleaseStep(reporter Reporter) Step {
	return Step{
		Name: StepPinRelease,
		Check: func(ctx context.Context, st *State) (bool, error) {
			if !pinningNeeded(st.Manifest) {
				return true, nil
			}

			return pinSatisfied(ctx, st)
		},
		Act: func(ctx context.Context, st *State) error {
			for _, a := range st.Manifest.Addons {
				if !a.Pinned() {
					continue
				}

				slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
				if err != nil {
					return err
				}
				if !found {
					continue
				}

				if err := st.Client.SetAddonAutoUpdate(ctx, slug, false); err != nil {
					return fmt.Errorf("disable auto-update on %s: %w", a.Name, err)
				}
			}

			if v := st.Manifest.HAOS.Version; v != "" {
				if err := convergeVersion(ctx, reporter, "Home Assistant OS", v,
					st.Client.OSInfo, st.Client.UpdateOS); err != nil {
					return err
				}
			}

			if v := st.Manifest.Core.Version; v != "" {
				if err := convergeVersion(ctx, reporter, "Home Assistant Core", v,
					st.Client.CoreInfo, st.Client.UpdateCore); err != nil {
					return err
				}
			}

			return nil
		},
		Verify: func(ctx context.Context, st *State) error {
			// Auto-update is the invariant we can confirm without racing an
			// OS/Core update+reboot, so verify only that.
			for _, a := range st.Manifest.Addons {
				if !a.Pinned() {
					continue
				}

				slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
				if err != nil {
					return err
				}
				if !found {
					continue
				}

				info, err := st.Client.AddonInfo(ctx, slug)
				if err != nil {
					return err
				}
				if info.AutoUpdate {
					return fmt.Errorf("auto-update is still enabled on %s", a.Name)
				}
			}

			return nil
		},
	}
}

// --- step helpers ---------------------------------------------------------

// resolveAgentSlug caches the agent add-on's full slug on the State the first
// time it is needed, so start/await/configure work on a resumed run that skipped
// the install step.
func resolveAgentSlug(ctx context.Context, st *State) error {
	if st.AgentSlug != "" {
		return nil
	}

	agent := st.Manifest.Agent()
	if agent == nil {
		return fmt.Errorf("manifest has no agent add-on")
	}

	slug, found, err := st.Client.ResolveAddonSlug(ctx, *agent)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("agent add-on is not in the store yet (add the repository first, then re-run)")
	}

	st.AgentSlug = slug

	return nil
}

// requiredRepositories is the edge repo plus each community add-on's own repo.
func requiredRepositories(m *fleet.Manifest) []string {
	repos := []string{m.AddonRepository}
	for _, a := range m.Addons {
		if !a.Core && a.Repository != "" {
			repos = append(repos, a.Repository)
		}
	}

	return dedupe(repos)
}

// repoPresent compares repository URLs ignoring a trailing slash or ".git"
// suffix so a cosmetically different form still counts as present.
func repoPresent(have []string, want string) bool {
	target := normalizeRepo(want)
	for _, h := range have {
		if normalizeRepo(h) == target {
			return true
		}
	}

	return false
}

func normalizeRepo(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")

	return strings.ToLower(u)
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	return out
}

// optionsSatisfied reports whether every desired option already equals the
// current value (JSON-compared so numeric/string encodings line up).
func optionsSatisfied(current, desired map[string]any) bool {
	if len(desired) == 0 {
		return true
	}
	if current == nil {
		return false
	}

	for k, want := range desired {
		got, ok := current[k]
		if !ok {
			return false
		}
		if !jsonEqual(got, want) {
			return false
		}
	}

	return true
}

func jsonEqual(a, b any) bool {
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}

	return string(ab) == string(bb)
}

// pinningNeeded reports whether the release names anything to pin: explicit
// OS/Core versions or at least one pinned add-on.
func pinningNeeded(m *fleet.Manifest) bool {
	if m.HAOS.Version != "" || m.Core.Version != "" {
		return true
	}
	for _, a := range m.Addons {
		if a.Pinned() {
			return true
		}
	}

	return false
}

// pinSatisfied reports whether auto-update is already off on every pinned add-on
// (the safely-checkable part of a pin).
func pinSatisfied(ctx context.Context, st *State) (bool, error) {
	for _, a := range st.Manifest.Addons {
		if !a.Pinned() {
			continue
		}

		slug, found, err := st.Client.ResolveAddonSlug(ctx, a)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}

		info, err := st.Client.AddonInfo(ctx, slug)
		if err != nil {
			return false, err
		}
		if info.AutoUpdate {
			return false, nil
		}
	}

	return true, nil
}

// convergeVersion updates OS/Core to want only when the running version differs,
// warning that this is disruptive (it can trigger a reboot/restart).
func convergeVersion(
	ctx context.Context,
	reporter Reporter,
	label, want string,
	info func(context.Context) (VersionInfo, error),
	update func(context.Context, string) error,
) error {
	current, err := info(ctx)
	if err != nil {
		return err
	}
	if current.Version == want {
		return nil
	}

	reporter.Info(fmt.Sprintf("converging %s from %s to pinned %s (this can restart the unit)", label, current.Version, want))

	return update(ctx, want)
}

// withTimeout runs fn under a child context bounded by d.
func withTimeout(ctx context.Context, d time.Duration, fn func(context.Context) error) error {
	child, cancel := context.WithTimeout(ctx, d)
	defer cancel()

	return fn(child)
}

// sleep waits for d or returns ctx's error if it is cancelled first.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
