// Package supervisor owns the frp-panel client child process: start, stop,
// restart, graceful shutdown and backoff restarts after unexpected exits.
//
// Concurrency contract:
//   - every control operation (Start/Stop/Restart/Close) is serialised by ops,
//     so two callers can never end up driving two children at once;
//   - the exit watcher only mutates shared state while m.cmd still points at
//     the very process it waited for, so a stale watcher can not overwrite the
//     state of a newer child;
//   - Close marks the manager closed before stopping, so no restart can be
//     scheduled while shutting down.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"sync"
	"time"
)

// State is the process lifecycle state reported to the UI.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateBackoff  State = "backoff"
	StateStopping State = "stopping"
)

// Spec describes how to launch the child process.
type Spec struct {
	Path string
	Args []string
	Env  []string
	Dir  string
}

// ExitInfo records how the previous run ended.
type ExitInfo struct {
	Code     int       `json:"code"`
	Signal   string    `json:"signal"`
	At       time.Time `json:"at"`
	UptimeMS int64     `json:"uptime_ms"`
	Error    string    `json:"error"`
}

// Snapshot is a consistent view of the supervisor state.
type Snapshot struct {
	State                   State     `json:"state"`
	PID                     int       `json:"pid"`
	StartedAt               time.Time `json:"started_at"`
	Restarts                int       `json:"restarts"`
	ConsecutiveFastFailures int       `json:"consecutive_fast_failures"`
	Unstable                bool      `json:"unstable"`
	NextRestartAt           time.Time `json:"next_restart_at"`
	LastExit                *ExitInfo `json:"last_exit"`
	LastError               string    `json:"last_error"`
}

// Options tunes supervision behaviour.
type Options struct {
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	// StableAfter is how long a run must last before the backoff resets.
	StableAfter time.Duration
	// FastFailure is the uptime below which an exit counts towards the
	// crash-loop warning.
	FastFailure time.Duration
	// TermTimeout is how long a SIGTERM is given before SIGKILL.
	TermTimeout time.Duration
	// Sink receives every byte of child stdout/stderr.
	Sink io.Writer
}

func (o Options) withDefaults() Options {
	if o.BaseBackoff <= 0 {
		o.BaseBackoff = 2 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 60 * time.Second
	}
	if o.StableAfter <= 0 {
		o.StableAfter = 60 * time.Second
	}
	if o.FastFailure <= 0 {
		o.FastFailure = 10 * time.Second
	}
	if o.TermTimeout <= 0 {
		o.TermTimeout = 8 * time.Second
	}
	if o.Sink == nil {
		o.Sink = io.Discard
	}
	return o
}

// Manager supervises a single child process.
type Manager struct {
	// ops serialises lifecycle transitions.
	ops sync.Mutex
	// mu protects every field below.
	mu           sync.Mutex
	specProvider func() (Spec, error)
	opts         Options

	state       State
	cmd         *exec.Cmd
	done        chan struct{}
	pid         int
	startedAt   time.Time
	restarts    int
	fastFails   int
	lastExit    *ExitInfo
	lastErr     string
	backoff     time.Duration
	nextRestart time.Time
	manualStop  bool
	closed      bool
	stopTimer   *time.Timer
}

// New creates a Manager. specProvider is called on every (re)start so that
// configuration changes take effect without recreating the supervisor.
func New(specProvider func() (Spec, error), opts Options) *Manager {
	opts = opts.withDefaults()
	return &Manager{
		specProvider: specProvider,
		opts:         opts,
		state:        StateStopped,
		backoff:      opts.BaseBackoff,
	}
}

// Start launches the child process. Starting while another lifecycle
// transition is in flight is either an error or a wait, never a second child.
func (m *Manager) Start() error {
	m.ops.Lock()
	defer m.ops.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("管理器已关闭")
	}
	switch m.state {
	case StateRunning, StateStarting:
		return errors.New("客户端已在运行")
	case StateStopping:
		return errors.New("客户端正在停止，请稍后重试")
	}
	m.manualStop = false
	m.cancelTimerLocked()
	m.backoff = m.opts.BaseBackoff
	return m.startLocked()
}

// Restart stops the child process (if running) and starts it again.
func (m *Manager) Restart(ctx context.Context) error {
	m.ops.Lock()
	defer m.ops.Unlock()

	if err := m.stopLocked(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("管理器已关闭")
	}
	m.manualStop = false
	m.cancelTimerLocked()
	m.backoff = m.opts.BaseBackoff
	return m.startLocked()
}

// Stop terminates the child process and disables automatic restarts.
func (m *Manager) Stop(ctx context.Context) error {
	m.ops.Lock()
	defer m.ops.Unlock()
	return m.stopLocked(ctx)
}

// Close stops the child process and prevents any further start.
func (m *Manager) Close(ctx context.Context) error {
	m.ops.Lock()
	defer m.ops.Unlock()

	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return m.stopLocked(ctx)
}

