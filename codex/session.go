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

	// resumeThreadID 非空即走 Resume（恢复已存线程），否则走 Open（新建）。
	// 由 WithResumeThreadID 设置；Open 与 Resume 互斥使用，同一 sessionConfig 不复用。
	resumeThreadID string
	// resumeParams 是 Resume 时的线程参数（含 config overlay/systemPrompt/model）。
	// 缺省用 protocol.NewThreadResumeParams(resumeThreadID) 的最小值（只带 thread id）。
	resumeParams *protocol.ThreadResumeParams

	tools      []DynamicTool
	toolGroups []ToolGroup
}

// WithResumeThreadID 让 Session 按已存的 thread id 恢复线程，而不是新建。
//
//	codex.Resume(ctx,
//	    codex.WithClientInfo("my_product", "My Product", "1.0.0"),
//	    codex.WithResumeThreadID("thr_123"),
//	)
//
// resume 的线程从 rollout 恢复历史与 dynamic tool spec（ADR-0016），因此 handler 的
// dispatch 也会按 Open 同一套装配注册工具。缺 thread id 时 Resume 直接失败，不会发出
// 一次空 thread/resume 请求。
func WithResumeThreadID(threadID string) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) { c.resumeThreadID = threadID })
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

// WithTools exposes tools to the model for the life of the session.
//
// The session builds the thread's dynamicTools spec, answers tool calls by
// running the matching tool, and returns its result to the model. Nothing else is
// required: there is no separate callback to register and no dispatch to write.
//
//	session, err := codex.Open(ctx,
//	    codex.WithTools(grepTool, readFileTool),
//	)
//
// Dynamic tools are an experimental app-server feature, so Open enables the
// experimental capability automatically when any tool is registered. The server
// rejects dynamicTools without it, and requiring callers to remember
// WithExperimentalAPI would turn that into a confusing runtime error.
//
// These become top-level tools. Use WithToolGroups to nest related tools under a
// shared name.
func WithTools(tools ...DynamicTool) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) {
		c.tools = append(c.tools, tools...)
	})
}

// WithToolGroups exposes groups of related tools to the model.
//
// A group is a progressive-disclosure step: the model reads the group's
// description first and only looks at the tools inside if the area seems
// relevant. That keeps a large tool surface from crowding out the conversation.
//
//	codex.WithToolGroups(codex.ToolGroup{
//	    Name:        "db",
//	    Description: "Inspect and query the application database",
//	    Tools:       []codex.DynamicTool{queryTool, schemaTool},
//	})
//
// Like WithTools, this enables the experimental capability automatically.
func WithToolGroups(groups ...ToolGroup) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) {
		c.toolGroups = append(c.toolGroups, groups...)
	})
}

// newClient 建 codex client 并装配动态工具。Open 与 Resume 共用这一步骤：两者都要
// spawn 子进程 + 握手 + 注册 handler 的 dispatch（tool call 由 SDK 内部派发），区别只在
// 最后一步是 StartThread 还是 ResumeThread。
//
// 返回 (client, hasTools)：hasTools 标记是否注册了工具，供调用方决定是否下发 DynamicTools。
func newClient(ctx context.Context, cfg *sessionConfig) (*Client, bool, error) {
	// Validate the tools before spawning anything: a duplicate or undescribed tool
	// is a programming error, and reporting it without having started a server
	// first keeps the failure cheap and the message clear.
	registry, err := buildToolRegistry(cfg.tools, cfg.toolGroups)
	if err != nil {
		return nil, false, err
	}
	hasTools := len(registry.specs) > 0

	clientOpts := cfg.clientOpts
	if hasTools {
		// dynamicTools is experimental-gated, and the server rejects it outright
		// without the capability. Enabling it here beats making the caller pair two
		// options correctly to avoid a confusing rejection.
		clientOpts = append(clientOpts, WithExperimentalAPI())
	}

	client, err := New(ctx, clientOpts...)
	if err != nil {
		return nil, hasTools, err
	}
	if hasTools {
		client.dispatch.tools = registry
		// Tool calls run under the session's context, so closing the session
		// cancels work still inside a tool.
		client.dispatch.setSessionContext(ctx)
	}
	return client, hasTools, nil
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
// failed Open leaks no child process. Open 总是新建线程；要恢复已存线程用 Resume。
func Open(ctx context.Context, opts ...SessionOption) (*Session, error) {
	cfg := sessionConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt.applySession(&cfg)
		}
	}

	client, hasTools, err := newClient(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	threadOpts := cfg.threadOpts
	if hasTools {
		threadOpts = append(threadOpts, protocol.WithThreadStartParamsDynamicTools(client.dispatch.tools.specs))
	}
	params := protocol.NewThreadStartParams(threadOpts...)
	if cfg.params != nil {
		params = *cfg.params
	}
	if instr := deferLoadingInstructions(cfg.toolGroups); instr != "" {
		// Defer-loaded groups' tool schemas are withheld from thread/start, so the
		// developer instructions must carry the map of what exists and how to
		// address it. 追加到调用方已有的提示词之后，而不是覆盖。
		params.DeveloperInstructions = appendDeveloperInstructions(params.DeveloperInstructions, instr)
	}

	thread, err := client.StartThread(ctx, params)
	if err != nil {
		// Do not leave a spawned server behind for a session that never opened.
		_ = client.Close()
		return nil, err
	}
	return &Session{client: client, thread: thread}, nil
}

