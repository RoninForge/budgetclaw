package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RoninForge/budgetclaw/internal/parser"
)

// eventCollector is a thread-safe sink for parsed events that
// tests can drain synchronously.
type eventCollector struct {
	mu     sync.Mutex
	events []*parser.Event
}

func (c *eventCollector) Handler() Handler {
	return func(_ context.Context, e *parser.Event, _ string) error {
		c.mu.Lock()
		c.events = append(c.events, e)
		c.mu.Unlock()
		return nil
	}
}

func (c *eventCollector) snapshot() []*parser.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*parser.Event, len(c.events))
	copy(out, c.events)
	return out
}

// waitForCount polls until c has at least n events or timeout fires.
// Returns the final snapshot either way so callers can assert.
func (c *eventCollector) waitForCount(t *testing.T, n int, timeout time.Duration) []*parser.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		got := c.snapshot()
		if len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return c.snapshot()
}

// validAssistantLine returns a JSONL line that Parse accepts.
// Varying uuid lets a single test write multiple distinct events.
func validAssistantLine(uuid, branch string) string {
	return `{"type":"assistant","uuid":"` + uuid +
		`","sessionId":"s1","timestamp":"2026-04-09T12:00:00Z","cwd":"/home/u/myapp","gitBranch":"` + branch +
		`","message":{"model":"claude-opus-4-6","usage":{"input_tokens":100,"output_tokens":50,"service_tier":"standard"}}}` + "\n"
}

// startWatcher boots a watcher rooted at dir in a background
// goroutine and returns a cleanup func that cancels and waits.
func startWatcher(t *testing.T, dir string, handler Handler) (*Watcher, func()) {
	t.Helper()
	w, err := New(dir, handler, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Give Run a moment to complete the initial scan and set up
	// the event loop before the test writes files.
	time.Sleep(50 * time.Millisecond)

	cleanup := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("watcher did not stop within 2s")
		}
		_ = w.Close()
	}
	return w, cleanup
}

// TestNewRejectsMissingRoot — the watcher cannot operate without
// $HOME/.claude/projects or its test equivalent.
func TestNewRejectsMissingRoot(t *testing.T) {
	_, err := New(filepath.Join(t.TempDir(), "nope"), func(context.Context, *parser.Event, string) error { return nil }, Options{})
	if err == nil {
		t.Fatal("expected error on missing root")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

// TestNewRejectsFileRoot — a file path where we expected a
// directory should be reported clearly.
func TestNewRejectsFileRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := New(path, func(context.Context, *parser.Event, string) error { return nil }, Options{})
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("expected 'not a directory' error, got %v", err)
	}
}

// TestNewRejectsNilHandler prevents misconfigured watchers.
func TestNewRejectsNilHandler(t *testing.T) {
	_, err := New(t.TempDir(), nil, Options{})
	if err == nil {
		t.Error("expected error on nil handler")
	}
}

// TestInitialScanProcessesExistingFiles — when the watcher starts
// on a directory that already has JSONL files, those files'
// contents should be parsed before Run begins waiting for events.
func TestInitialScanProcessesExistingFiles(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate a JSONL file with two events.
	preFile := filepath.Join(dir, "existing.jsonl")
	if err := os.WriteFile(preFile,
		[]byte(validAssistantLine("pre-1", "main")+validAssistantLine("pre-2", "main")),
		0o644); err != nil {
		t.Fatal(err)
	}

	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	got := c.waitForCount(t, 2, 500*time.Millisecond)
	if len(got) != 2 {
		t.Fatalf("expected 2 events from initial scan, got %d", len(got))
	}
	uuids := map[string]bool{got[0].UUID: true, got[1].UUID: true}
	if !uuids["pre-1"] || !uuids["pre-2"] {
		t.Errorf("unexpected uuids: %v", uuids)
	}
}

// TestWatcherProcessesNewFile — after Run is established, writing
// a brand-new JSONL file should produce events.
func TestWatcherProcessesNewFile(t *testing.T) {
	dir := t.TempDir()
	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	// Write a fresh file with one event.
	if err := os.WriteFile(filepath.Join(dir, "new.jsonl"),
		[]byte(validAssistantLine("new-1", "main")), 0o644); err != nil {
		t.Fatal(err)
	}

	got := c.waitForCount(t, 1, 2*time.Second)
	if len(got) != 1 || got[0].UUID != "new-1" {
		t.Errorf("expected new-1, got %+v", got)
	}
}

// TestWatcherTailsAppends — appending to an existing file should
// surface the new events without re-emitting the old ones.
func TestWatcherTailsAppends(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(validAssistantLine("a-1", "main")), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	// Initial scan picks up a-1.
	c.waitForCount(t, 1, 1*time.Second)

	// Append two more events.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(validAssistantLine("a-2", "main") + validAssistantLine("a-3", "feature/x")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got := c.waitForCount(t, 3, 2*time.Second)
	if len(got) != 3 {
		t.Fatalf("expected 3 events total, got %d", len(got))
	}

	// Verify uuids without asserting order (fsnotify and scanner
	// ordering within a single Write should be stable, but we
	// don't need to lock that in).
	seen := map[string]bool{}
	for _, e := range got {
		seen[e.UUID] = true
	}
	for _, want := range []string{"a-1", "a-2", "a-3"} {
		if !seen[want] {
			t.Errorf("missing event %q, got %v", want, seen)
		}
	}
}

