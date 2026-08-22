package codex_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// newTestClient wires a Client to a scripted fake server.
func newTestClient(t *testing.T, srv *fakeServer, opts ...codex.Option) *codex.Client {
	t.Helper()
	opts = append([]codex.Option{codex.WithTransport(srv)}, opts...)
	c, err := codex.New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("codex.New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHandshakeSendsInitializeThenInitialized(t *testing.T) {
	srv := newFakeServer(t)
	c := newTestClient(t, srv)

	methods := srv.calledMethods()
	if len(methods) < 2 || methods[0] != "initialize" || methods[1] != "initialized" {
		t.Fatalf("handshake sent %v, want initialize then initialized", methods)
	}

	info := c.ServerInfo()
	if info.UserAgent != "codex-fake/1.0" {
		t.Errorf("UserAgent = %q, want the server's value", info.UserAgent)
	}
	if info.PlatformOS != "linux" {
		t.Errorf("PlatformOS = %q, want linux", info.PlatformOS)
	}
}

// TestExperimentalAPIOffByDefault pins the default: the capability must not be
// sent unless asked for, because enabling experimental members changes what the
// server accepts.
func TestExperimentalAPIOffByDefault(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv)

	var params struct {
		Capabilities *struct {
			ExperimentalAPI *bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(srv.paramsFor("initialize"), &params); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	if params.Capabilities != nil && params.Capabilities.ExperimentalAPI != nil &&
		*params.Capabilities.ExperimentalAPI {
		t.Error("experimentalApi was sent as true by default; it must be opt-in")
	}
}

func TestExperimentalAPIOptIn(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv, codex.WithExperimentalAPI())

	var params struct {
		Capabilities struct {
			ExperimentalAPI *bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(srv.paramsFor("initialize"), &params); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	if params.Capabilities.ExperimentalAPI == nil || !*params.Capabilities.ExperimentalAPI {
		t.Error("WithExperimentalAPI did not set capabilities.experimentalApi")
	}
}

// TestOptOutOfRequiredNotificationIsRejected: suppressing turn/completed would
// make Run hang forever, so it must fail at construction instead.
func TestOptOutOfRequiredNotificationIsRejected(t *testing.T) {
	srv := newFakeServer(t)
	_, err := codex.New(context.Background(),
		codex.WithTransport(srv),
		codex.WithOptOutNotifications(protocol.NotifyTurnCompleted),
	)
	if err == nil {
		t.Fatal("New succeeded; opting out of turn/completed must be rejected")
	}
	if !strings.Contains(err.Error(), "turn/completed") {
		t.Errorf("error = %v, want it to name the offending method", err)
	}
}

func TestOptOutOfDeltaIsAllowed(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv, codex.WithOptOutNotifications(protocol.NotifyItemAgentMessageDelta))

	var params struct {
		Capabilities struct {
			OptOut []string `json:"optOutNotificationMethods"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(srv.paramsFor("initialize"), &params); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	if len(params.Capabilities.OptOut) != 1 ||
		params.Capabilities.OptOut[0] != protocol.NotifyItemAgentMessageDelta {
		t.Errorf("optOutNotificationMethods = %v, want the delta method", params.Capabilities.OptOut)
	}
}

func TestNotificationsReachTypedCallbacks(t *testing.T) {
	srv := newFakeServer(t)

	var (
		mu     sync.Mutex
		deltas []string
		turnID string
	)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
			mu.Lock()
			deltas = append(deltas, n.Delta)
			mu.Unlock()
		},
		OnTurnStarted: func(n *protocol.TurnStartedNotification) {
			mu.Lock()
			if n.Turn != nil {
				turnID = n.Turn.ID
			}
			mu.Unlock()
		},
	}))

	srv.notify(protocol.NotifyTurnStarted, map[string]any{
		"threadId": "thr_1",
		"turn":     map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
	})
	for _, d := range []string{"He", "llo", " world"} {
		srv.notify(protocol.NotifyItemAgentMessageDelta, map[string]any{
			"threadId": "thr_1", "turnId": "turn_1", "itemId": "i1", "delta": d,
		})
	}

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(deltas) == 3
	}, "the delta callback did not receive all three deltas")

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(deltas, ""); got != "Hello world" {
		t.Errorf("concatenated deltas = %q, want %q: ordering was not preserved", got, "Hello world")
	}
	if turnID != "turn_1" {
		t.Errorf("turn id = %q, want turn_1", turnID)
	}
}

// TestUnknownNotificationReachesOnUnhandled is the forward-compatibility path: a
// method from a newer server must surface, not vanish.
func TestUnknownNotificationReachesOnUnhandled(t *testing.T) {
	srv := newFakeServer(t)

	var (
		mu      sync.Mutex
		methods []string
	)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnUnhandled: func(method string, _ []byte) {
			mu.Lock()
			methods = append(methods, method)
			mu.Unlock()
		},
	}))

	srv.notify("thread/somethingFromTheFuture", map[string]any{"threadId": "thr_1"})

	waitUntil(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(methods) > 0
	}, "OnUnhandled never fired for an unknown notification")

	mu.Lock()
	defer mu.Unlock()
	if methods[0] != "thread/somethingFromTheFuture" {
		t.Errorf("method = %q, want the unknown method reported verbatim", methods[0])
	}
}

// TestKnownNotificationWithNoCallbackFallsBackToUnhandled: registering no
// specific callback must not mean silent loss.
func TestKnownNotificationWithNoCallbackFallsBackToUnhandled(t *testing.T) {
	srv := newFakeServer(t)

	got := make(chan string, 4)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnUnhandled: func(method string, _ []byte) { got <- method },
	}))

	srv.notify(protocol.NotifyThreadStatusChanged, map[string]any{
		"threadId": "thr_1", "status": map[string]any{"type": "idle"},
	})

	select {
	case m := <-got:
		if m != protocol.NotifyThreadStatusChanged {
			t.Errorf("method = %q, want %q", m, protocol.NotifyThreadStatusChanged)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a known notification with no callback was dropped instead of reaching OnUnhandled")
	}
}

// TestPerThreadOrderingIsIndependent verifies the core dispatch property: a slow
// callback for one thread must not delay another thread's callbacks.
func TestPerThreadOrderingIsIndependent(t *testing.T) {
	srv := newFakeServer(t)

	release := make(chan struct{})
	fastDone := make(chan struct{})
	var once sync.Once

	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnAgentMessageDelta: func(n *protocol.AgentMessageDeltaNotification) {
			if n.ThreadID == "thr_slow" {
				<-release // stall this thread indefinitely
				return
			}
			once.Do(func() { close(fastDone) })
		},
	}))

	// The slow thread's notification is queued first.
	srv.notify(protocol.NotifyItemAgentMessageDelta, map[string]any{
		"threadId": "thr_slow", "turnId": "t", "itemId": "i", "delta": "x",
	})
	srv.notify(protocol.NotifyItemAgentMessageDelta, map[string]any{
		"threadId": "thr_fast", "turnId": "t", "itemId": "i", "delta": "y",
	})

	select {
	case <-fastDone:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a stalled callback on one thread blocked another thread's callbacks")
	}
	close(release)
}

func TestStartThreadAndRunTurn(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread":         map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":          "gpt-5.6-terra",
		"modelProvider":  "openai",
		"cwd":            "/repo",
		"approvalPolicy": "never",
		"sandbox":        map[string]any{"type": "readOnly"},
	})

	// turn/start responds, then the server streams the turn to completion.
	srv.handle("turn/start", func(_ json.RawMessage, _ json.RawMessage) (any, map[string]any) {
		go func() {
			srv.notify(protocol.NotifyTurnStarted, map[string]any{
				"threadId": "thr_1",
				"turn":     map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
			})
			for _, d := range []string{"Two ", "plus ", "two is 4."} {
				srv.notify(protocol.NotifyItemAgentMessageDelta, map[string]any{
					"threadId": "thr_1", "turnId": "turn_1", "itemId": "i1", "delta": d,
				})
			}
			srv.notify(protocol.NotifyItemCompleted, map[string]any{
				"threadId": "thr_1", "turnId": "turn_1", "completedAtMs": 1,
				"item": map[string]any{
					"type": "agentMessage", "id": "i1", "text": "Two plus two is 4.",
					"phase": "final_answer",
				},
			})
			srv.notify(protocol.NotifyTurnCompleted, map[string]any{
				"threadId": "thr_1",
				"turn": map[string]any{
					"id": "turn_1", "status": "completed", "items": []any{},
				},
			})
		}()
		return map[string]any{
			"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
		}, nil
	})

	c := newTestClient(t, srv)
	thread, err := c.StartThread(context.Background(), protocol.ThreadStartParams{})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}
	if thread.ID() != "thr_1" {
		t.Fatalf("thread id = %q, want thr_1", thread.ID())
	}

	result, err := thread.RunText(context.Background(), "what is 2+2?")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if result.Status() != protocol.TurnStatusCompleted {
		t.Errorf("status = %q, want completed", result.Status())
	}
	if result.AgentMessage != "Two plus two is 4." {
		t.Errorf("AgentMessage = %q, want the completed item's text", result.AgentMessage)
	}
}

// TestRunTurnFailedReturnsTypedError checks that a failed turn is a Go error
// carrying the server's classification, so callers can branch on the cause.
func TestRunTurnFailedReturnsTypedError(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})
	srv.handle("turn/start", func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		go func() {
			srv.notify(protocol.NotifyTurnCompleted, map[string]any{
				"threadId": "thr_1",
				"turn": map[string]any{
					"id": "turn_1", "status": "failed", "items": []any{},
					"error": map[string]any{
						"message":        "context window exceeded",
						"codexErrorInfo": "contextWindowExceeded",
					},
				},
			})
		}()
		return map[string]any{
			"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
		}, nil
	})

	c := newTestClient(t, srv)
	thread, err := c.StartThread(context.Background(), protocol.ThreadStartParams{})
	if err != nil {
		t.Fatalf("StartThread: %v", err)
	}

	_, err = thread.RunText(context.Background(), "go")
	if err == nil {
		t.Fatal("RunText succeeded; a failed turn must return an error")
	}
	var failed *codex.TurnFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v (%T), want *codex.TurnFailedError", err, err)
	}
	if failed.Info == nil {
		t.Fatal("TurnFailedError.Info is nil; the classification was lost")
	}
	if !failed.Info.IsContextWindowExceeded() {
		t.Errorf("Info.Kind = %q, want contextWindowExceeded", failed.Info.Kind)
	}
}

// TestRunTurnInterruptedIsNotAnError: interruption is normally the caller's own
// doing, so it is reported as a status rather than an error.
func TestRunTurnInterruptedIsNotAnError(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})
	srv.handle("turn/start", func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		go func() {
			srv.notify(protocol.NotifyTurnCompleted, map[string]any{
				"threadId": "thr_1",
				"turn":     map[string]any{"id": "turn_1", "status": "interrupted", "items": []any{}},
			})
		}()
		return map[string]any{
			"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
		}, nil
	})

	c := newTestClient(t, srv)
	thread, _ := c.StartThread(context.Background(), protocol.ThreadStartParams{})

	result, err := thread.RunText(context.Background(), "go")
	if err != nil {
		t.Fatalf("RunText returned an error for an interrupted turn: %v", err)
	}
	if !result.Interrupted() {
		t.Errorf("Interrupted() = false, status = %q", result.Status())
	}
}

// TestUnansweredApprovalDeclines is the most important safety property: with no
// approval handler the SDK must decline rather than hang the turn.
func TestUnansweredApprovalDeclines(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv) // no approval handler registered

	srv.request(99, protocol.ServerMethodItemCommandExecutionRequestApproval, map[string]any{
		"itemId": "i1", "threadId": "thr_1", "turnId": "turn_1",
		"command": "rm -rf /", "cwd": "/",
	})

	frame := srv.waitForClientResponse()
	if frame["id"] != float64(99) {
		t.Fatalf("response id = %v, want 99", frame["id"])
	}
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want a decision object", frame["result"])
	}
	if result["decision"] != "decline" {
		t.Errorf("decision = %v, want decline when no handler is registered", result["decision"])
	}
}

func TestApprovalHandlerDecisionIsSent(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnCommandApproval: func(p *protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
			return protocol.NewCommandExecutionApprovalDecisionAcceptForSession(), nil
		},
	}))

	srv.request(7, protocol.ServerMethodItemCommandExecutionRequestApproval, map[string]any{
		"itemId": "i1", "threadId": "thr_1", "turnId": "turn_1",
		"command": "ls", "cwd": "/repo",
	})

	frame := srv.waitForClientResponse()
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("result = %v, want a decision object", frame["result"])
	}
	if result["decision"] != "acceptForSession" {
		t.Errorf("decision = %v, want acceptForSession", result["decision"])
	}
}

// TestApprovalHandlerErrorDeclines: a handler that fails must not hang the turn.
func TestApprovalHandlerErrorDeclines(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnCommandApproval: func(*protocol.CommandExecutionRequestApprovalParams) (protocol.CommandExecutionApprovalDecision, error) {
			return protocol.CommandExecutionApprovalDecision{}, errors.New("policy service unreachable")
		},
	}))

	srv.request(8, protocol.ServerMethodItemCommandExecutionRequestApproval, map[string]any{
		"itemId": "i1", "threadId": "t", "turnId": "u", "command": "ls", "cwd": "/",
	})

	frame := srv.waitForClientResponse()
	result, _ := frame["result"].(map[string]any)
	if result == nil || result["decision"] != "decline" {
		t.Errorf("result = %v, want a decline after the handler failed", frame["result"])
	}
}

// TestApprovalHandlerPanicIsAnswered: even a panicking handler must not leave the
// turn waiting.
func TestApprovalHandlerPanicIsAnswered(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv, codex.WithHandler(codex.Handler{
		OnFileChangeApproval: func(*protocol.FileChangeRequestApprovalParams) (protocol.FileChangeApprovalDecision, error) {
			panic("handler bug")
		},
	}))

	srv.request(9, protocol.ServerMethodItemFileChangeRequestApproval, map[string]any{
		"itemId": "i1", "threadId": "t", "turnId": "u",
	})

	frame := srv.waitForClientResponse()
	if frame["result"] == nil && frame["error"] == nil {
		t.Error("a panicking approval handler produced no response, which hangs the turn")
	}
}

// TestUnknownServerRequestIsRejected: an unrecognized server request must be
// answered with an error, not ignored.
func TestUnknownServerRequestIsRejected(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv)

	srv.request(11, "someFutureRequest/fromTheServer", map[string]any{})

	frame := srv.waitForClientResponse()
	rpcErr, ok := frame["error"].(map[string]any)
	if !ok {
		t.Fatalf("frame = %v, want an error response", frame)
	}
	if rpcErr["code"] != float64(jsonrpc.CodeMethodNotFound) {
		t.Errorf("code = %v, want %d", rpcErr["code"], jsonrpc.CodeMethodNotFound)
	}
}

func TestRPCErrorIsTypedAndPreservesCode(t *testing.T) {
	srv := newFakeServer(t)
	srv.replyError("thread/start", jsonrpc.CodeServerOverloaded, "Server overloaded; retry later.")

	c := newTestClient(t, srv)
	_, err := c.StartThread(context.Background(), protocol.ThreadStartParams{})
	if err == nil {
		t.Fatal("StartThread succeeded, want an error")
	}
	if !errors.Is(err, jsonrpc.ErrServerOverloaded) {
		t.Errorf("errors.Is(err, ErrServerOverloaded) = false for %v", err)
	}
}

func TestRunStreamEmitsOrderedEvents(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})
	srv.handle("turn/start", func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		go func() {
			srv.notify(protocol.NotifyTurnStarted, map[string]any{
				"threadId": "thr_1",
				"turn":     map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
			})
			for _, d := range []string{"a", "b", "c"} {
				srv.notify(protocol.NotifyItemAgentMessageDelta, map[string]any{
					"threadId": "thr_1", "turnId": "turn_1", "itemId": "i1", "delta": d,
				})
			}
			srv.notify(protocol.NotifyTurnCompleted, map[string]any{
				"threadId": "thr_1",
				"turn":     map[string]any{"id": "turn_1", "status": "completed", "items": []any{}},
			})
		}()
		return map[string]any{
			"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
		}, nil
	})

	c := newTestClient(t, srv)
	thread, _ := c.StartThread(context.Background(), protocol.ThreadStartParams{})

	stream, err := thread.RunStream(context.Background(), protocol.TurnStartParams{
		Input: []*protocol.UserInput{codex.TextInput("go")},
	})
	if err != nil {
		t.Fatalf("RunStream: %v", err)
	}

	var kinds []codex.EventKind
	var text strings.Builder
	for ev := range stream.Events() {
		kinds = append(kinds, ev.Kind)
		text.WriteString(ev.AgentText())
	}
	if text.String() != "abc" {
		t.Errorf("streamed text = %q, want abc", text.String())
	}
	if len(kinds) == 0 || kinds[len(kinds)-1] != codex.EventTurnCompleted {
		t.Errorf("event kinds = %v, want turn.completed last", kinds)
	}

	result, err := stream.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if result.Status() != protocol.TurnStatusCompleted {
		t.Errorf("status = %q, want completed", result.Status())
	}
}

// TestRunCancelledInterruptsTurn: a cancelled context must ask the server to
// stop, not just abandon the wait.
func TestRunCancelledInterruptsTurn(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})
	// The turn starts but never completes, so only cancellation ends it.
	srv.reply("turn/start", map[string]any{
		"turn": map[string]any{"id": "turn_1", "status": "inProgress", "items": []any{}},
	})
	srv.reply("turn/interrupt", map[string]any{})

	c := newTestClient(t, srv)
	thread, _ := c.StartThread(context.Background(), protocol.ThreadStartParams{})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := thread.RunText(ctx, "go")
		done <- err
	}()

	srv.waitForCall("turn/start")
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	srv.waitForCall("turn/interrupt")
}

func TestThinLayerSendsExactWireShape(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	c := newTestClient(t, srv)
	cwd := "/Users/me/project"
	mode := protocol.SandboxModeWorkspaceWrite
	if _, err := c.ThreadStart(context.Background(), protocol.ThreadStartParams{
		Cwd:     &cwd,
		Sandbox: &mode,
	}); err != nil {
		t.Fatalf("ThreadStart: %v", err)
	}

	var sent map[string]any
	if err := json.Unmarshal(srv.paramsFor("thread/start"), &sent); err != nil {
		t.Fatalf("decoding params: %v", err)
	}
	if sent["cwd"] != cwd {
		t.Errorf("cwd = %v, want %q", sent["cwd"], cwd)
	}
	// The kebab-case wire value matters: thread/start takes SandboxMode, while
	// turn/start takes the camelCase SandboxPolicy tags.
	if sent["sandbox"] != "workspace-write" {
		t.Errorf("sandbox = %v, want workspace-write", sent["sandbox"])
	}
	// Unset optional fields must be omitted, not sent as null.
	if _, present := sent["model"]; present {
		t.Error("model was sent despite being unset")
	}
}

func TestCloseIsIdempotentAndFailsInFlight(t *testing.T) {
	srv := newFakeServer(t)
	c := newTestClient(t, srv)

	if err := c.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if _, err := c.ThreadStart(context.Background(), protocol.ThreadStartParams{}); err == nil {
		t.Error("a call after Close succeeded, want an error")
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}
