package goei

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestValidToken(t *testing.T) {
	valid := "goei_dt_" + strings.Repeat("a", 32) // 40 chars
	cases := []struct {
		in   string
		want bool
	}{
		{valid, true},
		{"goei_dt_" + strings.Repeat("a", 31), false}, // too short
		{"goei_dt_" + strings.Repeat("a", 33), false}, // too long
		{"nope_" + strings.Repeat("a", 35), false},    // wrong prefix
		{"", false},
	}
	for _, c := range cases {
		if got := ValidToken(c.in); got != c.want {
			t.Errorf("ValidToken(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBranchFor(t *testing.T) {
	cases := []struct {
		branch  string
		include bool
		want    string
	}{
		{"main", true, "main"},
		{"main", false, ""},
		{"", true, ""},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := branchFor(c.branch, c.include); got != c.want {
			t.Errorf("branchFor(%q,%v) = %q, want %q", c.branch, c.include, got, c.want)
		}
	}
}

func TestCentsFromUSD(t *testing.T) {
	cases := []struct {
		usd  float64
		want int
	}{
		{0, 0},
		{0.004, 0}, // rounds down
		{0.005, 1}, // rounds up
		{1.23, 123},
		{1.235, 124}, // rounds to nearest cent
		{-5, 0},      // never negative
	}
	for _, c := range cases {
		if got := centsFromUSD(c.usd); got != c.want {
			t.Errorf("centsFromUSD(%v) = %d, want %d", c.usd, got, c.want)
		}
	}
}

func TestDayBounds(t *testing.T) {
	start, end := dayBounds("2026-06-10")
	if start != "2026-06-10T00:00:00Z" {
		t.Errorf("start = %q", start)
	}
	if end != "2026-06-11T00:00:00Z" {
		t.Errorf("end = %q", end)
	}
}

func TestBuildPayloadsBasic(t *testing.T) {
	aggs := []Aggregate{
		{
			Project: "app", GitBranch: "main", Model: "claude-opus-4-8", Day: "2026-06-10",
			CostUSD: 1.50, InputTokens: 1000, OutputTokens: 500,
			CacheReadTokens: 200, CacheWrite5mTokens: 10, CacheWrite1hTokens: 5,
		},
		{
			Project: "app", GitBranch: "feature-x", Model: "claude-sonnet-4-6", Day: "2026-06-10",
			CostUSD: 0.25, InputTokens: 300, OutputTokens: 0,
		},
	}
	payloads := BuildPayloads(aggs, true)
	if len(payloads) != 1 {
		t.Fatalf("got %d payloads, want 1", len(payloads))
	}
	p := payloads[0]
	if p.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", p.Provider)
	}
	if len(p.Spend) != 2 {
		t.Fatalf("got %d spend records, want 2", len(p.Spend))
	}
	// First aggregate: cost 1.50 -> 150 cents, bare project + branch field.
	s0 := p.Spend[0]
	if s0.AmountCents != 150 {
		t.Errorf("spend[0].AmountCents = %d, want 150", s0.AmountCents)
	}
	if s0.Project != "app" {
		t.Errorf("spend[0].Project = %q, want 'app' (bare)", s0.Project)
	}
	if s0.Branch != "main" {
		t.Errorf("spend[0].Branch = %q, want 'main'", s0.Branch)
	}
	if s0.Currency != "USD" {
		t.Errorf("spend[0].Currency = %q, want USD", s0.Currency)
	}
	// First aggregate has 4 non-zero metrics (in, out, cache_read, cache_creation=15).
	// Second aggregate has 2 non-zero metrics (in, out=0 skipped -> just in). So 4 + 1 = 5.
	if len(p.Usage) != 5 {
		t.Fatalf("got %d usage records, want 5", len(p.Usage))
	}
	// cache_creation must combine 5m + 1h.
	var foundCacheCreation bool
	for _, u := range p.Usage {
		if u.MetricType == "cache_creation_tokens" {
			foundCacheCreation = true
			if u.MetricValue != 15 {
				t.Errorf("cache_creation_tokens = %d, want 15", u.MetricValue)
			}
		}
		if u.BreakdownKey != "project" {
			t.Errorf("usage breakdownKey = %q, want project", u.BreakdownKey)
		}
		if u.BreakdownValue != "app" {
			t.Errorf("usage breakdownValue = %q, want 'app' (bare, no branch suffix)", u.BreakdownValue)
		}
	}
	if !foundCacheCreation {
		t.Error("missing cache_creation_tokens usage record")
	}
}

func TestBuildPayloadsNoBranch(t *testing.T) {
	aggs := []Aggregate{
		{Project: "app", GitBranch: "main", Model: "m", Day: "2026-06-10", CostUSD: 1, InputTokens: 1},
	}
	p := BuildPayloads(aggs, false)
	if p[0].Spend[0].Project != "app" {
		t.Errorf("project = %q, want 'app'", p[0].Spend[0].Project)
	}
	if p[0].Spend[0].Branch != "" {
		t.Errorf("branch = %q, want empty (suppressed by --no-branch)", p[0].Spend[0].Branch)
	}
}

func TestBuildPayloadsEmpty(t *testing.T) {
	if got := BuildPayloads(nil, true); len(got) != 0 {
		t.Errorf("expected no payloads for empty input, got %d", len(got))
	}
}

// TestBuildPayloadsChunking verifies that a large number of days is
// split across multiple requests, each non-empty and under the caps,
// with no records lost.
func TestBuildPayloadsChunking(t *testing.T) {
	var aggs []Aggregate
	// 9000 distinct (project, day) aggregates -> must exceed the 4000
	// spend cap and split into 3 chunks.
	for i := 0; i < 9000; i++ {
		day := "2026-06-10"
		aggs = append(aggs, Aggregate{
			Project: "p", GitBranch: "b", Model: "m", Day: day,
			CostUSD: 0.10, InputTokens: 1,
		})
		// vary the day so grouping packs whole days
		_ = i
	}
	// Spread across many days so day-packing actually chunks.
	for i := range aggs {
		aggs[i].Day = "2026-06-" + pad(10+(i%18))
	}
	payloads := BuildPayloads(aggs, true)
	if len(payloads) < 2 {
		t.Fatalf("expected chunking into >=2 payloads, got %d", len(payloads))
	}
	total := 0
	for _, p := range payloads {
		if len(p.Spend) == 0 {
			t.Error("payload has empty spend (endpoint would reject)")
		}
		if len(p.Spend) > maxSpendPerRequest {
			t.Errorf("payload spend %d exceeds cap %d", len(p.Spend), maxSpendPerRequest)
		}
		if len(p.Usage) > maxUsagePerRequest {
			t.Errorf("payload usage %d exceeds cap %d", len(p.Usage), maxUsagePerRequest)
		}
		total += len(p.Spend)
	}
	if total != 9000 {
		t.Errorf("total spend across chunks = %d, want 9000", total)
	}
}

func pad(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestPushSendsAuthAndBody(t *testing.T) {
	token := "goei_dt_" + strings.Repeat("a", 32)
	var gotAuth, gotCT string
	var gotBody Payload

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"records":3}`))
	}))
	defer srv.Close()

	c := New(srv.URL, token)
	n, err := c.Push(context.Background(), Payload{
		Provider: "anthropic",
		Spend:    []SpendRecord{{PeriodStart: "2026-06-10T00:00:00Z", PeriodEnd: "2026-06-11T00:00:00Z", AmountCents: 100, Currency: "USD"}},
	})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n != 3 {
		t.Errorf("records = %d, want 3", n)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody.Provider != "anthropic" || len(gotBody.Spend) != 1 {
		t.Errorf("server received unexpected body: %+v", gotBody)
	}
}

func TestPushRejectsBadToken(t *testing.T) {
	c := New("http://example.invalid", "not-a-token")
	if _, err := c.Push(context.Background(), Payload{}); err == nil {
		t.Error("expected error for invalid token, got nil")
	}
}

func TestPushSurfacesServerError(t *testing.T) {
	token := "goei_dt_" + strings.Repeat("a", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"Invalid or revoked token"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, token)
	_, err := c.Push(context.Background(), Payload{Provider: "anthropic", Spend: []SpendRecord{{}}})
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error should include status and body: %v", err)
	}
}

func TestNewDefaultsEndpoint(t *testing.T) {
	c := New("", "tok")
	if c.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want default %q", c.Endpoint, DefaultEndpoint)
	}
}
