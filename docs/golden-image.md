# Golden image — decision + build/refresh + field runbook

The **golden image** is a pre-baked disk image that turns field onboarding into
**flash → boot → claim** with zero CLI, zero wizard, and zero per-unit
configuration. It is the primary way units come off the line; the
[`smart-onboard`](factory-onboarding.md) CLI stays as the documented fallback
for non-imaged hardware and mid-flash recovery.

> **Why this is safe to keep simple:** every unit runs the **latest** of
> everything — the agent enables HA-native `auto_update` and its daily self-check
> pulls HAOS/Core forward (see [`fleet-update.md`](fleet-update.md)). So a golden
> image baked months ago updates itself to today's versions shortly after boot.
> "The baked image goes stale in the warehouse" is therefore a non-issue — the
> image only has to be new enough to boot, start the agent, and enroll.

## Decision

> **GO: `dd`/snapshot a fully-onboarded reference unit, with the factory key
> baked in.** Bring up one reference Pi through `smart-onboard` (stock HAOS →
> owner → add-ons → agent configured with the factory key → provisioned), then
> **sanitize its per-unit identity** and image the whole disk. Flashing that
> image onto a blank unit yields a device that boots straight into a running,
> pre-configured agent, enrolls itself under its own hardware serial, and
> surfaces its own claim code.

Two sub-decisions:

- **Image production = whole-disk `dd`/snapshot of a reference unit** (not HAOS
  backup-restore). It captures the exact, already-validated on-disk state — HA
  owner, installed add-ons, agent add-on + its options — as one artifact that a
  factory flashes with ordinary imaging tools. No restore step, no partition
  surgery, nothing HA-version-specific in the flash path.
- **Factory key = baked into the image** (in the agent add-on's
  `/data/options.json`), not injected per unit. Under the enrollment model
  (creation-only tokens, throttled provisioning, quarantine on abuse) the
  factory key is a low-value bearer secret whose blast radius is bounded
  server-side, so baking it removes the only per-unit manual step and keeps the
  field runbook to "flash → boot → claim".

### Why not the alternatives

- **HAOS backup-restore from the CONFIG partition** — more HA-native, but adds
  moving parts to the field flow (flash base HAOS, then restore a backup, then
  wait for the restore to settle) and couples the artifact to HA's backup format
  and Core version. A `dd` image is a single self-contained artifact with a
  dumb, universal flash path — the right trade for a factory line.
