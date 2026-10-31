package pool

import "time"

// DeviceInfo is a device without its private key.
type DeviceInfo struct {
	ID       string `json:"id"`
	IPv4     string `json:"ipv4"`
	Slots    int    `json:"slots"`
	Draining bool   `json:"draining"`
}

// SlotStatus is one slot in Status.
type SlotStatus struct {
	ID          string         `json:"id"`
	Device      string         `json:"device"`
	Interface   string         `json:"interface"`
	State       string         `json:"state"`
	Drain       string         `json:"drain,omitempty"`
	Relay       string         `json:"relay,omitempty"`
	Country     string         `json:"country,omitempty"`
	Sessions    int            `json:"sessions"`
	OpenTunnels int            `json:"open_tunnels"`
	Hosts       map[string]int `json:"sessions_per_host,omitempty"`
	RetryAt     *time.Time     `json:"retry_at,omitempty"`
}

// Status is a read-only snapshot for debugging.
type Status struct {
	Relays    int                  `json:"relays"`
	Devices   []DeviceInfo         `json:"devices"`
	Slots     []SlotStatus         `json:"slots"`
	Sessions  int                  `json:"sessions"`
	BadRelays map[string]time.Time `json:"bad_relays"`
}

// Devices lists the devices.
func (p *Pool) Devices() []DeviceInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.devicesLocked()
}

func (p *Pool) devicesLocked() []DeviceInfo {
	out := make([]DeviceInfo, 0, len(p.order))
	for _, id := range p.order {
		d := p.devices[id]
		out = append(out, DeviceInfo{ID: d.ID, IPv4: d.IPv4, Slots: len(d.slots), Draining: d.draining})
	}
	return out
}

// Status returns a snapshot of the pool.
func (p *Pool) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock.Now()
	st := Status{
		Relays:    len(p.relays),
		Devices:   p.devicesLocked(),
		Slots:     []SlotStatus{},
		Sessions:  len(p.bindings),
		BadRelays: map[string]time.Time{},
	}
	for k, until := range p.bad {
		if now.Before(until) {
			st.BadRelays[k] = until
		}
	}
	for _, s := range p.slots {
		ss := SlotStatus{
			ID: s.id, Device: s.dev.ID, State: s.state.String(), Drain: s.drain.String(),
			Sessions: len(s.bindings), OpenTunnels: s.open,
		}
		if s.tun != nil {
			ss.Interface = s.tun.Name()
		}
		if s.relay != nil {
			ss.Relay, ss.Country = s.relay.Hostname, s.relay.CountryCode
		}
		if s.state == stateIdle && now.Before(s.retryAt) {
			t := s.retryAt
			ss.RetryAt = &t
		}
		if len(s.bindings) > 0 {
			ss.Hosts = map[string]int{}
			for _, b := range s.bindings {
				for h := range b.hosts {
					ss.Hosts[h]++
				}
			}
		}
		st.Slots = append(st.Slots, ss)
	}
	return st
}
