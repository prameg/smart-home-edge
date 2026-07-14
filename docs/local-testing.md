# Local testing with the Home Assistant VM

You can exercise the whole agent against a **real** Home Assistant (your VM at
`http://homeassistant.local:8123`) on your machine — no Pi, no external broker,
no TLS. To wipe the VirtualBox `Onboarding` VM back to stock HAOS, see
[`haos-vm-reset.md`](haos-vm-reset.md). Two tracks:

- **Track 1 — agent + HA + local broker, NO cloud.** Fastest. Proves the bridge
  end-to-end (HA events → MQTT uplink, MQTT downlink → HA service calls) using
  `mosquitto_sub`/`mosquitto_pub` as a stand-in for the cloud. Start here.
- **Track 2 — full stack with a local Laravel cloud.** Adds real provisioning,
  claim, device rows, and the `mqtt:subscribe` ingest daemon.

Both use the agent's env-var fallback (it runs fine outside HAOS).

---

## Prerequisites (both tracks)

1. **Local broker** — from [`../infra/local`](../infra/local):
   ```bash
   brew install mosquitto
   mosquitto -c infra/local/mosquitto.local.conf -v
   ```
2. **HA Long-Lived Access Token** — in the VM: avatar/Profile → **Security** →
   **Long-lived access tokens** → *Create Token*. Copy it.
