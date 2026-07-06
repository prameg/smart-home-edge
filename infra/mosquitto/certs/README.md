# Broker TLS certificates

The TLS listener (`8883`) needs three files in this directory (git-ignored):

| File         | What                                                            |
| ------------ | -------------------------------------------------------------- |
| `ca.crt`     | CA that signed the server cert (agents trust this chain).       |
| `server.crt` | Broker server certificate (CN/SAN = the broker's public host).  |
| `server.key` | Broker server private key.                                      |

## Production

Use a real certificate for the broker's public hostname (Let's Encrypt via a
DNS/HTTP challenge, or your org CA). The agent connects with normal system trust
when the broker presents a publicly-trusted chain, so `mqtt_tls_insecure` stays
`false` on the gateway.

## Local / spike (self-signed)

```bash
# CA
openssl req -x509 -new -nodes -newkey rsa:2048 -days 3650 \
  -keyout ca.key -out ca.crt -subj "/CN=smart-home-dev-ca"

# Server cert for the broker host (replace broker.example.com)
openssl req -new -nodes -newkey rsa:2048 \
  -keyout server.key -out server.csr -subj "/CN=broker.example.com"
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 825 -out server.crt \
  -extfile <(printf "subjectAltName=DNS:broker.example.com")
```

With a self-signed CA, either ship `ca.crt` to the gateway's trust store or set
`mqtt_tls_insecure: true` on the add-on for the spike only.
