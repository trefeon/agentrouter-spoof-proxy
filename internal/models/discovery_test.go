package models

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
)

func TestNewDiscoveryStatic(t *testing.T) {
	d := NewDiscovery([]string{"gpt-5.6-sol", "claude-opus-5", " claude-opus-4-8 "})
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static", got)
	}
	list := d.List()
	if len(list) != 3 {
		t.Fatalf("List() has %d models, want 3", len(list))
	}
	for _, m := range list {
		if m.ID == "" {
			t.Fatal("model id must not be empty")
		}
		if m.Object != "model" {
			t.Errorf("model %q Object = %q, want model", m.ID, m.Object)
		}
		if m.Created != staticCreated {
			t.Errorf("model %q Created = %d, want %d", m.ID, m.Created, staticCreated)
		}
		if m.OwnedBy != "agentrouter" {
			t.Errorf("model %q OwnedBy = %q, want agentrouter", m.ID, m.OwnedBy)
		}
	}
}

func TestDiscoveryListReturnsCopy(t *testing.T) {
	d := NewDiscovery([]string{"a", "b"})
	got := d.List()
	got[0].ID = "MUTATED"
	if d.List()[0].ID == "MUTATED" {
		t.Fatal("List() must return a copy, not the internal slice")
	}
}

func TestDiscoveryNoKeySkipsFetch(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), srv.Client(), host, port, "")
	if called.Load() {
		t.Fatal("Fetch with empty apiKey must not contact the upstream")
	}
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static", got)
	}
}

func TestDiscoveryDynamicSuccess(t *testing.T) {
	var authHeader, uaHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("hit path %q, want /v1/models", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method %s, want GET", r.Method)
		}
		authHeader = r.Header.Get("Authorization")
		uaHeader = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"dynamic-1","object":"model","created":1700000000,"owned_by":"upstream"},{"id":"dynamic-2"}]}`)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), srv.Client(), host, port, "test-key")

	if got := d.Source(); got != "dynamic" {
		t.Fatalf("Source() = %q, want dynamic", got)
	}
	if authHeader != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", authHeader)
	}
	if uaHeader != "agentrouter-spoof-proxy/1.0" {
		t.Errorf("User-Agent = %q, want agentrouter-spoof-proxy/1.0", uaHeader)
	}
	list := d.List()
	if len(list) != 2 {
		t.Fatalf("List() has %d models, want 2", len(list))
	}
	if m := list[0]; m.ID != "dynamic-1" || m.Object != "model" || m.Created != 1700000000 || m.OwnedBy != "upstream" {
		t.Errorf("list[0] = %+v, want id=dynamic-1 created=1700000000 owned_by=upstream", m)
	}
	if m := list[1]; m.ID != "dynamic-2" || m.Created != staticCreated || m.OwnedBy != "agentrouter" {
		t.Errorf("list[1] = %+v, want created/owned_by fallbacks", m)
	}
}

func TestDiscoveryDynamicEmptyArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), srv.Client(), host, port, "key")
	if got := d.Source(); got != "dynamic" {
		t.Fatalf("Source() = %q, want dynamic (empty array is a valid dynamic list)", got)
	}
	if got := d.List(); len(got) != 0 {
		t.Fatalf("List() = %v, want empty", got)
	}
}

func TestDiscoveryFallsBackOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1", "static-2"})

	// A failing upstream must leave the discovery on the static list.
	d.Fetch(context.Background(), srv.Client(), host, port, "key")
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static after failure", got)
	}
	list := d.List()
	if len(list) != 2 || list[0].ID != "static-1" || list[1].ID != "static-2" {
		t.Fatalf("List() = %+v, want the static list", list)
	}
}

func TestDiscoveryFailureAfterDynamicRestoresStatic(t *testing.T) {
	var mode atomic.Int32 // 0 = ok, 1 = failing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"dynamic-1"}]}`)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), srv.Client(), host, port, "key")
	if got := d.Source(); got != "dynamic" {
		t.Fatalf("Source() = %q, want dynamic", got)
	}

	// A later failing fetch must reset to static (Node replaces, not keeps).
	mode.Store(1)
	d.Fetch(context.Background(), srv.Client(), host, port, "key")
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static after failure", got)
	}
	list := d.List()
	if len(list) != 1 || list[0].ID != "static-1" {
		t.Fatalf("List() = %+v, want the static list", list)
	}
}

func TestDiscoveryFallsBackOnBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `this is not json`)
	}))
	t.Cleanup(srv.Close)
	host, port := splitHostPort(t, srv.URL)

	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), srv.Client(), host, port, "key")
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static after bad JSON", got)
	}
	if got := d.List(); len(got) != 1 || got[0].ID != "static-1" {
		t.Fatalf("List() = %+v, want static list", got)
	}
}

func TestDiscoveryFallsBackOnNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused

	host, port := splitHostPort(t, url)
	d := NewDiscovery([]string{"static-1"})
	d.Fetch(context.Background(), &http.Client{}, host, port, "key")
	if got := d.Source(); got != "static" {
		t.Fatalf("Source() = %q, want static after network error", got)
	}
}

func splitHostPort(t *testing.T, u string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(stripScheme(u))
	if err != nil {
		t.Fatalf("split %q: %v", u, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

func stripScheme(u string) string {
	for _, prefix := range []string{"http://", "https://"} {
		if len(u) > len(prefix) && u[:len(prefix)] == prefix {
			return u[len(prefix):]
		}
	}
	return u
}

