package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicorobinreal/molehill/internal/pool"
	"github.com/nicorobinreal/molehill/internal/pool/pooltest"
)

const key = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="

func TestDevicesAPI(t *testing.T) {
	p := pool.New(pool.Config{SlotsPerDevice: 2}, &pooltest.Network{}, &pooltest.Prober{}, pooltest.NewClock(), nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.SetRelays(pooltest.Relays(map[string]int{"se": 5}))
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()

	do := func(method, path, body string) (int, string) {
		req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	dev := `{"id":"d1","private_key":"` + key + `","ipv4":"10.64.0.9/32"}`
	if code, body := do("POST", "/devices", dev); code != 201 || !strings.Contains(body, `"slots": 2`) {
		t.Fatalf("add: %d %s", code, body)
	}
	if code, _ := do("POST", "/devices", dev); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := do("POST", "/devices", `{"id":"x","private_key":"bad","ipv4":"1.2.3.4"}`); code != 400 {
		t.Fatalf("invalid: %d", code)
	}
	code, body := do("GET", "/devices", "")
	if code != 200 || strings.Contains(body, key) || !strings.Contains(body, `"10.64.0.9"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	p.WaitBackground()
	code, body = do("GET", "/status", "")
	var st pool.Status
	if code != 200 || json.Unmarshal([]byte(body), &st) != nil || len(st.Slots) != 2 || st.Relays != 5 {
		t.Fatalf("status: %d %s", code, body)
	}
	if code, _ := do("DELETE", "/devices/d1", ""); code != 202 {
		t.Fatalf("delete: %d", code)
	}
	p.WaitBackground()
	if code, _ := do("DELETE", "/devices/d1", ""); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
}
