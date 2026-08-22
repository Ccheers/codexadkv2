# Notifications dispatch through bounded per-Thread ordered queues

The transport has a single read loop, and Deltas are meaningless out of order. We decode on
the read loop, then hand each Notification to a bounded queue owned by its Thread, each
drained by its own goroutine. Per-Thread ordering is preserved; no Thread can stall another
or the request/response path.

## Considered Options

- **Invoke callbacks synchronously on the read loop.** Guarantees strict global ordering and
  is the simplest possible design, but any blocking callback stalls the entire connection —
  including responses to in-flight Requests. A callback that calls back into the client and
  waits deadlocks the whole SDK. Rejected: it converts an ordinary user mistake into a hang.
- **One goroutine per Notification.** Never stalls, but destroys ordering, which corrupts any
  message reconstructed from `item/agentMessage/delta`. Rejected outright.

## Consequences

- Routing is possible without the SDK tracking state, because every item- and turn-scoped
  Notification carries **both `threadId` and `turnId` as required fields**. The two
  exceptions are `thread/started` (id nested inside `thread`) and `WarningNotification`
  (`threadId` optional — a warning can be connection-scoped); connection-scoped Notifications
  are dispatched at client level only.
- On queue overflow we **block rather than drop**. Silently discarding Deltas corrupts the
  message the caller reconstructs, which is worse than backpressure. The bound is tunable.
- Callbacks must not block indefinitely. This is a documented contract, not an enforced one.
- `Thread.RunStream` is a thin adapter over the same queue, not a second delivery mechanism.
- This is what makes an event-driven `Thread.Run` trustworthy. The reference implementation we
  studied did not trust its own notification handling and fell back to polling `thread/read`
  every 250ms for up to 15 minutes, plus scraping Codex's private `.jsonl` session rollout
  file off disk to recover the final agent message. Both are avoidable, and both are avoided
  by making ordered delivery reliable here.
