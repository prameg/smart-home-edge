# Broker node

Mosquitto + mosquitto-go-auth on a small external host. This is the MQTT broker
the field gateways connect to and that the cloud's `mqtt:subscribe` daemon reads
from.

> For **local development / testing on your machine** (no external host, no
> TLS), use [`local/`](local/) instead — it's a one-command plain Mosquitto.

> **Why not Laravel Cloud?** The broker is a long-running TCP/TLS service, not an
> HTTP app. Laravel Cloud only exposes the HTTP broker-auth callbacks
> (`/api/v1/broker/*`); the broker itself runs here (Hetzner/Fly.io/etc).

## Architecture

```
 gateway agent ──TLS:8883──▶ mosquitto ──TLS──▶ Laravel cloud
                              (go-auth)          POST /api/v1/broker/<SECRET>/{auth,superuser,acl}
 cloud mqtt:subscribe ──1883/8883──▶ mosquitto
```

`go-auth` calls the cloud on every connect/subscribe/publish (with the plugin's
authn + ACL cache in front). The broker shared secret is carried in the **URL
path** — go-auth's HTTP backend has no way to send a custom header, only to set
the request URI, so `/api/v1/broker/<SECRET>/…` is how the cloud authenticates
the broker. (This replaces an earlier proxy-sidecar workaround.)

The `8883` listener is **server-only TLS**: gateways verify the broker's server
cert with normal system trust and prove their own identity with per-gateway
username/password + ACL, not a client cert. Client-cert mTLS is **deferred**
(see `mosquitto/certs/README.md`).

## Setup

```bash
cp .env.example .env          # set CLOUD_HOST, CLOUD_PORT, SMART_HOME_BROKER_AUTH_SECRET
# Seed the server cert into the stable mount (see "TLS cert" below), then:
docker compose up -d
docker compose logs -f mosquitto
```

The cloud must have, in its environment, the SAME
`SMART_HOME_BROKER_AUTH_SECRET`, plus `SMART_HOME_PROVISIONING_FACTORY_KEY` for
the agent's provisioning call.

## TLS cert (Let's Encrypt) + renewal hook

The compose binds the **stable host dir `/opt/mosquitto/certs`** (not Forge's
numbered `/etc/nginx/ssl/<domain>/<id>` path, which changes on every renewal and
would move the mount out from under the container). Let's Encrypt issues and
auto-renews the cert (via Forge); a small host hook keeps the stable dir in sync
and reloads Mosquitto on change.

1. Create the stable dir and seed it before the first `up`:

```bash
sudo mkdir -p /opt/mosquitto/certs
```

2. Install `/opt/mosquitto/sync-certs.sh` (resolves the current cert straight
   from the nginx vhost, so it follows each renewal; reloads only on change):

```bash
#!/usr/bin/env bash
set -euo pipefail

DOMAIN=broker.example.com
VHOST=/etc/nginx/sites-available/$DOMAIN
DEST=/opt/mosquitto/certs
INFRA=/home/forge/edge/infra          # dir containing docker-compose.yml

CRT=$(awk '/ssl_certificate /   {gsub(";","",$2); print $2; exit}' "$VHOST")
KEY=$(awk '/ssl_certificate_key/ {gsub(";","",$2); print $2; exit}' "$VHOST")

install -m 644 "$CRT" "$DEST/server.crt.new"
install -m 640 "$KEY" "$DEST/server.key.new"

if ! cmp -s "$DEST/server.crt.new" "$DEST/server.crt"; then
  mv -f "$DEST/server.crt.new" "$DEST/server.crt"
  mv -f "$DEST/server.key.new" "$DEST/server.key"
  ( cd "$INFRA" && docker compose kill -s HUP mosquitto ) || \
    ( cd "$INFRA" && docker compose up -d mosquitto )
else
  rm -f "$DEST/server.crt.new" "$DEST/server.key.new"
fi
```

3. Mosquitto reloads TLS certs on **SIGHUP** (no restart, no dropped sessions).
   The cert/key under `/etc/nginx/ssl` are root-owned, so schedule the script
   **as root** (Forge Scheduler with user `root`, or root's crontab) daily, and
   run it once by hand to seed the dir before the first `docker compose up -d`.

For a spike without Let's Encrypt, drop a self-signed `server.crt`/`server.key`
into `mosquitto/certs/` and point the compose mount at that dir instead (see
`mosquitto/certs/README.md`).

## Smoke tests

### 1. Broker-auth callbacks reach the cloud

Provision a test gateway (from anywhere that can reach the cloud):

```bash
curl -sS -X POST https://$CLOUD_HOST/api/v1/provisioning/gateways \
  -H "Authorization: Bearer $SMART_HOME_PROVISIONING_FACTORY_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"serial":"SMOKE-0001"}'
# -> { uid, topic_namespace, claim_status, mqtt:{username,password},
#      provision_token, claim_code, claim_code_expires_at }
```

Then connect with those MQTT creds and confirm the broker authorizes only the
gateway's own namespace:

```bash
UID=<uid from above>; USER=<mqtt.username>; PASS=<mqtt.password>

# Allowed: publish retained online availability to its own topic
mosquitto_pub -h $BROKER_HOST -p 8883 --cafile mosquitto/certs/ca.crt \
  -u "$USER" -P "$PASS" -t "homes/$UID/availability" -m online -q 1 -r

# Denied: another gateway's namespace (ACL should reject)
mosquitto_pub -h $BROKER_HOST -p 8883 --cafile mosquitto/certs/ca.crt \
  -u "$USER" -P "$PASS" -t "homes/some-other-uid/availability" -m nope -q 1
```

### 2. Cloud subscriber daemon connects to the LIVE broker

`mqtt:subscribe` is the cloud's uplink ingest daemon (it runs in every
environment; locally it reads the anonymous broker in `local/`). To point it at
this broker node, set `MQTT_*` (see `config/mqtt-client.php`) and run:

```bash
php artisan mqtt:subscribe        # connects and subscribes to homes/+/...
php artisan mqtt:health-check     # reports the daemon heartbeat fresh
```

Publishing to `homes/$UID/availability` (step 1) should flip `gateways.is_online`.
