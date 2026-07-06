# Smart Home Agent

Bridges this Home Assistant instance to the smart-home cloud. It self-provisions
on first boot, connects to the cloud MQTT broker over TLS with a persistent
session, and implements the platform contract in both directions.

## What it does

- **Provisions** on first boot (or after a `/data` wipe) against
  `POST /api/v1/provisioning/gateways`. A new serial enrolls with the shared
  factory key and is issued a per-gateway **provision token** (stored in `/data`)
  used for all later recovery; a normal reboot reuses the stored credentials and
  never re-provisions. First boot retries with backoff until it succeeds rather
  than exiting.
- **Reports its HA inventory** — publishes the retained
  `homes/{uid}/inventory` so the cloud can register a device per allow-listed
  entity and push back the authoritative `device_uid ↔ entity_id` map (see
  "Devices" below).
- **Uplink** — publishes mapped-device state changes to
  `homes/{uid}/state/{device_uid}` (HA-native `{ state, attributes }`), events to
  `homes/{uid}/event/{type}`, and a retained `homes/{uid}/availability` (`online`,
  with an `offline` last-will).
- **Downlink** — subscribes to `homes/{uid}/cmd` (one-shot HA service calls,
  TTL-gated, ack'd on `homes/{uid}/cmd/ack`) and the retained
  `homes/{uid}/shadow/desired` (applies the desired state, then reports HA's
  **actual** resulting state on the state topic carrying the applied version —
  never an echo of the desired doc).

The topic/payload boundary is documented in
[`../docs/mqtt-contract.md`](../docs/mqtt-contract.md).

## Web UI (sidebar panel)

The add-on exposes a **Home Assistant sidebar panel** ("Smart Home") via
Ingress. Supervisor proxies pre-authenticated requests to the agent's own small
web server (bound to `ingress_port`, default `8099`), so the panel is visible to
HA admins only and needs no separate login. It renders live status and a few
actions so you never have to read add-on logs.

**Status** (polled): claim status + broker-connected state, gateway `uid`,
serial, agent/HA version, the cloud origin and broker target, the config
version, and the mapped-device count. While unclaimed it also shows the current
claim code with a copy button. No secrets are ever sent to the browser (never
the MQTT password or provision token).

**Actions**:

| Action                | What it does                                                                                     |
| --------------------- | ------------------------------------------------------------------------------------------------ |
| Reissue claim code    | Mints and resurfaces a fresh claim code (unclaimed only; refused once claimed).                  |
| Republish inventory   | Forces a fresh HA-entity inventory publish to the cloud (resets the hash gate first).            |
| Create test light     | Provisions a controllable `light.bed_light` (backed by an `input_boolean`) for VMs/dev with no real hardware. Idempotent — republish inventory afterwards to register it. |
| Reconcile state       | Re-reports HA's current state for every mapped device (self-heals a stale cloud `reported_state`). |
| Re-provision          | **Side effect:** recovers credentials, **rotating the MQTT password**, then reconnects the broker. Use only when the broker is rejecting credentials — it is cool-down guarded. |

**Mapped devices**: a table of the authoritative `device_uid ↔ entity_id`
bindings with each entity's current HA state (best-effort; an unreachable entity
shows `unavailable`).

Outside HAOS the same server runs on `WEBUI_PORT` (default `8099`) — open
`http://localhost:8099`. The panel comes up only after provisioning succeeds; a
unit still stuck on first boot is diagnosed from the add-on log.

## Configuration

| Option              | Meaning                                                                 |
| ------------------- | ----------------------------------------------------------------------- |
| `cloud_base_url`    | Cloud origin, e.g. `https://app.example.com`.                           |
| `factory_key`       | Provisioning shared secret (`SMART_HOME_PROVISIONING_FACTORY_KEY`).     |
| `serial`            | Optional. Overrides the auto-derived hardware serial (Pi CPU serial / hostname). |
| `mqtt_host` / `mqtt_port` | Cloud broker host + port (TLS listener is typically `8883`).      |
| `mqtt_tls`          | Use TLS to the broker (recommended: `true`).                            |
| `mqtt_tls_insecure` | Dev only: skip broker certificate verification.                         |
| `log_level`         | `debug` \| `info` \| `warning` \| `error`.                              |

