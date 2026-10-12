//go:build !windows

package supervisor

import (
	"os/exec"
	"syscall"
)

// setProcAttr puts the child in its own process group so that a single signal
// reaches the client and anything it spawned.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

func kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

func exitStatus(cmd *exec.Cmd, waitErr error) (int, string) {
	if cmd == nil || cmd.ProcessState == nil {
		return -1, ""
	}
	st := cmd.ProcessState
	if ws, ok := st.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return -1, ws.Signal().String()
		}
		return ws.ExitStatus(), ""
	}
	return st.ExitCode(), ""
}
