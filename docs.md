# molehill reference

## Proxy username

`session-<SESSION>-cc-<COUNTRY_CODE>-strict-<STRICT>`: key-value pairs joined
by `-`, in any order. The password is ignored, and unknown keys are ignored.

| Key | | |
|---|---|---|
| `session` | required | Sticky session name. Must not contain `-`. |
| `cc` | optional | Mullvad country code (`se`, `de`, `gb`, ...; case-insensitive). Omitted = any country. |
| `strict` | optional, `0`/`1` | `1` refuses with `host_saturated` instead of sharing an exit IP with another active session for the same host (see Behavior). |

Only `CONNECT` is supported. The "host" of a request is the CONNECT target's
hostname.

## Configuration

| Env var | Default | |
|---|---|---|
| `MULLVAD_DEVICES` | (none) | JSON array of `{"id","private_key","ipv4"}`. `ipv4` may carry `/32`. IPv6 addresses are ignored. |
| `MULLVAD_SLOTS_PER_DEVICE` | (none) | Fixed number of exit IPs (WireGuard interfaces) per device, all opened at startup. Without it, a device opens them as sessions need them. |
| `MULLVAD_PROXY_ADDR` | `:8080` | CONNECT proxy. |
| `MULLVAD_API_ADDR` | `:8081` | Management API. |
| `MULLVAD_RELAYS_URL` | `https://api.mullvad.net/www/relays/wireguard/` | Relay list. Only active WireGuard relays with an IPv4 endpoint are used. |
| `MULLVAD_RELAYS_FILE` | `/data/relays.json` | Last good relay list, used when the relay API is down at startup. |
| `MULLVAD_RELAYS_REFRESH` | `6h` | Relay list refresh interval (retried every minute while no list was loaded at all). |
| `MULLVAD_PROBE_URL` | `https://am.i.mullvad.net/json` | Checked through a relay before it's used; must answer like Mullvad's connection check (`"mullvad_exit_ip": true`). |
| `MULLVAD_TUNNEL_DNS` | `10.64.0.1` | DNS server reached through each tunnel. |
| `MULLVAD_GRACE` | `60s` | How long a session keeps its exit IP after its last tunnel closes. |
| `MULLVAD_DIAL_TIMEOUT` | `10s` | Target dial (DNS + TCP) through the tunnel. |
| `MULLVAD_MTU` | `1380` | Interface MTU. |
| `LOG_LEVEL` | `info` | `debug` logs every session binding and tunnel. |

In a compose `.env` file, single-quote the JSON:
`MULLVAD_DEVICES='[{"id":"d1","private_key":"...","ipv4":"10.64.1.2"}]'`.

Nothing is kept across restarts except the cached relay list: sessions start
fresh after a restart.

## Responses

The target is dialed through the tunnel before the CONNECT is answered, so a
failed dial is the CONNECT response, not a dead tunnel after a 200. 502 and
503 responses carry `X-Mullvad-Error`.

| Status | `X-Mullvad-Error` | When |
|---|---|---|
| 200 | | Tunnel open. |
| 400 | | Missing or unparsable username, no `session`, bad `strict`, a `cc` with no relays, target not `host:port`. |
| 405 | | Not a CONNECT. |
| 502 | `session_expired` | The session ended (see Behavior). It never moves to a new exit IP. |
| 502 | `slot_drained` | The session's exit IP was taken out of service (device removed, or the relay stopped working). |
| 502 | `dial_timeout` | The target dial timed out. |
| 502 | `dial_failed` | The target refused or reset the connection, doesn't resolve, has no A record, is an IPv6 literal, or resolves to a local address. |
| 503 | `no_slot` | No exit IP can serve the request right now. `Retry-After: 5`. |
| 503 | `host_saturated` | `strict-1` and every exit IP in the country already has an active session for this host. `Retry-After: 10`. |

A 503 means "no capacity, try again later"; a 502 for an existing session
means that session is over and the client should start a new one.

## Behavior

- **Stickiness.** A session keeps its exit IP while it has open tunnels and
  for the grace period (`MULLVAD_GRACE`, 60s) after the last one closes, even
  if a later request asks for a different `cc`.
- **Expiry.** After the grace period the session gets 502 `session_expired`
  for an hour; it never silently moves to a new IP. After that hour the same
  name starts a fresh session.
- **New sessions** get an exit IP of their own while the country has relays
  no other session uses (and, with `MULLVAD_SLOTS_PER_DEVICE`, while the
  devices have room). After that they share: an exit IP with the fewest
  sessions for the same host first. Waiting for an exit IP gives up after 15s
  with `no_slot`.
- **`strict-1`** never shares an exit IP with another session that has open
  tunnels to the same host; when that isn't possible it gets
  `host_saturated`. Sessions idle in their grace period don't count, so an
  idle session that comes back may briefly share an IP with a newer one.
- **Relay checks.** A relay is checked before a session gets it. If a dial
  through it times out it's checked again; if that fails, it's taken out of
  service: its sessions get 502 `slot_drained`, open tunnels finish, and new
  sessions go elsewhere. A target refusing connections never counts against
  the relay.
- **Draining a device** (`DELETE /devices/{id}`) works the same way: no new
  sessions, its sessions get 502 on their next CONNECT, open tunnels finish,
  then its interfaces are removed.

## Management API (`:8081`)

| | |
|---|---|
| `GET /devices` | List devices (no private keys). |
| `POST /devices` | Add `{"id","private_key","ipv4"}`. 201; 409 if the id exists; 400 if invalid. In memory only: also add it to `MULLVAD_DEVICES` to keep it across restarts. |
| `DELETE /devices/{id}` | Drain and remove the device. 202. |
| `GET /status` | Snapshot of exit IPs (interface, state, relay, country, sessions, open tunnels, sessions per host), relays currently marked bad, relay count. |
| `GET /healthz` | `ok`. |

## Troubleshooting

- **Replies through the tunnel are dropped**: on a host with strict reverse
  path filtering, add `net.ipv4.conf.all.rp_filter: 2` to the service's
  `sysctls`.
- **Every request gets `no_slot`**: check `GET /status` and the logs; no
  device configured, or relays failing their check, both show up there.

## Development

```sh
go vet ./...
go test -race ./...
docker build -t molehill .
```
