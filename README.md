# codex-go

A Go client for the [Codex app-server](https://learn.chatgpt.com/docs/app-server) JSON-RPC protocol. It spawns `codex app-server` as a child process and gives you threads, turns, streamed items, and approvals as ordinary Go types.

```go
session, err := codex.Open(ctx,
    codex.WithClientInfo("my_product", "My Product", "1.0.0"))
if err != nil {
    return err
}
defer session.Close()

result, err := session.RunText(ctx, "Summarize this repo.")
if err != nil {
    return err
}
fmt.Println(result.AgentMessage)
```

`Open` spawns the server, completes the handshake, and starts one thread in a single call.

## Requirements

- Go 1.23+ (uses range-over-func iterators)
- The [Codex CLI](https://learn.chatgpt.com/docs/codex) on `PATH`, authenticated
- One dependency: `github.com/invopop/jsonschema`, for reflecting tool argument schemas

## Install

```bash
go get github.com/ccheers/codexadkv2
```

## Two layers

**The thin layer** mirrors the wire one-to-one. `client.ThreadStart`, `client.TurnStart`, `client.ThreadList` and friends take and return the generated types in `codex/protocol` unchanged, so you can check behaviour directly against the app-server docs.

**The ergonomic layer** sits on top and is built only from thin-layer calls. `Thread.Run` starts a turn and blocks until it completes; `Thread.RunStream` gives you an ordered event channel.

Anything not wrapped is still reachable:

```go
var out protocol.PluginListResponse
err := client.Call(ctx, "plugin/list", protocol.PluginListParams{}, &out)
```

## Streaming

There is one delivery mechanism: callbacks. `Run` blocks while they fire, so printing as output arrives and reading the final result are the same flow.

```go
client, err := codex.New(ctx, codex.WithHandler(codex.Handler{
    OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
        fmt.Print(n.Delta)
    },
    OnItemStarted: func(n *protocol.ItemStartedNotification) {
        if cmd, ok := n.Item.AsCommandExecution(); ok {
            fmt.Fprintf(os.Stderr, "$ %s\n", cmd.Command)
        }
    },
}))

thread, err := client.StartThread(ctx, protocol.ThreadStartParams{})
result, err := thread.RunText(ctx, "List the files here.")  // callbacks fire while this blocks
```

There is deliberately no event-channel API. Two mechanisms delivering the same notifications means two sets of ordering and backpressure rules to keep in agreement, and the callback path already covers the streaming case.

## Notifications

Register callbacks with `WithHandler`. Every field is optional; unset means ignored.

```go
codex.WithHandler(codex.Handler{
    OnTurnCompleted: func(n *protocol.TurnCompletedNotification) {
        log.Printf("turn %s: %s", n.Turn.ID, n.Turn.Status)
    },
    // Anything with no specific callback, including methods this build
    // doesn't know about, lands here rather than being dropped.
    OnUnhandled: func(method string, params []byte) {
        log.Printf("unhandled %s: %s", method, params)
    },
})
```

Callbacks for one thread run on that thread's own goroutine **in arrival order**, so deltas concatenate correctly. Different threads progress independently, so a slow callback on one thread doesn't delay another. Callbacks must not block indefinitely — the per-thread queue is bounded, and a stalled callback eventually applies backpressure to the connection. (Notifications are never *dropped* on overflow; losing a delta would corrupt the message you're reassembling.)

**Handlers are client-level, not per-thread.** That's not an oversight: the server creates threads you never asked for — sub-agents, reviews, compaction — and their ids are never returned by `StartThread`. A per-thread handler map would silently drop all of it. Every notification carries its `threadId`, so compare against `client.MainThread().ID()` to tell your own work from a sub-agent's.

## Sessions and threads

`codex.Open` merges the client and its thread into one object, which is what most programs want:

```go
session, err := codex.Open(ctx,
    codex.WithClientInfo("my_product", "My Product", "1.0.0"),
    codex.WithHandler(handler),
    codex.WithThreadOptions(
        protocol.WithThreadStartParamsCwd("/repo"),
        protocol.WithThreadStartParamsSandbox(protocol.SandboxModeReadOnly),
    ),
)
defer session.Close()

result, err := session.RunText(ctx, "Run the tests.")
```

Client options pass in directly; thread configuration goes through `WithThreadOptions`. If starting the thread fails, `Open` closes the client, so a failed open leaks no child process.

Under it, **a client drives exactly one caller-owned thread.** With `Open` that's structural — there's no second `StartThread` to call. Using `Client` directly, a second `StartThread` or `ResumeThread` returns `ErrMainThreadExists`; run another client for another conversation.

```go
client, err := codex.New(ctx)                       // when you need resume, fork,
thread, err := client.StartThread(ctx, params)      // or to inspect the server first
same := client.MainThread()
```

Addressing a thread you didn't create stays available and doesn't claim the slot — necessary, because sub-agent and review threads are created by the server:

```go
sub := client.Thread(someSubAgentThreadID)
```

## Steering

Steering appends input to a turn that is **already running**, so the agent adjusts what it is currently doing rather than starting over. `Run` blocks for the whole turn, so a steer comes from another goroutine:

```go
go func() {
    // Wait until a turn is actually in flight; steering before that fails,
    // because the server matches the steer against the active turn id.
    for session.CurrentTurnID() == "" {
        time.Sleep(10 * time.Millisecond)
    }
    _, err := session.SteerText(ctx, "also check the tests")
}()

result, err := session.RunText(ctx, longMultiStepTask)
```

`CurrentTurnID` saves capturing the id out of an `OnTurnStarted` callback. Use `Steer(ctx, turnID, ...)` when you specifically want the server to verify you are steering the turn you think you are.

Steering only makes sense while the agent still has work left, so `go run ./examples/steer` uses a deliberately multi-step prompt. A one-shot question finishes before the steer arrives and the server rejects it.

## Approvals

Approvals are server *requests*, not notifications: the turn is blocked until you answer.

```go
codex.WithHandler(codex.Handler{
    OnCommandApproval: func(p *protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
        if strings.HasPrefix(*p.Command, "rm ") {
            return protocol.NewCommandExecutionApprovalDecisionDecline(), nil
        }
        return protocol.NewCommandExecutionApprovalDecisionAccept(), nil
    },
})
```

**If no approval callback is registered, this SDK answers `decline` and logs loudly** — it never leaves the request unanswered, because an unanswered approval hangs the turn forever with no diagnostic.

For unattended use, don't rely on that. Configure the thread so approvals never fire:

```go
never := protocol.NewAskForApprovalNever()
sandbox := protocol.SandboxModeReadOnly
thread, err := client.StartThread(ctx, protocol.ThreadStartParams{
    ApprovalPolicy: &never,
    Sandbox:        &sandbox,
})
```

## Dynamic tools

Expose Go functions to the model. The session registers them at thread start, answers tool calls by running the matching handler, and returns the result — no callback to wire up, no dispatch to write.

```go
type GrepArgs struct {
    Pattern string `json:"pattern" jsonschema:"description=regular expression to match"`
    Path    string `json:"path,omitempty" jsonschema:"description=directory to search"`
}

grep := codex.NewTool("grep", "Search repo files by regular expression",
    func(ctx context.Context, callID string, a GrepArgs) (string, error) {
        return runGrep(ctx, a.Pattern, a.Path)
    })

session, err := codex.Open(ctx,
    codex.WithTools(grep),
    codex.WithToolGroups(codex.ToolGroup{
        Name:        "inventory",
        Description: "Read and update the warehouse inventory held by this program",
        Tools:       []codex.DynamicTool{checkTool, restockTool},
    }),
)
```

The input schema is reflected from the `Args` type. **A field is required unless its json tag has `omitempty`** — the opposite of what most people expect, and it changes how the model calls your tool, so mark optional arguments explicitly.

**Groups are progressive disclosure.** The model reads the group's `Description` to decide whether the area is relevant, and only then looks at the tools inside. That description is load-bearing, which is why it belongs to the group rather than being derived from its tools. A tool carries no namespace of its own, so the same instance can be registered standalone in one session and inside a group in another.

**Return an error, not an error string.** A non-nil error becomes a failed tool call with your message as the reason, and the model can act on it. Observed live: an agent called `check("sprockets")`, got back `no such item "sprockets"; known items are widget, gizmo, sprocket`, and retried with the singular form.

Dynamic tools are experimental, so `Open` enables the capability automatically when any tool is registered — the server rejects `dynamicTools` without it. Registration mistakes (duplicate names, a group with no description) fail at `Open` before a server is spawned, rather than mid-turn.

Run `go run ./examples/tools` to see it work.

## Errors

JSON-RPC errors preserve `code` and `data`, so you can branch without matching on message text:

```go
if errors.Is(err, jsonrpc.ErrServerOverloaded) { /* back off */ }

var rpcErr *jsonrpc.Error
if errors.As(err, &rpcErr) {
    log.Printf("code=%d data=%s", rpcErr.Code, rpcErr.Data)
}
```

A turn that fails returns a `*TurnFailedError` carrying the server's classification:

```go
var failed *codex.TurnFailedError
if errors.As(err, &failed) && failed.Info.IsUsageLimitExceeded() {
    // wait and retry
}
```

An **interrupted** turn is not an error — check `result.Interrupted()`. Cancelling the context sends `turn/interrupt` and then returns `ctx.Err()`, so a cancelled context doesn't leave the agent working.

There is no automatic retry. Wrap calls yourself if you want one; some methods have side effects.

## Pagination

```go
for thread, err := range client.ListThreads(ctx, protocol.ThreadListParams{}) {
    if err != nil {
        return err
    }
    fmt.Println(thread.ID, thread.Preview)
}
```

## Options

| Option | Default |
|---|---|
| `WithBinaryPath` | `codex` from `PATH` |
| `WithClientInfo` | `codex-go-sdk` — **set this**, it's what usage is attributed to |
| `WithStderr` | discarded (a bounded tail is always kept and attached to errors) |
| `WithExperimentalAPI` | off |
| `WithOptOutNotifications` | none |
| `WithHandler` | no callbacks |
| `WithWorkdirWatchDisabled` | auto-watch thread cwd |
| `WithLogger` | discards |
| `WithNotificationBuffer` | 256 per thread |
| `WithHandshakeTimeout` | 30s |
| `WithShutdownGrace` | 5s before `SIGKILL` |
| `WithTransport` | spawns a child process |

## Types

All ~950 protocol types are generated into `codex/protocol` and committed, so `go get` needs no codegen step and no `codex` binary.

**Constructors and options.** Every generated struct has a `New<Type>` constructor plus `With<Type><Field>` options. Required fields are positional arguments, so the compiler catches a missing one; optional fields are options that take plain values and handle the pointer-taking for you:

```go
params := protocol.NewTurnStartParams(input, "thr_1",
    protocol.WithTurnStartParamsModel("gpt-5.6-terra"),
    protocol.WithTurnStartParamsCwd("/repo"),
)
```

Option names are type-prefixed because Go has no per-type function namespace and dozens of types share field names like `Model` and `Cwd`. Options are values, so a house style can be built once and reused:

```go
base := []protocol.ThreadStartParamsOption{
    protocol.WithThreadStartParamsSandbox(protocol.SandboxModeReadOnly),
    protocol.WithThreadStartParamsApprovalPolicy(protocol.NewAskForApprovalNever()),
}
thread := protocol.NewThreadStartParams(append(base, protocol.WithThreadStartParamsCwd("/repo"))...)
```

Structs stay plain structs — a literal still works, and `protocol.Ptr` covers the cases where that reads better:

```go
params := protocol.ThreadStartParams{Cwd: protocol.Ptr("/repo")}
```

**Enums** are string-typed constants with the wire value verbatim, plus `IsKnown()`:

```go
protocol.SandboxModeReadOnly  // SandboxMode = "read-only"
```

**Unions** are one struct with a discriminant plus typed accessors:

```go
if ww, ok := policy.AsWorkspaceWrite(); ok {
    fmt.Println(ww.WritableRoots)
}
policy := protocol.NewSandboxPolicyWorkspaceWrite(
    protocol.SandboxPolicyWorkspaceWritePayload{WritableRoots: []protocol.AbsolutePathBuf{"/repo"}})
```

Marshaling is driven strictly by the discriminant, so a struct whose tag and payload disagree can't put a malformed message on the wire.

**Forward compatibility.** Unknown methods, union variants, and enum values all decode without error and round-trip unchanged — this SDK is expected to run against servers newer than the schema it was built from. Use `enum.IsKnown()` to detect a value this build doesn't define.

**Two things that look like duplicates but aren't:** `thread/start` takes `SandboxMode` (kebab-case: `read-only`), while `turn/start` takes `SandboxPolicy` (camelCase tags: `readOnly`). And three mutually incompatible path types coexist (`AbsolutePathBuf`, `LegacyAppPathString`, `PathUri`) — don't convert between them.

**Tri-state fields.** Five fields distinguish "absent" from "explicitly null", where absent leaves the server's value alone and null clears it:

```go
params.ServiceTier = protocol.Value("flex")  // set
params.ServiceTier = protocol.Null[string]() // clear it server-side
params.ServiceTier = nil                     // leave unchanged
```

Via options, the third state is the absence of an option and clearing has its own:

```go
protocol.WithTurnStartParamsServiceTier("flex") // set
protocol.ClearTurnStartParamsServiceTier()      // clear it server-side
```

## Experimental API

Off by default: the server rejects experimental methods and fields unless you opt in.

```go
client, err := codex.New(ctx, codex.WithExperimentalAPI())
```

Members that need it are marked `EXPERIMENTAL` in their godoc. That annotation is computed by diffing the stable and experimental schemas, because the schema carries no machine-readable marker for it.

## Development

```bash
make test              # hermetic; needs neither codex nor an account
make test-race
make test-integration  # talks to a real codex app-server
make generate          # regenerate codex/protocol from vendored schemas
make sync-schemas      # refresh vendored schemas from the installed codex
make check-generated   # CI gate: fails if generated code is stale
```

See [`CONTEXT.md`](./CONTEXT.md) for the glossary and [`docs/adr/`](./docs/adr/) for the design decisions.

## Scope and known gaps

Covers initialize, threads, turns, items, notifications, approvals, review, and models — the surface named in the design brief. Everything else in the protocol has generated types and is reachable via `Call`, just without a typed wrapper.

- **stdio transport only.** WebSocket and Unix socket are unimplemented; both are marked experimental and unsupported upstream anyway. The `Transport` interface makes them additive.
- **Windows process-tree teardown is best-effort.** There's no `SIGTERM`, so shutdown goes straight to kill and grandchildren may be orphaned. A proper fix needs a Job Object.
- **The `McpElicitation*` schema subtree stays `json.RawMessage`.** It's a four-level-deep untagged union disambiguated only by structural inspection.
- **The method→result table is hand-maintained** (the schema has no result mapping at all), but the generator fails the build if it drifts in either direction.
- **Integration tests only cover what needs no auth** — spawn, handshake, model catalog, shutdown. Turn behaviour is covered against a scripted fake server instead.

## License

MIT
