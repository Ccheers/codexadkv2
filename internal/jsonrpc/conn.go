package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Transport is a bidirectional stream of JSON-RPC frames. stdio is the only
// implementation that ships, but the interface keeps the WebSocket and Unix
// socket transports additive: framing belongs to the transport, since the
// stdio transport is newline-delimited while a WebSocket carries one message
// per frame.
type Transport interface {
	// Send writes one frame. Implementations must be safe for concurrent use.
	Send(data []byte) error

	// Recv returns the next frame, blocking until one arrives. It returns
	// io.EOF when the peer closes cleanly.
	Recv() ([]byte, error)

	// Close releases the transport. Pending Recv calls must unblock.
	Close() error
}

// Handler receives the server-initiated traffic that is not a response to one of
// our requests. Both methods are called from the connection's read loop, so
// neither may block for long: doing so stalls response delivery too.
type Handler interface {
	// HandleNotification is called for every one-way notification.
	HandleNotification(method string, params json.RawMessage, emittedAtMs *int64)

	// HandleRequest is called for every server-initiated request. The
	// implementation MUST eventually call respond exactly once, or the server
	// waits forever. Note that Codex blocks the turn on approval requests.
	HandleRequest(id ID, method string, params json.RawMessage, respond func(result any, err *Error))
}

// Conn multiplexes requests, responses, and notifications over one Transport.
type Conn struct {
	transport Transport
	handler   Handler

	nextID atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan *Message
	closed   bool
	closeErr error

	done chan struct{}
	once sync.Once
}

// NewConn returns a Conn reading from t and dispatching server traffic to h.
// Call Serve to start the read loop.
func NewConn(t Transport, h Handler) *Conn {
	return &Conn{
		transport: t,
		handler:   h,
		pending:   make(map[string]chan *Message),
		done:      make(chan struct{}),
	}
}

// Serve runs the read loop until the transport fails or Close is called. It
// returns the error that ended the loop, or nil on a clean shutdown.
//
// All decoding happens here, but no user callback does any real work on this
// goroutine: the Handler is expected to hand off to its own queues so that a
// slow consumer cannot stall response delivery.
func (c *Conn) Serve() error {
	for {
		data, err := c.transport.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			c.shutdown(err)
			return err
		}
		if len(data) == 0 {
			continue
		}

		var msg Message
		if uerr := json.Unmarshal(data, &msg); uerr != nil {
			// A frame we cannot parse is reported but does not kill the
			// connection: a newer server may send something unexpected, and
			// dropping the whole session over one bad frame is worse.
			c.notifyMalformed(data, uerr)
			continue
		}

		switch {
		case msg.IsResponse():
			c.deliver(&msg)
		case msg.IsRequest():
			c.dispatchRequest(&msg)
		case msg.IsNotification():
			c.handler.HandleNotification(msg.Method, msg.Params, msg.EmittedAtMs)
		default:
			c.notifyMalformed(data, errors.New("frame has neither id nor method"))
		}
	}
}

