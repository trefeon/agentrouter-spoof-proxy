package models

import (
	"reflect"
	"testing"
)

func TestRecorderStartIncrementsAndStamps(t *testing.T) {
	r := NewRecorder(30000)
	r.Start("m")
	s := snapshotModel(t, r, "m")
	if s.Requests != 1 {
		t.Fatalf("Requests = %d, want 1", s.Requests)
	}
	if s.LastSeen == "" {
		t.Fatal("Start must set LastSeen")
	}
}

func TestRecorderUnknownModel(t *testing.T) {
	r := NewRecorder(30000)
	r.Start("")
	r.Result("", ResultArgs{StatusCode: 200, DurationMs: 5})
	s := snapshotModel(t, r, "unknown")
	if s.Requests != 1 || s.Successes != 1 {
		t.Fatalf("unknown-model stats = %+v, want requests=1 successes=1", s)
	}
}

func TestRecorderAccumulatesAllCounters(t *testing.T) {
	r := NewRecorder(100) // slowResponseMs = 100

	// 1: success, slow (200ms >= 100ms), WAF-blocked.
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 200, DurationMs: 200, Chunks: 10, WafBlock: true})

	// 2: rate-limited.
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 429, DurationMs: 50})

	// 3: upstream 5xx with error.
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 502, DurationMs: 30, Error: "socket hang up"})

	// 4: 2xx but empty output.
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 200, DurationMs: 10, EmptyOutput: true})

	s := snapshotModel(t, r, "m")
	if s.Requests != 4 {
		t.Errorf("Requests = %d, want 4", s.Requests)
	}
	if s.Successes != 2 { // 200-ok + 200-empty
		t.Errorf("Successes = %d, want 2", s.Successes)
	}
	if s.Failures != 3 { // 429, 502, empty
		t.Errorf("Failures = %d, want 3", s.Failures)
	}
	if s.EmptyOutputs != 1 {
		t.Errorf("EmptyOutputs = %d, want 1", s.EmptyOutputs)
	}
	if s.SlowResponses != 1 {
		t.Errorf("SlowResponses = %d, want 1", s.SlowResponses)
	}
	if s.WafBlocks != 1 {
		t.Errorf("WafBlocks = %d, want 1", s.WafBlocks)
	}
	if s.RateLimits != 1 {
		t.Errorf("RateLimits = %d, want 1", s.RateLimits)
	}
	if s.UpstreamErrors != 1 {
		t.Errorf("UpstreamErrors = %d, want 1", s.UpstreamErrors)
	}
	if s.TotalMs != 290 {
		t.Errorf("TotalMs = %d, want 290", s.TotalMs)
	}
	if s.MaxMs != 200 {
		t.Errorf("MaxMs = %d, want 200", s.MaxMs)
	}
	if s.TotalChunks != 10 {
		t.Errorf("TotalChunks = %d, want 10", s.TotalChunks)
	}
	if s.LastStatus != "200" {
		t.Errorf("LastStatus = %q, want 200", s.LastStatus)
	}
	if s.LastError != "" {
		t.Errorf("LastError = %q, want empty", s.LastError)
	}
	if s.LastSeen == "" {
		t.Error("LastSeen must be set")
	}
	// Averages: round(290/4)=73, round(10/4)=3 (Math.round semantics).
	if s.AvgMs != 73 {
		t.Errorf("AvgMs = %d, want 73", s.AvgMs)
	}
	if s.AvgChunks != 3 {
		t.Errorf("AvgChunks = %d, want 3", s.AvgChunks)
	}
}

func TestRecorderResultDefaults(t *testing.T) {
	r := NewRecorder(30000)
	r.Start("m")
	r.Result("m", ResultArgs{}) // zero args: status 0, duration 0
	s := snapshotModel(t, r, "m")
	if s.Successes != 0 || s.Failures != 0 {
		t.Fatalf("zero result must not count as success or failure, got %+v", s)
	}
	if s.LastStatus != "" {
		t.Errorf("LastStatus = %q, want empty for status 0", s.LastStatus)
	}
}

func TestRecorderSlowThresholdBoundary(t *testing.T) {
	r := NewRecorder(100)
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 200, DurationMs: 99})
	r.Start("m")
	r.Result("m", ResultArgs{StatusCode: 200, DurationMs: 100})
	s := snapshotModel(t, r, "m")
	if s.SlowResponses != 1 {
		t.Fatalf("SlowResponses = %d, want 1 (>= threshold)", s.SlowResponses)
	}
}

func TestSnapshotSortsByLastSeenDesc(t *testing.T) {
	r := NewRecorder(100)
	r.Start("newer")
	r.Start("older")
	r.Start("never-seen")

	r.mu.Lock()
	r.stats["newer"].LastSeen = "2026-02-01T00:00:00Z"
	r.stats["older"].LastSeen = "2026-01-01T00:00:00Z"
	// "never-seen" must truly have no LastSeen (Start() set one, so clear it).
	r.stats["never-seen"].LastSeen = ""
	r.mu.Unlock()

	snap := r.Snapshot()
	var ids []string
	for _, s := range snap {
		ids = append(ids, s.Model)
	}
	want := []string{"newer", "older", "never-seen"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("Snapshot order = %v, want %v", ids, want)
	}
}

func TestSnapshotAveragesZeroWhenNoStart(t *testing.T) {
	r := NewRecorder(100)
	// A Result without a Start leaves Requests at 0, so averages must be 0,
	// mirroring the Node `s.requests ? Math.round(...) : 0` guard.
	r.Result("res-only", ResultArgs{StatusCode: 200, DurationMs: 10})
	s := snapshotModel(t, r, "res-only")
	if s.Requests != 0 {
		t.Fatalf("Requests = %d, want 0 (no Start)", s.Requests)
	}
	if s.AvgMs != 0 || s.AvgChunks != 0 {
		t.Fatalf("AvgMs/AvgChunks = %d/%d, want 0/0 when no Start", s.AvgMs, s.AvgChunks)
	}
	if s.LastStatus != "200" {
		t.Fatalf("LastStatus = %q, want 200", s.LastStatus)
	}
}

// snapshotModel finds one model in a snapshot by ID, failing the test if the
// ID is absent or not unique.
func snapshotModel(t *testing.T, r *Recorder, id string) ModelStats {
	t.Helper()
	snap := r.Snapshot()
	for _, s := range snap {
		if s.Model == id {
			return s
		}
	}
	t.Fatalf("model %q not found in snapshot %+v", id, snap)
	return ModelStats{}
}
