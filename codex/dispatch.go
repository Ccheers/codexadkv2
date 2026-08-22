package codex

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/ccheers/codexadkv2/codex/protocol"
	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// dispatcher routes server traffic from the connection's single read loop onto
// per-thread ordered queues.
//
// See docs/adr/0002-per-thread-ordered-dispatch.md. The short version: running
// callbacks inline on the read loop lets one blocking callback deadlock the
// whole connection, and running each notification in its own goroutine destroys
// the ordering that deltas depend on. One ordered queue per thread preserves
// ordering where it matters while isolating threads from each other.
type dispatcher struct {
	handler *Handler
	logger  *slog.Logger
	bufSize int

	// conn is set immediately after construction, before Serve starts.
	conn *jsonrpc.Conn

	mu     sync.Mutex
	queues map[string]*threadQueue
	closed bool

	// observers receive notifications for one thread in addition to the
	// client-level handler. Thread.Run and RunStream register these.
	observers map[string][]*observer

	wg sync.WaitGroup
}

// threadQueue is one thread's ordered delivery channel plus its draining
// goroutine.
type threadQueue struct {
	ch   chan *notification
	done chan struct{}
}

type notification struct {
	method string
	params json.RawMessage
}

func newDispatcher(h *Handler, logger *slog.Logger, buf int) *dispatcher {
	return &dispatcher{
		handler:   h,
		logger:    logger,
		bufSize:   buf,
		queues:    make(map[string]*threadQueue),
		observers: make(map[string][]*observer),
	}
}

// connectionScope is the queue key for notifications that carry no thread id,
// such as a connection-scoped warning. They are ordered among themselves but
// independent of any thread.
const connectionScope = "\x00connection"

// HandleNotification implements jsonrpc.Handler. It runs on the read loop and
// must not do real work: it extracts the routing key and enqueues.
func (d *dispatcher) HandleNotification(method string, params json.RawMessage, _ *int64) {
	key := threadKeyOf(params)

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	q, ok := d.queues[key]
	if !ok {
		q = &threadQueue{
			ch:   make(chan *notification, d.bufSize),
			done: make(chan struct{}),
		}
		d.queues[key] = q
		d.wg.Add(1)
		go d.drain(q)
	}
	d.mu.Unlock()

	// A send on a full channel blocks the read loop. That is deliberate:
	// dropping a delta would corrupt the message the caller reassembles, so
	// backpressure is the lesser evil. See WithNotificationBuffer.
	select {
	case q.ch <- &notification{method: method, params: params}:
	case <-q.done:
	}
}

