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
// Machine is an optional per-machine identity (typically the OS
// hostname). The Goei server uses it to keep two machines' rollups from
// colliding: the same (day, project, branch, model) synced from a
// laptop and a desktop stay separate instead of overwriting each other.
// When empty the server treats it as legacy/unknown, so the field is
// additive and backward compatible.
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
	Machine     string       `json:"machine,omitempty"`
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
	// GuardEvents carries content-free enforcement audit records back up
	// with the sync (Guard Mode's audit channel). Omitted when there are
	// none, so a plain sync is byte-for-byte unchanged.
	GuardEvents []GuardEvent `json:"guardEvents,omitempty"`
	// PRs carries content-free cost-per-PR metadata (opt-in via `budgetclaw
	// prs on`). Omitted when empty, so a plain sync is unchanged.
	PRs []PRRecord `json:"prs,omitempty"`
}

// PRRecord is one pull request (or in-flight branch) for cost-per-PR attribution.
// Content-free: a PR number, branch and base names, commit count, and diff size, never
// commit messages or code. State is "merged", "squashed", or "open". Goei joins it to
// local spend by (project, branch); a squashed PR has no head branch, so it carries the
// number and diff size but no per-branch cost.
type PRRecord struct {
	Project   string `json:"project"`
	Branch    string `json:"branch,omitempty"`
	PR        int    `json:"pr,omitempty"`
	Base      string `json:"base,omitempty"`
	State     string `json:"state"`
	MergedAt  string `json:"mergedAt,omitempty"`
	Commits   int    `json:"commits"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// GuardEvent is one enforcement audit record: a remote policy warned or
// killed a runaway on this machine. Content-free by construction (ids,
// amounts, scope labels, machine, timestamps only), so it upholds the
// zero-prompt pledge exactly like the spend records do.
type GuardEvent struct {
	PolicyID    string `json:"policyId"`
	Action      string `json:"action"` // notify | warn | kill | override
	ScopeType   string `json:"scopeType,omitempty"`
	ScopeValue  string `json:"scopeValue,omitempty"`
	Machine     string `json:"machine,omitempty"`
	AmountCents int    `json:"amountCents"`
	CapCents    int    `json:"capCents"`
	At          string `json:"at,omitempty"`
	// DedupKey makes reporting idempotent: the server INSERT OR IGNOREs on
	// it, so a re-sent event (same period, same action) is recorded once.
	DedupKey string `json:"dedupKey,omitempty"`
}

// WirePolicy is one budget policy as Goei serializes it for this device
// on the Guard Mode down-channel. enforcement is "local_exact" (enforce
// against this machine's own rollup, kill-eligible) or "server_aggregate"
// (a team-wide figure this device can only warn against, with staleness).
type WirePolicy struct {
	ID    string `json:"id"`
	Scope struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"scope"`
	Period           string `json:"period"`
	CapCents         int    `json:"capCents"`
	Enforcement      string `json:"enforcement"`
	Action           string `json:"action"`
	ServerSpentCents int    `json:"serverSpentCents"`
	AsOf             string `json:"asOf"`
	SetBy            string `json:"setBy"`
	Source           string `json:"source"`
}

// PolicyResponse is the Guard Mode policy bundle, returned both by
// GET /api/policy and piggybacked on the POST /api/ingest response.
type PolicyResponse struct {
	PolicyVersion int          `json:"policyVersion"`
	Policies      []WirePolicy `json:"policies"`
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
//
// machine is stamped on every spend record so the server can attribute
// rollups to the machine they came from. An empty machine is fine: the
// omitempty field is dropped and the server treats it as legacy.
func BuildPayloads(aggregates []Aggregate, includeBranch bool, machine string) []Payload {
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
		daySpend, dayUsage := recordsForDay(byDay[day], includeBranch, machine)
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
func recordsForDay(aggs []Aggregate, includeBranch bool, machine string) ([]SpendRecord, []UsageRecord) {
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
			Machine:     machine,
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
// reports it stored, plus any Guard Mode policy bundle piggybacked on the
// response (nil when the server sends none). Errors include the HTTP
// status and a snippet of the response body for diagnosis.
func (c *Client) Push(ctx context.Context, p Payload) (int, *PolicyResponse, error) {
	if !ValidToken(c.Token) {
		return 0, nil, fmt.Errorf("invalid device token format (expected goei_dt_ + 32 chars)")
	}

	body, err := json.Marshal(p)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post to %s: %w", c.Endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The response now carries an optional policy bundle, so allow more than
	// the old 4 KB.
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return 0, nil, fmt.Errorf("ingest rejected (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var parsed struct {
		OK            bool         `json:"ok"`
		Records       int          `json:"records"`
		PolicyVersion int          `json:"policyVersion"`
		Policies      []WirePolicy `json:"policies"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		// 200 with an unexpected body still counts as delivered.
		return 0, nil, nil
	}
	return parsed.Records, &PolicyResponse{PolicyVersion: parsed.PolicyVersion, Policies: parsed.Policies}, nil
}

// PullPolicies fetches this device's Guard Mode policy set from
// GET /api/policy. A conditional request with the last ETag returns
// notModified=true and no body when nothing changed, so `budgetclaw watch`
// can poll cheaply. The returned etag should be passed to the next call.
func (c *Client) PullPolicies(ctx context.Context, etag string) (resp *PolicyResponse, newETag string, notModified bool, err error) {
	if !ValidToken(c.Token) {
		return nil, "", false, fmt.Errorf("invalid device token format (expected goei_dt_ + 32 chars)")
	}

	url := policyURLFor(c.Endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	httpResp, err := hc.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("get %s: %w", url, err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}

	respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if httpResp.StatusCode != http.StatusOK {
		return nil, "", false, fmt.Errorf("policy fetch rejected (HTTP %d): %s", httpResp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var pr PolicyResponse
	if err := json.Unmarshal(respBody, &pr); err != nil {
		return nil, "", false, fmt.Errorf("decode policy response: %w", err)
	}
	return &pr, httpResp.Header.Get("ETag"), false, nil
}

// policyURLFor derives the policy endpoint from the ingest endpoint by
// swapping the trailing path segment, so a custom --endpoint keeps host and
// scheme. The default ".../api/ingest" becomes ".../api/policy".
func policyURLFor(endpoint string) string {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	if strings.HasSuffix(endpoint, "/api/ingest") {
		return strings.TrimSuffix(endpoint, "/api/ingest") + "/api/policy"
	}
	if strings.HasSuffix(endpoint, "/ingest") {
		return strings.TrimSuffix(endpoint, "/ingest") + "/policy"
	}
	return strings.TrimRight(endpoint, "/") + "/api/policy"
}
