# Onboarding a gateway to production (team runbook)

Repeatable, copy-paste runbook for taking a **fresh Home Assistant OS unit** (a
Pi, or a HAOS VM like our `Onboarding` VM) all the way to **claimed + syncing**
against our production cloud. Run it once per gateway.

- **Cloud:** `https://smart.prameg.net`
- **MQTT broker:** `mqtt.prameg.net:8883` (TLS)

This is the concrete production version of the decision + generic runbook in
[`factory-onboarding.md`](factory-onboarding.md). If you're standing up the cloud
or broker for the first time, do that first:

- Broker node (Mosquitto + go-auth, TLS, LE renewal): [`../infra/README.md`](../infra/README.md)
- Broker deploy plan (Forge tasks): `smart/.cursor/plans/mqtt_broker_deploy_8139f537.plan.md`

> **Secrets** (factory key, broker secret, MQTT service-account password) live in
> the cloud's Forge environment and the broker droplet's `infra/.env`. Never paste
> them into this doc, a ticket, or a chat. This runbook refers to them by name.

---

## 0. One-time platform check (not per gateway)

Verify these once and any time the infra changes — they're the shared plumbing
every onboarding depends on.

### Cloud (`smart.prameg.net`, Forge app server)

| What | Check |
| ---- | ----- |
| Env is deployed | `MQTT_HOST=mqtt.prameg.net`, `MQTT_PORT=8883`, `MQTT_TLS_ENABLED=true`, `SMART_HOME_CLOUD_MQTT_*`, `SMART_HOME_BROKER_AUTH_SECRET`, `SMART_HOME_PROVISIONING_FACTORY_KEY` all set (see `smart/config/mqtt-client.php`, `smart/config/smart_home.php`) |
| A deploy ran after any env change | Octane reload alone doesn't reload env — redeploy |
| `mqtt:subscribe` runs as a **Forge daemon** and reloads on deploy | Without it, uplink never reaches the DB |
| **Queue worker** daemon is running | Ingest is queued (`QUEUE_CONNECTION=redis`) |
| Subscriber is healthy | `php artisan mqtt:health-check` → `mqtt:subscribe healthy` |

### Broker (`mqtt.prameg.net` droplet)

> **Standing the droplet up for the first time (or rebuilding it)?** Follow
> [Appendix A — Stand up the MQTT broker droplet](#appendix-a--stand-up-the-mqtt-broker-droplet-mqttpramegnet)
> below, then come back here for the recurring health check.

```bash
cd /home/forge/edge/infra
docker compose ps                       # mosquitto + go-auth up
docker compose logs mosquitto | tail    # clean TLS listener on 8883, no cert errors
ls -l /opt/mosquitto/certs/server.crt   # LE cert synced into the stable mount
```

- `infra/.env` has `CLOUD_HOST=smart.prameg.net`, `CLOUD_PORT=443`, and the
  **same** `SMART_HOME_BROKER_AUTH_SECRET` as the cloud.
- UFW allows **8883** from anywhere; **1883 is NOT exposed** (`sudo ufw status`).

### Platform smoke (optional but recommended)

From any machine that can reach the cloud, prove provisioning + broker-auth end
to end (uses the factory key; delete the test gateway afterwards if you like):

```bash
# Provision a throwaway gateway
curl -sS -X POST https://smart.prameg.net/api/v1/provisioning/gateways \
  -H "Authorization: Bearer <FACTORY_KEY>" \
  -H 'Content-Type: application/json' \
  -d '{"serial":"PREFLIGHT-001"}'
# -> { uid, mqtt:{username,password}, claim_code, ... }

# Its creds should be authorized ONLY for its own namespace
UID=<uid>; U=<mqtt.username>; P=<mqtt.password>
mosquitto_pub -h mqtt.prameg.net -p 8883 -u "$U" -P "$P" \
  -t "homes/$UID/availability" -m online -q 1 -r        # allowed
mosquitto_pub -h mqtt.prameg.net -p 8883 -u "$U" -P "$P" \
  -t "homes/other-uid/availability" -m nope -q 1        # denied by ACL
```

---

## 1. Prepare the unit

1. **Flash stock HAOS** (or boot a fresh HAOS VM). First boot downloads Core and
   can take several minutes. For our VirtualBox `Onboarding` VM, see
   [`haos-vm-reset.md`](haos-vm-reset.md).
2. The unit must be **genuinely fresh** — nobody has clicked through the HA
   onboarding wizard, and it is not a clone/snapshot of an already-onboarded
   unit. Otherwise `smart-onboard` reports *"owner already exists"* and you must
   supply that owner's credentials or re-flash. (Don't mix the browser wizard
   with the CLI on the same device.)
