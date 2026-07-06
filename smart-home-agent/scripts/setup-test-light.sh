#!/usr/bin/env bash
# Create a controllable test light in Home Assistant for Track 2/3 cloud sync.
#
# The cloud inventory only registers allow-listed domains (light, switch, …).
# A plain Toggle helper is input_boolean.* and is ignored. This script creates:
#   1. input_boolean.bed_lights  — backing on/off state ("Bed Lights")
#   2. light.bed_light           — template light the cloud registers ("Bed Light")
#
# Loads HA credentials from smart-home-agent/.env.local (same token as the agent).
#
# Usage:
#   ./scripts/setup-test-light.sh
#   HA_AREA=Kitchen ./scripts/setup-test-light.sh
#
# After running, republish inventory (Smart Home panel → Republish inventory, or
# restart the add-on) so the cloud picks up light.bed_light.
set -euo pipefail

cd "$(dirname "$0")/.."

if [[ -f .env.local ]]; then
  # shellcheck disable=SC1091
  set -a && source .env.local && set +a
fi

TOKEN="${SUPERVISOR_TOKEN:-}"
REST_BASE="${HA_REST_BASE_URL:-http://127.0.0.1:8123/api}"
WS_URL="${HA_WEBSOCKET_URL:-ws://127.0.0.1:8123/api/websocket}"
AREA_NAME="${HA_AREA:-Bedroom}"

if [[ -z "${TOKEN}" ]]; then
  echo "error: set SUPERVISOR_TOKEN in .env.local (HA long-lived access token)" >&2
  exit 1
fi

export SETUP_HA_TOKEN="${TOKEN}"
export SETUP_HA_REST_BASE="${REST_BASE%/}"
export SETUP_HA_WS_URL="${WS_URL}"
export SETUP_HA_AREA="${AREA_NAME}"

python3 <<'PY'
import json
import os
import sys
import urllib.error
import urllib.request
import asyncio
import websockets

TOKEN = os.environ["SETUP_HA_TOKEN"]
REST = os.environ["SETUP_HA_REST_BASE"]
WS = os.environ["SETUP_HA_WS_URL"]
AREA_NAME = os.environ["SETUP_HA_AREA"]
TOGGLE_ID = "bed_lights"
LIGHT_ENTITY = "light.bed_light"


def rest(method: str, path: str, data=None):
    body = None if data is None else json.dumps(data).encode()
    req = urllib.request.Request(
        REST + path,
        data=body,
        method=method,
        headers={
            "Authorization": f"Bearer {TOKEN}",
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")
        raise RuntimeError(f"{method} {path} -> {e.code}: {detail}") from e


async def ws_call(ws, msg_id: list, type_, **kwargs):
    msg_id[0] += 1
    i = msg_id[0]
    await ws.send(json.dumps({"id": i, "type": type_, **kwargs}))
    while True:
        msg = json.loads(await ws.recv())
        if msg.get("id") == i:
            if not msg.get("success", True):
                raise RuntimeError(f"{type_} failed: {msg.get('error', msg)}")
            return msg.get("result")


async def ensure_toggle(ws, msg_id):
    state = rest("GET", f"/states/input_boolean.{TOGGLE_ID}")
    if state and "entity_id" in state:
        print(f"toggle input_boolean.{TOGGLE_ID} already exists (state={state['state']})")
        return

    result = await ws_call(
        ws,
        msg_id,
        "input_boolean/create",
        name="Bed Lights",
        icon="mdi:flashlight",
    )
    print(f"created toggle input_boolean.{result.get('id', TOGGLE_ID)}")


async def ensure_area(ws, msg_id):
    areas = await ws_call(ws, msg_id, "config/area_registry/list")
    for area in areas:
        if area.get("name") == AREA_NAME:
            return area["area_id"]

    area = await ws_call(ws, msg_id, "config/area_registry/create", name=AREA_NAME)
    print(f"created area {AREA_NAME!r}")
    return area["area_id"]


async def assign_area(ws, msg_id, area_id):
    entities = await ws_call(ws, msg_id, "config/entity_registry/list")
    entry = next((e for e in entities if e["entity_id"] == LIGHT_ENTITY), None)
    if entry is None:
        return
    if entry.get("area_id") == area_id:
        print(f"{LIGHT_ENTITY} already in area {AREA_NAME!r}")
        return
    await ws_call(
        ws,
        msg_id,
        "config/entity_registry/update",
        entity_id=LIGHT_ENTITY,
        area_id=area_id,
    )
    print(f"assigned {LIGHT_ENTITY} to area {AREA_NAME!r}")


def ensure_template_light():
    state = rest("GET", f"/states/{LIGHT_ENTITY}")
    if state and "entity_id" in state:
        print(f"template {LIGHT_ENTITY} already exists (state={state['state']})")
        return

    flow = rest("POST", "/config/config_entries/flow", {"handler": "template"})
    flow_id = flow["flow_id"]
    rest("POST", f"/config/config_entries/flow/{flow_id}", {"next_step_id": "light"})
    result = rest(
        "POST",
        f"/config/config_entries/flow/{flow_id}",
        {
            "name": "Bed Light",
            "state": f"{{{{ is_state('input_boolean.{TOGGLE_ID}', 'on') }}}}",
            "turn_on": {
                "action": "input_boolean.turn_on",
                "target": {"entity_id": f"input_boolean.{TOGGLE_ID}"},
            },
            "turn_off": {
                "action": "input_boolean.turn_off",
                "target": {"entity_id": f"input_boolean.{TOGGLE_ID}"},
            },
        },
    )
    if result.get("type") != "create_entry":
        raise RuntimeError(f"unexpected template flow result: {result}")
    print(f"created template {LIGHT_ENTITY}")


async def main():
    async with websockets.connect(WS) as ws:
        await ws.recv()
        await ws.send(json.dumps({"type": "auth", "access_token": TOKEN}))
        auth = json.loads(await ws.recv())
        if auth.get("type") != "auth_ok":
            raise RuntimeError(f"HA auth failed: {auth}")

        msg_id = [0]
        await ensure_toggle(ws, msg_id)
        ensure_template_light()
        area_id = await ensure_area(ws, msg_id)
        await assign_area(ws, msg_id, area_id)

    print()
    print("Done. Verify in HA: Developer Tools → States → light.bed_light")
    print("Then republish inventory (Smart Home panel → Republish inventory).")


try:
    asyncio.run(main())
except Exception as exc:
    print(f"error: {exc}", file=sys.stderr)
    sys.exit(1)
PY
