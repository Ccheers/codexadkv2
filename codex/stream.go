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

// interruptTimeout bounds the detached turn/interrupt sent when the caller's
// context is cancelled. It is short: the caller has already given up waiting,
// and this is a courtesy to stop the server doing needless work.
const interruptTimeout = 5 * time.Second

// EventKind identifies what a TurnStream event carries.
type EventKind string

const (
	// EventTurnStarted is emitted once when the turn begins.
	EventTurnStarted EventKind = "turn.started"

	// EventItemStarted is emitted when a new item begins.
	EventItemStarted EventKind = "item.started"

	// EventItemCompleted is emitted when an item finishes. This is the
	// authoritative state of the item.
	EventItemCompleted EventKind = "item.completed"

	// EventAgentMessageDelta carries streamed agent reply text. Concatenate
	// Delta values in the order received.
	EventAgentMessageDelta EventKind = "agentMessage.delta"

	// EventReasoningDelta carries streamed reasoning summary text.
	EventReasoningDelta EventKind = "reasoning.delta"

	// EventCommandOutputDelta carries streamed stdout and stderr from a command.
	EventCommandOutputDelta EventKind = "commandExecution.outputDelta"

	// EventPlanUpdated is emitted when the agent shares or revises its plan.
	EventPlanUpdated EventKind = "turn.plan"

	// EventDiffUpdated carries the aggregated unified diff for the turn so far.
	EventDiffUpdated EventKind = "turn.diff"

	// EventError reports a turn error. The turn then completes as failed.
	EventError EventKind = "error"

	// EventTurnCompleted is emitted once, last, when the turn reaches a terminal
	// state. The channel closes after it.
	EventTurnCompleted EventKind = "turn.completed"
)

// Event is one item in a TurnStream.
//
// Exactly one payload field is populated, determined by Kind. Delta carries the
// text for the delta kinds.
type Event struct {
	Kind EventKind

	// ItemID is the item this event concerns, for item and delta events.
	ItemID string

	// Delta is the streamed text fragment, for the delta event kinds.
	Delta string

	// Item is the full item, for EventItemStarted and EventItemCompleted.
	Item *protocol.ThreadItem

	// Turn is the turn, for EventTurnStarted and EventTurnCompleted.
	Turn *protocol.Turn

	// Plan is the agent's plan, for EventPlanUpdated.
	Plan *protocol.TurnPlanUpdatedNotification

	// Diff is the aggregated unified diff, for EventDiffUpdated.
	Diff string

	// Err is the reported error, for EventError.
	Err *protocol.ErrorNotification

	// Raw is the notification's original JSON.
	Raw json.RawMessage
}

// AgentText returns the agent reply text carried by this event, if any. It is a
// convenience for the common streaming-print loop.
func (e Event) AgentText() string {
	if e.Kind == EventAgentMessageDelta {
		return e.Delta
	}
	return ""
}

// TurnStream is an in-flight turn presented as an ordered event channel.
//
// Consume Events until it closes, then call Result. Events for one turn arrive
// in the order the server sent them, so deltas concatenate correctly.
//
// The stream must be drained. Abandoning it without draining leaks the
// underlying observer until the client closes; use the context to stop early,
// which interrupts the turn.
type TurnStream struct {
	thread *Thread
	turnID string

	obs    *observer
	events chan Event

	once   sync.Once
	result *TurnResult
	err    error
}

// TurnID returns the id of the turn being streamed.
func (s *TurnStream) TurnID() string { return s.turnID }

// Events returns the ordered event channel. It closes when the turn reaches a
// terminal state, or when the connection ends.
func (s *TurnStream) Events() <-chan Event { return s.events }

// Result returns the turn's outcome. It blocks until the stream has finished, so
// it is safe to call immediately after the Events channel closes.
//
// A failed turn returns a *TurnFailedError; an interrupted turn returns no error.
func (s *TurnStream) Result() (*TurnResult, error) {
	// Draining guarantees the terminal event was processed even if the caller
	// called Result without reading Events.
	for range s.events {
	}
	return s.result, s.err
}

