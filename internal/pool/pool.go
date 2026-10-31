package pool

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/nicorobinreal/molehill/internal/relays"
)

type slotState int

const (
	stateDown    slotState = iota // interface not up
	stateIdle                     // up, no verified relay (never pointed, or the last repoint gave up)
	stateProbing                  // repoint in progress
	stateReady                    // pointed at a relay that passed the probe
)

func (s slotState) String() string {
	return [...]string{"down", "idle", "probing", "ready"}[s]
}

type drainReason int

const (
	drainNone    drainReason = iota
	drainRemoved             // device removed: tear down once tunnels are closed
	drainFailed              // probe failed after a dial timeout: repoint once tunnels are closed
)

func (d drainReason) String() string {
	return [...]string{"", "device_removed", "probe_failed"}[d]
}

type device struct {
	Device
	slots       []*slot
	draining    bool
	next        int       // index of the next slot opened
	growRetryAt time.Time // no new slots until then (the last open failed)
}

type slot struct {
	id        string
	index     int
	dev       *device
	tun       Tunnel
	state     slotState
	relay     *relays.Relay // current relay, or the one being probed
	probeCC   string        // country the running repoint is for ("" = any)
	probeDone chan struct{} // closed when the running repoint ends
	claim     string        // session waiting for this slot's repoint
	drain     drainReason
	bindings  map[string]*binding
	open      int       // open tunnels, including ones of dropped bindings
	retryAt   time.Time // idle slot whose repoint gave up is skipped until then
	lastCC    string    // country of the last session bound here, used for the next repoint
	checking  bool      // probe after a dial timeout in flight
}

type binding struct {
	session string
	slot    *slot
	cc      string
	hosts   map[string]struct{}
	open    int
	timer   Timer
	gen     int // invalidates timers that fired while being stopped
}

type tomb struct {
	err   error
	until time.Time
}

// Pool owns all slots and bindings. All state is in memory.
type Pool struct {
	cfg    Config
	net    Network
	prober Prober
	clock  Clock
	rnd    Rand
	log    *slog.Logger

	mu        sync.Mutex
	relays    []relays.Relay
	byCountry map[string][]relays.Relay
	devices   map[string]*device
	order     []string // device IDs in insertion order
	slots     []*slot
	bindings  map[string]*binding
	tombs     map[string]tomb
	bad       map[string]time.Time // relay hostname -> bad until
	nextSweep time.Time

	bg sync.WaitGroup
}

// New creates an empty pool. clock and rnd may be nil for the real ones.
func New(cfg Config, nw Network, pr Prober, clock Clock, rnd Rand, log *slog.Logger) *Pool {
	cfg.defaults()
	if clock == nil {
		clock = RealClock
	}
	if rnd == nil {
		rnd = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if log == nil {
		log = slog.Default()
	}
	return &Pool{
		cfg: cfg, net: nw, prober: pr, clock: clock, rnd: rnd, log: log,
		byCountry: map[string][]relays.Relay{},
		devices:   map[string]*device{},
		bindings:  map[string]*binding{},
		tombs:     map[string]tomb{},
		bad:       map[string]time.Time{},
	}
}

// SetRelays replaces the relay list. Slots that were waiting for a list get
// pointed. Slots already on a relay that left the list keep it until freed.
func (p *Pool) SetRelays(list []relays.Relay) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.relays = slices.Clone(list)
	p.byCountry = map[string][]relays.Relay{}
	for _, r := range p.relays {
		p.byCountry[r.CountryCode] = append(p.byCountry[r.CountryCode], r)
	}
	for _, s := range p.slots {
		if s.state == stateIdle && s.relay == nil && s.drain == drainNone && len(s.bindings) == 0 && s.claim == "" {
			p.startRepoint(s, s.lastCC)
		}
	}
}

// HasCountry reports whether the relay list has relays in cc.
func (p *Pool) HasCountry(cc string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byCountry[cc]) > 0
}