3. **Plug in the Zigbee coordinator** (Sonoff **ZBDongle-E**) before onboarding.
   On a Pi 5 whose only serial device is the dongle it enumerates as
   `/dev/ttyACM0` — the default `smart-onboard` writes into Zigbee2MQTT. Confirm
   from the HA host's shell if unsure: `ls -l /dev/serial/by-id/` (a ZBDongle-E
   shows an `…Sonoff_Zigbee_3.0_USB_Dongle_Plus…-if00` entry). If a unit has extra
   serial hardware, pass the concrete node with `--zigbee-port`.
4. **The unit itself** must reach production (not just your laptop). From the HA
   host's shell (Settings → Terminal, or the VM console):

   ```bash
   curl -sS -o /dev/null -w "%{http_code}\n" https://smart.prameg.net   # expect 200/302
   nc -zv mqtt.prameg.net 8883                                          # expect open
   ```

   > Production always uses the real public hosts. The `10.0.2.2` / local-NAT /
   > `--dev` tricks in [`local-testing.md`](local-testing.md) are for on-machine
   > testing only — do **not** use them here.

---

## 2. Run `smart-onboard` (from a companion machine)

The CLI drives the unit over HA's stable public APIs. It is a **resumable state
machine**: if any step fails, fix the cause and **re-run the exact same command**
— completed steps short-circuit and it continues where it stopped.

### Build it

```bash
cd edge/smart-home-agent
go build -o smart-onboard ./cmd/smart-onboard
```

(Publish per-arch binaries for factory stations later; a local build is fine for
now.)

### Run it (production)

`--host` is auto-discovered over mDNS when omitted; pass it explicitly for a VM
(mDNS usually can't see a NAT'd VM). Provide the factory key by flag, by
`SMART_ONBOARD_FACTORY_KEY`, or let it prompt.

```bash
./smart-onboard \
  --host http://<UNIT-IP>:8123 \
  --cloud-base-url https://smart.prameg.net \
  --factory-key <FACTORY_KEY> \
  --mqtt-host mqtt.prameg.net \
  --mqtt-port 8883 \
  --mqtt-tls \
  --owner-password '<ha-owner-password>' \
  --country SA
```

- Interactive (a terminal attached, no `--yes`): unset values are prompted with
  defaults; a summary is confirmed before anything changes.
- Non-interactive (factory station / CI): add `--yes` and pass every required
  value; secrets can come from `SMART_ONBOARD_FACTORY_KEY` /
  `SMART_ONBOARD_OWNER_PASSWORD`.
- Optional: `--timezone Asia/Riyadh`, `--currency SAR`, `--serial <override>`,
  `--wait-core`, `--wait-provision`.
- Zigbee radio (defaults suit the ZBDongle-E): `--zigbee-port` (default
  `/dev/ttyACM0`; **empty disables** the Zigbee step), `--zigbee-adapter` (default
  `ember`; use `zstack` for a ZBDongle-P), `--zigbee-baudrate` (0 = adapter default).

### Steps it performs

| Step | Action |
| ---- | ------ |
| `connect` | Wait for HA Core to be usably up (bounded by `--wait-core`, default 10m). |
| `owner-and-token` | Create the HA owner (or log in on a re-run), mint a long-lived token, set country/time zone. |
| `addon-repository` | Add `https://github.com/prameg/smart-home-edge` to the add-on store. |
| `install-addons` | Install agent + Mosquitto + Zigbee2MQTT + Matter Server at **latest** (the version-free bootstrap set; no pinning). |
| `configure-zigbee` | Point Zigbee2MQTT at the coordinator (`/dev/ttyACM0`, adapter `ember` by default) and start it, so the unit can pair on day one. Skipped if `--zigbee-port` is empty. |
| `configure-agent` | Set the agent's `cloud_base_url`, `factory_key`, `mqtt_*` options. |
| `start-agent` | Start the agent add-on. |
| `await-provision` | Wait for the agent to provision and read back `uid` + **claim code** (bounded by `--wait-provision`, default 5m). |

