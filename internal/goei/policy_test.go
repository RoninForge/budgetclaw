package goei

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyURLFor(t *testing.T) {
	cases := map[string]string{
		"https://goei.roninforge.org/api/ingest": "https://goei.roninforge.org/api/policy",
		"https://example.com/ingest":             "https://example.com/policy",
		"https://example.com/custom":             "https://example.com/custom/api/policy",
		"":                                       strings.TrimSuffix(DefaultEndpoint, "/api/ingest") + "/api/policy",
	}
	for in, want := range cases {
		if got := policyURLFor(in); got != want {
			t.Errorf("policyURLFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPullPolicies(t *testing.T) {
	token := "goei_dt_" + strings.Repeat("a", 32)
	var gotPath, gotINM string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotINM = r.Header.Get("If-None-Match")
		// A conditional request matching the ETag returns 304.
		if gotINM == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"policyVersion":3,"policies":[{"id":"gp_1","scope":{"type":"team","value":""},"period":"month","capCents":50000,"enforcement":"server_aggregate","action":"warn","serverSpentCents":12345,"asOf":"2026-07-21T00:00:00Z","setBy":"boss@x.com","source":"goei"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL+"/api/ingest", token)

	resp, etag, notModified, err := c.PullPolicies(context.Background(), "")
	if err != nil {
		t.Fatalf("PullPolicies: %v", err)
	}
	if notModified {
		t.Fatal("first pull should not be 304")
	}
	if gotPath != "/api/policy" {
		t.Errorf("request path = %q, want /api/policy", gotPath)
	}
	if etag != `"v1"` {
		t.Errorf("etag = %q", etag)
	}
	if resp.PolicyVersion != 3 || len(resp.Policies) != 1 || resp.Policies[0].ID != "gp_1" {
		t.Errorf("policy response = %+v", resp)
	}

	// A second conditional request with the ETag is a cheap 304.
	_, _, notModified2, err := c.PullPolicies(context.Background(), `"v1"`)
	if err != nil {
		t.Fatalf("PullPolicies (conditional): %v", err)
	}
	if !notModified2 {
		t.Error("matching ETag should return notModified")
	}
}

func TestPushParsesPiggybackedPolicies(t *testing.T) {
	token := "goei_dt_" + strings.Repeat("a", 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"records":2,"policyVersion":9,"policies":[{"id":"gp_5","scope":{"type":"dev","value":""},"period":"month","capCents":10000,"enforcement":"local_exact","action":"kill"}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, token)
	n, pr, err := c.Push(context.Background(), Payload{Provider: "anthropic", Spend: []SpendRecord{{}}})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if n != 2 {
		t.Errorf("records = %d, want 2", n)
	}
	if pr == nil || pr.PolicyVersion != 9 || len(pr.Policies) != 1 || pr.Policies[0].Action != "kill" {
		t.Errorf("piggybacked policy = %+v", pr)
	}
}
