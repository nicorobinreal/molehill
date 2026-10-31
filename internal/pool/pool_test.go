package pool_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/pool/pooltest"
	"github.com/nicorobinreal/molehill/internal/relays"
)

type env struct {
	t      *testing.T
	p      *pool.Pool
	net    *pooltest.Network
	prober *pooltest.Prober
	clock  *pooltest.Clock
}

func newEnv(t *testing.T, seed uint64, slotsPerDevice int, rl []relays.Relay, devices ...string) *env {
	t.Helper()
	e := &env{t: t, net: &pooltest.Network{}, prober: &pooltest.Prober{}, clock: pooltest.NewClock()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e.p = pool.New(pool.Config{SlotsPerDevice: slotsPerDevice}, e.net, e.prober, e.clock, rand.New(rand.NewPCG(seed, 1)), log)
	e.p.SetRelays(rl)
	for _, d := range devices {
		e.addDevice(d)
	}
	return e
}

func (e *env) addDevice(id string) {
	e.t.Helper()
	if err := e.p.AddDevice(context.Background(), pool.Device{ID: id, PrivateKey: "k", IPv4: "10.0.0.1"}); err != nil {
		e.t.Fatal(err)
	}
	e.p.WaitBackground()
}

func (e *env) acquire(session, cc, host string, strict bool) (*pool.Lease, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := e.p.Acquire(ctx, pool.Request{Session: session, Country: cc, Host: host, Strict: strict})
	e.p.WaitBackground()
	return l, err
}

func (e *env) mustAcquire(session, cc, host string) *pool.Lease {
	e.t.Helper()
	l, err := e.acquire(session, cc, host, false)
	if err != nil {
		e.t.Fatalf("acquire %s: %v", session, err)
	}
	return l
}

// checkInvariants: relays unique across all slots, and every ready slot's
// tunnel really points at the relay the pool thinks it does.
func (e *env) checkInvariants() {
	e.t.Helper()
	seen := map[string]string{}
	for _, s := range e.p.Status().Slots {
		if s.Relay == "" {
			continue
		}
		if other, dup := seen[s.Relay]; dup {
			e.t.Fatalf("relay %s used by slots %s and %s", s.Relay, other, s.ID)
		}
		seen[s.Relay] = s.ID
	}
	for _, s := range e.p.Status().Slots {
		if s.State != "ready" {
			continue
		}
		for _, tun := range e.net.Tunnels {
			if tun.Name() == s.Interface && tun.Relay() != s.Relay {
				e.t.Fatalf("slot %s thinks %s, tunnel points at %s", s.ID, s.Relay, tun.Relay())
			}
		}
	}
}

func (e *env) slot(id string) pool.SlotStatus {
	for _, s := range e.p.Status().Slots {
		if s.ID == id {
			return s
		}
	}
	e.t.Fatalf("no slot %s", id)
	return pool.SlotStatus{}
}

func TestStartupPointsEverySlotAtUniqueProbedRelay(t *testing.T) {
	e := newEnv(t, 1, 5, pooltest.Relays(map[string]int{"se": 3, "de": 3}), "d1")
	st := e.p.Status()
	if len(st.Slots) != 5 {
		t.Fatalf("slots = %d", len(st.Slots))
	}
	for _, s := range st.Slots {
		if s.State != "ready" {
			t.Fatalf("slot %s state %s", s.ID, s.State)
		}
	}
	if n := len(e.prober.Calls()); n != 5 {
		t.Fatalf("probes = %d, want 5", n)
	}
	e.checkInvariants()
}

func TestUniqueRelaysAcrossDevicesAndMoreSlotsThanRelays(t *testing.T) {
	e := newEnv(t, 2, 3, pooltest.Relays(map[string]int{"se": 4}), "d1", "d2")
	ready := 0
	for _, s := range e.p.Status().Slots {
		if s.State == "ready" {
			ready++
		}
	}
	if ready != 4 {
		t.Fatalf("ready slots = %d, want 4 (one per relay)", ready)
	}
	e.checkInvariants()
}

func TestStickinessAlwaysWins(t *testing.T) {
	e := newEnv(t, 3, 3, pooltest.Relays(map[string]int{"se": 3, "de": 3}), "d1")
	l := e.mustAcquire("a", "se", "h")
	first := l.Relay().Hostname
	l.Release()
	e.clock.Advance(30 * time.Second)
	// Different country and host: stickiness still wins.
	l2 := e.mustAcquire("a", "de", "other")
	if l2.Relay().Hostname != first || l2.Slot() != l.Slot() {
		t.Fatalf("session moved: %s -> %s", first, l2.Relay().Hostname)
	}
	// Two tunnels at once on the same binding.
	l3 := e.mustAcquire("a", "se", "h")
	if l3.Slot() != l.Slot() {
		t.Fatal("second tunnel on another slot")
	}
	l2.Release()
	l3.Release()
}

func TestFreeSlotFirstGivesOwnIP(t *testing.T) {
	e := newEnv(t, 4, 3, pooltest.Relays(map[string]int{"se": 5}), "d1")
	got := map[string]bool{}
	for i := range 3 {
		l := e.mustAcquire(fmt.Sprintf("s%d", i), "se", "h")
		if got[l.Slot()] {
			t.Fatalf("session %d shares slot %s while free slots exist", i, l.Slot())
		}
		got[l.Slot()] = true
	}
	e.checkInvariants()
}

func TestFreeSlotRepointedToRequestedCountryWithRandomUnusedRelay(t *testing.T) {
	picked := map[string]bool{}
	for seed := range uint64(30) {
		// One slot; startup points it at any country. The request wants "de".
		e := newEnv(t, seed, 2, pooltest.Relays(map[string]int{"se": 2, "de": 4}), "d1")
		l := e.mustAcquire("a", "de", "h")
		r := l.Relay()
		if r.CountryCode != "de" {
			t.Fatalf("seed %d: relay %s not in de", seed, r.Hostname)
		}
		e.checkInvariants()
		picked[r.Hostname] = true
	}
	if len(picked) < 3 {
		t.Fatalf("relay choice not random enough: %v", picked)
	}
}

func TestRepointSkipsBadAndUsedRelays(t *testing.T) {
	e := newEnv(t, 5, 0, nil)
	e.p = pool.New(pool.Config{SlotsPerDevice: 2}, e.net, e.prober, e.clock, rand.New(rand.NewPCG(5, 1)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 2}))
	e.addDevice("d1") // both slots on se
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 2, "de": 3}))
	e.prober.SetFail("de-1", true)
	e.prober.SetFail("de-2", true)
	l := e.mustAcquire("a", "de", "h")
	if l.Relay().Hostname != "de-3" {
		t.Fatalf("got %s, want de-3", l.Relay().Hostname)
	}
	bad := e.p.Status().BadRelays
	for _, h := range []string{"de-1", "de-2"} {
		if _, ok := bad[h]; !ok && contains(e.prober.Calls(), h) {
			t.Fatalf("%s probed and failed but not marked bad: %v", h, bad)
		}
	}
	// The second slot cannot use de-3 (unique relays) and the others are bad.
	if _, err := e.acquire("b", "de", "h", true); !errors.Is(err, pool.ErrHostSaturated) {
		t.Fatalf("err = %v, want host_saturated", err)
	}
	e.checkInvariants()
}

