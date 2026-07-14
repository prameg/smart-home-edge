<!--
  MIRRORED DOC. Source of truth lives in the cloud repo at docs/mqtt-contract.md
  and in code at app/Mqtt/Topics.php. When the contract changes, edit the cloud
  repo + App\Mqtt\Topics + this file + smart-home-agent/internal/contract in one
  change. Do not let the two repos drift.
-->

# MQTT topic + payload contract

The single boundary between the cloud repo and this edge repo (Go agent add-on).
It is mirrored in code by the cloud repo's `App\Mqtt\Topics` and by this repo's
`smart-home-agent/internal/contract`; change all of them together.

All topics are rooted at `homes/{gateway_uid}/...`. The broker (mosquitto +
mosquitto-go-auth) confines a gateway to **its own** `homes/{gateway_uid}/#`
subtree; `gateway_uid` is the opaque `gateways.uid`, never the hardware serial.
`device_uid` is the stable `devices.uid` — the cloud never sees raw Home
Assistant `entity_id`s (the agent applies the `devices.entity_map`).

## Uplink (agent -> cloud)

| Topic                            | QoS | Retain | Payload                                                                                                                |
| -------------------------------- | --- | ------ | ---------------------------------------------------------------------------------------------------------------------- |
| `homes/{uid}/state/{device_uid}` | 1   | no     | `{ "state": { "state": "<string>", "attributes": {...} }, "version"?: <int>, "ts": "<iso8601>" }`                      |
| `homes/{uid}/event/{type}`       | 1   | no     | `{ "event_id": "<string>", "severity": "info\|warning\|critical", "device_uid"?: "<string>", "ts": "<iso8601>", ... }` |
| `homes/{uid}/inventory`          | 1   | yes    | `{ "hash": "<string>", "entities": [{ "entity_id": "<string>", "domain": "<string>", "name": "<string>", "area"?: "<string>", "device_class"?: "<string>", "unit_of_measurement"?: "<string>", "ha_device_id"?: "<string>", "ha_device_name"?: "<string>" }], "ts": "<iso8601>" }` |
| `homes/{uid}/cmd/ack`            | 1   | no     | `{ "cmd_id": "<uuid>", "status": "acked\|failed" }`                                                                    |
| `homes/{uid}/availability`       | 1   | yes    | `online` (retained) / `offline` (last-will)                                                                            |
| `homes/{uid}/versions`           | 1   | yes    | `{ "agent_version": "<string>", "ha_version"?: "<string>", "os_version"?: "<string>", "ts": "<iso8601>" }` |
| `homes/{uid}/update/status`      | 1   | no     | `{ "update_id"?: "<uuid>", "status": "started\|ok\|failed", "error"?: "<string>", "ts": "<iso8601>" }` |

Semantics:

- **state** — the `state` object is **HA-native**: the entity's HA state string
  plus its attribute bag (see "State vocabulary" below). It is HA's ACTUAL state,
  never an echo of the desired document. `version` echoes the `desired_version`
  the agent has applied. Ingest ordering is guarded on two axes: a message whose
  `version` is lower than the stored `reported_version` is dropped (QoS-1
  duplicate / reordering safe); for EQUAL versions (pure telemetry never bumps
  the version) the `ts` breaks the tie — a report not strictly newer than the
  last applied one is dropped, so parallel workers can't roll the shadow back.
  Convergence is `reported_version == desired_version`; there is no separate
  state ack. Absent `version` means "still converged" (pure telemetry, no
  desired ever set) — this is also what the (re)connect reconcile publishes.
  Each accepted state TRANSITION is appended to the cloud's `device_state_changes`
  history (a re-report of the same state is not); this is a cloud-internal seam,
  not part of the wire.
- **event** — deduped on `event_id` (an indexed, unique-per-home column); a
  redelivery never creates a duplicate row.
- **inventory** — retained snapshot of the gateway's HA entities. The cloud
  upserts a `Device` per allow-listed `domain` (seeding name/room once — cloud
  owns naming thereafter), flags vanished entities `missing_since` (never
  deletes), and republishes `config` when the device set changes. Deduped on
  `hash`: an unchanged retained redelivery is a no-op. Each entry also carries
  HA's grouping metadata — `device_class`, `unit_of_measurement`, and the HA
  device-registry `ha_device_id`/`ha_device_name` — which are HA-authoritative
  (refreshed on every resync) and stored as columns on `devices`; `area` seeds a
  first-class `rooms` row once (cloud-owned thereafter).
- **cmd/ack** — idempotent; a command already in a terminal state is not
  re-transitioned.