// AddDevice opens the device's first slot interfaces (all of them with
// SlotsPerDevice set, else one; more are opened as sessions need them) and
// starts pointing them at relays.
func (p *Pool) AddDevice(ctx context.Context, d Device) error {
	p.mu.Lock()
	_, exists := p.devices[d.ID]
	p.mu.Unlock()
	if exists {
		return ErrDeviceExists
	}

	n := max(p.cfg.SlotsPerDevice, 1)
	tuns := make([]Tunnel, 0, n)
	closeAll := func() {
		for _, t := range tuns {
			_ = t.Close()
		}
	}
	for i := range n {
		t, err := p.net.Open(ctx, d, i)
		if err != nil {
			closeAll()
			return fmt.Errorf("device %s slot %d: %w", d.ID, i, err)
		}
		tuns = append(tuns, t)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.devices[d.ID]; ok {
		closeAll()
		return ErrDeviceExists
	}
	dev := &device{Device: d}
	for _, t := range tuns {
		p.addSlot(dev, t)
	}
	p.devices[d.ID] = dev
	p.order = append(p.order, d.ID)
	for _, s := range dev.slots {
		p.startRepoint(s, "")
	}
	p.log.Info("device added", "device", d.ID, "slots", len(tuns))
	return nil
}

// addSlot adds a slot to dev, idle. Requires the lock.
func (p *Pool) addSlot(dev *device, t Tunnel) *slot {
	i := dev.next
	dev.next++
	s := &slot{
		id: fmt.Sprintf("%s/%d", dev.ID, i), index: i, dev: dev, tun: t,
		state: stateIdle, bindings: map[string]*binding{},
	}
	dev.slots = append(dev.slots, s)
	p.slots = append(p.slots, s)
	return s
}

// grow opens a new slot for cc on the device with the fewest slots, if one
// has room and cc has a relay no slot uses. Requires the lock; the slot is
// probing (opening, then repointing) when it returns.
func (p *Pool) grow(cc string, now time.Time) *slot {
	if !p.canPick(nil, cc) {
		return nil
	}
	for _, s := range p.slots {
		// Its repoint just gave up on cc: a new slot would likely fail too.
		if s.state == stateIdle && now.Before(s.retryAt) && s.probeCC == cc {
			return nil
		}
	}
	var dev *device
	for _, id := range p.order {
		d := p.devices[id]
		if d.draining || now.Before(d.growRetryAt) ||
			(p.cfg.SlotsPerDevice > 0 && len(d.slots) >= p.cfg.SlotsPerDevice) {
			continue
		}
		if dev == nil || len(d.slots) < len(dev.slots) {
			dev = d
		}
	}
	if dev == nil {
		return nil
	}
	s := p.addSlot(dev, nil)
	s.state = stateProbing
	s.probeCC = cc
	done := make(chan struct{})
	s.probeDone = done
	p.bg.Go(func() { p.open(s, cc, done) })
	return s
}

// open brings up a slot's interface, then repoints it.
func (p *Pool) open(s *slot, cc string, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t, err := p.net.Open(ctx, s.dev.Device, s.index)
	cancel()
	p.mu.Lock()
	if err == nil && s.state == stateDown { // pool closed meanwhile
		p.mu.Unlock()
		_ = t.Close()
		close(done)
		return
	}
	if err != nil {
		p.log.Error("open slot", "slot", s.id, "err", err)
		s.dev.growRetryAt = p.clock.Now().Add(p.cfg.SlotRetry)
		s.probeDone = nil
		p.removeSlot(s)
		p.mu.Unlock()
		close(done)
		return
	}
	s.tun = t
	if s.drain != drainNone || len(p.relays) == 0 {
		s.state = stateIdle
		s.probeDone = nil
		p.maybeFinishDrain(s)
		p.mu.Unlock()
		close(done)
		return
	}
	p.mu.Unlock()
	p.repoint(s, cc, done)
}

// removeSlot forgets s, and its device once draining and empty. Requires the lock.
func (p *Pool) removeSlot(s *slot) {
	s.state = stateDown
	p.slots = slices.DeleteFunc(p.slots, func(x *slot) bool { return x == s })
	dev := s.dev
	dev.slots = slices.DeleteFunc(dev.slots, func(x *slot) bool { return x == s })
	if dev.draining && len(dev.slots) == 0 {
		delete(p.devices, dev.ID)
		p.order = slices.DeleteFunc(p.order, func(id string) bool { return id == dev.ID })
		p.log.Info("device removed", "device", dev.ID)
	}
}

// RemoveDevice drains the device: its slots take no new sessions, sessions
// bound to them get 502 on their next connect, open tunnels finish, then the
// interfaces are torn down.
func (p *Pool) RemoveDevice(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	dev, ok := p.devices[id]
	if !ok {
		return ErrDeviceNotFound
	}
	if dev.draining {
		return nil
	}
	dev.draining = true
	p.log.Info("device draining", "device", id)
	for _, s := range slices.Clone(dev.slots) {
		p.drainSlot(s, drainRemoved)
	}
	return nil
}

// Acquire binds the request's session to a slot (or reuses its binding) and
// returns a lease for one tunnel. The caller must Release it.
func (p *Pool) Acquire(ctx context.Context, req Request) (*Lease, error) {
	p.mu.Lock()
	for {
		now := p.clock.Now()
		p.sweep(now)

		// 1. Stickiness always wins.
		if b := p.bindings[req.Session]; b != nil {
			l := p.use(b, req.Host)
			p.mu.Unlock()
			return l, nil
		}
		// A dropped binding never silently moves to a new exit IP.
		if t, ok := p.tombs[req.Session]; ok {
			p.mu.Unlock()
			return nil, t.err
		}
		if req.Country != "" && len(p.relays) > 0 && len(p.byCountry[req.Country]) == 0 {
			p.mu.Unlock()
			return nil, ErrUnknownCountry
		}

		// A repoint this session asked for (or is waiting on) earlier.
		if c := p.claimOf(req.Session); c != nil {
			switch {
			case c.drain != drainNone:
				c.claim = ""
			case c.state == stateProbing:
				if err := p.wait(ctx, c, req.Session); err != nil {
					return nil, err
				}
				continue
			case c.state == stateReady && len(c.bindings) == 0 && matches(c.relay, req.Country):
				c.claim = ""
				l := p.bind(c, req)
				p.mu.Unlock()
				return l, nil
			default:
				c.claim = ""
			}
		}

		// 2.1 A free slot, or a new one.
		var ready, probingMatch, repointable, probingOther []*slot
		for _, s := range p.slots {
			if !p.free(s, req.Session) {
				continue
			}
			switch s.state {
			case stateReady:
				if matches(s.relay, req.Country) {
					ready = append(ready, s)
				} else if p.canPick(s, req.Country) {
					// Only give up a working relay when there is somewhere to go.
					repointable = append(repointable, s)
				}
			case stateProbing:
				if req.Country == "" || s.probeCC == req.Country {
					probingMatch = append(probingMatch, s)
				} else {
					probingOther = append(probingOther, s)
				}
			case stateIdle:
				if !now.Before(s.retryAt) && p.canPick(s, req.Country) {
					repointable = append(repointable, s)
				}
			}
		}
		if len(ready) > 0 {
			l := p.bind(p.pick(ready), req)
			p.mu.Unlock()
			return l, nil
		}
		if len(probingMatch) > 0 {
			s := p.pick(probingMatch)
			s.claim = req.Session
			if err := p.wait(ctx, s, req.Session); err != nil {
				return nil, err
			}
			continue
		}
		if len(repointable) > 0 {
			s := p.pick(repointable)
			s.claim = req.Session
			p.startRepoint(s, req.Country)
			if err := p.wait(ctx, s, req.Session); err != nil {
				return nil, err
			}
			continue
		}
		if len(probingOther) > 0 {
			// Becomes repointable once its probe ends.
			if err := p.wait(ctx, p.pick(probingOther), req.Session); err != nil {
				return nil, err
			}
			continue
		}
		if s := p.grow(req.Country, now); s != nil {
			s.claim = req.Session
			if err := p.wait(ctx, s, req.Session); err != nil {
				return nil, err
			}
			continue
		}

		// 2.2 Share a slot already in the right country.
		var cands []*slot
		for _, s := range p.slots {
			if s.drain == drainNone && s.state == stateReady && len(s.bindings) > 0 && matches(s.relay, req.Country) {
				cands = append(cands, s)
			}
		}
		if len(cands) == 0 {
			p.mu.Unlock()
			return nil, ErrNoSlot // 3. nothing can serve the request
		}
		// 2.3 strict: only slots without an active session (open tunnels) for
		// this host qualify. Idle sessions don't count: a client that stopped
		// using a session keeps its binding for the grace period, and counting
		// it would refuse its replacement. The cost: an idle session that
		// comes back may briefly share the IP with a newer one.
		if req.Strict {
			var free []*slot
			for _, s := range cands {
				if activeHostSessions(s, req.Host) == 0 {
					free = append(free, s)
				}
			}
			if len(free) == 0 {
				p.mu.Unlock()
				return nil, ErrHostSaturated
			}
			cands = free
		}
		minCount := -1
		var best []*slot
		for _, s := range cands {
			n := hostSessions(s, req.Host)
			switch {
			case minCount < 0 || n < minCount:
				minCount, best = n, []*slot{s}
			case n == minCount:
				best = append(best, s)
			}
		}
		l := p.bind(p.pick(best), req)
		p.mu.Unlock()
		return l, nil
	}
}

// wait releases the lock until s's repoint ends. On error the lock is not
// held on return; otherwise it is.
func (p *Pool) wait(ctx context.Context, s *slot, session string) error {
	ch := s.probeDone
	p.mu.Unlock()
	if ch == nil {
		p.mu.Lock()
		return nil
	}
	select {
	case <-ch:
		p.mu.Lock()
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		if s.claim == session {
			s.claim = ""
		}
		p.mu.Unlock()
		return ctx.Err()
	}
}

func (p *Pool) claimOf(session string) *slot {
	for _, s := range p.slots {
		if s.claim == session {
			return s
		}
	}
	return nil
}

func (p *Pool) free(s *slot, session string) bool {
	return s.drain == drainNone && s.state != stateDown && len(s.bindings) == 0 &&
		(s.claim == "" || s.claim == session)
}

func matches(r *relays.Relay, cc string) bool {
	return r != nil && (cc == "" || r.CountryCode == cc)
}

func hostSessions(s *slot, host string) int {
	n := 0
	for _, b := range s.bindings {
		if _, ok := b.hosts[host]; ok {
			n++
		}
	}
	return n
}

// activeHostSessions counts s's sessions for host that have open tunnels.
func activeHostSessions(s *slot, host string) int {
	n := 0
	for _, b := range s.bindings {
		if _, ok := b.hosts[host]; ok && b.open > 0 {
			n++
		}
	}
	return n
}

func (p *Pool) pick(ss []*slot) *slot { return ss[p.rnd.IntN(len(ss))] }

func (p *Pool) bind(s *slot, req Request) *Lease {
	b := &binding{session: req.Session, slot: s, cc: req.Country, hosts: map[string]struct{}{}}
	p.bindings[req.Session] = b
	s.bindings[req.Session] = b
	s.lastCC = req.Country
	p.log.Debug("session bound", "session", req.Session, "slot", s.id, "relay", s.relay.Hostname)
	return p.use(b, req.Host)
}

func (p *Pool) use(b *binding, host string) *Lease {
	b.open++
	b.slot.open++
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
		b.gen++
	}
	if host != "" {
		b.hosts[host] = struct{}{}
	}
	return &Lease{p: p, b: b, s: b.slot, tun: b.slot.tun, relay: *b.slot.relay}
}

