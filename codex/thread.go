package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// Thread is a handle to one conversation.
//
// It is the ergonomic layer: Run starts a turn and waits for it to finish,
// RunStream gives you an ordered event channel. Both are built only from the
// thin-layer methods on Client, so dropping down to those is always possible.
//
// A Thread is safe for concurrent use, but a thread can only have one turn in
// flight at a time: the server rejects a second TurnStart while one is active.
// Use Steer to add to a running turn.
type Thread struct {
	client *Client

	id      string
	started *protocol.ThreadStartResponse

	// currentTurn is the turn Run is currently blocked on, so a caller on another
	// goroutine can steer or interrupt it without having to capture the id from an
	// OnTurnStarted callback.
	turnMu      sync.Mutex
	currentTurn string
}

// ID returns the thread id.
func (t *Thread) ID() string { return t.id }

// Info returns the response that created or resumed this thread, including the
// resolved model, cwd, sandbox policy, and instruction sources.
func (t *Thread) Info() *protocol.ThreadStartResponse { return t.started }

// StartThread creates the client's main thread and returns a handle to it.
//
// One client drives one main thread. The server may still create additional
// threads on its own for sub-agents, reviews, and compaction, and their
// notifications arrive through the same Handler carrying their own threadId; the
// invariant is about what the CALLER drives, not what exists on the server.
//
// Calling this twice returns ErrMainThreadExists. Run a second client for a
// second conversation, which also gives each one its own process and handler.
func (c *Client) StartThread(ctx context.Context, params protocol.ThreadStartParams) (*Thread, error) {
	if err := c.claimMainThread(); err != nil {
		return nil, err
	}
	out, err := c.ThreadStart(ctx, params)
	if err != nil {
		c.releaseMainThread()
		return nil, err
	}
	if out.Thread == nil || out.Thread.ID == "" {
		c.releaseMainThread()
		return nil, errors.New("codex: thread/start returned no thread id")
	}
	t := &Thread{client: c, id: out.Thread.ID, started: out}
	c.setMainThread(t)
	return t, nil
}

// ResumeThread reopens a stored thread as this client's main thread.
//
// Like StartThread, this returns ErrMainThreadExists if the client already has
// one.
func (c *Client) ResumeThread(ctx context.Context, params protocol.ThreadResumeParams) (*Thread, error) {
	if err := c.claimMainThread(); err != nil {
		return nil, err
	}
	out, err := c.ThreadResume(ctx, params)
	if err != nil {
		c.releaseMainThread()
		return nil, err
	}
	if out.Thread == nil || out.Thread.ID == "" {
		c.releaseMainThread()
		return nil, errors.New("codex: thread/resume returned no thread id")
	}
	// ThreadResumeResponse and ThreadStartResponse describe the same thing, so
	// the handle carries the start-shaped view for a uniform Info().
	t := &Thread{client: c, id: out.Thread.ID, started: resumeToStart(out)}
	c.setMainThread(t)
	return t, nil
}

// Thread returns a handle for a thread id that is already loaded, without
// starting or resuming anything.
//
// Use it to address a thread whose id came from elsewhere: thread/list, or a
// sub-agent id observed in a notification. This does NOT claim the main thread
// slot, because the caller did not create the thread and does not own its
// lifecycle. Info returns nil for such a handle.
func (c *Client) Thread(id string) *Thread {
	return &Thread{client: c, id: id}
}

// TurnResult is the outcome of a completed turn.
type TurnResult struct {
	// Turn is the final turn as the server reported it, including every item.
	Turn *protocol.Turn

	// AgentMessage is the agent's reply text, concatenated from the final-answer
	// agentMessage items. It is empty when the agent produced no message.
	AgentMessage string

	// Items are the turn's items, in order.
	Items []*protocol.ThreadItem
}

// Status returns the turn's terminal status: completed, interrupted, or failed.
func (r *TurnResult) Status() protocol.TurnStatus {
	if r == nil || r.Turn == nil {
		return ""
	}
	return r.Turn.Status
}

// Interrupted reports whether the turn ended because it was cancelled.
//
// Interruption is not an error: it is usually the caller's own doing, via
// Interrupt or a cancelled context.
func (r *TurnResult) Interrupted() bool {
	return r.Status() == protocol.TurnStatusInterrupted
}

// TurnFailedError reports a turn that ended with a failed status.
//
// Inspect Info to branch on the cause, for example a usage limit versus a
// context window overflow:
//
//	var failed *codex.TurnFailedError
//	if errors.As(err, &failed) {
//	    if failed.Info.IsUsageLimitExceeded() { ... }
//	}
type TurnFailedError struct {
	// TurnID is the turn that failed.
	TurnID string

	// Message is the server's error message.
	Message string

	// Info is the structured error classification, when the server supplied one.
	Info *protocol.CodexErrorInfo

	// Details carries any additional server-provided context.
	Details string

	// Turn is the full failed turn.
	Turn *protocol.Turn
}

