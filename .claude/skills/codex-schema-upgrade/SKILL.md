---
name: codex-schema-upgrade
description: Upgrade this repo's vendored Codex app-server protocol schemas to a newer codex-cli version, regenerate codex/protocol, repair the generator's hand-maintained tables when the new schema breaks them, and wire newly-added methods and notifications into the typed API. Use this whenever the user wants to bump/update/sync the codex or app-server protocol version, mentions internal/schemas or schemagen, asks to regenerate codex/protocol, says the generated types are stale or out of date, wants to expose a protocol method or notification that the SDK does not wrap yet, or hits a "method table is out of sync" / "no longer a strict superset" generator failure. Also use it when `make check-generated` fails in CI or a new codex-cli release needs adopting.
---

# Upgrading the Codex protocol schemas

This repo generates `codex/protocol` from JSON Schema vendored in
`internal/schemas/`. Upgrading means: refresh the vendored schema, regenerate,
repair whatever the new schema broke, then decide what newly-available protocol
surface deserves a typed wrapper.

The generator is deliberately loud. Most of the work in an upgrade is responding
to its failures, and each failure message names the file and table to edit. Read
`docs/adr/0001-generated-protocol-types.md` and
`docs/adr/0004-experimental-schema-diff.md` once before starting — they explain
why the pipeline is shaped this way, which makes the failures make sense.

## Before you start

Confirm the working tree is clean (`git status`). An upgrade produces a large
generated diff, and mixing it with unrelated edits makes it unreviewable. Also
record the version you're moving from and to:

```bash
cat internal/schemas/VERSION     # the version currently vendored
codex --version                  # the version installed
```

If they already match, there is nothing to sync — the user probably wants
`make generate` (regenerate from the existing schema) or the integration half of
this skill. Say so rather than pointlessly re-vendoring.

## Step 1: Sync the vendored schemas

```bash
make sync-schemas
```

This writes six files twice: once from `--experimental` and once without, with
the stable copies prefixed `stable-`. Both are needed because the schema carries
no machine-readable marker for experimental members, so the generator identifies
them by diffing the two. It also updates `internal/schemas/VERSION`.

Inspect what actually changed before regenerating — this is what tells you what
the upgrade contains:

```bash
git diff --stat internal/schemas/
scripts/diff-schema-versions.sh   # if present; otherwise see below
```

To enumerate new methods and new types, compare the old and new schema with the
helper:

```bash
python3 .claude/skills/codex-schema-upgrade/scripts/schema_diff.py
```

It prints added/removed methods per direction, added/removed definitions, and
per-type field additions. Read its output carefully: added notifications and
added methods are the candidates for Step 4, and **removed** anything is a
compatibility problem worth raising with the user before proceeding.

## Step 2: Regenerate

```bash
make generate
```

Success means `codex/protocol/*_gen.go` was rewritten and `go build ./...`
passed. If it fails, the message tells you which of four hand-maintained tables
needs attention. They exist because the schema cannot express what they encode.

### "method %q takes no params and has no entry in resultOverrides"

A new method takes no params, so its result type name cannot be derived from a
params type name. The schema describes method→params but has **no result mapping
at all**, so the table in `internal/cmd/schemagen/methods.go` is the only source.

Find the real response type name, then add the entry:

```bash
python3 -c "
import json
for f in ['codex_app_server_protocol.schemas.json','codex_app_server_protocol.v2.schemas.json']:
    d=json.load(open('internal/schemas/'+f))['definitions']
    print([k for k in d if 'YourGuess' in k])
"
```

Response type names diverge from method names more often than you'd expect
(`app/read` → `AppsReadResponse`, `account/read` → `GetAccountResponse`,
`mcpServerStatus/list` → `ListMcpServerStatusResponse`), so look the name up
rather than guessing it.

### "method table is out of sync with the schema"

Either a method the table references no longer exists (delete the entry), or a
response type it names is absent (fix the name). The message lists each problem
individually. This check runs in both directions on purpose: a stale entry is as
much a bug as a missing one, because it silently decodes into the wrong struct.

### "the experimental schema is no longer a strict superset"

The generator assumes `--experimental` only *adds* optional members, and verifies
it every run. This failure means that stopped being true: something was removed
or reshaped between the two variants.

Do not paper over this. Stop and report to the user what specifically changed,
because the annotation logic and possibly the "generate from experimental" choice
in ADR 0004 need revisiting. Ask before proceeding.

### Generated code does not parse

The generator writes `codex/protocol/<file>.broken` with the unformatted output
and names it in the error. Open that file at the reported line — it's almost
always a new schema shape the emitter mishandles (a new discriminant field name,
an unusual union arm, an identifier collision). Fix the emitter in
`internal/cmd/schemagen/`, not the generated output.

### Two more tables you may need

- `rawTypes` (`main.go`) — definitions modelled as `json.RawMessage` instead of
  generated, for genuinely untagged or recursive shapes. Add to it when a new
  type is an untagged union that cannot be discriminated structurally.