func (p *Pool) release(l *Lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, b := l.s, l.b
	s.open--
	if p.bindings[b.session] == b {
		b.open--
		if b.open == 0 {
			b.gen++
			gen := b.gen
			b.timer = p.clock.AfterFunc(p.cfg.Grace, func() { p.expire(b, gen) })
		}
	}
	if s.drain != drainNone {
		p.maybeFinishDrain(s)
	}
}

// expire drops a binding at the end of its grace period and frees its slot
// if it was the last one.
func (p *Pool) expire(b *binding, gen int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bindings[b.session] != b || b.gen != gen || b.open > 0 {
		return
	}
	s := b.slot
	p.dropBinding(b, ErrSessionExpired)
	p.log.Debug("session expired", "session", b.session, "slot", s.id)
	if len(s.bindings) == 0 && s.drain == drainNone && s.state == stateReady && !s.checking {
		// Rotate (when a check after a dial timeout is running, its outcome
		// decides: drain on failure, rotate on success).: point the free slot at its next relay and probe it now, so
		// the next new session gets a checked slot.
		p.startRepoint(s, s.lastCC)
	}
}

func (p *Pool) dropBinding(b *binding, reason error) {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.gen++
	delete(p.bindings, b.session)
	delete(b.slot.bindings, b.session)
	p.tombs[b.session] = tomb{err: reason, until: p.clock.Now().Add(p.cfg.TombstoneTTL)}
}

