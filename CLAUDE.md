# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

A Go client for the [Codex app-server](https://learn.chatgpt.com/docs/app-server)
JSON-RPC 2.0 protocol. It spawns `codex app-server` as a stdio child process and
exposes threads, turns, streamed items, approvals, and dynamic tools.

## Commands

```bash
make test              # hermetic: needs neither the codex binary nor an account
make test-race         # always run this; the dispatch layer is concurrent
make test-integration  # talks to a real app-server; skips itself if codex is absent
make generate          # regenerate codex/protocol from vendored schemas
make sync-schemas      # refresh vendored schemas from the installed codex CLI
make check-generated   # CI gate: fails if generated output is stale or hand-edited
make vet               # go vet, including the integration build tag
```

Single test, and integration tests (behind a build tag):

```bash
go test ./codex/ -run TestSteerCurrentTurn
go test -tags integration ./codex/ -run TestRealHandshake -v
```

The examples run against a real agent and are the fastest end-to-end check:

```bash
go run ./examples/basic -prompt "Reply with exactly: OK"
go run ./examples/tools      # dynamic tools
go run ./examples/steer      # mid-turn steering
```

## Architecture

Three layers, one-way dependencies. Keep it that way: `codex/protocol` must
import nothing from `codex`, or regeneration starts fighting hand-written code.

```
codex/            public API (thin wire layer + ergonomic Session/Thread)
codex/protocol/   ~950 GENERATED types. Never hand-edit.
internal/jsonrpc/ framing, request correlation, child-process supervision
internal/cmd/schemagen/  the generator
internal/schemas/ vendored JSON Schema, stable + experimental variants
```

**Types are generated and the output is committed**, so `go get` needs no codegen
step and no `codex` binary. The generator reads vendored JSON Schema rather than
shelling out, which keeps builds hermetic. See
[`docs/adr/0001`](docs/adr/0001-generated-protocol-types.md).

**Two API layers.** `codex/methods.go` mirrors the wire one method at a time
(`ThreadStart`, `TurnStart`, …) and is auditable against the app-server docs.
`Session`/`Thread` sit on top and are built only from those calls. Anything
unwrapped is reachable via `client.Call(ctx, method, params, &result)`.

**`codex.Open` is the normal entry point** — it spawns the server, handshakes, and
starts one thread in a single call.

## Decisions that were tried and reverted

These are the ones most likely to be re-attempted. Read before "improving" them.

**Handlers are client-level, NOT per-thread.** The server creates threads the
caller never asks for: five sub-agent source kinds exist, and `spawnAgent`
produces thread ids `StartThread` never returns. A thread-keyed handler map
silently drops all of that into `OnUnhandled`. Every notification carries its
`threadId`, so callbacks can distinguish whose work it is.

**Callbacks are the only delivery mechanism.** A `TurnStream` event-channel API
existed and was deleted (commit `65458e7`): two mechanisms for the same
notifications means two sets of ordering and backpressure rules to keep in
agreement. `Thread.Run` blocks while callbacks stream.

**One client drives one caller-owned thread.** A second `StartThread` returns
`ErrMainThreadExists`. `Client.Thread(id)` does *not* claim the slot, because
sub-agent and review threads must stay addressable.

## Protocol traps

Full list with context in [`docs/HANDOFF.md`](docs/HANDOFF.md). The ones that bite
most often:

- **`item/*` is the source of truth for turn items**, not the `turn/completed`
  payload, which can carry an empty `items` array.
- **`SandboxMode` and `SandboxPolicy` are the same concept, different spellings**:
  `thread/start` takes kebab-case `read-only`, `turn/start` takes camelCase
  `readOnly`. Deriving one from the other breaks the wire.
- **Four enum casings coexist** (kebab, snake, SCREAMING_SNAKE, camel). Enum values
  and struct tags are copied verbatim from the schema, never derived from Go
  identifiers. Struct tag casing varies *within* a single type.
- **Seven fields are tri-state** (`Option<Option<T>>`: absent ≠ null). The JSON
  Schema collapses this; only the TypeScript output preserves it, so
  `triStateFields` is hand-maintained.
- **Never opt out of `requiredNotifications`** — `Thread.Run` consumes them and
  would hang. `client.go` rejects it.
- **Unanswered server-requests hang the turn forever.** A missing approval handler
  declines loudly; an unhandled dynamic tool call answers `success:false`. Never
  add a code path that stays silent.

## Maintenance

**Version bumps: invoke the `codex-schema-upgrade` skill** rather than improvising.
It encodes the procedure and every failure mode.

Five tables are hand-maintained because the schema cannot express them. Everything
else is derived:

| Table | File |
|---|---|
| `resultOverrides` — the schema has **no** method→result mapping | `schemagen/methods.go` |
| `triStateFields` — JSON Schema collapses `Option<Option<T>>` | `schemagen/main.go` |
| `rawTypes` — untagged unions, not structurally discriminable | `schemagen/main.go` |
| `skipTypes` — umbrella unions (217 types for `ServerNotification` alone) | `schemagen/main.go` |
| `requiredNotifications` — what `Thread.Run` consumes | `codex/client.go` |

**Forward compatibility is a design goal, not an accident.** Unknown methods,
union variants, and enum values all decode without error and round-trip unchanged;
the SDK is expected to run against servers newer than its schema. Do not add
strict validation that rejects unknown input.

**Tests use a scripted in-process fake server** (`codex/fakeserver_test.go`), so
the default suite needs no binary or credentials. Integration tests deliberately
cover only what needs no auth: spawn, handshake, model catalog, shutdown.
