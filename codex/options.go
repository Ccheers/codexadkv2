package codex

import (
	"io"
	"log/slog"
	"time"

	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// Option configures a Client. Options are applied in order, so a later option
// overrides an earlier one.
//
// The zero configuration is usable: New() with no options spawns `codex
// app-server` from PATH, stays on the stable API surface, discards the child's
// stderr, and ignores every notification.
type Option func(*options)

// WithBinaryPath sets the codex executable to run. It is resolved through PATH
// unless it contains a separator. Defaults to "codex".
func WithBinaryPath(path string) Option {
	return func(o *options) { o.binaryPath = path }
}

// WithArgs replaces the arguments passed to the executable.
//
// The default is ["app-server"]. Override it to pass transport or auth flags,
// but note that this package only speaks the stdio transport, so keep
// --listen at its default.
func WithArgs(args ...string) Option {
	return func(o *options) { o.args = args }
}

// WithWorkingDir sets the child process's working directory.
//
// This is not the same as a thread's cwd: the agent's working directory is set
// per thread or per turn. This only affects the server process itself.
func WithWorkingDir(dir string) Option {
	return func(o *options) { o.dir = dir }
}

// WithEnv replaces the child's environment. Nil, the default, inherits the
// parent's environment.
func WithEnv(env []string) Option {
	return func(o *options) { o.env = env }
}

// WithStderr forwards the child's stderr to w.
//
// This is worth setting during development: app-server reports startup failures
// and panics only on stderr. A bounded tail is retained internally either way
// and attached to errors, but a full copy is more useful when debugging.
func WithStderr(w io.Writer) Option {
	return func(o *options) { o.stderr = w }
}

// WithClientInfo identifies your integration to the server.
//
// Set this. The name is what the OpenAI Compliance Logs Platform attributes
// activity to, and enterprise integrations need to be registered with OpenAI
// under a stable name.
func WithClientInfo(name, title, version string) Option {
	return func(o *options) {
		o.clientName = name
		o.clientTitle = title
		o.clientVersion = version
	}
}

// WithExperimentalAPI opts into experimental methods and fields.
//
// Off by default, deliberately: with it off the server rejects experimental
// members outright, which is the predictable behaviour. With it on, methods and
// fields marked EXPERIMENTAL in codex/protocol become usable, but they can
// change shape between codex releases.
func WithExperimentalAPI() Option {
	return func(o *options) { o.experimentalAPI = true }
}

// WithOptOutNotifications suppresses the named notification methods server-side
// for this connection.
//
// Matching is exact, and unknown names are accepted and ignored by the server.
// This is the efficient way to avoid high-volume traffic you do not consume:
// the delta streams (item/agentMessage/delta, item/reasoning/summaryTextDelta,
// item/reasoning/textDelta, item/commandExecution/outputDelta) dominate the
// wire on a busy turn.
//
// It is not derived automatically from which callbacks you registered, because
// initialize happens once per connection while handlers can be registered at
// any time, and a wrong guess silently stops callbacks from firing.
//
// New returns an error if you try to suppress a method Thread.Run depends on.
func WithOptOutNotifications(methods ...string) Option {
	return func(o *options) {
		o.optOutNotifications = append(o.optOutNotifications, methods...)
	}
}

// WithRequestAttestation opts into the server-initiated attestation/generate
// request. Only desktop hosts that can provide upstream attestation should set
// this, and they must also register Handler.OnAttestation.
func WithRequestAttestation() Option {
	return func(o *options) { o.requestAttestation = true }
}

// WithHandler registers notification and approval callbacks.
//
// Calling it more than once replaces the previous handler rather than merging,
// so build one Handler with every callback you need.
func WithHandler(h Handler) Option {
	return func(o *options) {
		copied := h
		o.handler = &copied
	}
}

// WithLogger sets the logger used for conditions the SDK must report but cannot
// return as an error: an auto-declined approval, a dropped notification, a
// malformed frame. Defaults to a logger that discards everything.
func WithLogger(l *slog.Logger) Option {
	return func(o *options) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithNotificationBuffer sets how many notifications may queue per thread
// before the connection applies backpressure. Defaults to 256.
//
// Notifications are never dropped on overflow; the read loop blocks instead.
// Dropping them would corrupt any message reassembled from deltas. A slow
// callback therefore slows the connection, which is the intended tradeoff.
func WithNotificationBuffer(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.notificationBuffer = n
		}
	}
}

// WithHandshakeTimeout bounds how long New waits for the initialize response.
// Defaults to 30s. Zero disables the timeout.
func WithHandshakeTimeout(d time.Duration) Option {
	return func(o *options) { o.handshakeTimeout = d }
}

// WithShutdownGrace sets how long Close waits after SIGTERM before escalating
// to SIGKILL. Defaults to 5s.
//
// Allow enough time for the server to persist thread state and stop its own
// subprocesses.
func WithShutdownGrace(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.shutdownGrace = d
		}
	}
}

// WithTransport connects to an already-running server instead of spawning one.
//
// The Client does not own the transport: Close closes the connection but does
// not terminate any process behind it. This is primarily for tests, which drive
// a scripted in-process server.
func WithTransport(t jsonrpc.Transport) Option {
	return func(o *options) { o.transport = t }
}
