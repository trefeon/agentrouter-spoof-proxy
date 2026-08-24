package proxy

import (
	"strings"
	"time"
)

// transportErrorKeywords are the message substrings that make an upstream
// failure retryable when no 5xx status is present (utils.mjs isRetryable).
// Matching is case-sensitive, as in the Node version.
var transportErrorKeywords = []string{
	"socket hang up",
	"timeout",
	"ECONNRESET",
	"ETIMEDOUT",
	"ENETUNREACH",
}

// IsRetryable reports whether an upstream attempt should be retried
// (utils.mjs isRetryable): 5xx statuses retry only when retryOn5xx is set;
// any other status falls through to a case-sensitive scan of the error
// message for transport-error keywords.
func IsRetryable(statusCode int, errorMessage string, retryOn5xx bool) bool {
	if statusCode >= 500 && statusCode <= 599 {
		return retryOn5xx
	}
	for _, kw := range transportErrorKeywords {
		if strings.Contains(errorMessage, kw) {
			return true
		}
	}
	return false
}

// RetryDelay returns the exponential backoff for attempt: base << attempt
// (utils.mjs getRetryDelay: base * 2^attempt).
func RetryDelay(attempt int, baseMs int) time.Duration {
	return time.Duration(baseMs<<attempt) * time.Millisecond
}

// ResponseTimeout returns the adaptive upstream response timeout
// (utils.mjs getResponseTimeout): larger request bodies need more upstream
// processing time. The ladder thresholds are byte-exact equivalents of the
// Node float comparisons (mb > 5 / 2 / 1 / 0.5).
func ResponseTimeout(bodyBytes int, defaultMs int) time.Duration {
	switch {
	case bodyBytes > 5<<20:
		return 300000 * time.Millisecond // 5min for >5MB
	case bodyBytes > 2<<20:
		return 180000 * time.Millisecond // 3min for 2-5MB
	case bodyBytes > 1<<20:
		return 120000 * time.Millisecond // 2min for 1-2MB
	case bodyBytes > 512<<10:
		return 90000 * time.Millisecond // 90s for 500KB-1MB
	default:
		return time.Duration(defaultMs) * time.Millisecond
	}
}

// wafBlockMarkers are Alibaba-Cloud-style WAF block-page markers
// (utils.mjs WAF_BLOCK_MARKERS). `waf.js` is the static challenge script
// referenced by block pages; it catches pages that strip the
// alicdn/block_message markers.
var wafBlockMarkers = []string{"alicdn", "block_message", "renderData", "waf.js"}

// IsWafBlock reports whether a 403/405 upstream body is a WAF block page
// (utils.mjs isWafBlock). Only evaluated on 403/405; markers are matched as
// case-sensitive substrings of the UTF-8 body.
func IsWafBlock(statusCode int, body []byte) bool {
	if statusCode != 405 && statusCode != 403 {
		return false
	}
	html := string(body)
	for _, m := range wafBlockMarkers {
		if strings.Contains(html, m) {
			return true
		}
	}
	return false
}

