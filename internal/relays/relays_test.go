package relays

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

const sample = `[
 {"hostname":"se-sto-wg-001","country_code":"se","active":true,"type":"wireguard","ipv4_addr_in":"185.0.0.1","ipv6_addr_in":"2a03::1","pubkey":"k1"},
 {"hostname":"SE-sto-wg-002","country_code":"SE","active":true,"type":"wireguard","ipv4_addr_in":"185.0.0.2","pubkey":"k2"},
 {"hostname":"de-fra-wg-001","country_code":"de","active":false,"type":"wireguard","ipv4_addr_in":"185.0.0.3","pubkey":"k3"},
 {"hostname":"de-fra-ovpn-001","country_code":"de","active":true,"type":"openvpn","ipv4_addr_in":"185.0.0.4","pubkey":"k4"},
 {"hostname":"x","country_code":"de","active":true,"type":"wireguard","ipv4_addr_in":"2a03::5","pubkey":"k5"}
]`

func TestParseFilters(t *testing.T) {
	list, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[1].CountryCode != "se" || list[0].Endpoint() != "185.0.0.1:51820" {
		t.Fatalf("%+v", list)
	}
}

func TestSourceFetchPersistAndFallback(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	path := filepath.Join(t.TempDir(), "sub", "relays.json")
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up {
			w.WriteHeader(502)
			return
		}
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	var got []Relay
	s := &Source{URL: srv.URL, Path: path, Client: srv.Client(), Log: log, OnUpdate: func(r []Relay) { got = r }}
	if !s.Init(context.Background()) || len(got) != 2 {
		t.Fatalf("init: %d", len(got))
	}
	up = false
	got = nil
	if !s.Init(context.Background()) || len(got) != 2 {
		t.Fatalf("fallback: %d", len(got))
	}
	s.Path = filepath.Join(t.TempDir(), "missing.json")
	if s.Init(context.Background()) {
		t.Fatal("no API and no file should fail")
	}
}
