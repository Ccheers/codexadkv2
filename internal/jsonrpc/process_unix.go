//go:build !windows

package jsonrpc

import (
	"os"
	"os/exec"
	"syscall"
)

// newCmd builds the child command for the platform. On Unix a binary is
// directly executable, so no shim is needed.
func newCmd(binary string, args []string) *exec.Cmd {
	return exec.Command(binary, args...)
}

// configureProcessGroup places the child in its own process group so signals
// reach the whole tree (app-server itself spawns subprocesses for tool calls).
func configureProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// terminateGroup asks the child and its descendants to exit with SIGTERM,
// falling back to an interrupt signal that any process can handle.
func terminateGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		if err := syscall.Kill(-pgid, syscall.SIGTERM); err == nil {
			return
		}
	}
	_ = cmd.Process.Signal(os.Interrupt)
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil && pgid == cmd.Process.Pid {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}