- `triStateFields` (`main.go`) — fields where absent and explicit-null differ.
  **The JSON Schema collapses these**, rendering `Option<Option<T>>` as a plain
  nullable, so they are invisible to the generator. Only the TypeScript output
  preserves the distinction. After any upgrade, re-check:

  ```bash
  codex app-server generate-ts --out /tmp/codex-ts --experimental >/dev/null 2>&1
  grep -rn "null | null" /tmp/codex-ts | sed 's/:.*\(\w*?: [a-zA-Z]* | null | null\).*/ => \1/'
  ```

  Every hit must appear in `triStateFields` as `TypeName.fieldName`. A missing
  entry means callers silently cannot clear that field.

## Step 3: Verify the regeneration

```bash
make test          # hermetic; needs neither the codex binary nor an account
go vet ./... && go vet -tags integration ./codex/
make test-integration   # talks to a real app-server; skips itself if codex is absent
```

The protocol tests in `codex/protocol/protocol_test.go` are the ones that catch
generation regressions. Pay attention if these fail:

- **Union round-trips** — a new schema shape broke a codec.
- **`TestSandboxModeWireValues`** — enum wire values must stay verbatim. The
  schema mixes kebab, snake, SCREAMING_SNAKE, and camel casing, and
  `SandboxMode` (`read-only`) and `SandboxPolicy` (`readOnly`) spell the same
  concept differently. If a wire value changed, that's an upstream breaking
  change worth surfacing, not a test to update.
- **`TestUnknownVariantPreserved` / `TestUnknownEnumValuePreserved`** — forward
  compatibility. These must keep passing; the SDK is expected to run against
  servers newer than its schema.

Integration tests are the real drift detector: they talk to the installed
server. `TestRealHandshake` failing after an upgrade means the response shape
genuinely changed.

## Step 4: Integrate new surface

Regenerating gives every new method and field a Go **type**. It does not give
them a typed **wrapper**. Decide what's worth wrapping, guided by what the SDK
already covers: threads, turns, items, notifications, approvals, review, models.
Anything else is reachable via `client.Call` and does not need a wrapper unless
the user asks.

Ask the user which new capabilities they want exposed rather than wrapping all
of them — 55 new methods appeared in a single past version, and most were
plugins/projects/remote-control surface outside this SDK's scope.

### Wiring a new request method

Add to `codex/methods.go`, following the existing shape exactly:

```go
// ThreadSearch searches stored threads. <one line on what the server does>
func (c *Client) ThreadSearch(ctx context.Context, params protocol.ThreadSearchParams) (*protocol.ThreadSearchResponse, error) {
	var out protocol.ThreadSearchResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadSearch, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
```

Use the generated `protocol.Method*` constant, never a string literal — that's
what keeps the wrapper and the schema in lockstep. If the method is experimental,
say so in the doc comment and point at `WithExperimentalAPI`.

### Wiring a new notification

Three edits, in this order:

1. **`codex/handler.go`** — add a callback field with a doc comment explaining
   when the server emits it and what the caller should do about it.
2. **`codex/dispatch.go`** — add a `case protocol.Notify<X>: dispatch(d, n, h.On<X>)`
   arm to `invoke`. Without this the notification only reaches `OnUnhandled`.
3. **`codex/client_test.go`** — add a test that pushes the notification through
   the fake server and asserts the callback fires with decoded fields.

If the new notification is thread-scoped, confirm its params carry `threadId`.
Per-thread routing depends on it (`threadKeyOf` in `dispatch.go`); a
notification without it lands in the connection-scoped queue, which is correct
for genuinely connection-wide events but wrong for thread events.

Do **not** add a new notification to `requiredNotifications` in `client.go`
unless `Thread.Run`/`RunStream` actually consume it. That list only exists to
reject opt-outs that would make those methods hang.

### If a new notification is part of turn lifecycle

Also add an `EventKind` and a `translate` arm in `codex/stream.go` so it reaches
`RunStream` consumers. Filter on `p.TurnID != s.turnID` like the existing arms —
a thread can have had other turns.

## Step 5: Commit

Two commits read better than one, because the generated diff is enormous and
reviewers want to skip it:

1. The schema sync plus regenerated output, with the version bump in the subject
   (`Upgrade vendored Codex schemas to 0.152.0`). Mention in the body what the
   diff contains: new methods, new notifications, added fields on existing
   types, and any generator table you had to edit and why.
2. The integration work, naming which capabilities you wrapped.

Before committing, confirm the CI gate passes:

```bash
make check-generated   # fails if generated output is stale or hand-edited
```

## What to report back

State plainly:

- The version moved from and to.
- New methods and notifications, and which you wrapped versus left to `Call`.
- Any generator table you edited, and why the schema forced it.
- Anything **removed** or reshaped upstream, since that is a potential breaking
  change for callers.
- Test results, including whether the integration suite actually ran or skipped
  for want of an authenticated `codex`.

If something is unverified — integration tests skipped, a new notification typed
but never exercised against a real server — say so rather than implying full
coverage.