func TestShareFewestSessionsForHost(t *testing.T) {
	e := newEnv(t, 6, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	a := e.mustAcquire("a", "se", "h")
	b := e.mustAcquire("b", "se", "g")
	if a.Slot() == b.Slot() {
		t.Fatal("a and b share while a slot was free")
	}
	// No free slot. Both slots have 1 session, but only b's slot has none for h.
	c := e.mustAcquire("c", "se", "h")
	if c.Slot() != b.Slot() {
		t.Fatalf("c on %s, want %s (fewest sessions for host h)", c.Slot(), b.Slot())
	}
	// Now h has 1 session on each slot; g has 1 on b's slot only.
	d := e.mustAcquire("d", "se", "g")
	if d.Slot() != a.Slot() {
		t.Fatalf("d on %s, want %s", d.Slot(), a.Slot())
	}
}

func TestShareRandomTies(t *testing.T) {
	counts := map[string]int{}
	for seed := range uint64(40) {
		e := newEnv(t, seed, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
		a := e.mustAcquire("a", "se", "h")
		e.mustAcquire("b", "se", "h")
		c := e.mustAcquire("c", "se", "h")
		if c.Slot() == a.Slot() {
			counts["a"]++
		} else {
			counts["b"]++
		}
	}
	if counts["a"] == 0 || counts["b"] == 0 {
		t.Fatalf("ties not broken randomly: %v", counts)
	}
}

func TestShareOnlyInRequestedCountry(t *testing.T) {
	e := newEnv(t, 7, 2, pooltest.Relays(map[string]int{"se": 2, "de": 2}), "d1")
	a := e.mustAcquire("a", "se", "h")
	e.mustAcquire("b", "de", "h")
	c := e.mustAcquire("c", "se", "x")
	if c.Slot() != a.Slot() {
		t.Fatal("c shared a slot in another country")
	}
	// No country: any slot may be shared.
	if _, err := e.acquire("d", "", "x", false); err != nil {
		t.Fatal(err)
	}
}

func TestStrictHostSaturated(t *testing.T) {
	e := newEnv(t, 8, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	e.mustAcquire("a", "se", "h")
	e.mustAcquire("b", "se", "h")
	if _, err := e.acquire("c", "se", "h", true); !errors.Is(err, pool.ErrHostSaturated) {
		t.Fatalf("err = %v, want host_saturated", err)
	}
	// Another host still fits without sharing an IP with itself.
	if _, err := e.acquire("d", "se", "g", true); err != nil {
		t.Fatalf("strict for another host: %v", err)
	}
	// Non-strict shares.
	if _, err := e.acquire("e", "se", "h", false); err != nil {
		t.Fatal(err)
	}
	// strict with a free slot available is fine.
	e2 := newEnv(t, 8, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	e2.mustAcquire("a", "se", "h")
	if _, err := e2.acquire("b", "se", "h", true); err != nil {
		t.Fatal(err)
	}
}

func TestStrictIgnoresIdleSessions(t *testing.T) {
	e := newEnv(t, 8, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	a := e.mustAcquire("a", "se", "h")
	b := e.mustAcquire("b", "se", "h")
	a.Release() // a idles in its grace period, e.g. a client that stopped using a session
	l, err := e.acquire("c", "se", "h", true)
	if err != nil {
		t.Fatalf("strict with an idle session for the host: %v", err)
	}
	if l.Slot() == b.Slot() {
		t.Fatalf("c shares b's slot %s, which has an active session for h", b.Slot())
	}
	// a keeps its binding.
	if l2, err := e.acquire("a", "se", "h", false); err != nil || l2.Slot() != l.Slot() {
		t.Fatalf("a after idling: slot %v, err %v; want %s", l2, err, l.Slot())
	}
	// Both slots now have an active session for h.
	if _, err := e.acquire("d", "se", "h", true); !errors.Is(err, pool.ErrHostSaturated) {
		t.Fatalf("err = %v, want host_saturated", err)
	}
}

func TestNoSlotNoDevices(t *testing.T) {
	e := newEnv(t, 9, 5, pooltest.Relays(map[string]int{"se": 4}))
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoSlotEverySlotBusyInOtherCountry(t *testing.T) {
	e := newEnv(t, 10, 2, pooltest.Relays(map[string]int{"se": 2, "de": 2}), "d1")
	e.mustAcquire("a", "se", "h")
	e.mustAcquire("b", "se", "h")
	if _, err := e.acquire("c", "de", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
}

func TestNoSlotWhenEveryCandidateRelayFailsProbe(t *testing.T) {
	e := newEnv(t, 11, 2, pooltest.Relays(map[string]int{"se": 2}), "d1")
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 2, "de": 5}))
	e.prober.SetFailAll(true)
	before := len(e.prober.Calls())
	if _, err := e.acquire("a", "de", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
	// Each slot tried at most 3 relays.
	if n := len(e.prober.Calls()) - before; n == 0 || n > 6 {
		t.Fatalf("probes = %d, want 1..6", n)
	}
	if len(e.p.Status().BadRelays) == 0 {
		t.Fatal("no relay marked bad")
	}
	// Given-up slots are skipped until SlotRetry, so this fails fast.
	before = len(e.prober.Calls())
	if _, err := e.acquire("b", "de", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
	if n := len(e.prober.Calls()) - before; n != 0 {
		t.Fatalf("retried given-up slots immediately (%d probes)", n)
	}
	// After the retry delay and with relays healthy again, it works.
	e.prober.SetFailAll(false)
	e.clock.Advance(11 * time.Minute) // past SlotRetry and BadRelayTTL
	if _, err := e.acquire("c", "de", "h", false); err != nil {
		t.Fatal(err)
	}
}

func TestNoSlotWhenRemainingSlotsDraining(t *testing.T) {
	e := newEnv(t, 12, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	l := e.mustAcquire("a", "se", "h")
	if err := e.p.RemoveDevice("d1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.acquire("b", "se", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
	l.Release()
}

func TestUnknownCountry(t *testing.T) {
	e := newEnv(t, 13, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	if _, err := e.acquire("a", "zz", "h", false); !errors.Is(err, pool.ErrUnknownCountry) {
		t.Fatalf("err = %v", err)
	}
}

func TestGracePeriodThen502(t *testing.T) {
	e := newEnv(t, 14, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	l := e.mustAcquire("a", "se", "h")
	// An open tunnel keeps the binding however long it lasts.
	e.clock.Advance(10 * time.Minute)
	l.Release()
	e.clock.Advance(59 * time.Second)
	l2 := e.mustAcquire("a", "se", "h")
	if l2.Slot() != l.Slot() {
		t.Fatal("binding lost within grace")
	}
	l2.Release()
	l2.Release() // idempotent
	e.clock.Advance(60 * time.Second)
	e.p.WaitBackground()
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrSessionExpired) {
		t.Fatalf("err = %v, want session expired", err)
	}
	// Still 502 a while later: never silently moved to a new IP.
	e.clock.Advance(30 * time.Minute)
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrSessionExpired) {
		t.Fatalf("err = %v, want session expired", err)
	}
}

func TestGraceRestartsFromLastTunnelClose(t *testing.T) {
	e := newEnv(t, 15, 2, pooltest.Relays(map[string]int{"se": 4}), "d1")
	l1 := e.mustAcquire("a", "se", "h")
	l2 := e.mustAcquire("a", "se", "h")
	l1.Release()
	e.clock.Advance(50 * time.Second)
	l2.Release()
	e.clock.Advance(50 * time.Second) // 100s after the first close, 50s after the last
	l3 := e.mustAcquire("a", "se", "h")
	l3.Release()
	// Timer stopped by l3 must not expire the binding early.
	e.clock.Advance(30 * time.Second)
	l4 := e.mustAcquire("a", "se", "h")
	l4.Release()
}

func TestFreedSlotRepointsAndProbesInBackground(t *testing.T) {
	e := newEnv(t, 16, 1, pooltest.Relays(map[string]int{"se": 4, "de": 4}), "d1")
	l := e.mustAcquire("a", "se", "h")
	old := l.Relay().Hostname
	l.Release()
	probes := len(e.prober.Calls())
	e.clock.Advance(61 * time.Second)
	e.p.WaitBackground()
	s := e.p.Status().Slots[0]
	if s.State != "ready" || s.Relay == old {
		t.Fatalf("slot not rotated: %+v (old %s)", s, old)
	}
	if s.Country != "se" {
		t.Fatalf("rotated to %s, want the last session's country se", s.Country)
	}
	if len(e.prober.Calls()) != probes+1 {
		t.Fatal("new relay not probed in background")
	}
	// The next new session uses the pre-probed slot without another probe.
	l2 := e.mustAcquire("b", "se", "h")
	if l2.Relay().Hostname != s.Relay || len(e.prober.Calls()) != probes+1 {
		t.Fatal("next session did not get the pre-probed relay")
	}
	e.checkInvariants()
}

func TestDrainOnDeviceRemoval(t *testing.T) {
	e := newEnv(t, 17, 2, pooltest.Relays(map[string]int{"se": 4}), "d1", "d2")
	l := e.mustAcquire("a", "se", "h")
	dev := e.slot(l.Slot()).Device
	if err := e.p.RemoveDevice(dev); err != nil {
		t.Fatal(err)
	}
	e.p.WaitBackground()
	// Existing session: 502 on its next connect.
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrSlotDrained) {
		t.Fatalf("err = %v, want slot drained", err)
	}
	// New sessions land on the other device.
	nl := e.mustAcquire("b", "se", "h")
	if e.slot(nl.Slot()).Device == dev {
		t.Fatal("new session on draining device")
	}
	// The open tunnel keeps its interface until it closes.
	var tun *pooltest.Tunnel
	for _, tt := range e.net.Tunnels {
		if tt.Device == dev && !tt.Closed() {
			tun = tt
		}
	}
	if tun == nil {
		t.Fatal("interface torn down while a tunnel was open")
	}
	l.Release()
	e.p.WaitBackground()
	for _, tt := range e.net.Tunnels {
		if tt.Device == dev && !tt.Closed() {
			t.Fatal("interface not torn down after drain")
		}
	}
	for _, d := range e.p.Devices() {
		if d.ID == dev {
			t.Fatal("device still listed")
		}
	}
	if err := e.p.RemoveDevice(dev); !errors.Is(err, pool.ErrDeviceNotFound) {
		t.Fatalf("err = %v", err)
	}
	e.addDevice(dev) // can be re-added
	e.checkInvariants()
}

func TestDeviceRemovalWhileProbing(t *testing.T) {
	e := newEnv(t, 18, 2, pooltest.Relays(map[string]int{"se": 4}))
	if err := e.p.AddDevice(context.Background(), pool.Device{ID: "d1"}); err != nil {
		t.Fatal(err)
	}
	_ = e.p.RemoveDevice("d1")
	e.p.WaitBackground()
	if len(e.p.Devices()) != 0 || len(e.p.Status().Slots) != 0 {
		t.Fatalf("device not removed: %+v", e.p.Status())
	}
}

func TestDuplicateDevice(t *testing.T) {
	e := newEnv(t, 19, 1, pooltest.Relays(map[string]int{"se": 4}), "d1")
	if err := e.p.AddDevice(context.Background(), pool.Device{ID: "d1"}); !errors.Is(err, pool.ErrDeviceExists) {
		t.Fatalf("err = %v", err)
	}
}

func TestDialTimeoutFailedProbeDrainsSlot(t *testing.T) {
	e := newEnv(t, 20, 1, pooltest.Relays(map[string]int{"se": 4}), "d1")
	l := e.mustAcquire("a", "se", "h")
	other := e.mustAcquire("b", "se", "h") // shares the only slot
	bad := l.Relay().Hostname
	e.prober.SetFail(bad, true)
	l.DialTimedOut()
	e.p.WaitBackground()
	if _, ok := e.p.Status().BadRelays[bad]; !ok {
		t.Fatal("relay not marked bad")
	}
	for _, s := range []string{"a", "b"} {
		if _, err := e.acquire(s, "se", "h", false); !errors.Is(err, pool.ErrSlotDrained) {
			t.Fatalf("%s: err = %v, want slot drained", s, err)
		}
	}
	if _, err := e.acquire("c", "se", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("new session on draining slot: %v", err)
	}
	// Once its tunnels close it is repointed at a fresh relay.
	l.Release()
	other.Release()
	e.p.WaitBackground()
	s := e.p.Status().Slots[0]
	if s.State != "ready" || s.Relay == bad || s.Drain != "" {
		t.Fatalf("slot not recovered: %+v", s)
	}
	if _, err := e.acquire("d", "se", "h", false); err != nil {
		t.Fatal(err)
	}
}

func TestDialTimeoutPassingProbeKeepsSlot(t *testing.T) {
	e := newEnv(t, 21, 1, pooltest.Relays(map[string]int{"se": 4}), "d1")
	l := e.mustAcquire("a", "se", "h")
	l.DialTimedOut()
	e.p.WaitBackground()
	l.Release()
	if _, err := e.acquire("a", "se", "h", false); err != nil {
		t.Fatalf("slot discarded after a passing probe: %v", err)
	}
	if len(e.p.Status().BadRelays) != 0 {
		t.Fatal("relay marked bad")
	}
}

func TestSlotRetryAfterGivingUp(t *testing.T) {
	e := newEnv(t, 22, 1, pooltest.Relays(map[string]int{"se": 1}), "d1")
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 1, "de": 1}))
	// The only de relay fails; the slot gives up and is skipped for SlotRetry.
	e.prober.SetFail("de-1", true)
	if _, err := e.acquire("a", "de", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
	s := e.p.Status().Slots[0]
	if s.State != "idle" || s.RetryAt == nil {
		t.Fatalf("slot = %+v", s)
	}
}

func TestNoRelayListYetThenArrives(t *testing.T) {
	e := newEnv(t, 23, 2, nil, "d1")
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v", err)
	}
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 3}))
	e.p.WaitBackground()
	if _, err := e.acquire("b", "se", "h", false); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireWaitsForBackgroundProbeAndCancel(t *testing.T) {
	block := make(chan struct{})
	e := &env{t: t, net: &pooltest.Network{}, prober: &pooltest.Prober{}, clock: pooltest.NewClock()}
	bp := &blockingProber{inner: e.prober, block: block}
	e.p = pool.New(pool.Config{SlotsPerDevice: 1}, e.net, bp, e.clock, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 3}))
	if err := e.p.AddDevice(context.Background(), pool.Device{ID: "d1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.p.Acquire(ctx, pool.Request{Session: "a", Country: "se"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	done := make(chan error, 1)
	go func() {
		l, err := e.p.Acquire(context.Background(), pool.Request{Session: "b", Country: "se"})
		if l != nil {
			l.Release()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	close(block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	e.p.WaitBackground()
}

type blockingProber struct {
	inner pool.Prober
	block chan struct{}
}

func (b *blockingProber) Probe(ctx context.Context, t pool.Tunnel, r relays.Relay) error {
	<-b.block
	return b.inner.Probe(ctx, t, r)
}

func TestConcurrentSessions(t *testing.T) {
	e := newEnv(t, 24, 5, pooltest.Relays(map[string]int{"se": 8, "de": 8}), "d1", "d2")
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Go(func() {
			cc := []string{"se", "de", ""}[i%3]
			for j := range 20 {
				l, err := e.p.Acquire(context.Background(), pool.Request{
					Session: fmt.Sprintf("s%d", i), Country: cc, Host: fmt.Sprintf("h%d", j%3), Strict: i%2 == 0,
				})
				if err == nil {
					l.Release()
				}
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			e.clock.Advance(20 * time.Second)
			time.Sleep(time.Millisecond)
		}
	})
	wg.Wait()
	e.p.WaitBackground()
	e.checkInvariants()
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestFreeSlotKeepsRelayWhenRequestedCountryHasNoUsableRelay(t *testing.T) {
	e := newEnv(t, 25, 2, pooltest.Relays(map[string]int{"se": 2}), "d1")
	e.p.SetRelays(pooltest.Relays(map[string]int{"se": 2, "de": 1}))
	a := e.mustAcquire("a", "de", "h")
	if a.Relay().Hostname != "de-1" {
		t.Fatalf("a on %s", a.Relay().Hostname)
	}
	probes := len(e.prober.Calls())
	// The only de relay is taken: b shares a's slot instead of throwing away
	// the free slot's working se relay on a repoint that cannot succeed.
	b := e.mustAcquire("b", "de", "g")
	if b.Slot() != a.Slot() {
		t.Fatal("b not shared")
	}
	if len(e.prober.Calls()) != probes {
		t.Fatal("pointless repoint attempted")
	}
	for _, s := range e.p.Status().Slots {
		if s.ID != a.Slot() && (s.State != "ready" || s.Country != "se") {
			t.Fatalf("free se slot disturbed: %+v", s)
		}
	}
	if l, err := e.acquire("c", "se", "h", false); err != nil || l.Relay().CountryCode != "se" {
		t.Fatalf("se: %v", err)
	}
}

type gateProber struct {
	inner *pooltest.Prober
	mu    sync.Mutex
	gate  chan struct{}
}

func (g *gateProber) Probe(ctx context.Context, t pool.Tunnel, r relays.Relay) error {
	g.mu.Lock()
	gate := g.gate
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	return g.inner.Probe(ctx, t, r)
}

func TestGraceExpiryDuringDialTimeoutCheck(t *testing.T) {
	for _, fail := range []bool{false, true} {
		e := &env{t: t, net: &pooltest.Network{}, prober: &pooltest.Prober{}, clock: pooltest.NewClock()}
		gp := &gateProber{inner: e.prober}
		e.p = pool.New(pool.Config{SlotsPerDevice: 1}, e.net, gp, e.clock, rand.New(rand.NewPCG(3, 3)), slog.New(slog.NewTextHandler(io.Discard, nil)))
		e.p.SetRelays(pooltest.Relays(map[string]int{"se": 3}))
		e.addDevice("d1")
		l := e.mustAcquire("a", "se", "h")
		old := l.Relay().Hostname
		gp.mu.Lock()
		gp.gate = make(chan struct{})
		gate := gp.gate
		gp.mu.Unlock()
		e.prober.SetFail(old, fail)
		l.DialTimedOut()
		l.Release()
		e.clock.Advance(61 * time.Second) // binding expires while the check runs
		if s := e.p.Status().Slots[0]; s.State != "ready" || s.Relay != old {
			t.Fatalf("fail=%v: slot repointed during the check: %+v", fail, s)
		}
		gp.mu.Lock()
		gp.gate = nil
		gp.mu.Unlock()
		close(gate)
		e.p.WaitBackground()
		s := e.p.Status().Slots[0]
		if s.State != "ready" || s.Relay == old || s.Drain != "" {
			t.Fatalf("fail=%v: slot not rotated after the check: %+v", fail, s)
		}
		if _, bad := e.p.Status().BadRelays[old]; bad != fail {
			t.Fatalf("fail=%v: bad = %v", fail, bad)
		}
	}
}

func slotCount(e *env) int { return len(e.p.Status().Slots) }

func TestGrowsSlotsOnDemandUntilRelaysRunOut(t *testing.T) {
	e := newEnv(t, 11, 0, pooltest.Relays(map[string]int{"se": 3}), "d1")
	if n := slotCount(e); n != 1 {
		t.Fatalf("slots at start = %d, want 1", n)
	}
	seen := map[string]bool{}
	for _, s := range []string{"a", "b", "c"} {
		l := e.mustAcquire(s, "se", "h")
		if seen[l.Slot()] {
			t.Fatalf("session %s shares %s while relays are left", s, l.Slot())
		}
		seen[l.Slot()] = true
	}
	if n := slotCount(e); n != 3 {
		t.Fatalf("slots = %d, want 3", n)
	}
	// Every se relay is in use: the next session shares instead of opening a slot.
	l := e.mustAcquire("d", "se", "h")
	if !seen[l.Slot()] || slotCount(e) != 3 {
		t.Fatalf("d got %s with %d slots, want a shared slot", l.Slot(), slotCount(e))
	}
	e.checkInvariants()
}

func TestGrowRespectsSlotsPerDevice(t *testing.T) {
	e := newEnv(t, 12, 2, pooltest.Relays(map[string]int{"se": 5}), "d1")
	for _, s := range []string{"a", "b", "c", "d"} {
		e.mustAcquire(s, "se", "h")
	}
	if n := slotCount(e); n != 2 {
		t.Fatalf("slots = %d, want 2", n)
	}
}

func TestGrowSpreadsAcrossDevices(t *testing.T) {
	e := newEnv(t, 13, 0, pooltest.Relays(map[string]int{"se": 6}), "d1", "d2")
	for _, s := range []string{"a", "b", "c", "d"} {
		e.mustAcquire(s, "se", "h")
	}
	for _, d := range e.p.Devices() {
		if d.Slots != 2 {
			t.Fatalf("device %s has %d slots, want 2", d.ID, d.Slots)
		}
	}
}

func TestGrowOpenFailureBacksOff(t *testing.T) {
	e := newEnv(t, 14, 0, pooltest.Relays(map[string]int{"se": 3}), "d1")
	a := e.mustAcquire("a", "se", "h")
	e.net.OpenErr = errors.New("ip link: boom")
	if l := e.mustAcquire("b", "se", "h"); l.Slot() != a.Slot() {
		t.Fatalf("b got %s, want to share %s", l.Slot(), a.Slot())
	}
	e.net.OpenErr = nil
	// Within the backoff no new slot is tried.
	if l := e.mustAcquire("c", "se", "h"); l.Slot() != a.Slot() || slotCount(e) != 1 {
		t.Fatalf("c got %s with %d slots during backoff", l.Slot(), slotCount(e))
	}
	e.clock.Advance(31 * time.Second)
	if l := e.mustAcquire("d", "se", "h"); l.Slot() == a.Slot() || slotCount(e) != 2 {
		t.Fatalf("d got %s with %d slots after backoff, want a new slot", l.Slot(), slotCount(e))
	}
}

func TestGrowNoSlotWhenNoDevices(t *testing.T) {
	e := newEnv(t, 15, 0, pooltest.Relays(map[string]int{"se": 3}))
	if _, err := e.acquire("a", "se", "h", false); !errors.Is(err, pool.ErrNoSlot) {
		t.Fatalf("err = %v, want no_slot", err)
	}
}

func TestStrictGrowsInsteadOfSaturating(t *testing.T) {
	e := newEnv(t, 16, 0, pooltest.Relays(map[string]int{"se": 2}), "d1")
	a := e.mustAcquire("a", "se", "h") // tunnel stays open: active for h
	l, err := e.acquire("b", "se", "h", true)
	if err != nil || l.Slot() == a.Slot() {
		t.Fatalf("strict b: %v, slot %v", err, l)
	}
	// No relay left and both slots active for h.
	if _, err := e.acquire("c", "se", "h", true); !errors.Is(err, pool.ErrHostSaturated) {
		t.Fatalf("strict c: err = %v, want host_saturated", err)
	}
}

func TestRemoveDeviceTearsDownGrownSlots(t *testing.T) {
	e := newEnv(t, 17, 0, pooltest.Relays(map[string]int{"se": 3}), "d1")
	a, b := e.mustAcquire("a", "se", "h"), e.mustAcquire("b", "se", "h")
	if err := e.p.RemoveDevice("d1"); err != nil {
		t.Fatal(err)
	}
	a.Release()
	b.Release()
	e.p.WaitBackground()
	if len(e.p.Devices()) != 0 || slotCount(e) != 0 {
		t.Fatalf("devices %v, slots %d", e.p.Devices(), slotCount(e))
	}
	for _, tun := range e.net.Tunnels {
		if !tun.Closed() {
			t.Fatalf("tunnel %s left open", tun.Name())
		}
	}
}