// RunStream starts a turn and returns it as an ordered event stream.
//
// Use it when you want to render output as it arrives; use Run when you only
// want the final result.
//
// If ctx is cancelled, the turn is interrupted and the stream ends.
func (t *Thread) RunStream(ctx context.Context, params protocol.TurnStartParams) (*TurnStream, error) {
	params.ThreadID = t.id

	// Subscribe BEFORE starting the turn. The server can emit turn/started and
	// even item events before the turn/start response arrives, and subscribing
	// afterwards would race and miss them.
	obs := newObserver(t.client.opts.notificationBuffer)
	t.client.dispatch.addObserver(t.id, obs)

	out, err := t.client.TurnStart(ctx, params)
	if err != nil {
		t.client.dispatch.removeObserver(t.id, obs)
		obs.close()
		return nil, err
	}
	if out.Turn == nil || out.Turn.ID == "" {
		t.client.dispatch.removeObserver(t.id, obs)
		obs.close()
		return nil, errors.New("codex: turn/start returned no turn id")
	}

	s := &TurnStream{
		thread: t,
		turnID: out.Turn.ID,
		obs:    obs,
		events: make(chan Event, 64),
	}
	go s.pump(ctx)
	return s, nil
}

// pump translates this thread's notifications into stream events until the turn
// reaches a terminal state.
func (s *TurnStream) pump(ctx context.Context) {
	defer func() {
		s.thread.client.dispatch.removeObserver(s.thread.id, s.obs)
		s.obs.close()
		close(s.events)
	}()

	var (
		text     strings.Builder
		items    []*protocol.ThreadItem
		lastErr  *protocol.ErrorNotification
		finished bool
	)

	for !finished {
		select {
		case <-ctx.Done():
			// Ask the server to stop, so a cancelled context does not leave the
			// agent working, then report the context error.
			s.interruptDetached()
			s.finish(nil, ctx.Err(), text.String(), items)
			return

		case n, ok := <-s.obs.events:
			if !ok {
				// The connection went away before the turn finished.
				s.finish(nil, errors.New("codex: connection closed before the turn completed"),
					text.String(), items)
				return
			}

			ev, terminal := s.translate(n, &text, &items, &lastErr)
			if ev != nil {
				// A slow consumer must not deadlock the dispatch goroutine, so
				// the send races the context.
				select {
				case s.events <- *ev:
				case <-ctx.Done():
					s.interruptDetached()
					s.finish(nil, ctx.Err(), text.String(), items)
					return
				}
			}
			if terminal != nil {
				finished = true
				s.completeTurn(terminal, text.String(), items, lastErr)
			}
		}
	}
}

// translate converts one notification into an event, and reports the completed
// turn when this notification ends the turn.
func (s *TurnStream) translate(
	n *notification,
	text *strings.Builder,
	items *[]*protocol.ThreadItem,
	lastErr **protocol.ErrorNotification,
) (ev *Event, terminal *protocol.Turn) {
	switch n.method {
	case protocol.NotifyTurnStarted:
		var p protocol.TurnStartedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.Turn == nil {
			return nil, nil
		}
		if p.Turn.ID != s.turnID {
			return nil, nil
		}
		return &Event{Kind: EventTurnStarted, Turn: p.Turn, Raw: n.params}, nil

	case protocol.NotifyTurnCompleted:
		var p protocol.TurnCompletedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.Turn == nil {
			return nil, nil
		}
		if p.Turn.ID != s.turnID {
			return nil, nil
		}
		return &Event{Kind: EventTurnCompleted, Turn: p.Turn, Raw: n.params}, p.Turn

	case protocol.NotifyItemStarted:
		var p protocol.ItemStartedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{Kind: EventItemStarted, Item: p.Item, ItemID: itemID(p.Item), Raw: n.params}, nil

	case protocol.NotifyItemCompleted:
		var p protocol.ItemCompletedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		if p.Item != nil {
			*items = append(*items, p.Item)
			// The completed item is authoritative for the reply text; deltas are
			// advisory and may not concatenate to exactly this.
			if msg, ok := p.Item.AsAgentMessage(); ok && isFinalAnswer(msg) {
				text.Reset()
				text.WriteString(msg.Text)
			}
		}
		return &Event{Kind: EventItemCompleted, Item: p.Item, ItemID: itemID(p.Item), Raw: n.params}, nil

	case protocol.NotifyItemAgentMessageDelta:
		var p protocol.AgentMessageDeltaNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{
			Kind: EventAgentMessageDelta, ItemID: p.ItemID, Delta: p.Delta, Raw: n.params,
		}, nil

	case protocol.NotifyItemReasoningSummaryTextDelta:
		var p protocol.ReasoningSummaryTextDeltaNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{
			Kind: EventReasoningDelta, ItemID: p.ItemID, Delta: p.Delta, Raw: n.params,
		}, nil

	case protocol.NotifyItemCommandExecutionOutputDelta:
		var p protocol.CommandExecutionOutputDeltaNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{
			Kind: EventCommandOutputDelta, ItemID: p.ItemID, Delta: p.Delta, Raw: n.params,
		}, nil

	case protocol.NotifyTurnPlanUpdated:
		var p protocol.TurnPlanUpdatedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{Kind: EventPlanUpdated, Plan: &p, Raw: n.params}, nil

	case protocol.NotifyTurnDiffUpdated:
		var p protocol.TurnDiffUpdatedNotification
		if err := json.Unmarshal(n.params, &p); err != nil || p.TurnID != s.turnID {
			return nil, nil
		}
		return &Event{Kind: EventDiffUpdated, Diff: p.Diff, Raw: n.params}, nil

	case protocol.NotifyError:
		var p protocol.ErrorNotification
		if err := json.Unmarshal(n.params, &p); err != nil {
			return nil, nil
		}
		if p.TurnID != s.turnID {
			return nil, nil
		}
		// Retain it: the turn will complete as failed, and this carries the
		// classification the turn payload may not.
		*lastErr = &p
		return &Event{Kind: EventError, Err: &p, Raw: n.params}, nil
	}
	return nil, nil
}

