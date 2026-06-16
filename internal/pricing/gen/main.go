//go:build ignore

// Command gen produces table_gen.go for the pricing package from a
// VENDORED, pinned release of the ai-price-index dataset.
//
// Two modes:
//
//	go run ./gen            (default, offline)
//	    Reads the vendored artifacts under internal/pricing/index/**,
//	    verifies every file's sha256 against PROVENANCE.json (fails on
//	    drift), parses the anthropic per-model series, and emits
//	    table_gen.go. No network access.
//
//	go run ./gen -fetch
//	    Refreshes internal/pricing/index/** and PROVENANCE.json from the
//	    pinned tag (PINNED_TAG). It tries a local clone first
//	    (AI_PRICE_INDEX_CLONE env or ../../../ai-price-index) and falls
//	    back to cloning github.com/RoninForge/ai-price-index. Then it
//	    emits table_gen.go like the default mode.
//
// The generated table_gen.go is deterministic: maps are iterated in
// sorted order, intervals are sorted by their from date, floats are
// formatted stably, and the output is gofmt-clean. Re-running
// `go generate ./internal/pricing/` on an unchanged dataset is a no-op.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	repoURL  = "https://github.com/RoninForge/ai-price-index"
	provName = "PROVENANCE.json"
)

// Layout relative to internal/pricing, which is the cwd both when the
// `//go:generate` directive fires (it runs in the directive file's dir)
// and when invoked manually via `go run ./gen/main.go` from that dir.
const (
	pkgDirRel   = "."
	indexDirRel = "index"
)

// dataset paths inside the ai-price-index repo, relative to its root.
const (
	dsIndex   = "data/ai-price-index/index.json"
	dsCurrent = "data/ai-price-index/current.json"
	dsVectors = "examples/pricing-vectors.json"
	dsLicense = "DATA-LICENSE.md"
	dsModels  = "data/ai-price-index/models/anthropic"
)

// vendoredFiles maps the vendored relpath (under index/) that gets
// hashed into PROVENANCE.json to its source path inside the dataset
// repo. Per-model anthropic files are discovered dynamically.
var staticVendored = map[string]string{
	"index.json":           dsIndex,
	"current.json":         dsCurrent,
	"pricing-vectors.json": dsVectors,
	"LICENSE":              dsLicense,
}

func main() {
	fetch := flag.Bool("fetch", false, "refresh vendored artifacts + PROVENANCE.json from the pinned tag, then generate")
	flag.Parse()

	tag := readPinnedTag()

	if *fetch {
		if err := refresh(tag); err != nil {
			fatalf("fetch: %v", err)
		}
	}

	prov, err := loadProvenance()
	if err != nil {
		fatalf("load provenance: %v", err)
	}
	if prov.Tag != tag {
		fatalf("provenance tag %q != PINNED_TAG %q (run `go run ./gen -fetch`)", prov.Tag, tag)
	}

	if err := verifyVendored(prov); err != nil {
		fatalf("verify vendored: %v", err)
	}

	series, aliases, err := parseSeries()
	if err != nil {
		fatalf("parse series: %v", err)
	}

	src, err := renderTable(tag, prov.Commit, series, aliases)
	if err != nil {
		fatalf("render: %v", err)
	}

	out := filepath.Join(pkgDirRel, "table_gen.go")
	if err := os.WriteFile(out, src, 0o644); err != nil {
		fatalf("write %s: %v", out, err)
	}
	fmt.Printf("wrote %s (%d models, %d aliases) from %s (%s)\n", out, len(series), len(aliases), tag, shortCommit(prov.Commit))
}

// ---------------------------------------------------------------------------
// PINNED_TAG + PROVENANCE
// ---------------------------------------------------------------------------

func readPinnedTag() string {
	b, err := os.ReadFile(filepath.Join(pkgDirRel, "PINNED_TAG"))
	if err != nil {
		fatalf("read PINNED_TAG: %v", err)
	}
	tag := strings.TrimSpace(string(b))
	if tag == "" {
		fatalf("PINNED_TAG is empty")
	}
	return tag
}

type provenance struct {
	Tag       string            `json:"tag"`
	Commit    string            `json:"commit"`
	FetchedAt string            `json:"fetched_at"`
	Files     map[string]string `json:"files"`
}

