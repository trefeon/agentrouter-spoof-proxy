package checkin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestLogger returns a logger that writes nowhere.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRun records the last invocation and optionally blocks until released.
type fakeRun struct {
	mu     sync.Mutex
	calls  int
	cmd    string
	args   []string
	dir    string
	code   int
	output []byte
	err    error
	block  chan struct{}
}

func (f *fakeRun) call(ctx context.Context, cmd string, args []string, dir string) (int, []byte, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return -1, nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls++
	f.cmd, f.args, f.dir = cmd, args, dir
	f.mu.Unlock()
	return f.code, f.output, f.err
}

// setRunCommand swaps the package runCommand var for the test duration.
func setRunCommand(t *testing.T, fn func(context.Context, string, []string, string) (int, []byte, error)) {
	t.Helper()
	old := runCommand
	runCommand = fn
	t.Cleanup(func() { runCommand = old })
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within 5s")
}

func TestStatusNotConfigured(t *testing.T) {
	m := New("uv", "run python checkin.py", "", false, "08:00", "22:00", newTestLogger())
	st := m.Status()
	if st.Configured {
		t.Error("Configured = true, want false for empty workdir")
	}
	if st.Running {
		t.Error("Running = true before any run")
	}
	if st.ScheduleEnabled {
		t.Error("ScheduleEnabled = true without a configured workdir")
	}
	if st.LastRun != nil {
		t.Error("LastRun should be nil before any run")
	}
	if err := m.RunNow(context.Background()); err != ErrNotConfigured {
		t.Errorf("RunNow err = %v, want ErrNotConfigured", err)
	}
}

func TestStatusBeforeRun(t *testing.T) {
	m := New("uv", "run python checkin.py", "/tmp/ci", true, "08:00", "22:00", newTestLogger())
	st := m.Status()
	if !st.Configured || st.Running {
		t.Errorf("Configured=%v Running=%v, want true/false", st.Configured, st.Running)
	}
	if st.Cmd != "uv" || st.Workdir != "/tmp/ci" {
		t.Errorf("Cmd=%q Workdir=%q", st.Cmd, st.Workdir)
	}
	if !st.ScheduleEnabled {
		t.Error("ScheduleEnabled = false, want true")
	}
	if st.WindowStart != "08:00" || st.WindowEnd != "22:00" {
		t.Errorf("window = %s..%s, want 08:00..22:00", st.WindowStart, st.WindowEnd)
	}
	if st.NextRun != "" {
		t.Errorf("NextRun = %q before Start, want empty", st.NextRun)
	}
	if st.LastRun != nil {
		t.Error("LastRun should be nil before any run")
	}
}

func TestRunNowExecutes(t *testing.T) {
	f := &fakeRun{code: 0, output: []byte("all good")}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "/tmp/ci", false, "08:00", "22:00", newTestLogger())
	if err := m.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	waitFor(t, func() bool { return !m.Status().Running })

	f.mu.Lock()
	cmd, args, dir, calls := f.cmd, f.args, f.dir, f.calls
	f.mu.Unlock()
	if calls != 1 {
		t.Errorf("runCommand calls = %d, want 1", calls)
	}
	if cmd != "uv" || dir != "/tmp/ci" {
		t.Errorf("runCommand got cmd=%q dir=%q", cmd, dir)
	}
	if got := strings.Join(args, " "); got != "run python checkin.py" {
		t.Errorf("args = %q, want base args without --random-wait", got)
	}

	st := m.Status()
	if st.LastRun == nil {
		t.Fatal("LastRun = nil after run")
	}
	if !st.LastRun.OK || st.LastRun.ExitCode != 0 {
		t.Errorf("LastRun OK=%v ExitCode=%d, want true/0", st.LastRun.OK, st.LastRun.ExitCode)
	}
	if st.LastRun.Output != "all good" {
		t.Errorf("LastRun.Output = %q", st.LastRun.Output)
	}
	if st.LastRun.Started == "" || st.LastRun.Finished == "" {
		t.Error("Started/Finished should be RFC3339 timestamps")
	}
	if _, err := time.Parse(time.RFC3339, st.LastRun.Started); err != nil {
		t.Errorf("Started %q is not RFC3339: %v", st.LastRun.Started, err)
	}
}

func TestRunFailureRecorded(t *testing.T) {
	f := &fakeRun{code: 3}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "/tmp/ci", false, "08:00", "22:00", newTestLogger())
	if err := m.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	waitFor(t, func() bool { return !m.Status().Running })
	st := m.Status()
	if st.LastRun == nil || st.LastRun.OK || st.LastRun.ExitCode != 3 {
		t.Errorf("LastRun = %+v, want OK=false ExitCode=3", st.LastRun)
	}
}