// stopLocked performs the stop sequence; m.ops must be held.
func (m *Manager) stopLocked(ctx context.Context) error {
	m.mu.Lock()
	m.manualStop = true
	m.cancelTimerLocked()
	cmd, done := m.cmd, m.done
	if cmd == nil || cmd.Process == nil {
		m.state = StateStopped
		m.pid = 0
		m.nextRestart = time.Time{}
		m.mu.Unlock()
		return nil
	}
	m.state = StateStopping
	m.mu.Unlock()

	_ = terminate(cmd)

	select {
	case <-done:
	case <-time.After(m.opts.TermTimeout):
		_ = kill(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	case <-ctx.Done():
		_ = kill(cmd)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}

	m.mu.Lock()
	m.state = StateStopped
	m.pid = 0
	m.nextRestart = time.Time{}
	m.cmd = nil
	m.mu.Unlock()
	return nil
}

// Status returns the current state.
func (m *Manager) Status() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{
		State:                   m.state,
		PID:                     m.pid,
		StartedAt:               m.startedAt,
		Restarts:                m.restarts,
		ConsecutiveFastFailures: m.fastFails,
		Unstable:                m.fastFails >= 5,
		NextRestartAt:           m.nextRestart,
		LastExit:                m.lastExit,
		LastError:               m.lastErr,
	}
}

// startLocked launches the child; m.mu must be held.
func (m *Manager) startLocked() error {
	if m.closed {
		return errors.New("管理器已关闭")
	}
	spec, err := m.specProvider()
	if err != nil {
		m.state = StateStopped
		m.lastErr = err.Error()
		return err
	}
	if spec.Path == "" {
		m.state = StateStopped
		err = errors.New("未找到 frp-panel 客户端可执行文件")
		m.lastErr = err.Error()
		return err
	}
	if _, statErr := os.Stat(spec.Path); statErr != nil {
		m.state = StateStopped
		err = fmt.Errorf("无法访问客户端可执行文件 %s：%v", spec.Path, statErr)
		m.lastErr = err.Error()
		return err
	}

	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Dir = spec.Dir
	cmd.Stdout = m.opts.Sink
	cmd.Stderr = m.opts.Sink
	setProcAttr(cmd)

	m.state = StateStarting
	if err := cmd.Start(); err != nil {
		m.state = StateStopped
		m.lastErr = err.Error()
		return fmt.Errorf("启动客户端失败：%w", err)
	}

	done := make(chan struct{})
	m.cmd = cmd
	m.done = done
	m.pid = cmd.Process.Pid
	m.startedAt = time.Now()
	m.nextRestart = time.Time{}
	m.state = StateRunning
	m.lastErr = ""

	go func() {
		waitErr := cmd.Wait()
		m.handleExit(cmd, waitErr)
		close(done)
	}()
	return nil
}

func (m *Manager) handleExit(cmd *exec.Cmd, waitErr error) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()

	// A newer child replaced this one while we were waiting: touching shared
	// state here would corrupt the newer process bookkeeping.
	if m.cmd != cmd {
		return
	}

	info := &ExitInfo{At: now}
	if !m.startedAt.IsZero() {
		info.UptimeMS = now.Sub(m.startedAt).Milliseconds()
	}
	if waitErr != nil {
		info.Error = waitErr.Error()
	}
	info.Code, info.Signal = exitStatus(cmd, waitErr)
	m.lastExit = info
	m.pid = 0
	m.cmd = nil

	if m.closed || m.manualStop {
		m.state = StateStopped
		return
	}

	m.restarts++
	if info.UptimeMS < m.opts.FastFailure.Milliseconds() {
		m.fastFails++
	} else {
		m.fastFails = 0
		m.backoff = m.opts.BaseBackoff
	}

	delay := m.backoff
	if delay < m.opts.BaseBackoff {
		delay = m.opts.BaseBackoff
	}
	// Small jitter keeps several clients from hammering a master in lockstep.
	delay += time.Duration(rand.Int63n(int64(delay/4 + 1)))
	if delay > m.opts.MaxBackoff {
		delay = m.opts.MaxBackoff
	}
	m.backoff = time.Duration(float64(m.backoff) * 2)
	if m.backoff > m.opts.MaxBackoff {
		m.backoff = m.opts.MaxBackoff
	}

	m.nextRestart = now.Add(delay)
	m.state = StateBackoff
	m.stopTimer = time.AfterFunc(delay, m.scheduledRestart)
}

func (m *Manager) scheduledRestart() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.manualStop || m.state != StateBackoff {
		return
	}
	m.stopTimer = nil
	if err := m.startLocked(); err != nil {
		// Keep retrying with the capped backoff; the master may simply be down.
		m.state = StateBackoff
		delay := m.opts.MaxBackoff
		m.nextRestart = time.Now().Add(delay)
		m.stopTimer = time.AfterFunc(delay, m.scheduledRestart)
	}
}

func (m *Manager) cancelTimerLocked() {
	if m.stopTimer != nil {
		m.stopTimer.Stop()
		m.stopTimer = nil
	}
}
