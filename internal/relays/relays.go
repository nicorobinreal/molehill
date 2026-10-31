// Package relays fetches, persists and refreshes the Mullvad WireGuard relay
// list. The Mullvad endpoints are undocumented and have been retired before,
// so everything that knows about their shape lives here.
package relays

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultURL is the flat WireGuard relay list (no auth).
const DefaultURL = "https://api.mullvad.net/www/relays/wireguard/"

// Relay is one WireGuard relay. The JSON tags match the flat endpoint, so the
// persisted file uses the same shape and can be hand-written for tests.
type Relay struct {
	Hostname    string `json:"hostname"`
	CountryCode string `json:"country_code"`
	CityCode    string `json:"city_code,omitempty"`
	Active      bool   `json:"active"`
	Type        string `json:"type"`
	IPv4        string `json:"ipv4_addr_in"`
	PublicKey   string `json:"pubkey"`
}

// Endpoint is the relay's WireGuard endpoint (IPv4 only).
func (r Relay) Endpoint() string { return net.JoinHostPort(r.IPv4, "51820") }

// Filter keeps active WireGuard relays with a usable IPv4 endpoint and key,
// and lowercases the country code.
func Filter(in []Relay) []Relay {
	out := make([]Relay, 0, len(in))
	for _, r := range in {
		if !r.Active || r.Type != "wireguard" || r.Hostname == "" || r.PublicKey == "" {
			continue
		}
		ip := net.ParseIP(r.IPv4)
		if ip == nil || ip.To4() == nil {
			continue
		}
		r.CountryCode = strings.ToLower(r.CountryCode)
		out = append(out, r)
	}
	return out
}

// Parse decodes a flat relay list and filters it.
func Parse(data []byte) ([]Relay, error) {
	var list []Relay
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode relay list: %w", err)
	}
	return Filter(list), nil
}

// Fetch downloads the relay list from url.
func Fetch(ctx context.Context, client *http.Client, url string) ([]Relay, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("relay list: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	list, err := Parse(data)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return nil, nil, errors.New("relay list: no active wireguard relays")
	}
	return list, data, nil
}

// Load reads a persisted relay list.
func Load(path string) ([]Relay, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Save writes the raw relay list atomically.
func Save(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Source keeps the relay list fresh and hands every new list to OnUpdate.
type Source struct {
	URL      string
	Path     string        // persisted copy, fallback when the API is down
	Interval time.Duration // background refresh interval
	Retry    time.Duration // retry interval while no list could be loaded at all
	Client   *http.Client
	OnUpdate func([]Relay)
	Log      *slog.Logger
}

// Init loads the list once: the API first, then the persisted file. It reports
// whether any list was loaded.
func (s *Source) Init(ctx context.Context) bool {
	if s.refresh(ctx) {
		return true
	}
	list, err := Load(s.Path)
	if err != nil {
		s.Log.Error("relay list: no persisted fallback either", "path", s.Path, "err", err)
		return false
	}
	s.Log.Warn("relay list: using persisted fallback", "path", s.Path, "relays", len(list))
	s.OnUpdate(list)
	return true
}

func (s *Source) refresh(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	list, raw, err := Fetch(ctx, s.Client, s.URL)
	if err != nil {
		s.Log.Error("relay list: fetch from Mullvad API failed (endpoint is undocumented and may have moved)", "url", s.URL, "err", err)
		return false
	}
	if err := Save(s.Path, raw); err != nil {
		s.Log.Warn("relay list: could not persist", "path", s.Path, "err", err)
	}
	s.Log.Info("relay list: fetched", "relays", len(list))
	s.OnUpdate(list)
	return true
}

// Run refreshes in the background until ctx ends. loaded is Init's result:
// until a list is loaded it retries at Retry instead of Interval.
func (s *Source) Run(ctx context.Context, loaded bool) {
	for {
		wait := s.Interval
		if !loaded {
			wait = s.Retry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if s.refresh(ctx) {
			loaded = true
		}
	}
}
