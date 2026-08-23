// Package codex is a Go client for the Codex app-server JSON-RPC protocol.
//
// It spawns `codex app-server` as a child process, performs the initialize
// handshake, and exposes threads, turns, streamed items, and approval prompts.
//
// # Layers
//
// There are two, and they compose:
//
//   - The thin layer mirrors the wire exactly. Client.ThreadStart,
//     Client.TurnStart, and friends take and return the generated types in
//     codex/protocol, one method per call, nothing hidden.
//   - The ergonomic layer sits on top. Client.StartThread returns a Thread whose
//     Run method starts a turn and blocks until it completes, and whose RunStream
//     returns an ordered channel of events.
//
// Anything the thin layer does not wrap is still reachable through Client.Call.
//
// # Notifications
//
// Register callbacks with WithHandler. Every field is optional; an unset
// callback means "ignore that notification". OnUnhandled receives anything with
// no specific callback, including methods this build does not know, so nothing
// is silently lost.
//
// Callbacks for one thread run on that thread's own goroutine, in arrival
// order. Deltas therefore always arrive in order. A callback must not block
// indefinitely: the queue it drains is bounded, and a stalled callback
// eventually applies backpressure to the connection.
//
// # Approvals
//
// Approvals are server-initiated requests, and the turn does not proceed until
// the client answers. If no approval callback is registered, this package
// answers "decline" and logs loudly rather than letting the turn hang. For
// unattended use, set an approval policy of "never" together with a sandbox at
// thread start so approvals never fire.
package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"github.com/ccheers/codexadkv2/codex/protocol"
	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// Client is a connection to one app-server process.
//
// It is safe for concurrent use. A Client owns the child process unless it was
// given an external transport with WithTransport.
type Client struct {
	opts    options
	conn    *jsonrpc.Conn
	process *jsonrpc.Process

	// initResult is the server's initialize response, exposed via ServerInfo.
	initResult protocol.InitializeResponse

	dispatch *dispatcher

	serveErr  chan error
	closeOnce sync.Once
	closeErr  error

	// mainThread is the single thread this client drives. One client to one main
	// thread keeps handler routing unambiguous: a callback firing for this
	// client's own work is the common case, and anything else is a thread the
	// server created (a sub-agent, a review, a compaction) which the caller can
	// recognize by its threadId.
	mainMu      sync.Mutex
	mainThread  *Thread
	mainClaimed bool

	// fsMu guards fsWatchIDs, the file-system watches this client has open. They
	// are unwatched on Close.
	fsMu       sync.Mutex
	fsWatchIDs []string
}

// ErrMainThreadExists is returned by StartThread and ResumeThread when the client
// already drives a main thread.
//
// The SDK models one client per conversation. Sharing one client across several
// caller-driven threads would make every callback ambiguous about which
// conversation it belongs to, and would tie unrelated conversations to a shared
// notification queue and a shared process lifetime.
var ErrMainThreadExists = errors.New(
	"codex: this client already has a main thread; create another client for another conversation")

// claimMainThread reserves the main-thread slot before the request goes out, so
// two concurrent StartThread calls cannot both succeed.
func (c *Client) claimMainThread() error {
	c.mainMu.Lock()
	defer c.mainMu.Unlock()
	if c.mainClaimed {
		return ErrMainThreadExists
	}
	c.mainClaimed = true
	return nil
}

// releaseMainThread returns the slot after a failed start, so a transient error
// does not permanently disable the client.
func (c *Client) releaseMainThread() {
	c.mainMu.Lock()
	defer c.mainMu.Unlock()
	c.mainClaimed = false
	c.mainThread = nil
}

func (c *Client) setMainThread(t *Thread) {
	c.mainMu.Lock()
	defer c.mainMu.Unlock()
	c.mainThread = t
}

// MainThread returns the thread this client drives, or nil before StartThread or
// ResumeThread has succeeded.
//
// It saves threading a *Thread through call sites that already hold the client,
// which is common inside notification callbacks.
func (c *Client) MainThread() *Thread {
	c.mainMu.Lock()
	defer c.mainMu.Unlock()
	return c.mainThread
}

