// Command streaming shows the callback style: register handlers once, then run a
// turn and let the callbacks render output as it arrives.
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
	prompt := flag.String("prompt", "List the files in this directory.", "the message to send")
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

	// Callbacks are the single delivery path for everything the server streams.
	// They fire on the thread's own goroutine in arrival order, so appending
	// deltas as they come reassembles the reply correctly.
	handler := codex.Handler{
		OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
			fmt.Print(n.Delta)
		},
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
		OnCommandOutputDelta: func(n *protocol.CommandExecutionOutputDeltaNotification) {
			fmt.Fprint(os.Stderr, indent(n.Delta))
		},
		OnTurnPlan: func(n *protocol.TurnPlanUpdatedNotification) {
			fmt.Fprintln(os.Stderr, "\n  plan:")
			for _, step := range n.Plan {
				fmt.Fprintf(os.Stderr, "    [%s] %s\n", step.Status, step.Step)
			}
		},
		OnError: func(n *protocol.ErrorNotification) {
			if n.Error != nil {
				fmt.Fprintf(os.Stderr, "\n  error: %s\n", n.Error.Message)
			}
		},
		OnWarning: func(n *protocol.WarningNotification) {
			fmt.Fprintf(os.Stderr, "\n  warning: %s\n", n.Message)
		},

		// Approvals are requests, not notifications: the turn is blocked until
		// these return. Without them the SDK declines everything, so an agent
		// asked to write would get nowhere.
		OnCommandApproval: func(p *protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
			command := "the requested command"
			if p.Command != nil {
				command = *p.Command
			}
			reason := ""
			if p.Reason != nil {
				reason = " (" + *p.Reason + ")"
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
	sandbox := protocol.SandboxModeReadOnly
	if write {
		sandbox = protocol.SandboxModeWorkspaceWrite
	}

	thread, err := client.StartThread(ctx, protocol.NewThreadStartParams(
		protocol.WithThreadStartParamsCwd(cwd),
		protocol.WithThreadStartParamsSandbox(sandbox),
	))
	if err != nil {
		return err
	}

	// Run blocks until the turn ends; the callbacks above do the rendering in the
	// meantime.
	result, err := thread.Run(ctx, protocol.NewTurnStartParams(
		[]*protocol.UserInput{codex.TextInput(prompt)},
		thread.ID(),
	))
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Fprintf(os.Stderr, "\nturn %s: %d items\n", result.Status(), len(result.Items))
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
