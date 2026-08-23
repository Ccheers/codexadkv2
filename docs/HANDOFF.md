# Handoff: codex-go SDK

Written 2026-08-23. Everything here is verified against the tree at commit
`b410859`, not recalled. Where something is unverified it says so.

This is a maintainer's orientation document: the design reasoning, the layout, the
traps, and what to do next. It does not repeat what other artifacts already say —
follow the references.

## Read these first, in order

| What | Where | Why |
|---|---|---|
| User-facing API and idioms | [`README.md`](../README.md) | The API as a consumer sees it |
| Vocabulary | [`CONTEXT.md`](../CONTEXT.md) | Terms that look interchangeable but are not |
| Why the pipeline is generated | [`adr/0001`](adr/0001-generated-protocol-types.md) | Read before touching `internal/cmd/schemagen` |
| Why dispatch is shaped as it is | [`adr/0002`](adr/0002-per-thread-ordered-dispatch.md) | Read before touching `codex/dispatch.go` |
| Why a missing approval declines | [`adr/0003`](adr/0003-unanswered-approvals-decline.md) | Short; the reasoning generalizes |
| Why two schemas are vendored | [`adr/0004`](adr/0004-experimental-schema-diff.md) | Read before a version bump |
| How to do a version bump | [`../.claude/skills/codex-schema-upgrade/SKILL.md`](../.claude/skills/codex-schema-upgrade/) | Invoke the skill; don't improvise |

The five commit messages are unusually detailed and carry rationale not repeated
here. `git log` is worth reading in full — particularly `65458e7` (why callbacks
rather than event channels) and `b410859` (dynamic tools).

## What this is

A Go client for the Codex app-server JSON-RPC 2.0 protocol. It spawns
`codex app-server` as a child process over stdio and exposes threads, turns,
streamed items, approvals, and dynamic tools.

Scale: **9,384 hand-written lines, 35,129 generated, 81 tests + 13 examples.**
Built against **codex-cli 0.149.0** (`internal/schemas/VERSION`).

## Layout

```
codex/                    the public API
  client.go               Client: spawn, handshake, Call/Notify, main-thread slot
  session.go              Session + Open: client and thread as one object
  thread.go               Thread, Run, TurnResult, TurnFailedError, input builders
  handler.go              Handler: every callback field, with docs
  dispatch.go             read loop -> per-thread queues -> callbacks; turnWaiter
  methods.go              thin layer: one Go method per wire method
  options.go              WithXxx client options
  paginate.go             iter.Seq2 pagination helpers
  tool.go                 DynamicTool, ToolGroup, NewTool, registry
  protocol/*_gen.go       GENERATED. Never hand-edit.

internal/jsonrpc/         framing, correlation, child-process supervision
internal/cmd/schemagen/   the generator
internal/schemas/         vendored JSON Schema, both variants (~3.4 MB)
examples/                 basic, streaming, steer, tools
.claude/skills/           the version-upgrade skill, its evals and helper scripts
```

The `codex` → `codex/protocol` → `internal/*` dependency direction is one-way.
Keep it that way: `protocol` must import nothing from `codex`, or regeneration
starts fighting hand-written code.

## Design decisions and the reasoning behind them

Most of these were argued through before implementation. The reasoning matters
more than the outcome, because it tells you when the outcome should change.

**Types are generated, output committed.** ~950 types from vendored JSON Schema.
Committed so `go get` needs no codegen step and no `codex` binary. See ADR 0001.

**Both schema variants are vendored and diffed.** The schema has *no*
machine-readable marker for experimental members, and the prose hints are
actively misleading — `Thread.path` is marked `[UNSTABLE]` yet ships in stable.
So the generator diffs stable against experimental at property-path granularity.
See ADR 0004.

**Callbacks are the only delivery mechanism.** There was a `TurnStream` event-channel
API; it was deleted in `65458e7`. Two mechanisms delivering the same
notifications means two sets of ordering and backpressure rules to keep in
agreement. `Thread.Run` blocks while callbacks stream, which covers
print-as-you-go without a second surface.

**Handlers are client-level, NOT per-thread.** This was tried and reverted. The
server creates threads the caller never asks for: the schema has five sub-agent
source kinds (`subAgent`, `subAgentReview`, `subAgentCompact`,
`subAgentThreadSpawn`, `subAgentOther`), and `spawnAgent` produces thread ids
`StartThread` never returns. A thread-keyed handler map silently drops all of that
into `OnUnhandled`. **If you are tempted to add per-thread handlers, this is why
not.**

**One client drives one caller-owned thread.** `codex.Open` makes it structural;
`Client.StartThread` enforces it with `ErrMainThreadExists`. `Client.Thread(id)`
does *not* claim the slot, because sub-agent and review threads must stay
addressable.

