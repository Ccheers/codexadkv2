package codex

import "github.com/ccheers/codexadkv2/codex/protocol"

// Handler holds the callbacks invoked for server-initiated traffic. Every field
// is optional: a nil callback means that notification is ignored.
//
// A struct of function fields is used rather than an interface so that new
// notifications can be added without breaking implementors, and so that godoc
// lists every available event in one place.
//
// # Why callbacks are the only delivery mechanism
//
// Callbacks are client-level rather than registered per thread, and they are the
// single path for everything the server streams. Both choices are deliberate.
//
// Client-level, because the server creates threads the caller never asked for:
// sub-agents, reviews, and compaction all run on their own thread ids, which are
// never returned by StartThread. A handler registered per thread would silently
// drop all of that traffic. Every notification carries its threadId, so a callback
// can tell whose work it is: compare against Client.MainThread().
//
// Single path, because an event-channel API alongside callbacks would mean two
// mechanisms delivering the same notifications, with two sets of ordering and
// backpressure rules to keep in agreement. Thread.Run blocks while callbacks
// stream, which covers the print-as-you-go case without a second API.
//
// # Ordering and concurrency
//
// Callbacks for a given thread are invoked on that thread's own goroutine, in
// the order the server sent them. Deltas therefore always arrive in order.
// Different threads progress independently, so a slow callback for one thread
// does not delay another.
//
// Thread.Run may return just before OnTurnCompleted finishes, since the internal
// completion signal deliberately fires ahead of user callbacks. Read Run's
// TurnResult rather than depending on the callback having completed.
//
// Callbacks must not block indefinitely. Each thread's queue is bounded, and
// once it fills the connection stops reading, which delays responses to
// in-flight requests. It is safe to call back into the Client from a callback,
// including a different thread's methods.
//
// # Approvals
//
// The approval callbacks return a decision. They are requests, not
// notifications: the turn is blocked until one returns. If a required approval
// callback is nil, the SDK answers "decline" and logs a warning rather than
// hanging the turn.
type Handler struct {
	// --- Thread lifecycle ---

	// OnThreadStarted fires when a thread is created, resumed, or forked.
	OnThreadStarted func(*protocol.ThreadStartedNotification)

	// OnThreadStatusChanged fires when a loaded thread's runtime status changes,
	// for example when it starts waiting on an approval.
	OnThreadStatusChanged func(*protocol.ThreadStatusChangedNotification)

	// OnThreadClosed fires when the server unloads a thread after its last
	// subscriber went away and the inactivity grace period expired.
	OnThreadClosed func(*protocol.ThreadClosedNotification)

	OnThreadArchived    func(*protocol.ThreadArchivedNotification)
	OnThreadUnarchived  func(*protocol.ThreadUnarchivedNotification)
	OnThreadDeleted     func(*protocol.ThreadDeletedNotification)
	OnThreadNameUpdated func(*protocol.ThreadNameUpdatedNotification)

	OnThreadGoalUpdated func(*protocol.ThreadGoalUpdatedNotification)
	OnThreadGoalCleared func(*protocol.ThreadGoalClearedNotification)

	// OnTokenUsage reports cumulative token usage for a thread.
	OnTokenUsage func(*protocol.ThreadTokenUsageUpdatedNotification)

	// --- Turn lifecycle ---

	// OnTurnStarted fires when a turn begins.
	OnTurnStarted func(*protocol.TurnStartedNotification)

	// OnTurnCompleted fires when a turn ends, whether it completed, was
	// interrupted, or failed. Inspect the turn's status to tell which.
	OnTurnCompleted func(*protocol.TurnCompletedNotification)

	// OnTurnDiff reports the aggregated unified diff across every file change in
	// the turn so far.
	OnTurnDiff func(*protocol.TurnDiffUpdatedNotification)

	// OnTurnPlan fires whenever the agent shares or revises its plan.
	OnTurnPlan func(*protocol.TurnPlanUpdatedNotification)

	// --- Items ---

	// OnItemStarted fires when a new unit of work begins. The item's id matches
	// the itemId carried by subsequent deltas.
	OnItemStarted func(*protocol.ItemStartedNotification)

	// OnItemCompleted fires when an item finishes. Treat this as the
	// authoritative state of the item; deltas are advisory.
	OnItemCompleted func(*protocol.ItemCompletedNotification)

	// OnAgentMessageDelta streams the agent's reply as it is generated. Append
	// deltas in the order received.
	OnAgentMessageDelta func(*protocol.AgentMessageDeltaNotification)

	// OnPlanDelta streams proposed plan text. The final plan item may not equal
	// the concatenation of its deltas.
	OnPlanDelta func(*protocol.PlanDeltaNotification)

	// OnReasoningSummaryDelta streams readable reasoning summaries.
	// SummaryIndex increments when a new summary section opens.
	OnReasoningSummaryDelta func(*protocol.ReasoningSummaryTextDeltaNotification)

	// OnReasoningSummaryPartAdded marks a boundary between summary sections.
	OnReasoningSummaryPartAdded func(*protocol.ReasoningSummaryPartAddedNotification)

	// OnReasoningTextDelta streams raw reasoning text, for models that expose it.
	OnReasoningTextDelta func(*protocol.ReasoningTextDeltaNotification)

	// OnCommandOutputDelta streams stdout and stderr from a command execution
	// item. Append deltas in order.
	OnCommandOutputDelta func(*protocol.CommandExecutionOutputDeltaNotification)

	// OnContextCompaction fires when the server compacts conversation history.
	OnContextCompaction func(*protocol.ContextCompactedNotification)

	// --- Diagnostics ---

	// OnError reports a turn failure. The turn subsequently completes with a
	// failed status. WillRetry indicates the server intends to retry.
	OnError func(*protocol.ErrorNotification)

	// OnWarning reports a non-fatal runtime warning. ThreadID may be nil, since
	// a warning can be connection-scoped rather than thread-scoped.
	OnWarning func(*protocol.WarningNotification)

	// OnConfigWarning reports a recoverable configuration problem.
	OnConfigWarning func(*protocol.ConfigWarningNotification)

	// OnServerRequestResolved confirms a pending server request was answered or
	// cleared, including when a turn ending cleared it before the client replied.
	OnServerRequestResolved func(*protocol.ServerRequestResolvedNotification)

	// OnUnhandled receives any notification with no specific callback set,
	// including methods this build of the SDK does not know.
	//
	// It is the escape hatch that guarantees nothing is silently lost when
	// talking to a newer server. params is the raw JSON of the notification.
	OnUnhandled func(method string, params []byte)

	// --- Approvals: these are requests and must return a decision ---

	// OnCommandApproval is consulted before the agent runs a command that
	// requires approval.
	//
	// If nil, the SDK answers Decline and logs a warning. Returning an error
	// also declines, and logs the error.
	OnCommandApproval func(*protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error)

	// OnFileChangeApproval is consulted before the agent applies a file change
	// that requires approval. If nil, the SDK answers Decline.
	OnFileChangeApproval func(*protocol.FileChangeRequestApprovalParams) (protocol.FileChangeApprovalDecision, error)

	// OnUserInputRequest is consulted when a tool asks the user 1-3 short
	// questions. If nil, the SDK declines.
	OnUserInputRequest func(*protocol.ToolRequestUserInputParams) (*protocol.ToolRequestUserInputResponse, error)

	// OnPermissionsApproval is consulted when the built-in request_permissions
	// tool asks for network or filesystem access. Grant only the subset you
	// intend to allow; permissions that were not requested are ignored.
	//
	// If nil, the SDK grants nothing.
	OnPermissionsApproval func(*protocol.PermissionsRequestApprovalParams) (*protocol.PermissionsRequestApprovalResponse, error)

	// OnElicitation is consulted when an MCP server asks for structured form
	// input or confirmation of a URL flow. If nil, the SDK declines.
	OnElicitation func(*protocol.MCPServerElicitationRequestParams) (*protocol.MCPServerElicitationRequestResponse, error)

	// OnUnhandledRequest receives any server-initiated request with no specific
	// callback.
	//
	// Unlike OnUnhandled, answering matters: a server request that goes
	// unanswered blocks the turn. Return a result to answer it, or an error to
	// reject it. If this is nil, the SDK rejects unknown requests with a
	// method-not-found error, which is safer than silence.
	OnUnhandledRequest func(method string, params []byte) (any, error)
}
