// Command basic demonstrates the ergonomic layer: start a thread, run a turn,
// print the reply.
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
	// the agent working.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	opts := []codex.Option{
		codex.WithClientInfo("codex-go-example", "Codex Go SDK Example", "0.1.0"),
	}
	if verbose {
		opts = append(opts,
			codex.WithStderr(os.Stderr),
			codex.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
				Level: slog.LevelDebug,
			}))),
		)
	}

	client, err := codex.New(ctx, opts...)
	if err != nil {
		return err
	}
	defer client.Close()

	if verbose {
		info := client.ServerInfo()
		fmt.Fprintf(os.Stderr, "connected to %s (%s/%s)\n",
			info.UserAgent, info.PlatformFamily, info.PlatformOS)
	}

	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}

	// Read-only with approvals disabled is the right default for an unattended
	// example: the agent cannot modify anything, and nothing can block waiting
	// for a decision.
	sandbox := protocol.SandboxModeReadOnly
	never := protocol.NewAskForApprovalNever()
	params := protocol.ThreadStartParams{
		Cwd:            &cwd,
		Sandbox:        &sandbox,
		ApprovalPolicy: &never,
	}
	if model != "" {
		params.Model = &model
	}

	thread, err := client.StartThread(ctx, params)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "thread %s\n", thread.ID())

	result, err := thread.RunText(ctx, prompt)
	if err != nil {
		return err
	}
	if result.Interrupted() {
		fmt.Fprintln(os.Stderr, "the turn was interrupted")
	}
	fmt.Println(result.AgentMessage)
	return nil
}