func (p *Pool) sweep(now time.Time) {
	if now.Before(p.nextSweep) {
		return
	}
	p.nextSweep = now.Add(time.Minute)
	for k, t := range p.tombs {
		if !now.Before(t.until) {
			delete(p.tombs, k)
		}
	}
	for k, until := range p.bad {
		if !now.Before(until) {
			delete(p.bad, k)
		}
	}
}

// drainSlot stops a slot taking sessions. Its bindings are dropped right
// away (502 on their next connect); open tunnels keep going.
func (p *Pool) drainSlot(s *slot, reason drainReason) {
	if s.drain == drainRemoved || s.drain == reason {
		return
	}
	s.drain = reason
	for _, b := range s.bindings {
		p.dropBinding(b, ErrSlotDrained)
	}
	p.log.Info("slot draining", "slot", s.id, "reason", reason.String(), "open_tunnels", s.open)
	p.maybeFinishDrain(s)
}

func (p *Pool) maybeFinishDrain(s *slot) {
	if s.drain == drainNone || s.open > 0 || s.state == stateProbing || s.checking {
		return
	}
	switch s.drain {
	case drainFailed:
		s.drain = drainNone
		s.state = stateIdle
		p.startRepoint(s, s.lastCC)
	case drainRemoved:
		p.removeSlot(s)
		t := s.tun
		p.bg.Go(func() {
			if err := t.Close(); err != nil {
				p.log.Warn("close slot", "slot", s.id, "err", err)
			}
		})
	}
}

