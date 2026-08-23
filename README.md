# codex-go

A Go client for the [Codex app-server](https://learn.chatgpt.com/docs/app-server) JSON-RPC protocol. It spawns `codex app-server` as a child process and gives you threads, turns, streamed items, and approvals as ordinary Go types.

```go
client, err := codex.New(ctx,
    codex.WithClientInfo("my_product", "My Product", "1.0.0"))
if err != nil {
    return err
}
defer client.Close()

thread, err := client.StartThread(ctx, protocol.ThreadStartParams{})
if err != nil {
    return err
}

result, err := thread.RunText(ctx, "Summarize this repo.")
if err != nil {
    return err
}
fmt.Println(result.AgentMessage)
```

## Requirements

- Go 1.23+ (uses range-over-func iterators)
- The [Codex CLI](https://learn.chatgpt.com/docs/codex) on `PATH`, authenticated
- No third-party dependencies — stdlib only

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

```go
stream, err := thread.RunStream(ctx, protocol.TurnStartParams{
    Input: []*protocol.UserInput{codex.TextInput("List the files here.")},
})
if err != nil {
    return err
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

result, err := stream.Result()
```

## Notifications

Register callbacks with `WithHandler`. Every field is optional; unset means ignored.

```go
codex.WithHandler(codex.Handler{
    OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
        fmt.Print(n.Delta)
    },
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
