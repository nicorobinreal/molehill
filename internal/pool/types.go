// Package pool is the core of the service: devices, slots, session bindings,
// slot selection, the grace period, draining, bad relays and probe
// orchestration. It knows nothing about WireGuard; the network layer is
// behind Network/Tunnel and the clock and randomness are injectable, so it
// is unit-tested with fakes.
package pool

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/nicorobinreal/molehill/internal/relays"
)

// Device is one WireGuard key registered on a Mullvad account.
type Device struct {
	ID         string `json:"id"`
	PrivateKey string `json:"private_key"`
	IPv4       string `json:"ipv4"` // plain address, no prefix
}

// Network creates slot interfaces.
type Network interface {
	// Open brings up one interface for the device, not yet pointed at a relay.
	Open(ctx context.Context, dev Device, index int) (Tunnel, error)
}

// Tunnel is one slot interface.
type Tunnel interface {
	Name() string
	// Point moves the interface's only peer to the relay.
	Point(ctx context.Context, r relays.Relay) error
	// DialContext dials addr (host:port) through the tunnel, IPv4 only, with
	// DNS resolved through the tunnel.
	DialContext(ctx context.Context, addr string) (net.Conn, error)
	Close() error
}

// Prober checks that a tunnel pointed at r works and exits through r.
type Prober interface {
	Probe(ctx context.Context, t Tunnel, r relays.Relay) error
}

// Clock is the time source, faked in tests.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a stoppable AfterFunc timer.
type Timer interface{ Stop() bool }

// Rand picks random indexes, injectable for tests.
type Rand interface{ IntN(n int) int }

type realClock struct{}

func (realClock) Now() time.Time                            { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Errors returned by Acquire. The proxy maps them to HTTP responses.
var (
	ErrNoSlot         = errors.New("no_slot")         // 503
	ErrHostSaturated  = errors.New("host_saturated")  // 503
	ErrSessionExpired = errors.New("session expired") // 502
	ErrSlotDrained    = errors.New("slot drained")    // 502
	ErrUnknownCountry = errors.New("unknown country") // 400
	ErrDeviceExists   = errors.New("device already exists")
	ErrDeviceNotFound = errors.New("device not found")
)

// Config tunes the pool. Zero values get the defaults below.
type Config struct {
	SlotsPerDevice int           // fixed slots per device, opened up front; 0 = open them as needed
	Grace          time.Duration // binding kept after the last tunnel closes, default 60s
	BadRelayTTL    time.Duration // default 10m
	ProbeAttempts  int           // relays tried per repoint, default 3
	ProbeTimeout   time.Duration // default 5s
	SlotRetry      time.Duration // a slot whose repoint gave up is skipped this long, default 30s
	TombstoneTTL   time.Duration // how long a dropped session keeps getting 502, default 1h
}

func (c *Config) defaults() {
	if c.Grace <= 0 {
		c.Grace = 60 * time.Second
	}
	if c.BadRelayTTL <= 0 {
		c.BadRelayTTL = 10 * time.Minute
	}
	if c.ProbeAttempts <= 0 {
		c.ProbeAttempts = 3
	}
	if c.ProbeTimeout <= 0 {
		c.ProbeTimeout = 5 * time.Second
	}
	if c.SlotRetry <= 0 {
		c.SlotRetry = 30 * time.Second
	}
	if c.TombstoneTTL <= 0 {
		c.TombstoneTTL = time.Hour
	}
}

// Request is one CONNECT asking for a slot.
type Request struct {
	Session string
	Country string // lowercase Mullvad country code, "" = any
	Host    string // target host, used for per-host session counts
	Strict  bool
}
