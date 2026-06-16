// Package goei pushes locally-computed Claude Code spend aggregates to
// a Goei dashboard's device-token ingest endpoint.
//
// This is the "zero-key" half of Goei's two-track trust model: instead
// of handing Goei an Anthropic admin API key, the user runs budgetclaw
// locally (which only ever reads ~/.claude/projects/*.jsonl) and ships
// aggregated dollar-and-token rollups to their own dashboard. No key
// leaves the machine; the only thing transmitted is the cost summary.
//
// The wire contract mirrors Goei's POST /api/ingest handler:
//
//   - Authorization: Bearer goei_dt_<32-hex>   (exactly 40 chars)
//   - body: {provider, spend[], usage?}        provider must be "anthropic"
//   - spend dedup key:  (period_start, model, project, branch)
//   - usage dedup key:  (period_start, metric_type, model, breakdown_key, breakdown_value)
//
// Each spend record also carries an optional inline "tokens" object
// (input, output, cache_read, cache_write_5m, cache_write_1h) at the
// same per-(day, project, branch, model) grain as its dollar amount.
// The current server ignores it and keys off amountCents; a future
// server prefers tokens so it can re-price at its own point-in-time
// rate. Both are always sent, so the change is backward compatible.
//
// Per-branch attribution (a budgetclaw differentiator) rides on the
// spend record's own optional branch field, so the project field always
// carries the bare project name. With --no-branch the branch is omitted
// and all branches of a project collapse server-side. Usage records
// break down by bare project name regardless.
//
// Both arrays dedupe server-side via upsert, so re-running sync is
// idempotent: the same day re-sent overwrites, it does not double-count.
package goei

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is the production Goei ingest URL.
const DefaultEndpoint = "https://goei.roninforge.org/api/ingest"

// Provider is the only provider value Goei accepts for Claude Code
// data. Claude Code spend is Anthropic API spend.
const Provider = "anthropic"

// Server-side per-request caps from Goei's ingest handler. We stay
// safely below them when chunking.
const (
	maxSpendPerRequest = 4000
	maxUsagePerRequest = 4500
)

// TokenCounts is the per-(day, project, branch, model) token rollup
// carried inline on a spend record. It is the same grain as the spend
// dollar amount, so a future Goei server can re-price the tokens at its
// own point-in-time rate instead of trusting amountCents. The current
// server ignores this field; sending it is forward-compatible.
type TokenCounts struct {
	Input        int `json:"input"`
	Output       int `json:"output"`
	CacheRead    int `json:"cache_read"`
	CacheWrite5m int `json:"cache_write_5m"`
	CacheWrite1h int `json:"cache_write_1h"`
}

// SpendRecord is one daily per-(model, project, branch) dollar amount.
// Branch is optional: when empty (the --no-branch case) the server
// collapses every branch of a project into a single project-level row.
//
// Tokens is an optional per-(day, project, branch, model) token rollup
// at the same grain as AmountCents. The current Goei server ignores it
// and keys off AmountCents; a future server prefers Tokens so it can
// re-price at its own point-in-time rate. Both are always sent.
type SpendRecord struct {
	PeriodStart string       `json:"periodStart"`
	PeriodEnd   string       `json:"periodEnd"`
	AmountCents int          `json:"amountCents"`
	Currency    string       `json:"currency"`
	Model       string       `json:"model,omitempty"`
	Project     string       `json:"project,omitempty"`
	Branch      string       `json:"branch,omitempty"`
	Tokens      *TokenCounts `json:"tokens,omitempty"`
}

// UsageRecord is one daily per-(model, project) token count for a
// single metric type.
type UsageRecord struct {
	PeriodStart    string `json:"periodStart"`
	PeriodEnd      string `json:"periodEnd"`
	MetricType     string `json:"metricType"`
	MetricValue    int    `json:"metricValue"`
	Model          string `json:"model,omitempty"`
	BreakdownKey   string `json:"breakdownKey,omitempty"`
	BreakdownValue string `json:"breakdownValue,omitempty"`
}

// Payload is one POST body to /api/ingest.
type Payload struct {
	Provider string        `json:"provider"`
	Spend    []SpendRecord `json:"spend"`
	Usage    []UsageRecord `json:"usage,omitempty"`
}

// Aggregate is the neutral input to BuildPayloads, decoupled from the
// db package so this package has no storage dependency. It mirrors
// db.SyncAggregate.
type Aggregate struct {
	Project            string
	GitBranch          string
	Model              string
	Day                string // YYYY-MM-DD (UTC)
	CostUSD            float64
	InputTokens        int
	OutputTokens       int
	CacheReadTokens    int
	CacheWrite5mTokens int
	CacheWrite1hTokens int
}

// ValidToken reports whether a string is shaped like a Goei device
// token: the "goei_dt_" prefix plus a 32-char body, 40 chars total.
// This matches the format check Goei's ingest handler enforces.
func ValidToken(t string) bool {
	return strings.HasPrefix(t, "goei_dt_") && len(t) == 40
}

// branchFor resolves the branch sent on a spend record. With
// includeBranch false the branch is dropped (empty), which tells Goei to
// collapse every branch of a project into one project-level row. With
// includeBranch true the aggregate's branch is sent as-is.
func branchFor(branch string, includeBranch bool) string {
	if !includeBranch {
		return ""
	}
	return branch
}