func (e *TurnFailedError) Error() string {
	var b strings.Builder
	b.WriteString("codex: turn ")
	b.WriteString(e.TurnID)
	b.WriteString(" failed")
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Info != nil && e.Info.Kind != "" {
		b.WriteString(" (")
		b.WriteString(string(e.Info.Kind))
		b.WriteString(")")
	}
	if e.Details != "" {
		b.WriteString("; ")
		b.WriteString(e.Details)
	}
	return b.String()
}

// Run starts a turn and blocks until it completes.
//
// Incremental output arrives through the client's Handler callbacks while Run is
// blocked. A caller that wants to print text as it streams registers
// OnAgentMessageDelta with WithHandler and reads the final result here; there is
// deliberately no second event-channel API, because two delivery mechanisms for
// the same notifications means two things to keep in sync.
//
// A turn that fails returns a *TurnFailedError. A turn that is interrupted does
// NOT return an error, since interruption is usually the caller's own doing:
// check TurnResult.Interrupted.
//
// If ctx is cancelled, Run asks the server to interrupt the turn and then returns
// ctx.Err(), so a cancelled context does not leave the agent working.
//
// One ordering caveat: Run may return before OnTurnCompleted has finished. The
// internal completion signal fires ahead of user callbacks so that a panicking
// callback cannot strand a Run call, which means the two are concurrent at the
// very end of a turn. Everything OnTurnCompleted receives is also in the returned
// TurnResult, so read the result rather than relying on the callback having run.
func (t *Thread) Run(ctx context.Context, params protocol.TurnStartParams) (*TurnResult, error) {
	params.ThreadID = t.id

	// Register the waiter BEFORE starting the turn. On a fast turn the server can
	// emit turn/completed before the turn/start response is processed, and
	// registering afterwards would miss it and block forever.
	waiter := newTurnWaiter()
	t.client.dispatch.addWaiter(t.id, waiter)
	defer t.client.dispatch.removeWaiter(t.id, waiter)

	out, err := t.client.TurnStart(ctx, params)
	if err != nil {
		return nil, err
	}
	if out.Turn == nil || out.Turn.ID == "" {
		return nil, errors.New("codex: turn/start returned no turn id")
	}
	waiter.setTurnID(out.Turn.ID)
	t.setCurrentTurn(out.Turn.ID)
	defer t.setCurrentTurn("")

	select {
	case <-ctx.Done():
		// The caller gave up, so ask the server to stop rather than leaving it
		// working on a result nobody will read.
		t.interruptDetached(out.Turn.ID)
		return nil, ctx.Err()

	case turn := <-waiter.done:
		if turn == nil {
			return nil, errors.New("codex: connection closed before the turn completed")
		}
		return newTurnResult(turn, waiter.collectedItems(), waiter.err())
	}
}

func (t *Thread) setCurrentTurn(id string) {
	t.turnMu.Lock()
	t.currentTurn = id
	t.turnMu.Unlock()
}

// CurrentTurnID returns the id of the turn Run is blocked on, or "" when no turn
// is in flight.
//
// Run blocks, so steering means calling from another goroutine, and this saves
// capturing the id out of an OnTurnStarted callback just to name the turn you
// want to steer.
func (t *Thread) CurrentTurnID() string {
	t.turnMu.Lock()
	defer t.turnMu.Unlock()
	return t.currentTurn
}

// SteerCurrent appends input to whichever turn is in flight.
//
// It fails if no turn is running. Prefer it over Steer when you simply want to
// add to "the turn happening now", which is the usual case: the server rejects a
// steer whose expected turn id does not match the active one, so passing the id
// yourself only helps when you specifically need that check.
func (t *Thread) SteerCurrent(ctx context.Context, input ...*protocol.UserInput) (string, error) {
	turnID := t.CurrentTurnID()
	if turnID == "" {
		return "", errors.New("codex: no turn is in flight to steer")
	}
	return t.Steer(ctx, turnID, input...)
}

// SteerText is SteerCurrent for a single text message.
func (t *Thread) SteerText(ctx context.Context, text string) (string, error) {
	return t.SteerCurrent(ctx, TextInput(text))
}

// interruptTimeout bounds the courtesy interrupt sent when a caller's context is
// cancelled. It is short: the caller has already stopped waiting.
const interruptTimeout = 5 * time.Second

// interruptDetached cancels a turn on a fresh context, because the caller's
// context is already cancelled and would reject the call.
func (t *Thread) interruptDetached(turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), interruptTimeout)
	defer cancel()
	_ = t.Interrupt(ctx, turnID)
}

