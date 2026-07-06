# Local Mosquitto (development)

A plain, anonymous, no-TLS broker for iterating on your machine. Two ways to run
it — pick one.

## Option A — Homebrew (no Docker needed)

```bash
brew install mosquitto            # one-time
# Run it in the foreground with this config:
mosquitto -c infra/local/mosquitto.local.conf -v
```

The `mosquitto_sub` / `mosquitto_pub` CLI tools ship with the same package.

> **macOS caveat (honest):** on recent Homebrew builds the libmosquitto 2.1.x
> CLI clients can fail with `Error: Bad file descriptor` on connect. If that
> hits you, either use the Docker clients
> (`docker run --rm -it eclipse-mosquitto mosquitto_sub …`) or the repo's small
> observer, which uses the agent's own (working) MQTT library:
>
> ```bash
> # from smart-home-agent/ — watch everything, or add -pub/-m to inject
> go run ./tools/mqttobserve -t 'homes/#'
> ```

## Option B — Docker

```bash
docker compose -f infra/local/docker-compose.yml up
```

## Sanity check

In one terminal subscribe to everything, in another publish:

```bash
mosquitto_sub -h localhost -p 1883 -t 'homes/#' -v      # watch
mosquitto_pub -h localhost -p 1883 -t 'homes/demo/availability' -m online -r   # publish
```

You should see the retained `online` message echoed in the subscriber.

Next: wire the agent + your Home Assistant VM to this broker — see
[`../../docs/local-testing.md`](../../docs/local-testing.md).