// startRepoint points s at a new relay and probes it in the background,
// trying up to ProbeAttempts relays. Requires the lock.
func (p *Pool) startRepoint(s *slot, cc string) {
	if s.state == stateProbing || s.state == stateDown || s.drain != drainNone {
		return
	}
	if len(p.relays) == 0 {
		s.state = stateIdle
		return
	}
	s.state = stateProbing
	s.probeCC = cc
	done := make(chan struct{})
	s.probeDone = done
	p.bg.Go(func() { p.repoint(s, cc, done) })
}

func (p *Pool) repoint(s *slot, cc string, done chan struct{}) {
	ok := false
	for range p.cfg.ProbeAttempts {
		p.mu.Lock()
		if s.drain != drainNone {
			p.mu.Unlock()
			break
		}
		r, found := p.pickRelay(s, cc)
		if !found {
			p.mu.Unlock()
			p.log.Warn("no relay available for slot", "slot", s.id, "country", cc)
			break
		}
		s.relay = &r
		p.mu.Unlock()

		err := p.check(s.tun, r, true)
		if err == nil {
			ok = true
			p.log.Info("slot ready", "slot", s.id, "relay", r.Hostname, "country", r.CountryCode)
			break
		}
		p.log.Warn("relay probe failed", "slot", s.id, "relay", r.Hostname, "err", err)
		p.mu.Lock()
		p.bad[r.Hostname] = p.clock.Now().Add(p.cfg.BadRelayTTL)
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	s.probeDone = nil
	if ok {
		s.state = stateReady
		s.retryAt = time.Time{}
	} else {
		s.state = stateIdle
		s.retryAt = p.clock.Now().Add(p.cfg.SlotRetry)
		if s.drain == drainNone {
			p.log.Warn("slot gave up for now", "slot", s.id, "retry_in", p.cfg.SlotRetry)
		}
	}
	p.maybeFinishDrain(s)
	close(done)
}

// check points (optionally) and probes a tunnel.
func (p *Pool) check(t Tunnel, r relays.Relay, point bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), p.cfg.ProbeTimeout+5*time.Second)
	defer cancel()
	if point {
		if err := t.Point(ctx, r); err != nil {
			return fmt.Errorf("point: %w", err)
		}
	}
	pctx, pcancel := context.WithTimeout(ctx, p.cfg.ProbeTimeout)
	defer pcancel()
	return p.prober.Probe(pctx, t, r)
}

