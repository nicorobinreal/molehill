// Command molehill wraps Mullvad WireGuard relays in an HTTP CONNECT proxy
// with sticky sessions. See README.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nicorobinreal/molehill/internal/api"
	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/probe"
	"github.com/nicorobinreal/molehill/internal/proxy"
	"github.com/nicorobinreal/molehill/internal/relays"
	"github.com/nicorobinreal/molehill/internal/wg"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	level := slog.LevelInfo
	if strings.EqualFold(os.Getenv("LOG_LEVEL"), "debug") {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	devices, err := pool.ParseDevices(os.Getenv("MULLVAD_DEVICES"))
	if err != nil {
		return err
	}
	slots, err := envInt("MULLVAD_SLOTS_PER_DEVICE", 0)
	if err != nil {
		return err
	}
	mtu, err := envInt("MULLVAD_MTU", 1380)
	if err != nil {
		return err
	}
	refresh, err := envDuration("MULLVAD_RELAYS_REFRESH", 6*time.Hour)
	if err != nil {
		return err
	}
	dialTimeout, err := envDuration("MULLVAD_DIAL_TIMEOUT", 10*time.Second)
	if err != nil {
		return err
	}
	grace, err := envDuration("MULLVAD_GRACE", 60*time.Second)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	wg.CleanupOrphans(wg.DefaultTableBase, log)
	network := &wg.Network{MTU: mtu, DNS: env("MULLVAD_TUNNEL_DNS", "10.64.0.1"), DialTimeout: dialTimeout, Log: log}
	p := pool.New(pool.Config{SlotsPerDevice: slots, Grace: grace}, network,
		probe.HTTP{URL: env("MULLVAD_PROBE_URL", probe.DefaultURL)}, nil, nil, log)
	defer p.Close()

	src := &relays.Source{
		URL:      env("MULLVAD_RELAYS_URL", relays.DefaultURL),
		Path:     env("MULLVAD_RELAYS_FILE", "/data/relays.json"),
		Interval: refresh,
		Retry:    time.Minute,
		Client:   &http.Client{Timeout: 30 * time.Second},
		OnUpdate: p.SetRelays,
		Log:      log,
	}
	loaded := src.Init(ctx)
	go src.Run(ctx, loaded)

	for _, d := range devices {
		if err := p.AddDevice(ctx, d); err != nil {
			return fmt.Errorf("add device %s: %w", d.ID, err)
		}
	}
	if len(devices) == 0 {
		log.Warn("no devices configured (MULLVAD_DEVICES); every CONNECT gets no_slot until one is added via the API")
	}

	proxySrv := &http.Server{
		Addr:              env("MULLVAD_PROXY_ADDR", ":8080"),
		Handler:           &proxy.Server{Pool: p, DialTimeout: dialTimeout, Log: log},
		ReadHeaderTimeout: 10 * time.Second,
	}
	apiSrv := &http.Server{
		Addr:              env("MULLVAD_API_ADDR", ":8081"),
		Handler:           api.Handler(p),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 2)
	for _, s := range []*http.Server{proxySrv, apiSrv} {
		go func() {
			log.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = apiSrv.Shutdown(sctx)
	_ = proxySrv.Shutdown(sctx) // hijacked tunnels are cut when the interfaces go
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) (int, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s: want a positive integer, got %q", k, v)
	}
	return n, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(k)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s: want a duration like 10s, got %q", k, v)
	}
	return d, nil
}