- **availability** — drives `gateways.is_online` + `gateways.last_seen_at`. HA's
  standard availability convention (a retained `online` / last-will `offline`).
- **versions** — retained software inventory the agent publishes on every
  (claimed) connect and after a self-update reboot, so `gateways.agent_version` /
  `ha_version` / `os_version` reflect what is ACTUALLY running independent of
  provisioning (which only ran at first boot / recovery). Claimed-only (the
  broker ACL confines an unclaimed gateway to `availability`/`config`).
- **update/status** — fleet-update progress: `started` when the agent begins an
  update run, `ok` when everything is on the latest, or `failed` with an `error`
  string on a hard failure. Emitted for both cloud-triggered updates (carrying
  the command's `update_id`) and the agent's own daily self-check (no
  `update_id`). The cloud reflects the latest report onto `gateways.update_status`
  / `update_error` / `update_completed_at`; there is no convergence cursor — the
  most recent report always wins.

## Downlink (cloud -> agent)

| Topic                                    | QoS | Retain | Payload                                                                                                                                                      |
| ---------------------------------------- | --- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `homes/{uid}/cmd`                        | 1   | no     | `{ "cmd_id": "<uuid>", "device_uid": "<string>", "action": "<ha.service>", "params": {...}, "ts": "<iso8601>", "ttl_sec": <int>, "source": "user\|system" }` |
| `homes/{uid}/shadow/desired/{device_uid}` | 1   | yes    | `{ "device_uid": "<string>", "desired_version": <int>, "state": { "state": "<string>", "attributes"?: {...} } }`                                             |
| `homes/{uid}/config`                     | 1   | yes    | `{ "claimed": <bool>, "config_version": <int>, "entity_map": [{ "device_uid": "<string>", "entity_id": "<string>" }], "ts": "<iso8601>" }`                    |
| `homes/{uid}/update`                     | 1   | no     | `{ "update_id": "<uuid>", "ts": "<iso8601>" }`                                                                                                               |

Semantics:

- **cmd** — one-shot action (an HA service call). Idempotent on `cmd_id`. `ttl_sec`
  is the MQTT-3.1.1 substitute for message expiry: past the TTL the agent drops
  the command and the cloud marks it `expired` on purpose. The agent reconciles
  pending commands on reconnect.
- **shadow/desired/{device_uid}** — **per device** and retained, mirroring the
  state topic, so a reconnecting/offline gateway gets the latest desired state
  for **every** device immediately and converges by version. (A single shared
  `shadow/desired` topic retains only the last device's document, silently
  breaking convergence with 2+ devices — hence one retained topic per device.)
  The agent subscribes `shadow/desired/#`. The `state` object is the **HA-native
  target** (same vocabulary as reported — see below), and is the full target that
  **replaces** the previous desired (the cloud does not blind-merge). The agent
  applies it, then reports HA's actual resulting state on the `state` topic
  carrying the applied `version` (no separate ack).
- **config** — retained; the cloud's source of truth for the agent's runtime.
  Carries the claim status and the `device_uid` <-> `entity_id` map. Monotonic on
  `config_version` (a lower redelivery is ignored). Published on claim, unclaim,
  and any device add/rename/remap/removal, so claim and every device change
  self-heal on the edge with no manual resync.
- **retained cleanup on unclaim** — when a gateway is unclaimed (home deleted /
  re-claimed), the cloud publishes empty retained payloads to every per-device
  `shadow/desired/{device_uid}` and to `inventory`, so a re-claimed gateway never
  inherits a previous home's desired state or inventory.
- **update** — one-shot "update everything to the latest" command from the
  `/gateways` fleet page (single row or "update matching"). The agent responds by
  bringing everything current via the Supervisor API — dependency add-ons first,
  then HAOS, then Core, and its OWN add-on LAST (a self-update restarts it) — and
  reports progress on `update/status`. There is **no pinning**: managed add-ons
  are set to HA-native `auto_update`, and the agent always targets the latest
  available, so this is not a version-set convergence — it is a "go now" nudge.
  Resume-safe: an OS/self update reboots the unit, and a `/data` marker re-drives
  the (idempotent) pass on restart — each already-current phase no-ops. Deliberately
  **not retained** and without a sequence cursor or reconcile sweep: a gateway that
  misses the command (offline) is converged by its own daily self-check instead.
  Claimed-only (subscribed alongside `cmd`/`shadow`).

## Pairing (gateway-scoped)

Pairing a new physical device is driven from the cloud wizard over the existing
`cmd` + `event` topics — no new topic families, so the broker ACL is unchanged.

Downlink `homes/{uid}/cmd` actions with **no `device_uid`** (the gateway itself
is the target; `commands.device_id` is null):

- `pairing.start` — params `{ "protocol": "zigbee", "duration_sec": 30..254, "session_id": "<uuid>" }`.
  Opens the protocol's join window (Zigbee2MQTT `permit_join` over the
  gateway-local broker). 254 s is Z2M's maximum window.
