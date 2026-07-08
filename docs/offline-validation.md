# Offline-autonomy + reconnect-reconciliation runbook

The Phase 2 acceptance test. Success proves the architecture's core promise: the
home keeps working with the WAN unplugged, and the cloud self-heals on reconnect
with no manual intervention and no provisioning round-trip.

Run this on the bring-up Pi 5 (HAOS + the pinned add-ons + our agent) against the
live cloud broker.

## Preconditions

- Broker node up (`infra/`), cloud `SMART_HOME_MQTT_*` pointed at it, and
  `mqtt:subscribe` + `mqtt:health-check` green (see `infra/README.md`).
- The Pi's agent add-on is configured and the gateway is **claimed** to a home.
- At least one real device is mapped (default entity map or
  `/addon_config/entity-map.json`) and appears in the cloud with a `device_uid`.
- A local automation exists on the Pi that does NOT depend on the cloud, e.g.
  "when `binary_sensor.motion` is on, turn on `light.spike_test`".

## A. Baseline (online) round-trips

1. **State uplink** — toggle the mapped device physically; confirm the cloud's
   `reported_state` updates (Reverb pushes on `home.{home}`; or query the DB).
2. **Command round-trip** — issue a command from the cloud (`SendDeviceCommand`);
   confirm the device actuates and the command goes `published -> acked` (the
   agent publishes `cmd/ack`).
3. **Shadow convergence** — set a desired state (HA-native `{ state, attributes }`,
   bumping `desired_version`); confirm the device applies it and
   `reported_version == desired_version`. The agent applies it, then reports HA's
   **actual** resulting state on the state topic carrying the applied version —
   so `reported_state` is real HA truth, not an echo of the desired doc.
4. **Event** — the agent's boot event (`agent.boot`) should already be recorded;
   optionally trigger another event path.

## B. Offline autonomy (pull the plug)

1. Physically disconnect the Pi's WAN (unplug ethernet / drop Wi-Fi upstream).
   Keep local Zigbee/Matter radios powered.
2. Trigger the local automation (wave at the motion sensor). **PASS:** the light
   turns on locally within the normal latency — HA + local Mosquitto +
   Zigbee2MQTT run the home with no cloud.
3. While offline, toggle the mapped device a few times. The agent's bounded
   drop-oldest uplink buffer (`uplinkBufferLimit`) holds the most recent uplinks;
   older ones are intentionally dropped so memory stays bounded.
4. From the cloud (still reachable by the operator, just not by the Pi), issue a
   command and set a new desired shadow. These are retained/queued at the broker.

## C. Reconnect reconciliation

1. Reconnect the Pi's WAN.
2. **Last-will / status self-heal** — while offline the broker published the
   agent's retained `offline` last-will, so `gateways.is_online` went false. On
   reconnect the agent republishes retained `online`. **PASS:** `is_online`
   returns to true and `last_seen_at` advances, with no provisioning call.
3. **Downlink QoS-1 buffer** — the command issued in B.4 was buffered by the
   broker against the agent's persistent session (`clean_session=false`, stable
   client id) and is delivered on reconnect. **PASS:** the command applies if
   still within `ttl_sec`, else the cloud shows it `expired` (both are correct).
4. **Retained shadow reconverges** — the retained per-device
   `shadow/desired/{device_uid}` from B.4 is delivered on subscribe; the agent
   applies it and reports the version. **PASS:**
   `reported_version == desired_version` again.
5. **Buffered uplinks flush + reconnect reconcile** — the recent uplinks from
   B.3 are published on reconnect, and the agent additionally re-reports every
   mapped device's current HA state on (re)connect (so `reported_state` self-heals
   even for a device that did not change while offline, and after an agent
   restart that emptied the in-memory buffer). **PASS:** the cloud's
   `reported_state` reflects the latest local state (monotonic version ingest
   means any stale duplicate is ignored).

## D. Recovery provisioning (idempotency)

1. Note the gateway's `uid` and claimed home.
2. Wipe the agent's `/data` (simulate factory reset) and restart the add-on.
3. **PASS:** the agent re-provisions on the same serial, gets the **same `uid`**
   with a rotated MQTT password, and the home/claim binding is intact (no new
   gateway row, home membership unchanged).

## E. Footprint

Record on the Pi 5 under steady state and during a reconnect flush:

```bash
ha addons stats smart_home_agent   # CPU % + memory of the agent add-on
free -m                            # overall RAM headroom
df -h /                            # flash usage
```

Capture CPU %, RSS, and flash delta. Target: the agent is a rounding error next
to Core + Zigbee2MQTT; if not, investigate before scaling the fleet.

## Result log

| Check | Result | Notes |
| ----- | ------ | ----- |
| A.1 state uplink | | |
| A.2 command round-trip | | |
| A.3 shadow convergence | | |
| B.2 local automation offline | | |
| C.2 status self-heal | | |
| C.3 downlink buffered delivery | | |
| C.4 shadow reconverge | | |
| C.5 buffered uplinks flush | | |
| D.3 recovery idempotency | | |
| E footprint (CPU/RAM/flash) | | |
