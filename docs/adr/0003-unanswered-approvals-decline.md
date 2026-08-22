# An unanswered approval declines instead of hanging

Approvals arrive as Server-Requests, which block the Turn until the client answers. If no
approval handler is registered, the SDK auto-answers `decline` and logs loudly. It never
leaves the Request unanswered.

## Considered Options

- **Leave it unanswered.** The literal protocol behaviour, and what happens if you forget to
  wire a handler. Rejected: it hangs the Turn forever with no diagnostic, which is the single
  worst failure mode available in this SDK — the symptom (nothing happens) points nowhere near
  the cause (an unregistered callback).
- **Auto-accept.** Rejected: silently grants a Turn unreviewed shell and filesystem access.
  A library must not make that choice on the caller's behalf.
- **Fail the Turn with an error.** Closer, but `decline` is the protocol's own vocabulary for
  "no", and it lets the agent react and continue rather than aborting.

## Consequences

- "Defaults are usable" for unattended operation does **not** mean auto-approving. It means
  pairing `approvalPolicy: "never"` with a Sandbox Mode at `thread/start` so approvals never
  fire at all. This is the documented path for automation.
- The loud log on an auto-decline is deliberate. A silent decline would be nearly as hard to
  diagnose as a hang.
