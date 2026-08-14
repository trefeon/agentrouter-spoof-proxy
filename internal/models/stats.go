package models

import (
	"sort"
	"strconv"
	"sync"
	"time"
)

// ModelStats is the per-model telemetry snapshot, ported from
// src/models/stats.mjs. AvgMs/AvgChunks are derived at Snapshot time. The
// JSON tags produce the exact Node field names served by /health
// (getModelStats), so 9Router sees byte-compatible keys.
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

// ResultArgs carries the outcome of one proxied request for Result.
type ResultArgs struct {
	StatusCode  int
	DurationMs  int64
	Chunks      int
	Error       string
	EmptyOutput bool
	WafBlock    bool
}

// Recorder accumulates per-model counters, mirroring src/models/stats.mjs.
type Recorder struct {
	mu             sync.Mutex
	stats          map[string]*ModelStats
	slowResponseMs int
}

// NewRecorder returns a recorder that classifies responses slower than
// slowResponseMs as slow (SLOW_RESPONSE_MS).
func NewRecorder(slowResponseMs int) *Recorder {
	return &Recorder{
		stats:          make(map[string]*ModelStats),
		slowResponseMs: slowResponseMs,
	}
}

// Start records the beginning of a request for a model: requests++ and
// lastSeen = now (RFC3339).
func (r *Recorder) Start(modelID string) {
	id := orUnknown(modelID)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(id)
	s.Requests++
	s.LastSeen = time.Now().UTC().Format(time.RFC3339)
}

// Result records the outcome of a request, applying the same counter rules as
// recordModelResult:
//
//   - 2xx && no error      → successes++
//   - error || >=400 || emptyOutput → failures++
//   - emptyOutput → emptyOutputs++; wafBlock → wafBlocks++
//   - 429 → rateLimits++; >=500 → upstreamErrors++
//   - durationMs >= slowResponseMs → slowResponses++
//
// totalMs/maxMs/totalChunks accumulate; lastStatus/lastError/lastSeen update.
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

// Snapshot returns copies of all model stats with AvgMs/AvgChunks derived
// (rounded half-up, matching Math.round), sorted by lastSeen descending —
// models that were never seen (empty lastSeen) sort last.
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

// roundDiv divides total by count, rounding half up (Math.round semantics).
func roundDiv(total int64, count int) int64 {
	if count <= 0 {
		return 0
	}
	return (total + int64(count)/2) / int64(count)
}

// orUnknown maps an empty model id to "unknown", like the Node `|| "unknown"`.
func orUnknown(modelID string) string {
	if modelID == "" {
		return "unknown"
	}
	return modelID
}