**Done screen** prints the `uid`, serial, and the **claim code** (`XXXX-XXXX`).
You can also read the code from the HA **Smart Home** sidebar panel, an HA
persistent notification, or the agent add-on log.

> **The CLI never pins versions.** `smart-onboard` installs the version-free
> bootstrap add-on set (`smart-home-agent/fleet/release.json` — repo + slugs, no
> versions) so the unit can start the agent and enroll. Everything then runs the
> latest: the agent enables HA-native `auto_update` and its `updateAll` engine
> keeps agent/HAOS/Core/add-ons current. See
> [`fleet-update.md`](fleet-update.md).

> **Updates are latest-everywhere, no pinning.** After onboarding the unit keeps
> itself current with no admin action: HA `auto_update` handles add-ons, and the
> agent's daily self-check (and on-boot pass) drives HAOS/Core forward. Pushing an
> update *now* to already-claimed units is a per-row or bulk **Update** button on
> **/gateways** — never a CLI re-run. The agent add-on has `hassio_api: true` +
> `hassio_role: manager` so it can drive `/addons`, `/store`, `/os`, `/core`
> itself: on a `homes/{uid}/update` command (or its own tick) it brings everything
> to latest (self-update last), and reports progress on `homes/{uid}/update/status`
> + `homes/{uid}/versions`. Full flow in [`fleet-update.md`](fleet-update.md).

---

## 3. Claim the gateway in the cloud

An unclaimed gateway is broker-confined to `availability`/`config` only, and the
cloud drops any device uplink (no home to attach to) — this is expected until you
claim.

1. Sign in at `https://smart.prameg.net`.
2. Go to **Homes**.
3. **Claim gateway** → enter the **claim code** (+ optional home name) → submit.
4. Admins can confirm the unit on the **Gateways** fleet page (`/gateways`): it
   should move Unclaimed → Claimed, then show **online** once the agent
   reconnects.

On claim, the cloud pushes the retained `config`; the agent then reports its HA
inventory and the cloud auto-registers a device per allow-listed domain. You do
not hand-create device rows.

---

## 4. Verify the round trip

### 4a. No-hardware path (VM / `Onboarding`)

If the unit has no real hardware (e.g. the `Onboarding` VM), use the built-in
test light:

1. HA **Smart Home** sidebar panel → **Actions** → **Create test light**
   (creates `light.bed_light`).
2. Same panel → **Republish inventory** so the cloud registers it.
3. Cloud → the claimed home → **Devices**: **Bed Light** should appear.
4. Toggle it in HA → the device's `reported_state` updates in the cloud (needs
   `mqtt:subscribe` + the queue worker).
5. Control it from the cloud → the downlink actuates HA.

### 4b. Real coordinator (production units — required before cutting a golden image)

The fake light exercises the cloud round-trip but **not** the Zigbee radio, which
is the first thing a real installer uses. Do this on any unit with a coordinator,
and always on the reference unit before imaging (`configure-zigbee` bakes the
radio into the golden image, so the reference unit must prove it works):

1. **Z2M came up on the coordinator.** Settings → Add-ons → **Zigbee2MQTT** →
   started, and its log shows the adapter connecting (e.g. `ember`/`EmberZNet`
   started, coordinator firmware line) — **not** `Error: Failed to connect` /
   `Error while opening serialport`. If it can't open the port, the dongle isn't
   at the configured `--zigbee-port` (re-check `ls -l /dev/serial/by-id/`).
2. **Pair a real device.** Cloud → the claimed home → **Pair device** (or the HA
   **Smart Home** panel) → put a sensor/bulb into pairing → it joins (Z2M logs
   `Device … successfully joined`).
3. **The paired device reaches the cloud.** It registers under the home's
   **Devices** and its state updates on change (motion, on/off), same
   `reported_state` path as the fake light.
4. **Control round-trips** for a controllable device (bulb/switch): actuate from
   the cloud → the physical device responds.

Only once 4b passes end-to-end on the reference unit is it safe to
[cut the golden image](golden-image.md).

Health checks:

```bash
php artisan mqtt:health-check      # on the cloud app server
```

The gateway should read `is_online = true` on `/gateways` once it publishes its
retained `availability`.

---

## 5. Troubleshooting

