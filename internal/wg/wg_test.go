package wg

import (
	"context"
	"errors"
	"net"
	"testing"
)

type dialRec struct {
	lookups []string
	dials   []string
}

func (r *dialRec) lookup(ips ...string) func(context.Context, string, string) ([]net.IP, error) {
	return func(_ context.Context, network, host string) ([]net.IP, error) {
		r.lookups = append(r.lookups, network+" "+host)
		var out []net.IP
		for _, s := range ips {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
}

func (r *dialRec) dial(fail map[string]bool) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, network, addr string) (net.Conn, error) {
		r.dials = append(r.dials, network+" "+addr)
		if fail[addr] {
			return nil, errors.New("refused")
		}
		a, _ := net.Pipe()
		return a, nil
	}
}

func noLocal(net.IP) bool { return false }

func TestDialIPv4RejectsIPv6Literal(t *testing.T) {
	r := &dialRec{}
	for _, addr := range []string{"[2001:db8::1]:443", "[::1]:80"} {
		if _, err := dialIPv4(context.Background(), addr, r.lookup(), r.dial(nil), noLocal); !errors.Is(err, errIPv6) {
			t.Fatalf("%s: err = %v", addr, err)
		}
	}
	if len(r.dials) != 0 {
		t.Fatalf("dialed: %v", r.dials)
	}
}

func TestDialIPv4ResolvesARecordsOnlyAndDialsTCP4(t *testing.T) {
	r := &dialRec{}
	// A resolver returning a v6 address anyway must not lead to a v6 dial.
	c, err := dialIPv4(context.Background(), "example.com:443", r.lookup("2001:db8::1", "192.0.2.7"), r.dial(nil), noLocal)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(r.lookups) != 1 || r.lookups[0] != "ip4 example.com" {
		t.Fatalf("lookups = %v", r.lookups)
	}
	if len(r.dials) != 1 || r.dials[0] != "tcp4 192.0.2.7:443" {
		t.Fatalf("dials = %v", r.dials)
	}
}

func TestDialIPv4NoARecords(t *testing.T) {
	r := &dialRec{}
	_, err := dialIPv4(context.Background(), "v6only.test:443", r.lookup("2001:db8::1"), r.dial(nil), noLocal)
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || len(r.dials) != 0 {
		t.Fatalf("err = %v, dials = %v", err, r.dials)
	}
}

func TestDialIPv4TriesNextAddress(t *testing.T) {
	r := &dialRec{}
	c, err := dialIPv4(context.Background(), "h:80", r.lookup("192.0.2.1", "192.0.2.2"), r.dial(map[string]bool{"192.0.2.1:80": true}), noLocal)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if len(r.dials) != 2 {
		t.Fatalf("dials = %v", r.dials)
	}
}

func TestDialIPv4RefusesLocalTargets(t *testing.T) {
	r := &dialRec{}
	for _, addr := range []string{"127.0.0.1:8081", "0.0.0.0:80", "[::ffff:127.0.0.1]:80"} {
		if _, err := dialIPv4(context.Background(), addr, r.lookup(), r.dial(nil), isLocalAddr); !errors.Is(err, errLocalTarget) {
			t.Fatalf("%s: err = %v", addr, err)
		}
	}
	if _, err := dialIPv4(context.Background(), "localhost:80", r.lookup("127.0.0.1"), r.dial(nil), isLocalAddr); !errors.Is(err, errLocalTarget) {
		t.Fatalf("err = %v", err)
	}
	if len(r.dials) != 0 {
		t.Fatalf("dialed: %v", r.dials)
	}
}

func TestControlRefusesNonIPv4Sockets(t *testing.T) {
	tun := &Tunnel{mark: 1}
	for _, nw := range []string{"tcp6", "udp6", "tcp", "udp"} {
		if err := tun.control(nw, "", nil); err == nil {
			t.Fatalf("%s accepted", nw)
		}
	}
}
