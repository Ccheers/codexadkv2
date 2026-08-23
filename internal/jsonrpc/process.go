package jsonrpc

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ProcessConfig configures a spawned app-server child process.
type ProcessConfig struct {
	// Path is the codex executable. Defaults to "codex", resolved via PATH.
	Path string

	// Args are the arguments after the executable name. Defaults to
	// ["app-server"].
	Args []string

	// Dir is the child's working directory. Empty means inherit.
	Dir string

	// Env is the child's environment. Nil means inherit the parent's.
	Env []string

	// Stderr receives a copy of the child's stderr. Nil discards it.
	//
	// A copy is always retained internally regardless: app-server reports
	// startup failures and panics only on stderr, so without it an unexpected
	// exit produces an error with no explanation.
	Stderr io.Writer

	// ShutdownGrace is how long to wait after SIGTERM before escalating to
	// SIGKILL. Defaults to 5s.
	ShutdownGrace time.Duration

	// KillGrace is how long to wait after SIGKILL before giving up. Defaults
	// to 2s.
	KillGrace time.Duration
}

func (c *ProcessConfig) withDefaults() ProcessConfig {
	out := *c
	if out.Path == "" {
		out.Path = "codex"
	}
	if out.Args == nil {
		out.Args = []string{"app-server"}
	}
	if out.ShutdownGrace == 0 {
		out.ShutdownGrace = 5 * time.Second
	}
	if out.KillGrace == 0 {
		out.KillGrace = 2 * time.Second
	}
	return out
}

// Process is a spawned app-server child and the transport that talks to it.
type Process struct {
	cmd    *exec.Cmd
	config ProcessConfig

	stdin  io.WriteCloser
	stderr *stderrTail

	transport Transport

	waitOnce sync.Once
	waitErr  error
	exited   chan struct{}
}

// ErrServerExited reports that the app-server child process is gone. Errors
// wrapping it carry the exit status and the tail of the child's stderr.
var ErrServerExited = errors.New("codex app-server exited")

// StartProcess spawns app-server and returns a Process whose Transport speaks
// newline-delimited JSON over the child's stdin and stdout.
func StartProcess(cfg ProcessConfig) (*Process, error) {
	config := cfg.withDefaults()

	// Resolve up front so a missing binary produces an actionable error rather
	// than a generic exec failure.
	resolved, err := exec.LookPath(config.Path)
	if err != nil {
		return nil, fmt.Errorf(
			"codex executable %q not found: %w\n"+
				"Install the Codex CLI, or set the executable path explicitly with WithBinaryPath",
			config.Path, err)
	}

	cmd := newCmd(resolved, config.Args)
	cmd.Dir = config.Dir
	cmd.Env = config.Env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: opening stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: opening stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("codex: opening stderr: %w", err)
	}

	// On Unix, put the child in its own process group so shutdown can signal
	// the whole tree. app-server itself spawns subprocesses for tool calls.
	configureProcessGroup(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: starting %s: %w", resolved, err)
	}

	p := &Process{
		cmd:    cmd,
		config: config,
		stdin:  stdin,
		stderr: newStderrTail(config.Stderr),
		exited: make(chan struct{}),
	}
	p.transport = &processTransport{
		Transport: NewStreamTransport(stdout, stdin, stdin),
		proc:      p,
	}

	go p.drainStderr(stderrPipe)
	go p.reap()

	return p, nil
}

// Transport returns the transport talking to the child.
func (p *Process) Transport() Transport { return p.transport }

// Pid returns the child's process id, or 0 if it never started.
func (p *Process) Pid() int {
	if p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// StderrTail returns the retained tail of the child's stderr, for diagnostics.
func (p *Process) StderrTail() string { return p.stderr.String() }

func (p *Process) drainStderr(r io.Reader) {
	_, _ = io.Copy(p.stderr, r)
}

func (p *Process) reap() {
	err := p.cmd.Wait()
	p.waitOnce.Do(func() { p.waitErr = err })
	close(p.exited)
}

// exitError builds the error returned to in-flight calls once the child dies.
// It includes the child's own stderr, because an exit status alone rarely
// explains anything.
func (p *Process) exitError() error {
	<-p.exited
	status := "exited cleanly"
	if p.waitErr != nil {
		status = p.waitErr.Error()
	}
	if tail := p.stderr.String(); tail != "" {
		return fmt.Errorf("%w (%s); last stderr output:\n%s", ErrServerExited, status, tail)
	}
	return fmt.Errorf("%w (%s); the process produced no stderr output", ErrServerExited, status)
}

// Close shuts the child down: close stdin so it can exit on its own, then
// SIGTERM, then SIGKILL.
//
// The ladder matters. Closing stdin alone is not enough for a server that is
// mid-turn, and SIGKILL alone denies it the chance to clean up its own
// subprocesses and persist thread state.
func (p *Process) Close() error {
	if p.cmd.Process == nil {
		return nil
	}

	// Closing stdin is the protocol's own end-of-input signal.
	_ = p.stdin.Close()

	select {
	case <-p.exited:
		return p.waitErr
	case <-time.After(200 * time.Millisecond):
	}

	terminateGroup(p.cmd)

	select {
	case <-p.exited:
		return p.waitErr
	case <-time.After(p.config.ShutdownGrace):
	}

	killGroup(p.cmd)

	select {
	case <-p.exited:
		return p.waitErr
	case <-time.After(p.config.KillGrace):
		return fmt.Errorf("codex: process %d did not exit after SIGKILL", p.Pid())
	}
}

// Exited returns a channel closed when the child exits.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// processTransport annotates transport errors with the child's exit status, so
// a read failure caused by a crashed server says so instead of reporting a bare
// "file already closed".
type processTransport struct {
	Transport
	proc *Process
}

func (t *processTransport) Recv() ([]byte, error) {
	data, err := t.Transport.Recv()
	if err == nil {
		return data, nil
	}
	select {
	case <-t.proc.exited:
		return nil, t.proc.exitError()
	default:
		return nil, err
	}
}

func (t *processTransport) Send(data []byte) error {
	err := t.Transport.Send(data)
	if err == nil {
		return nil
	}
	select {
	case <-t.proc.exited:
		return t.proc.exitError()
	default:
		return err
	}
}

// stderrTail retains the last few KiB of the child's stderr in a ring buffer
// while optionally forwarding it to a caller-supplied writer.
type stderrTail struct {
	mu   sync.Mutex
	buf  []byte
	out  io.Writer
	size int
}

func newStderrTail(out io.Writer) *stderrTail {
	return &stderrTail{out: out, size: 8 * 1024}
}

func (s *stderrTail) Write(p []byte) (int, error) {
	s.mu.Lock()
	s.buf = append(s.buf, p...)
	if len(s.buf) > s.size {
		s.buf = s.buf[len(s.buf)-s.size:]
	}
	s.mu.Unlock()

	if s.out != nil {
		// A failing user writer must not break stderr draining.
		_, _ = s.out.Write(p)
	}
	return len(p), nil
}

func (s *stderrTail) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(string(s.buf))
}