- `pairing.stop` — params `{ "protocol": "...", "session_id": "<uuid>" }`. Closes it early.

Uplink `homes/{uid}/event/pairing` (QoS 1, not retained) — one message per phase:

```json
{ "session_id": "…", "phase": "device_found", "ts": "…",
  "expires_at": "…", "device": { "ieee": "…", "friendly_name": "…", "model": "…", "vendor": "…" },
  "reason": "…" }
```

Phases: `started` (carries `expires_at`) → `device_found` → `interviewing` →
`completed` | `failed` (carries `reason`) | `stopped` (`reason`: `user` | `timeout`).

The cloud routes this event type to the pairing-session state machine
(`PairingEventHandler`), NOT the generic home-events feed. Transitions are
idempotent under QoS-1 redelivery: a terminal session never regresses, so no
`event_id` dedupe is needed. The agent runs at most one session at a time
(a second `pairing.start` acks `failed` with a `failed` phase event, mirroring
the cloud's 409) and the registered device still arrives via the normal
`inventory` sync — pairing only opens the door.

## State vocabulary (desired and reported share one shape)

Both the reported `state` (uplink) and the desired `state` (downlink `state`
field) use **one HA-native shape**, so they are directly comparable:

```json
{
    "state": "<ha state string>",
    "attributes": { "brightness": 200, "...": "..." }
}
```

- `state` is HA's state string for the entity (`"on"`, `"off"`, a sensor's
  numeric value, `"heat"`, ...). `attributes` is HA's attribute bag.
- **Reported** is HA's actual state, read from HA — never a copy of the desired
  document. (Historically the agent echoed the desired doc back as the reported
  state, which made `reported_state` a meaningless mirror of `desired_state`;
  that is fixed — the agent reads HA back and reports the real state.)
- **Desired** is the same shape used as a _target_. The agent translates it to
  the appropriate HA service call(s) via a **per-domain capability table**
  (`internal/agent/translate.go`), mirrored on the cloud by
  `App\Support\SmartHome\DeviceCapabilities` (which validates desired/command
  payloads) and in `resources/js/types/smart-home.ts` (which picks the UI
  control). Coverage: `light`/`switch`/`fan` (on/off + attributes like
  `brightness`/`percentage`), `lock` (`locked`/`unlocked`), `cover`
  (`open`/`closed` or an `attributes.position`), `climate` (state = hvac mode via
  `set_hvac_mode`, plus `attributes.temperature` via `set_temperature` — one
  document can expand to several ordered calls), and `media_player`
  (play/pause/on/off + `volume_level`). Unknown domains fall back to generic
  on/off via the `homeassistant` domain. `sensor`/`binary_sensor` are read-only.
  Adding a domain is one entry in each of the three mirrors; the shape does not
  change.

Because both sides speak this one vocabulary, `desired_state` vs `reported_state`
can be diffed by content, and convergence is confirmed by `version` as before.

## Claim gate

Until a gateway is **claimed** (bound to a home), it is confined by the broker
ACL to publishing only `availability` and subscribing only to `config`. All
device uplink (`state`/`event`/`inventory`) and command/shadow downlink stay
denied. The agent enforces the same gate itself (availability-only until the
retained `config` doc reports `claimed: true`, and it runs `deactivateClaimed` —
unsubscribing `cmd` + `shadow/desired/#` and clearing its entity map — when a
config reports `claimed: false`), and the cloud ingest drops any home-scoped
message from an unclaimed gateway — three independent layers of one gate.

**Cloud broker principal.** The cloud's own MQTT clients (the `mqtt:subscribe`
uplink daemon and the transient downlink publisher) are not gateway rows; they
authenticate to the broker as a single configured service account
(`SMART_HOME_CLOUD_MQTT_USERNAME`/`_PASSWORD`) that the broker-auth endpoint
recognizes and grants superuser (ACL-exempt) rights, so they can read the
`homes/+/…` uplink filters and write/clear any `homes/{uid}/…` downlink topic.

## Session / reconnect

- The agent connects with `clean_session=false` and a **stable client id** so the
  broker retains its subscriptions and buffers QoS-1 downlink across reboots.