// New starts an app-server, completes the initialize handshake, and returns a
// ready Client.
//
// It fails fast: a missing executable, a child that dies immediately, or a
// rejected handshake is reported here rather than surfacing later as a
// confusing failure on the first real call.
//
// The caller must Close the Client to release the child process.
func New(ctx context.Context, opts ...Option) (*Client, error) {
	cfg := defaultOptions()
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	c := &Client{opts: cfg, serveErr: make(chan error, 1)}
	c.dispatch = newDispatcher(cfg.handler, cfg.logger, cfg.notificationBuffer)

	transport := cfg.transport
	if transport == nil {
		proc, err := jsonrpc.StartProcess(jsonrpc.ProcessConfig{
			Path:          cfg.binaryPath,
			Args:          cfg.args,
			Dir:           cfg.dir,
			Env:           cfg.env,
			Stderr:        cfg.stderr,
			ShutdownGrace: cfg.shutdownGrace,
			KillGrace:     cfg.killGrace,
		})
		if err != nil {
			return nil, err
		}
		c.process = proc
		transport = proc.Transport()
	}

	c.conn = jsonrpc.NewConn(transport, c.dispatch)
	c.dispatch.conn = c.conn

	go func() { c.serveErr <- c.conn.Serve() }()

	if err := c.handshake(ctx); err != nil {
		// Tear the child down rather than leaking a process that will never be
		// spoken to.
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// handshake performs the initialize request and the initialized notification.
// The server rejects every other method until both have completed.
func (c *Client) handshake(ctx context.Context) error {
	if c.opts.handshakeTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.opts.handshakeTimeout)
		defer cancel()
	}

	params := protocol.InitializeParams{
		ClientInfo: &protocol.ClientInfo{
			Name:    c.opts.clientName,
			Title:   &c.opts.clientTitle,
			Version: c.opts.clientVersion,
		},
	}
	if caps := c.opts.capabilities(); caps != nil {
		params.Capabilities = caps
	}

	if err := c.conn.Call(ctx, protocol.MethodInitialize, params, &c.initResult); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("codex: initialize timed out after %s: %w%s",
				c.opts.handshakeTimeout, err, c.stderrHint())
		}
		return fmt.Errorf("codex: initialize failed: %w%s", err, c.stderrHint())
	}
	if err := c.conn.Notify(methodInitialized, struct{}{}); err != nil {
		return fmt.Errorf("codex: sending initialized: %w", err)
	}
	return nil
}

// stderrHint appends the child's stderr tail to an error message. app-server
// reports startup problems only on stderr, so without this a handshake failure
// has no explanation.
func (c *Client) stderrHint() string {
	if c.process == nil {
		return ""
	}
	if tail := c.process.StderrTail(); tail != "" {
		return "\ncodex app-server stderr:\n" + tail
	}
	return ""
}

// ServerInfo returns the server's initialize response: its user agent, its
// CODEX_HOME, and the platform it is running on.
func (c *Client) ServerInfo() protocol.InitializeResponse { return c.initResult }

// SchemaVersion returns the codex-cli version whose schema generated the types
// in this build. It is informational: the client does not require the server to
// match, because a newer server's unknown methods, fields, and enum values all
// decode without error.
func (c *Client) SchemaVersion() string { return protocol.SchemaVersion }

// Call sends any request and decodes its result, for methods the typed API does
// not wrap. Prefer the typed methods where they exist.
//
//	var out protocol.PluginListResponse
//	err := client.Call(ctx, "plugin/list", protocol.PluginListParams{}, &out)
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.conn.Call(ctx, method, params, result)
}

// Notify sends a one-way notification.
func (c *Client) Notify(method string, params any) error {
	return c.conn.Notify(method, params)
}