func loadProvenance() (*provenance, error) {
	b, err := os.ReadFile(filepath.Join(indexDirRel, provName))
	if err != nil {
		return nil, err
	}
	var p provenance
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// verifyVendored hashes every vendored file listed in the provenance and
// fails on any drift or missing file. It also fails if a vendored model
// file exists that is not listed in the provenance (unexpected extra).
func verifyVendored(p *provenance) error {
	for rel, want := range p.Files {
		got, err := sha256File(filepath.Join(indexDirRel, rel))
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if got != want {
			return fmt.Errorf("%s: sha256 drift\n  want %s\n  got  %s", rel, want, got)
		}
	}
	// Guard against stray model files not covered by provenance.
	got, err := listVendoredModelFiles()
	if err != nil {
		return err
	}
	for _, rel := range got {
		if _, ok := p.Files[rel]; !ok {
			return fmt.Errorf("%s is on disk but missing from %s", rel, provName)
		}
	}
	return nil
}

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// listVendoredModelFiles returns the vendored anthropic model file
// relpaths (relative to index/), sorted.
func listVendoredModelFiles() ([]string, error) {
	dir := filepath.Join(indexDirRel, "models", "anthropic")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		out = append(out, filepath.ToSlash(filepath.Join("models", "anthropic", e.Name())))
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------------
// -fetch: refresh vendored artifacts from the pinned tag
// ---------------------------------------------------------------------------

func refresh(tag string) error {
	repo, cleanup, err := resolveRepo(tag)
	if err != nil {
		return err
	}
	defer cleanup()

	commit, err := gitCommit(repo, tag)
	if err != nil {
		return fmt.Errorf("resolve commit for %s: %w", tag, err)
	}

	// Clean and recreate the vendored tree.
	if err := os.RemoveAll(indexDirRel); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(indexDirRel, "models", "anthropic"), 0o755); err != nil {
		return err
	}

	files := map[string]string{} // vendored relpath -> sha256

	// Static files. index.json is filtered to anthropic-only entries.
	for rel, src := range staticVendored {
		b, err := gitShow(repo, tag, src)
		if err != nil {
			return fmt.Errorf("read %s@%s: %w", src, tag, err)
		}
		if rel == "index.json" {
			b, err = filterIndexAnthropic(b)
			if err != nil {
				return fmt.Errorf("filter index.json: %w", err)
			}
		}
		dst := filepath.Join(indexDirRel, rel)
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		files[rel] = hex.EncodeToString(sum[:])
	}

	// Anthropic per-model series files.
	names, err := gitListTree(repo, tag, dsModels)
	if err != nil {
		return fmt.Errorf("list %s: %w", dsModels, err)
	}
	for _, src := range names {
		if !strings.HasSuffix(src, ".json") {
			continue
		}
		b, err := gitShow(repo, tag, src)
		if err != nil {
			return fmt.Errorf("read %s: %w", src, err)
		}
		rel := filepath.ToSlash(filepath.Join("models", "anthropic", filepath.Base(src)))
		dst := filepath.Join(indexDirRel, rel)
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		files[rel] = hex.EncodeToString(sum[:])
	}

	prov := provenance{
		Tag:       tag,
		Commit:    commit,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
		Files:     files,
	}
	pb, err := json.MarshalIndent(prov, "", "  ")
	if err != nil {
		return err
	}
	pb = append(pb, '\n')
	if err := os.WriteFile(filepath.Join(indexDirRel, provName), pb, 0o644); err != nil {
		return err
	}
	fmt.Printf("refreshed %d files from %s (%s)\n", len(files), tag, shortCommit(commit))
	return nil
}

// resolveRepo returns a path to a checkout of the dataset repo that has
// the pinned tag available, plus a cleanup func. It prefers a local
// clone (AI_PRICE_INDEX_CLONE or ../../../ai-price-index) that already
// has the tag; otherwise it shallow-clones the tag into a temp dir.
func resolveRepo(tag string) (string, func(), error) {
	noop := func() {}

	candidates := []string{}
	if env := os.Getenv("AI_PRICE_INDEX_CLONE"); env != "" {
		candidates = append(candidates, env)
	}
	// cwd is internal/pricing; the sibling dataset clone sits next to the
	// budgetclaw repo (internal/pricing -> internal -> budgetclaw ->
	// development -> ai-price-index).
	candidates = append(candidates, filepath.Join("..", "..", "..", "ai-price-index"))

	for _, c := range candidates {
		if hasTag(c, tag) {
			return c, noop, nil
		}
	}

	tmp, err := os.MkdirTemp("", "ai-price-index-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	cmd := exec.Command("git", "clone", "--depth", "1", "--branch", tag, repoURL, tmp)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", noop, fmt.Errorf("git clone %s @ %s: %w", repoURL, tag, err)
	}
	return tmp, cleanup, nil
}

func hasTag(repo, tag string) bool {
	if fi, err := os.Stat(filepath.Join(repo, ".git")); err != nil || !(fi.IsDir() || fi.Mode().IsRegular()) {
		return false
	}
	cmd := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", tag+"^{commit}")
	return cmd.Run() == nil
}

func gitCommit(repo, tag string) (string, error) {
	out, err := exec.Command("git", "-C", repo, "rev-list", "-n", "1", tag).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitShow(repo, tag, path string) ([]byte, error) {
	return exec.Command("git", "-C", repo, "show", tag+":"+path).Output()
}

func gitListTree(repo, tag, dir string) ([]string, error) {
	out, err := exec.Command("git", "-C", repo, "ls-tree", "-r", "--name-only", tag, dir).Output()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	sort.Strings(names)
	return names, nil
}

// filterIndexAnthropic reduces a dataset index.json to anthropic entries,
// preserving the top-level wrapper shape and emitting deterministic JSON.
func filterIndexAnthropic(raw []byte) ([]byte, error) {
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		if _, ok := wrapper["models"]; ok {
			var models []indexEntry
			if err := json.Unmarshal(wrapper["models"], &models); err != nil {
				return nil, err
			}
			anth := filterEntries(models)
			mb, err := json.Marshal(anth)
			if err != nil {
				return nil, err
			}
			wrapper["models"] = mb
			out, err := json.MarshalIndent(wrapper, "", "  ")
			if err != nil {
				return nil, err
			}
			return append(out, '\n'), nil
		}
	}
	// Fall back to a top-level array.
	var models []indexEntry
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(filterEntries(models), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

func filterEntries(in []indexEntry) []indexEntry {
	var out []indexEntry
	for _, m := range in {
		if m.Provider == "anthropic" {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// parse anthropic series from the vendored index/**
// ---------------------------------------------------------------------------

type indexEntry struct {
	ID        string   `json:"id"`
	Provider  string   `json:"provider"`
	File      string   `json:"file"`
	LatestRev string   `json:"latestRev,omitempty"`
	Aliases   []string `json:"aliases,omitempty"`
}

type modelDoc struct {
	Model      string   `json:"model"`
	Provider   string   `json:"provider"`
	Aliases    []string `json:"aliases,omitempty"`
	Variations struct {
		Input  []interval `json:"input"`
		Output []interval `json:"output"`
	} `json:"variations"`
}

type interval struct {
	From     string   `json:"from"`
	To       *string  `json:"to"`
	PriceUSD float64  `json:"price_usd"`
	Unit     string   `json:"unit"`
}

// genInterval is the parsed, generation-ready interval.
type genInterval struct {
	FromY, FromM, FromD int
	ToY, ToM, ToD       int
	HasTo               bool
	PriceUSD            float64
}

type genModel struct {
	ID     string
	Input  []genInterval
	Output []genInterval
}

// parseSeries reads the vendored anthropic per-model files (resolved via
// the vendored index.json) and returns the canonical model series plus
// the alias -> canonical map. Aliases are sourced from both index.json
// and each model doc; they must agree.
func parseSeries() (map[string]genModel, map[string]string, error) {
	idxRaw, err := os.ReadFile(filepath.Join(indexDirRel, "index.json"))
	if err != nil {
		return nil, nil, err
	}
	entries, err := unmarshalIndex(idxRaw)
	if err != nil {
		return nil, nil, err
	}

	series := map[string]genModel{}
	aliases := map[string]string{}

	for _, e := range entries {
		if e.Provider != "anthropic" {
			continue
		}
		// e.File is dataset-relative (data/ai-price-index/models/...);
		// the vendored copy keeps only the basename under models/anthropic.
		path := filepath.Join(indexDirRel, "models", "anthropic", filepath.Base(e.File))
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", path, err)
		}
		var doc modelDoc
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", path, err)
		}
		if doc.Model == "" {
			return nil, nil, fmt.Errorf("%s: empty model id", path)
		}

		gm := genModel{ID: doc.Model}
		if gm.Input, err = parseIntervals(doc.Variations.Input); err != nil {
			return nil, nil, fmt.Errorf("%s input: %w", doc.Model, err)
		}
		if gm.Output, err = parseIntervals(doc.Variations.Output); err != nil {
			return nil, nil, fmt.Errorf("%s output: %w", doc.Model, err)
		}
		if len(gm.Input) == 0 || len(gm.Output) == 0 {
			return nil, nil, fmt.Errorf("%s: missing input or output intervals", doc.Model)
		}
		series[doc.Model] = gm

		// Collect aliases (union of index + doc), all -> canonical.
		for _, a := range mergeAliases(e.Aliases, doc.Aliases) {
			if a == "" || a == doc.Model {
				continue
			}
			if prev, ok := aliases[a]; ok && prev != doc.Model {
				return nil, nil, fmt.Errorf("alias %q maps to both %q and %q", a, prev, doc.Model)
			}
			aliases[a] = doc.Model
		}
	}
	if len(series) == 0 {
		return nil, nil, fmt.Errorf("no anthropic models found in index.json")
	}
	return series, aliases, nil
}

func unmarshalIndex(raw []byte) ([]indexEntry, error) {
	var wrapper struct {
		Models []indexEntry `json:"models"`
	}
	if err := json.Unmarshal(raw, &wrapper); err == nil && wrapper.Models != nil {
		return wrapper.Models, nil
	}
	var arr []indexEntry
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, err
	}
	return arr, nil
}

func mergeAliases(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func parseIntervals(in []interval) ([]genInterval, error) {
	out := make([]genInterval, 0, len(in))
	for _, iv := range in {
		fy, fm, fd, err := parseDate(iv.From)
		if err != nil {
			return nil, fmt.Errorf("from %q: %w", iv.From, err)
		}
		g := genInterval{FromY: fy, FromM: fm, FromD: fd, PriceUSD: iv.PriceUSD}
		if iv.To != nil && *iv.To != "" {
			ty, tm, td, err := parseDate(*iv.To)
			if err != nil {
				return nil, fmt.Errorf("to %q: %w", *iv.To, err)
			}
			g.HasTo = true
			g.ToY, g.ToM, g.ToD = ty, tm, td
		}
		out = append(out, g)
	}
	// Deterministic: sort by from date ascending.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.FromY != b.FromY {
			return a.FromY < b.FromY
		}
		if a.FromM != b.FromM {
			return a.FromM < b.FromM
		}
		return a.FromD < b.FromD
	})
	return out, nil
}

func parseDate(s string) (y, m, d int, err error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, 0, 0, err
	}
	return t.Year(), int(t.Month()), t.Day(), nil
}

