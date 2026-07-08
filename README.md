# Smart Home Edge

The **edge** half of the smart-home platform. The cloud repo (Laravel) owns the
server side of a frozen contract; this repo owns everything that runs on the
physical gateway (a Raspberry Pi 5 running Home Assistant OS) plus the infra
needed to connect it to the cloud.

This is a **Home Assistant add-on repository**. Point a HAOS instance at this
repo's URL (Settings → Add-ons → Add-on Store → ⋮ → Repositories) and the
`smart-home-agent` add-on becomes installable.

## Layout

| Path                 | What it is                                                                                     |
| -------------------- | --------------------------------------------------------------------------------------------- |
| `repository.json`    | HA add-on repository descriptor (makes this repo an installable store).                        |
| `smart-home-agent/`  | The Go agent add-on: provisions, bridges HA ↔ cloud MQTT, implements the contract both ways.   |
| `smart-home-agent/cmd/smart-onboard/` | The onboarding CLI: drives a freshly flashed HAOS gateway to provisioned + claim code over HA's public APIs (resumable). |
| `smart-home-agent/fleet/release.json` | Machine-readable fleet manifest the CLI reads — add-on/Core/OS versions to install + pin (twin of `docs/fleet-release.md`). |
| `infra/`             | Cloud broker node (Mosquitto + mosquitto-go-auth) — `docker-compose` + config for an external host. |
| `infra/local/`       | Plain, anonymous local Mosquitto for on-machine testing (Homebrew or Docker).                  |
| `docs/local-testing.md` | Run the agent against your Home Assistant VM + a local broker, with or without the cloud.    |
| `docs/mqtt-contract.md` | **Mirrored** copy of the cloud repo's boundary doc. The single source of truth is the cloud repo; keep both in lockstep. |
| `docs/fleet-release.md` | Pinned Core/OS/add-on version manifest (template — populate on the bring-up unit). |
| `docs/offline-validation.md` | Runbook for the offline-autonomy + reconnect-reconciliation acceptance test.   |
| `docs/factory-onboarding.md` | Onboarding decision + `smart-onboard` runbook (stock HAOS + CLI; Supervised rejected; Jetson-as-companion recorded). |
| `docs/production-onboarding.md` | Team runbook: onboard a gateway against the live cloud (`smart.prameg.net`) + broker (`mqtt.prameg.net`), repeatable per unit. |

## The contract

The agent talks to three cloud endpoints and one MQTT broker. The topic/payload
boundary is stable — change it only in lockstep across both repos (see the
mirror note in [`docs/mqtt-contract.md`](docs/mqtt-contract.md)).

- **Provision** (factory key to enroll, per-gateway provision token to recover): `POST /api/v1/provisioning/gateways` → `{ uid, topic_namespace, claim_status, mqtt:{username,password}, provision_token, claim_code, claim_code_expires_at }`. Idempotent on hardware serial (recovery re-provisions to the same `uid`); a claim-code reissue lives at `POST /api/v1/provisioning/gateways/claim-code`.
- **Broker auth** (broker secret): the broker (not the agent) calls `POST /api/v1/broker/<secret>/{auth,superuser,acl}` — the secret is in the URL path because mosquitto-go-auth can't send a custom header.
- **MQTT topics/payloads**: see [`docs/mqtt-contract.md`](docs/mqtt-contract.md), mirrored in Go by `smart-home-agent/internal/contract`.

## Quick start (development)

```bash
cd smart-home-agent
go build ./...          # compile the agent
go vet ./... && go test ./...
```

To run the agent outside HAOS against your HA VM + a local broker, see
[`docs/local-testing.md`](docs/local-testing.md) (and
[`smart-home-agent/DOCS.md`](smart-home-agent/DOCS.md) for the config surface).

## What works today

- **Provisioning + recovery** — first boot registers the gateway; a `/data` wipe
  re-provisions to the same `uid` (rotated password), keeping the home binding.
- **Claiming** — an unclaimed gateway is broker-confined to `availability`/`config`
  only; the cloud claim flow binds it to a home and pushes the retained config.
- **Device auto-registration** — the agent reports its HA entity inventory
  (retained); the cloud creates a device per allow-listed domain and pushes the
  authoritative `device_uid ↔ entity_id` map back down. No hand-maintained map.
- **Two-way state sync** — HA state changes flow up as HA-native
  `{ state, attributes }`; the cloud's desired shadow flows down and converges by
  version, with the agent reporting HA's *actual* state (not an echo).
- **Offline autonomy + reconnect self-heal** — the home runs locally with the
  WAN down; on reconnect the retained `availability`/`config`/`shadow`, the
  persistent session's buffered downlink, and a (re)connect state reconcile bring
  the cloud back in sync with no provisioning round-trip.
- **Sidebar status panel** — an HA Ingress panel ("Smart Home") shows live
  agent status (claim state, broker connection, mapped devices, claim code) and
  offers a few actions (reissue claim code, republish inventory, reconcile
  state, re-provision) without reading add-on logs (see
  [`smart-home-agent/DOCS.md`](smart-home-agent/DOCS.md)).

## Not built yet (honest gaps)

- **mTLS** per gateway — deferred. Today it's per-gateway username/password over
  server-only TLS + per-gateway ACL, which is sufficient; if adopted later,
  per-gateway client certs (revocable per device) are preferred over a shared
  fleet cert.
- **Fleet-release rollout automation** — the version manifest exists
  (`docs/fleet-release.md`) but rolling a release across a fleet is manual.
- **Device events/alarms** — the event topic + cloud `events` table are wired,
  but the agent only emits `agent.boot` today; no device-level alarms (motion,
  door, low battery, offline) are produced yet.
- **Per-domain desired translation beyond on/off** — locks, covers and climate
  setpoints fall back to on/off + attribute passthrough for now.
- **Deployed TLS broker node** — `infra/` is ready to stand up but not yet a
  live host; local dev uses the anonymous broker in `infra/local/`.
- **Onboarding CLI on real hardware** — `smart-onboard` (stock HAOS + HA's
  public APIs, see `docs/factory-onboarding.md`) is implemented and unit-tested,
  but its end-to-end run against a live Supervisor is still to be validated on
  the bring-up unit (and the fleet manifest's versions populated).
- **AI companion node (Jetson, etc.)** — decided as a companion node next to the
  Pi (stock JetPack + Docker → local MQTT via MQTT Discovery, synced by the
  agent), but roadmap-only: no Jetson runtime package, MQTT entity contract, or
  cloud allow-list extension (`binary_sensor`/`sensor`/`event`) exists yet. See
  `docs/factory-onboarding.md`.
