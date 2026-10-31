// Package proxy is the HTTP CONNECT front end. It parses the session
// username, gets a slot lease from the pool, dials the target through the
// slot before answering, and maps pool errors to CONNECT responses.
package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
)

// Retry-After values for capacity refusals.
const (
	RetryNoSlot        = 5  // seconds; a slot frees up within the grace period or a repoint
	RetryHostSaturated = 10 // seconds; a session for this host has to end first
)

// Params is a parsed proxy username.
type Params struct {
	Session string
	Country string
	Strict  bool
}

// ParseUsername parses session-<SESSION>-cc-<CC>-strict-<0|1>. Segments are
// key-value pairs joined by "-", in any order; cc and strict are optional.
// Unknown keys are ignored.
func ParseUsername(u string) (Params, error) {
	var p Params
	if u == "" {
		return p, errors.New("missing proxy username")
	}
	parts := strings.Split(u, "-")
	if len(parts)%2 != 0 {
		return p, errors.New("username must be key-value pairs joined by '-'")
	}
	seen := map[string]bool{}
	for i := 0; i < len(parts); i += 2 {
		k, v := strings.ToLower(parts[i]), parts[i+1]
		if k == "" || v == "" {
			return p, errors.New("empty username segment")
		}
		if seen[k] {
			return p, fmt.Errorf("duplicate %q", k)
		}
		seen[k] = true
		switch k {
		case "session":
			p.Session = v
		case "cc":
			v = strings.ToLower(v)
			for _, c := range v {
				if c < 'a' || c > 'z' {
					return p, fmt.Errorf("invalid country code %q", v)
				}
			}
			p.Country = v
		case "strict":
			switch v {
			case "1":
				p.Strict = true
			case "0":
			default:
				return p, fmt.Errorf("strict must be 0 or 1, got %q", v)
			}
		}
	}
	if p.Session == "" {
		return p, errors.New("missing session")
	}
	return p, nil
}

func proxyUsername(r *http.Request) (string, error) {
	h := r.Header.Get("Proxy-Authorization")
	if h == "" {
		return "", errors.New("missing Proxy-Authorization")
	}
	scheme, enc, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", errors.New("Proxy-Authorization must be Basic")
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(enc))
	if err != nil {
		return "", errors.New("invalid Proxy-Authorization")
	}
	user, _, _ := strings.Cut(string(dec), ":")
	return user, nil
}

// Server handles CONNECT requests.
type Server struct {
	Pool           *pool.Pool
	DialTimeout    time.Duration // target dial through the slot, default 10s
	AcquireTimeout time.Duration // waiting for a slot (repoint + probes), default 15s; with DialTimeout it must stay under the client's CONNECT timeout
	Log            *slog.Logger
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		w.Header().Set("Allow", http.MethodConnect)
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}
	user, err := proxyUsername(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	params, err := ParseUsername(user)
	if err != nil {
		http.Error(w, "bad proxy username: "+err.Error(), http.StatusBadRequest)
		return
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" || port == "" {
		http.Error(w, "CONNECT target must be host:port", http.StatusBadRequest)
		return
	}

	actx, cancel := context.WithTimeout(r.Context(), s.acquireTimeout())
	lease, err := s.Pool.Acquire(actx, pool.Request{
		Session: params.Session, Country: params.Country, Host: strings.ToLower(host), Strict: params.Strict,
	})
	cancel()
	if err != nil {
		s.refuse(w, r, params, err)
		return
	}

	dctx, cancel := context.WithTimeout(r.Context(), s.dialTimeout())
	upstream, err := lease.DialContext(dctx, r.Host)
	cancel()
	if err != nil {
		code := "dial_failed"
		// Only a timeout says something about the tunnel. A refusal, reset
		// or NXDOMAIN is the target's business and never discards a slot.
		if isTimeout(err) && r.Context().Err() == nil {
			code = "dial_timeout"
			lease.DialTimedOut()
		}
		lease.Release()
		s.Log.Info("target dial failed", "session", params.Session, "target", r.Host, "slot", lease.Slot(), "err", err)
		w.Header().Set("X-Mullvad-Error", code)
		http.Error(w, "dial target: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer lease.Release()

	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	s.Log.Debug("tunnel open", "session", params.Session, "target", r.Host, "relay", lease.Relay().Hostname)
	pipe(client, brw.Reader, upstream)
}

// pipe copies both ways until either side ends, then closes both.
func pipe(client net.Conn, clientR io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, clientR); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	client.Close()
	upstream.Close()
	<-done
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, p Params, err error) {
	h := w.Header()
	switch {
	case errors.Is(err, pool.ErrHostSaturated):
		h.Set("X-Mullvad-Error", "host_saturated")
		h.Set("Retry-After", strconv.Itoa(RetryHostSaturated))
		http.Error(w, "every exit IP in this country already has a session for this host", http.StatusServiceUnavailable)
	case errors.Is(err, pool.ErrNoSlot), errors.Is(err, context.DeadlineExceeded):
		h.Set("X-Mullvad-Error", "no_slot")
		h.Set("Retry-After", strconv.Itoa(RetryNoSlot))
		http.Error(w, "no slot can serve this request", http.StatusServiceUnavailable)
	case errors.Is(err, pool.ErrSessionExpired):
		h.Set("X-Mullvad-Error", "session_expired")
		http.Error(w, "session binding expired", http.StatusBadGateway)
	case errors.Is(err, pool.ErrSlotDrained):
		h.Set("X-Mullvad-Error", "slot_drained")
		http.Error(w, "session's slot was drained", http.StatusBadGateway)
	case errors.Is(err, pool.ErrUnknownCountry):
		http.Error(w, fmt.Sprintf("unknown country %q", p.Country), http.StatusBadRequest)
	case errors.Is(err, context.Canceled):
		// Client went away.
		return
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	s.Log.Info("connect refused", "session", p.Session, "country", p.Country, "target", r.Host, "err", err)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Server) dialTimeout() time.Duration {
	if s.DialTimeout > 0 {
		return s.DialTimeout
	}
	return 10 * time.Second
}

func (s *Server) acquireTimeout() time.Duration {
	if s.AcquireTimeout > 0 {
		return s.AcquireTimeout
	}
	return 15 * time.Second
}
