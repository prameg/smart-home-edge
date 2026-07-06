#!/usr/bin/env bash
# Build smart-home-agent.tgz for Track 3 (local add-on install via HA terminal).
# Excludes dev-only secrets and local state — the add-on gets its own /data volume.
#
# Usage:
#   ./scripts/sync-addon-to-vm.sh        # write dist/smart-home-agent.tgz, print curl one-liner
#   ./scripts/sync-addon-to-vm.sh serve  # tarball on :8765 + Laravel on :8090 + print add-on URLs
#
# `serve` binds Laravel and the tarball HTTP server on 0.0.0.0 so a VirtualBox HA VM
# can reach the Mac host at 10.0.2.2. Mosquitto must already be running separately
# (see infra/local/mosquitto.local.conf — it listens on 0.0.0.0:1883).
set -euo pipefail

cd "$(dirname "$0")/.."
TARBALL="${TARBALL:-$(pwd)/dist/smart-home-agent.tgz}"
PORT="${SERVE_PORT:-8765}"
CLOUD_PORT="${CLOUD_PORT:-8090}"
MQTT_PORT="${MQTT_PORT:-1883}"
VB_HOST="${VB_HOST:-10.0.2.2}"
SMART_APP="${SMART_APP:-$(cd "$(dirname "$0")/../../../smart" && pwd)}"
LARAVEL_PID=""

host_lan_ip() {
  ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true
}

listening_on() {
  local port="$1"
  lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
}

cleanup() {
  if [[ -n "${LARAVEL_PID}" ]] && kill -0 "${LARAVEL_PID}" 2>/dev/null; then
    kill "${LARAVEL_PID}" 2>/dev/null || true
    wait "${LARAVEL_PID}" 2>/dev/null || true
  fi
}

start_laravel() {
  if listening_on "${CLOUD_PORT}"; then
    echo "Laravel already listening on 0.0.0.0:${CLOUD_PORT}"
    return 0
  fi

  if [[ ! -f "${SMART_APP}/artisan" ]]; then
    echo "error: Laravel app not found at ${SMART_APP} (set SMART_APP=…)" >&2
    exit 1
  fi

  echo "starting Laravel on 0.0.0.0:${CLOUD_PORT} (${SMART_APP})"
  (
    cd "${SMART_APP}"
    php artisan serve --host=0.0.0.0 --port="${CLOUD_PORT}" --no-reload
  ) &
  LARAVEL_PID=$!

  for _ in $(seq 1 30); do
    if listening_on "${CLOUD_PORT}"; then
      return 0
    fi
    sleep 0.2
  done

  echo "error: Laravel did not start on port ${CLOUD_PORT}" >&2
  exit 1
}

check_mosquitto() {
  if listening_on "${MQTT_PORT}"; then
    echo "MQTT broker listening on *:${MQTT_PORT}"
    return 0
  fi

  cat <<EOF >&2
warning: nothing listening on port ${MQTT_PORT}.
Start the local broker in another terminal:

  mosquitto -c infra/local/mosquitto.local.conf -v

(run from the edge repo root)
EOF
}

print_addon_config() {
  local lan_ip
  lan_ip="$(host_lan_ip)"

  cat <<EOF

Add-on configuration (Settings → Apps → Smart Home Agent → Configuration):

  VirtualBox NAT (same host address as the curl one-liner below):
    cloud_base_url: http://${VB_HOST}:${CLOUD_PORT}
    mqtt_host: ${VB_HOST}
    mqtt_port: ${MQTT_PORT}
    mqtt_tls: false

EOF

  if [[ -n "${lan_ip}" ]]; then
    cat <<EOF
  Bridged VM on your LAN (only if ${lan_ip} is reachable from the VM):
    cloud_base_url: http://${lan_ip}:${CLOUD_PORT}
    mqtt_host: ${lan_ip}

EOF
  fi
}

build_tarball() {
  mkdir -p "$(dirname "${TARBALL}")"

  # Avoid macOS AppleDouble (._*) junk in the archive.
  COPYFILE_DISABLE=1 tar czf "${TARBALL}" \
    --exclude='smart-home-agent/.data' \
    --exclude='smart-home-agent/.env.local' \
    --exclude='smart-home-agent/agent' \
    --exclude='smart-home-agent/dist' \
    -C "$(pwd)/.." \
    smart-home-agent
}

print_ha_commands() {
  cat <<EOF
built ${TARBALL} ($(du -h "${TARBALL}" | cut -f1))

In the HA VM (Settings → Terminal), drop in the new source:

  cd ~/addons && rm -rf smart-home-agent \\
    && curl -fL http://10.0.2.2:${PORT}/smart-home-agent.tgz | tar xz \\
    && ls smart-home-agent/config.yaml smart-home-agent/Dockerfile

FIRST install:  Settings → Add-ons → Add-on store → ⋮ → Check for updates →
                Local add-ons → Smart Home Agent → Install.
UPDATE (already installed — the UI does NOT auto-detect source changes):

  ha addons rebuild local_smart_home_agent && ha addons restart local_smart_home_agent

  # REBUILD is what re-reads config.yaml for a local add-on (manifest changes
  # like the ingress panel are applied here, not by 'ha addons reload', which
  # only refreshes store data).

Sidebar panel:  the ingress panel does NOT appear automatically.
  - If the add-on Info page has NO 'OPEN WEB UI' button, ingress is not
    registered: Supervisor's rebuild/update do not rebuild the ingress token
    map when an add-on GAINS ingress (supervisor#6556). Restart the Supervisor
    to fix it without losing /data:  ha supervisor restart
    (last resort: uninstall + reinstall the add-on — wipes /data).
  - Then on the Info page: 'OPEN WEB UI' to confirm ingress, turn ON
    'Show in sidebar' (defaults off), and hard-refresh (Cmd/Ctrl+Shift+R).

Watch it:  ha addons logs local_smart_home_agent -f
EOF
}

build_tarball
print_ha_commands

if [[ "${1:-}" == "serve" ]]; then
  trap cleanup EXIT INT TERM

  echo
  check_mosquitto
  start_laravel
  print_addon_config

  echo "serving $(dirname "${TARBALL}") on 0.0.0.0:${PORT} (Ctrl+C to stop)"
  exec python3 -m http.server "${PORT}" --bind 0.0.0.0 --directory "$(dirname "${TARBALL}")"
fi
