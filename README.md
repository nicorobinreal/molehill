# molehill

Mullvad as a rotating connect proxy with sticky sessions and country support.

## Usage

1. Register a WireGuard key on your Mullvad account (needs `wg`, `curl`, `jq`):

   ```sh
   ACCOUNT=<your account number> && KEY=$(wg genkey) && \
   TOKEN=$(curl -s -X POST https://api.mullvad.net/auth/v1/token -H 'Content-Type: application/json' \
     -d "{\"account_number\":\"$ACCOUNT\"}" | jq -r .access_token) && \
   IPV4=$(curl -s -X POST https://api.mullvad.net/accounts/v1/devices -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' -d "{\"pubkey\":\"$(echo "$KEY" | wg pubkey)\",\"hijack_dns\":false}" \
     | jq -r .ipv4_address)
   ```

2. Build and run the image:

   ```sh
   docker build -t molehill https://github.com/nicorobinreal/molehill.git#v0.1.0 && \
   docker run -d --name molehill --cap-add NET_ADMIN --sysctl net.ipv4.conf.all.src_valid_mark=1 \
     -p 127.0.0.1:8080:8080 -p 127.0.0.1:8081:8081 \
     -e MULLVAD_DEVICES="[{\"id\":\"d1\",\"private_key\":\"$KEY\",\"ipv4\":\"$IPV4\"}]" molehill
   ```

3. Send a request through a session named `s1` exiting in the US:

   ```sh
   curl -x 'http://session-s1-cc-us:x@localhost:8080' https://am.i.mullvad.net/json
   ```

The full reference (username format, configuration, response codes,
behavior, management API) is in [docs.md](docs.md).

## License

MIT
