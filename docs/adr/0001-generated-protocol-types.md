# Protocol types are generated from vendored JSON Schema, with output committed

The Codex app-server protocol has ~656 types (155 in the thread/turn/notification core we
wrap by hand), and upstream regenerates them per `codex` release. We generate Go types from
JSON Schema bundles vendored into `internal/schemas/` and **commit the generated output**, so
consumers need no codegen step and no `codex` binary to build.

## Considered Options

- **Hand-write the structs.** Rejected: 155 types of transcription by hand guarantees drift,
  and a `codex` version bump becomes a week of work. It also cannot be verified against the
  schema.
- **Generate at build time from the installed `codex` binary.** Rejected: makes the build
  non-hermetic and version-dependent on whatever `codex` the developer happens to have.
- **`map[string]any` passthrough for the hot types.** Rejected explicitly; the requirement is
  complete struct definitions.

## Consequences

- The input is the **JSON Schema** bundles, not the generated TypeScript. Both describe the
  same protocol, but the JSON Schema is real JSON (the TS emits each union as one 14KB line)
  and it carries `required` and `format` metadata that the TS drops — which we need for
  per-field nullability. Two bundles are required and are complementary, not overlapping:
  `codex_app_server_protocol.schemas.json` (82 root/v1 definitions, including
  `InitializeResponse`) and `codex_app_server_protocol.v2.schemas.json` (579 definitions,
  including everything `Thread*`/`Turn*`/`ThreadItem`), plus the four method-map files.
- Refreshing the schema is a separate, explicit, reviewable step (`make sync-schemas`), so a
  protocol change always appears as a diff rather than silently altering the build.
- CI runs `make generate && git diff --exit-code` — a hand-edit to a generated file fails the
  build.
- The **method-to-result mapping must be hand-maintained**: the schema describes
  method-to-params but contains no result mapping at all. The `XParams` → `XResponse`
  convention covers 91 of 95 methods; the rest diverge (`app/read` → `AppsReadParams`,
  `account/read` → `GetAccountParams`) and 8 responses have no params sibling. This table is
  the one hand-written artifact in the pipeline, and the generator fails the build if it goes
  stale in either direction.
- Enum constant *values* are copied verbatim from the schema and never derived from the Go
  identifier. Four casing conventions coexist across 32 enum types — kebab (`on-request`,
  `danger-full-access`), snake (`final_answer`), screaming-snake (`AGENTS_MD`), and camel —
  so any convention-based derivation produces wrong wire values. The same applies to struct
  tags, which vary *within* single types.
