package codex_test

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeServer is a scripted in-process app-server. It implements
// jsonrpc.Transport, so a Client drives it exactly as it would drive a real
// child process, without spawning anything.
//
// It is deliberately dumb: a test registers a responder per method and pushes
// notifications explicitly. Making it simulate real agent behaviour would test
// the simulation rather than the SDK.
type fakeServer struct {
	t *testing.T

	mu         sync.Mutex
	responders map[string]responder
	requests   []recordedCall
	closed     bool

	outbound chan []byte // server -> client
	inbound  chan []byte // client -> server, for assertions

	done chan struct{}
}

// responder produces a result (or an error object) for one method.
type responder func(id json.RawMessage, params json.RawMessage) (result any, rpcErr map[string]any)

type recordedCall struct {
	Method string
	Params json.RawMessage
}

func newFakeServer(t *testing.T) *fakeServer {
	s := &fakeServer{
		t:          t,
		responders: make(map[string]responder),
		outbound:   make(chan []byte, 256),
		inbound:    make(chan []byte, 256),
		done:       make(chan struct{}),
	}
	// initialize and initialized must always work: the Client refuses to
	// construct without a successful handshake.
	s.handle("initialize", func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		return map[string]any{
			"userAgent":      "codex-fake/1.0",
			"codexHome":      "/tmp/codex-home",
			"platformFamily": "unix",
			"platformOs":     "linux",
		}, nil
	})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// handle registers a responder for a method.
func (s *fakeServer) handle(method string, fn responder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responders[method] = fn
}

// reply registers a static result for a method.
func (s *fakeServer) reply(method string, result any) {
	s.handle(method, func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		return result, nil
	})
}

// replyError registers a JSON-RPC error response for a method.
func (s *fakeServer) replyError(method string, code int, message string) {
	s.handle(method, func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		return nil, map[string]any{"code": code, "message": message}
	})
}

// notify pushes a notification to the client.
func (s *fakeServer) notify(method string, params any) {
	s.push(map[string]any{"method": method, "params": params})
}

// request pushes a server-initiated request, which the client must answer.
func (s *fakeServer) request(id int, method string, params any) {
	s.push(map[string]any{"id": id, "method": method, "params": params})
}

func (s *fakeServer) push(frame map[string]any) {
	data, err := json.Marshal(frame)
	if err != nil {
		s.t.Fatalf("fakeServer: encoding frame: %v", err)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return
	}
	select {
	case s.outbound <- data:
	case <-s.done:
	case <-time.After(2 * time.Second):
		s.t.Error("fakeServer: timed out pushing a frame to the client")
	}
}

// --- jsonrpc.Transport ---

// Send receives a frame from the client, answers it if a responder is
// registered, and records it for assertions.
func (s *fakeServer) Send(data []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return io.ErrClosedPipe
	}
	s.mu.Unlock()

	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("fakeServer: client sent invalid JSON: %w", err)
	}

	select {
	case s.inbound <- append([]byte(nil), data...):
	default:
	}

	// A frame with an id but no method is the client answering one of our
	// requests, not a request of its own.
	if msg.Method == "" {
		return nil
	}

	s.mu.Lock()
	s.requests = append(s.requests, recordedCall{Method: msg.Method, Params: msg.Params})
	fn, ok := s.responders[msg.Method]
	s.mu.Unlock()

	// Notifications carry no id and need no answer.
	if len(msg.ID) == 0 {
		return nil
	}

	if !ok {
		s.push(map[string]any{
			"id": msg.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "fakeServer has no responder for " + msg.Method,
			},
		})
		return nil
	}

	result, rpcErr := fn(msg.ID, msg.Params)
	frame := map[string]any{"id": msg.ID}
	if rpcErr != nil {
		frame["error"] = rpcErr
	} else {
		frame["result"] = result
	}
	s.push(frame)
	return nil
}

func (s *fakeServer) Recv() ([]byte, error) {
	select {
	case data, ok := <-s.outbound:
		if !ok {
			return nil, io.EOF
		}
		return data, nil
	case <-s.done:
		return nil, io.EOF
	}
}

func (s *fakeServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.done)
	}
	return nil
}

// --- assertions ---

// calledMethods returns the methods the client sent, in order.
func (s *fakeServer) calledMethods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.requests))
	for i, r := range s.requests {
		out[i] = r.Method
	}
	return out
}

// paramsFor returns the params of the first call to method.
func (s *fakeServer) paramsFor(method string) json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.Method == method {
			return r.Params
		}
	}
	return nil
}

// waitForCall blocks until the client has called method.
func (s *fakeServer) waitForCall(method string) {
	s.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range s.calledMethods() {
			if m == method {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	s.t.Fatalf("fakeServer: the client never called %q (it called %v)", method, s.calledMethods())
}

// nextClientFrame returns the next frame the client sent, decoded.
func (s *fakeServer) nextClientFrame() map[string]any {
	s.t.Helper()
	select {
	case data := <-s.inbound:
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			s.t.Fatalf("fakeServer: client frame is not JSON: %v", err)
		}
		return out
	case <-time.After(2 * time.Second):
		s.t.Fatal("fakeServer: timed out waiting for a frame from the client")
		return nil
	}
}

// waitForClientResponse returns the next frame that answers a server request,
// skipping the client's own requests and notifications.
func (s *fakeServer) waitForClientResponse() map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frame := s.nextClientFrame()
		if _, hasMethod := frame["method"]; hasMethod {
			continue // a request or notification from the client
		}
		if _, hasID := frame["id"]; hasID {
			return frame
		}
	}
	s.t.Fatal("fakeServer: the client never answered the server request")
	return nil
}
