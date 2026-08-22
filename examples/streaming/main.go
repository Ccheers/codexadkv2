// Command streaming demonstrates the streaming layer: print the agent's reply as
// it arrives, show tool activity, and handle approvals interactively.
//
//	go run ./examples/streaming -prompt "list the files here and summarize"
//
// It needs the codex CLI installed and an authenticated account.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

func main() {
	prompt := flag.String("prompt", "List the files in this directory. 你写一下 1.md 内容是 123", "the message to send")
	write := flag.Bool("write", false, "allow the agent to modify files (asks before each change)")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	flag.Parse()

	if err := run(*prompt, *write, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(prompt string, write bool, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The handler covers the events worth showing a user. Note the approval
	// callbacks: without them the SDK would decline every request, so an agent
	// that needs to write would silently get nowhere.
	handler := codex.Handler{
		OnItemStarted: func(n *protocol.ItemStartedNotification) {
			if cmd, ok := n.Item.AsCommandExecution(); ok {
				fmt.Fprintf(os.Stderr, "\n  $ %s\n", cmd.Command)
			}
			if fc, ok := n.Item.AsFileChange(); ok {
				for _, ch := range fc.Changes {
					fmt.Fprintf(os.Stderr, "\n  edit %s\n", ch.Path)
				}
			}
		},
		OnTurnPlan: func(n *protocol.TurnPlanUpdatedNotification) {
			fmt.Fprintln(os.Stderr, "\n  plan:")
			for _, step := range n.Plan {
				fmt.Fprintf(os.Stderr, "    [%s] %s\n", step.Status, step.Step)
			}
		},
		OnWarning: func(n *protocol.WarningNotification) {
			fmt.Fprintf(os.Stderr, "\n  warning: %s\n", n.Message)
		},
		OnCommandApproval: func(p *protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
			reason := ""
			if p.Reason != nil {
				reason = " (" + *p.Reason + ")"
			}
			command := "the requested command"
			if p.Command != nil {
				command = *p.Command
			}
			if confirm(fmt.Sprintf("run %q%s?", command, reason)) {
				return protocol.NewCommandExecutionApprovalDecisionAccept(), nil
			}
			return protocol.NewCommandExecutionApprovalDecisionDecline(), nil
		},
		OnFileChangeApproval: func(p *protocol.FileChangeRequestApprovalParams) (protocol.FileChangeApprovalDecision, error) {
			if confirm("apply the proposed file changes?") {
				return protocol.NewFileChangeApprovalDecisionAccept(), nil
			}
			return protocol.NewFileChangeApprovalDecisionDecline(), nil
		},
	}

	client, err := codex.New(ctx,
		codex.WithClientInfo("codex-go-streaming-example", "Codex Go SDK Streaming Example", "0.1.0"),
		codex.WithHandler(handler),
	)
	if err != nil {
		return err
	}
	defer client.Close()

	cwd, _ := os.Getwd()
	params := protocol.ThreadStartParams{Cwd: &cwd}
	if write {
		mode := protocol.SandboxModeWorkspaceWrite
		params.Sandbox = &mode
	} else {
		mode := protocol.SandboxModeReadOnly
		params.Sandbox = &mode
	}

	thread, err := client.StartThread(ctx, params)
	if err != nil {
		return err
	}

	stream, err := thread.RunStream(ctx, protocol.TurnStartParams{
		Input: []*protocol.UserInput{codex.TextInput(prompt)},
	})
	if err != nil {
		return err
	}

	// Events arrive in order, so printing deltas as they come reassembles the
	// reply correctly.
	for ev := range stream.Events() {
		switch ev.Kind {
		case codex.EventItemStarted:
			fmt.Print(ev.Item.AsMCPToolCall())
		case codex.EventReasoningDelta:
			fmt.Print(ev.Delta)
		case codex.EventAgentMessageDelta:
			fmt.Print(ev.Delta)
		case codex.EventCommandOutputDelta:
			fmt.Fprint(os.Stderr, indent(ev.Delta))
		case codex.EventError:
			if ev.Err != nil && ev.Err.Error != nil {
				fmt.Fprintf(os.Stderr, "\n  error: %s\n", ev.Err.Error.Message)
			}
		}
	}
	fmt.Println()

	result, err := stream.Result()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nturn %s: %s, %d items\n",
		stream.TurnID(), result.Status(), len(result.Items))
	return nil
}

func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "\n  %s [y/N] ", question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

func indent(s string) string {
	return strings.ReplaceAll(s, "\n", "\n    ")
}
