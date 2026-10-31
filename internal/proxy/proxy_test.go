package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/pool/pooltest"
)

func TestParseUsername(t *testing.T) {
	good := map[string]Params{
		"session-abc":                    {Session: "abc"},
		"session-abc-cc-se":              {Session: "abc", Country: "se"},
		"session-abc-cc-SE-strict-1":     {Session: "abc", Country: "se", Strict: true},
		"session-abc-strict-0":           {Session: "abc"},
		"cc-de-session-x1":               {Session: "x1", Country: "de"},
		"session-abc-cc-se-future-thing": {Session: "abc", Country: "se"},
	}
	for u, want := range good {
		got, err := ParseUsername(u)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", u, got, err, want)
		}
	}
	for _, u := range []string{"", "session", "cc-se", "session-abc-cc", "session--cc-se", "session-a-strict-2",
		"session-a-session-b", "session-abc-cc-s1", "abc"} {
		if _, err := ParseUsername(u); err == nil {
			t.Errorf("%q accepted", u)
		}
	}
}

type tenv struct {
	t      *testing.T
	p      *pool.Pool
	net    *pooltest.Network
	prober *pooltest.Prober
	clock  *pooltest.Clock
	proxy  string
	echo   string
	closed string
}

// timeoutErr is a dial timeout as net.Dialer reports it.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func newTenv(t *testing.T, devices int) *tenv {
	e := &tenv{t: t, net: &pooltest.Network{}, prober: &pooltest.Prober{}, clock: pooltest.NewClock()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Echo server standing in for the target.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	e.echo = ln.Addr().String()
	cl, _ := net.Listen("tcp4", "127.0.0.1:0")
	e.closed = cl.Addr().String()
	cl.Close()

	e.net.Dial = func(ctx context.Context, _ *pooltest.Tunnel, addr string) (net.Conn, error) {
		switch addr {
		case "echo.test:80":
			addr = e.echo
		case "refused.test:80":
			addr = e.closed
		case "slow.test:80":
			return nil, &net.OpError{Op: "dial", Net: "tcp4", Err: timeoutErr{}}
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp4", addr)
	}
	e.p = pool.New(pool.Config{SlotsPerDevice: 1}, e.net, e.prober, e.clock, rand.New(rand.NewPCG(1, 2)), log)
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 4}))
	for i := range devices {
		if err := e.p.AddDevice(context.Background(), pool.Device{ID: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	e.p.WaitBackground()
	srv := httptest.NewServer(&Server{Pool: e.p, Log: log, DialTimeout: 2 * time.Second, AcquireTimeout: 2 * time.Second})
	t.Cleanup(srv.Close)
	e.proxy = strings.TrimPrefix(srv.URL, "http://")
	return e
}

// connect sends a CONNECT and returns the response and the open connection.
func (e *tenv) connect(user, target string) (*http.Response, net.Conn, *bufio.Reader) {
	e.t.Helper()
	c, err := net.Dial("tcp", e.proxy)
	if err != nil {
		e.t.Fatal(err)
	}
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":x")) + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		e.t.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		c.Close()
	}
	e.p.WaitBackground()
	return resp, c, br
}

func (e *tenv) expect(user, target string, status int, mullvadErr string) {
	e.t.Helper()
	resp, c, _ := e.connect(user, target)
	if resp.StatusCode != status || resp.Header.Get("X-Mullvad-Error") != mullvadErr {
		e.t.Fatalf("%s %s: got %d %q, want %d %q", user, target, resp.StatusCode, resp.Header.Get("X-Mullvad-Error"), status, mullvadErr)
	}
	if status == 200 {
		c.Close()
	}
}