// ---------------------------------------------------------------------------
// render table_gen.go
// ---------------------------------------------------------------------------

func renderTable(tag, commit string, series map[string]genModel, aliases map[string]string) ([]byte, error) {
	var b bytes.Buffer

	fmt.Fprintf(&b, "// Code generated by pricing/gen from %s (%s); DO NOT EDIT.\n\n", tag, commit)
	b.WriteString("package pricing\n\n")
	b.WriteString("import \"time\"\n\n")

	fmt.Fprintf(&b, "const generatedTag = %q\n", tag)
	fmt.Fprintf(&b, "const generatedIndexCommit = %q\n\n", commit)

	// modelSeries, sorted by model id.
	ids := make([]string, 0, len(series))
	for id := range series {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	b.WriteString("// modelSeries maps a canonical anthropic model id to its\n")
	b.WriteString("// point-in-time input/output price history (half-open [from,to)).\n")
	b.WriteString("var modelSeries = map[string]modelHist{\n")
	for _, id := range ids {
		gm := series[id]
		fmt.Fprintf(&b, "\t%q: {\n", id)
		b.WriteString("\t\tinput: []priceInterval{\n")
		writeIntervals(&b, gm.Input)
		b.WriteString("\t\t},\n")
		b.WriteString("\t\toutput: []priceInterval{\n")
		writeIntervals(&b, gm.Output)
		b.WriteString("\t\t},\n")
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n\n")

	// modelAliases, sorted by alias.
	akeys := make([]string, 0, len(aliases))
	for a := range aliases {
		akeys = append(akeys, a)
	}
	sort.Strings(akeys)

	b.WriteString("// modelAliases maps an alias (short or display form) to the\n")
	b.WriteString("// canonical model id in modelSeries.\n")
	b.WriteString("var modelAliases = map[string]string{\n")
	for _, a := range akeys {
		fmt.Fprintf(&b, "\t%q: %q,\n", a, aliases[a])
	}
	b.WriteString("}\n")

	return format.Source(b.Bytes())
}

func writeIntervals(b *bytes.Buffer, ivs []genInterval) {
	for _, iv := range ivs {
		fmt.Fprintf(b, "\t\t\t{from: time.Date(%d, %d, %d, 0, 0, 0, 0, time.UTC), to: %s, priceUSD: %s},\n",
			iv.FromY, iv.FromM, iv.FromD, toExpr(iv), floatLit(iv.PriceUSD))
	}
}

func toExpr(iv genInterval) string {
	if !iv.HasTo {
		return "nil"
	}
	return fmt.Sprintf("ptrTime(time.Date(%d, %d, %d, 0, 0, 0, 0, time.UTC))", iv.ToY, iv.ToM, iv.ToD)
}

// floatLit formats a price as a stable Go float literal. We use the
// shortest decimal that round-trips, then ensure a decimal point so the
// literal is unambiguously a float.
func floatLit(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if !strings.ContainsAny(s, ".eE") {
		s += ".0"
	}
	return s
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func shortCommit(c string) string {
	if len(c) >= 7 {
		return c[:7]
	}
	return c
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pricing/gen: "+format+"\n", args...)
	os.Exit(1)
}
