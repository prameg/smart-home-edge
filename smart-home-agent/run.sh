#!/usr/bin/with-contenv sh
# Supervisor injects SUPERVISOR_TOKEN (and options → /data/options.json) only
# into processes started through with-contenv — a bare CMD on the Go binary skips
# that and HA API auth fails with an empty token.
exec /usr/bin/smart-home-agent
