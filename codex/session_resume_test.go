package codex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/internal/jsonrpc"
)

// threadResponse 是 fakeServer 对 thread/start 与 thread/resume 的应答。两者同构
// （ResumeResponse 与 StartResponse 描述同一件事，thread.go 的 resumeToStart 也靠
// JSON round-trip 复用 start 形状）。
func threadResponse(id string) map[string]any {
	return map[string]any{
		"thread": map[string]any{"id": id, "sessionId": id},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	}
}

// assertSent/assertNotSent 用 fakeServer 的 calledMethods 断言某方法是否被调用。
func assertSent(t *testing.T, srv *fakeServer, method string) {
	t.Helper()
	for _, m := range srv.calledMethods() {
		if m == method {
			return
		}
	}
	t.Errorf("expected method %q to be called; got %v", method, srv.calledMethods())
}

func assertNotSent(t *testing.T, srv *fakeServer, method string) {
	t.Helper()
	for _, m := range srv.calledMethods() {
		if m == method {
			t.Errorf("expected method %q NOT to be called; got %v", method, srv.calledMethods())
			return
		}
	}
}

// echoTool 是带一个参数的动态工具，验证 Resume/Open 的 dispatch 装配。
func echoTool() codex.DynamicTool {
	return codex.NewTool[struct {
		Text string `json:"text"`
	}]("echo", "echo text back",
		func(_ context.Context, _ string, args struct {
			Text string `json:"text"`
		}) (string, error) {
			return args.Text, nil
		})
}

// TestSessionOpenSendsThreadStart pins Session.Open（新建）走 thread/start。
func TestSessionOpenSendsThreadStart(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", threadResponse("thr_new"))

	sess, err := codex.Open(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sess.Close()

	if got := sess.ID(); got != "thr_new" {
		t.Errorf("sess.ID() = %q, want thr_new", got)
	}
	assertSent(t, srv, "thread/start")
	assertNotSent(t, srv, "thread/resume")
}

// TestSessionResumeSendsThreadResume pins Session.Resume（续接）走 thread/resume，
// 且按续接 id 恢复线程而非新建。
func TestSessionResumeSendsThreadResume(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/resume", threadResponse("thr_existing"))

	sess, err := codex.Resume(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
		codex.WithResumeThreadID("thr_existing"),
	)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer sess.Close()

	if got := sess.ID(); got != "thr_existing" {
		t.Errorf("sess.ID() = %q, want thr_existing", got)
	}
	assertSent(t, srv, "thread/resume")
	assertNotSent(t, srv, "thread/start")
}

// TestSessionResumeValidatesThreadID pins Resume 缺 resumeThreadID 时立即失败，
// 不把「无续接目标」变成对 thread/resume 的一次空请求。
func TestSessionResumeValidatesThreadID(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/resume", threadResponse("thr_existing"))

	_, err := codex.Resume(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
	)
	if err == nil {
		t.Fatal("Resume without resumeThreadID succeeded, want an error")
	}
	assertNotSent(t, srv, "thread/resume")
}

// TestSessionResumeFailureReturnsServerError pins Resume 把 thread/resume 的原始
// 错误上抛（含 jsonrpc *Error），不做「可恢复/不可恢复」分类——分类交给调用方。
func TestSessionResumeFailureReturnsServerError(t *testing.T) {
	srv := newFakeServer(t)
	srv.replyError("thread/resume", -32000, "thread not found")

	_, err := codex.Resume(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
		codex.WithResumeThreadID("thr_gone"),
	)
	if err == nil {
		t.Fatal("Resume of a missing thread succeeded, want an error")
	}
	var rpcErr *jsonrpc.Error
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected a jsonrpc *Error preserved through Resume, got %v", err)
	}
}

// TestSessionResumeRegistersTools pins Resume 的 handler 同样注册动态工具派发：
// 续接的线程从 rollout 恢复 dynamic tool spec（ADR-0016），但 handler 的 dispatch
// 必须在本 client 注册，否则续接回合里调 joey 工具会得到「未知工具」。
func TestSessionResumeRegistersTools(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/resume", threadResponse("thr_existing"))

	sess, err := codex.Resume(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
		codex.WithResumeThreadID("thr_existing"),
		codex.WithTools(echoTool()),
	)
	if err != nil {
		t.Fatalf("Resume with tools: %v", err)
	}
	defer sess.Close()
	// 注册成功即达成：resume 的 dispatch 已装配；真实「续接回合能调工具」由缝 2 验证。
}

// TestSessionResumeRejectsBadTool pins Resume 与 Open 同样在 spawn 前拒绝空名字工具，
// 而不是把一个编程错误推迟到 thread/resume 之后。
func TestSessionResumeRejectsBadTool(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/resume", threadResponse("thr_existing"))

	_, err := codex.Resume(context.Background(),
		codex.WithClientInfo("t", "Test", "1.0"),
		codex.WithTransport(srv),
		codex.WithResumeThreadID("thr_existing"),
		codex.WithTools(codex.NewTool[struct{}]("", "empty name", func(context.Context, string, struct{}) (string, error) {
			return "", nil
		})),
	)
	if err == nil {
		t.Fatal("Resume with an empty-named tool succeeded, want an error")
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		t.Fatalf("expected a pre-spawn tool registry error, got a jsonrpc error: %v", err)
	}
}
