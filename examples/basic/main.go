// Command basic is the shortest useful program: open a session, run a turn, print
// the reply.
//
//	go run ./examples/basic -prompt "summarize this repo"
//
// It needs the codex CLI installed and an authenticated account.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

func main() {
	prompt := flag.String("prompt", "What is 2+2? Answer in one word.", "the message to send")
	cwd := flag.String("cwd", "", "working directory for the agent (defaults to the current one)")
	model := flag.String("model", "", "model to use (defaults to the server's choice)")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	verbose := flag.Bool("v", false, "log SDK diagnostics and the server's stderr")
	flag.Parse()

	if err := run(*prompt, *cwd, *model, *timeout, *verbose); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(prompt, cwd, model string, timeout time.Duration, verbose bool) error {
	// Ctrl-C cancels the context, which interrupts the turn rather than leaving
	// the agent working on a result nobody will read.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	// Read-only with approvals disabled is the right default for an unattended
	// run: the agent cannot modify anything, and nothing blocks waiting for a
	// decision that no one is there to make.
	threadOpts := []protocol.ThreadStartParamsOption{
		protocol.WithThreadStartParamsCwd(cwd),
		protocol.WithThreadStartParamsSandbox(protocol.SandboxModeReadOnly),
		protocol.WithThreadStartParamsApprovalPolicy(protocol.NewAskForApprovalNever()),
	}
	if model != "" {
		threadOpts = append(threadOpts, protocol.WithThreadStartParamsModel(model))
	}

	opts := []codex.SessionOption{
		codex.WithClientInfo("codex-go-example", "Codex Go SDK Example", "0.1.0"),
		codex.WithThreadOptions(threadOpts...),
	}
	if verbose {
		opts = append(opts,
			codex.WithStderr(os.Stderr),
			codex.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelDebug,
			}))),
		)
	}

	// One call spawns the server, completes the handshake, and starts the thread.
	session, err := codex.Open(ctx, opts...)
	if err != nil {
		return err
	}
	defer session.Close()

	if verbose {
		info := session.ServerInfo()
		fmt.Fprintf(os.Stderr, "connected to %s (%s/%s)\n",
			info.UserAgent, info.PlatformFamily, info.PlatformOS)
	}
	fmt.Fprintf(os.Stderr, "thread %s\n", session.ID())

	result, err := session.RunText(ctx, prompt)
	if err != nil {
		return err
	}
	if result.Interrupted() {
		fmt.Fprintln(os.Stderr, "the turn was interrupted")
	}
	fmt.Println(result.AgentMessage)
	return nil
}
