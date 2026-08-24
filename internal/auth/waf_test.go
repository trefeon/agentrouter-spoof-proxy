package auth

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExtractWafCookies(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"valid acw_tc with attributes", []string{"acw_tc=abc123; Path=/; HttpOnly"}, []string{"acw_tc=abc123"}},
		{"valid cdn_sec_tc", []string{"cdn_sec_tc=xyz; Path=/; SameSite=Lax"}, []string{"cdn_sec_tc=xyz"}},
		{"valid acw_sc__v2", []string{"acw_sc__v2=v2val"}, []string{"acw_sc__v2=v2val"}},
		{"valid acw_sc__v3", []string{"acw_sc__v3=v3val"}, []string{"acw_sc__v3=v3val"}},
		{"non-waf name skipped", []string{"session=abc; Path=/"}, nil},
		{"empty value skipped", []string{"acw_tc=; Max-Age=0"}, nil},
		{"empty value skipped with spaces", []string{"acw_tc=   ; Max-Age=0"}, nil},
		{"no equals skipped", []string{"acw_sc__v2blah"}, nil},
		{"empty name skipped", []string{"=value"}, nil},
		{"array input with mixed entries", []string{"acw_tc=keep; Path=/", "session=drop"}, []string{"acw_tc=keep"}},
		{"multiple waf cookies", []string{"acw_tc=a; Path=/", "cdn_sec_tc=b; Path=/"}, []string{"acw_tc=a", "cdn_sec_tc=b"}},
		{"value trimmed", []string{"acw_tc=  42  ; Path=/"}, []string{"acw_tc=42"}},
		{"empty input", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractWafCookies(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExtractWafCookies(%v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestMergeWafCookies(t *testing.T) {
	tests := []struct {
		name    string
		current []string
		fresh   []string
		want    []string
	}{
		{"fresh replaces same name", []string{"acw_tc=old", "cdn_sec_tc=keep"}, []string{"acw_tc=new"}, []string{"acw_tc=new", "cdn_sec_tc=keep"}},
		{"unrelated preserved", []string{"acw_tc=old"}, []string{"cdn_sec_tc=new"}, []string{"acw_tc=old", "cdn_sec_tc=new"}},
		{"no dupes", []string{"acw_tc=a"}, []string{"acw_tc=b"}, []string{"acw_tc=b"}},
		{"duplicate names in fresh collapse", nil, []string{"acw_tc=a", "acw_tc=b"}, []string{"acw_tc=b"}},
		{"empty fresh", []string{"acw_tc=a"}, nil, []string{"acw_tc=a"}},
		{"empty current", nil, []string{"acw_tc=a"}, []string{"acw_tc=a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MergeWafCookies(tt.current, tt.fresh)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("MergeWafCookies(%v, %v) = %v, want %v", tt.current, tt.fresh, got, tt.want)
			}
		})
	}
}

func TestMergeWafCookiesIsPure(t *testing.T) {
	current := []string{"acw_tc=a", "cdn_sec_tc=b"}
	fresh := []string{"acw_tc=c"}
	curCopy := append([]string(nil), current...)
	freshCopy := append([]string(nil), fresh...)
	MergeWafCookies(current, fresh)
	if !reflect.DeepEqual(current, curCopy) || !reflect.DeepEqual(fresh, freshCopy) {
		t.Fatal("MergeWafCookies must not mutate its inputs")
	}
}

func TestStoreGetEmpty(t *testing.T) {
	s := NewStore()
	if got := s.Get(); got != "" {
		t.Fatalf("Get() = %q, want empty", got)
	}
}

func TestStoreSetReplaces(t *testing.T) {
	s := NewStore()
	s.Set([]string{"acw_tc=a", "cdn_sec_tc=b"})
	s.Set([]string{"acw_tc=b"})
	if got := s.Get(); got != "acw_tc=b" {
		t.Fatalf("Get() = %q, want %q", got, "acw_tc=b")
	}
}

// captureSrv spins up an httptest server that sets the given Set-Cookie
// headers on every request.
func captureSrv(t *testing.T, setCookies []string) (*httptest.Server, string, int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range setCookies {
			w.Header().Add("Set-Cookie", c)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("parse srv url: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse srv port: %v", err)
	}
	return srv, host, port
}

func TestStoreCaptureFromResponse(t *testing.T) {
	srv, _, _ := captureSrv(t, []string{"acw_tc=serverval; Path=/", "session=nonwaf; Path=/"})
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	s := NewStore()
	s.Capture(resp)
	got := s.Get()
	if !strings.Contains(got, "acw_tc=serverval") {
		t.Fatalf("store should contain acw_tc=serverval, got %q", got)
	}
	if strings.Contains(got, "session=nonwaf") {
		t.Fatalf("store must not contain non-WAF cookies, got %q", got)
	}
}

func TestStoreCaptureIgnoresNonWaf(t *testing.T) {
	srv, _, _ := captureSrv(t, []string{"session=x; Path=/"})
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	s := NewStore()
	s.Capture(resp)
	if got := s.Get(); got != "" {
		t.Fatalf("Get() = %q, want empty (non-WAF cookie ignored)", got)
	}
}

func TestStoreCaptureMergesByName(t *testing.T) {
	srv, _, _ := captureSrv(t, []string{"acw_tc=fresh; Path=/"})
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	s := NewStore()
	s.Set([]string{"acw_tc=stale", "cdn_sec_tc=traffic"})
	s.Capture(resp)
	got := s.Get()
	if !strings.Contains(got, "acw_tc=fresh") {
		t.Fatalf("fresh acw_tc should replace stale, got %q", got)
	}
	if strings.Contains(got, "acw_tc=stale") {
		t.Fatalf("stale acw_tc must be replaced, got %q", got)
	}
	if !strings.Contains(got, "cdn_sec_tc=traffic") {
		t.Fatalf("cdn_sec_tc should be preserved, got %q", got)
	}
}

func TestStoreWarmupSuccess(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/" {
			t.Errorf("warmup hit path %q, want /", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("warmup method %s, want GET", r.Method)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Mozilla/5.0") {
			t.Errorf("warmup User-Agent = %q, want browser UA", ua)
		}
		w.Header().Add("Set-Cookie", "acw_tc=warmupval; Path=/; HttpOnly")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	s := NewStore()
	s.SetTarget("http", host, port)
	if ok := s.Warmup(context.Background()); !ok {
		t.Fatal("Warmup should report success when cookies are captured")
	}
	if !strings.Contains(s.Get(), "acw_tc=warmupval") {
		t.Fatalf("store should hold the warmup cookie, got %q", s.Get())
	}
	if hits.Load() != 1 {
		t.Fatalf("warmup made %d attempts, want 1 (early return on success)", hits.Load())
	}
}

func TestStoreWarmupMergesNotReplaces(t *testing.T) {
	srv, host, port := captureSrv(t, []string{"acw_tc=warmupval; Path=/"})
	_ = srv // server lifecycle is handled by captureSrv's cleanup
	s := NewStore()
	s.SetTarget("http", host, port)
	s.Set([]string{"cdn_sec_tc=traffic"})
	if ok := s.Warmup(context.Background()); !ok {
		t.Fatal("Warmup should succeed")
	}
	got := s.Get()
	if !strings.Contains(got, "acw_tc=warmupval") {
		t.Fatalf("warmup cookie missing, got %q", got)
	}
	if !strings.Contains(got, "cdn_sec_tc=traffic") {
		t.Fatalf("traffic-only cookie must survive warmup merge, got %q", got)
	}
}

func TestStoreWarmupNoCookies(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	oldBackoff := warmupBackoff
	warmupBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { warmupBackoff = oldBackoff })

	s := NewStore()
	s.SetTarget("http", host, port)
	if ok := s.Warmup(context.Background()); ok {
		t.Fatal("Warmup without cookies must report failure")
	}
	if hits.Load() != 3 {
		t.Fatalf("warmup made %d attempts, want 3", hits.Load())
	}
	if got := s.Get(); got != "" {
		t.Fatalf("store must stay empty, got %q", got)
	}
}

func TestStoreWarmupTransportError(t *testing.T) {
	// A listener that accepts then immediately closes: the request fails at
	// the transport layer, exercising the retry path.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	oldBackoff := warmupBackoff
	warmupBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { warmupBackoff = oldBackoff })

	s := NewStore()
	s.SetTarget("http", host, port)
	s.Set([]string{"acw_tc=keep"})
	if ok := s.Warmup(context.Background()); ok {
		t.Fatal("Warmup against a failing target must report failure")
	}
	if got := s.Get(); got != "acw_tc=keep" {
		t.Fatalf("existing cookies must be preserved, got %q", got)
	}
}

func TestStoreWarmupContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	port, _ := strconv.Atoi(portStr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled: warmup must not block on backoff

	s := NewStore()
	s.SetTarget("http", host, port)
	start := time.Now()
	if ok := s.Warmup(ctx); ok {
		t.Fatal("Warmup with canceled ctx must report failure")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Warmup must return promptly on canceled context")
	}
}