**Notifications route per-thread but are never dropped.** One bounded ordered
queue per thread id; overflow blocks the read loop rather than dropping, because
losing a delta corrupts the message the caller reassembles. See ADR 0002.

**A missing approval declines, loudly.** Never silence. An unanswered approval
blocks the turn forever with no diagnostic — the worst failure this SDK can
produce. Same reasoning covers dynamic tools: an unhandled tool call answers
`success:false` rather than staying quiet. See ADR 0003.

**Forward compatibility over strictness.** Unknown methods, union variants, and
enum values all decode without error and round-trip unchanged. The SDK is expected
to run against servers newer than its schema.

## Traps

These cost real debugging time. Most are protocol quirks, not code smells.

**`item/*` is the source of truth for turn items, not the turn payload.** The
`turn/completed` payload can carry an empty `items` array. Reading items from it
returns nothing and `AgentMessage` comes back empty. The `turnWaiter` accumulates
from `item/completed` and falls back to the payload. This was a real bug, caught
by a test.

**`SandboxMode` and `SandboxPolicy` are the same concept, different spellings.**
`thread/start` takes kebab-case `read-only`; `turn/start` takes camelCase
`readOnly`, and `ThreadStartResponse` *returns* the camelCase union. Deriving one
from the other breaks the wire. Pinned by `TestSandboxModeWireValues`.

**Four enum casings coexist**: kebab (`on-request`), snake (`final_answer`),
SCREAMING_SNAKE (`AGENTS_MD`), camel. Enum constant values are copied verbatim
from the schema and never derived from the Go identifier. Same for struct tags,
which vary *within* a single type.

**Tri-state fields.** Seven fields are `Option<Option<T>>` upstream: absent leaves
the value unchanged, explicit null clears it. **The JSON Schema collapses this** —
only the TypeScript output preserves it as `| null | null`. `triStateFields` in
`schemagen/main.go` is therefore hand-maintained, and two entries were missing on
first write (fixed in `749749b`). Re-derive after any bump:

```bash
codex app-server generate-ts --out /tmp/codex-ts --experimental
grep -rl "null | null" /tmp/codex-ts
```

**`jsonschema` marks a field required unless the json tag has `omitempty`.** Not
what most people expect, and `jsonschema:"required"` is not what drives it. This
changes how the model calls your tool. Established by experiment, not assumption;
documented on `NewTool`, pinned by a test.

**`ReasoningEffort` is deliberately not an enum.** The schema calls it "a
non-empty reasoning effort value advertised by the model" — an open string. Valid
values come from `model/list`. Do not add constants.

**Three incompatible path types coexist**: `AbsolutePathBuf`,
`LegacyAppPathString`, `PathUri`. `ThreadItem.cwd` uses one,
`SandboxPolicy.writableRoots` another. Do not convert between them.

**`Run` may return just before `OnTurnCompleted` finishes.** Waiters are signalled
*before* user callbacks so a panicking callback cannot strand `Run`. Read `Run`'s
`TurnResult`; don't depend on the callback having run. A test asserts only the
ordering that actually holds.

**Steering needs a turn in flight.** `Run` blocks, so steer from another
goroutine, and wait until `CurrentTurnID() != ""`. The server matches
`expectedTurnId` against the active turn and rejects a mismatch.

**Don't opt out of `requiredNotifications`.** `turn/started`, `turn/completed`,
`thread/started`, `error` — `client.go` rejects suppressing these, because
`Thread.Run` consumes them and would hang.

## Maintenance

### Version bumps

**Invoke the `codex-schema-upgrade` skill.** It encodes the whole procedure and
the failure modes. Do not improvise: the generator's four hand-maintained tables
each fail loudly with a message naming the file to edit, and the skill explains
each one.

```bash
make sync-schemas    # refresh vendored schemas (explicit, reviewable diff)
make generate        # regenerate; fix whatever table it complains about
make test-race
make test-integration
make check-generated # CI gate: fails if generated output is stale or hand-edited
```

`python3 .claude/skills/codex-schema-upgrade/scripts/schema_diff.py` reports added
and removed methods, types, and fields between the committed and working-tree
schema, flagging breaking changes. Verified working on a synthetic upgrade.

### The five hand-maintained tables

Everything else is derived. These are not:

| Table | File | Why it can't be generated |
|---|---|---|
| `resultOverrides` | `schemagen/methods.go` | The schema has **no** method→result mapping at all |
| `triStateFields` | `schemagen/main.go` | JSON Schema collapses `Option<Option<T>>` |
| `rawTypes` | `schemagen/main.go` | Untagged unions can't be discriminated structurally |
| `skipTypes` | `schemagen/main.go` | Umbrella unions (217 types for `ServerNotification` alone) |
| `requiredNotifications` | `codex/client.go` | What `Thread.Run` consumes internally |