| Symptom | Likely cause | Fix |
| ------- | ------------ | --- |
| `await-provision` times out; add-on log shows "network unreachable" | The **unit** can't reach the cloud/broker | Fix the unit's outbound network/DNS/firewall; re-run the same `smart-onboard` command (it restarts the agent with corrected config). |
| `401` on `owner-and-token`/`addon-repository` right after boot | Core's HTTP socket is up but auth/onboarding isn't yet | Wait until the HA onboarding screen actually loads in a browser, then re-run. |
| `401` on store/add-on steps that persists across re-runs | Old `smart-onboard` binary (HA locked the `/api/hassio` REST proxy) | Rebuild from `cmd/smart-onboard`; current builds use the `supervisor/api` WS command. |
| Provisioning returns `401` | Factory key mismatch | `--factory-key` must equal the cloud's `SMART_HOME_PROVISIONING_FACTORY_KEY`. |
| `configure-zigbee` fails, or Z2M log shows `Failed to connect` / `Error while opening serialport` | Coordinator not at the configured port (unplugged, or a non-`/dev/ttyACM0` node) | Confirm the dongle: `ls -l /dev/serial/by-id/`; re-run with `--zigbee-port <node>`. On a `dd` golden image keep it serial-agnostic (`/dev/ttyACM0`), not a per-unit `by-id` path. |
| Z2M started but `Pair device` never joins anything | Wrong adapter driver, or coordinator firmware | ZBDongle-E uses `--zigbee-adapter ember`; a ZBDongle-P uses `zstack`. Check the Z2M log's adapter line matches the physical dongle. |
| Agent connects but every publish is ACL-denied | Broker secret mismatch, or go-auth can't reach the cloud | Align `SMART_HOME_BROKER_AUTH_SECRET` on the cloud and `infra/.env`; check `docker compose logs` on the broker. |
| "owner already exists" on a supposedly fresh unit | Image isn't fresh (cloned snapshot, or someone used the browser wizard) | Supply that owner's credentials, or re-flash stock HAOS ([`haos-vm-reset.md`](haos-vm-reset.md) for the VirtualBox VM). |
| Claim code invalid/expired | TTL elapsed (default 24h, `SMART_HOME_CLAIM_CODE_TTL_HOURS`) or typo | Reissue from the Smart Home panel → **Reissue claim code**. |
| Devices empty after claim | No allow-listed entities (`light`, `switch`, …) | **Create test light** + **Republish inventory**, or add a real entity. |
| Gateway stays offline in the cloud | `mqtt:subscribe`/queue down, or broker unreachable | Check the Forge daemons and the broker `docker compose logs`. |

