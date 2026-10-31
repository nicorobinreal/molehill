package probe

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicorobinreal/molehill/internal/relays"
)

type directTunnel struct{ dials int }

func (d *directTunnel) Name() string                              { return "direct" }
func (d *directTunnel) Point(context.Context, relays.Relay) error { return nil }
func (d *directTunnel) Close() error                              { return nil }
func (d *directTunnel) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	d.dials++
	return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
}

func TestProbe(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		wantErr    string
	}{
		{"ok with hostname", `{"ip":"1.2.3.4","mullvad_exit_ip":true,"mullvad_exit_ip_hostname":"se-sto-wg-001"}`, 200, ""},
		{"ok without hostname", `{"mullvad_exit_ip":true}`, 200, ""},
		{"not mullvad", `{"ip":"1.2.3.4","mullvad_exit_ip":false}`, 200, "not a Mullvad exit"},
		{"other relay", `{"mullvad_exit_ip":true,"mullvad_exit_ip_hostname":"de-fra-wg-001"}`, 200, "exit relay is de-fra-wg-001"},
		{"http error", `{}`, 503, "HTTP 503"},
		{"bad json", `nope`, 200, "decode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			tun := &directTunnel{}
			err := HTTP{URL: srv.URL}.Probe(context.Background(), tun, relays.Relay{Hostname: "se-sto-wg-001"})
			if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("err = %v, want %q", err, c.wantErr)
			}
			if tun.dials != 1 {
				t.Fatalf("dials through tunnel = %d", tun.dials)
			}
		})
	}
}