### Testing

Default `go test ./...` is hermetic — a scripted in-process fake server
(`codex/fakeserver_test.go`), no `codex` binary or account needed. Integration
tests are behind `-tags integration` and cover only what needs no auth: spawn,
handshake, model catalog, shutdown.

**Always run `-race`.** The dispatch layer is concurrent and the race detector has
caught real problems here.

## Known gaps

- **stdio transport only.** WebSocket and Unix socket unimplemented; both are
  marked experimental and unsupported upstream. The `Transport` interface makes
  them additive.
- **Windows process-tree teardown is best-effort.** No `SIGTERM`, so shutdown goes
  straight to kill and grandchildren may orphan. Needs a Job Object.
- **`McpElicitation*` stays `json.RawMessage`** — a four-level untagged union
  disambiguated only structurally.
- **No live coverage of multi-item turns.** Turn behaviour is tested against the
  fake server; the integration suite can't reach it without credentials.
- **The upgrade skill's eval set is worthless as evidence.** All six runs scored
  100%, with-skill and baseline alike. Details below.

## The one piece of unfinished work

`.claude/skills/codex-schema-upgrade/` is functional and its helper scripts are
verified, but **its eval set does not discriminate** and should be rebuilt before
anyone trusts a score from it. From
`/Users/eric/Downloads/codexadkv2-workspace/iteration-1/benchmark.md`:

- All 6 runs scored 100%. The set provides no evidence the skill helps.
- **Eval 2 is non-discriminating**: both configurations scored 6/6 and both
  spontaneously added a paginator nobody asked for. It measures model competence.
- **Eval 3's fixture is invalid.** It mutates one of the three copies each type
  has in a real schema dump, so a careful agent solves it by noticing the
  *forgery* rather than by reasoning about the invariant. The baseline also checked
  npm and found 0.152.0 doesn't exist. Rebuild it to mutate all three copies
  consistently.
- **Assertion 3f cannot fail**: the grader greps for `adr`/`0004`, and the
  generator's own error text contains the ADR path.

Two process lessons worth carrying, both my own errors:

1. **Pin the grading baseline to a commit.** I committed a fix mid-eval, so the
   grader diffed against a moved baseline and reported four false failures.
   `grade.py` now takes a baseline ref; pass it.
2. **Grade only after every run reports complete.** I graded early twice and
   reported false failures to the user, including claiming a baseline run missed
   breaking changes it had actually named six times.

One genuine win came out of it regardless: the with-skill run followed the skill's
instruction to re-check `triStateFields` and found two fields **genuinely missing
from the SDK** — a real pre-existing bug, fixed in `749749b`.

## Suggested next steps

Nothing is broken. In rough priority:

1. **Rebuild the eval set** (above). It's the only actively misleading artifact.
2. **Live coverage of a multi-item turn** — commands, file changes, approvals
   against a real server. The largest untested surface.
3. **Windows teardown** via a Job Object, if Windows matters.
4. **Typed wrappers for more methods** as needed. ~55 methods have generated types
   but no wrapper; `client.Call` reaches them meanwhile. Add on demand, not
   speculatively.

## Suggested skills for the next session

- **`codex-schema-upgrade`** (project) — for any version bump, regeneration, or
  wiring a new method/notification. It exists precisely so this isn't improvised.
- **`skill-creator`** — for rebuilding the eval set. The relevant guidance:
  assertions must fail for a *wrong* implementation, not merely pass for a right
  one, and the human should review outputs in the viewer before you form an
  opinion.
- **`tdd`** — for the multi-item-turn coverage. The fake server makes red-green
  natural here.
- **`domain-modeling`** — if new protocol vocabulary appears; keep `CONTEXT.md`
  current, especially anything that looks like a synonym but isn't.
- **`code-review`** — before merging generated-code changes. `make check-generated`
  catches staleness but not whether a generator change was *correct*.

## Working notes

- Go 1.27.0, macOS (darwin/arm64). Go 1.23+ required for range-over-func iterators.
- `codex` is at `/Users/eric/.local/bin/codex`, authenticated, so integration
  tests and examples actually run here.
- One dependency: `github.com/invopop/jsonschema` (+4 indirect), solely for
  reflecting tool argument schemas. The README no longer claims stdlib-only.
- The eval workspace at `/Users/eric/Downloads/codexadkv2-workspace/` is outside
  the repo and contains six full repo copies (~large). Safe to delete; it holds
  the iteration-1 outputs and benchmark referenced above.
- `1.md` in the repo root is a scratch file containing `123`. Not mine, left
  untracked deliberately.
- Nothing has been pushed. There is no remote configured.