// newTurnResult assembles the outcome, mapping a failed turn to a typed error and
// leaving an interrupted turn error-free.
func newTurnResult(
	turn *protocol.Turn,
	streamed []*protocol.ThreadItem,
	lastErr *protocol.ErrorNotification,
) (*TurnResult, error) {
	// Prefer the items observed via item/completed: the app-server documentation
	// names item/* as the source of truth, and the turn payload can arrive with an
	// empty items array. Fall back to the payload for a server that populates it.
	items := streamed
	if len(items) == 0 {
		items = turn.Items
	}

	result := &TurnResult{
		Turn:         turn,
		Items:        items,
		AgentMessage: finalAgentMessage(items),
	}
	if turn.Status != protocol.TurnStatusFailed {
		return result, nil
	}

	failure := &TurnFailedError{TurnID: turn.ID, Turn: turn}
	if turn.Error != nil {
		failure.Message = turn.Error.Message
		failure.Info = turn.Error.CodexErrorInfo
		if turn.Error.AdditionalDetails != nil {
			failure.Details = *turn.Error.AdditionalDetails
		}
	}
	// The error notification often carries detail the turn payload omits.
	if lastErr != nil && lastErr.Error != nil {
		if failure.Message == "" {
			failure.Message = lastErr.Error.Message
		}
		if failure.Info == nil {
			failure.Info = lastErr.Error.CodexErrorInfo
		}
	}
	return result, failure
}

// finalAgentMessage concatenates the final-answer agent messages, which is the
// reply a caller usually wants. Commentary-phase messages are excluded.
func finalAgentMessage(items []*protocol.ThreadItem) string {
	var b strings.Builder
	for _, item := range items {
		msg, ok := item.AsAgentMessage()
		if !ok {
			continue
		}
		// A nil phase means the server did not classify it; treat that as the reply.
		if msg.Phase == nil || msg.Phase.IsFinalAnswer() {
			b.WriteString(msg.Text)
		}
	}
	return b.String()
}

// RunText is Run for the common case of a single text message.
func (t *Thread) RunText(ctx context.Context, text string) (*TurnResult, error) {
	return t.Run(ctx, protocol.TurnStartParams{Input: []*protocol.UserInput{TextInput(text)}})
}

// TextInput builds a text input item.
func TextInput(text string) *protocol.UserInput {
	in := protocol.NewUserInputText(protocol.UserInputTextPayload{Text: text})
	return &in
}

// ImageInput builds an input item referencing a remote image by URL.
func ImageInput(url string) *protocol.UserInput {
	in := protocol.NewUserInputImage(protocol.UserInputImagePayload{URL: url})
	return &in
}

// LocalImageInput builds an input item referencing an image on the server's
// filesystem by absolute path.
func LocalImageInput(path string) *protocol.UserInput {
	in := protocol.NewUserInputLocalImage(protocol.UserInputLocalImagePayload{Path: path})
	return &in
}

// SkillInput builds a skill input item.
//
// Pair it with text containing "$<skill-name>". Including this item lets the
// server inject the skill's full instructions rather than making the model
// resolve the name, which is faster and more reliable.
func SkillInput(name, path string) *protocol.UserInput {
	in := protocol.NewUserInputSkill(protocol.UserInputSkillPayload{Name: name, Path: path})
	return &in
}

// Steer appends user input to the turn currently in flight.
//
// It fails if there is no active turn. Turn-level overrides are not accepted.
func (t *Thread) Steer(ctx context.Context, turnID string, input ...*protocol.UserInput) (string, error) {
	out, err := t.client.TurnSteer(ctx, protocol.TurnSteerParams{
		ThreadID:       t.id,
		ExpectedTurnID: turnID,
		Input:          input,
	})
	if err != nil {
		return "", err
	}
	if out.TurnID == "" {
		return turnID, nil
	}
	return out.TurnID, nil
}

// Interrupt requests cancellation of a turn. The turn then ends with an
// interrupted status.
func (t *Thread) Interrupt(ctx context.Context, turnID string) error {
	return t.client.TurnInterrupt(ctx, t.id, turnID)
}

// Archive moves this thread's log into the archived directory.
func (t *Thread) Archive(ctx context.Context) error {
	return t.client.ThreadArchive(ctx, t.id)
}

// Delete permanently deletes this thread and its spawned descendants.
func (t *Thread) Delete(ctx context.Context) error {
	return t.client.ThreadDelete(ctx, t.id)
}

// Unsubscribe stops this connection receiving the thread's events.
func (t *Thread) Unsubscribe(ctx context.Context) (*protocol.ThreadUnsubscribeResponse, error) {
	return t.client.ThreadUnsubscribe(ctx, t.id)
}

// SetName sets the thread's user-facing name.
func (t *Thread) SetName(ctx context.Context, name string) error {
	_, err := t.client.ThreadSetName(ctx, protocol.ThreadSetNameParams{ThreadID: t.id, Name: name})
	return err
}

// Compact triggers history compaction for this thread.
func (t *Thread) Compact(ctx context.Context) error {
	return t.client.ThreadCompactStart(ctx, t.id)
}

// resumeToStart re-shapes a resume response as a start response, so a Thread
// handle exposes one type regardless of how it was obtained.
func resumeToStart(in *protocol.ThreadResumeResponse) *protocol.ThreadStartResponse {
	if in == nil {
		return nil
	}
	// The two types are structurally compatible on the fields that matter;
	// round-tripping through JSON avoids hand-copying 20 fields and staying in
	// sync with them as the schema evolves.
	data, err := json.Marshal(in)
	if err != nil {
		return nil
	}
	var out protocol.ThreadStartResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}
