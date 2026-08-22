package codex_test

// These examples double as compile checks for the snippets in README.md. If the
// API changes, the docs break the build instead of quietly going stale.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// Example is the shortest useful program: start a thread, run one turn, print
// the reply.
func Example() {
	ctx := context.Background()

	client, err := codex.New(ctx,
		codex.WithClientInfo("my_product", "My Product", "1.0.0"))
	if err != nil {
		log.Fatal(err)
	}
	defer client.Close()

	thread, err := client.StartThread(ctx, protocol.ThreadStartParams{})
	if err != nil {
		log.Fatal(err)
	}

	result, err := thread.RunText(ctx, "Summarize this repo.")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(result.AgentMessage)
}

// ExampleThread_RunStream prints the agent's reply as it arrives and reports
// commands as they start.
func ExampleThread_RunStream() {
	ctx := context.Background()
	client, _ := codex.New(ctx)
	defer client.Close()
	thread, _ := client.StartThread(ctx, protocol.ThreadStartParams{})

	stream, err := thread.RunStream(ctx, protocol.TurnStartParams{
		Input: []*protocol.UserInput{codex.TextInput("List the files here.")},
	})
	if err != nil {
		log.Fatal(err)
	}

	for ev := range stream.Events() {
		switch ev.Kind {
		case codex.EventAgentMessageDelta:
			fmt.Print(ev.Delta)
		case codex.EventItemStarted:
			if cmd, ok := ev.Item.AsCommandExecution(); ok {
				fmt.Fprintf(os.Stderr, "$ %s\n", cmd.Command)
			}
		}
	}

	if _, err := stream.Result(); err != nil {
		log.Fatal(err)
	}
}

// ExampleWithHandler registers notification callbacks.
func ExampleWithHandler() {
	_ = codex.WithHandler(codex.Handler{
		OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
			fmt.Print(n.Delta)
		},
		OnTurnCompleted: func(n *protocol.TurnCompletedNotification) {
			log.Printf("turn %s: %s", n.Turn.ID, n.Turn.Status)
		},
		// Anything with no specific callback, including methods this build does
		// not know about, lands here rather than being dropped.
		OnUnhandled: func(method string, params []byte) {
			log.Printf("unhandled %s: %s", method, params)
		},
	})
}

// ExampleHandler_OnCommandApproval decides approvals by inspecting the command.
func ExampleHandler_OnCommandApproval() {
	_ = codex.WithHandler(codex.Handler{
		OnCommandApproval: func(p *protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
			if p.Command != nil && strings.HasPrefix(*p.Command, "rm ") {
				return protocol.NewCommandExecutionApprovalDecisionDecline(), nil
			}
			return protocol.NewCommandExecutionApprovalDecisionAccept(), nil
		},
	})
}

// ExampleClient_StartThread configures a thread so approvals never fire, which
// is what unattended operation should do rather than relying on the
// decline-by-default behaviour.
func ExampleClient_StartThread() {
	ctx := context.Background()
	client, _ := codex.New(ctx)
	defer client.Close()

	never := protocol.NewAskForApprovalNever()
	sandbox := protocol.SandboxModeReadOnly
	thread, err := client.StartThread(ctx, protocol.ThreadStartParams{
		ApprovalPolicy: &never,
		Sandbox:        &sandbox,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(thread.ID())
}

// ExampleClient_ListThreads iterates every stored thread, fetching pages as
// needed.
func ExampleClient_ListThreads() {
	ctx := context.Background()
	client, _ := codex.New(ctx)
	defer client.Close()

	for thread, err := range client.ListThreads(ctx, protocol.ThreadListParams{}) {
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(thread.ID, thread.Preview)
	}
}

// ExampleTurnFailedError shows branching on the server's error classification.
func ExampleTurnFailedError() {
	ctx := context.Background()
	client, _ := codex.New(ctx)
	defer client.Close()
	thread, _ := client.StartThread(ctx, protocol.ThreadStartParams{})

	_, err := thread.RunText(ctx, "do something expensive")

	var failed *codex.TurnFailedError
	if errors.As(err, &failed) && failed.Info.IsUsageLimitExceeded() {
		log.Println("usage limit reached; back off and retry later")
	}

	// JSON-RPC errors keep their code and data, so no message matching is needed.
	if errors.Is(err, jsonrpc.ErrServerOverloaded) {
		log.Println("server overloaded; retry with backoff")
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		log.Printf("code=%d data=%s", rpcErr.Code, rpcErr.Data)
	}
}

// ExampleNullable demonstrates the three states of a tri-state field.
func ExampleNullable() {
	var params protocol.TurnStartParams

	params.ServiceTier = protocol.Value("flex")  // set it
	params.ServiceTier = protocol.Null[string]() // clear it on the server
	params.ServiceTier = nil                     // leave it unchanged

	fmt.Println(params.ServiceTier == nil)
	// Output: true
}

// ExampleSandboxPolicy shows union construction and the typed accessor.
func ExampleSandboxPolicy() {
	policy := protocol.NewSandboxPolicyWorkspaceWrite(
		protocol.SandboxPolicyWorkspaceWritePayload{
			WritableRoots: []protocol.AbsolutePathBuf{"/repo"},
		})

	if ww, ok := policy.AsWorkspaceWrite(); ok {
		fmt.Println(ww.WritableRoots)
	}

	// The same concept has two spellings on the wire: thread/start takes the
	// kebab-case SandboxMode, turn/start takes the camelCase SandboxPolicy tag.
	fmt.Println(protocol.SandboxModeWorkspaceWrite, protocol.SandboxPolicyWorkspaceWrite)
	// Output:
	// [/repo]
	// workspace-write workspaceWrite
}

// ExampleClient_Call reaches a method the typed API does not wrap.
func ExampleClient_Call() {
	ctx := context.Background()
	client, _ := codex.New(ctx)
	defer client.Close()

	var out protocol.PluginListResponse
	if err := client.Call(ctx, "plugin/list", protocol.PluginListParams{}, &out); err != nil {
		log.Fatal(err)
	}
}
