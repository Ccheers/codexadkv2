package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTransport is an in-process Transport driven by channels, so the tests
// exercise framing and correlation without spawning anything.
type fakeTransport struct {
	sent     chan []byte
	incoming chan []byte

	mu     sync.Mutex
	closed bool
}

func newFakeTransport() *fakeTransport {
	return &fakeTransport{
		sent:     make(chan []byte, 64),
		incoming: make(chan []byte, 64),
	}
}

func (f *fakeTransport) Send(data []byte) error {
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return io.ErrClosedPipe
	}
	f.sent <- append([]byte(nil), data...)
	return nil
}

func (f *fakeTransport) Recv() ([]byte, error) {
	data, ok := <-f.incoming
	if !ok {
		return nil, io.EOF
	}
	return data, nil
}

func (f *fakeTransport) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.incoming)
	}
	return nil
}

// push queues a frame as though the server sent it.
func (f *fakeTransport) push(t *testing.T, frame string) {
	t.Helper()
	select {
	case f.incoming <- []byte(frame):
	case <-time.After(time.Second):
		t.Fatal("timed out queueing an inbound frame")
	}
}

// nextSent returns the next frame the Conn wrote.
func (f *fakeTransport) nextSent(t *testing.T) map[string]any {
	t.Helper()
	select {
	case data := <-f.sent:
		var out map[string]any
		if err := json.Unmarshal(data, &out); err != nil {
			t.Fatalf("sent frame is not JSON: %v (%s)", err, data)
		}
		return out
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an outbound frame")
		return nil
	}
}

// recordingHandler captures dispatched server traffic.
type recordingHandler struct {
	mu            sync.Mutex
	notifications []recordedNotification
	requests      []recordedRequest

	// onRequest, when set, is invoked for each server request so a test can
	// answer it.
	onRequest func(id ID, method string, params json.RawMessage, respond func(any, *Error))
}

type recordedNotification struct {
	Method string
	Params json.RawMessage
}

type recordedRequest struct {
	ID     ID
	Method string
}

func (h *recordingHandler) HandleNotification(method string, params json.RawMessage, _ *int64) {
	h.mu.Lock()
	h.notifications = append(h.notifications, recordedNotification{method, params})
	h.mu.Unlock()
}

func (h *recordingHandler) HandleRequest(id ID, method string, params json.RawMessage, respond func(any, *Error)) {
	h.mu.Lock()
	h.requests = append(h.requests, recordedRequest{id, method})
	cb := h.onRequest
	h.mu.Unlock()
	if cb != nil {
		cb(id, method, params, respond)
	}
}

func (h *recordingHandler) notificationMethods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, len(h.notifications))
	for i, n := range h.notifications {
		out[i] = n.Method
	}
	return out
}

// newTestConn wires a Conn to a fake transport and runs its read loop.
func newTestConn(t *testing.T, h Handler) (*Conn, *fakeTransport) {
	t.Helper()
	ft := newFakeTransport()
	conn := NewConn(ft, h)
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = conn.Serve()
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-served:
		case <-time.After(2 * time.Second):
			t.Error("Serve did not return after Close")
		}
	})
	return conn, ft
}

func TestCallCorrelatesResponse(t *testing.T) {
	conn, ft := newTestConn(t, &recordingHandler{})

	type result struct {
		Thread struct{ ID string } `json:"thread"`
	}
	var got result
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Call(context.Background(), "thread/start", map[string]any{"model": "m"}, &got)
	}()

	sent := ft.nextSent(t)
	if sent["method"] != "thread/start" {
		t.Fatalf("method = %v, want thread/start", sent["method"])
	}
	id, ok := sent["id"].(float64)
	if !ok {
		t.Fatalf("id = %v (%T), want a number", sent["id"], sent["id"])
	}

	ft.push(t, `{"id":`+jsonNumber(id)+`,"result":{"thread":{"id":"thr_1"}}}`)

	if err := <-errCh; err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got.Thread.ID != "thr_1" {
		t.Errorf("thread id = %q, want thr_1", got.Thread.ID)
	}
}

// TestConcurrentCallsCorrelateIndependently checks that out-of-order responses
// still reach the right caller, which is the whole point of id correlation.
func TestConcurrentCallsCorrelateIndependently(t *testing.T) {
	conn, ft := newTestConn(t, &recordingHandler{})

	const n = 8
	type reply struct{ Value int }
	results := make([]int, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r reply
			if err := conn.Call(context.Background(), "test/echo", map[string]int{"i": i}, &r); err != nil {
				errs <- err
				return
			}
			results[i] = r.Value
		}(i)
	}

	// Collect the ids, then answer in reverse order.
	ids := make([]float64, 0, n)
	params := make(map[float64]int, n)
	for i := 0; i < n; i++ {
		sent := ft.nextSent(t)
		id := sent["id"].(float64)
		ids = append(ids, id)
		p := sent["params"].(map[string]any)
		params[id] = int(p["i"].(float64))
	}
	for i := len(ids) - 1; i >= 0; i-- {
		id := ids[i]
		// Echo back a value derived from the request, so a mis-correlated
		// response is detectable.
		ft.push(t, `{"id":`+jsonNumber(id)+`,"result":{"Value":`+itoa(params[id]*100)+`}}`)
	}

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("Call: %v", err)
	default:
	}
	for i := 0; i < n; i++ {
		if results[i] != i*100 {
			t.Errorf("call %d got %d, want %d: responses were mis-correlated", i, results[i], i*100)
		}
	}
}