// Close shuts down the connection and the child process.
//
// In-flight calls fail with an error wrapping jsonrpc.ErrConnClosed. Close is
// idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		var errs []error
		c.unwatchAll()
		if c.conn != nil {
			if err := c.conn.Close(); err != nil && !errors.Is(err, io.ErrClosedPipe) {
				errs = append(errs, err)
			}
		}
		if c.process != nil {
			if err := c.process.Close(); err != nil {
				errs = append(errs, err)
			}
		}
		// Draining the dispatcher after the transport is closed guarantees no
		// callback runs once Close returns.
		if c.dispatch != nil {
			c.dispatch.shutdown()
		}
		c.closeErr = errors.Join(errs...)
	})
	return c.closeErr
}

// unwatchAll best-effort closes every open fs/watch subscription before the
// connection is torn down. Errors are ignored: the connection is closing anyway.
func (c *Client) unwatchAll() {
	c.fsMu.Lock()
	ids := append([]string(nil), c.fsWatchIDs...)
	c.fsWatchIDs = nil
	c.fsMu.Unlock()

	if len(ids) == 0 || c.conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, id := range ids {
		_ = c.FSUnwatch(ctx, protocol.FsUnwatchParams{WatchID: id})
	}
}

// Done returns a channel that is closed when the connection ends, carrying the
// reason. Use it to notice a server that died on its own.
func (c *Client) Done() <-chan error { return c.serveErr }

// options holds everything configurable about a Client.
type options struct {
	binaryPath string
	args       []string
	dir        string
	env        []string
	stderr     io.Writer

	clientName    string
	clientTitle   string
	clientVersion string

	experimentalAPI     bool
	optOutNotifications []string
	requestAttestation  bool

	handler *Handler
	logger  *slog.Logger

	notificationBuffer int
	handshakeTimeout   time.Duration
	shutdownGrace      time.Duration
	killGrace          time.Duration

	transport jsonrpc.Transport
}

func defaultOptions() options {
	binaryPath := "codex"
	if runtime.GOOS == "windows" {
		binaryPath = "codex.exe"
	}
	return options{
		binaryPath: binaryPath,
		args:       []string{"app-server"},

		// A client name is required for the server to attribute usage. The
		// default is deliberately generic; callers should set their own.
		clientName:    "codex-go-sdk",
		clientTitle:   "Codex Go SDK",
		clientVersion: "0.1.0",

		handler: &Handler{},
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),

		notificationBuffer: 1024,
		handshakeTimeout:   30 * time.Second,
		shutdownGrace:      5 * time.Second,
		killGrace:          2 * time.Second,
	}
}

func (o *options) validate() error {
	if o.clientName == "" {
		return errors.New("codex: client name must not be empty; set it with WithClientInfo")
	}
	for _, method := range o.optOutNotifications {
		if requiredNotifications[method] {
			return fmt.Errorf(
				"codex: cannot opt out of %q: Thread.Run and Thread.RunStream depend on it "+
					"to detect turn completion. Opting out would make them hang", method)
		}
	}
	return nil
}

// capabilities builds the initialize capabilities, or nil when everything is at
// its default. Sending an empty object is harmless but noisier in logs.
func (o *options) capabilities() *protocol.InitializeCapabilities {
	if !o.experimentalAPI && len(o.optOutNotifications) == 0 && !o.requestAttestation {
		return nil
	}
	caps := &protocol.InitializeCapabilities{}
	if o.experimentalAPI {
		caps.ExperimentalAPI = &o.experimentalAPI
	}
	if o.requestAttestation {
		caps.RequestAttestation = &o.requestAttestation
	}
	if len(o.optOutNotifications) > 0 {
		caps.OptOutNotificationMethods = o.optOutNotifications
	}
	return caps
}

// requiredNotifications are methods the ergonomic layer consumes internally.
// Suppressing them server-side would make Thread.Run wait forever, so
// WithOptOutNotifications rejects them.
var requiredNotifications = map[string]bool{
	protocol.NotifyTurnStarted:   true,
	protocol.NotifyTurnCompleted: true,
	protocol.NotifyThreadStarted: true,
	protocol.NotifyError:         true,
}

// methodInitialized is the handshake acknowledgement. It is a client
// notification rather than a request, so it has no generated method constant.
const methodInitialized = "initialized"

// rawJSON is a tiny helper for building params without a generated type.
func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}
