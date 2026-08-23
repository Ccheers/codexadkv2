//go:build windows

package jsonrpc

import (
	"os/exec"
	"strings"
	"syscall"
)

// newCmd builds the child command for the platform. On Windows a globally
// npm-installed codex is a .cmd shim that CreateProcess cannot execute
// directly, so it must be routed through cmd.exe.
func newCmd(binary string, args []string) *exec.Cmd {
	if strings.HasSuffix(strings.ToLower(binary), ".cmd") {
		return exec.Command("cmd.exe", append([]string{"/c", binary}, args...)...)
	}
	return exec.Command(binary, args...)
}

// hideWindow makes the child spawn without an allocated visible console. The
// controller is itself launched by a GUI process (Electron, via windowsHide),
// so it has no console; when it then spawns a console child (codex / the
// cmd.exe shim), Windows would otherwise open a fresh visible console window.
// CREATE_NO_WINDOW prevents that popup. stdout/stderr are already captured by
// pipes, so they are unaffected.
func hideWindow(cmd *exec.Cmd) {
	const createNoWindow = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNoWindow}
}

// configureProcessGroup is a no-op on Windows: process groups and signals are
// managed by the OS and there is no Setpgid equivalent. It hides the child's
// console instead, which is what the GUI-launched controller needs.
func configureProcessGroup(cmd *exec.Cmd) { hideWindow(cmd) }

// terminateGroup asks the child to exit. Windows has no SIGTERM: Process.Kill
// (TerminateProcess) is the only option, so the graceful step is skipped and
// orphaned grandchildren are possible. A full fix needs a Job Object or
// taskkill /T, which is not implemented.
func terminateGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func killGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