func TestCallReturnsTypedRPCError(t *testing.T) {
	conn, ft := newTestConn(t, &recordingHandler{})

	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Call(context.Background(), "thread/start", nil, nil)
	}()

	sent := ft.nextSent(t)
	id := sent["id"].(float64)
	ft.push(t, `{"id":`+jsonNumber(id)+`,"error":{"code":-32001,"message":"Server overloaded; retry later.","data":{"retryAfterMs":250}}}`)

	err := <-errCh
	if err == nil {
		t.Fatal("Call succeeded, want an error")
	}

	// The sentinel must match, so callers can branch without string matching.
	if !errors.Is(err, ErrServerOverloaded) {
		t.Errorf("errors.Is(err, ErrServerOverloaded) = false for %v", err)
	}

	// And the structured detail must survive, which is what makes the sentinel
	// useful rather than lossy.
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("errors.As did not yield *Error for %v", err)
	}
	if rpcErr.Code != CodeServerOverloaded {
		t.Errorf("Code = %d, want %d", rpcErr.Code, CodeServerOverloaded)
	}
	if !strings.Contains(string(rpcErr.Data), "retryAfterMs") {
		t.Errorf("Data = %s, want the server's data preserved", rpcErr.Data)
	}
}

func TestCallUnblocksOnContextCancel(t *testing.T) {
	conn, ft := newTestConn(t, &recordingHandler{})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- conn.Call(ctx, "turn/start", nil, nil)
	}()

	ft.nextSent(t) // wait until the request is actually on the wire
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after its context was cancelled")
	}
}

func TestCallUnblocksWhenConnectionCloses(t *testing.T) {
	ft := newFakeTransport()
	conn := NewConn(ft, &recordingHandler{})
	go func() { _ = conn.Serve() }()

	errCh := make(chan error, 1)
	go func() { errCh <- conn.Call(context.Background(), "turn/start", nil, nil) }()

	ft.nextSent(t)
	_ = conn.Close()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Call succeeded after the connection closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call hung after the connection closed; in-flight calls must fail")
	}
}

func TestNotificationsDispatch(t *testing.T) {
	h := &recordingHandler{}
	_, ft := newTestConn(t, h)

	ft.push(t, `{"method":"thread/started","params":{"thread":{"id":"thr_1"}}}`)
	ft.push(t, `{"method":"item/agentMessage/delta","params":{"delta":"hi","itemId":"i1","threadId":"t","turnId":"u"}}`)

	waitFor(t, func() bool { return len(h.notificationMethods()) == 2 },
		"expected 2 notifications to be dispatched")

	got := h.notificationMethods()
	if got[0] != "thread/started" || got[1] != "item/agentMessage/delta" {
		t.Errorf("notifications = %v, want them in arrival order", got)
	}
}

// TestNotificationOrderPreserved is the guarantee deltas depend on: text
// reassembled out of order is corrupt.
func TestNotificationOrderPreserved(t *testing.T) {
	h := &recordingHandler{}
	_, ft := newTestConn(t, h)

	const n = 50
	for i := 0; i < n; i++ {
		ft.push(t, `{"method":"item/agentMessage/delta","params":{"delta":"`+itoa(i)+`"}}`)
	}

	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.notifications) == n
	}, "expected all deltas to be dispatched")

	h.mu.Lock()
	defer h.mu.Unlock()
	for i, rec := range h.notifications {
		var p struct{ Delta string }
		if err := json.Unmarshal(rec.Params, &p); err != nil {
			t.Fatalf("delta %d: %v", i, err)
		}
		if p.Delta != itoa(i) {
			t.Fatalf("delta at position %d is %q, want %q: ordering was not preserved",
				i, p.Delta, itoa(i))
		}
	}
}

func TestServerRequestIsAnswered(t *testing.T) {
	h := &recordingHandler{
		onRequest: func(_ ID, _ string, _ json.RawMessage, respond func(any, *Error)) {
			respond(map[string]string{"decision": "accept"}, nil)
		},
	}
	_, ft := newTestConn(t, h)

	ft.push(t, `{"id":7,"method":"item/commandExecution/requestApproval","params":{"itemId":"i1"}}`)

	sent := ft.nextSent(t)
	if sent["id"] != float64(7) {
		t.Errorf("response id = %v, want 7", sent["id"])
	}
	result, ok := sent["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want an object", sent["result"])
	}
	if result["decision"] != "accept" {
		t.Errorf("decision = %v, want accept", result["decision"])
	}
}

