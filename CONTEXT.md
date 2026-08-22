# Codex App Server Go SDK

A Go client library for the Codex app-server JSON-RPC 2.0 protocol. It speaks the protocol
over a locally spawned `codex app-server` process and surfaces threads, turns, streamed
items, and approval prompts to Go programs.

## Language

### Protocol shapes

**Request**:
A client-to-server call carrying `method`, `params`, and an `id`; the server answers with a
matching `result` or `error`.

**Notification**:
A one-way server-to-client message carrying only `method` and `params`. Never answered.
_Avoid_: Event (the docs use "event notifications", but "notification" is the wire term)

**Server-Request**:
A server-to-client call carrying an `id` that the client **must** answer, blocking the turn
until it does. Approvals and elicitations are Server-Requests, not Notifications — the
distinction is load-bearing, because ignoring a Notification is free and ignoring a
Server-Request hangs the turn.
_Avoid_: Reverse request, callback request

**Method**:
The string key identifying a Request, Notification, or Server-Request on the wire
(`thread/start`, `item/agentMessage/delta`). Dispatch is keyed on Method strings, never on a
generated umbrella union.

### Domain primitives

**Thread**:
A conversation between a user and the Codex agent. Contains Turns. Identified by a Thread id
that is also its `sessionId` unless it was forked.

**Turn**:
A single user request and all the agent work that follows it. Contains Items. Ends as
`completed`, `interrupted`, or `failed`.

**Item**:
One unit of input or output within a Turn — a user message, an agent message, a command
execution, a file change, a tool call. Twenty variants share one tagged union.
_Avoid_: Message, entry, event

**Delta**:
An incremental fragment of an Item's content, streamed before the Item completes. Deltas are
only meaningful in arrival order.

**Goal**:
Persisted per-Thread objective with a token budget and usage accounting, independent of any
Turn.

### Wire-shape distinctions that look like duplicates but are not

**Sandbox Mode**:
The kebab-case string enum (`read-only`, `workspace-write`, `danger-full-access`) accepted by
`thread/start`, `thread/resume`, and `thread/fork`.

**Sandbox Policy**:
The camelCase tagged union (`readOnly`, `workspaceWrite`, `dangerFullAccess`,
`externalSandbox`) accepted by `turn/start` and *returned* by `thread/start`. Same concept as
Sandbox Mode, different spelling and different shape; the two are not interchangeable.

**Absolute Path**:
A filesystem path in the server's native absolute syntax. One of three mutually
incompatible path representations in the protocol; used for sandbox writable roots.

**Legacy App Path**:
A second, legacy path representation. Used for an Item's working directory. Not convertible
to an Absolute Path without an explicit path convention.

**Path URI**:
A third path representation, carried as a canonical `file:` URI string. Distinct from both
of the above.

### Compatibility posture

**Unknown Variant**:
A tagged-union `type` value, enum value, or Method string that this build of the SDK does not
recognize, because it is talking to a newer server. Always decoded and preserved, never an
error.

**Schema Version**:
The `codex` CLI version the committed protocol types were generated from. Recorded and
exposed, never enforced against the running server.
