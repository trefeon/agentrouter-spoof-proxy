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

// wafCookieNames is the allowlist of WAF/edge session-cookie names. Set-Cookie
// entries with any other name are treated as non-WAF (browser/app cookies) and
// never shipped upstream. Mirrors WAF_COOKIE_NAMES in src/auth/waf.mjs.
var wafCookieNames = map[string]struct{}{
	"acw_tc":     {},
	"acw_sc__v2": {},
	"acw_sc__v3": {},
	"cdn_sec_tc": {},
}

// warmupHeaders mirrors WARMUP_HEADERS in src/auth/waf.mjs: the browser-ish
// header set sent on the warmup GET "/" so the WAF issues its challenge cookie.
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

// warmupBackoff is the sleep between warmup attempts (1s, 2s, mirroring
// `sleep(1000 * (attempt + 1))`). A package variable so tests can shorten it.
var warmupBackoff = []time.Duration{time.Second, 2 * time.Second}

// ExtractWafCookies parses set-cookie header values and returns the valid WAF
// cookies as "name=value" strings (src/auth/waf.mjs extractWafCookies).
//
// Expired/cleared cookies (`name=; max-age=0`) and entries without a `=` are
// skipped: an empty value must never be shipped inside the Cookie header —
// the upstream WAF would read it as a *failed* challenge (worse than no
// cookie at all).
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

// MergeWafCookies merges cookie lists keyed by cookie NAME: a fresh value
// replaces the old one for the same name while unrelated names are preserved
// (src/auth/waf.mjs mergeWafCookies). Pure: neither input is modified.
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

// Store is the thread-safe WAF cookie holder. Warmup refreshes it against the
// upstream "/" page and every upstream response feeds it via Capture.
type Store struct {
	mu      sync.RWMutex
	cookies []string

	// Upstream origin the Warmup request is sent to.
	targetProto string
	targetHost  string
	targetPort  int
}

// NewStore returns an empty store. The warmup target defaults to the config
// defaults (https://agentrouter.org:443); override with SetTarget before
// starting the warmup goroutine.
func NewStore() *Store {
	return &Store{
		targetProto: "https",
		targetHost:  "agentrouter.org",
		targetPort:  443,
	}
}

// SetTarget configures the upstream origin used by Warmup.
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

// Get returns the current cookies joined with "; ", or "" when empty.
func (s *Store) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.Join(s.cookies, "; ")
}

// Set replaces the entire cookie list.
func (s *Store) Set(cookies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cookies = append([]string(nil), cookies...)
}

// Capture merges any WAF cookies seen on an upstream response into the store,
// keyed by cookie NAME. No-op when the response carries no valid WAF cookie —
// this lets a rotated acw_tc/cdn_sec_tc on an API response be picked up
// immediately instead of waiting for the next warmup cycle
// (src/auth/waf.mjs captureWafCookies).
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

// Warmup performs the WAF warmup: up to 3 GET "/" attempts against the target
// using a FRESH, non-pooled client (the Node agent:false equivalent — never
// the shared Transport), 10s per attempt, 1s/2s backoff in between. Captured
// cookies are merged — never replaced — into the store so traffic-only names
// survive. Returns true when at least one cookie was captured, else false.
//
// Log lines keep the Node grep prefixes: "WARMUP → 200 cookies: N" and
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

// warmupAttempt runs one warmup GET "/" with the browser header set and a
// fresh client. A response is a success regardless of status code — only
// transport errors fail the attempt.
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