- Graceful reboot is protocol-level: retained `availability` /
  `shadow/desired/{device_uid}` + last-will self-heal; no provisioning round-trip
  is needed. The provisioning HTTP endpoint is only touched on first boot or
  recovery (see below).
- Reconnection is **agent-driven**, not Paho auto-reconnect: a broker credential
  rejection (a password the cloud rotated) is caught and triggers a re-provision
  before the next attempt, instead of looping on a dead password forever.

## Provisioning & claim (HTTP)

Provisioning and claiming happen over **HTTP**, around the MQTT session. The
device-facing endpoints carry no Sanctum session; authorization is decided by
**which secret** is presented, so exactly one credential can be wrong per
operation.

### Three secrets, three jobs

- **Factory key** — the shared enrollment secret
  (`SMART_HOME_PROVISIONING_FACTORY_KEY`), presented as a `Bearer` token (or the
  `X-Auth-Secret` header). **Creation-only**: it authorizes enrolling a _new_
  serial and can never rotate a live gateway's credentials — except inside an
  admin-opened re-enrollment window. Worst-case leak is throttled, admin-visible
  spam of unclaimed rows, never a hijack of a live unit.
- **Provision token** — the per-gateway recovery secret, minted once at
  enrollment and stored by the device in `/data/agent-creds.json`. The **only**
  secret that re-provisions an existing serial (rotating its MQTT password) while
  keeping the same `uid` and the home/claim binding.
- **Claim code** — a short, human-typeable, expiring `XXXX-XXXX` code drawn from
  an unambiguous alphabet. Low-privilege: it only binds an _unclaimed_ gateway to
  a home, so it is safe to display on-device.

### `POST /api/v1/provisioning/gateways` — enroll or recover

An idempotent upsert on the hardware `serial`.
Body: `{ "serial", "provision_token"?, "ha_version"?, "agent_version"?, "os_version"? }`,
plus an optional `Authorization: Bearer <factory_key>`.

- **serial unknown → ENROLL** (needs a valid factory key): creates the row and
  returns, once, the MQTT password, a fresh `provision_token`, and a `claim_code`
  (+ expiry). `201`.
- **serial known → RECOVER** (needs a valid `provision_token`): rotates the MQTT
  password, preserves `uid` + home/claim binding, and reissues a `claim_code`
  only while still unclaimed. The `provision_token` is **not** resent (the device
  already holds it). `200`.
- **serial known but token lost** (factory-reset unit): recovery is refused
  unless an admin has opened a re-enrollment window on the fleet page; inside it
  the factory key re-enrolls that known serial once, rotating **both** the MQTT
  password and the provision token.

Response:
`{ "uid", "topic_namespace", "claim_status", "mqtt": { "username", "password" }, "provision_token": <string|null>, "claim_code": <string|null>, "claim_code_expires_at": <iso8601|null> }`.

Errors are `{ "error": "<code>", "message": "<human>" }` with stable codes the
agent branches on:

| status | `error`                   | meaning                                                           |
| ------ | ------------------------- | ----------------------------------------------------------------- |
| 401    | `factory_key_required`    | new serial with a missing/invalid factory key                     |
| 409    | `recovery_not_authorized` | known serial with a bad/absent token and no open re-enroll window |

### `POST /api/v1/provisioning/gateways/claim-code` — reissue

`{ "serial", "provision_token" }` → `{ "claim_code", "claim_code_expires_at" }`.
Mints a fresh code for a still-unclaimed unit **without** touching MQTT
credentials; returns `409 already_claimed` once claimed. The agent calls it when
its on-device code is missing or expired (e.g. after an unclaim cleared it).

### `POST /api/v1/gateways/claim` — user-facing (Sanctum)

Binds an unclaimed gateway to a home for the signed-in user:
`{ "claim_code", "home_id"?, "home_name"? }`. The code is normalized
(case/dashes) then matched by hash; an expired or already-claimed code is
rejected. On success the code is cleared and the retained `config` doc is
published (`claimed: true`), flipping the agent into claimed mode with no
provisioning round-trip.

### Edge behavior

- **First boot** provisions and blocks with capped exponential backoff (never a
  fatal exit) until it succeeds, so a flaky WAN self-heals with no manual restart.
- **Stale credentials**: a CONNACK bad-credentials rejection makes the agent
  re-provision via its provision token (cool-down guarded), persist the rotated
  password, and reconnect.
- **Claim-code surfacing**: while unclaimed the agent logs the code and posts it
  as an HA persistent notification (reissuing a missing/expired one); it clears
  the notification on claim.
