package auth

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// wafCookieNames is the allowlist of WAF and edge session cookies. Only these
// names are forwarded upstream. Everything else is ignored. Mirrors
// WAF_COOKIE_NAMES in src/auth/waf.mjs.
var wafCookieNames = map[string]struct{}{
	"acw_tc":     {},
	"acw_sc__v2": {},
	"acw_sc__v3": {},
	"cdn_sec_tc": {},
}

// warmupHeaders is the browser-like header set sent on GET / warmup so the
// WAF issues its challenge cookie. Mirrors WARMUP_HEADERS in src/auth/waf.mjs.
var warmupHeaders = map[string]string{
	"User-Agent":                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
	"Accept":                    "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
	"Accept-Language":           "en-US,en;q=0.9",
	"Accept-Encoding":           "gzip, deflate, br",
	"Connection":                "keep-alive",
	"Upgrade-Insecure-Requests": "1",
	"Sec-Fetch-Dest":            "document",
	"Sec-Fetch-Mode":            "navigate",
	"Sec-Fetch-Site":            "none",
	"Sec-Fetch-User":            "?1",
}

// warmupBackoff sleeps between warmup attempts (1s, then 2s, mirroring
// sleep(1000 * (attempt + 1))). Package var so tests can shorten it.
var warmupBackoff = []time.Duration{time.Second, 2 * time.Second}

// ExtractWafCookies parses Set-Cookie values and returns valid WAF cookies as
// "name=value" strings (src/auth/waf.mjs extractWafCookies).
//
// Expired or cleared cookies (name=, max-age=0) and entries without "=" are
// skipped. An empty value must never be sent in Cookie, the upstream WAF
// treats it as a failed challenge, which is worse than sending nothing.
func ExtractWafCookies(setCookieValues []string) []string {
	var waf []string
	for _, c := range setCookieValues {
		pair := c
		if i := strings.IndexByte(pair, ';'); i >= 0 {
			pair = pair[:i]
		}
		eq := strings.IndexByte(pair, '=')
		if eq < 1 {
			continue // no `=` or empty name → skip
		}
		name := strings.TrimSpace(pair[:eq])
		value := strings.TrimSpace(pair[eq+1:])
		if _, ok := wafCookieNames[name]; !ok {
			continue
		}
		if value == "" {
			continue
		}
		waf = append(waf, name+"="+value)
	}
	return waf
}

// MergeWafCookies merges cookie lists by name. A fresh value replaces the old
// one for the same name, other names are kept. Pure, neither input is modified
// (src/auth/waf.mjs mergeWafCookies).
func MergeWafCookies(current, fresh []string) []string {
	out := make([]string, 0, len(current)+len(fresh))
	out = append(out, current...)
	for _, c := range fresh {
		eq := strings.IndexByte(c, '=')
		name := ""
		if eq >= 0 {
			name = c[:eq]
		}
		replaced := false
		for i, old := range out {
			oeq := strings.IndexByte(old, '=')
			oldName := ""
			if oeq >= 0 {
				oldName = old[:oeq]
			}
			if oldName == name {
				out[i] = c
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, c)
		}
	}
	return out
}

// Store holds WAF cookies safely for concurrent use. Warmup refreshes it from
// GET / and every upstream response updates it via Capture.
type Store struct {
	mu      sync.RWMutex
	cookies []string

	// Upstream origin the Warmup request is sent to.
	targetProto string
	targetHost  string
	targetPort  int
}

// NewStore returns an empty store. Defaults to https://agentrouter.org:443.
// Call SetTarget before warmup if you need a different origin.
func NewStore() *Store {
	return &Store{
		targetProto: "https",
		targetHost:  "agentrouter.org",
		targetPort:  443,
	}
}

// SetTarget sets the upstream origin for Warmup.
func (s *Store) SetTarget(proto, host string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if proto == "" {
		proto = "https"
	}
	s.targetProto = proto
	s.targetHost = host
	s.targetPort = port
}

// Get returns cookies joined as "a=b; c=d", or "" if empty.
func (s *Store) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.Join(s.cookies, "; ")
}

// Set replaces the cookie list.
func (s *Store) Set(cookies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookies = append([]string(nil), cookies...)
}

// Capture merges WAF cookies from an upstream response into the store, keyed
// by name. No-op if the response has no valid WAF cookie. This picks up a
// rotated acw_tc or cdn_sec_tc from an API response right away, without
// waiting for the next warmup (src/auth/waf.mjs captureWafCookies).
func (s *Store) Capture(resp *http.Response) {
	if resp == nil {
		return
	}
	fresh := ExtractWafCookies(resp.Header.Values("Set-Cookie"))
	if len(fresh) == 0 {
		return
	}
	s.mu.Lock()
	s.cookies = MergeWafCookies(s.cookies, fresh)
	s.mu.Unlock()
}

// Warmup fetches GET / up to 3 times to get WAF cookies. It uses a fresh,
// non-pooled client (Node agent:false equivalent, never the shared Transport),
// 10s per attempt, with 1s and 2s backoff. New cookies are merged, never
// replaced, so traffic-only names survive. Returns true if any cookie was
// captured. Log lines keep Node prefixes: "WARMUP -> 200 cookies: N" and
// "WARMUP failed after 3 attempts".
func (s *Store) Warmup(ctx context.Context) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.RLock()
	u := s.warmupURL()
	s.mu.RUnlock()

	for attempt := 0; attempt < 3; attempt++ {
		cookies, err := warmupAttempt(ctx, u)
		if err != nil && attempt == 2 {
			slog.Warn(fmt.Sprintf("WARMUP attempt 3/3 failed: %v", err))
		}
		if len(cookies) > 0 {
			s.mu.Lock()
			s.cookies = MergeWafCookies(s.cookies, cookies)
			s.mu.Unlock()
			slog.Info(fmt.Sprintf("WARMUP → 200 cookies: %d", len(cookies)))
			return true
		}
		if attempt < 2 {
			if !sleepCtx(ctx, warmupBackoff[attempt]) {
				return false
			}
		}
	}
	slog.Warn("WARMUP failed after 3 attempts")
	return false
}

func (s *Store) warmupURL() string {
	return s.targetProto + "://" + net.JoinHostPort(s.targetHost, strconv.Itoa(s.targetPort)) + "/"
}

// warmupAttempt runs one GET / with browser headers and a fresh client.
// Any HTTP status counts as success, only transport errors fail the attempt.
func warmupAttempt(ctx context.Context, u string) ([]string, error) {
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true}, // mirrors Node agent:false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range warmupHeaders {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// Drain for connection hygiene; the body is irrelevant, only cookies are.
	_, _ = io.Copy(io.Discard, resp.Body)
	return ExtractWafCookies(resp.Header.Values("Set-Cookie")), nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
