// Package wg is the kernel WireGuard network layer: one interface per slot,
// no routes of its own (the equivalent of Table = off), and per-slot fwmark
// policy routing so only sockets carrying the slot's SO_MARK use it.
//
// The interface is built directly with ip/wg rather than wg-quick, which
// gives the same result as a wg-quick config with Table = off, no FwMark on
// the interface (WireGuard's own UDP must stay unmarked or it would loop into
// its own table) and no DNS handling.
package wg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/relays"
)

const (
	// IfacePrefix names slot interfaces mvd<N>.
	IfacePrefix = "mvd"
	// DefaultTableBase: slot N uses routing table and fwmark TableBase+N. Far
	// from wg-quick's 51820, and the interfaces set no fwmark of their own.
	DefaultTableBase = 27000
	maxSlots         = 1000
)

// Network opens kernel WireGuard slot interfaces.
type Network struct {
	MTU         int           // default 1380
	DNS         string        // tunnel DNS server, default 10.64.0.1
	TableBase   int           // default DefaultTableBase
	DialTimeout time.Duration // TCP connect timeout, default 10s
	Log         *slog.Logger

	mu   sync.Mutex
	used map[int]bool // interface indexes in use, reused after Close
}

func (n *Network) alloc() (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.used == nil {
		n.used = map[int]bool{}
	}
	for i := range maxSlots {
		if !n.used[i] {
			n.used[i] = true
			return i, nil
		}
	}
	return 0, errors.New("too many slot interfaces")
}

func (n *Network) free(i int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.used, i)
}

var _ pool.Network = (*Network)(nil)

func (n *Network) defaults() {
	if n.MTU == 0 {
		n.MTU = 1380
	}
	if n.DNS == "" {
		n.DNS = "10.64.0.1"
	}
	if n.TableBase == 0 {
		n.TableBase = DefaultTableBase
	}
	if n.DialTimeout == 0 {
		n.DialTimeout = 10 * time.Second
	}
	if n.Log == nil {
		n.Log = slog.Default()
	}
}

// Open creates one WireGuard interface for the device, with no peer yet.
func (n *Network) Open(ctx context.Context, d pool.Device, _ int) (pool.Tunnel, error) {
	n.defaults()
	addr := net.ParseIP(d.IPv4).To4()
	if addr == nil {
		return nil, fmt.Errorf("device %s: invalid IPv4 %q", d.ID, d.IPv4)
	}
	idx, err := n.alloc()
	if err != nil {
		return nil, err
	}
	t := &Tunnel{
		name:  fmt.Sprintf("%s%d", IfacePrefix, idx),
		table: n.TableBase + idx,
		addr:  addr.String(),
		dns:   net.JoinHostPort(n.DNS, "53"),
		log:   n.Log,
		free:  func() { n.free(idx) },
	}
	t.mark = t.table
	t.dialer = &net.Dialer{Timeout: n.DialTimeout, Control: t.control}
	// PreferGo forces the pure-Go resolver so every DNS socket goes through
	// our marked dialer. cgo's getaddrinfo would bypass it and leak DNS out
	// the default route.
	t.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			nw := "udp4"
			if strings.HasPrefix(network, "tcp") {
				nw = "tcp4"
			}
			d := &net.Dialer{Timeout: 5 * time.Second, Control: t.control}
			return d.DialContext(ctx, nw, t.dns)
		},
	}

	steps := [][]string{
		{"ip", "link", "add", "dev", t.name, "type", "wireguard"},
		{"ip", "-4", "address", "add", t.addr + "/32", "dev", t.name},
		{"ip", "link", "set", "dev", t.name, "mtu", strconv.Itoa(n.MTU), "up"},
	}
	fail := func(err error) (pool.Tunnel, error) {
		t.teardown()
		return nil, err
	}
	if err := run(ctx, nil, steps[0]...); err != nil {
		return fail(err)
	}
	if err := run(ctx, strings.NewReader(d.PrivateKey), "wg", "set", t.name, "private-key", "/dev/stdin"); err != nil {
		return fail(err)
	}
	for _, s := range steps[1:] {
		if err := run(ctx, nil, s...); err != nil {
			return fail(err)
		}
	}
	if err := t.setupRouting(ctx); err != nil {
		return fail(err)
	}
	n.Log.Info("slot interface up", "iface", t.name, "device", d.ID, "table", t.table)
	return t, nil
}

// Tunnel is one slot interface.
type Tunnel struct {
	name     string
	table    int
	mark     int
	addr     string
	dns      string
	dialer   *net.Dialer
	resolver *net.Resolver
	log      *slog.Logger
	free     func()
	once     sync.Once

	mu   sync.Mutex
	peer string // current peer public key
}

func (t *Tunnel) Name() string { return t.name }

// Point replaces the interface's peer with the relay (wg set, the interface
// stays up). AllowedIPs is 0.0.0.0/0 only: no IPv6 through or around it.
func (t *Tunnel) Point(ctx context.Context, r relays.Relay) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	args := []string{"wg", "set", t.name}
	if t.peer != "" && t.peer != r.PublicKey {
		args = append(args, "peer", t.peer, "remove")
	}
	args = append(args, "peer", r.PublicKey, "endpoint", r.Endpoint(), "allowed-ips", "0.0.0.0/0")
	if err := run(ctx, nil, args...); err != nil {
		return err
	}
	t.peer = r.PublicKey
	return nil
}

// DialContext dials addr through the tunnel: IPv4 only, A records only, DNS
// through the tunnel.
func (t *Tunnel) DialContext(ctx context.Context, addr string) (net.Conn, error) {
	return dialIPv4(ctx, addr, t.resolver.LookupIP, t.dialer.DialContext, isLocalAddr)
}

