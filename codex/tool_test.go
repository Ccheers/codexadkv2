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
)

// grepArgs exercises the required-ness rule: reflection makes a field required
// unless its json tag carries omitempty.
type grepArgs struct {
	Pattern string `json:"pattern" jsonschema:"description=regular expression to match"`
	Path    string `json:"path,omitempty" jsonschema:"description=directory to search"`
	Limit   int    `json:"limit,omitempty"`
}

// TestNewToolReflectsSchema checks the reflected schema is the shape a model tool
// definition expects: a plain inlined object, no $ref indirection, no draft
// marker, with the json tag names and required-ness preserved.
func TestNewToolReflectsSchema(t *testing.T) {
	tool := codex.NewTool("grep", "Search files by regex",
		func(ctx context.Context, callID string, a grepArgs) (string, error) {
			return "", nil
		})

	if tool.Name() != "grep" {
		t.Errorf("Name() = %q, want grep", tool.Name())
	}
	if tool.Description() == "" {
		t.Error("Description() is empty; the model needs it to decide whether to call")
	}

	schema := tool.InputSchema()
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}
	// A $ref-based schema would be useless to a model that cannot resolve it.
	if _, ok := schema["$schema"]; ok {
		t.Error("schema still carries the $schema draft marker")
	}
	if _, ok := schema["$defs"]; ok {
		t.Error("schema uses $defs; it must be inlined for a tool definition")
	}

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties object: %#v", schema)
	}
	for _, want := range []string{"pattern", "path", "limit"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema is missing property %q; json tag names must be preserved", want)
		}
	}

	// Required-ness comes from omitempty, not from a jsonschema:"required" tag:
	// pattern has no omitempty so it is required, path and limit do so they are
	// not. This is the opposite of most people's expectation, so pin it.
	required, _ := schema["required"].([]any)
	var names []string
	for _, r := range required {
		if s, ok := r.(string); ok {
			names = append(names, s)
		}
	}
	if len(names) != 1 || names[0] != "pattern" {
		t.Errorf("required = %v, want exactly [pattern]: a field is required "+
			"unless its json tag has omitempty", names)
	}

	// Descriptions from the tags must survive, since they are what the model reads.
	pattern, _ := props["pattern"].(map[string]any)
	if desc, _ := pattern["description"].(string); !strings.Contains(desc, "regular expression") {
		t.Errorf("pattern description = %q, want the jsonschema tag text", desc)
	}
}

