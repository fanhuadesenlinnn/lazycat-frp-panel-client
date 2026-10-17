package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
)

func TestRuntimeStatusRejectsStaleOrPreviousProcess(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "runtime.json")
	proc := supervisor.Snapshot{State: supervisor.StateRunning, PID: 42}
	for _, tc := range []struct {
		name  string
		pid   int
		at    time.Time
		fresh bool
	}{
		{"current", 42, now, true}, {"old process", 41, now, false}, {"stale", 42, now.Add(-time.Minute), false}, {"future", 42, now.Add(time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(runtimeStatus{PID: tc.pid, At: tc.at, RPCRegistered: true, Tunnels: []runtimeTunnel{{Status: "running"}, {Status: "start error"}}})
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			status := readRuntimeStatus(path, proc, now)
			if status.Fresh != tc.fresh {
				t.Fatalf("fresh=%v", status.Fresh)
			}
			if tc.fresh && (status.Total != 2 || status.Running != 1) {
				t.Fatalf("wrong tunnel counters: %+v", status)
			}
		})
	}
	proc.State = supervisor.StateStopped
	if readRuntimeStatus(path, proc, now).Fresh {
		t.Fatal("stopped process reported healthy")
	}
}
