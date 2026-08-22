package codex

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

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
}

// ID returns the thread id.
func (t *Thread) ID() string { return t.id }

// Info returns the response that created or resumed this thread, including the
// resolved model, cwd, sandbox policy, and instruction sources.
func (t *Thread) Info() *protocol.ThreadStartResponse { return t.started }

// StartThread creates a thread and returns a handle to it.
func (c *Client) StartThread(ctx context.Context, params protocol.ThreadStartParams) (*Thread, error) {
	out, err := c.ThreadStart(ctx, params)
	if err != nil {
		return nil, err
	}
	if out.Thread == nil || out.Thread.ID == "" {
		return nil, errors.New("codex: thread/start returned no thread id")
	}
	return &Thread{client: c, id: out.Thread.ID, started: out}, nil
}

// ResumeThread reopens a stored thread and returns a handle to it.
func (c *Client) ResumeThread(ctx context.Context, params protocol.ThreadResumeParams) (*Thread, error) {
	out, err := c.ThreadResume(ctx, params)
	if err != nil {
		return nil, err
	}
	if out.Thread == nil || out.Thread.ID == "" {
		return nil, errors.New("codex: thread/resume returned no thread id")
	}
	// ThreadResumeResponse and ThreadStartResponse describe the same thing, so
	// the handle carries the start-shaped view for a uniform Info().
	return &Thread{
		client:  c,
		id:      out.Thread.ID,
		started: resumeToStart(out),
	}, nil
}

// Thread returns a handle for a thread id that is already loaded, without
// starting or resuming anything.
//
// Use it when the id came from elsewhere, such as thread/list. Info returns nil
// for such a handle.
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

// Run starts a turn with the given input and blocks until it completes.
//
// A turn that fails returns a *TurnFailedError. A turn that is interrupted does
// NOT return an error: check TurnResult.Interrupted.
//
// If ctx is cancelled, Run asks the server to interrupt the turn and then
// returns ctx.Err(), so a cancelled context does not leave the agent working.
func (t *Thread) Run(ctx context.Context, params protocol.TurnStartParams) (*TurnResult, error) {
	stream, err := t.RunStream(ctx, params)
	if err != nil {
		return nil, err
	}
	// Draining is required: the events channel is what advances the stream.
	for range stream.Events() {
	}
	return stream.Result()
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

// observer receives one thread's notifications alongside the client-level
// handler. It is how Run and RunStream await turn completion without competing
// with user callbacks.
type observer struct {
	mu     sync.Mutex
	events chan *notification
	closed bool
}

// observerReserve is spare capacity beyond the nominal buffer, held for terminal
// notifications so they are never dropped.
const observerReserve = 8

func newObserver(buf int) *observer {
	return &observer{events: make(chan *notification, buf+observerReserve)}
}

// notify enqueues a notification for the observer.
//
// It runs on the thread's dispatch goroutine and must not block indefinitely, so
// a full queue drops the notification for the OBSERVER only; the client-level
// handler still receives it. Dropping a delta degrades the stream but does not
// break it.
//
// Terminal notifications are the exception and are NEVER dropped: losing
// turn/completed would leave Run and RunStream waiting forever. The queue has a
// small reserve beyond its nominal capacity for exactly this.
func (o *observer) notify(n *notification) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	if isTerminalNotification(n.method) {
		// Blocking here is unacceptable, so rely on the reserve headroom. If even
		// that is exhausted the consumer is wedged, and the context or Close will
		// unblock it instead.
		select {
		case o.events <- n:
		default:
			go func() {
				o.mu.Lock()
				defer o.mu.Unlock()
				if !o.closed {
					select {
					case o.events <- n:
					default:
					}
				}
			}()
		}
		return
	}
	select {
	case o.events <- n:
	default:
	}
}

// isTerminalNotification reports whether losing this notification would strand a
// caller waiting for a turn to finish.
func isTerminalNotification(method string) bool {
	return method == protocol.NotifyTurnCompleted || method == protocol.NotifyError
}

func (o *observer) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		close(o.events)
	}
}