// Close removes the interface and its routing.
func (t *Tunnel) Close() error {
	t.teardown()
	return nil
}

// control sets SO_MARK so the socket is routed by the slot's table. Anything
// but IPv4 is refused: the policy routing only covers IPv4, an IPv6 socket
// would leave through the container's default route with the real IP.
func (t *Tunnel) control(network, _ string, c syscall.RawConn) error {
	if !strings.HasSuffix(network, "4") {
		return fmt.Errorf("refusing non-IPv4 socket %q", network)
	}
	var seterr error
	if err := c.Control(func(fd uintptr) {
		seterr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, t.mark)
	}); err != nil {
		return err
	}
	return seterr
}

// setupRouting: marked packets use the slot's table, whose default route is
// the interface. A blackhole default with a worse metric stays when the
// interface route is gone, so marked packets are dropped rather than falling
// through to the main table. For IPv6, marked packets are prohibited.
func (t *Tunnel) setupRouting(ctx context.Context) error {
	tbl, mark := strconv.Itoa(t.table), strconv.Itoa(t.mark)
	cmds := [][]string{
		{"ip", "-4", "route", "add", "default", "dev", t.name, "src", t.addr, "table", tbl},
		{"ip", "-4", "route", "add", "blackhole", "default", "metric", "4000000000", "table", tbl},
		{"ip", "-4", "rule", "add", "fwmark", mark, "table", tbl, "priority", tbl},
	}
	for _, c := range cmds {
		if err := run(ctx, nil, c...); err != nil {
			return err
		}
	}
	// Best effort: fails harmlessly when the container has no IPv6.
	if err := run(ctx, nil, "ip", "-6", "rule", "add", "fwmark", mark, "prohibit", "priority", tbl); err != nil {
		t.log.Debug("ipv6 prohibit rule not added", "iface", t.name, "err", err)
	}
	return nil
}

func (t *Tunnel) teardown() {
	t.once.Do(func() {
		teardown(t.name, t.table, t.mark)
		if t.free != nil {
			t.free()
		}
	})
}

func teardown(name string, table, mark int) {
	tbl, m := strconv.Itoa(table), strconv.Itoa(mark)
	ctx := context.Background()
	_ = run(ctx, nil, "ip", "link", "del", "dev", name)
	for run(ctx, nil, "ip", "-4", "rule", "del", "fwmark", m, "table", tbl) == nil {
	}
	for run(ctx, nil, "ip", "-6", "rule", "del", "fwmark", m, "prohibit") == nil {
	}
	_ = run(ctx, nil, "ip", "-4", "route", "flush", "table", tbl)
}

// CleanupOrphans removes slot interfaces and routing left over from a
// previous crash.
func CleanupOrphans(tableBase int, log *slog.Logger) {
	if tableBase == 0 {
		tableBase = DefaultTableBase
	}
	out, err := exec.Command("ip", "-o", "link", "show", "type", "wireguard").CombinedOutput()
	if err != nil {
		log.Warn("list wireguard interfaces", "err", err, "out", string(out))
		return
	}
	re := regexp.MustCompile(`\d+:\s+` + IfacePrefix + `(\d+)[@:]`)
	for _, m := range re.FindAllStringSubmatch(string(out), -1) {
		idx, _ := strconv.Atoi(m[1])
		name := IfacePrefix + m[1]
		log.Info("cleaning up orphaned interface", "iface", name)
		teardown(name, tableBase+idx, tableBase+idx)
	}
	// Rules whose interface is already gone.
	rules, _ := exec.Command("ip", "-4", "rule", "show").CombinedOutput()
	ruleRe := regexp.MustCompile(`fwmark 0x([0-9a-f]+) lookup (\d+)`)
	for _, m := range ruleRe.FindAllStringSubmatch(string(rules), -1) {
		mark, _ := strconv.ParseInt(m[1], 16, 64)
		tbl, _ := strconv.Atoi(m[2])
		if tbl >= tableBase && tbl < tableBase+maxSlots && int(mark) == tbl {
			teardown(fmt.Sprintf("%s%d", IfacePrefix, tbl-tableBase), tbl, tbl)
		}
	}
}

func run(ctx context.Context, stdin *strings.Reader, args ...string) error {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", strings.Join(redact(args), " "), err, strings.TrimSpace(out.String()))
	}
	return nil
}

func redact(args []string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if i > 0 && out[i-1] == "private-key" {
			out[i] = "<redacted>"
		}
	}
	return out
}

var (
	errIPv6        = errors.New("IPv6 targets are not supported (would leak outside the tunnel)")
	errNoA         = errors.New("no A records")
	errLocalTarget = errors.New("target resolves to a local address")
)

// dialIPv4 resolves A records only and dials tcp4. Targets that resolve to a
// local address are refused: the kernel's local table is consulted before
// our fwmark rule, so they would never go through the tunnel.
func dialIPv4(
	ctx context.Context, addr string,
	lookup func(ctx context.Context, network, host string) ([]net.IP, error),
	dial func(ctx context.Context, network, addr string) (net.Conn, error),
	local func(net.IP) bool,
) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ip4 := ip.To4()
		if ip4 == nil {
			return nil, errIPv6
		}
		ips = []net.IP{ip4}
	} else {
		found, err := lookup(ctx, "ip4", host)
		if err != nil {
			return nil, err
		}
		for _, ip := range found {
			if ip4 := ip.To4(); ip4 != nil {
				ips = append(ips, ip4)
			}
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: errNoA.Error(), Name: host, IsNotFound: true}
		}
	}
	var lastErr error
	for _, ip := range ips {
		if local(ip) {
			lastErr = errLocalTarget
			continue
		}
		c, err := dial(ctx, "tcp4", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return c, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, lastErr
}

func isLocalAddr(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}