// completeTurn records the terminal result, mapping a failed turn to a typed
// error and leaving an interrupted turn error-free.
func (s *TurnStream) completeTurn(
	turn *protocol.Turn,
	text string,
	items []*protocol.ThreadItem,
	lastErr *protocol.ErrorNotification,
) {
	if len(items) == 0 && len(turn.Items) > 0 {
		items = turn.Items
	}
	result := &TurnResult{Turn: turn, AgentMessage: text, Items: items}

	if turn.Status != protocol.TurnStatusFailed {
		s.finish(result, nil, text, items)
		return
	}

	failure := &TurnFailedError{TurnID: turn.ID, Turn: turn}
	if turn.Error != nil {
		failure.Message = turn.Error.Message
		failure.Info = turn.Error.CodexErrorInfo
		if turn.Error.AdditionalDetails != nil {
			failure.Details = *turn.Error.AdditionalDetails
		}
	}
	// An error notification often carries detail the turn payload omits.
	if lastErr != nil && lastErr.Error != nil {
		if failure.Message == "" {
			failure.Message = lastErr.Error.Message
		}
		if failure.Info == nil {
			failure.Info = lastErr.Error.CodexErrorInfo
		}
	}
	s.finish(result, failure, text, items)
}

func (s *TurnStream) finish(result *TurnResult, err error, text string, items []*protocol.ThreadItem) {
	s.once.Do(func() {
		if result == nil {
			result = &TurnResult{AgentMessage: text, Items: items}
		}
		s.result = result
		s.err = err
	})
}

// interruptDetached asks the server to cancel the turn using a fresh context,
// because the caller's context is already cancelled and would reject the call.
func (s *TurnStream) interruptDetached() {
	ctx, cancel := context.WithTimeout(context.Background(), interruptTimeout)
	defer cancel()
	_ = s.thread.Interrupt(ctx, s.turnID)
}

func itemID(item *protocol.ThreadItem) string {
	if item == nil {
		return ""
	}
	// Every variant carries an id, but they live on the variant payloads, so a
	// cheap probe of the raw JSON is simpler than a 20-arm switch.
	var probe struct {
		ID string `json:"id"`
	}
	if len(item.Raw) > 0 {
		_ = json.Unmarshal(item.Raw, &probe)
	}
	return probe.ID
}

// isFinalAnswer reports whether an agent message is the reply rather than
// intermediate commentary. A nil phase means the server did not classify it, and
// those are treated as the reply.
func isFinalAnswer(msg *protocol.ThreadItemAgentMessagePayload) bool {
	if msg == nil {
		return false
	}
	if msg.Phase == nil {
		return true
	}
	return msg.Phase.IsFinalAnswer()
}
