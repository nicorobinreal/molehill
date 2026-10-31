// Package pooltest has fakes for the pool's network, prober and clock.
package pooltest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/relays"
)

// Network hands out fake tunnels.
type Network struct {
	mu      sync.Mutex
	Tunnels []*Tunnel
	// Dial, if set, is used by every tunnel's DialContext.
	Dial    func(ctx context.Context, t *Tunnel, addr string) (net.Conn, error)
	OpenErr error
}

func (n *Network) Open(_ context.Context, d pool.Device, index int) (pool.Tunnel, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.OpenErr != nil {
		return nil, n.OpenErr
	}
	t := &Tunnel{name: fmt.Sprintf("fake%d", len(n.Tunnels)), Device: d.ID, net: n}
	n.Tunnels = append(n.Tunnels, t)
	return t, nil
}

// Tunnel records what the pool did with it.
type Tunnel struct {
	name   string
	Device string
	net    *Network

	mu     sync.Mutex
	relay  string
	points []string
	closed bool
}

func (t *Tunnel) Name() string { return t.name }

func (t *Tunnel) Point(_ context.Context, r relays.Relay) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errors.New("closed")
	}
	t.relay = r.Hostname
	t.points = append(t.points, r.Hostname)
	return nil
}

func (t *Tunnel) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	if t.net.Dial != nil {
		return t.net.Dial(ctx, t, addr)
	}
	a, b := net.Pipe()
	go func() { <-ctx.Done(); b.Close() }()
	return a, nil
}

func (t *Tunnel) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// Relay is the relay the tunnel currently points at.
func (t *Tunnel) Relay() string { t.mu.Lock(); defer t.mu.Unlock(); return t.relay }

// Points lists every relay the tunnel was pointed at.
func (t *Tunnel) Points() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.points...)
}

func (t *Tunnel) Closed() bool { t.mu.Lock(); defer t.mu.Unlock(); return t.closed }

// Prober fails relays listed in Fail.
type Prober struct {
	mu      sync.Mutex
	fail    map[string]bool
	calls   []string
	FailAll bool
}

func (p *Prober) SetFail(hostname string, fail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail == nil {
		p.fail = map[string]bool{}
	}
	p.fail[hostname] = fail
}

func (p *Prober) SetFailAll(v bool) { p.mu.Lock(); p.FailAll = v; p.mu.Unlock() }

func (p *Prober) Probe(_ context.Context, t pool.Tunnel, r relays.Relay) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, r.Hostname)
	if p.FailAll || p.fail[r.Hostname] {
		return errors.New("probe failed")
	}
	if ft, ok := t.(*Tunnel); ok && ft.Relay() != r.Hostname {
		return fmt.Errorf("tunnel points at %s, not %s", ft.Relay(), r.Hostname)
	}
	return nil
}

// Calls lists every probed relay.
func (p *Prober) Calls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// Clock is a manual clock.
type Clock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*timer
}

type timer struct {
	at      time.Time
	f       func()
	stopped bool
	fired   bool
}

func NewClock() *Clock { return &Clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *Clock) AfterFunc(d time.Duration, f func()) pool.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &timer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return &stopper{c: c, t: t}
}

type stopper struct {
	c *Clock
	t *timer
}

func (s *stopper) Stop() bool {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.t.fired || s.t.stopped {
		return false
	}
	s.t.stopped = true
	return true
}

// Advance moves time forward and runs due timers in order (synchronously).
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var due []*timer
		for _, t := range c.timers {
			if !t.fired && !t.stopped && !t.at.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			break
		}
		sort.Slice(due, func(i, j int) bool { return due[i].at.Before(due[j].at) })
		t := due[0]
		t.fired = true
		if t.at.After(c.now) {
			c.now = t.at
		}
		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// Relays builds relays named "<cc>-<n>" for each country.
func Relays(perCountry map[string]int) []relays.Relay {
	var out []relays.Relay
	ccs := make([]string, 0, len(perCountry))
	for cc := range perCountry {
		ccs = append(ccs, cc)
	}
	sort.Strings(ccs)
	i := 0
	for _, cc := range ccs {
		for n := 1; n <= perCountry[cc]; n++ {
			i++
			out = append(out, relays.Relay{
				Hostname: fmt.Sprintf("%s-%d", cc, n), CountryCode: cc, Active: true, Type: "wireguard",
				IPv4: fmt.Sprintf("192.0.2.%d", i), PublicKey: fmt.Sprintf("key%d", i),
			})
		}
	}
	return out
}
