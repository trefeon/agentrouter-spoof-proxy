package models

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// ModelStats is the per-model telemetry snapshot. Ported from
// src/models/stats.mjs. AvgMs and AvgChunks are computed at Snapshot time.
// JSON tags match Node field names from /health (getModelStats), so 9Router
// sees the same keys.
type ModelStats struct {
	Model          string `json:"model"`
	Requests       int    `json:"requests"`
	Successes      int    `json:"successes"`
	Failures       int    `json:"failures"`
	EmptyOutputs   int    `json:"emptyOutputs"`
	SlowResponses  int    `json:"slowResponses"`
	WafBlocks      int    `json:"wafBlocks"`
	RateLimits     int    `json:"rateLimits"`
	UpstreamErrors int    `json:"upstreamErrors"`
	TotalMs        int64  `json:"totalMs"`
	MaxMs          int64  `json:"maxMs"`
	TotalChunks    int64  `json:"totalChunks"`
	LastStatus     string `json:"lastStatus"`
	LastError      string `json:"lastError"`
	LastSeen       string `json:"lastSeen"`
	AvgMs          int64  `json:"avgMs"`
	AvgChunks      int64  `json:"avgChunks"`
}

// ResultArgs is the input for Recorder.Result.
type ResultArgs struct {
	StatusCode  int
	DurationMs  int64
	Chunks      int
	Error       string
	EmptyOutput bool
	WafBlock    bool
}

// Recorder tracks per-model counters. Mirrors src/models/stats.mjs.
type Recorder struct {
	mu             sync.Mutex
	stats          map[string]*ModelStats
	slowResponseMs int
}

// NewRecorder creates a recorder. Responses slower than slowResponseMs
// count as slow (SLOW_RESPONSE_MS).
func NewRecorder(slowResponseMs int) *Recorder {
	return &Recorder{
		stats:          make(map[string]*ModelStats),
		slowResponseMs: slowResponseMs,
	}
}

// Start records that a request started. Increments requests and sets
// lastSeen to now (RFC3339).
func (r *Recorder) Start(modelID string) {
	id := orUnknown(modelID)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(id)
	s.Requests++
	s.LastSeen = time.Now().UTC().Format(time.RFC3339)
}

// Result records one request outcome, same rules as recordModelResult:
//
//   2xx and no error counts as success
//   error, >=400 or emptyOutput counts as failure
//   emptyOutput increments emptyOutputs, wafBlock increments wafBlocks
//   429 increments rateLimits, >=500 increments upstreamErrors
//   durationMs >= slowResponseMs increments slowResponses
//
// totalMs, maxMs and totalChunks are accumulated, lastStatus and lastError
// are updated.
func (r *Recorder) Result(modelID string, args ResultArgs) {
	id := orUnknown(modelID)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(id)
	if args.StatusCode > 0 {
		s.LastStatus = strconv.Itoa(args.StatusCode)
	} else {
		s.LastStatus = ""
	}
	s.LastError = args.Error
	s.LastSeen = time.Now().UTC().Format(time.RFC3339)
	s.TotalMs += args.DurationMs
	if args.DurationMs > s.MaxMs {
		s.MaxMs = args.DurationMs
	}
	s.TotalChunks += int64(args.Chunks)

	if args.StatusCode >= 200 && args.StatusCode < 300 && args.Error == "" {
		s.Successes++
	}
	if args.Error != "" || args.StatusCode >= 400 || args.EmptyOutput {
		s.Failures++
	}
	if args.EmptyOutput {
		s.EmptyOutputs++
	}
	if args.WafBlock {
		s.WafBlocks++
	}
	if args.StatusCode == 429 {
		s.RateLimits++
	}
	if args.StatusCode >= 500 {
		s.UpstreamErrors++
	}
	if args.DurationMs >= int64(r.slowResponseMs) {
		s.SlowResponses++
	}
}

// Snapshot returns a copy of all stats with AvgMs and AvgChunks derived
// (rounded half up, like Math.round). Sorted by lastSeen descending, unseen
// models (empty lastSeen) sort last.
func (r *Recorder) Snapshot() []ModelStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ModelStats, 0, len(r.stats))
	for _, s := range r.stats {
		c := *s
		if c.Requests > 0 {
			c.AvgMs = roundDiv(c.TotalMs, c.Requests)
			c.AvgChunks = roundDiv(c.TotalChunks, c.Requests)
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].LastSeen, out[j].LastSeen
		if a == "" {
			return false // empty lastSeen sorts last
		}
		if b == "" {
			return true
		}
		return a > b
	})
	return out
}

func (r *Recorder) get(modelID string) *ModelStats {
	s, ok := r.stats[modelID]
	if !ok {
		s = &ModelStats{Model: modelID}
		r.stats[modelID] = s
	}
	return s
}

// roundDiv divides total by count, rounding half up like Math.round.
func roundDiv(total int64, count int) int64 {
	if count <= 0 {
		return 0
	}
	return (total + int64(count)/2) / int64(count)
}

// orUnknown returns "unknown" for an empty model ID, like Node || "unknown".
func orUnknown(modelID string) string {
	if modelID == "" {
		return "unknown"
	}
	return modelID
}
