package parser

import (
	"bufio"
	"errors"
	"os"
	"testing"
	"time"
)

// TestParseFixture walks the scrubbed session_sample.jsonl fixture and
// verifies the parser produces exactly 4 billable events out of 9
// lines. The fixture is drawn from real Claude Code JSONL structure
// (with paths/uuids scrubbed) and exercises every line type the
// parser must handle.
func TestParseFixture(t *testing.T) {
	f, err := os.Open("testdata/session_sample.jsonl")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	var events []*Event
	scanner := bufio.NewScanner(f)
	// Claude Code JSONL lines can be large (one tool call with a lot
	// of output can easily exceed the default 64 KB buffer).
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	lineNum := 0
	for scanner.Scan() {
		lineNum++
		e, err := Parse(scanner.Bytes())
		if err != nil {
			t.Fatalf("line %d: parse error: %v", lineNum, err)
		}
		if e != nil {
			events = append(events, e)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	if got, want := len(events), 4; got != want {
		t.Fatalf("expected %d billable events, got %d", want, got)
	}

	// Event 1: opus-4-6, main branch, with cache creation split
	e := events[0]
	if e.UUID != "a1111111-1111-1111-1111-111111111111" {
		t.Errorf("event[0].UUID = %q", e.UUID)
	}
	if e.Model != "claude-opus-4-6" {
		t.Errorf("event[0].Model = %q", e.Model)
	}
	if e.GitBranch != "main" {
		t.Errorf("event[0].GitBranch = %q", e.GitBranch)
	}
	if e.Project != "myapp" {
		t.Errorf("event[0].Project = %q, want myapp", e.Project)
	}
	if e.InputTokens != 100 {
		t.Errorf("event[0].InputTokens = %d, want 100", e.InputTokens)
	}
	if e.OutputTokens != 50 {
		t.Errorf("event[0].OutputTokens = %d, want 50", e.OutputTokens)
	}
	if e.CacheCreation5mTokens != 4000 {
		t.Errorf("event[0].CacheCreation5mTokens = %d, want 4000", e.CacheCreation5mTokens)
	}
	if e.CacheCreation1hTokens != 1000 {
		t.Errorf("event[0].CacheCreation1hTokens = %d, want 1000", e.CacheCreation1hTokens)
	}
	if e.CacheReadTokens != 0 {
		t.Errorf("event[0].CacheReadTokens = %d, want 0", e.CacheReadTokens)
	}
	if e.ServiceTier != "standard" {
		t.Errorf("event[0].ServiceTier = %q", e.ServiceTier)
	}
	wantTS, _ := time.Parse(time.RFC3339, "2026-03-17T07:51:43.567Z")
	if !e.Timestamp.Equal(wantTS) {
		t.Errorf("event[0].Timestamp = %v, want %v", e.Timestamp, wantTS)
	}

	// Event 2: haiku, pure cache-read session
	e = events[1]
	if e.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("event[1].Model = %q", e.Model)
	}
	if e.CacheReadTokens != 5100 {
		t.Errorf("event[1].CacheReadTokens = %d, want 5100", e.CacheReadTokens)
	}
	if e.InputTokens != 20 {
		t.Errorf("event[1].InputTokens = %d, want 20", e.InputTokens)
	}

	// Event 3: sonnet on a feature branch
	e = events[2]
	if e.Model != "claude-sonnet-4-6" {
		t.Errorf("event[2].Model = %q", e.Model)
	}
	if e.GitBranch != "feature/x" {
		t.Errorf("event[2].GitBranch = %q", e.GitBranch)
	}

	// Event 4: opus on detached HEAD
	e = events[3]
	if e.GitBranch != "HEAD" {
		t.Errorf("event[3].GitBranch = %q, want HEAD (detached)", e.GitBranch)
	}
	if e.InputTokens != 50 || e.OutputTokens != 25 {
		t.Errorf("event[3] tokens = %d in / %d out", e.InputTokens, e.OutputTokens)
	}

	// Note: the synthetic assistant line (model="<synthetic>") is
	// absent from the slice. That's the contract.
}

// TestParseSkipsNonBillable confirms that every line type other than
// assistant returns (nil, nil). Blank lines and whitespace also skip.
func TestParseSkipsNonBillable(t *testing.T) {
	cases := []struct {
		name string
		line string
	}{
		{"progress", `{"type":"progress","uuid":"x"}`},
		{"user", `{"type":"user","uuid":"x","message":{"role":"user","content":"hi"}}`},
		{"file-history-snapshot", `{"type":"file-history-snapshot","messageId":"m"}`},
		{"system", `{"type":"system","uuid":"x","subtype":"turn_duration"}`},
		{"queue-operation", `{"type":"queue-operation","uuid":"x"}`},
		{"pr-link", `{"type":"pr-link","uuid":"x"}`},
		{"last-prompt", `{"type":"last-prompt","uuid":"x"}`},
		{"empty", ``},
		{"whitespace", `   `},
		{"newline", "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Parse([]byte(tc.line))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if e != nil {
				t.Errorf("expected nil event, got %+v", e)
			}
		})
	}
}

