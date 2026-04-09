// Package parser extracts billable events from Claude Code session JSONL logs.
//
// Claude Code writes one JSON object per line to files under
// $HOME/.claude/projects/<project>/*.jsonl (and subagents/ subdirs).
// Every line has a "type" field. Only lines with type:"assistant"
// carry token usage; the rest (progress, user, system, file-history-snapshot,
// queue-operation, pr-link, last-prompt) are skipped silently.
//
// Schema observations from real data (2026-04-09, n=38,911 assistant
// events across 727 files in 3 projects):
//
//   - Every assistant line has "cwd", "gitBranch", "uuid", "sessionId",
//     "timestamp", and "message.model". We trust these and fail loudly
//     if any are missing.
//   - Claude Code writes gitBranch directly — we do NOT walk .git/HEAD.
//     Detached HEAD shows up literally as "HEAD".
//   - Models seen: claude-opus-4-6, claude-sonnet-4-6,
//     claude-sonnet-4-5-20250929, claude-haiku-4-5-20251001, <synthetic>.
//   - The "<synthetic>" model with null service_tier represents Claude
//     Code framework internals that Anthropic does not bill. We skip them.
//   - cache_creation_input_tokens is the total; the breakdown into
//     ephemeral_5m_input_tokens and ephemeral_1h_input_tokens lives in
//     the nested "cache_creation" object. These have different prices
//     (1.25× vs 2× input rate) so we split them here.
//
// This package is deliberately pure: it takes bytes, returns events
// or errors. File tailing, deduplication, and attribution belong to
// other packages.
package parser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Event is one billable assistant message, flattened for downstream
// consumers (rollups, budget evaluator, enforcer).
type Event struct {
	// Identity
	UUID      string    // message uuid, unique per event (dedupe key)
	SessionID string    // Claude Code session uuid
	Timestamp time.Time // message timestamp in UTC

	// Attribution
	CWD       string // working directory from the JSONL line
	Project   string // basename of CWD
	GitBranch string // provided verbatim by Claude Code

	// Cost inputs
	Model                 string // e.g. "claude-opus-4-6"
	ServiceTier           string // "standard", "batch", "priority"; defaults to standard
	InputTokens           int
	OutputTokens          int
	CacheReadTokens       int
	CacheCreation5mTokens int // ephemeral 5-minute cache write
	CacheCreation1hTokens int // ephemeral 1-hour cache write
}

// ErrMalformed wraps errors for invalid JSONL lines or assistant
// lines missing required fields. Use errors.Is to check.
var ErrMalformed = errors.New("malformed JSONL line")

// rawLine mirrors the subset of Claude Code's JSONL schema we care
// about. json.Unmarshal silently drops unknown fields, so we stay
// resilient to Claude Code adding new ones.
type rawLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Message   struct {
		Model string `json:"model"`
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheCreation            struct {
				Ephemeral5m int `json:"ephemeral_5m_input_tokens"`
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
			ServiceTier string `json:"service_tier"`
		} `json:"usage"`
	} `json:"message"`
}

// Parse decodes a single JSONL line. It returns:
//
//	(event, nil) for a billable assistant message
//	(nil, nil)   for any non-billable or empty line (caller should skip)
//	(nil, err)   for malformed JSON or an assistant line missing required fields
//
// Callers iterating a file line-by-line should treat (nil, nil) as
// "keep going" and log-and-continue on errors.
func Parse(line []byte) (*Event, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil
	}

	var r rawLine
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	// Only assistant lines are billable. Everything else is noise.
	if r.Type != "assistant" {
		return nil, nil
	}

	// Synthetic messages are Claude Code framework internals that
	// Anthropic does not bill. They show up with model="<synthetic>"
	// and service_tier=null. Skip them to keep cost math clean.
	// Any future sentinel Anthropic adds will follow the same
	// angle-bracket convention.
	if strings.HasPrefix(r.Message.Model, "<") {
		return nil, nil
	}

	if r.UUID == "" {
		return nil, fmt.Errorf("%w: assistant line missing uuid", ErrMalformed)
	}
	if r.Message.Model == "" {
		return nil, fmt.Errorf("%w: assistant line missing model", ErrMalformed)
	}
	if r.Timestamp == "" {
		return nil, fmt.Errorf("%w: assistant line missing timestamp", ErrMalformed)
	}

	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: bad timestamp %q: %v", ErrMalformed, r.Timestamp, err)
	}

	tier := r.Message.Usage.ServiceTier
	if tier == "" {
		tier = "standard"
	}

	return &Event{
		UUID:      r.UUID,
		SessionID: r.SessionID,
		Timestamp: ts.UTC(),

		CWD:       r.CWD,
		Project:   projectFromCWD(r.CWD),
		GitBranch: r.GitBranch,

		Model:                 r.Message.Model,
		ServiceTier:           tier,
		InputTokens:           r.Message.Usage.InputTokens,
		OutputTokens:          r.Message.Usage.OutputTokens,
		CacheReadTokens:       r.Message.Usage.CacheReadInputTokens,
		CacheCreation5mTokens: r.Message.Usage.CacheCreation.Ephemeral5m,
		CacheCreation1hTokens: r.Message.Usage.CacheCreation.Ephemeral1h,
	}, nil
}

// projectFromCWD returns the basename of the working directory, which
// is what users expect to see in rollups ("IxDF-web" rather than the
// full "/Users/jens/development/IxDF-web"). Returns "unknown" for any
// cwd that can't sensibly be named.
func projectFromCWD(cwd string) string {
	if cwd == "" {
		return "unknown"
	}
	base := filepath.Base(cwd)
	if base == "" || base == "/" || base == "." {
		return "unknown"
	}
	return base
}