// TestToolCallDecodesTypedArgs covers the decode path from wire JSON to the
// handler's typed argument struct.
func TestToolCallDecodesTypedArgs(t *testing.T) {
	var got grepArgs
	tool := codex.NewTool("grep", "Search",
		func(ctx context.Context, callID string, a grepArgs) (string, error) {
			got = a
			return "3 matches", nil
		})

	out, err := tool.Call(context.Background(), "call_1",
		json.RawMessage(`{"pattern":"foo.*bar","path":"/repo","limit":5}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if out != "3 matches" {
		t.Errorf("result = %q, want 3 matches", out)
	}
	if got.Pattern != "foo.*bar" || got.Path != "/repo" || got.Limit != 5 {
		t.Errorf("decoded args = %+v, want the wire values", got)
	}
}

// TestToolCallWithNoArguments: a tool that takes nothing must not fail on an empty
// or null payload, which is what the server sends for an argument-free tool.
func TestToolCallWithNoArguments(t *testing.T) {
	type noArgs struct{}
	tool := codex.NewTool("ping", "Respond with pong",
		func(ctx context.Context, callID string, a noArgs) (string, error) {
			return "pong", nil
		})

	for _, payload := range []string{``, `null`, `{}`} {
		out, err := tool.Call(context.Background(), "c", json.RawMessage(payload))
		if err != nil {
			t.Errorf("payload %q: %v", payload, err)
			continue
		}
		if out != "pong" {
			t.Errorf("payload %q gave %q, want pong", payload, out)
		}
	}
}

// TestSessionRegistersToolsAndRoutesCalls is the end-to-end path: tools appear in
// the thread/start spec, and a tool call from the server reaches the right handler
// and comes back as a successful response.
func TestSessionRegistersToolsAndRoutesCalls(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	var (
		mu       sync.Mutex
		calledAs []string
	)
	record := func(label string) func(context.Context, string, grepArgs) (string, error) {
		return func(_ context.Context, callID string, a grepArgs) (string, error) {
			mu.Lock()
			calledAs = append(calledAs, label+":"+a.Pattern+":"+callID)
			mu.Unlock()
			return label + " ran", nil
		}
	}

	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithTools(codex.NewTool("grep", "Search files", record("top"))),
		codex.WithToolGroups(codex.ToolGroup{
			Name:        "db",
			Description: "Inspect the application database",
			Tools:       []codex.DynamicTool{codex.NewTool("query", "Run a query", record("db"))},
		}),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	// The spec must reach thread/start, or the model never learns the tools exist.
	var started struct {
		DynamicTools []struct {
			Type        string `json:"type"`
			Name        string `json:"name"`
			Description string `json:"description"`
			Tools       []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"dynamicTools"`
	}
	if err := json.Unmarshal(srv.paramsFor("thread/start"), &started); err != nil {
		t.Fatalf("decoding thread/start params: %v", err)
	}
	if len(started.DynamicTools) != 2 {
		t.Fatalf("dynamicTools has %d entries, want 2", len(started.DynamicTools))
	}
	if started.DynamicTools[0].Type != "function" || started.DynamicTools[0].Name != "grep" {
		t.Errorf("first spec = %+v, want the grep function", started.DynamicTools[0])
	}
	group := started.DynamicTools[1]
	if group.Type != "namespace" || group.Name != "db" {
		t.Errorf("second spec = %+v, want the db namespace", group)
	}
	if group.Description != "Inspect the application database" {
		t.Errorf("group description = %q; it must be the group's own, not a tool's",
			group.Description)
	}
	if len(group.Tools) != 1 || group.Tools[0].Name != "query" {
		t.Errorf("group tools = %+v, want [query]", group.Tools)
	}

	// Registering tools must enable the experimental capability, or the server
	// rejects dynamicTools outright.
	var caps struct {
		Capabilities struct {
			ExperimentalAPI *bool `json:"experimentalApi"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(srv.paramsFor("initialize"), &caps); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	if caps.Capabilities.ExperimentalAPI == nil || !*caps.Capabilities.ExperimentalAPI {
		t.Error("registering tools did not enable the experimental capability")
	}

	// A top-level call routes to the top-level tool.
	srv.request(1, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "call_a",
		"tool": "grep", "arguments": map[string]any{"pattern": "alpha"},
	})
	assertToolSuccess(t, srv.waitForClientResponse(), "top ran")

	// A namespaced call routes to the tool inside that group.
	srv.request(2, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "call_b",
		"namespace": "db", "tool": "query", "arguments": map[string]any{"pattern": "beta"},
	})
	assertToolSuccess(t, srv.waitForClientResponse(), "db ran")

	mu.Lock()
	defer mu.Unlock()
	want := []string{"top:alpha:call_a", "db:beta:call_b"}
	if len(calledAs) != 2 || calledAs[0] != want[0] || calledAs[1] != want[1] {
		t.Errorf("handlers saw %v, want %v", calledAs, want)
	}
}

// TestUnknownToolCallIsAnswered: an unknown tool must produce a failed tool call,
// not silence. Leaving it unanswered would block the turn forever.
func TestUnknownToolCallIsAnswered(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithTools(codex.NewTool("grep", "Search",
			func(context.Context, string, grepArgs) (string, error) { return "ok", nil })),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	srv.request(5, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
		"tool": "doesNotExist", "arguments": map[string]any{},
	})

	frame := srv.waitForClientResponse()
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("frame = %v, want a tool-call result", frame)
	}
	if result["success"] != false {
		t.Errorf("success = %v, want false for an unknown tool", result["success"])
	}
	if !strings.Contains(toolResponseText(t, result), "doesNotExist") {
		t.Errorf("failure text does not name the tool: %v", result)
	}
}

// TestToolErrorReportsFailedCall: a handler error must reach the model as a failed
// call with the reason, so it can adapt rather than seeing an empty success.
func TestToolErrorReportsFailedCall(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithTools(codex.NewTool("flaky", "Fails on purpose",
			func(context.Context, string, grepArgs) (string, error) {
				return "", errors.New("upstream service unreachable")
			})),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	srv.request(6, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
		"tool": "flaky", "arguments": map[string]any{},
	})

	frame := srv.waitForClientResponse()
	result, _ := frame["result"].(map[string]any)
	if result == nil || result["success"] != false {
		t.Fatalf("result = %v, want success=false", frame["result"])
	}
	if !strings.Contains(toolResponseText(t, result), "upstream service unreachable") {
		t.Errorf("failure text lost the handler's reason: %v", result)
	}
}

// TestToolCallWithNoToolsRegisteredIsAnswered: the turn must not hang just because
// the client has no tools.
func TestToolCallWithNoToolsRegisteredIsAnswered(t *testing.T) {
	srv := newFakeServer(t)
	newTestClient(t, srv)

	srv.request(7, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
		"tool": "anything", "arguments": map[string]any{},
	})

	frame := srv.waitForClientResponse()
	if frame["result"] == nil && frame["error"] == nil {
		t.Error("a tool call with no tools registered got no answer, which hangs the turn")
	}
}

// TestExplicitToolHandlerTakesPrecedence lets a caller own dispatch entirely.
func TestExplicitToolHandlerTakesPrecedence(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	registryRan := false
	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithTools(codex.NewTool("grep", "Search",
			func(context.Context, string, grepArgs) (string, error) {
				registryRan = true
				return "registry", nil
			})),
		codex.WithHandler(codex.Handler{
			OnDynamicToolCall: func(p *protocol.DynamicToolCallParams) (*protocol.DynamicToolCallResponse, error) {
				item := protocol.NewDynamicToolCallOutputContentItemInputText(
					protocol.DynamicToolCallOutputContentItemInputTextPayload{Text: "handler"})
				return &protocol.DynamicToolCallResponse{
					Success:      true,
					ContentItems: []*protocol.DynamicToolCallOutputContentItem{&item},
				}, nil
			},
		}),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	srv.request(8, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
		"tool": "grep", "arguments": map[string]any{},
	})
	assertToolSuccess(t, srv.waitForClientResponse(), "handler")

	if registryRan {
		t.Error("the registered tool ran despite an explicit OnDynamicToolCall handler")
	}
}

// TestToolRegistrationValidation catches assembly mistakes when the session opens
// rather than mid-turn when the model happens to call one.
func TestToolRegistrationValidation(t *testing.T) {
	stub := func(name string) codex.DynamicTool {
		return codex.NewTool(name, "desc",
			func(context.Context, string, grepArgs) (string, error) { return "", nil })
	}

	for _, tc := range []struct {
		name string
		opts []codex.SessionOption
		want string
	}{
		{
			"duplicate top-level tools",
			[]codex.SessionOption{codex.WithTools(stub("grep"), stub("grep"))},
			"duplicate tool",
		},
		{
			"group without a description",
			[]codex.SessionOption{codex.WithToolGroups(codex.ToolGroup{
				Name: "db", Tools: []codex.DynamicTool{stub("query")},
			})},
			"no description",
		},
		{
			"empty group",
			[]codex.SessionOption{codex.WithToolGroups(codex.ToolGroup{
				Name: "db", Description: "d",
			})},
			"no tools",
		},
		{
			"duplicate group names",
			[]codex.SessionOption{codex.WithToolGroups(
				codex.ToolGroup{Name: "db", Description: "d", Tools: []codex.DynamicTool{stub("a")}},
				codex.ToolGroup{Name: "db", Description: "d", Tools: []codex.DynamicTool{stub("b")}},
			)},
			"duplicate tool group",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t)
			opts := append([]codex.SessionOption{codex.WithTransport(srv)}, tc.opts...)
			_, err := codex.Open(context.Background(), opts...)
			if err == nil {
				t.Fatal("Open succeeded, want a validation error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
			// The failure must happen before a server is spawned, so nothing to clean up.
			if len(srv.calledMethods()) > 0 {
				t.Errorf("validation spawned a server first (called %v)", srv.calledMethods())
			}
		})
	}
}

// TestDeferLoadingInstructions pins the developer-instructions prompt for
// defer-loaded groups: 只有 ToolDeferLoading 为 true 的组才进提示词，普通组不进，
// 格式为 Name/Description 加 Name__tool 列表，且与调用方自带提示词叠加而非覆盖。
func TestDeferLoadingInstructions(t *testing.T) {
	deferred := codex.ToolGroup{
		Name:        "db",
		Description: "Inspect the application database",
		Tools: []codex.DynamicTool{
			codex.NewTool[struct{}]("query", "Run a query", func(context.Context, string, struct{}) (string, error) { return "", nil }),
			codex.NewTool[struct{}]("migrate", "Run a migration", func(context.Context, string, struct{}) (string, error) { return "", nil }),
		},
		ToolDeferLoading: true,
	}
	eager := codex.ToolGroup{
		Name:        "fs",
		Description: "Read and write files",
		Tools: []codex.DynamicTool{
			codex.NewTool[struct{}]("read", "Read a file", func(context.Context, string, struct{}) (string, error) { return "", nil }),
		},
	}

	t.Run("injects and skips eager groups", func(t *testing.T) {
		srv := newFakeServer(t)
		srv.reply("thread/start", threadResponse("thr_1"))

		session, err := codex.Open(context.Background(),
			codex.WithTransport(srv),
			codex.WithToolGroups(deferred, eager),
		)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = session.Close() })

		var started struct {
			DeveloperInstructions string `json:"developerInstructions"`
		}
		if err := json.Unmarshal(srv.paramsFor("thread/start"), &started); err != nil {
			t.Fatalf("decoding thread/start params: %v", err)
		}

		want := "## ToolGroups:\n" +
			"### db\nInspect the application database\n" +
			"- db__query\n" +
			"- db__migrate\n"
		if started.DeveloperInstructions != want {
			t.Errorf("developerInstructions =\n%q\nwant\n%q", started.DeveloperInstructions, want)
		}
	})

	t.Run("appends to caller-provided prompt", func(t *testing.T) {
		srv := newFakeServer(t)
		srv.reply("thread/start", threadResponse("thr_1"))

		session, err := codex.Open(context.Background(),
			codex.WithTransport(srv),
			codex.WithToolGroups(deferred),
			codex.WithThreadOptions(protocol.WithThreadStartParamsDeveloperInstructions("caller system prompt")),
		)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = session.Close() })

		var started struct {
			DeveloperInstructions string `json:"developerInstructions"`
		}
		if err := json.Unmarshal(srv.paramsFor("thread/start"), &started); err != nil {
			t.Fatalf("decoding thread/start params: %v", err)
		}

		want := "caller system prompt\n\n" +
			"## ToolGroups:\n### db\nInspect the application database\n- db__query\n- db__migrate\n"
		if started.DeveloperInstructions != want {
			t.Errorf("developerInstructions =\n%q\nwant caller prompt with defer map appended:\n%q",
				started.DeveloperInstructions, want)
		}
	})
}

// TestResumeInjectsDeferLoadingInstructions pins Resume 同样注入 defer 提示词：
// 提示词不在 rollout 里，续接线程时须随 thread/resume 重新下发；调用方已通过
// WithResumeThreadParams 自带 developerInstructions 时以调用方为准。
func TestResumeInjectsDeferLoadingInstructions(t *testing.T) {
	group := codex.ToolGroup{
		Name:        "db",
		Description: "Inspect the application database",
		Tools: []codex.DynamicTool{
			codex.NewTool[struct{}]("query", "Run a query", func(context.Context, string, struct{}) (string, error) { return "", nil }),
		},
		ToolDeferLoading: true,
	}

	t.Run("injects when absent", func(t *testing.T) {
		srv := newFakeServer(t)
		srv.reply("thread/resume", threadResponse("thr_existing"))

		sess, err := codex.Resume(context.Background(),
			codex.WithTransport(srv),
			codex.WithResumeThreadID("thr_existing"),
			codex.WithToolGroups(group),
		)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })

		var resumed struct {
			DeveloperInstructions string `json:"developerInstructions"`
		}
		if err := json.Unmarshal(srv.paramsFor("thread/resume"), &resumed); err != nil {
			t.Fatalf("decoding thread/resume params: %v", err)
		}
		want := "## ToolGroups:\n### db\nInspect the application database\n- db__query\n"
		if resumed.DeveloperInstructions != want {
			t.Errorf("developerInstructions =\n%q\nwant\n%q", resumed.DeveloperInstructions, want)
		}
	})

	t.Run("appends to caller-provided prompt", func(t *testing.T) {
		srv := newFakeServer(t)
		srv.reply("thread/resume", threadResponse("thr_existing"))

		sess, err := codex.Resume(context.Background(),
			codex.WithTransport(srv),
			codex.WithResumeThreadID("thr_existing"),
			codex.WithToolGroups(group),
			codex.WithResumeThreadParams(protocol.ThreadResumeParams{
				DeveloperInstructions: protocol.Ptr("caller system prompt"),
			}),
		)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		t.Cleanup(func() { _ = sess.Close() })

		var resumed struct {
			DeveloperInstructions string `json:"developerInstructions"`
		}
		if err := json.Unmarshal(srv.paramsFor("thread/resume"), &resumed); err != nil {
			t.Fatalf("decoding thread/resume params: %v", err)
		}
		want := "caller system prompt\n\n" +
			"## ToolGroups:\n### db\nInspect the application database\n- db__query\n"
		if resumed.DeveloperInstructions != want {
			t.Errorf("developerInstructions =\n%q\nwant caller prompt with defer map appended:\n%q",
				resumed.DeveloperInstructions, want)
		}
	})
}

// TestSameToolInDifferentGroups: a tool carries no namespace of its own, so the
// same instance can be assembled into different groups.
func TestSameToolInDifferentGroups(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	shared := codex.NewTool("search", "Search this area",
		func(_ context.Context, _ string, a grepArgs) (string, error) {
			return "found " + a.Pattern, nil
		})

	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithToolGroups(
			codex.ToolGroup{Name: "docs", Description: "Search the docs",
				Tools: []codex.DynamicTool{shared}},
			codex.ToolGroup{Name: "code", Description: "Search the code",
				Tools: []codex.DynamicTool{shared}},
		),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	for i, ns := range []string{"docs", "code"} {
		srv.request(20+i, protocol.ServerMethodItemToolCall, map[string]any{
			"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
			"namespace": ns, "tool": "search",
			"arguments": map[string]any{"pattern": ns},
		})
		assertToolSuccess(t, srv.waitForClientResponse(), "found "+ns)
	}
}

// TestToolCallDuringRun proves the whole point: a tool call is answered while Run
// is blocked, so the turn can proceed.
func TestToolCallDuringRun(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	toolRan := make(chan struct{})
	srv.handle("turn/start", func(json.RawMessage, json.RawMessage) (any, map[string]any) {
		go func() {
			// The model calls the tool mid-turn, then finishes once it answers.
			srv.request(30, protocol.ServerMethodItemToolCall, map[string]any{
				"threadId": "thr_1", "turnId": "turn_1", "callId": "c",
				"tool": "grep", "arguments": map[string]any{"pattern": "x"},
			})
			select {
			case <-toolRan:
			case <-time.After(3 * time.Second):
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

	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithTools(codex.NewTool("grep", "Search",
			func(context.Context, string, grepArgs) (string, error) {
				close(toolRan)
				return "1 match", nil
			})),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.RunText(context.Background(), "find x")
	if err != nil {
		t.Fatalf("RunText: %v", err)
	}
	if result.Status() != protocol.TurnStatusCompleted {
		t.Errorf("status = %q, want completed", result.Status())
	}
	select {
	case <-toolRan:
	default:
		t.Error("the tool was never called during the turn")
	}
}

func assertToolSuccess(t *testing.T, frame map[string]any, wantText string) {
	t.Helper()
	result, ok := frame["result"].(map[string]any)
	if !ok {
		t.Fatalf("frame = %v, want a tool-call result", frame)
	}
	if result["success"] != true {
		t.Errorf("success = %v, want true", result["success"])
	}
	if got := toolResponseText(t, result); got != wantText {
		t.Errorf("tool result text = %q, want %q", got, wantText)
	}
}

func toolResponseText(t *testing.T, result map[string]any) string {
	t.Helper()
	items, ok := result["contentItems"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("result has no contentItems: %v", result)
	}
	first, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("contentItems[0] is not an object: %v", items[0])
	}
	text, _ := first["text"].(string)
	return text
}
