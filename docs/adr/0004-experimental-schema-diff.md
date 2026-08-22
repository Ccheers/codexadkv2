# Experimental members are identified by diffing two vendored schemas

`codex app-server generate-json-schema` accepts `--experimental`, which emits a
larger schema including gated methods and fields. We vendor **both** outputs and
compute the difference at generation time to annotate experimental members,
rather than relying on any marker in the schema itself.

## Why the diff is necessary

The schema has no machine-readable marker for experimental members. There is no
`@experimental` JSDoc tag, no `x-*` JSON Schema extension, no separate manifest,
and no naming convention — `thread/queue/add` and `ProjectCreateParams` look
exactly like stable names.

The prose hints are worse than useless because they are not gate-correlated in
either direction. Of 115 experimental-only definitions, only 15 mention
"EXPERIMENTAL"; meanwhile 41 files in the *stable* output contain the word, and
`Thread.path` is marked `[UNSTABLE]` yet ships in stable. A generator that
classified by description text would be wrong in both directions.

## Why generate from the experimental schema

Verified for codex-cli 0.149.0 across all 661 shared definitions: experimental is
a **strict superset**. Nothing is removed, no property changes shape, no field
flips between required and optional, no enum loses a value, and every added field
is optional. So types generated from the experimental schema stay wire-compatible
with a stable server — the extra fields simply go unsent.

Generating from the stable schema instead would make `WithExperimentalAPI()`
useless: the capability would be negotiated but the fields to use it with would
not exist. The gated members matter, and they land on the types most central to
this SDK — `ThreadStartParams` gains 11 fields, `TurnStartParams` 7, `Thread` 3,
and there are 55 additional client methods.

The superset property is **verified, not assumed**. It is not a documented
guarantee, so the generator re-checks it on every run and fails the build if a
future version removes or reshapes anything.

## Consequences

- The diff is computed at **property-path** granularity, not type granularity.
  Eleven support types (`ThreadExtra`, `MultiAgentMode`, `TurnsPage`, and others)
  already exist in the stable schema as orphans with no inbound references: only
  the field referencing them is gated. Classifying by type presence would mark
  those fields stable.
- `internal/schemas/` holds two copies of each bundle, roughly doubling its size.
  That is worth it to avoid a hand-maintained list of gated members, which would
  drift on every codex release.
- `WithExperimentalAPI()` remains **off by default**. Complete types and a
  conservative default are independent choices; this ADR is only about the former.
