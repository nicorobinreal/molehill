// Package api is the management HTTP API: devices and a read-only status.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/nicorobinreal/molehill/internal/pool"
)

// Handler returns the management API handler.
func Handler(p *pool.Pool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.Status())
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.Devices())
	})
	mux.HandleFunc("POST /devices", func(w http.ResponseWriter, r *http.Request) {
		var d pool.Device
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&d); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		d, err := pool.NormalizeDevice(d)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		switch err := p.AddDevice(ctx, d); {
		case errors.Is(err, pool.ErrDeviceExists):
			writeError(w, http.StatusConflict, err.Error())
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			for _, info := range p.Devices() {
				if info.ID == d.ID {
					writeJSON(w, http.StatusCreated, info)
					return
				}
			}
			w.WriteHeader(http.StatusCreated)
		}
	})
	mux.HandleFunc("DELETE /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		switch err := p.RemoveDevice(r.PathValue("id")); {
		case errors.Is(err, pool.ErrDeviceNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case err != nil:
			writeError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"status": "draining"})
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
