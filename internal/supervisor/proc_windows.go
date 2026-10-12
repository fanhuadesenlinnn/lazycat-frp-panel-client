//go:build windows

package supervisor

import (
	"os/exec"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// setProcAttr is a no-op beyond detaching the child from our console group.
// The real target is Linux; Windows support only exists so that the test suite
// runs on the development machine.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}

func terminate(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func kill(cmd *exec.Cmd) error {
	return terminate(cmd)
}

func exitStatus(cmd *exec.Cmd, waitErr error) (int, string) {
	if cmd == nil || cmd.ProcessState == nil {
		return -1, ""
	}
	return cmd.ProcessState.ExitCode(), ""
}