// BuildPayloads converts aggregates into one or more ingest payloads,
// chunked to stay under the server's per-request caps. Aggregates are
// grouped by day and whole days are packed into requests, so every
// request carries each day's spend and usage together and no request
// is ever spend-empty (which the endpoint rejects).
//
// A spend record is emitted for every aggregate (including ones that
// round to zero cents) so that a day with only sub-cent usage still
// has the spend row its usage rows ride along with. Usage records are
// emitted only for non-zero token metrics.
func BuildPayloads(aggregates []Aggregate, includeBranch bool) []Payload {
	// Preserve input order within a day; group days in first-seen order.
	dayOrder := make([]string, 0)
	byDay := make(map[string][]Aggregate)
	for _, a := range aggregates {
		if _, seen := byDay[a.Day]; !seen {
			dayOrder = append(dayOrder, a.Day)
		}
		byDay[a.Day] = append(byDay[a.Day], a)
	}

	var (
		payloads []Payload
		curSpend []SpendRecord
		curUsage []UsageRecord
	)
	flush := func() {
		if len(curSpend) == 0 {
			return
		}
		payloads = append(payloads, Payload{Provider: Provider, Spend: curSpend, Usage: curUsage})
		curSpend, curUsage = nil, nil
	}

	for _, day := range dayOrder {
		daySpend, dayUsage := recordsForDay(byDay[day], includeBranch)
		// Flush before adding this day if it would overflow a cap.
		if len(curSpend) > 0 &&
			(len(curSpend)+len(daySpend) > maxSpendPerRequest ||
				len(curUsage)+len(dayUsage) > maxUsagePerRequest) {
			flush()
		}
		curSpend = append(curSpend, daySpend...)
		curUsage = append(curUsage, dayUsage...)
	}
	flush()
	return payloads
}

// recordsForDay builds the spend and usage records for a single day's
// aggregates.
func recordsForDay(aggs []Aggregate, includeBranch bool) ([]SpendRecord, []UsageRecord) {
	spend := make([]SpendRecord, 0, len(aggs))
	usage := make([]UsageRecord, 0, len(aggs)*4)

	for _, a := range aggs {
		start, end := dayBounds(a.Day)

		spend = append(spend, SpendRecord{
			PeriodStart: start,
			PeriodEnd:   end,
			AmountCents: centsFromUSD(a.CostUSD),
			Currency:    "USD",
			Model:       a.Model,
			Project:     a.Project,
			Branch:      branchFor(a.GitBranch, includeBranch),
			Tokens: &TokenCounts{
				Input:        a.InputTokens,
				Output:       a.OutputTokens,
				CacheRead:    a.CacheReadTokens,
				CacheWrite5m: a.CacheWrite5mTokens,
				CacheWrite1h: a.CacheWrite1hTokens,
			},
		})

		metrics := []struct {
			typ string
			val int
		}{
			{"input_tokens", a.InputTokens},
			{"output_tokens", a.OutputTokens},
			{"cache_read_tokens", a.CacheReadTokens},
			{"cache_creation_tokens", a.CacheWrite5mTokens + a.CacheWrite1hTokens},
		}
		for _, m := range metrics {
			if m.val == 0 {
				continue
			}
			usage = append(usage, UsageRecord{
				PeriodStart:    start,
				PeriodEnd:      end,
				MetricType:     m.typ,
				MetricValue:    m.val,
				Model:          a.Model,
				BreakdownKey:   "project",
				BreakdownValue: a.Project,
			})
		}
	}
	return spend, usage
}

// centsFromUSD converts a dollar amount to integer cents, rounding to
// the nearest cent. Never negative.
func centsFromUSD(usd float64) int {
	c := int(usd*100 + 0.5)
	if c < 0 {
		return 0
	}
	return c
}

// dayBounds returns the ISO8601 start and end timestamps for a
// YYYY-MM-DD day, matching the 1-day bucket boundaries Goei's
// API-connected adapters produce (start of day to start of next day,
// UTC). A malformed day falls back to using the raw string as start
// and end so a bad row cannot panic the sync.
func dayBounds(day string) (string, string) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return day, day
	}
	start := t.UTC().Format("2006-01-02T15:04:05Z")
	end := t.UTC().AddDate(0, 0, 1).Format("2006-01-02T15:04:05Z")
	return start, end
}

// Client posts payloads to a Goei ingest endpoint.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
}

// New returns a Client with sane defaults. An empty endpoint falls
// back to DefaultEndpoint.
func New(endpoint, token string) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Client{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// Push sends a single payload and returns the number of records Goei
// reports it stored. Errors include the HTTP status and a snippet of
// the response body for diagnosis.
func (c *Client) Push(ctx context.Context, p Payload) (int, error) {
	if !ValidToken(c.Token) {
		return 0, fmt.Errorf("invalid device token format (expected goei_dt_ + 32 chars)")
	}

	body, err := json.Marshal(p)
	if err != nil {
		return 0, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("post to %s: %w", c.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ingest rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		OK      bool `json:"ok"`
		Records int  `json:"records"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// 200 with an unexpected body still counts as delivered.
		return 0, nil
	}
	return parsed.Records, nil
}