## Devices

Once the gateway is **claimed**, devices appear automatically — you do not
hand-maintain a map:

1. The agent reports its HA entity inventory on the retained `inventory` topic.
2. The cloud registers a `Device` (with a stable `device_uid`) for each entity
   in an allow-listed domain (`light`, `switch`, `climate`, `cover`, `lock`,
   `fan`, `sensor`, `binary_sensor`, `media_player`) and pushes the authoritative
   `device_uid ↔ entity_id` map back down on the retained `config` topic.
3. The agent applies that map. An entity that disappears is flagged (not
   deleted) and drops out of the map until it returns.

So the authoritative map is **cloud-owned** and self-heals — HA owns which
entities exist, the cloud owns device identity/naming/desired-state.

### Dev-only entity-map override

For running the agent **without the cloud** (see `../docs/local-testing.md`,
Track 1), drop an `entity-map.json` into the add-on's config folder to point a
`device_uid` at a real entity. It is a bootstrap convenience only: once the
cloud sends a `config`, the cloud map wins. The folder is the `addon_config`
mapping, mounted at **`/config`** in the container and on the host at
`/addon_configs/local_smart_home_agent/` (reach it via the *Samba* / *File
editor* add-ons):

```json
[
  { "device_uid": "spike-light-1", "entity_id": "light.living_room" }
]
```

Restart the add-on after editing (it's read once at startup).

For a VM with no real hardware, create a test `light.*` entity first with the
**Create test light** action in the Smart Home panel (see
[`../docs/local-testing.md`](../docs/local-testing.md#create-a-test-light-track-23)).

## Claiming

While unclaimed, the agent surfaces a **short claim code** (`XXXX-XXXX`) both in
the add-on log and as a **Home Assistant persistent notification**, so you never
have to read logs to find it. Enter it in the cloud's user-facing claim flow to
bind the gateway to a home; the notification clears automatically on claim. If
the code is missing or expired the agent reissues one via
`POST /api/v1/provisioning/gateways/claim-code` (authenticated by the provision
token). Until claimed, the broker confines the gateway to `availability`/`config`
only and the cloud drops any device uplink (no home to attach to) — this is
expected.

## Recovery

The agent re-provisions itself without manual intervention when its stored MQTT
password goes stale (e.g. the cloud rotated it): a broker "bad credentials"
rejection triggers a token-authenticated re-provision (cool-down guarded) that
rotates the password and reconnects — the same `uid` and home/claim binding are
preserved. A `/data`-wiped unit that lost its provision token recovers
**automatically** on the factory key alone (a fresh token is minted), so a
genuine factory reset self-heals with no operator action. If that unit was
already **claimed**, the cloud quarantines it — it reconnects but device control
stays frozen until the home owner (or an admin) taps **Confirm gateway** — since
a shared factory key plus a known serial must never silently reclaim a bound
home. An admin can **suspend** a suspect unit on the fleet page, which blocks its
recovery and disconnects it until restored.

## Running outside HAOS (local development)

The agent falls back to environment variables for every option, so you can run
it against a local HA + broker without the Supervisor:

```bash
export SUPERVISOR_TOKEN=<a long-lived HA token>
export HA_REST_BASE_URL=http://homeassistant.local:8123/api
export HA_WEBSOCKET_URL=ws://homeassistant.local:8123/api/websocket
export CLOUD_BASE_URL=http://localhost
export FACTORY_KEY=<SMART_HOME_PROVISIONING_FACTORY_KEY>
export MQTT_HOST=localhost MQTT_PORT=1883 MQTT_TLS=false
export AGENT_DATA_DIR=./.data

go run .
```

## State on disk

Private, never-exposed state lives on the persistent `/data` volume; the
user-editable entity-map override lives in the `/config` (`addon_config`) folder.

| File                             | Location   | Purpose                                              |
| -------------------------------- | ---------- | ---------------------------------------------------- |
| `agent-creds.json`               | `/data`    | Provisioned uid, MQTT username/password, provision token + claim code (0600). |
| `applied-versions.json`          | `/data`    | Per-device applied desired_version (convergence).    |
| `entity-map.json`                | `/config`  | Optional entity-map override (see above).            |