More onboarding-specific troubleshooting: [`factory-onboarding.md`](factory-onboarding.md#troubleshooting).

---

## Quick reference

**Per-gateway commands:**

```bash
# 1. On the unit: confirm it can reach production
curl -sS -o /dev/null -w "%{http_code}\n" https://smart.prameg.net
nc -zv mqtt.prameg.net 8883

# 2. From a companion machine: onboard
cd edge/smart-home-agent && go build -o smart-onboard ./cmd/smart-onboard
./smart-onboard \
  --host http://<UNIT-IP>:8123 \
  --cloud-base-url https://smart.prameg.net \
  --factory-key <FACTORY_KEY> \
  --mqtt-host mqtt.prameg.net --mqtt-port 8883 --mqtt-tls \
  --owner-password '<ha-owner-password>' --country SA

# 3. Claim at https://smart.prameg.net → Homes → Claim gateway
# 4. In HA: Smart Home panel → Create test light → Republish inventory
```

**Production env this runbook assumes** (names, not values):

| Side | Keys |
| ---- | ---- |
| Cloud (`smart.prameg.net`) | `MQTT_HOST`, `MQTT_PORT`, `MQTT_TLS_ENABLED`, `MQTT_CLIENT_ID`, `MQTT_CLEAN_SESSION`, `SMART_HOME_CLOUD_MQTT_USERNAME`, `SMART_HOME_CLOUD_MQTT_PASSWORD`, `SMART_HOME_BROKER_AUTH_SECRET`, `SMART_HOME_PROVISIONING_FACTORY_KEY` |
| Broker (`infra/.env`) | `CLOUD_HOST`, `CLOUD_PORT`, `SMART_HOME_BROKER_AUTH_SECRET` (must equal the cloud's) |
| CLI (`smart-onboard`) | `--cloud-base-url`, `--factory-key` (`SMART_ONBOARD_FACTORY_KEY`), `--mqtt-host`, `--mqtt-port`, `--mqtt-tls`, `--owner-password` (`SMART_ONBOARD_OWNER_PASSWORD`) |

---

# Appendix A — Stand up the MQTT broker droplet (`mqtt.prameg.net`)

One-time infra build (repeat only when rebuilding/replacing the broker host).
The broker is **Mosquitto + mosquitto-go-auth** running as the Docker Compose in
[`../infra`](../infra), on a **dedicated DigitalOcean droplet provisioned via
Laravel Forge**, with **server-side TLS only** (per-gateway username/password +
ACL; client-cert mTLS is deferred). This is the concrete version of the deploy
plan (`smart/.cursor/plans/mqtt_broker_deploy_8139f537.plan.md`); see also
[`../infra/README.md`](../infra/README.md).

**Why this shape:** Forge gives one console next to the cloud app, a UFW firewall
UI, and managed Let's Encrypt issuance + auto-renewal. The broker is not a PHP
app — Forge only provisions/secures the host and owns the cert; the broker itself
runs under Docker Compose. The one bespoke piece is a small script (Section C)
that syncs each renewed LE cert into the broker's mount and SIGHUPs Mosquitto.

## A. Provision the droplet + install Docker

1. Forge → **Create Server** on DigitalOcean — a **new server**, not a site on
   the app server. ~1–2 GB RAM is plenty (idle MQTT sessions are cheap); pick the
   **same region** as the cloud app so go-auth's per-connect callbacks to
   `smart.prameg.net` are low-latency.
2. Forge droplets don't ship Docker. SSH in as `forge` and install Docker Engine
   + the Compose plugin:

   ```bash
   curl -fsSL https://get.docker.com | sh
   sudo usermod -aG docker forge      # run docker without sudo
   ```

3. Log out and back in (so the `docker` group applies), then verify:

   ```bash
   docker version
   docker compose version             # Compose v2 = the `docker compose` subcommand
   ```

4. Get the repo onto the host (deploy key or clone) so `infra/` is available at
   **`/home/forge/edge/infra`** (this path is referenced by the renewal script in
   Section C — adjust it there if you clone elsewhere):

   ```bash
   git clone https://github.com/prameg/smart-home-edge.git /home/forge/edge
   ```

## B. DNS + Let's Encrypt site

1. Add a DNS **A record**: `mqtt.prameg.net` → the droplet's public IP. Wait for
   it to resolve: `dig +short mqtt.prameg.net`.
2. In Forge, add a **Site** for `mqtt.prameg.net` on this server (a plain site —
   it exists only to own the cert + serve the HTTP-01 challenge; it never collides
   with Mosquitto's 8883).
3. Forge → the site → **SSL** → **Let's Encrypt** → obtain certificate. Forge
   issues it and **auto-renews via its own cron** (~every 60 days), rewriting the
   nginx vhost to the new cert path each renewal.
4. Confirm the live cert paths from the vhost (authoritative):

   ```bash
   grep -E 'ssl_certificate(_key)? ' /etc/nginx/sites-available/mqtt.prameg.net
   # -> ssl_certificate     /etc/nginx/ssl/mqtt.prameg.net/<id>/server.crt;
   # -> ssl_certificate_key /etc/nginx/ssl/mqtt.prameg.net/<id>/server.key;
   ```

   The `<id>` **changes on every renewal** — which is exactly why the broker must
   not bind that path directly (Section C solves this).

## C. Hook LE renewal → Mosquitto reload (the important one)

Mosquitto reloads its TLS cert on **SIGHUP** (no restart, no dropped sessions).
Forge writes each renewed cert into a **new** `/etc/nginx/ssl/<domain>/<id>/` dir
and only reloads nginx — it has no per-cert deploy hook. So a tiny scheduled
script (a) resolves the current cert from the nginx vhost, (b) copies it into the
stable dir the container binds, and (c) SIGHUPs Mosquitto only when it changed.

1. Create the stable dir the compose mount uses:

   ```bash
   sudo mkdir -p /opt/mosquitto/certs
   ```

2. Create `/opt/mosquitto/sync-certs.sh`:

   ```bash
   #!/usr/bin/env bash
   set -euo pipefail

   DOMAIN=mqtt.prameg.net
   VHOST=/etc/nginx/sites-available/$DOMAIN
   DEST=/opt/mosquitto/certs
   INFRA=/home/forge/edge/infra          # dir containing docker-compose.yml

   # Resolve the CURRENT cert/key straight from the nginx vhost, so we always
   # follow Forge's latest numbered dir after a renewal.
   CRT=$(awk '/ssl_certificate /   {gsub(";","",$2); print $2; exit}' "$VHOST")
   KEY=$(awk '/ssl_certificate_key/ {gsub(";","",$2); print $2; exit}' "$VHOST")

   install -m 644 "$CRT" "$DEST/server.crt.new"
   install -m 640 "$KEY" "$DEST/server.key.new"

   # Only reload when the served cert actually changed.
   if ! cmp -s "$DEST/server.crt.new" "$DEST/server.crt"; then
     mv -f "$DEST/server.crt.new" "$DEST/server.crt"
     mv -f "$DEST/server.key.new" "$DEST/server.key"
     ( cd "$INFRA" && docker compose kill -s HUP mosquitto ) || \
       ( cd "$INFRA" && docker compose up -d mosquitto )   # first run / container down
   else
     rm -f "$DEST/server.crt.new" "$DEST/server.key.new"
   fi
   ```

   ```bash
   sudo chmod +x /opt/mosquitto/sync-certs.sh
   ```

3. Schedule it as **root** (the key + `/etc/nginx/ssl` are root-owned). In Forge →
   the server → **Scheduler**, add a **daily** job with user `root` running
   `/opt/mosquitto/sync-certs.sh`. (Or root's crontab:
   `sudo crontab -e` → `17 3 * * * /opt/mosquitto/sync-certs.sh`.)

4. Run it once by hand now to seed the dir **before** the first `up`:

   ```bash
   sudo /opt/mosquitto/sync-certs.sh
   ls -l /opt/mosquitto/certs        # server.crt + server.key present
   ```

## D. Environment

On the **droplet**, in `infra/`:

```bash
cd /home/forge/edge/infra
cp .env.example .env
```

Set (see [`../infra/.env.example`](../infra/.env.example)):

```
CLOUD_HOST=smart.prameg.net
CLOUD_PORT=443
SMART_HOME_BROKER_AUTH_SECRET=<shared secret — MUST equal the cloud's>
```

On the **cloud app** (Forge → app site → Environment), confirm the matching side
(see [`smart/config/mqtt-client.php`](../../smart/config/mqtt-client.php)):

```
MQTT_HOST=mqtt.prameg.net
MQTT_PORT=8883
MQTT_TLS_ENABLED=true
MQTT_TLS_VERIFY_PEER=true
SMART_HOME_CLOUD_MQTT_USERNAME=<service account>
SMART_HOME_CLOUD_MQTT_PASSWORD=<service account>
SMART_HOME_BROKER_AUTH_SECRET=<same shared secret as the droplet>
```

## E. Firewall (Forge UFW)

- Allow **8883** from anywhere (field gateways + the cloud daemon).
- Allow **80/443** (Forge adds these; needed for the LE HTTP-01 challenge + renewal).
- Do **not** expose **1883** — leave it off the allow-list. The compose still
  publishes 1883, so UFW is what keeps it private; confirm with `sudo ufw status`.

## F. Bring up + verify

```bash
cd /home/forge/edge/infra
docker compose up -d               # restart: unless-stopped survives reboots
docker compose logs -f mosquitto   # expect a clean TLS listener on 8883, no cert errors
```

Then run the platform smoke in [§0 → Platform smoke](#platform-smoke-optional-but-recommended):
provision a throwaway gateway, confirm an **allowed** publish to its own
namespace succeeds and a **denied** cross-namespace publish is rejected by the
ACL, and confirm the cloud's `php artisan mqtt:subscribe` connects to
`mqtt.prameg.net:8883` and `mqtt:health-check` reports a fresh heartbeat.

## G. Deferred — client-cert mTLS

Skipped on purpose: server-TLS + per-gateway password + ACL is sufficient, and a
**shared** fleet cert is a fleet-wide secret on physical devices (marginal gain,
painful rotation). If ever added, prefer **per-gateway** mTLS — see the "deferred"
notes in the deploy plan and [`../infra/mosquitto/certs/README.md`](../infra/mosquitto/certs/README.md).
