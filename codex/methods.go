package codex

import (
	"context"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// This file is the thin layer: one Go method per wire method, taking and
// returning the generated types unchanged. Nothing is hidden or reinterpreted,
// so behaviour can be checked directly against the app-server documentation.
//
// The ergonomic layer in thread.go is built only from these methods.

// ThreadStart creates a new thread. It also subscribes this connection to the
// thread's turn and item notifications.
func (c *Client) ThreadStart(ctx context.Context, params protocol.ThreadStartParams) (*protocol.ThreadStartResponse, error) {
	var out protocol.ThreadStartResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadStart, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadResume reopens a stored thread so later turns append to it.
func (c *Client) ThreadResume(ctx context.Context, params protocol.ThreadResumeParams) (*protocol.ThreadResumeResponse, error) {
	var out protocol.ThreadResumeResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadResume, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadFork branches a thread into a new thread id by copying its history.
//
// Pass LastTurnID to copy history through that turn and drop later ones, or
// Ephemeral to fork in memory without adding the result to stored listings.
func (c *Client) ThreadFork(ctx context.Context, params protocol.ThreadForkParams) (*protocol.ThreadForkResponse, error) {
	var out protocol.ThreadForkResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadFork, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadRead reads a stored thread without resuming it or subscribing to its
// events. Set IncludeTurns to get the full turn history.
func (c *Client) ThreadRead(ctx context.Context, params protocol.ThreadReadParams) (*protocol.ThreadReadResponse, error) {
	var out protocol.ThreadReadResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadRead, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadList pages through stored threads, newest first by default.
//
// A nil NextCursor in the response means the last page. See ListThreads for an
// iterator that handles pagination.
func (c *Client) ThreadList(ctx context.Context, params protocol.ThreadListParams) (*protocol.ThreadListResponse, error) {
	var out protocol.ThreadListResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadList, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadLoadedList returns the ids of threads currently loaded in memory.
func (c *Client) ThreadLoadedList(ctx context.Context) (*protocol.ThreadLoadedListResponse, error) {
	var out protocol.ThreadLoadedListResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadLoadedList, protocol.ThreadLoadedListParams{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadArchive moves a thread's log into the archived directory, along with any
// spawned descendant threads that are not already archived.
func (c *Client) ThreadArchive(ctx context.Context, threadID string) error {
	var out protocol.ThreadArchiveResponse
	return c.conn.Call(ctx, protocol.MethodThreadArchive,
		protocol.ThreadArchiveParams{ThreadID: threadID}, &out)
}

// ThreadUnarchive restores an archived thread to the active sessions directory.
func (c *Client) ThreadUnarchive(ctx context.Context, threadID string) (*protocol.ThreadUnarchiveResponse, error) {
	var out protocol.ThreadUnarchiveResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadUnarchive,
		protocol.ThreadUnarchiveParams{ThreadID: threadID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadDelete permanently deletes a persisted thread and its spawned
// descendants. Ephemeral root threads cannot be deleted.
func (c *Client) ThreadDelete(ctx context.Context, threadID string) error {
	var out protocol.ThreadDeleteResponse
	return c.conn.Call(ctx, protocol.MethodThreadDelete,
		protocol.ThreadDeleteParams{ThreadID: threadID}, &out)
}

// ThreadUnsubscribe removes this connection's subscription to a thread.
//
// If it was the last subscriber, the server unloads the thread after a
// no-subscriber inactivity grace period and then emits thread/closed.
func (c *Client) ThreadUnsubscribe(ctx context.Context, threadID string) (*protocol.ThreadUnsubscribeResponse, error) {
	var out protocol.ThreadUnsubscribeResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadUnsubscribe,
		protocol.ThreadUnsubscribeParams{ThreadID: threadID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadSetName sets a thread's user-facing name.
func (c *Client) ThreadSetName(ctx context.Context, params protocol.ThreadSetNameParams) (*protocol.ThreadSetNameResponse, error) {
	var out protocol.ThreadSetNameResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadNameSet, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadGoalSet sets or updates a thread's goal.
//
// Supplying a new objective replaces the goal and resets usage accounting;
// omitting the objective updates status or token budget while preserving it.
func (c *Client) ThreadGoalSet(ctx context.Context, params protocol.ThreadGoalSetParams) (*protocol.ThreadGoalSetResponse, error) {
	var out protocol.ThreadGoalSetResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadGoalSet, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadGoalGet reads a thread's current goal.
func (c *Client) ThreadGoalGet(ctx context.Context, threadID string) (*protocol.ThreadGoalGetResponse, error) {
	var out protocol.ThreadGoalGetResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadGoalGet,
		protocol.ThreadGoalGetParams{ThreadID: threadID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadGoalClear clears a thread's goal.
func (c *Client) ThreadGoalClear(ctx context.Context, threadID string) error {
	var out protocol.ThreadGoalClearResponse
	return c.conn.Call(ctx, protocol.MethodThreadGoalClear,
		protocol.ThreadGoalClearParams{ThreadID: threadID}, &out)
}

// ThreadMetadataUpdate patches stored thread metadata without resuming the
// thread. Omitted fields stay unchanged.
func (c *Client) ThreadMetadataUpdate(ctx context.Context, params protocol.ThreadMetadataUpdateParams) (*protocol.ThreadMetadataUpdateResponse, error) {
	var out protocol.ThreadMetadataUpdateResponse
	if err := c.conn.Call(ctx, protocol.MethodThreadMetadataUpdate, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ThreadCompactStart triggers history compaction for a thread.
//
// It returns as soon as the server accepts the request. Progress arrives as
// ordinary turn and item notifications, including a contextCompaction item.
func (c *Client) ThreadCompactStart(ctx context.Context, threadID string) error {
	var out protocol.ThreadCompactStartResponse
	return c.conn.Call(ctx, protocol.MethodThreadCompactStart,
		protocol.ThreadCompactStartParams{ThreadID: threadID}, &out)
}

// ThreadShellCommand runs a user-initiated shell command against a thread.
//
// This runs OUTSIDE the sandbox with full access and does not inherit the
// thread's sandbox policy. Expose it only for commands the user explicitly
// asked for.
func (c *Client) ThreadShellCommand(ctx context.Context, params protocol.ThreadShellCommandParams) error {
	var out protocol.ThreadShellCommandResponse
	return c.conn.Call(ctx, protocol.MethodThreadShellCommand, params, &out)
}

// TurnStart adds user input to a thread and begins generation.
//
// It returns as soon as the turn is accepted, not when the turn finishes;
// progress arrives as notifications. Use Thread.Run to wait for completion.
func (c *Client) TurnStart(ctx context.Context, params protocol.TurnStartParams) (*protocol.TurnStartResponse, error) {
	var out protocol.TurnStartResponse
	if err := c.conn.Call(ctx, protocol.MethodTurnStart, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TurnSteer appends more user input to the turn already in flight.
//
// ExpectedTurnID must match the active turn. This does not accept turn-level
// overrides and does not emit a new turn/started notification.
func (c *Client) TurnSteer(ctx context.Context, params protocol.TurnSteerParams) (*protocol.TurnSteerResponse, error) {
	var out protocol.TurnSteerResponse
	if err := c.conn.Call(ctx, protocol.MethodTurnSteer, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// TurnInterrupt requests cancellation of an in-flight turn. On success the turn
// ends with an interrupted status.
func (c *Client) TurnInterrupt(ctx context.Context, threadID, turnID string) error {
	var out protocol.TurnInterruptResponse
	return c.conn.Call(ctx, protocol.MethodTurnInterrupt,
		protocol.TurnInterruptParams{ThreadID: threadID, TurnID: turnID}, &out)
}

// ReviewStart runs the Codex reviewer for a thread and streams review items.
func (c *Client) ReviewStart(ctx context.Context, params protocol.ReviewStartParams) (*protocol.ReviewStartResponse, error) {
	var out protocol.ReviewStartResponse
	if err := c.conn.Call(ctx, protocol.MethodReviewStart, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ModelList lists available models and their capabilities.
//
// By default only picker-visible models are returned; set IncludeHidden for the
// full list.
func (c *Client) ModelList(ctx context.Context, params protocol.ModelListParams) (*protocol.ModelListResponse, error) {
	var out protocol.ModelListResponse
	if err := c.conn.Call(ctx, protocol.MethodModelList, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FSWatch subscribes to filesystem changes under path. It returns the canonical
// path the server actually watches. Match the returned watchId against the
// fs/changed events, and pass it to FSUnwatch to stop watching.
func (c *Client) FSWatch(ctx context.Context, params protocol.FsWatchParams) (*protocol.FsWatchResponse, error) {
	var out protocol.FsWatchResponse
	if err := c.conn.Call(ctx, protocol.MethodFsWatch, params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FSUnwatch stops a subscription started by FSWatch or the workdir
// auto-subscription in StartThread.
func (c *Client) FSUnwatch(ctx context.Context, params protocol.FsUnwatchParams) error {
	var out protocol.FsUnwatchResponse
	return c.conn.Call(ctx, protocol.MethodFsUnwatch, params, &out)
}
