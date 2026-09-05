package codex_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ccheers/codexadkv2/codex"
	"github.com/ccheers/codexadkv2/codex/protocol"
)

// The guard's own constants, restated: they are the contract a host relies on when
// it sizes its context budget, so a change here must fail a test rather than slip
// through as "still truncated".
const (
	guardBudget    = 20 << 10
	guardHeadLines = 20
	guardTailLines = 20
	guardLineRunes = 500
)

// numberedLines builds an n-line result whose every line is unique, so a test can
// tell a surviving line from an omitted one by content rather than by counting.
func numberedLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %04d\n", i)
	}
	return b.String()
}

// TestToolResultGuardPassesSmallResultsThrough: the guard must be invisible below
// the budget. A tool that behaves has its answer delivered byte for byte — any
// reshaping here would be a formatter, not a floor.
func TestToolResultGuardPassesSmallResultsThrough(t *testing.T) {
	want := numberedLines(100)
	if len(want) > guardBudget {
		t.Fatalf("test fixture is %d bytes, already over the %d budget", len(want), guardBudget)
	}
	dir := t.TempDir()
	got := codex.ToolResultGuard{SpillDir: dir}.Apply("small", want)
	if got != want {
		t.Errorf("a result under the budget was reshaped:\n got %q\nwant %q", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("an under-budget result spilled %d files; it must not touch the disk", len(entries))
	}
}

// TestToolResultGuardKeepsHeadAndTail is the core shape: an oversized result
// answers with its first and last lines, names what was dropped in between, and
// hands over a file holding the whole thing.
func TestToolResultGuardKeepsHeadAndTail(t *testing.T) {
	const total = 3000
	full := numberedLines(total)
	if len(full) <= guardBudget {
		t.Fatalf("fixture is %d bytes, want over %d", len(full), guardBudget)
	}
	dir := t.TempDir()
	got := codex.ToolResultGuard{SpillDir: dir}.Apply("build", full)

	if len(got) >= len(full) {
		t.Errorf("truncated result is %d bytes, original %d; the guard grew it", len(got), len(full))
	}
	for i := 0; i < guardHeadLines; i++ {
		if want := fmt.Sprintf("line %04d", i); !strings.Contains(got, want) {
			t.Errorf("head line %q is missing from the result", want)
		}
	}
	for i := total - guardTailLines; i < total; i++ {
		if want := fmt.Sprintf("line %04d", i); !strings.Contains(got, want) {
			t.Errorf("tail line %q is missing from the result", want)
		}
	}
	// The middle is what the budget exists to keep out.
	if strings.Contains(got, "line 0020") || strings.Contains(got, "line 1500") {
		t.Error("the omitted middle leaked into the truncated result")
	}
	want := fmt.Sprintf("[%d lines omitted]", total-guardHeadLines-guardTailLines)
	if !strings.Contains(got, want) {
		t.Errorf("result does not say what was dropped, want %q in:\n%s", want, got)
	}
	if !strings.Contains(got, "build result truncated") {
		t.Errorf("the note does not name the tool that produced it:\n%s", got)
	}

	// The spilled file is the whole result, or the note is a promise the model
	// cannot cash.
	path := assertSpilledFile(t, dir, full)
	if !strings.Contains(got, path) {
		t.Errorf("the note does not carry the spill path %q:\n%s", path, got)
	}
}

// TestToolResultGuardCapsLongLines covers the other way a result gets big: a
// handful of enormous lines. The line windows cannot bound that, so each surviving
// line is cut on its own — and cut by rune, because the cap is about what the model
// reads, not about bytes.
func TestToolResultGuardCapsLongLines(t *testing.T) {
	long := strings.Repeat("漢", guardLineRunes*8) // 4000 runes, 12000 bytes each
	full := strings.Join([]string{long, long, long}, "\n")
	if len(full) <= guardBudget {
		t.Fatalf("fixture is %d bytes, want over %d", len(full), guardBudget)
	}
	dir := t.TempDir()
	got := codex.ToolResultGuard{SpillDir: dir}.Apply("dump", full)

	if strings.Contains(got, "lines omitted") {
		t.Errorf("no line was dropped, but the result claims some were:\n%s", got)
	}
	if n := strings.Count(got, fmt.Sprintf("… [%d runes]", guardLineRunes*8)); n != 3 {
		t.Errorf("%d lines carry a rune-cap marker, want 3:\n%s", n, got)
	}
	if n := strings.Count(got, "漢"); n > 3*guardLineRunes {
		t.Errorf("%d runes survived, want at most %d (3 lines at the cap)", n, 3*guardLineRunes)
	}
	assertSpilledFile(t, dir, full)
}

// TestToolResultGuardReportsSpillFailure: when the full result cannot be saved, the
// note must say so. Pointing the model at a path that does not exist is worse than
// no path — it will try to read it and lose a turn.
func TestToolResultGuardReportsSpillFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	full := numberedLines(3000)
	got := codex.ToolResultGuard{SpillDir: blocker}.Apply("build", full)

	if !strings.Contains(got, "could NOT be saved") {
		t.Errorf("a failed spill was not reported:\n%s", got)
	}
	// The mkdir error legitimately names the path it failed on; what must not appear
	// is a claim that the result is there to be read.
	if strings.Contains(got, "is saved at") {
		t.Errorf("the note claims a saved result that does not exist:\n%s", got)
	}
	// Truncation still happened: the context is protected even when the spill is not.
	if len(got) >= len(full) {
		t.Errorf("truncated result is %d bytes, original %d", len(got), len(full))
	}
}

