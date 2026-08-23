// Command tools exposes Go functions to the model as dynamic tools and lets it
// call them.
//
//	go run ./examples/tools
//	go run ./examples/tools -prompt "what is 17 factorial, and what time is it in Tokyo?"
//
// The tools here are deliberately things the agent cannot do on its own with a
// shell: they answer from this process's memory. That makes it unambiguous when a
// tool actually ran, rather than the agent shelling out and reaching the same
// answer another way.
//
// Dynamic tools are an experimental app-server feature; codex.Open enables the
// capability automatically when tools are registered.
//
// It needs the codex CLI installed and an authenticated account.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

// inventory is state that lives only in this process, so the agent has no way to
// answer questions about it except by calling the tool.
var inventory = map[string]int{
	"widget":   42,
	"gizmo":    7,
	"sprocket": 0,
}

type factorialArgs struct {
	N int `json:"n" jsonschema:"description=the number to compute the factorial of"`
}

type stockArgs struct {
	Item string `json:"item" jsonschema:"description=the item name to look up"`
}

type restockArgs struct {
	Item     string `json:"item" jsonschema:"description=the item to restock"`
	Quantity int    `json:"quantity" jsonschema:"description=how many units to add"`
}

func main() {
	prompt := flag.String("prompt",
		"How many sprockets and widgets are in stock? Restock any item that is at zero "+
			"with 25 units, then tell me 20 factorial.",
		"the task to give the agent")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	flag.Parse()

	if err := run(*prompt, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(prompt string, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Track what the model actually called, so the run reports evidence rather
	// than an impression.
	var (
		mu     sync.Mutex
		called []string
	)
	note := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		mu.Lock()
		called = append(called, line)
		mu.Unlock()
		fmt.Fprintf(os.Stderr, "\n  [tool] %s\n", line)
	}

	// A top-level tool: pure computation, no state.
	factorial := codex.NewTool("factorial",
		"Compute the exact factorial of a non-negative integer, with no size limit",
		func(_ context.Context, callID string, a factorialArgs) (string, error) {
			if a.N < 0 {
				// A returned error becomes a failed tool call, so the model can see
				// what it did wrong and correct itself.
				return "", fmt.Errorf("factorial is undefined for negative numbers, got %d", a.N)
			}
			if a.N > 10000 {
				return "", fmt.Errorf("refusing to compute %d!; keep it under 10000", a.N)
			}
			note("factorial(%d) [call %s]", a.N, callID)
			return new(big.Int).MulRange(1, int64(max(a.N, 1))).String(), nil
		})

	// A group: related tools the model expands only if inventory is relevant.
	stock := codex.NewTool("check",
		"Look up the current stock level of one item",
		func(_ context.Context, _ string, a stockArgs) (string, error) {
			mu.Lock()
			qty, ok := inventory[strings.ToLower(a.Item)]
			mu.Unlock()
			note("inventory.check(%q)", a.Item)
			if !ok {
				return "", fmt.Errorf("no such item %q; known items are widget, gizmo, sprocket", a.Item)
			}
			return fmt.Sprintf("%s: %d in stock", a.Item, qty), nil
		})

	restock := codex.NewTool("restock",
		"Add units to an item's stock level",
		func(_ context.Context, _ string, a restockArgs) (string, error) {
			if a.Quantity <= 0 {
				return "", fmt.Errorf("quantity must be positive, got %d", a.Quantity)
			}
			key := strings.ToLower(a.Item)
			mu.Lock()
			_, ok := inventory[key]
			if ok {
				inventory[key] += a.Quantity
			}
			updated := inventory[key]
			mu.Unlock()
			note("inventory.restock(%q, %d)", a.Item, a.Quantity)
			if !ok {
				return "", fmt.Errorf("no such item %q", a.Item)
			}
			return fmt.Sprintf("%s is now at %d units", a.Item, updated), nil
		})

	cwd, _ := os.Getwd()
	session, err := codex.Open(ctx,
		codex.WithClientInfo("codex-go-tools-example", "Codex Go SDK Tools Example", "0.1.0"),
		codex.WithHandler(codex.Handler{
			OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
				fmt.Print(n.Delta)
			},
		}),
		codex.WithTools(factorial),
		codex.WithToolGroups(codex.ToolGroup{
			Name: "inventory",
			// The group description is the model's first hop: it decides from this
			// alone whether to look at the tools inside.
			Description: "Read and update the warehouse inventory held by this program. " +
				"Use it for any question about stock levels; the data exists nowhere else.",
			Tools: []codex.DynamicTool{stock, restock},
		}),
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

	fmt.Fprintf(os.Stderr, "thread %s\n", session.ID())

	result, err := session.RunText(ctx, prompt)
	if err != nil {
		return err
	}
	fmt.Println()

	mu.Lock()
	defer mu.Unlock()
	if len(called) == 0 {
		fmt.Fprintln(os.Stderr, "\n[no tool was called: the agent answered without them, "+
			"or the experimental capability was rejected]")
	} else {
		fmt.Fprintf(os.Stderr, "\n[%d tool calls]\n", len(called))
		for _, c := range called {
			fmt.Fprintf(os.Stderr, "  - %s\n", c)
		}
	}
	fmt.Fprintf(os.Stderr, "[turn %s, %d items]\n", result.Status(), len(result.Items))
	return nil
}