// TestParseSkipsSynthetic documents the <synthetic> skip rule so a
// future refactor can't accidentally start billing framework internals.
func TestParseSkipsSynthetic(t *testing.T) {
	line := `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"2026-04-09T00:00:00Z","message":{"model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}`
	e, err := Parse([]byte(line))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if e != nil {
		t.Error("synthetic assistant messages must be skipped")
	}
}

// TestParseMalformedJSON verifies that a non-JSON payload produces
// ErrMalformed (wrapped, checkable via errors.Is).
func TestParseMalformedJSON(t *testing.T) {
	_, err := Parse([]byte(`{"type":"assistant"`)) // unterminated
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// TestParseMissingRequiredFields verifies each of the three required
// fields (uuid, model, timestamp) produces a targeted error message
// mentioning which one is missing.
func TestParseMissingRequiredFields(t *testing.T) {
	cases := map[string]string{
		"no uuid":      `{"type":"assistant","sessionId":"s","timestamp":"2026-04-09T00:00:00Z","message":{"model":"claude-opus-4-6"}}`,
		"no model":     `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"2026-04-09T00:00:00Z","message":{}}`,
		"no timestamp": `{"type":"assistant","uuid":"a","sessionId":"s","message":{"model":"claude-opus-4-6"}}`,
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(line))
			if !errors.Is(err, ErrMalformed) {
				t.Errorf("expected ErrMalformed, got %v", err)
			}
		})
	}
}

// TestParseBadTimestamp verifies non-RFC3339 timestamps are rejected.
func TestParseBadTimestamp(t *testing.T) {
	line := `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"not-a-date","message":{"model":"claude-opus-4-6","usage":{}}}`
	_, err := Parse([]byte(line))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("expected ErrMalformed, got %v", err)
	}
}

// TestParseDefaultsTier documents that an empty service_tier defaults
// to "standard". This matches the 29 real-world cases we saw with
// null tier on synthetic messages (though synthetics are skipped
// before this code path runs, the default still matters for future
// Claude Code versions).
func TestParseDefaultsTier(t *testing.T) {
	line := `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"2026-04-09T00:00:00Z","cwd":"/home/u/p","gitBranch":"main","message":{"model":"claude-opus-4-6","usage":{"input_tokens":1,"output_tokens":1}}}`
	e, err := Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if e == nil {
		t.Fatal("expected event, got nil")
	}
	if e.ServiceTier != "standard" {
		t.Errorf("empty service_tier should default to standard, got %q", e.ServiceTier)
	}
}

// TestParseUTCNormalization verifies timestamps are converted to UTC
// regardless of input zone. Internal comparisons rely on UTC.
func TestParseUTCNormalization(t *testing.T) {
	line := `{"type":"assistant","uuid":"a","sessionId":"s","timestamp":"2026-04-09T10:00:00+07:00","cwd":"/home/u/p","gitBranch":"main","message":{"model":"claude-opus-4-6","usage":{"input_tokens":1}}}`
	e, err := Parse([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if loc := e.Timestamp.Location().String(); loc != "UTC" {
		t.Errorf("timestamp should be UTC, got %s", loc)
	}
	if h := e.Timestamp.Hour(); h != 3 {
		t.Errorf("10:00 +07:00 should become 03:00 UTC, got %d:00", h)
	}
}

// TestProjectFromCWD exhausts the edge cases the helper must handle.
func TestProjectFromCWD(t *testing.T) {
	cases := map[string]string{
		"/home/user/myapp":  "myapp",
		"/Users/x/dev/proj": "proj",
		"/foo/bar/baz/":     "baz",
		"":                  "unknown",
		"/":                 "unknown",
		".":                 "unknown",
		"relative-only":     "relative-only",
	}
	for cwd, want := range cases {
		got := projectFromCWD(cwd)
		if got != want {
			t.Errorf("projectFromCWD(%q) = %q, want %q", cwd, got, want)
		}
	}
}