// TestToolResultGuardDefaultSpillDir: the zero guard is usable, because the point
// is that no caller can forget it. It lands under the temp dir.
func TestToolResultGuardDefaultSpillDir(t *testing.T) {
	full := numberedLines(3000)
	got := codex.ToolResultGuard{}.Apply("build", full)

	// The note ends with the path, so the fallback location is found by looking for
	// it rather than by parsing the sentence.
	wantDir := filepath.Join(os.TempDir(), "codex-tool-results")
	at := strings.LastIndex(got, wantDir)
	if at < 0 {
		t.Fatalf("the note does not mention the default spill dir %q:\n%s", wantDir, got)
	}
	path := got[at:]
	if end := strings.IndexAny(path, " \n"); end >= 0 {
		path = path[:end]
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the default spill dir did not hold the result: %v", err)
	}
	if string(raw) != full {
		t.Errorf("spilled file is %d bytes, want the full %d", len(raw), len(full))
	}
}

// assertSpilledFile checks the spill dir holds exactly one file and that it is the
// full result, and returns its path.
func assertSpilledFile(t *testing.T, dir, full string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading spill dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("spill dir holds %d entries, want exactly the one result", len(entries))
	}
	path := filepath.Join(dir, entries[0].Name())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading spilled file: %v", err)
	}
	if string(raw) != full {
		t.Errorf("spilled file is %d bytes, want the full %d bytes", len(raw), len(full))
	}
	return path
}

// TestSessionGuardsOversizedToolResults is the wiring: the guard is not opt-in per
// tool, it is what dispatch answers with. Both branches — a huge result and a huge
// failure reason — must come back bounded, or one verbose handler can still evict
// the conversation.
func TestSessionGuardsOversizedToolResults(t *testing.T) {
	srv := newFakeServer(t)
	srv.reply("thread/start", map[string]any{
		"thread": map[string]any{"id": "thr_1", "sessionId": "thr_1"},
		"model":  "m", "modelProvider": "openai", "cwd": "/r",
		"approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"},
	})

	full := numberedLines(3000)
	dir := t.TempDir()
	session, err := codex.Open(context.Background(),
		codex.WithTransport(srv),
		codex.WithToolResultSpillDir(dir),
		codex.WithTools(
			codex.NewTool("verbose", "Returns far too much",
				func(context.Context, string, grepArgs) (string, error) { return full, nil }),
			codex.NewTool("loud", "Fails with far too much",
				func(context.Context, string, grepArgs) (string, error) {
					return "", errors.New(full)
				}),
		),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	srv.request(1, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c1",
		"tool": "verbose", "arguments": map[string]any{},
	})
	frame := srv.waitForClientResponse()
	result, _ := frame["result"].(map[string]any)
	if result == nil || result["success"] != true {
		t.Fatalf("result = %v, want success=true", frame["result"])
	}
	text := toolResponseText(t, result)
	if len(text) >= len(full) {
		t.Errorf("the session answered with %d bytes, the tool returned %d; dispatch did not guard it",
			len(text), len(full))
	}
	if !strings.Contains(text, dir) {
		t.Errorf("the answer does not point at the configured spill dir %q:\n%s", dir, text)
	}
	assertSpilledFile(t, dir, full)

	// The failure branch carries handler-written text too.
	srv.request(2, protocol.ServerMethodItemToolCall, map[string]any{
		"threadId": "thr_1", "turnId": "turn_1", "callId": "c2",
		"tool": "loud", "arguments": map[string]any{},
	})
	failed, _ := srv.waitForClientResponse()["result"].(map[string]any)
	if failed == nil || failed["success"] != false {
		t.Fatalf("result = %v, want success=false", failed)
	}
	if failText := toolResponseText(t, failed); len(failText) >= len(full) {
		t.Errorf("a %d-byte failure reason reached the model unbounded", len(full))
	}
}
