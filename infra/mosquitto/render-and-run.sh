#!/bin/sh
# Render mosquitto.conf from its template and exec the broker.
#
# We deliberately do NOT use envsubst here. The iegomez/mosquitto-go-auth image
# is Debian-based and ships no gettext, and installing it at runtime is a trap:
# the image's apt `stable` alias has rolled bullseye -> trixie (apt refuses with
# a "Codename changed" error), and forcing past that pulls a trixie envsubst that
# won't run against the image's older glibc. `sed` is already in the image, needs
# no network, and covers the three placeholders we actually use.
#
# Fail hard on any missing input or a bad render so the broker can never silently
# fall back to Mosquitto's default LOCAL-ONLY config (no 8883, no auth), which is
# what made the cloud/gateway connections time out.
set -eu

TEMPLATE=/mosquitto/config/mosquitto.conf.template
CONF=/mosquitto/config/mosquitto.conf

: "${CLOUD_HOST:?CLOUD_HOST is required}"
: "${CLOUD_PORT:?CLOUD_PORT is required}"
: "${SMART_HOME_BROKER_AUTH_SECRET:?SMART_HOME_BROKER_AUTH_SECRET is required}"

# Escape a value for safe use as a sed replacement string (\, /, and &).
escape() {
  printf '%s' "$1" | sed -e 's/[\/&\\]/\\&/g'
}

sed \
  -e "s/\${CLOUD_HOST}/$(escape "$CLOUD_HOST")/g" \
  -e "s/\${CLOUD_PORT}/$(escape "$CLOUD_PORT")/g" \
  -e "s/\${SMART_HOME_BROKER_AUTH_SECRET}/$(escape "$SMART_HOME_BROKER_AUTH_SECRET")/g" \
  "$TEMPLATE" > "$CONF"

# Guard: non-empty and no unresolved ${...} placeholders left behind.
test -s "$CONF"
if grep -q '\${' "$CONF"; then
  echo "render-and-run: unresolved \${...} placeholder in $CONF" >&2
  exit 1
fi

exec mosquitto -c "$CONF"