// threadKeyOf extracts the thread id a notification belongs to.
//
// Every item- and turn-scoped notification carries threadId as a required
// field, which is what makes stateless routing possible. Two shapes need
// special handling: thread/started nests the id inside "thread", and a warning's
// threadId is optional because it can be connection-scoped.
func threadKeyOf(params json.RawMessage) string {
	if len(params) == 0 {
		return connectionScope
	}
	var probe struct {
		ThreadID *string `json:"threadId"`
		Thread   *struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(params, &probe); err != nil {
		return connectionScope
	}
	if probe.ThreadID != nil && *probe.ThreadID != "" {
		return *probe.ThreadID
	}
	if probe.Thread != nil && probe.Thread.ID != "" {
		return probe.Thread.ID
	}
	return connectionScope
}

// drain delivers one thread's notifications in order.
func (d *dispatcher) drain(q *threadQueue) {
	defer d.wg.Done()
	for {
		select {
		case n := <-q.ch:
			d.deliver(n)
		case <-q.done:
			// Flush whatever is already queued so Close does not truncate a
			// turn's tail, then stop.
			for {
				select {
				case n := <-q.ch:
					d.deliver(n)
				default:
					return
				}
			}
		}
	}
}

// deliver invokes the observers and then the client-level handler. A panic in a
// user callback is recovered and logged: it must not take down the connection or
// silently stop delivery for that thread.
func (d *dispatcher) deliver(n *notification) {
	defer func() {
		if r := recover(); r != nil {
			d.logger.Error("codex: notification handler panicked",
				"method", n.method, "panic", r)
		}
	}()

	// Thread-scoped observers run first, then the client-level handler always
	// runs. Delivery is additive: an observer never suppresses the handler.
	key := threadKeyOf(n.params)
	d.mu.Lock()
	obs := append([]*observer(nil), d.observers[key]...)
	d.mu.Unlock()
	for _, o := range obs {
		o.notify(n)
	}

	d.invoke(n)
}

// invoke calls the specific callback for a method, falling back to OnUnhandled.
func (d *dispatcher) invoke(n *notification) {
	h := d.handler

	switch n.method {
	case protocol.NotifyThreadStarted:
		dispatch(d, n, h.OnThreadStarted)
	case protocol.NotifyThreadStatusChanged:
		dispatch(d, n, h.OnThreadStatusChanged)
	case protocol.NotifyThreadClosed:
		dispatch(d, n, h.OnThreadClosed)
	case protocol.NotifyThreadArchived:
		dispatch(d, n, h.OnThreadArchived)
	case protocol.NotifyThreadUnarchived:
		dispatch(d, n, h.OnThreadUnarchived)
	case protocol.NotifyThreadDeleted:
		dispatch(d, n, h.OnThreadDeleted)
	case protocol.NotifyThreadNameUpdated:
		dispatch(d, n, h.OnThreadNameUpdated)
	case protocol.NotifyThreadGoalUpdated:
		dispatch(d, n, h.OnThreadGoalUpdated)
	case protocol.NotifyThreadGoalCleared:
		dispatch(d, n, h.OnThreadGoalCleared)
	case protocol.NotifyThreadTokenUsageUpdated:
		dispatch(d, n, h.OnTokenUsage)

	case protocol.NotifyTurnStarted:
		dispatch(d, n, h.OnTurnStarted)
	case protocol.NotifyTurnCompleted:
		dispatch(d, n, h.OnTurnCompleted)
	case protocol.NotifyTurnDiffUpdated:
		dispatch(d, n, h.OnTurnDiff)
	case protocol.NotifyTurnPlanUpdated:
		dispatch(d, n, h.OnTurnPlan)

	case protocol.NotifyItemStarted:
		dispatch(d, n, h.OnItemStarted)
	case protocol.NotifyItemCompleted:
		dispatch(d, n, h.OnItemCompleted)
	case protocol.NotifyItemAgentMessageDelta:
		dispatch(d, n, h.OnAgentMessageDelta)
	case protocol.NotifyItemPlanDelta:
		dispatch(d, n, h.OnPlanDelta)
	case protocol.NotifyItemReasoningSummaryTextDelta:
		dispatch(d, n, h.OnReasoningSummaryDelta)
	case protocol.NotifyItemReasoningSummaryPartAdded:
		dispatch(d, n, h.OnReasoningSummaryPartAdded)
	case protocol.NotifyItemReasoningTextDelta:
		dispatch(d, n, h.OnReasoningTextDelta)
	case protocol.NotifyItemCommandExecutionOutputDelta:
		dispatch(d, n, h.OnCommandOutputDelta)
	case protocol.NotifyThreadCompacted:
		dispatch(d, n, h.OnContextCompaction)

	case protocol.NotifyError:
		dispatch(d, n, h.OnError)
	case protocol.NotifyWarning:
		dispatch(d, n, h.OnWarning)
	case protocol.NotifyConfigWarning:
		dispatch(d, n, h.OnConfigWarning)
	case protocol.NotifyServerRequestResolved:
		dispatch(d, n, h.OnServerRequestResolved)

	default:
		d.unhandled(n)
	}
}

// dispatch decodes a notification into T and calls cb, falling back to
// OnUnhandled when cb is nil so that nothing is silently discarded.
func dispatch[T any](d *dispatcher, n *notification, cb func(*T)) {
	if cb == nil {
		d.unhandled(n)
		return
	}
	var payload T
	if err := json.Unmarshal(n.params, &payload); err != nil {
		// A field this build cannot decode should not lose the whole
		// notification, so report it and hand over the raw JSON.
		d.logger.Warn("codex: could not decode notification; delivering it as raw JSON",
			"method", n.method, "error", err)
		d.unhandled(n)
		return
	}
	cb(&payload)
}

func (d *dispatcher) unhandled(n *notification) {
	if d.handler.OnUnhandled != nil {
		d.handler.OnUnhandled(n.method, n.params)
	}
}

// HandleRequest implements jsonrpc.Handler for server-initiated requests.
//
// Each is answered on its own goroutine, because an approval callback typically
// waits for a human and must not block the read loop or other approvals.
func (d *dispatcher) HandleRequest(
	id jsonrpc.ID,
	method string,
	params json.RawMessage,
	respond func(any, *jsonrpc.Error),
) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				d.logger.Error("codex: approval handler panicked; declining",
					"method", method, "panic", r)
				respond(nil, &jsonrpc.Error{
					Code:    jsonrpc.CodeInternalError,
					Message: fmt.Sprintf("client handler panicked: %v", r),
				})
			}
		}()
		d.answer(method, params, respond)
	}()
}

