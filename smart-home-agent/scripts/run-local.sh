#!/usr/bin/env bash
# Run the agent on your machine (outside HAOS) against a local broker + your
# Home Assistant VM. Loads .env.local (see .env.local.example) and runs it in
# the foreground — Ctrl+C stops it. See docs/local-testing.md.
set -euo pipefail

cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
  echo "error: 'go' is not installed or not on PATH." >&2
  exit 1
fi

if [ ! -f .env.local ]; then
  echo "error: no .env.local found — copy .env.local.example to .env.local and edit it first." >&2
  exit 1
fi

set -a
# shellcheck disable=SC1091
. ./.env.local
set +a

if [ -z "${MQTT_HOST:-}" ]; then
  echo "error: MQTT_HOST is unset in .env.local (nothing to connect to)." >&2
  exit 1
fi

DATA_DIR="${AGENT_DATA_DIR:-./.data}"
mkdir -p "${DATA_DIR}"

echo "starting agent → HA ${HA_REST_BASE_URL:-<unset>}, broker ${MQTT_HOST}:${MQTT_PORT:-8883} (tls=${MQTT_TLS:-true}), data ${DATA_DIR}"
if [ ! -f "${DATA_DIR}/agent-creds.json" ] && [ -z "${CLOUD_BASE_URL:-}" ]; then
  echo "note: no ${DATA_DIR}/agent-creds.json and no CLOUD_BASE_URL — the agent will try to provision and fail." >&2
  echo "      pre-seed creds for the no-cloud track, or set CLOUD_BASE_URL/FACTORY_KEY (see docs/local-testing.md)." >&2
fi

exec go run .
