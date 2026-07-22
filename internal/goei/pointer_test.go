package goei

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePointer(t *testing.T) {
	valid := []byte(`
[goei]
team = "acme-platform"
endpoint = "https://goei.roninforge.org"
join_code = "goei_jc_abc123"
mode = "request"
`)
	p, err := ParsePointer(valid, "/x/.budgetclaw.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p == nil {
		t.Fatal("expected a pointer")
	}
	if p.Team != "acme-platform" || p.JoinCode != "goei_jc_abc123" || p.Mode != "request" {
		t.Fatalf("bad parse: %+v", p)
	}
	if !p.PromptsEnabled() {
		t.Fatal("prompts should default to enabled")
	}

	// A file with no join_code is not a pointer (nil, nil), so a local config that
	// happens to sit in a repo is never mistaken for one.
	notPointer := []byte(`
[goei]
token = "goei_dt_0000000000000000000000000000000000"
`)
	np, err := ParsePointer(notPointer, "/x/.budgetclaw.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if np != nil {
		t.Fatalf("a file without join_code must not be a pointer, got %+v", np)
	}
}

func TestPromptsSilenced(t *testing.T) {
	data := []byte(`
[goei]
join_code = "goei_jc_x"
prompts = false
`)
	p, err := ParsePointer(data, "p")
	if err != nil || p == nil {
		t.Fatalf("parse: %v", err)
	}
	if p.PromptsEnabled() {
		t.Fatal("prompts = false must silence the discovery line")
	}
}

func TestFindRepoPointerWalksUp(t *testing.T) {
	root := t.TempDir()
	// pointer lives at the repo root
	if err := os.WriteFile(filepath.Join(root, PointerFileName),
		[]byte("[goei]\njoin_code = \"goei_jc_here\"\nteam = \"t\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	p, err := FindRepoPointer(nested)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if p == nil || p.JoinCode != "goei_jc_here" {
		t.Fatalf("expected to find the ancestor pointer, got %+v", p)
	}
}

func TestFindRepoPointerAbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	p, err := FindRepoPointer(dir)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if p != nil {
		t.Fatalf("expected nil when no pointer exists, got %+v", p)
	}
}

func TestFindRepoPointerStopsAtGitRoot(t *testing.T) {
	base := t.TempDir()
	// A stray pointer sits ABOVE the repo (e.g. a shared home dir).
	if err := os.WriteFile(filepath.Join(base, PointerFileName),
		[]byte("[goei]\njoin_code = \"goei_jc_stray\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The repo has a .git marker but NO pointer of its own.
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(repo, "pkg", "x")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}

	p, err := FindRepoPointer(nested)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if p != nil {
		t.Fatalf("must not pick up the stray pointer above the repo root, got %+v", p)
	}
}

func TestIngestEndpointFor(t *testing.T) {
	cases := map[string]string{
		"":                             DefaultEndpoint,
		"https://goei.roninforge.org":  DefaultEndpoint,
		"https://goei.roninforge.org/": DefaultEndpoint,
		"https://team.example.com":     "https://team.example.com/api/ingest",
		// A scheme-less host is treated as https rather than yielding a scheme-less URL.
		"goei.roninforge.org": DefaultEndpoint,
		"team.example.com":    "https://team.example.com/api/ingest",
	}
	for in, want := range cases {
		if got := IngestEndpointFor(in); got != want {
			t.Errorf("IngestEndpointFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"":                            DefaultHost,
		"https://goei.roninforge.org": DefaultHost,
		"goei.roninforge.org":         DefaultHost,
		"https://team.example.com":    "team.example.com",
		"team.example.com":            "team.example.com",
	}
	for in, want := range cases {
		if got := HostOf(in); got != want {
			t.Errorf("HostOf(%q) = %q, want %q", in, got, want)
		}
	}
}