func TestErrAlreadyRunning(t *testing.T) {
	block := make(chan struct{})
	f := &fakeRun{block: block}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "/tmp/ci", false, "08:00", "22:00", newTestLogger())
	if err := m.RunNow(context.Background()); err != nil {
		t.Fatalf("first RunNow: %v", err)
	}
	if err := m.RunNow(context.Background()); err != ErrAlreadyRunning {
		t.Fatalf("second RunNow err = %v, want ErrAlreadyRunning", err)
	}
	close(block)
	waitFor(t, func() bool { return !m.Status().Running })
	if err := m.RunNow(context.Background()); err != nil {
		t.Fatalf("RunNow after completion: %v", err)
	}
	waitFor(t, func() bool { return !m.Status().Running })
}

func TestSchedulerRunAppendsRandomWait(t *testing.T) {
	f := &fakeRun{code: 0}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "/tmp/ci", true, "08:00", "22:00", newTestLogger())
	if err := m.start(context.Background(), []string{"--random-wait"}); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitFor(t, func() bool { return !m.Status().Running })
	f.mu.Lock()
	args := f.args
	f.mu.Unlock()
	if got := strings.Join(args, " "); got != "run python checkin.py --random-wait" {
		t.Errorf("scheduled args = %q, want --random-wait appended", got)
	}
}

func TestStartNoOpWhenNotConfigured(t *testing.T) {
	f := &fakeRun{}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "", true, "08:00", "22:00", newTestLogger())
	m.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("runCommand called %d times, want 0 for unconfigured scheduler", calls)
	}
	if st := m.Status(); st.Running || st.NextRun != "" {
		t.Errorf("status after no-op Start: Running=%v NextRun=%q", st.Running, st.NextRun)
	}
}

func TestStartNoOpWhenSchedulingDisabled(t *testing.T) {
	f := &fakeRun{}
	setRunCommand(t, f.call)
	m := New("uv", "run python checkin.py", "/tmp/ci", false, "08:00", "22:00", newTestLogger())
	m.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("runCommand called %d times, want 0 with scheduling disabled", calls)
	}
}

func TestStartGuardedAgainstDouble(t *testing.T) {
	f := &fakeRun{code: 0}
	setRunCommand(t, f.call)
	// Window at least an hour ahead, so the scheduler cannot fire within the
	// sleep below no matter what time the test runs.
	future := time.Now().Add(time.Hour).Format("15:04")
	m := New("uv", "run python checkin.py", "/tmp/ci", true, future, "22:00", newTestLogger())
	m.Start(context.Background())
	m.Start(context.Background())
	time.Sleep(20 * time.Millisecond)
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if calls != 0 {
		t.Errorf("runCommand called %d times, want 0 (scheduler waits for the window)", calls)
	}
}

func TestNextRunComputation(t *testing.T) {
	loc := time.FixedZone("t", 0)
	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "future window today",
			now:  time.Date(2026, 8, 28, 7, 0, 0, 0, loc),
			want: time.Date(2026, 8, 28, 8, 0, 0, 0, loc),
		},
		{
			name: "past window tomorrow",
			now:  time.Date(2026, 8, 28, 9, 0, 0, 0, loc),
			want: time.Date(2026, 8, 29, 8, 0, 0, 0, loc),
		},
		{
			name: "exactly at window tomorrow",
			now:  time.Date(2026, 8, 28, 8, 0, 0, 0, loc),
			want: time.Date(2026, 8, 29, 8, 0, 0, 0, loc),
		},
		{
			name: "month boundary",
			now:  time.Date(2026, 8, 31, 23, 59, 0, 0, loc),
			want: time.Date(2026, 9, 1, 8, 0, 0, 0, loc),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextWindowStart(tc.now, "08:00"); !got.Equal(tc.want) {
				t.Errorf("nextWindowStart = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOutputTailCappedSingleWrite(t *testing.T) {
	w := &cappedWriter{}
	blob := bytes.Repeat([]byte("x"), 20000)
	if _, err := w.Write(blob); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(w.buf) != maxOutput {
		t.Fatalf("len = %d, want %d", len(w.buf), maxOutput)
	}
	if !bytes.Equal(w.buf, blob[len(blob)-maxOutput:]) {
		t.Error("buffer should hold the tail of the output")
	}
}

func TestOutputTailCappedAcrossWrites(t *testing.T) {
	w := &cappedWriter{}
	chunk := bytes.Repeat([]byte("y"), 6000)
	for i := 0; i < 3; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatalf("Write %d: %v", i, err)
		}
	}
	want := bytes.Repeat([]byte("y"), maxOutput)
	if !bytes.Equal(w.buf, want) {
		t.Error("tail across writes should keep the last 8 KiB")
	}
}

func TestCappedWriterConcurrent(t *testing.T) {
	w := &cappedWriter{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, _ = w.Write([]byte("abcdefghij"))
			}
		}()
	}
	wg.Wait()
	if len(w.buf) > maxOutput {
		t.Fatalf("len = %d, want <= %d", len(w.buf), maxOutput)
	}
}
