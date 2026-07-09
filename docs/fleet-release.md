# Fleet release manifest

A **fleet release** is one pinned, tested set of versions for everything running
on a gateway: HAOS, HA Core, the add-ons we depend on, and our agent. Every
gateway in the fleet should converge to the current release.

Pin versions explicitly — never let a gateway auto-update Core/OS/add-ons out
from under a tested release.

## Cloud-orchestrated rollout (how a new version reaches the fleet)

`smart-onboard` applies a release **once, at onboarding**. Ongoing rollout is
now **cloud-orchestrated** and mirrors the config/shadow "desired vs reported"
convergence pattern — the cloud is the only rollout authority (add-on
`auto_update` stays off fleet-wide):

1. **Publish the image.** Bump `version:` in
   [`../smart-home-agent/config.yaml`](../smart-home-agent/config.yaml), commit,
   and push a `vX.Y.Z` tag. The GitHub Actions workflow
   ([`.github/workflows/build.yaml`](../.github/workflows/build.yaml)) builds the
   multi-arch image with `home-assistant/builder` and pushes it to the **public**
   GHCR package `ghcr.io/prameg/smart-home-edge/{arch}-smart_home_agent`
   (gateways pull anonymously — no token). The tag is passed as the
   `BUILD_VERSION` build-arg so the agent's `-ldflags` version stamp matches
   `config.yaml`.
2. **Register the release in the cloud.** Create a `releases` row (the cloud twin
   of `fleet/release.json`): `release_id`, `agent_version`, optional
   `haos_version`/`core_version`/`addons[]`, `notes`.
3. **Assign a target.** On the admin **/gateways** fleet page, assign the release
   to a canary subset (filter to a set, then "Apply to matching", or use the
   per-row "Assign release"). This sets `gateways.target_release_id`, bumps the
   per-gateway monotonic `release_seq`, and dispatches `PublishGatewayRelease`,
   which publishes the **retained** `homes/{uid}/release/desired` doc.
4. **Edge converges.** The agent (subscribed to `release/desired`) updates
   dependency add-ons / OS / Core first and **its own add-on last** (self-update
   restarts it); the target is persisted to `/data` so the flow is resume-safe
   across the restart.
5. **Watch convergence.** On every connect (and after a self-update reboot) the
   agent republishes the retained `homes/{uid}/versions` doc; cloud ingest writes
   `agent_version`/`ha_version`/`os_version` and sets `reported_release_id`.
   `rollout_status` flips to `converged` when
   `reported_release_id == target_release_id`. Widen the canary once it converges.
6. **Rollback** = re-assign the prior release (its image is still on GHCR). The
   `release_seq` still advances, so the edge sees a strictly-newer doc and
   re-applies the older release rather than ignoring it.

A `smart-home:reconcile-release` sweep re-publishes for any gateway whose
`release_synced_seq` is behind `release_seq` (a publish that never reached the
broker), the sibling of the config reconcile sweep.

## Machine-readable twin: `smart-home-agent/fleet/release.json`

This document is the human-readable manifest; its machine-readable twin lives at
[`../smart-home-agent/fleet/release.json`](../smart-home-agent/fleet/release.json)
and is what the `smart-onboard` CLI actually reads (embedded into the binary at
build; override with the `smart-onboard --manifest` flag). Keep the two in lockstep:
when you populate the table below, populate `release.json` (`release_id`, the
`addons[]` versions, `haos`/`core` versions) and flip its `populated` flag to
`true`. While `populated` is `false`, `smart-onboard` installs the latest add-ons
and skips version pinning (it warns rather than failing), so the tool still runs
end-to-end on a bring-up unit.

The future **Jetson AI-node** contract (JetPack version, container image
digests, model versions) will get its own section in `release.json` when that
phase lands — see the companion-node decision in
[`factory-onboarding.md`](factory-onboarding.md).

## Current release: `2026.07-r1` (template — not yet populated)

> **Status:** this is the manifest *shape*, not a validated release. Fill in the
> exact versions after flashing the bring-up Pi 5 — replace each `TBD` with what
> `ha os info` / `ha core info` / `ha addons` report — and only then treat it as
> the pinned release.

| Component            | Version | How to pin                                         |
| -------------------- | ------- | -------------------------------------------------- |
| Home Assistant OS    | `TBD`   | `ha os update --version <x>`                        |
| Home Assistant Core  | `TBD`   | `ha core update --version <x>`                      |
| Add-on: Mosquitto    | `TBD`   | pin in add-on store (local broker on the Pi)        |
| Add-on: Zigbee2MQTT  | `TBD`   | pin in add-on store                                 |
| Add-on: Matter Server| `TBD`   | pin in add-on store                                 |
| Add-on: Smart Home Agent | `0.1.0` | this repo's `smart-home-agent/config.yaml` version |

## Pinning procedure (Pi 5, HAOS)

Home Assistant OS auto-updates by default; disable that and set exact versions:

```bash
# From the HA CLI (Terminal & SSH add-on, or `ha` over SSH):
ha os info                       # read current OS version
ha core info                     # read current Core version
ha addons                        # list installed add-ons + versions

# Pin OS + Core to a known-good version
ha os update   --version <os_version>
ha core update --version <core_version>

# Disable auto-update on the add-ons we depend on (pin in the UI or):
ha addons options <slug> --auto_update=false
```

Record the resulting versions in the table above and tag this file's release id
(`YYYY.MM-rN`). The local Mosquitto + Zigbee2MQTT + Matter add-ons are what keep
automations running offline (see `offline-validation.md`); their versions are
part of the contract we validate.

## Rationale

- **Reproducibility** — a support case or a field bug is meaningless without
  knowing exactly what shipped. The manifest is the answer to "what is on this
  unit?".
- **Offline autonomy depends on local components** — Zigbee2MQTT + local
  Mosquitto + Matter run the home when the WAN is down; pinning them is pinning
  the offline behavior we tested.
- **Cloud rollout** builds on this: the cloud pushes a target release id per
  gateway (`homes/{uid}/release/desired`) and each gateway reports convergence
  (`homes/{uid}/versions`), exactly like the device shadow does for a single
  device. See the "Cloud-orchestrated rollout" section above.