// answer routes one server request to its callback and always responds.
//
// The default when a callback is missing is to DECLINE, never to stay silent:
// an unanswered approval blocks the turn forever with no diagnostic, which is
// the worst failure mode this SDK can produce. See
// docs/adr/0003-unanswered-approvals-decline.md.
func (d *dispatcher) answer(method string, params json.RawMessage, respond func(any, *jsonrpc.Error)) {
	h := d.handler

	switch method {
	case protocol.ServerMethodItemCommandExecutionRequestApproval:
		var p protocol.CommandExecutionRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			d.logDecodeFailure(method, err)
			respond(declineCommand(), nil)
			return
		}
		if h.OnCommandApproval == nil {
			d.warnNoHandler(method, "OnCommandApproval")
			respond(declineCommand(), nil)
			return
		}
		decision, err := h.OnCommandApproval(&p)
		if err != nil {
			d.logger.Error("codex: command approval handler failed; declining", "error", err)
			respond(declineCommand(), nil)
			return
		}
		respond(&protocol.CommandExecutionRequestApprovalResponse{Decision: &decision}, nil)

	case protocol.ServerMethodItemFileChangeRequestApproval:
		var p protocol.FileChangeRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			d.logDecodeFailure(method, err)
			respond(declineFileChange(), nil)
			return
		}
		if h.OnFileChangeApproval == nil {
			d.warnNoHandler(method, "OnFileChangeApproval")
			respond(declineFileChange(), nil)
			return
		}
		decision, err := h.OnFileChangeApproval(&p)
		if err != nil {
			d.logger.Error("codex: file change approval handler failed; declining", "error", err)
			respond(declineFileChange(), nil)
			return
		}
		respond(&protocol.FileChangeRequestApprovalResponse{Decision: &decision}, nil)

	case protocol.ServerMethodItemToolRequestUserInput:
		var p protocol.ToolRequestUserInputParams
		if err := json.Unmarshal(params, &p); err != nil {
			d.logDecodeFailure(method, err)
			respond(nil, declined("client could not decode the request: "+err.Error()))
			return
		}
		if h.OnUserInputRequest == nil {
			d.warnNoHandler(method, "OnUserInputRequest")
			respond(nil, declined("no user input handler is registered"))
			return
		}
		out, err := h.OnUserInputRequest(&p)
		if err != nil {
			respond(nil, declined(err.Error()))
			return
		}
		respond(out, nil)

	case protocol.ServerMethodItemPermissionsRequestApproval:
		var p protocol.PermissionsRequestApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			d.logDecodeFailure(method, err)
			respond(&protocol.PermissionsRequestApprovalResponse{}, nil)
			return
		}
		if h.OnPermissionsApproval == nil {
			d.warnNoHandler(method, "OnPermissionsApproval")
			// An empty grant is the safe answer: it denies everything while
			// still unblocking the turn.
			respond(&protocol.PermissionsRequestApprovalResponse{}, nil)
			return
		}
		out, err := h.OnPermissionsApproval(&p)
		if err != nil || out == nil {
			if err != nil {
				d.logger.Error("codex: permissions handler failed; granting nothing", "error", err)
			}
			respond(&protocol.PermissionsRequestApprovalResponse{}, nil)
			return
		}
		respond(out, nil)

	case protocol.ServerMethodMcpServerElicitationRequest:
		var p protocol.MCPServerElicitationRequestParams
		if err := json.Unmarshal(params, &p); err != nil {
			d.logDecodeFailure(method, err)
			respond(nil, declined("client could not decode the request: "+err.Error()))
			return
		}
		if h.OnElicitation == nil {
			d.warnNoHandler(method, "OnElicitation")
			respond(nil, declined("no elicitation handler is registered"))
			return
		}
		out, err := h.OnElicitation(&p)
		if err != nil || out == nil {
			respond(nil, declined("elicitation declined by the client"))
			return
		}
		respond(out, nil)

	default:
		if h.OnUnhandledRequest == nil {
			d.logger.Warn("codex: rejecting an unhandled server request; "+
				"set Handler.OnUnhandledRequest to answer it", "method", method)
			respond(nil, &jsonrpc.Error{
				Code:    jsonrpc.CodeMethodNotFound,
				Message: fmt.Sprintf("this client does not handle %q", method),
			})
			return
		}
		out, err := h.OnUnhandledRequest(method, params)
		if err != nil {
			respond(nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: err.Error()})
			return
		}
		respond(out, nil)
	}
}

