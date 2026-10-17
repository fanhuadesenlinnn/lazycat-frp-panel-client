package web

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/config"
	"github.com/fanhuadesenlinnn/lazycat-frp-panel-client/internal/supervisor"
)

type runtimeTunnel struct {
	ClientID string `json:"client_id"`
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
}

type runtimeStatus struct {
	Fresh         bool            `json:"fresh"`
	Message       string          `json:"message"`
	PID           int             `json:"pid"`
	At            time.Time       `json:"at"`
	RPCRegistered bool            `json:"rpc_registered"`
	Total         int             `json:"total"`
	Running       int             `json:"running"`
	Tunnels       []runtimeTunnel `json:"tunnels,omitempty"`
}

func (s *Server) runtimeStatus(id string, proc supervisor.Snapshot, now time.Time) runtimeStatus {
	dir := s.opts.DataDir
	if id != config.DefaultConnection {
		dir = filepath.Join(dir, "connections", id)
	}
	return readRuntimeStatus(filepath.Join(dir, "frpp-runtime.json"), proc, now)
}

func readRuntimeStatus(path string, proc supervisor.Snapshot, now time.Time) runtimeStatus {
	unknown := runtimeStatus{Message: "等待客户端报告；进程运行不代表隧道可用"}
	if proc.State != supervisor.StateRunning {
		unknown.Message = "客户端进程未运行"
		return unknown
	}
	file, err := os.Open(path)
	if err != nil {
		return unknown
	}
	defer file.Close()
	var report runtimeStatus
	if err := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&report); err != nil {
		return unknown
	}
	age := now.Sub(report.At)
	if report.PID != proc.PID || age > 20*time.Second || age < -5*time.Second {
		unknown.Message = "客户端状态报告已过期，等待更新"
		return unknown
	}
	report.Fresh, report.Total = true, len(report.Tunnels)
	for _, tunnel := range report.Tunnels {
		if tunnel.Status == "running" {
			report.Running++
		}
	}
	report.Message = "FRP 运行状态；后端服务可用性需另行验证"
	return report
}
