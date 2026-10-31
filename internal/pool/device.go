package pool

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// NormalizeDevice validates a device and strips a /32 suffix from its
// address (the Mullvad API returns "10.x.y.z/32").
func NormalizeDevice(d Device) (Device, error) {
	d.ID = strings.TrimSpace(d.ID)
	if d.ID == "" || strings.ContainsAny(d.ID, "/ ") {
		return d, fmt.Errorf("device id %q: must be non-empty without '/' or spaces", d.ID)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(d.PrivateKey))
	if err != nil || len(key) != 32 {
		return d, fmt.Errorf("device %s: private_key must be a base64 WireGuard key", d.ID)
	}
	d.PrivateKey = strings.TrimSpace(d.PrivateKey)
	addr, _, _ := strings.Cut(strings.TrimSpace(d.IPv4), "/")
	ip := net.ParseIP(addr)
	if ip == nil || ip.To4() == nil {
		return d, fmt.Errorf("device %s: ipv4 %q is not an IPv4 address", d.ID, d.IPv4)
	}
	d.IPv4 = ip.To4().String()
	return d, nil
}

// ParseDevices parses MULLVAD_DEVICES: a JSON array of {"id","private_key","ipv4"}.
func ParseDevices(s string) ([]Device, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var list []Device
	if err := json.Unmarshal([]byte(s), &list); err != nil {
		return nil, fmt.Errorf("MULLVAD_DEVICES: %w", err)
	}
	seen := map[string]bool{}
	for i, d := range list {
		nd, err := NormalizeDevice(d)
		if err != nil {
			return nil, fmt.Errorf("MULLVAD_DEVICES[%d]: %w", i, err)
		}
		if seen[nd.ID] {
			return nil, fmt.Errorf("MULLVAD_DEVICES: duplicate id %q", nd.ID)
		}
		seen[nd.ID] = true
		list[i] = nd
	}
	return list, nil
}
