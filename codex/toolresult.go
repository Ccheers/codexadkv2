package codex

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ToolResultGuard bounds how much of one tool call's result may enter the model's
// context.
//
// A tool's return value is the only part of a turn whose size this SDK does not
// control: handlers wrap shell output, HTTP responses, file contents, and results
// proxied in from another process. One verbose handler can therefore evict the
// entire conversation, and nothing on the wire pushes back — the server accepts
// whatever content items the client answers a tool call with.
//
// The guard is a floor, not a formatter. A result that fits the budget passes
// through byte for byte, so well-behaved tools never see it. Only oversized
// results are reshaped, and even then the full text is preserved on disk with its
// path handed to the model: truncation costs context, not information.
//
// The zero value is usable and is what dispatch applies unless the session was
// opened WithToolResultSpillDir.
type ToolResultGuard struct {
	// SpillDir is the directory oversized results are written to. Empty means a
	// "codex-tool-results" subdirectory of os.TempDir().
	//
	// Point it somewhere the agent can actually read — a host that runs the model
	// inside a sandbox usually wants this under the thread's working directory,
	// because a path outside it cannot be opened no matter how correctly it is
	// reported.
	SpillDir string
}

const (
	// toolResultBudget is the byte ceiling for a single tool result. Above it the
	// result is reshaped to head plus tail lines and spilled to a file.
	//
	// Bytes rather than tokens because that is what the caller can measure without
	// a tokenizer, and 20KiB is comfortably under any model's per-message limit
	// while still holding a few hundred lines of ordinary command output.
	toolResultBudget = 20 << 10

	// toolResultHeadLines and toolResultTailLines are what survives in context.
	// Head and tail rather than a plain prefix: the interesting part of a long
	// result is usually how it started and how it ended (a build log's error is at
	// the bottom, a command's usage banner at the top), and a prefix-only cut loses
	// exactly the half the model needs.
	toolResultHeadLines = 20
	toolResultTailLines = 20

	// toolResultLineRunes caps one surviving line. A result may be huge because of
	// line count or because of a single enormous line (minified JSON, a hexdump);
	// the line budget covers the second case, which the line counts cannot.
	//
	// Runes, not bytes: the cap exists to bound what the model has to read, and
	// counting bytes would let a CJK result keep only a third as much text.
	toolResultLineRunes = 500

	// toolResultSpillSubdir is the fallback spill location under os.TempDir().
	toolResultSpillSubdir = "codex-tool-results"
)

// Apply returns result unchanged when it fits the budget. Otherwise it writes the
// full result to a file under SpillDir and returns the head and tail lines, each
// capped, followed by a note carrying the file path.
//
// tool is the name the model called, used only in the note so a truncated result
// says which tool produced it.
//
// Apply never fails. A spill error is reported inside the note instead of being
// returned, because the alternative — failing the tool call — would turn "this
// result is too big to show" into "the tool did not work", which is a lie about
// the handler's outcome.
func (g ToolResultGuard) Apply(tool, result string) string {
	if len(result) <= toolResultBudget {
		return result
	}

	// Trailing newlines are trimmed before splitting: command output almost always
	// ends with one, and Split would turn it into a final empty line that spends one
	// of the tail slots on nothing and reports one line more than the result has.
	lines := strings.Split(strings.TrimRight(result, "\n"), "\n")
	head, tail := toolResultHeadLines, toolResultTailLines
	if len(lines) <= head+tail {
		// Fewer lines than the two windows together: show them all and let the line
		// cap do the bounding. Printing an omission marker here would claim lines
		// were dropped when none were.
		head, tail = len(lines), 0
	}

	var b strings.Builder
	for _, line := range lines[:head] {
		writeCappedLine(&b, line)
	}
	if omitted := len(lines) - head - tail; omitted > 0 {
		fmt.Fprintf(&b, "\n... [%d lines omitted] ...\n\n", omitted)
	}
	for _, line := range lines[len(lines)-tail:] {
		writeCappedLine(&b, line)
	}

	path, err := g.spill(result)
	if err != nil {
		// Name the failure rather than point at a file that does not exist: a path
		// the model cannot open is worse than no path, because it will try.
		return b.String() + fmt.Sprintf(
			"\n[%s result truncated: %d bytes, %d lines; lines capped at %d runes; "+
				"the full result could NOT be saved: %v]",
			tool, len(result), len(lines), toolResultLineRunes, err)
	}
	return b.String() + fmt.Sprintf(
		"\n[%s result truncated: %d bytes, %d lines; lines capped at %d runes; "+
			"the full result is saved at %s — read that file if you need the omitted middle]",
		tool, len(result), len(lines), toolResultLineRunes, path)
}

// writeCappedLine appends one line to b, cut to the line budget, with its own
// terminator restored.
func writeCappedLine(b *strings.Builder, line string) {
	// \r\n endings survive Split("\n") as a trailing \r; trimming it before the
	// rune count keeps the cap about visible characters, and the marker below would
	// otherwise sit after the carriage return.
	line = strings.TrimSuffix(line, "\r")
	if r := []rune(line); len(r) > toolResultLineRunes {
		fmt.Fprintf(b, "%s… [%d runes]\n", string(r[:toolResultLineRunes]), len(r))
		return
	}
	b.WriteString(line)
	b.WriteByte('\n')
}

// spill writes the full result under SpillDir and returns its absolute path.
//
// The name is the content hash, so the same result spilled twice is one file and
// an already-present file can be reused without reading it. Tool names and call
// ids are deliberately not part of it: both are caller-supplied strings that would
// need sanitizing before they were safe as path segments, and the note the model
// receives already names the tool.
func (g ToolResultGuard) spill(result string) (string, error) {
	dir := g.SpillDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), toolResultSpillSubdir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create spill dir: %w", err)
	}
	sum := sha256.Sum256([]byte(result))
	path := filepath.Join(dir, hex.EncodeToString(sum[:8])+".txt")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	// Write aside then rename: a reader that lands on the final path must never see
	// half a result, and the model is told to read this file by path.
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(result); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", fmt.Errorf("write file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("rename file: %w", err)
	}
	return path, nil
}
