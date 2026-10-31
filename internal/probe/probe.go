// Package probe checks a slot's tunnel end to end with one request to
// am.i.mullvad.net: handshake done, traffic flowing, DNS through the tunnel
// working, and exiting through Mullvad (through the relay we picked).
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/relays"
)

// DefaultURL is Mullvad's connection check.
const DefaultURL = "https://am.i.mullvad.net/json"

// HTTP probes by GETting URL through the tunnel.
type HTTP struct{ URL string }

var _ pool.Prober = HTTP{}

type amIMullvad struct {
	IP             string `json:"ip"`
	MullvadExitIP  bool   `json:"mullvad_exit_ip"`
	ExitIPHostname string `json:"mullvad_exit_ip_hostname"`
}

// Probe returns nil when the response says we exit through Mullvad and, if
// it names the relay, through r. The deadline comes from ctx.
func (h HTTP) Probe(ctx context.Context, t pool.Tunnel, r relays.Relay) error {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return t.DialContext(ctx, addr)
		},
		DisableKeepAlives: true,
		// No Proxy: never take HTTP(S)_PROXY from the environment.
	}
	defer tr.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.URL, nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe: HTTP %d", resp.StatusCode)
	}
	var body amIMullvad
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&body); err != nil {
		return fmt.Errorf("probe: decode: %w", err)
	}
	if !body.MullvadExitIP {
		return fmt.Errorf("probe: not a Mullvad exit (ip %s)", body.IP)
	}
	if body.ExitIPHostname != "" && body.ExitIPHostname != r.Hostname {
		return fmt.Errorf("probe: exit relay is %s, want %s", body.ExitIPHostname, r.Hostname)
	}
	return nil
}