func (e *tenv) waitOpenTunnels(n int) {
	e.t.Helper()
	for range 200 {
		total := 0
		for _, s := range e.p.Status().Slots {
			total += s.OpenTunnels
		}
		if total == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("open tunnels never reached %d", n)
}

func TestConnectTunnelsData(t *testing.T) {
	e := newTenv(t, 1)
	resp, c, br := e.connect("session-a-cc-se", "echo.test:80")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	io.WriteString(c, "hello\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "hello\n" {
		t.Fatalf("echo: %q %v", line, err)
	}
	e.waitOpenTunnels(1)
	c.Close()
	e.waitOpenTunnels(0)
	// Within grace the session reconnects to the same slot.
	e.expect("session-a-cc-se", "echo.test:80", 200, "")
}

func TestConnectBadRequests(t *testing.T) {
	e := newTenv(t, 1)
	e.expect("", "echo.test:80", 400, "")
	e.expect("cc-se", "echo.test:80", 400, "")
	e.expect("session-a-strict-yes", "echo.test:80", 400, "")
	e.expect("session-a-cc-zz", "echo.test:80", 400, "")
	e.expect("session-a", "echo.test", 400, "")

	resp, err := http.Get("http://" + e.proxy + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
}

func TestConnectNoSlot(t *testing.T) {
	e := newTenv(t, 0)
	resp, _, _ := e.connect("session-a", "echo.test:80")
	if resp.StatusCode != 503 || resp.Header.Get("X-Mullvad-Error") != "no_slot" || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
}

func TestConnectHostSaturated(t *testing.T) {
	e := newTenv(t, 1)
	// a's tunnel stays open: only active sessions count for strict.
	ra, ca, _ := e.connect("session-a-cc-se", "echo.test:80")
	if ra.StatusCode != 200 {
		t.Fatalf("a: got %d", ra.StatusCode)
	}
	defer ca.Close()
	resp, _, _ := e.connect("session-b-cc-se-strict-1", "echo.test:80")
	if resp.StatusCode != 503 || resp.Header.Get("X-Mullvad-Error") != "host_saturated" || resp.Header.Get("Retry-After") != "10" {
		t.Fatalf("got %d %v", resp.StatusCode, resp.Header)
	}
	// Without strict it shares.
	e.expect("session-b-cc-se", "echo.test:80", 200, "")
}

func TestConnectSessionExpired502(t *testing.T) {
	e := newTenv(t, 1)
	e.expect("session-a", "echo.test:80", 200, "")
	e.waitOpenTunnels(0)
	e.clock.Advance(61 * time.Second)
	e.p.WaitBackground()
	e.expect("session-a", "echo.test:80", 502, "session_expired")
}

func TestTargetRefusalKeepsSlot(t *testing.T) {
	e := newTenv(t, 1)
	before := len(e.prober.Calls())
	e.expect("session-a", "refused.test:80", 502, "dial_failed")
	if n := len(e.prober.Calls()); n != before {
		t.Fatalf("refusal triggered a probe (%d -> %d)", before, n)
	}
	// Same session, same slot, still usable.
	e.expect("session-a", "echo.test:80", 200, "")
	if st := e.p.Status(); st.Slots[0].Drain != "" || len(st.BadRelays) != 0 {
		t.Fatalf("slot discarded: %+v", st)
	}
}

func TestDialTimeoutProbesAndDrains(t *testing.T) {
	e := newTenv(t, 1)
	e.expect("session-a", "echo.test:80", 200, "")
	relay := e.p.Status().Slots[0].Relay

	// Probe passes: slot kept.
	before := len(e.prober.Calls())
	e.expect("session-a", "slow.test:80", 502, "dial_timeout")
	if len(e.prober.Calls()) != before+1 {
		t.Fatal("dial timeout did not probe the slot")
	}
	e.expect("session-a", "echo.test:80", 200, "")

	// Probe fails: slot drained, session gets 502.
	e.prober.SetFail(relay, true)
	e.expect("session-a", "slow.test:80", 502, "dial_timeout")
	e.expect("session-a", "echo.test:80", 502, "slot_drained")
	e.waitOpenTunnels(0)
	e.p.WaitBackground()
	// Repointed to another relay, new sessions work.
	if s := e.p.Status().Slots[0]; s.Relay == relay || s.State != "ready" {
		t.Fatalf("slot not repointed: %+v", s)
	}
	e.expect("session-b", "echo.test:80", 200, "")
}
