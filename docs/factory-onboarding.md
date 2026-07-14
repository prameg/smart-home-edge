# Device onboarding — decision + runbook

How a unit comes off the line (or out of a technician's hands) already able to
provision itself and surface a claim code, with the least manual touch and the
most maintainability. This was the least-proven piece of Phase 2; it is now
**decided** and implemented as the `smart-onboard` CLI.

> **Just need to onboard a unit against our live cloud?** Use the concrete,
> copy-paste team runbook: [`production-onboarding.md`](production-onboarding.md)
> (`smart.prameg.net` + `mqtt.prameg.net`). This doc is the decision + generic
> reference behind it.

> **CLI vs golden image.** The `smart-onboard` CLI is the **bring-up + fallback**
> path — cutting the reference unit for the golden image, non-imaged hardware,
> and mid-flash recovery. The **primary** field path is the pre-baked golden
> image (flash → boot → claim, no CLI): see [`golden-image.md`](golden-image.md).
> Both reach the same provisioned end state; the vocabulary tying them together
> is in [`gateway-lifecycle.md`](gateway-lifecycle.md).

## What "onboarded" means

A gateway is field-ready when, from first boot, it can:

1. Present a **factory key** to `POST /api/v1/provisioning/gateways` (the agent
   add-on's `factory_key` option), and
2. Reach the cloud + broker (network config), and
3. Surface its short **claim code** (in the agent log + an HA persistent
   notification) for the installer/end-user to bind it to a home.

Everything after that is the agent (this repo) + the managed add-ons, all kept
at latest.

## Decision

> **GO: stock Home Assistant OS + a resumable onboarding CLI
> (`smart-onboard`).** Flash stock HAOS onto the gateway, then drive the rest —
> owner creation, add-on repository, add-on installs (at latest), agent
> configuration, start, and reading back the claim code — over Home Assistant's
> **stable public APIs** (the onboarding + auth REST API and the Supervisor API,
> reached through Core's authenticated `supervisor/api` WebSocket command — the
> same one the HA frontend uses; add-on logs are read over the allowlisted
> `/api/hassio` proxy). The CLI runs from a companion machine (a technician
> laptop / factory station) pointed at the device, or on the device itself.

The rationale is robustness through **resumability over stable contracts**:

- **Stable contracts, not `.storage` hacks.** The onboarding/auth APIs and the
  `supervisor/api` WebSocket command are the same ones the official HA frontend
  uses. We re-validate them once per HA Core baseline, but they do not shift the
  way the internal `.storage` layout does. (This surface *does* move
  occasionally: HA locked the `/api/hassio` REST proxy down to an allowlist —
  backups/logs only — so general Supervisor calls moved to `supervisor/api`. That
  is exactly the per-Core-version re-validation this section calls for.)
- **Resumable state machine.** Each step is `check → act → verify`, and the
  source of truth for "is this done?" is the **device itself**, queried fresh at
  the start of every step — not a local progress file. A run that dies halfway
  (network blip, killed process, a slow add-on install) is recovered by simply
  re-running the exact same command: completed steps short-circuit and the run
  resumes at the first unfinished one. This is the "interactive failover /
  recoverable steps" property we wanted.
- **Stays on the supported update path.** OS/Core/add-ons update the normal HAOS
  way; the agent enables HA-native `auto_update` and drives the Supervisor update
  endpoints for HAOS/Core on a daily self-check or an on-demand fleet **Update**
  (see [`fleet-update.md`](fleet-update.md)).

### Options that were rejected

- **`.storage` pre-seeding of the onboarding wizard / add-ons** — brittle: it
  depends on HAOS internal file layout that is not a public contract and shifts
  between Core versions. The CLI achieves the same "no human clicks the wizard"
  outcome over the public onboarding API instead.
- **HA Supervised on generic Debian/Linux** — **rejected because it is
  deprecated upstream.** The
  [supervised-installer](https://github.com/home-assistant/supervised-installer)
  is marked unsupported as of the HAOS 2025.12.0 release, and
  [ADR-0014](https://github.com/home-assistant/architecture/blob/master/adr/0014-home-assistant-supervised.md)
  was reverted ("Home Assistant Supervised is no longer an officially supported
  installation method"). It also required a byte-exact dedicated Debian host.
  Building a fleet on it would mean owning an abandoned install method. **The
  gateway is therefore always HAOS.**
- **HA Container (plain Docker)** — excluded by product decision (no Supervisor
  means no add-on management / `homeassistant_api: true`, which the agent's fleet
  updates lean on).
- **Custom buildroot / HAOS image** — deferred until fleet scale justifies a
  build pipeline; it diverges from stock HAOS and rebuilds on every OS bump.

## AI-capable hardware (Jetson, etc.) — companion node, not a second HA host

Because Supervised is dead and HAOS has no Jetson image, AI-capable hardware
does **not** run Home Assistant. Instead it joins the system as a **companion
node next to the Pi**:

- The Pi keeps HAOS, Supervisor, the add-ons, the agent, and offline autonomy.
- The Jetson runs stock JetPack/L4T with our AI services in Docker and publishes
  results/controls to the gateway's **local Mosquitto** via **MQTT Discovery**
  (the Frigate pattern). HA sees ordinary entities; the agent syncs them to the
  cloud unchanged. Offline autonomy is preserved (camera → Jetson → local broker
  → HA never touches the WAN).

This keeps every box on a vendor-supported OS. It is **Phase 4 roadmap work**
(Jetson runtime package, its MQTT entity contract, extending the cloud device
allow-list beyond actuator domains to `binary_sensor`/`sensor`/`event`, JetPack +
model management for the Jetson node, and a `curl https://smart.test/ha/sh | sh`
plain-Linux node installer) — recorded here as the direction, **not built yet**.

## Operator runbook

1. **Flash stock HAOS** onto the gateway (Pi 4/5, x86, ODROID…) and boot it on
   the network. First boot downloads Core, which can take a few minutes.
2. **Run `smart-onboard`** from a companion machine on the same network (build
   it from `smart-home-agent/cmd/smart-onboard`). With no flags it runs a
   **guided flow**: it discovers the gateway on the LAN over mDNS and then
   prompts for the remaining settings with sensible defaults (press enter to
   accept each), ending with a confirmation summary before it starts:

   ```bash
   smart-onboard
   ```

   ```
   Searching the network for a Home Assistant gateway…
     found Home at http://192.168.1.42:8123
   Cloud base URL (e.g. https://app.example.com): https://app.example.com
   Factory key: fk-abc123
   Broker host [app.example.com]:
   Broker port [8883]:
   Use TLS to the broker (y/n) [y]:
   HA owner username (used to log in on a re-run) [admin]:
   HA owner password (set on a fresh device, or the existing one on a re-run): hunter2
   Country code [SA]:

   Ready to onboard with:
     gateway:  http://192.168.1.42:8123
     …
   Proceed? (y/n) [y]:
   ```

3. **Or specify everything up front** (factory stations / CI). Pass `--yes` to
   disable prompting; the host is still auto-discovered when `--host` is omitted:

   ```bash
   smart-onboard \
     --host http://homeassistant.local:8123 \
     --cloud-base-url https://app.example.com \
     --factory-key <factory-key> \
     --mqtt-host broker.example.com --mqtt-port 8883 --mqtt-tls \
     --owner-password <owner-password> --country SA --yes
   ```

   Secrets can come from flags, from env (`SMART_ONBOARD_FACTORY_KEY`,
   `SMART_ONBOARD_OWNER_PASSWORD`), or interactively. On the **CLI** path the
   factory key is entered per run, never baked into a script. (The **golden
   image** path is different by design: it bakes the factory key into the image
   so field onboarding is flash → boot → claim — see
   [`golden-image.md`](golden-image.md).)
4. **Read the claim code** off the final screen and enter it in the Smart Home
   app to bind the gateway to a home.

If any step fails, the CLI prints which step failed and why; fix the cause and
**re-run the same command** — it resumes from where it stopped.

### CLI UX contract (how it is built)

The CLI is designed so the happy path needs no arguments and the automated path
needs no prompts — one binary, two entry points:

- **Zero-config discovery.** When `--host` is omitted it browses mDNS for
  `_home-assistant._tcp` (HAOS advertises this once Core is up, carrying the
  instance name + a reachable URL). One hit is used automatically; several show
  a picker (interactive) or the first is used (with `--yes`); none falls back to
  the `homeassistant.local` default / a prompt. Discovery is best-effort — a
  quiet or mDNS-unfriendly network simply falls through to the prompt/default.
- **Developer mode (`--dev`).** A local HA — a VirtualBox/UTM VM that NATs
  `guest:8123` onto the host's loopback — is invisible to mDNS (its announcement
  carries the guest's internal IP, not `127.0.0.1`). `--dev` additionally probes
  the well-known local URLs (`http://127.0.0.1:8123`, `http://homeassistant.local:8123`),
  defaults the host to `127.0.0.1:8123`, defaults the broker to plaintext
  (`--mqtt-tls=false`), and defaults `cloud_base_url` / `mqtt_host` to the
  VirtualBox NAT host `10.0.2.2` (matching `scripts/sync-addon-to-vm.sh`) — the
  usual local setup. It also **skips** the store agent add-on in `install-addons`:
  you install the agent from your **local checkout** out-of-band with
  `scripts/sync-addon-to-vm.sh` (see below). Production runs omit it.
- **Guided prompts with defaults.** With a terminal attached and no `--yes`, any
  value not passed by flag (and not already in the environment, for secrets) is
  prompted with its default in brackets — enter accepts it. The broker host
  defaults to the cloud host, and the **owner username** is prompted (not just
  defaulted) because it is the login identity on a re-run against an already-owned
  device. A masked summary is confirmed before the run.
- **Non-interactive by contract.** Piped stdin or `--yes` disables every prompt;
  missing required inputs (`--cloud-base-url`, `--mqtt-host`, factory key, owner
  password) fail fast with a message that names the flag and its env fallback.
- **Country/location.** `--country` (default `SA`; plus optional `--timezone`,
  `--currency`, `--unit-system`) is applied to HA core config after onboarding,
  so the finished device is not left warning that no country is configured.

### Troubleshooting

- **`401` on `owner-and-token` / `addon-repository` right after boot.** Core's
  HTTP socket comes up minutes before its auth provider, onboarding views, and
  Supervisor link finish initializing; in that window `/api/onboarding` and
  `/api/hassio/*` answer a transient `401`. `connect` now waits for a
  *definitive* onboarding signal (HTTP 200 = wizard, or 404 = already
  onboarded) instead of "the socket answered", so it holds until Core is usably
  up. If you still see a `401`, wait until the Home Assistant onboarding screen
  actually loads in a browser, then re-run.
- **`401` on `addon-repository` (or any store/add-on step) that persists across
  re-runs.** This is not a timing issue: current Home Assistant locks the
  `/api/hassio` REST proxy to an allowlist (backups, add-on logs,
  changelog/docs) and answers `401` for `/store*` and `/addons*`. `smart-onboard`
  drives those through the `supervisor/api` WebSocket command instead, so a build
  from this repo should not hit it — if you do, you are running an older
  `smart-onboard` binary; rebuild from `cmd/smart-onboard`.
- **`await-provision` times out with "network is unreachable" to the cloud
  (visible in the agent add-on log).** The add-on cannot reach
  `--cloud-base-url` / `--mqtt-host` from *inside* the gateway's network. The
  classic cause is testing against a VM: `10.0.2.2` reaches the host only under
  QEMU/SLIRP user-networking — **VirtualBox NAT does not expose the host at
  `10.0.2.2`** (the guest has no route there). Point `--cloud-base-url` and
  `--mqtt-host` at the host's real LAN IP (e.g. `http://192.168.100.73:8090`,
  `--mqtt-host 192.168.100.73`) and make sure the cloud and broker listen on all
  interfaces (`0.0.0.0`), not just loopback. Because HA loads add-on options
  only at container start, `smart-onboard` restarts the already-running agent
  when a re-run changes its config, so simply re-running with the corrected
  flags resumes cleanly.
- **"owner already exists" on what should be a first run.** `smart-onboard`
  creates the owner only on a genuinely fresh flash; on a device that already
  has one it logs in with the supplied credentials. Seeing "owner already
  exists" on a supposedly clean unit means the image is **not fresh** — a VM
  cloned from a snapshot/disk of a previous unit, or a device where someone
  clicked partway through the browser onboarding wizard. Either supply the
  credentials that owner was created with, or **re-flash stock HAOS** for a
  clean run (do not mix browser onboarding with the CLI on the same device). For
  the team's VirtualBox `Onboarding` VM, see [`haos-vm-reset.md`](haos-vm-reset.md).

### What the CLI does (steps)

| Step | Action |
| ---- | ------ |
| `connect` | Wait for Core's API to answer (bounded by `--wait-core`). |
| `owner-and-token` | Create the HA owner (or log in on a re-run), mint a long-lived token (re-run-safe: the previous run's token is purged first, since HA refuses a duplicate), and set core config (country/time zone) so the device isn't left warning about missing location. |
| `addon-repository` | Register this repo (and any community add-on repos) in the add-on store. |
| `install-addons` | Install the bootstrap add-ons (agent + Mosquitto + Zigbee2MQTT + Matter Server) at **latest**. In `--dev` mode the agent add-on is **skipped** — you install it from the local checkout via `scripts/sync-addon-to-vm.sh` (see below); the step only checks it is present. |
| `start-broker` | Start the Mosquitto broker add-on. Installing it isn't enough: a stopped broker means the `mqtt` service is never registered, so Z2M can't reach a broker and the agent's local MQTT (pairing + coordinator backup) is dead. Runs before `configure-zigbee` so the broker is up first. Skipped if the manifest has no broker. |
| `configure-zigbee` | Point the Zigbee2MQTT add-on at the coordinator (`serial.port` + `serial.adapter`) and start it, so the unit can pair devices immediately instead of shipping with an unconfigured radio. Defaults to `/dev/ttyACM0` + adapter `ember` (Sonoff ZBDongle-E); set with `--zigbee-port` / `--zigbee-adapter`. **Skipped** when `--zigbee-port` is empty — the default under `--dev`, where a VM has no radio and pairing is validated against a fake `light.*` entity. For a `dd` golden image keep the port serial-agnostic (`/dev/ttyACM0`): a `/dev/serial/by-id/…` path embeds one dongle's serial and breaks every clone. |
| `configure-agent` | Set the agent add-on options (`cloud_base_url`, `factory_key`, `mqtt_*`). |
| `start-agent` | Start the agent add-on. |
| `await-provision` | Wait for the agent to provision (bounded by `--wait-provision`) and read back the `uid` + claim code. |

The CLI installs the **version-free bootstrap add-on set** from
`smart-home-agent/fleet/release.json` (repo + slugs, no versions; override for
testing with `--manifest`). There is no version pinning: the agent enables
HA-native `auto_update` on the add-ons it manages and its `updateAll` engine
keeps agent/HAOS/Core/add-ons on the latest via a daily self-check or an
on-demand fleet **Update**. See [`fleet-update.md`](fleet-update.md).

## Local agent dev flow (`--dev`)

For iterating on the agent against a local HA VM without publishing a GHCR image
every time, the agent add-on is installed from your **local checkout** by
`scripts/sync-addon-to-vm.sh` (which serves the source + Laravel to the VM), and
`--dev` wires `smart-onboard` around that.

**How it fits together.** `smart-onboard --dev` does **not** deliver the add-on
source itself — it drives everything else and hands the agent add-on off to the
sync script:

1. **Host side — serve the source + cloud.** Run `scripts/sync-addon-to-vm.sh
   serve`: it builds `dist/smart-home-agent.tgz`, serves it on `0.0.0.0:8765`,
   starts Laravel on `0.0.0.0:8090`, and prints the `curl` one-liner + the
   `cloud_base_url` / `mqtt_host` values.
2. **Install the local add-on in HA.** In the HA terminal, `curl` the tarball
   into `/addons` and extract it, then Settings → Add-ons → Add-on store → ⋮ →
   **Check for updates** → Local add-ons → **Smart Home Agent** → **Install**.
   Supervisor builds it locally as `local_smart_home_agent`.
3. **Run `smart-onboard --dev`.** The `install-addons` step **skips** the store
   agent (printing the reminder above) and only checks `local_smart_home_agent`
   is present — if it isn't yet, the step fails with that guidance, and because
   the run is resumable you just install it and re-run. Other add-ons
   (Mosquitto, Zigbee2MQTT, Matter) still install from the store at latest. The
   `configure-agent` / `start-agent` / `await-provision` steps then run against
   the local add-on, defaulting `cloud_base_url` to `http://10.0.2.2:8090` and
   `mqtt_host` to `10.0.2.2` (override with `--cloud-base-url` / `--mqtt-host`).

**Iterating on agent code.** Re-run `scripts/sync-addon-to-vm.sh` to rebuild the
tarball, re-drop it in the HA terminal, then rebuild + restart the local add-on
(`ha addons rebuild local_smart_home_agent && ha addons restart
local_smart_home_agent`). See [`local-testing.md`](local-testing.md) Track 3.

## Contract re-validation (per Core version)

Re-validate these once per new HA Core baseline on the bring-up unit — the HA
surfaces the CLI and agent drive move occasionally, so record the outcome:

| Question | Finding |
| -------- | ------- |
| Onboarding + auth API drives owner/token on this Core version? | |
| `supervisor/api` WS command still serves store/add-on/OS/Core calls (and `/api/hassio` still allows add-on logs)? | |
| Full-slug resolution for the community add-ons (repo-hash prefix) | |
| Claim code read back from add-on log / persistent notification | |
| Agent `updateAll` brings add-ons/OS/Core to latest + enables `auto_update` | |