// TestWatcherSkipsPartialLines — a Write event that delivers
// incomplete bytes (no trailing newline) should not produce a
// parse error. The partial is consumed on the next Write.
func TestWatcherSkipsPartialLines(t *testing.T) {
	dir := t.TempDir()
	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	path := filepath.Join(dir, "partial.jsonl")

	// Split one valid line into two writes, with the split at
	// exactly the halfway point of the JSON.
	line := validAssistantLine("partial-1", "main")
	half := len(line) / 2

	// First write: first half, no trailing newline. Watcher should
	// see nothing billable from this.
	if err := os.WriteFile(path, []byte(line[:half]), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if got := c.snapshot(); len(got) != 0 {
		t.Errorf("expected 0 events after partial write, got %d", len(got))
	}

	// Second write: append the rest (including trailing newline).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line[half:]); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got := c.waitForCount(t, 1, 2*time.Second)
	if len(got) != 1 || got[0].UUID != "partial-1" {
		t.Errorf("expected partial-1 after completion, got %+v", got)
	}
}

// TestWatcherHandlesNewSubdirectories — Claude Code creates new
// project subdirectories (and subagents/ dirs inside them). The
// watcher must recursively pick them up without restart.
func TestWatcherHandlesNewSubdirectories(t *testing.T) {
	dir := t.TempDir()
	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	subdir := filepath.Join(dir, "new-project", "subagents")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Writing a JSONL file inside the brand-new subdirectory
	// should still trigger events.
	if err := os.WriteFile(filepath.Join(subdir, "s.jsonl"),
		[]byte(validAssistantLine("sub-1", "main")), 0o644); err != nil {
		t.Fatal(err)
	}

	got := c.waitForCount(t, 1, 3*time.Second)
	if len(got) != 1 || got[0].UUID != "sub-1" {
		t.Errorf("expected sub-1, got %+v", got)
	}
}

// TestWatcherSkipsNonJSONL — arbitrary file creation inside the
// root should not produce parse errors.
func TestWatcherSkipsNonJSONL(t *testing.T) {
	dir := t.TempDir()
	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	if err := os.WriteFile(filepath.Join(dir, "README.md"),
		[]byte("# not a session log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	if got := c.snapshot(); len(got) != 0 {
		t.Errorf("expected 0 events for non-JSONL file, got %d", len(got))
	}
}

// TestWatcherHandlerErrorsDoNotAbort — a handler that always
// errors should not kill the watch loop. Subsequent events must
// still reach the handler.
func TestWatcherHandlerErrorsDoNotAbort(t *testing.T) {
	dir := t.TempDir()

	var count int
	var mu sync.Mutex
	handler := func(_ context.Context, _ *parser.Event, _ string) error {
		mu.Lock()
		count++
		mu.Unlock()
		return errors.New("deliberate test error")
	}

	_, stop := startWatcher(t, dir, handler)
	defer stop()

	// Two events in two separate writes.
	path := filepath.Join(dir, "errs.jsonl")
	if err := os.WriteFile(path, []byte(validAssistantLine("e-1", "main")), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	_, _ = f.WriteString(validAssistantLine("e-2", "main"))
	_ = f.Close()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		c := count
		mu.Unlock()
		if c >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Errorf("handler call count = %d, want 2", count)
}

// TestWatcherSkipsMalformedJSON — a non-JSON line in the middle
// of a file should be logged and skipped without affecting the
// surrounding valid lines.
func TestWatcherSkipsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	body := validAssistantLine("ok-1", "main") +
		"this is not json\n" +
		validAssistantLine("ok-2", "main")

	if err := os.WriteFile(filepath.Join(dir, "mix.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	got := c.waitForCount(t, 2, 2*time.Second)
	if len(got) != 2 {
		t.Errorf("expected 2 valid events, got %d", len(got))
	}
}

// TestWatcherResetsOnTruncation — if a file is truncated below
// the watcher's offset, the next Write should reset to 0 and
// reprocess from the beginning.
func TestWatcherResetsOnTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rot.jsonl")

	// Write two events first.
	if err := os.WriteFile(path,
		[]byte(validAssistantLine("r-1", "main")+validAssistantLine("r-2", "main")),
		0o644); err != nil {
		t.Fatal(err)
	}

	c := &eventCollector{}
	_, stop := startWatcher(t, dir, c.Handler())
	defer stop()

	c.waitForCount(t, 2, 1*time.Second)

	// Truncate and re-write with one new event.
	if err := os.WriteFile(path, []byte(validAssistantLine("r-fresh", "main")), 0o644); err != nil {
		t.Fatal(err)
	}

	got := c.waitForCount(t, 3, 2*time.Second)
	// We expect 3 total: the two originals from initial scan + the
	// "fresh" event from after truncation. (r-1 and r-2 are NOT
	// reprocessed because the db layer dedupes by uuid — but the
	// watcher itself does re-emit them when the file is reset.
	// That's fine: the dedupe is the db's job.)
	//
	// Acceptable: 3 or more events (watcher may also re-emit
	// r-1/r-2 if the truncation reset exposes them). We only care
	// that r-fresh made it through.
	hasFresh := false
	for _, e := range got {
		if e.UUID == "r-fresh" {
			hasFresh = true
			break
		}
	}
	if !hasFresh {
		t.Errorf("expected r-fresh in events, got %+v", got)
	}
}

// TestWatcherRunExitsOnContextCancel — Run must return cleanly
// (no goroutine leak) when its context is canceled.
func TestWatcherRunExitsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, func(context.Context, *parser.Event, string) error { return nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want nil or context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of cancel")
	}
}

// TestCloseIdempotent — Close should be safe to call multiple
// times without panicking or returning an error.
func TestCloseIdempotent(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, func(context.Context, *parser.Event, string) error { return nil }, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