func declineCommand() *protocol.CommandExecutionRequestApprovalResponse {
	d := protocol.NewCommandExecutionApprovalDecisionDecline()
	return &protocol.CommandExecutionRequestApprovalResponse{Decision: &d}
}

func declineFileChange() *protocol.FileChangeRequestApprovalResponse {
	d := protocol.NewFileChangeApprovalDecisionDecline()
	return &protocol.FileChangeRequestApprovalResponse{Decision: &d}
}

func (d *dispatcher) warnNoHandler(method, field string) {
	d.logger.Warn("codex: DECLINING an approval request because no handler is registered. "+
		"The turn continues, but the agent was denied. Register codex.Handler."+field+
		" to decide, or start the thread with an approval policy of \"never\" plus a "+
		"sandbox so approvals never fire.",
		"method", method)
}

func (d *dispatcher) logDecodeFailure(method string, err error) {
	d.logger.Error("codex: could not decode an approval request; declining",
		"method", method, "error", err)
}

func declined(reason string) *jsonrpc.Error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: reason}
}

// addObserver registers a per-thread observer, used by Thread.Run and RunStream
// to await turn completion without competing with the client-level handler.
func (d *dispatcher) addObserver(threadID string, o *observer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observers[threadID] = append(d.observers[threadID], o)
}

func (d *dispatcher) removeObserver(threadID string, target *observer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	list := d.observers[threadID]
	for i, o := range list {
		if o == target {
			d.observers[threadID] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(d.observers[threadID]) == 0 {
		delete(d.observers, threadID)
	}
}

// shutdown stops every queue and waits for in-flight callbacks to finish, so no
// callback runs after Client.Close returns.
func (d *dispatcher) shutdown() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	queues := make([]*threadQueue, 0, len(d.queues))
	for _, q := range d.queues {
		queues = append(queues, q)
	}
	observers := d.observers
	d.observers = make(map[string][]*observer)
	d.mu.Unlock()

	for _, q := range queues {
		close(q.done)
	}
	d.wg.Wait()

	// Unblock anything waiting on a turn that will now never complete.
	for _, list := range observers {
		for _, o := range list {
			o.close()
		}
	}
}