- **Per-unit factory-key injection at flash/first-boot** — smaller blast radius
  if an image leaks, but it re-introduces a per-unit provisioning step, which is
  exactly what the golden image exists to remove. Revisit if/when a leaked image
  becomes a real threat (see [Factory-key risk](#factory-key-risk--rotation)).
- **Custom buildroot / HAOS image** — already rejected in
  [`factory-onboarding.md`](factory-onboarding.md): it diverges from stock HAOS
  and rebuilds on every OS bump. The golden image is stock HAOS captured *after*
  onboarding, not a custom OS build.

## `/data` hygiene — the one correctness requirement

A `dd` image captures **everything**, including per-unit identity that must
**not** be shared across units. Before imaging, the reference unit's agent
identity and applied state must be wiped so each flashed unit enrolls fresh and
gets its **own** `uid` + claim code. The agent stores these under the agent
add-on's private `/data` volume:

| File | What it is | Golden-image action |
| ---- | ---------- | ------------------- |
| `agent-creds.json` | The enrolled cloud identity + MQTT credentials (`uid`, broker user/pass). **Per-unit.** | **DELETE** — the single file that must never ship in an image. |
| `applied-versions.json` | Reported version state. | **DELETE.** |
| `update-in-progress.json` | Resume marker for an interrupted update pass, if present. | **DELETE.** |
| `gateway-config.json` | Applied config/shadow state. | **DELETE.** |
| `options.json` | Add-on options (cloud URL, `mqtt_*`, **`factory_key`**). Not per-unit. | **KEEP** — this is where the baked factory key lives. |

Deleting `agent-creds.json` is safe by design: provisioning is built so a
`/data`-wiped unit that lost its token **re-provisions automatically on boot**
using the baked factory key (see
[`internal/provision/provision.go`](../smart-home-agent/internal/provision/provision.go)).
The `uid` is derived from the **hardware serial** (e.g. the Pi's CPU serial), so
every flashed unit enrolls under its own identity even though it booted from an
identical image — the clone does not inherit the reference unit's `uid`.

> **Shared HA owner (accepted, documented):** the image also carries the
> reference unit's HA owner account, so every flashed unit shares one HA login.
> This is what lets us skip the onboarding wizard in the field. It is accepted
> for pre-prod; rotate the owner credential per batch if this ever needs
> hardening, or move owner creation to first-boot.

## Build pipeline (cut a new golden image)

1. **Bring up a reference unit.** Flash stock HAOS on a clean unit of the target
   hardware (Pi 5 today), plug in the Zigbee coordinator (ZBDongle-E), boot it on
   the network, and run `smart-onboard` against the **production** cloud + broker
   with the real factory key (see
   [`production-onboarding.md`](production-onboarding.md)). Let it fully
   provision and confirm it surfaces a claim code. The `configure-zigbee` step
   bakes the radio config (`serial.port` + `serial.adapter`) into the Zigbee2MQTT
   add-on, which the `dd` clone carries to every unit — so keep the port
   **serial-agnostic** (`/dev/ttyACM0`, the default), never a `/dev/serial/by-id/…`
   path that embeds this dongle's serial and would break every other unit.
   **Before imaging, confirm section 4b (real-coordinator pairing) passes** — the
   whole point of baking the radio is that a flashed unit pairs on day one.
2. **Let it update (optional but recommended).** Leave it running a while so its
   self-check + `auto_update` pull everything to the latest; this bakes a warm,
   up-to-date starting point. (Skippable — field units update themselves after
   boot regardless.)
3. **Sanitize `/data`.** Stop the agent add-on and delete the per-unit files in
   the table above from the agent add-on's `/data` (keep `options.json`). Also
   clear anything else unit-specific you added (logs, HA backups).
4. **Power down cleanly and capture the disk.** Shut the unit down (not pull
   power) and image the whole disk to a compressed artifact:

   ```bash
   # From a host with the unit's storage attached (e.g. the SD/NVMe on a reader):
   dd if=/dev/<device> bs=4M conv=sync,noerror status=progress | zstd -o golden-<hw>-<haos>-<yyyymmdd>.img.zst
   ```

   Name it for the hardware, the HAOS version it was built on, and the date, so
   the fleet can tell images apart.
5. **Shrink + publish.** Optionally shrink the last partition before capture so
   the image fits the smallest supported media, then publish the artifact to the
   image store the factory pulls from. Record the agent version it was baked with
   (informational only — units update themselves to latest after boot).

## Refresh pipeline (when to re-bake)

Re-bake only when the **bootstrap floor** moves, not on every release:

- A new HAOS baseline (the image's OS is too old to boot/enroll reliably), or
- The bootstrap add-on set in [`fleet/release.json`](../smart-home-agent/fleet/release.json)
  changes (an add-on added/removed), or
- The factory key is rotated, or
- The cloud/broker endpoints baked into `options.json` change, or
- The Zigbee coordinator model (and thus the baked `serial.port`/`serial.adapter`)
  changes — e.g. moving from the ZBDongle-E (`ember`) to a ZBDongle-P (`zstack`).

Routine agent/Core/add-on version bumps do **not** require a re-bake: field units
pull them via `auto_update` + the daily self-check. This keeps image churn low —
the golden image is a *bootstrap floor*, and "latest" is reached on-device.

## Field runbook (claim-only)

1. **Flash** the golden image onto the unit's media with any standard imager
   (Raspberry Pi Imager, `dd`, Balena Etcher).
2. **Boot** the unit on a network with a path to the cloud + broker. It comes up
   with HA already onboarded and the agent already running; on first boot it
   re-provisions (fresh `agent-creds.json`) and surfaces a **claim code** (agent
   log + an HA persistent notification; also visible in the Ingress panel).
3. **Claim** the code in the Smart Home app to bind the gateway to a home. The
   unit is already on the latest and keeps itself current via `auto_update` + its
   daily self-check — no CLI, no version pinning, no admin action.

If a unit was **not** imaged, the image is corrupt, or first-boot provisioning
never surfaces a code, fall back to the `smart-onboard` CLI
([`factory-onboarding.md`](factory-onboarding.md)) — it drives a stock-HAOS unit
through the same end state.

## Fallback & recovery

- **Bad/bricked flash** — re-flash; if the image itself is suspect, bring the
  unit up with `smart-onboard` instead and re-cut the image.
- **Claim code never appears** — check the agent add-on log / Ingress panel;
  cloud unreachable at first boot is handled by the agent's enroll retry loop
  (2s → 5m backoff), so give it time on a slow link before intervening.
- **A shipped image accidentally contained `agent-creds.json`** — every unit
  from that batch would collide on one `uid`. Treat as a spoiled batch: re-cut
  the image with correct `/data` hygiene and re-flash. (The hardware-serial
  `uid` derivation only protects units that enroll *fresh*; a baked credential
  overrides it.)

## Factory-key risk & rotation

The baked factory key is a shared bearer secret. Its exposure is bounded by the
enrollment model — provisioning tokens are creation-only, throttled, and a unit
can be quarantined server-side — so a leaked key lets an attacker *enroll*
gateways (which then sit unclaimed and harmless) but not impersonate a claimed
unit. To rotate: issue a new factory key in the cloud, re-bake the golden image
with the new key in `options.json`, and retire the old key once the field has
turned over. Rotation is the trigger for a [refresh](#refresh-pipeline-when-to-re-bake),
not an emergency re-flash of the whole fleet.
```
