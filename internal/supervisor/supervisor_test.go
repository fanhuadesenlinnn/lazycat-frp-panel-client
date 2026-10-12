package supervisor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestHelperChild is not a real test: it is the child process that the
// supervisor is told to run. Behaviour is selected with environment variables.
func TestHelperChild(t *testing.T) {
	if os.Getenv("LZC_FRPP_HELPER") != "1" {
		return
	}
	mode := os.Getenv("LZC_FRPP_HELPER_MODE")
	switch mode {
	case "exit-fast":
		os.Exit(3)
	case "run-then-exit":
		time.Sleep(300 * time.Millisecond)
		os.Exit(1)
	case "marker":
		// 先注册信号处理，再写 start：如果在“写 start”和“注册信号”之间收到
		// SIGTERM，进程会按默认动作直接退出，留下一个永远等不到 end 的 start，
		// 从而让父进程的“子进程不重叠”断言假阳性失败。
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		if err := appendMarker("start"); err != nil {
			os.Exit(4)
		}
		// Give the manager a chance to ask us to stop gracefully.
		select {
		case <-ch:
			_ = appendMarker("end")
			os.Exit(0)
		case <-time.After(10 * time.Second):
			_ = appendMarker("end")
			os.Exit(0)
		}
	default: // "sleep"
		time.Sleep(30 * time.Second)
	}
	os.Exit(0)
}

func appendMarker(kind string) error {
	path := os.Getenv("LZC_FRPP_MARKER_FILE")
	if path == "" {
		return fmt.Errorf("missing marker file")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s %d\n", kind, os.Getpid())
	return err
}

func helperSpec(mode string) func() (Spec, error) {
	return func() (Spec, error) {
		env := []string{"LZC_FRPP_HELPER=1", "LZC_FRPP_HELPER_MODE=" + mode}
		if path := os.Getenv("LZC_FRPP_MARKER_FILE"); path != "" {
			env = append(env, "LZC_FRPP_MARKER_FILE="+path)
		}
		return Spec{
			Path: os.Args[0],
			Args: []string{"-test.run=TestHelperChild", "--"},
			Env:  env,
		}, nil
	}
}

func fastOptions(sink *bytes.Buffer) Options {
	return Options{
		BaseBackoff: 30 * time.Millisecond,
		MaxBackoff:  150 * time.Millisecond,
		FastFailure: 120 * time.Millisecond,
		StableAfter: 200 * time.Millisecond,
		TermTimeout: 2 * time.Second,
		Sink:        sink,
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func(Snapshot) bool) Snapshot {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		snap := supervisorSnapshot(t)
		if cond(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 %s 超时，最后状态: %+v", what, snap)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// currentManager is a test-only indirection so waitFor can stay readable.
var currentManager atomic.Pointer[Manager]

func supervisorSnapshot(t *testing.T) Snapshot {
	t.Helper()
	m := currentManager.Load()
	if m == nil {
		t.Fatal("manager 未设置")
	}
	return m.Status()
}

func newTestManager(t *testing.T, mode string, sink *bytes.Buffer) *Manager {
	t.Helper()
	m := New(helperSpec(mode), fastOptions(sink))
	currentManager.Store(m)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

func TestStartStopLifecycle(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "sleep", sink)

	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	snap := waitFor(t, 3*time.Second, "running", func(s Snapshot) bool {
		return s.State == StateRunning && s.PID > 0
	})
	if snap.StartedAt.IsZero() {
		t.Error("StartedAt 未记录")
	}

	if err := m.Start(); err == nil {
		t.Error("重复 Start 应当返回错误")
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	snap = m.Status()
	if snap.State != StateStopped || snap.PID != 0 {
		t.Fatalf("停止后状态不正确: %+v", snap)
	}
	// 手动停止不应触发自动重启。
	time.Sleep(300 * time.Millisecond)
	if snap = m.Status(); snap.Restarts != 0 || snap.State != StateStopped {
		t.Fatalf("手动停止后不应自动重启: %+v", snap)
	}
}

func TestAutoRestartWithBackoff(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "exit-fast", sink)
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	snap := waitFor(t, 5*time.Second, "至少两次重启", func(s Snapshot) bool {
		return s.Restarts >= 2
	})
	if snap.LastExit == nil {
		t.Fatal("应记录上次退出信息")
	}
	if snap.LastExit.Code != 3 {
		t.Errorf("退出码应为 3，实际 %+v", snap.LastExit)
	}
	if snap.State != StateBackoff && snap.State != StateRunning {
		t.Errorf("重启期间状态应为 backoff/running，实际 %s", snap.State)
	}
}

func TestCrashLoopIsFlaggedUnstable(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "exit-fast", sink)
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	waitFor(t, 6*time.Second, "崩溃循环标记", func(s Snapshot) bool {
		return s.Unstable && s.ConsecutiveFastFailures >= 5
	})
}

func TestStableRunResetsFastFailures(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "run-then-exit", sink)
	if err := m.Start(); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	snap := waitFor(t, 5*time.Second, "稳定运行后退出", func(s Snapshot) bool {
		return s.Restarts >= 1
	})
	if snap.ConsecutiveFastFailures != 0 {
		t.Fatalf("运行时间超过阈值后不应计入快速失败: %+v", snap)
	}
	if snap.Unstable {
		t.Fatal("不应被标记为崩溃循环")
	}
}

func TestStartRejectedAfterClose(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "sleep", sink)
	if err := m.Close(context.Background()); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if err := m.Start(); err == nil {
		t.Fatal("Close 之后 Start 应被拒绝")
	}
}

func TestStartWithoutPathFails(t *testing.T) {
	m := New(func() (Spec, error) { return Spec{}, nil }, fastOptions(&bytes.Buffer{}))
	if err := m.Start(); err == nil {
		t.Fatal("缺少可执行文件路径时应报错")
	}
	if snap := m.Status(); snap.State != StateStopped {
		t.Fatalf("启动失败后状态应为 stopped: %+v", snap)
	}
}

func TestConcurrentLifecycleOpsStayConsistent(t *testing.T) {
	sink := &bytes.Buffer{}
	m := newTestManager(t, "sleep", sink)

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				_ = m.Start()
			case 1:
				_ = m.Stop(context.Background())
			case 2:
				_ = m.Restart(context.Background())
			default:
				_ = m.Status()
			}
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	snap := m.Status()
	if snap.State != StateStopped || snap.PID != 0 {
		t.Fatalf("并发操作后应回到 stopped: %+v", snap)
	}
	m.mu.Lock()
	leftover := m.cmd
	closed := m.closed
	m.mu.Unlock()
	if leftover != nil {
		t.Fatal("Close 之后不应残留子进程")
	}
	if !closed {
		t.Fatal("Close 应标记 closed")
	}
}

func TestConcurrentOpsNeverOverlapChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上无法保证子进程有机会处理终止信号")
	}
	marker := filepath.Join(t.TempDir(), "markers.txt")
	t.Setenv("LZC_FRPP_MARKER_FILE", marker)

	sink := &bytes.Buffer{}
	opts := fastOptions(sink)
	opts.BaseBackoff = time.Second
	opts.MaxBackoff = time.Second
	m := New(helperSpec("marker"), opts)
	currentManager.Store(m)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	}()

	// 先预热一轮：等子进程确实写出第一个标记后再进入并发流程。否则在 -race
	// 或负载较高的机器上，子进程可能在跑到写标记之前就被停掉，标记文件根本
	// 不会出现，测试会以“文件不存在”这种与主题无关的方式失败。
	if err := m.Start(); err != nil {
		t.Fatalf("预热启动失败: %v", err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && len(bytes.TrimSpace(data)) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("预热阶段子进程始终没有写出标记文件 %s", marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("预热停止失败: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := m.Start(); err == nil {
					time.Sleep(20 * time.Millisecond)
				}
			} else {
				_ = m.Stop(context.Background())
			}
		}(i)
	}
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Close(ctx); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("读取标记文件失败: %v", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(data)))
	var kinds []string
	for i, f := range lines {
		if i%2 == 0 {
			kinds = append(kinds, f)
		}
	}
	expect := "start"
	for i, k := range kinds {
		if k != expect {
			t.Fatalf("第 %d 个标记为 %q，期望 %q（子进程发生重叠）", i, k, expect)
		}
		expect = map[string]string{"start": "end", "end": "start"}[expect]
	}
}
