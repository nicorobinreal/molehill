# molehill

Mullvad WireGuard relays as an HTTP CONNECT proxy: each sticky session gets
its own exit IP, chosen by country, and keeps it while it's in use.

There is no auth. Keep both ports (proxy and management API) on an internal
network.

## Requirements

- Docker, with the container given `NET_ADMIN` and
  `net.ipv4.conf.all.src_valid_mark=1`.
- A Mullvad account and at least one **device**: a WireGuard private key
  registered on the account plus the IPv4 address Mullvad assigned to it.
  molehill never creates devices itself.

## Getting a device

Either use the WireGuard config generator on Mullvad's website and take
`PrivateKey` and the IPv4 `Address` from the generated file, or use the API:

1. Account number → access token:

   ```sh
   curl -s -X POST https://api.mullvad.net/auth/v1/token \
     -H 'Content-Type: application/json' -d '{"account_number":"1234567890123456"}'
   ```

   Returns `{"access_token":"mva_...","expiry":"..."}`.

2. Register your own key (`wg genkey | tee priv | wg pubkey`):

   ```sh
   curl -s -X POST https://api.mullvad.net/accounts/v1/devices \
     -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
     -d '{"pubkey":"<public key>","hijack_dns":false}'
   ```

   The response includes `ipv4_address`. The private key plus that address is
   the device. `GET` on the same URL lists devices, `DELETE .../devices/{id}`
   removes one.

## Quick start

```yaml
services:
  molehill:
    build: https://github.com/nicorobinreal/molehill.git#v0.1.0
    image: molehill:v0.1.0
    restart: unless-stopped
    cap_add: [NET_ADMIN]
    sysctls:
      net.ipv4.conf.all.src_valid_mark: 1
    environment:
      MULLVAD_DEVICES: '[{"id":"d1","private_key":"<private key>","ipv4":"10.64.1.2"}]'
    ports:
      - "127.0.0.1:8080:8080" # CONNECT proxy
      - "127.0.0.1:8081:8081" # management API
    volumes:
      - molehill-data:/data

volumes:
  molehill-data:
```

Then send a request through a session named `s1` exiting in Sweden:

```sh
curl -x 'http://session-s1-cc-se:x@localhost:8080' https://am.i.mullvad.net/json
```

Usage, configuration, response codes and the management API are in
[docs.md](docs.md).

## License

MIT