3. **A controllable `light.*` entity in HA** — the cloud inventory only registers
   allow-listed domains (`light`, `switch`, `climate`, …). A plain **Toggle**
   helper is `input_boolean.*` and will **not** appear as a cloud device. For VM
   testing, create the standard test light pair (see
   [Create a test light](#create-a-test-light-track-23) below) or use any real
   `light.*` / `switch.*` entity you already have (e.g. from the *Demo*
   integration).
4. **Agent env** — in `smart-home-agent/`:
   ```bash
   cp .env.local.example .env.local   # then edit it
   ```
   Set `SUPERVISOR_TOKEN` to the token from step 2; leave `HA_*`, `MQTT_*` as the
   defaults (they already point at the VM + local broker).

---

## Create a test light (Track 2/3)

Use this when your HA VM has no real hardware and you want a controllable device
in the cloud app after claim + inventory sync.

### Why two entities?

| Entity | Domain | Cloud sees it? | Role |
| ------ | ------ | -------------- | ---- |
| `input_boolean.bed_lights` | `input_boolean` | No | Stores the on/off state |
| `light.bed_light` | `light` | Yes | Template light the agent reports and the cloud controls |

The agent reads HA's **entity registry** and the cloud upserts a `Device` only
for allow-listed domains (see [`../docs/mqtt-contract.md`](../docs/mqtt-contract.md)
— `light`, `switch`, …). Wrapping a toggle in a **template light** gives you a
`light.*` entity without physical hardware.

### Option A — Smart Home panel button (recommended)

The agent creates the test light itself over the Supervisor-proxied HA APIs, so
there is no separate script or token to wire up. In the agent's web UI (**Smart
Home** sidebar panel on Track 3, or `http://localhost:8099` for the local agent
on Track 2) → **Actions** → **Create test light**.

This creates `input_boolean.bed_lights` and the `light.bed_light` template light
(idempotent — running it again is a no-op). Then click **Republish inventory**
in the same panel so the cloud registers the device.

Refresh the cloud home **Devices** page — **Bed Light** should appear with on/off
controls. The internal toggle (`input_boolean.bed_lights`) stays in HA only; hide
it from dashboards if you like.

### Option B — Home Assistant UI

1. **Toggle (backing state)** — Settings → **Devices & services** → **Helpers**
   → **Create helper** → **Toggle** → name it `Bed Lights` → **Create**.
   Entity id: `input_boolean.bed_lights`.
2. **Template light (cloud-visible)** — **Create helper** → **Template** →
   **Template light**:
   - **Name:** `Bed Light`
   - **State template:** `{{ is_state('input_boolean.bed_lights', 'on') }}`
   - **Turn on:** service `input_boolean.turn_on`, entity `input_boolean.bed_lights`
   - **Turn off:** service `input_boolean.turn_off`, entity `input_boolean.bed_lights`
3. **Area** — Settings → **Entities** → `light.bed_light` → set **Area** to
   **Bedroom** (seeds the cloud room on first inventory sync).
4. **Republish inventory** — as in Option A.

### Option C — YAML (File editor)

Add to `configuration.yaml` (or a package under `packages/`):

```yaml
input_boolean:
  bed_lights:
    name: Bed Lights
    icon: mdi:flashlight

template:
  - light:
      - unique_id: bed_light_cloud
        name: Bed Light
        icon: mdi:flashlight
        state: "{{ is_state('input_boolean.bed_lights', 'on') }}"
        turn_on:
          action: input_boolean.turn_on
          target:
            entity_id: input_boolean.bed_lights
        turn_off:
          action: input_boolean.turn_off
          target:
            entity_id: input_boolean.bed_lights
```

Reload **Template entities** (Developer Tools → YAML) or restart HA, assign
**Bedroom** on `light.bed_light`, then republish inventory.

### Verify

```bash
# Both entities exist and stay in sync
curl -s -H "Authorization: Bearer $SUPERVISOR_TOKEN" \
  http://127.0.0.1:8123/api/states/light.bed_light | jq .state
curl -s -H "Authorization: Bearer $SUPERVISOR_TOKEN" \
  http://127.0.0.1:8123/api/states/input_boolean.bed_lights | jq .state
```

Toggle **Bed Light** in the HA overview — both states should match. After
inventory sync, the cloud app controls the same entity via `light.bed_light`.

---

## Track 1 — agent + HA + local broker (no cloud)

Skip provisioning by pre-seeding fake credentials (the anonymous local broker
ignores them) and map the cloud device_uid to your real entity.

```bash
cd smart-home-agent
mkdir -p .data

# 1) Pre-seed credentials so the agent skips the cloud provisioning call.
cat > .data/agent-creds.json <<'JSON'
{
  "uid": "demo",
  "topic_namespace": "homes/demo",
  "claim_status": "unclaimed",
  "mqtt_username": "demo",
  "mqtt_password": "demo",
  "serial": "DEV-LOCAL"
}
JSON

# 2) Map a device_uid to a REAL entity in your VM (edit the entity_id).
cat > .data/entity-map.json <<'JSON'
[
  { "device_uid": "spike-light-1", "entity_id": "light.bed_light" }
]
JSON

# 3) Run the agent (loads .env.local).
./scripts/run-local.sh
```

You should see logs: `connected to broker`, `subscribed to HA state_changed
events`, and `provisioned uid=demo`.

### Watch the uplink

In another terminal:

```bash
mosquitto_sub -h localhost -p 1883 -t 'homes/demo/#' -v
```

- On start you'll see a retained `homes/demo/availability online` and an
  `homes/demo/event/agent.boot` message.
- Toggle `light.bed_light` in the HA UI → a `homes/demo/state/spike-light-1`
  message appears with the entity's state + attributes.

### Test a command (downlink → HA actuates)

```bash
mosquitto_pub -h localhost -p 1883 -t 'homes/demo/cmd' -m "{
  \"cmd_id\":\"$(uuidgen)\",
  \"device_uid\":\"spike-light-1\",
  \"action\":\"light.turn_on\",
  \"params\":{\"brightness\":200},
  \"ts\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\",
  \"ttl_sec\":60,
  \"source\":\"user\"
}"
```

- The light turns on in HA.
- The subscriber shows `homes/demo/cmd/ack {"cmd_id":"…","status":"acked"}`.
- Try an old `ts` (e.g. yesterday) with a small `ttl_sec` → the agent logs
  "cmd expired before apply, dropping" and does not actuate.

### Test the desired shadow (retained convergence)

The desired `state` is **HA-native** — the same shape reported state uses
(`{ state, attributes }`), so desired and reported are directly comparable:

```bash
mosquitto_pub -h localhost -p 1883 -r -t 'homes/demo/shadow/desired/spike-light-1' -m '{
  "device_uid":"spike-light-1",
  "desired_version":1,
  "state":{"state":"on","attributes":{"brightness":128}}
}'
```

The desired shadow is **per device** (`shadow/desired/{device_uid}`, retained);
the agent subscribes `homes/demo/shadow/desired/#`.

- The agent applies it in HA (`state != "off"` → `turn_on`, passing through
  recognized attributes), then reads HA back and publishes a
  `homes/demo/state/spike-light-1` carrying the entity's **actual** state +
  `"version":1` (this is how the cloud declares convergence — never an echo of
  the desired doc).
- Because it's **retained**, stop the agent and restart it: on reconnect it gets
  the retained desired again AND re-reports every mapped device's current state
  (the reconnect reconcile) — the reconnect-reconciliation behavior, locally.

---

## Track 2 — full stack with a local Laravel cloud

Adds real provisioning + ingest. Do this after Track 1 works.

1. **Cloud env** (`smart/.env`): set `SMART_HOME_PROVISIONING_FACTORY_KEY` and
   `SMART_HOME_BROKER_AUTH_SECRET` to any dev values, and point the daemon at the
   local broker:
   ```env
   MQTT_HOST=localhost
   MQTT_PORT=1883
   MQTT_CLEAN_SESSION=false
   MQTT_CLIENT_ID=smart-home-subscriber
   ```
2. **Run the cloud** with the queue + ingest daemon:
   ```bash
   composer dev            # app + queue + vite
   php artisan mqtt:subscribe   # in its own terminal — connects to local broker
   php artisan reverb:start     # websockets — live UI updates (device pairing
                                # wizard etc.); the UI falls back to polling
                                # without it, so optional but recommended
   ```
   (The anonymous local broker means the cloud's `mqtt:subscribe` connects
   without go-auth; that's fine for local. go-auth is only exercised on the
   real broker node in `../infra`.)
3. **Agent env** (`.env.local`): remove the pre-seeded creds AND any dev
   `entity-map.json` (with the cloud, the map is cloud-authoritative — see below),
   then fill the cloud vars:
   ```bash
   rm -f .data/agent-creds.json .data/entity-map.json
   ```
   ```env
   CLOUD_BASE_URL=http://localhost
   FACTORY_KEY=<the SMART_HOME_PROVISIONING_FACTORY_KEY>
   GATEWAY_SERIAL=DEV-0001
   ```
4. **Run the agent** → it calls `POST /api/v1/provisioning/gateways`, logs its
   real `uid` and a short **claim code** (`XXXX-XXXX`, also posted as an HA
   persistent notification), and stores its per-gateway provision token in
   `.data/agent-creds.json`. Watch `homes/<uid>/#` on the broker.
5. **Claim** — bind the gateway to a home with that claim code (via the claim
   API, or quickly in `php artisan tinker`). On claim the cloud pushes the
   retained `config`; the
   agent then reports its HA `inventory` and the cloud **auto-registers a device
   per allow-listed domain** and pushes back the authoritative
   `device_uid ↔ entity_id` map. You do not create device rows by hand.
6. **Round-trip for real** — with the auto-created devices, toggling an entity in
   HA updates that device's `reported_state` in the DB, and `SendDeviceCommand` /
   `DeviceControl::setDesiredState` (HA-native `{ state, attributes }`) round-trip
   and converge by version.

> **Recovery check (provision token):** the provision token in
> `.data/agent-creds.json` is the recovery credential. To exercise recovery,
> keep it and force a re-provision — e.g. rotate the password cloud-side (or edit
> `mqtt_password` in the creds file) and restart: the agent detects the broker
> rejection, re-provisions with its token → **same `uid`**, rotated password,
> claim/home binding preserved. No factory key involved.
>
> **Factory-reset check (token lost):** `rm -rf .data` wipes the provision token
> too, so a bare restart is now **refused** (`recovery_not_authorized`) — the
> creation-only factory key can no longer silently re-adopt a live serial. First
> open a re-enrollment window for that serial on the cloud **Gateways** (admin)
> page, then restart: the factory key re-adopts the known serial once and mints a
> fresh token, again keeping the same `uid` and binding.

---

## Track 3 — install the add-on inside the VM (closest to production)

The VM is HAOS, so it can run the add-on itself. This validates the **packaging**
(the add-on builds in the Supervisor, gets a real Supervisor token, reads its
`config.yaml` options, and persists `/data` across restarts). It's heavier than
Track 1/2 — use it to prove packaging, not for fast iteration.

Until this repo is published at a git URL you can add as an add-on *repository*,
use the **local add-ons** path.

### Semi-automated: `scripts/sync-addon-to-vm.sh serve` + `smart-onboard --dev`

`scripts/sync-addon-to-vm.sh serve` installs the add-on (serves the source so you
drop it into the VM and install it via the HA UI), and `smart-onboard --dev`
drives the rest of onboarding around it. `--dev` **skips** the store agent add-on
and only checks `local_smart_home_agent` is present, defaults `cloud_base_url` to
`http://10.0.2.2:8090` and `mqtt_host` to `10.0.2.2` (VirtualBox NAT), and
defaults the broker to plaintext — so no addressing flags are needed:

```bash
# Terminal 1 — expose the dev stack to the VM (serves source + Laravel)
./scripts/sync-addon-to-vm.sh serve                # tarball on :8765 + Laravel on :8090
mosquitto -c infra/local/mosquitto.local.conf -v   # broker on 0.0.0.0:1883

# Then in the HA terminal, drop the source in and install it via the UI
# (Settings → Add-ons → Add-on store → ⋮ → Check for updates → Local add-ons →
# Smart Home Agent → Install) — see step 1/2 below.

# Terminal 2 — from smart-home-agent/, once local_smart_home_agent is installed
./smart-onboard --dev --factory-key <SMART_HOME_PROVISIONING_FACTORY_KEY>
```

`smart-onboard --dev` is resumable: if you run it before the local add-on is
installed, `install-addons` fails with a reminder to run
`scripts/sync-addon-to-vm.sh serve` and install it — do that, then re-run and it
picks up at `configure-agent` / `start-agent` / `await-provision`.

Iterate on agent code by re-running `scripts/sync-addon-to-vm.sh` (rebuilds the
tarball + prints the re-drop `curl` + the `ha addons rebuild` / `restart`
commands). The manual steps below are the same flow written out.

### 1. Get the add-on folder into the VM

Build the tarball and expose the dev stack to the VM (VirtualBox host =
`10.0.2.2` from inside the VM). In one terminal start the local broker, in
another run:

```bash
# Terminal 1 — from edge repo root
mosquitto -c infra/local/mosquitto.local.conf -v

# Terminal 2 — from smart-home-agent/
./scripts/sync-addon-to-vm.sh serve
```

`serve` does three things:

1. Serves `dist/smart-home-agent.tgz` on `0.0.0.0:8765` (for the curl below).
2. Starts Laravel on `0.0.0.0:8090` (`php artisan serve`) so the add-on can
   reach provisioning at `http://10.0.2.2:8090`. Herd/Valet on `127.0.0.1:80`
   is **not** reachable from the VM — use this port instead.
3. Prints the add-on `cloud_base_url` / `mqtt_host` values to paste into the
   Configuration tab.

> Use `10.0.2.2`, not your Mac's LAN IP, when the HA VM uses VirtualBox NAT
> (the same address that works for the curl one-liner). Only use your LAN IP
> if the VM is bridged and can ping it.

In **Settings → Terminal**:

```bash
cd ~/addons && rm -rf smart-home-agent
curl -fL http://10.0.2.2:8765/smart-home-agent.tgz | tar xz
```

(`./scripts/sync-addon-to-vm.sh` without `serve` only writes
`dist/smart-home-agent.tgz` and prints the same curl one-liner — use `serve`
when you need the HTTP server.)

You should end up with `/addons/smart-home-agent/` containing `config.yaml`,
`Dockerfile`, the Go sources, etc. The add-on gets its own `/data` volume and
its config from the options tab — do not copy local dev state.

### 2. Install + configure

Settings → Add-ons → **Add-on store** → ⋮ → **Check for updates**, then find
*Smart Home Agent* under **Local add-ons** → **Install**. The Supervisor builds
the Dockerfile inside the VM (first build pulls the Go + HA base images, so give
it a few minutes).

Open the **Configuration** tab and set the `config.yaml` options. Because the
external TLS broker node isn't stood up yet, point it at a broker you control.
Two realistic choices:

- **Against the local Mac broker + local cloud (Track 2 wiring, real add-on):**
  run `./scripts/sync-addon-to-vm.sh serve` (starts Laravel on `0.0.0.0:8090`
  and prints the URLs). Set `cloud_base_url` to `http://10.0.2.2:8090` and
  `mqtt_host` to `10.0.2.2` (VirtualBox NAT). Start mosquitto separately with
  `infra/local/mosquitto.local.conf`. Set `mqtt_port: 1883`, `mqtt_tls: false`,
  and `factory_key` to your dev `SMART_HOME_PROVISIONING_FACTORY_KEY`.
- **Packaging-only smoke test:** point `mqtt_host` at any reachable broker; the
  goal is just to confirm the add-on builds, starts, provisions, and connects.

> `mqtt_tls: false` now actually takes effect from the options tab (previously the
> option was ignored). Keep `mqtt_tls: true` once the real TLS broker exists.

No long-lived token is needed — `homeassistant_api: true` means the Supervisor
injects `SUPERVISOR_TOKEN` and the agent reaches Core at `http://supervisor/core/api`.

> **Sidebar panel (Ingress).** The add-on ships a "Smart Home" sidebar panel
> (agent status + actions, see [`../smart-home-agent/DOCS.md`](../smart-home-agent/DOCS.md)).
> The panel does **not** appear on its own:
>
> 1. **Rebuild** the add-on (`ha addons rebuild local_smart_home_agent`) — for a
>    local add-on this re-reads `config.yaml` and rebuilds the image.
> 2. **If the add-on Info page shows no "Open Web UI" button (and no "Show in
>    sidebar" toggle), ingress is not registered yet.** When an add-on gains
>    ingress *after* it was already installed, Supervisor's `rebuild`/`update`
>    do **not** rebuild the ingress token map — only a fresh install or a
>    Supervisor restart does (Supervisor
>    [#6556](https://github.com/home-assistant/supervisor/pull/6556)). Fix it
>    without losing `/data` by restarting the Supervisor:
>    ```bash
>    ha supervisor restart
>    ```
>    If the button still doesn't appear, **uninstall + reinstall** the add-on
>    (that runs the `install()` path that registers ingress; it wipes `/data`,
>    so the agent re-provisions — an already-claimed unit may need re-confirming
>    cloud-side).
> 3. On the add-on **Info** tab, click **Open Web UI** to confirm ingress
>    proxies to the agent, then turn on the **Show in sidebar** toggle — it
>    defaults **off**.
> 4. **Hard-refresh** the browser (Cmd/Ctrl+Shift+R); the sidebar is cached
>    client-side, so a new panel won't show until you reload the frontend.

### 3. Entity map

**If you claimed the gateway to the local cloud (recommended):** do nothing —
the cloud auto-registers devices from the reported HA inventory and pushes the
authoritative map, which overrides any local file. Skip to step 4.

**Packaging-only / no-cloud:** the built-in map points at `light.spike_test` /
`switch.spike_test`, which won't exist in your VM, so map a real entity — with
the **Samba**/**File editor** add-on, create
`/addon_configs/local_smart_home_agent/entity-map.json`:

```json
[
  { "device_uid": "spike-light-1", "entity_id": "light.bed_light" }
]
```

That folder is the add-on's `addon_config` mapping (mounted at `/config` inside
the container). **Restart the add-on** after editing — it's read once at startup.

### 4. Watch it

Open the add-on **Log** tab: you should see `smart-home-agent starting`,
`provisioned uid=…`, and `connected to broker`. From there the uplink/command/
shadow round-trips behave exactly as in Track 1/2, now driven by the real
Supervisor-managed add-on.
