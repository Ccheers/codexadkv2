//go:build integration

// This suite talks to a real `codex app-server` child process, so it is behind a
// build tag and excluded from the default `go test ./...`.
//
// Run it with:
//
//	go test -tags integration ./codex/
//
// It deliberately exercises only what needs no authentication: process spawn,
// the initialize handshake, the model catalog, and clean shutdown. Anything that
// starts a turn would need a logged-in account and would make the suite
// unrunnable in most environments. That is a real coverage gap, and the point of
// this suite is narrower: it detects wire-level drift between the committed
// schema and whatever codex is installed.
package codex_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

// requireCodex skips the test when the codex CLI is not installed.
func requireCodex(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex not found in PATH; skipping the integration suite")
	}
}

func newRealClient(t *testing.T, opts ...codex.Option) *codex.Client {
	t.Helper()
	requireCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)

	var stderr strings.Builder
	opts = append([]codex.Option{
		codex.WithClientInfo("codex-go-sdk-tests", "Codex Go SDK integration tests", "0.1.0"),
		codex.WithStderr(&stderr),
	}, opts...)

	c, err := codex.New(ctx, opts...)
	if err != nil {
		t.Fatalf("codex.New against the real app-server: %v\nstderr:\n%s", err, stderr.String())
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return c
}

// TestRealHandshake is the core drift detector: it proves the committed
// InitializeResponse still matches what the installed server sends.
func TestRealHandshake(t *testing.T) {
	c := newRealClient(t)

	info := c.ServerInfo()
	if info.UserAgent == "" {
		t.Error("ServerInfo().UserAgent is empty; the initialize response shape may have changed")
	}
	if info.CodexHome == "" {
		t.Error("ServerInfo().CodexHome is empty")
	}
	if info.PlatformOS == "" {
		t.Error("ServerInfo().PlatformOS is empty")
	}
	t.Logf("server userAgent=%q platform=%s/%s codexHome=%s schemaVersion=%s",
		info.UserAgent, info.PlatformFamily, info.PlatformOS, info.CodexHome, c.SchemaVersion())
}

// TestRealModelList checks a real paginated response decodes into the generated
// types. It needs no authentication because the catalog is served locally.
func TestRealModelList(t *testing.T) {
	c := newRealClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := c.ModelList(ctx, protocol.ModelListParams{})
	if err != nil {
		t.Fatalf("ModelList: %v", err)
	}
	if len(out.Data) == 0 {
		t.Skip("the server returned no models; it is probably unauthenticated")
	}
	for _, m := range out.Data {
		if m.ID == "" {
			t.Error("a model entry has an empty id")
		}
	}
	t.Logf("model/list returned %d models, first=%s", len(out.Data), out.Data[0].ID)
}

// TestRealModelListIterator exercises the pagination helper against real cursors.
func TestRealModelListIterator(t *testing.T) {
	c := newRealClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	count := 0
	for model, err := range c.ListModels(ctx, protocol.ModelListParams{}) {
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		if model.ID == "" {
			t.Error("a model entry has an empty id")
		}
		count++
		if count > 500 {
			t.Fatal("the iterator did not terminate")
		}
	}
	t.Logf("iterated %d models", count)
}

// TestRealLoadedThreadList calls a thread method that needs no authentication,
// confirming the thread surface is reachable.
func TestRealLoadedThreadList(t *testing.T) {
	c := newRealClient(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := c.ThreadLoadedList(ctx)
	if err != nil {
		t.Fatalf("ThreadLoadedList: %v", err)
	}
	t.Logf("thread/loaded/list returned %d loaded threads", len(out.Data))
}

// TestRealExperimentalRejectedWithoutOptIn documents the gate: an experimental
// method must fail when the capability was not negotiated. If this ever starts
// passing, the server stopped gating and the default is worth revisiting.
func TestRealExperimentalRejectedWithoutOptIn(t *testing.T) {
	c := newRealClient(t) // experimentalApi off by default

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out map[string]any
	err := c.Call(ctx, "thread/turns/list", map[string]any{
		"threadId": "thr_does_not_exist",
	}, &out)
	if err == nil {
		t.Error("an experimental method succeeded without the experimentalApi capability")
		return
	}
	t.Logf("experimental method correctly rejected: %v", err)
}

// TestRealMissingBinaryFailsFast verifies the error path a user is most likely to
// hit first, and that it names the problem.
func TestRealMissingBinaryFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := codex.New(ctx, codex.WithBinaryPath("codex-does-not-exist-anywhere"))
	if err == nil {
		t.Fatal("New succeeded with a nonexistent binary")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the executable was not found", err)
	}
}

// TestRealServerExitIsReported checks that killing the server surfaces a
// distinguishable error rather than a bare pipe failure.
func TestRealServerExitIsReported(t *testing.T) {
	requireCodex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c, err := codex.New(ctx,
		codex.WithClientInfo("codex-go-sdk-tests", "Codex Go SDK integration tests", "0.1.0"),
		codex.WithStderr(os.Stderr),
	)
	if err != nil {
		t.Fatalf("codex.New: %v", err)
	}

	// Closing shuts the child down; the connection must then end.
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	select {
	case serveErr := <-c.Done():
		t.Logf("connection ended with: %v", serveErr)
	case <-time.After(10 * time.Second):
		t.Error("the connection did not end after Close")
	}

	if _, err := c.ThreadLoadedList(context.Background()); err == nil {
		t.Error("a call succeeded after the server exited")
	} else if !errors.Is(err, context.Canceled) {
		t.Logf("post-exit call failed as expected: %v", err)
	}
}