// TestServerRequestRespondsOnlyOnce guards against a double reply, which would
// desynchronize the server's own correlation.
func TestServerRequestRespondsOnlyOnce(t *testing.T) {
	h := &recordingHandler{
		onRequest: func(_ ID, _ string, _ json.RawMessage, respond func(any, *Error)) {
			respond(map[string]string{"decision": "accept"}, nil)
			respond(map[string]string{"decision": "decline"}, nil)
		},
	}
	_, ft := newTestConn(t, h)

	ft.push(t, `{"id":7,"method":"item/fileChange/requestApproval","params":{}}`)
	ft.nextSent(t)

	select {
	case extra := <-ft.sent:
		t.Errorf("a second response was sent: %s", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestStringAndNumberIDsRoundTrip covers RequestId being string-or-number: a
// numeric id echoed back as a string breaks correlation on the server side.
func TestStringAndNumberIDsRoundTrip(t *testing.T) {
	h := &recordingHandler{
		onRequest: func(_ ID, _ string, _ json.RawMessage, respond func(any, *Error)) {
			respond(map[string]string{"ok": "yes"}, nil)
		},
	}
	_, ft := newTestConn(t, h)

	ft.push(t, `{"id":"req-abc","method":"item/tool/requestUserInput","params":{}}`)
	sent := ft.nextSent(t)
	if sent["id"] != "req-abc" {
		t.Errorf("id = %#v, want the string \"req-abc\" echoed unchanged", sent["id"])
	}

	ft.push(t, `{"id":42,"method":"item/tool/requestUserInput","params":{}}`)
	sent = ft.nextSent(t)
	if sent["id"] != float64(42) {
		t.Errorf("id = %#v, want the number 42 echoed unchanged", sent["id"])
	}
}

// TestMalformedFrameDoesNotKillConnection: a newer server may emit something
// unexpected, and one bad frame must not end the session.
func TestMalformedFrameDoesNotKillConnection(t *testing.T) {
	h := &recordingHandler{}
	conn, ft := newTestConn(t, h)

	ft.push(t, `{this is not json`)
	ft.push(t, `{"method":"thread/started","params":{}}`)

	waitFor(t, func() bool {
		for _, m := range h.notificationMethods() {
			if m == "thread/started" {
				return true
			}
		}
		return false
	}, "the connection stopped processing after a malformed frame")

	// And it must still be usable for requests.
	errCh := make(chan error, 1)
	go func() { errCh <- conn.Call(context.Background(), "thread/loaded/list", nil, nil) }()
	sent := ft.nextSent(t)
	ft.push(t, `{"id":`+jsonNumber(sent["id"].(float64))+`,"result":{}}`)
	if err := <-errCh; err != nil {
		t.Errorf("Call after a malformed frame failed: %v", err)
	}
}

func TestStreamTransportFraming(t *testing.T) {
	// Blank lines must be skipped and a trailing frame without a newline must
	// still be delivered.
	input := "{\"method\":\"a\"}\n\n{\"method\":\"b\"}\n{\"method\":\"c\"}"
	var out strings.Builder
	tr := NewStreamTransport(strings.NewReader(input), &out, nil)

	for _, want := range []string{`{"method":"a"}`, `{"method":"b"}`, `{"method":"c"}`} {
		got, err := tr.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if string(got) != want {
			t.Errorf("Recv = %s, want %s", got, want)
		}
	}
	if _, err := tr.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("final Recv err = %v, want io.EOF", err)
	}
}

func TestStreamTransportSendAppendsNewline(t *testing.T) {
	var out strings.Builder
	tr := NewStreamTransport(strings.NewReader(""), &out, nil)
	if err := tr.Send([]byte(`{"method":"x"}`)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := out.String(); got != "{\"method\":\"x\"}\n" {
		t.Errorf("Send wrote %q, want a single newline-terminated frame", got)
	}
}

// TestStreamTransportLargeFrame covers a completed turn carrying large
// aggregated command output, which exceeds bufio's default buffer.
func TestStreamTransportLargeFrame(t *testing.T) {
	big := strings.Repeat("x", 512*1024)
	frame := `{"method":"item/completed","params":{"output":"` + big + `"}}`
	tr := NewStreamTransport(strings.NewReader(frame+"\n"), io.Discard, nil)

	got, err := tr.Recv()
	if err != nil {
		t.Fatalf("Recv on a %d byte frame: %v", len(frame), err)
	}
	if len(got) != len(frame) {
		t.Errorf("Recv returned %d bytes, want %d: a large frame was truncated", len(got), len(frame))
	}
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func jsonNumber(f float64) string { return itoa(int(f)) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var digits []byte
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
