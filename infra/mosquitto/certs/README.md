# Broker TLS certificate

The TLS listener (`8883`) is **server-only TLS**: the broker presents a server
certificate, gateways verify it with normal system trust, and there is **no
client certificate** (`require_certificate false`). Device identity is proven
per-gateway by MQTT username/password (verified by the cloud broker-auth
endpoint) plus the per-gateway ACL — not by a cert. So the listener needs just
two files:

| File         | What                                    |
| ------------ | --------------------------------------- |
| `server.crt` | Broker server certificate (CN/SAN = the broker's public host). |
| `server.key` | Broker server private key.              |

No `ca.crt` / client CA is required for server-only TLS.

## Production — Let's Encrypt, not this directory

In production the server cert is a **Let's Encrypt** certificate for the
broker's public hostname (e.g. `broker.example.com`), issued and auto-renewed by
Laravel Forge. The container does **not** read this directory; it binds a stable
host path (`/opt/mosquitto/certs`) that a host renewal hook keeps in sync with
Forge's latest cert, then SIGHUPs Mosquitto so it reloads without dropping
sessions. See [`infra/README.md`](../../README.md) for the host setup + the
renewal hook. Because the broker presents a publicly-trusted chain, gateways
connect with normal system trust and `mqtt_tls_insecure` stays `false`.

## Local / spike (self-signed)

For a spike against an external host without Let's Encrypt, drop a self-signed
`server.crt` / `server.key` here and point the compose mount at this directory
instead of `/opt/mosquitto/certs`:

```bash
# CA (only to sign the server cert; agents don't need a client cert)
openssl req -x509 -new -nodes -newkey rsa:2048 -days 3650 \
  -keyout ca.key -out ca.crt -subj "/CN=smart-home-dev-ca"

# Server cert for the broker host (replace broker.example.com)
openssl req -new -nodes -newkey rsa:2048 \
  -keyout server.key -out server.csr -subj "/CN=broker.example.com"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 825 -out server.crt \
  -extfile <(printf "subjectAltName=DNS:broker.example.com")
```

With a self-signed chain, either ship `ca.crt` to the gateway's trust store or
set `mqtt_tls_insecure: true` on the add-on for the spike only. For local
development on your own machine, prefer the plain (no-TLS) broker in
[`infra/local/`](../../local/).

## Deferred: client-cert mTLS

Client-cert mTLS is intentionally **not** used today — server TLS + per-gateway
username/password + per-gateway ACL + the suspend/quarantine killswitch is a
sufficient posture. If it is ever adopted, prefer **per-gateway** certs (a
revocable per-device identity) over a shared fleet cert: add a client `cafile`
here, set `require_certificate true` in `mosquitto.conf.template`, and present
the client cert from the agent and the cloud MQTT clients.
