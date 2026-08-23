package codex

import (
	"context"
	"errors"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// Session is a client and its single thread as one object.
//
// It exists because the two are almost always created together and in a fixed
// order: spawn the server, handshake, start one thread, run turns. Open does all
// of that in one call, so the one-client-one-thread invariant is structural
// rather than something Client enforces at runtime — there is no second
// StartThread to call, and therefore no ErrMainThreadExists to handle.
//
//	session, err := codex.Open(ctx,
//	    codex.WithClientInfo("my_product", "My Product", "1.0.0"),
//	    codex.WithHandler(codex.Handler{
//	        OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
//	            fmt.Print(n.Delta)
//	        },
//	    }),
//	)
//	if err != nil {
//	    return err
//	}
//	defer session.Close()
//
//	result, err := session.RunText(ctx, "Summarize this repo.")
//
// Reach for Client and StartThread directly when you need something Session
// hides: resuming a stored thread, forking, or inspecting the server before
// deciding how to configure the thread.
type Session struct {
	client *Client
	thread *Thread
}

// SessionOption configures Open, which has two jobs: building the client and
// starting the thread.
//
// It is an interface rather than a function type so that client options satisfy
// it directly. That is the whole point: Open(ctx, WithClientInfo(...)) works
// without wrapping, while still rejecting anything that is not an option at
// compile time.
type SessionOption interface {
	applySession(*sessionConfig)
}

// applySession lets any client Option be passed straight to Open.
func (o Option) applySession(c *sessionConfig) {
	c.clientOpts = append(c.clientOpts, o)
}

// sessionOptionFunc adapts a plain function to SessionOption.
type sessionOptionFunc func(*sessionConfig)

func (f sessionOptionFunc) applySession(c *sessionConfig) { f(c) }

type sessionConfig struct {
	clientOpts []Option
	threadOpts []protocol.ThreadStartParamsOption
	params     *protocol.ThreadStartParams
}

// WithClientOptions passes options through to the underlying Client.
//
// Every Client option works here: WithClientInfo, WithHandler, WithStderr,
// WithExperimentalAPI, and so on.
func WithClientOptions(opts ...Option) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) {
		c.clientOpts = append(c.clientOpts, opts...)
	})
}

// WithThreadOptions configures the thread Open starts.
//
//	codex.WithThreadOptions(
//	    protocol.WithThreadStartParamsCwd("/repo"),
//	    protocol.WithThreadStartParamsSandbox(protocol.SandboxModeReadOnly),
//	)
func WithThreadOptions(opts ...protocol.ThreadStartParamsOption) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) {
		c.threadOpts = append(c.threadOpts, opts...)
	})
}

// WithThreadParams sets the thread parameters wholesale, replacing anything
// WithThreadOptions contributed. Use it when you already have a params value.
func WithThreadParams(params protocol.ThreadStartParams) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) { c.params = &params })
}

// Open starts an app-server, completes the handshake, and starts one thread.
//
//	session, err := codex.Open(ctx,
//	    codex.WithClientInfo("my_product", "My Product", "1.0.0"),
//	    codex.WithThreadOptions(protocol.WithThreadStartParamsCwd("/repo")),
//	)
//
// Client options are accepted directly, since those are the ones most calls need.
// Thread configuration goes through WithThreadOptions or WithThreadParams.
//
// If starting the thread fails, the client is closed before returning, so a
// failed Open leaks no child process.
func Open(ctx context.Context, opts ...SessionOption) (*Session, error) {
	cfg := sessionConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt.applySession(&cfg)
		}
	}

	client, err := New(ctx, cfg.clientOpts...)
	if err != nil {
		return nil, err
	}

	params := protocol.NewThreadStartParams(cfg.threadOpts...)
	if cfg.params != nil {
		params = *cfg.params
	}

	thread, err := client.StartThread(ctx, params)
	if err != nil {
		// Do not leave a spawned server behind for a session that never opened.
		_ = client.Close()
		return nil, err
	}
	return &Session{client: client, thread: thread}, nil
}

// Client returns the underlying client, for the protocol methods Session does
// not expose.
func (s *Session) Client() *Client { return s.client }

// Thread returns the underlying thread handle.
func (s *Session) Thread() *Thread { return s.thread }

// ID returns the thread id.
func (s *Session) ID() string {
	if s.thread == nil {
		return ""
	}
	return s.thread.ID()
}

// Info returns the response that started the thread, including the resolved
// model, cwd, and sandbox policy.
func (s *Session) Info() *protocol.ThreadStartResponse {
	if s.thread == nil {
		return nil
	}
	return s.thread.Info()
}

// ServerInfo returns the server's initialize response.
func (s *Session) ServerInfo() protocol.InitializeResponse {
	return s.client.ServerInfo()
}

// Run starts a turn and blocks until it completes, with the same semantics as
// Thread.Run: streamed output arrives through the handler's callbacks while this
// is blocked, a failed turn returns a *TurnFailedError, and an interrupted turn
// returns no error.
func (s *Session) Run(ctx context.Context, params protocol.TurnStartParams) (*TurnResult, error) {
	if s.thread == nil {
		return nil, errors.New("codex: session has no thread")
	}
	return s.thread.Run(ctx, params)
}

// RunText is Run for the common case of a single text message.
func (s *Session) RunText(ctx context.Context, text string) (*TurnResult, error) {
	if s.thread == nil {
		return nil, errors.New("codex: session has no thread")
	}
	return s.thread.RunText(ctx, text)
}

// Steer appends user input to a specific turn, which must be the active one.
func (s *Session) Steer(ctx context.Context, turnID string, input ...*protocol.UserInput) (string, error) {
	return s.thread.Steer(ctx, turnID, input...)
}

// SteerText appends a text message to whichever turn is in flight.
//
// Run blocks, so call this from another goroutine, typically after an
// OnItemStarted or OnAgentMessageDelta callback showed you what the agent is
// doing. It fails if no turn is running.
func (s *Session) SteerText(ctx context.Context, text string) (string, error) {
	return s.thread.SteerText(ctx, text)
}

// SteerCurrent appends input to whichever turn is in flight.
func (s *Session) SteerCurrent(ctx context.Context, input ...*protocol.UserInput) (string, error) {
	return s.thread.SteerCurrent(ctx, input...)
}

// CurrentTurnID returns the id of the turn Run is blocked on, or "" when none is
// in flight.
func (s *Session) CurrentTurnID() string { return s.thread.CurrentTurnID() }

// Interrupt requests cancellation of a turn.
func (s *Session) Interrupt(ctx context.Context, turnID string) error {
	return s.thread.Interrupt(ctx, turnID)
}

// Compact triggers history compaction for the thread.
func (s *Session) Compact(ctx context.Context) error { return s.thread.Compact(ctx) }

// SetName sets the thread's user-facing name.
func (s *Session) SetName(ctx context.Context, name string) error {
	return s.thread.SetName(ctx, name)
}

// Done returns a channel closed when the connection ends, carrying the reason.
// Use it to notice a server that died on its own.
func (s *Session) Done() <-chan error { return s.client.Done() }

// Close shuts down the connection and the child process. It is idempotent.
func (s *Session) Close() error { return s.client.Close() }
