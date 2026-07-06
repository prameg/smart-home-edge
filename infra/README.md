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
the broker. (This replaces an earlier proxy-sidecar workaround; superseded by
per-gateway mTLS in Phase 4.)

## Setup

```bash
cp .env.example .env          # set CLOUD_HOST, CLOUD_PORT, SMART_HOME_BROKER_AUTH_SECRET
# Put ca.crt / server.crt / server.key in mosquitto/certs/ (see its README)
docker compose up -d
docker compose logs -f mosquitto
```

The cloud must have, in its environment, the SAME
`SMART_HOME_BROKER_AUTH_SECRET`, plus `SMART_HOME_PROVISIONING_FACTORY_KEY` for
the agent's provisioning call.

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