// canPick reports whether pickRelay would find a relay for s in cc.
func (p *Pool) canPick(s *slot, cc string) bool {
	return len(p.relayCandidates(s, cc)) > 0
}

// pickRelay picks a random relay for s in cc, excluding bad relays and relays
// used by other slots (unique relays across all slots). The slot's current
// relay is only reused when nothing else is left.
func (p *Pool) pickRelay(s *slot, cc string) (relays.Relay, bool) {
	c := p.relayCandidates(s, cc)
	if len(c) == 0 {
		return relays.Relay{}, false
	}
	return c[p.rnd.IntN(len(c))], true
}

func (p *Pool) relayCandidates(s *slot, cc string) []relays.Relay {
	cands := p.relays
	if cc != "" {
		cands = p.byCountry[cc]
	}
	used := map[string]bool{}
	for _, o := range p.slots {
		if (s == nil || o != s) && o.relay != nil {
			used[o.relay.Hostname] = true
		}
	}
	now := p.clock.Now()
	var current, ok []relays.Relay
	for _, r := range cands {
		if used[r.Hostname] {
			continue
		}
		if until, bad := p.bad[r.Hostname]; bad && now.Before(until) {
			continue
		}
		if s != nil && s.relay != nil && r.Hostname == s.relay.Hostname {
			current = append(current, r)
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		return current
	}
	return ok
}

// Close tears down every slot. The pool is unusable afterwards.
func (p *Pool) Close() {
	p.mu.Lock()
	var tuns []Tunnel
	for _, s := range p.slots {
		if s.tun != nil {
			tuns = append(tuns, s.tun)
		}
		s.drain = drainRemoved
		s.state = stateDown
	}
	p.slots = nil
	p.mu.Unlock()
	for _, t := range tuns {
		_ = t.Close()
	}
}

// Lease is one tunnel's use of a session binding.
type Lease struct {
	p     *Pool
	b     *binding
	s     *slot
	tun   Tunnel
	relay relays.Relay
	once  sync.Once
}

// Relay is the relay the session exits through.
func (l *Lease) Relay() relays.Relay { return l.relay }

// Slot is the slot ID.
func (l *Lease) Slot() string { return l.s.id }

// DialContext dials the target through the slot's tunnel.
func (l *Lease) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	return l.tun.DialContext(ctx, addr)
}

// Release ends this tunnel. The binding's grace period starts when its last
// tunnel is released. Safe to call more than once.
func (l *Lease) Release() { l.once.Do(func() { l.p.release(l) }) }

// DialTimedOut reports that a dial through this slot timed out. The slot is
// probed once; if the probe fails the slot is drained and its relay marked
// bad. A target refusing the connection must not be reported here.
func (l *Lease) DialTimedOut() {
	p, s := l.p, l.s
	p.mu.Lock()
	if s.checking || s.drain != drainNone || s.state != stateReady || s.relay == nil || s.relay.Hostname != l.relay.Hostname {
		p.mu.Unlock()
		return
	}
	s.checking = true
	r, t := *s.relay, s.tun
	p.mu.Unlock()
	p.bg.Go(func() {
		err := p.check(t, r, false)
		p.mu.Lock()
		defer p.mu.Unlock()
		s.checking = false
		if err != nil {
			p.log.Warn("slot failed probe after dial timeout", "slot", s.id, "relay", r.Hostname, "err", err)
			p.bad[r.Hostname] = p.clock.Now().Add(p.cfg.BadRelayTTL)
			p.drainSlot(s, drainFailed)
			return
		}
		if len(s.bindings) == 0 && s.drain == drainNone && s.state == stateReady {
			p.startRepoint(s, s.lastCC) // freed while being checked
		}
		p.maybeFinishDrain(s)
	})
}

// WaitBackground waits for running repoints, probes and teardowns (tests).
func (p *Pool) WaitBackground() { p.bg.Wait() }