// Resume starts an app-server, completes the handshake, and reopens a stored
// thread as the session's thread.
//
//	session, err := codex.Resume(ctx,
//	    codex.WithClientInfo("my_product", "My Product", "1.0.0"),
//	    codex.WithResumeThreadID("thr_123"),
//	    codex.WithResumeThreadParams(protocol.ThreadResumeParams{
//	        DeveloperInstructions: protocol.Ptr("system prompt"),
//	        Config:               overlay,
//	    }),
//	)
//
// Client options are accepted directly, same as Open. Thread configuration goes
// through WithResumeThreadID and WithResumeThreadParams; the params are what a
// caller would pass to Thread.start's resume sibling. A resumed thread recovers
// its history and dynamic tool spec from the rollout, so tool dispatch is
// registered exactly as Open would — the handler still answers joey tools.
//
// resumeThreadID 缺省时直接失败，不会发出一次空 thread/resume 请求。续接失败时把
// thread/resume 的原始错误（含 jsonrpc *Error）上抛，不做「可恢复/不可恢复」分类——
// 分类交由调用方（worker 的降级策略：一律降级，靠 start 自证连接死活）。
func Resume(ctx context.Context, opts ...SessionOption) (*Session, error) {
	cfg := sessionConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt.applySession(&cfg)
		}
	}
	if cfg.resumeThreadID == "" {
		return nil, errors.New("codex: resume requires WithResumeThreadID")
	}

	client, _, err := newClient(ctx, &cfg)
	if err != nil {
		return nil, err
	}

	params := protocol.NewThreadResumeParams(cfg.resumeThreadID)
	if cfg.resumeParams != nil {
		params = *cfg.resumeParams
		params.ThreadID = cfg.resumeThreadID // 以 option 为准，拼进来的 params 不许覆盖 id
	}
	if instr := deferLoadingInstructions(cfg.toolGroups); instr != "" {
		// Defer 提示词不在 rollout 里，恢复线程时需要随 resume 重新下发；追加到调用方
		// 已有的提示词之后，而不是覆盖。
		params.DeveloperInstructions = appendDeveloperInstructions(params.DeveloperInstructions, instr)
	}

	thread, err := client.ResumeThread(ctx, params)
	if err != nil {
		// Do not leave a spawned server behind for a session that never opened.
		_ = client.Close()
		return nil, err
	}
	return &Session{client: client, thread: thread}, nil
}

// WithResumeThreadParams sets the thread parameters for Resume wholesale,
// replacing anything the resume would otherwise default to. Use it when you have
// a resume params value; In particular the config overlay (MCP / model_providers /
// reasoning effort) must be carried here, since a resumed thread recovers from its
// rollout and would otherwise run a stale config.
func WithResumeThreadParams(params protocol.ThreadResumeParams) SessionOption {
	return sessionOptionFunc(func(c *sessionConfig) { c.resumeParams = &params })
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