// Call sends a request and waits for its response.
//
// If ctx is cancelled, Call returns ctx.Err() and stops waiting, but the server
// may still be working: cancelling a Call does not cancel the server-side
// operation. Callers that need real cancellation must send the protocol's own
// cancellation request, such as turn/interrupt.
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	id := NumberID(c.nextID.Add(1))

	frame := struct {
		ID     ID     `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{ID: id, Method: method, Params: params}

	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("jsonrpc: encoding %s: %w", method, err)
	}

	replies := make(chan *Message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.closedError()
	}
	c.pending[id.String()] = replies
	c.mu.Unlock()

	// Always clear the pending entry, so an abandoned Call cannot leak.
	defer func() {
		c.mu.Lock()
		delete(c.pending, id.String())
		c.mu.Unlock()
	}()

	if err := c.transport.Send(data); err != nil {
		return fmt.Errorf("jsonrpc: sending %s: %w", method, err)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return c.closedError()
	case reply := <-replies:
		if reply.Error != nil {
			return reply.Error
		}
		if result == nil || len(reply.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(reply.Result, result); err != nil {
			return fmt.Errorf("jsonrpc: decoding %s result: %w", method, err)
		}
		return nil
	}
}

// Notify sends a one-way notification. It does not wait for anything.
func (c *Conn) Notify(method string, params any) error {
	frame := struct {
		Method string `json:"method"`
		Params any    `json:"params,omitempty"`
	}{Method: method, Params: params}

	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("jsonrpc: encoding %s: %w", method, err)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.closedError()
	}
	return c.transport.Send(data)
}

// deliver routes a response to whichever Call is waiting for it. A response with
// no waiter is dropped: it can only mean the Call was abandoned.
func (c *Conn) deliver(msg *Message) {
	c.mu.Lock()
	ch, ok := c.pending[msg.ID.String()]
	if ok {
		delete(c.pending, msg.ID.String())
	}
	c.mu.Unlock()
	if ok {
		ch <- msg
	}
}

// dispatchRequest hands a server-initiated request to the Handler along with a
// respond function that is safe to call once, from any goroutine, at any later
// time. Approvals routinely take as long as a human takes to click.
func (c *Conn) dispatchRequest(msg *Message) {
	id := *msg.ID
	var once sync.Once

	respond := func(result any, rpcErr *Error) {
		once.Do(func() {
			frame := struct {
				ID     ID     `json:"id"`
				Result any    `json:"result,omitempty"`
				Error  *Error `json:"error,omitempty"`
			}{ID: id}
			if rpcErr != nil {
				frame.Error = rpcErr
			} else {
				frame.Result = result
			}
			data, err := json.Marshal(frame)
			if err != nil {
				// Falling back to an error response is better than never
				// answering, which would hang the turn.
				data, _ = json.Marshal(struct {
					ID    ID     `json:"id"`
					Error *Error `json:"error"`
				}{ID: id, Error: &Error{
					Code:    CodeInternalError,
					Message: fmt.Sprintf("client failed to encode its response: %v", err),
				}})
			}
			_ = c.transport.Send(data)
		})
	}

	c.handler.HandleRequest(id, msg.Method, msg.Params, respond)
}

func (c *Conn) notifyMalformed(data []byte, err error) {
	c.handler.HandleNotification("$/malformed", json.RawMessage(mustQuote(fmt.Sprintf(
		"%v: %s", err, truncate(string(data), 512)))), nil)
}

// Close shuts the connection down and fails every in-flight Call.
func (c *Conn) Close() error {
	err := c.transport.Close()
	c.shutdown(nil)
	return err
}

// shutdown marks the connection closed and unblocks all waiters exactly once.
func (c *Conn) shutdown(cause error) {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeErr = cause
		waiters := c.pending
		c.pending = make(map[string]chan *Message)
		c.mu.Unlock()

		close(c.done)
		// Waiters observe c.done and return closedError, so the channels need
		// no further signalling; clearing the map releases them for GC.
		_ = waiters
	})
}

func (c *Conn) closedError() error {
	c.mu.Lock()
	cause := c.closeErr
	c.mu.Unlock()
	if cause != nil {
		return fmt.Errorf("jsonrpc: connection closed: %w", cause)
	}
	return ErrConnClosed
}

// ErrConnClosed is returned by Call and Notify after the connection has shut
// down cleanly.
var ErrConnClosed = errors.New("jsonrpc: connection closed")

// streamTransport is the newline-delimited JSON framing used by stdio.
type streamTransport struct {
	r *bufio.Reader
	w io.Writer
	c io.Closer

	writeMu sync.Mutex
}

// NewStreamTransport returns a Transport over a newline-delimited JSON stream.
func NewStreamTransport(r io.Reader, w io.Writer, closer io.Closer) Transport {
	return &streamTransport{
		// app-server can emit very large frames: a completed turn carries every
		// item, and aggregated command output is not truncated. bufio's default
		// 64KiB limit would reject those, so start larger and let ReadBytes grow
		// past it as needed.
		r: bufio.NewReaderSize(r, 256*1024),
		w: w,
		c: closer,
	}
}

func (t *streamTransport) Send(data []byte) error {
	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	// One write per frame keeps frames from interleaving under concurrency.
	buf := make([]byte, 0, len(data)+1)
	buf = append(buf, data...)
	buf = append(buf, '\n')
	if _, err := t.w.Write(buf); err != nil {
		return err
	}
	if f, ok := t.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

func (t *streamTransport) Recv() ([]byte, error) {
	for {
		line, err := t.r.ReadBytes('\n')
		if err != nil {
			// A final line without a trailing newline is still a valid frame.
			if len(trimSpace(line)) > 0 && errors.Is(err, io.EOF) {
				return trimSpace(line), nil
			}
			return nil, err
		}
		if line = trimSpace(line); len(line) > 0 {
			return line, nil
		}
		// Skip blank lines rather than surfacing empty frames.
	}
}

func (t *streamTransport) Close() error {
	if t.c == nil {
		return nil
	}
	return t.c.Close()
}

func trimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func mustQuote(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		return []byte(`""`)
	}
	return b
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
