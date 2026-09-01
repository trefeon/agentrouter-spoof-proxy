// Package checkin runs the periodic check-in command on a schedule and
// exposes status through the dashboard API.
package checkin

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxOutput caps the retained command output tail in bytes.
const maxOutput = 8 * 1024

// runCommand executes cmd with args in dir, capturing combined output capped
// to maxOutput bytes. It returns the process exit code (or -1 when the
// command did not run to completion) and the output tail. Tests override this
// package var with a fake.
var runCommand = func(ctx context.Context, cmd string, args []string, dir string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, args...)
	c.Dir = dir
	w := &cappedWriter{}
	c.Stdout = w
	c.Stderr = w
	err := c.Run()
	code := 0
	if err != nil {
		code = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	return code, append([]byte(nil), w.buf...), err
}

var (
	// ErrNotConfigured reports a check-in run with no workdir configured.
	ErrNotConfigured = errors.New("check-in not configured")
	// ErrAlreadyRunning reports a check-in run while another is in flight.
	ErrAlreadyRunning = errors.New("check-in already running")
)

// RunResult records one check-in run. Finished is empty while the run is
// still in flight.
type RunResult struct {
	Started  string `json:"started"`
	Finished string `json:"finished"`
	ExitCode int    `json:"exitCode"`
	OK       bool   `json:"ok"`
	Output   string `json:"output"`
}

// Status is the snapshot served by GET /api/checkin/status.
type Status struct {
	Configured      bool       `json:"configured"`
	Running         bool       `json:"running"`
	Cmd             string     `json:"cmd"`
	Workdir         string     `json:"workdir"`
	ScheduleEnabled bool       `json:"scheduleEnabled"`
	WindowStart     string     `json:"windowStart"`
	WindowEnd       string     `json:"windowEnd"`
	NextRun         string     `json:"nextRun"`
	LastRun         *RunResult `json:"lastRun"`
}

// Manager owns the check-in command state and its scheduler. All mutable
// fields are guarded by mu.
type Manager struct {
	cmd, args, workdir     string
	schedule               bool
	windowStart, windowEnd string
	log                    *slog.Logger

	mu         sync.Mutex
	running    bool
	started    bool // scheduler goroutine launched
	configured bool
	lastRun    *RunResult
	nextRun    time.Time
}

// New builds a Manager. An empty workdir means check-in is not configured.
func New(cmd, args, workdir string, schedule bool, windowStart, windowEnd string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		cmd:         cmd,
		args:        args,
		workdir:     workdir,
		schedule:    schedule,
		windowStart: windowStart,
		windowEnd:   windowEnd,
		log:         log,
		configured:  workdir != "",
	}
}

// Status returns a copy of the current state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		Configured:      m.configured,
		Running:         m.running,
		Cmd:             m.cmd,
		Workdir:         m.workdir,
		ScheduleEnabled: m.schedule && m.configured,
		WindowStart:     m.windowStart,
		WindowEnd:       m.windowEnd,
	}
	if m.schedule && m.configured && !m.nextRun.IsZero() {
		st.NextRun = m.nextRun.Format(time.RFC3339)
	}
	if m.lastRun != nil {
		cp := *m.lastRun
		st.LastRun = &cp
	}
	return st
}

// RunNow starts a check-in run asynchronously. It returns ErrNotConfigured
// when no workdir is set and ErrAlreadyRunning while a run is in flight.
func (m *Manager) RunNow(ctx context.Context) error {
	return m.start(ctx, nil)
}

// start launches a run with optional extra args appended to the base args.
func (m *Manager) start(ctx context.Context, extra []string) error {
	m.mu.Lock()
	if !m.configured {
		m.mu.Unlock()
		return ErrNotConfigured
	}
	if m.running {
		m.mu.Unlock()
		return ErrAlreadyRunning
	}
	m.running = true
	res := &RunResult{Started: time.Now().Format(time.RFC3339), ExitCode: -1}
	m.lastRun = res
	m.mu.Unlock()

	args := strings.Fields(m.args)
	if len(extra) > 0 {
		args = append(append([]string{}, args...), extra...)
	}
	m.log.Info("check-in run started", "cmd", m.cmd)
	go func() {
		code, output, err := runCommand(ctx, m.cmd, args, m.workdir)
		m.mu.Lock()
		res.ExitCode = code
		res.OK = err == nil && code == 0
		res.Finished = time.Now().Format(time.RFC3339)
		res.Output = string(output)
		m.running = false
		m.mu.Unlock()
		m.log.Info("check-in run finished", "cmd", m.cmd, "exitCode", code, "ok", res.OK)
	}()
	return nil
}

// Start launches the scheduler goroutine. It is a no-op without a configured
// workdir or with scheduling disabled, and is guarded against double Start.
// Each scheduled run appends --random-wait to the base args.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started || !m.configured || !m.schedule {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	go m.loop(ctx)
}

func (m *Manager) loop(ctx context.Context) {
	m.updateNextRun()
	for {
		m.mu.Lock()
		next := m.nextRun
		m.mu.Unlock()
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if err := m.start(ctx, []string{"--random-wait"}); err != nil {
			m.log.Warn("scheduled check-in skipped", "err", err)
		}
		m.updateNextRun()
	}
}

func (m *Manager) updateNextRun() {
	m.mu.Lock()
	m.nextRun = nextWindowStart(time.Now(), m.windowStart)
	m.mu.Unlock()
}

// nextWindowStart returns the next occurrence of windowStart ("HH:MM" local)
// that is strictly in the future: today when still ahead, otherwise tomorrow.
func nextWindowStart(now time.Time, windowStart string) time.Time {
	h, min, ok := parseClock(windowStart)
	if !ok {
		h, min = 0, 0
	}
	next := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// parseClock parses an "HH:MM" clock string into hour and minute.
func parseClock(s string) (hour, min int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	if err != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// cappedWriter keeps the tail of everything written, capped at maxOutput
// bytes. Safe for the concurrent writes exec makes from its stdout and
// stderr copy goroutines.
type cappedWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) >= maxOutput {
		w.buf = append(w.buf[:0], p[len(p)-maxOutput:]...)
		return len(p), nil
	}
	if len(w.buf)+len(p) > maxOutput {
		drop := len(w.buf) + len(p) - maxOutput
		w.buf = append(w.buf[:0], w.buf[drop:]...)
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}
