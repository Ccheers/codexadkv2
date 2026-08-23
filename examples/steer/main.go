// Command steer demonstrates steering: appending input to a turn that is already
// running, without starting a new one.
//
//	go run ./examples/steer
//	go run ./examples/steer -steer "actually, skip the tests and just list the files"
//
// The default prompt is a deliberately multi-step task, because steering is only
// observable while the agent still has work left to do. A one-shot question would
// finish before the steer arrives, and the server would reject it.
//
// Steering is worth understanding as distinct from a follow-up turn: it feeds the
// SAME turn, so the agent adjusts what it is currently doing rather than starting
// over with new context.
//
// It needs the codex CLI installed and an authenticated account.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

const defaultPrompt = `Do these steps one at a time, reporting after each:
1. List the Go files in this directory with their line counts.
2. Name the three largest ones.
3. For each of those three, summarize what it does in one sentence.
4. Finally, say which file a newcomer should read first and why.`

func main() {
	prompt := flag.String("prompt", defaultPrompt, "the initial multi-step task")
	steerText := flag.String("steer", "Also mention which file has the most tests.",
		"the message to inject mid-turn")
	after := flag.Duration("after", 0,
		"steer this long after the turn starts; 0 waits for the agent's first output instead")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	flag.Parse()

	if err := run(*prompt, *steerText, *after, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(prompt, steerText string, after, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// firstOutput closes as soon as the agent produces anything, which is the
	// signal that the turn is genuinely under way and can accept a steer.
	var once sync.Once
	firstOutput := make(chan struct{})
	signalStarted := func() { once.Do(func() { close(firstOutput) }) }

	handler := codex.Handler{
		OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
			fmt.Print(n.Delta)
			signalStarted()
		},
		OnItemStarted: func(n *protocol.ItemStartedNotification) {
			if cmd, ok := n.Item.AsCommandExecution(); ok {
				fmt.Fprintf(os.Stderr, "\n[cmd] %s\n", cmd.Command)
			}
			signalStarted()
		},
		OnTurnStarted: func(n *protocol.TurnStartedNotification) {
			if n.Turn != nil {
				fmt.Fprintf(os.Stderr, "[turn %s started]\n", n.Turn.ID)
			}
		},
		OnError: func(n *protocol.ErrorNotification) {
			if n.Error != nil {
				fmt.Fprintf(os.Stderr, "\n[error] %s\n", n.Error.Message)
			}
		},
	}

	cwd, _ := os.Getwd()
	session, err := codex.Open(ctx,
		codex.WithClientInfo("codex-go-steer-example", "Codex Go SDK Steer Example", "0.1.0"),
		codex.WithHandler(handler),
		codex.WithThreadOptions(
			protocol.WithThreadStartParamsCwd(cwd),
			protocol.WithThreadStartParamsSandbox(protocol.SandboxModeReadOnly),
			protocol.WithThreadStartParamsApprovalPolicy(protocol.NewAskForApprovalNever()),
		),
	)
	if err != nil {
		return err
	}
	defer session.Close()

	fmt.Fprintf(os.Stderr, "thread %s\n\n", session.ID())

	// Run blocks for the whole turn, so the steer has to come from another
	// goroutine. That is the shape of the API rather than a workaround: the turn is
	// one unit of work, and steering adjusts it while it runs.
	steerDone := make(chan error, 1)
	go func() {
		steerDone <- steerOnce(ctx, session, steerText, after, firstOutput)
	}()

	result, err := session.RunText(ctx, prompt)

	// Report the steer outcome before the turn error, since a failed steer explains
	// a turn that ignored the injected instruction.
	if serr := <-steerDone; serr != nil {
		fmt.Fprintf(os.Stderr, "\n[steer failed] %v\n", serr)
	}
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Fprintf(os.Stderr, "\n[turn %s: %d items]\n", result.Status(), len(result.Items))

	// What this example proves and what it does not: "[steer accepted]" above means
	// the server took the input into the running turn, which is the SDK behaviour
	// under test. Whether the agent then acts on it is a model decision, and a
	// steer arriving late in a long task is often reasonably ignored. So report the
	// two separately rather than treating the agent's wording as a pass/fail.
	if idx := strings.Index(strings.ToLower(result.AgentMessage), "test"); idx >= 0 {
		fmt.Fprintln(os.Stderr, "[the final reply mentions tests, so the agent acted on the steer]")
	} else {
		fmt.Fprintln(os.Stderr, "[the final reply does not mention the steered topic; "+
			"the steer was still accepted by the server, but the agent did not fold it "+
			"into its answer. Steering earlier with -after usually helps]")
	}
	return nil
}

// steerOnce waits for the right moment and then injects one message.
func steerOnce(
	ctx context.Context,
	session *codex.Session,
	text string,
	after time.Duration,
	firstOutput <-chan struct{},
) error {
	// Wait until the agent is actually working. Steering before the turn has begun
	// fails, because the server matches the steer against the active turn id.
	if after > 0 {
		select {
		case <-time.After(after):
		case <-ctx.Done():
			return ctx.Err()
		}
	} else {
		select {
		case <-firstOutput:
			// Give the agent a moment past its first token so the steer lands
			// mid-task rather than racing the opening of the turn.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	turnID := session.CurrentTurnID()
	if turnID == "" {
		return errors.New("no turn was in flight by the time we tried to steer")
	}

	fmt.Fprintf(os.Stderr, "\n[steering turn %s: %q]\n", turnID, text)
	if _, err := session.SteerText(ctx, text); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "[steer accepted]")
	return nil
}
