package refresh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/RoninForge/budgetclaw/internal/paths"
	"github.com/RoninForge/budgetclaw/internal/pricing"
)

// Fetching a signed pricing bundle.
//
// This is the only outbound request budgetclaw ever makes, it is off by
// default, and it is a plain conditional GET for a public CC-BY file.
// Nothing is sent: no key, no token, no identifier, no usage data, no
// query string, no cookie. The request carries a User-Agent and an
// If-None-Match, and that is the complete list.

// DefaultBundleURL is the published interval-history bundle. The detached
// signature is the same URL with ".minisig" appended.
const DefaultBundleURL = "https://roninforge.org/data/ai-price-index/history/anthropic.json"

// Limits. The bundle is about 22 KB today; the caps are generous enough
// to absorb real growth and tight enough to bound a hostile response.
const (
	maxBundleBytes    = 2 << 20 // 2 MiB
	maxSignatureBytes = 8 << 10 // 8 KiB
	requestTimeout    = 15 * time.Second
	maxRedirects      = 2
)

// ErrOffline reports that the fetch could not complete for an ordinary
// reason: no network, a timeout, a 5xx, a captive portal. Callers treat
// it as "try again later", never as a reason to change pricing.
var ErrOffline = errors.New("pricing refresh unavailable")

// ErrNotModified reports a 304. The cached table is already current.
var ErrNotModified = errors.New("not modified")

// Result describes what a refresh attempt did.
type Result struct {
	// Updated is true when a new table was installed.
	Updated bool
	// Tag and DataDate identify the installed dataset.
	Tag      string
	DataDate string
	// KeyName is the trusted key that verified it.
	KeyName string
	// Models is how many models the bundle carried.
	Models int
	// NotModified is true when the server said 304.
	NotModified bool
}

// cached is what we keep on disk between runs.
type cached struct {
	bundle []byte
	sig    []byte
	etag   string
}

// cacheDir is where the verified bundle is kept. Under the cache
// directory rather than state, because it is reconstructible from the
// network and safe to delete: losing it costs one extra fetch.
func cacheDir() (string, error) {
	base, err := paths.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "pricing"), nil
}

// loadCache reads the last verified bundle, if any. A missing or partial
// cache is not an error; it just means nothing to load.
func loadCache() (cached, bool) {
	dir, err := cacheDir()
	if err != nil {
		return cached{}, false
	}
	bundle, err := os.ReadFile(filepath.Join(dir, "anthropic.json"))
	if err != nil {
		return cached{}, false
	}
	sig, err := os.ReadFile(filepath.Join(dir, "anthropic.json.minisig"))
	if err != nil {
		return cached{}, false
	}
	etag, _ := os.ReadFile(filepath.Join(dir, "etag"))
	return cached{bundle: bundle, sig: sig, etag: string(etag)}, true
}

// saveCache stores a verified bundle. Written to a temp file and renamed
// so a crash cannot leave a half-written bundle that would fail
// verification on the next start.
func saveCache(c cached) error {
	dir, err := cacheDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	write := func(name string, data []byte) error {
		tmp := filepath.Join(dir, name+".tmp")
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(dir, name))
	}
	if err := write("anthropic.json", c.bundle); err != nil {
		return err
	}
	if err := write("anthropic.json.minisig", c.sig); err != nil {
		return err
	}
	return write("etag", []byte(c.etag))
}

// LoadCachedTable installs the last verified bundle from disk, if it is
// still trustworthy and still newer than the compiled-in table.
//
// Called at startup so an enabled client keeps its fresher prices across
// restarts without waiting for a network round trip, and without ever
// trusting the cache blindly: the signature is re-verified every time it
// is read, so tampering with or corrupting the cache file is caught.
func LoadCachedTable(now time.Time) (Result, error) {
	c, ok := loadCache()
	if !ok {
		return Result{}, nil
	}
	return installBundle(c.bundle, c.sig, now)
}

// Refresh fetches, verifies and installs the pricing bundle.
//
// Returns ErrNotModified when the server reports the cached copy is
// current, ErrOffline for any ordinary network failure, ErrBadSignature
// if verification fails, and ErrRejected if the content is implausible.
// On every error the table already in force is left alone.
func Refresh(ctx context.Context, url string, now time.Time) (Result, error) {
	return refreshWithClient(ctx, url, now, defaultClient())
}

// defaultClient is the production HTTP client: a hard timeout, a redirect
// cap, and the default transport so standard proxy environment variables
// are honored without any config of ours.
func defaultClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}
}

// refreshWithClient is Refresh with the HTTP client injected, so tests can
// exercise the real path against a local TLS server.
func refreshWithClient(ctx context.Context, url string, now time.Time, client *http.Client) (Result, error) {
	if url == "" {
		url = DefaultBundleURL
	}

	prev, hasPrev := loadCache()
	etag := ""
	if hasPrev {
		etag = prev.etag
	}

	bundle, newETag, err := get(ctx, client, url, etag, maxBundleBytes)
	if err != nil {
		if errors.Is(err, ErrNotModified) {
			return Result{NotModified: true}, ErrNotModified
		}
		return Result{}, err
	}

	// The signature is fetched unconditionally: it is tiny, and pairing a
	// fresh bundle with a stale signature is exactly the mismatch that
	// must not silently pass.
	sig, _, err := get(ctx, client, url+".minisig", "", maxSignatureBytes)
	if err != nil {
		return Result{}, err
	}

	res, err := installBundle(bundle, sig, now)
	if err != nil {
		return Result{}, err
	}

	// Only cache what verified and installed, so the cache can never be
	// a source of data the running process rejected.
	if err := saveCache(cached{bundle: bundle, sig: sig, etag: newETag}); err != nil {
		// A cache write failure costs one extra fetch next time; the
		// table is already installed, so this is not worth failing on.
		res.Updated = true
	}
	return res, nil
}

// installBundle verifies, parses, gate-checks and installs bundle bytes.
// Shared by the network path and the cache-load path so both apply
// exactly the same checks.
func installBundle(bundle, sig []byte, now time.Time) (Result, error) {
	v, err := verifySignature(bundle, sig, TrustedKeys)
	if err != nil {
		return Result{}, err
	}

	table, dataDate, err := parseBundle(bundle)
	if err != nil {
		return Result{}, err
	}

	// The trusted comment is signature-covered, so the tag in it can be
	// believed. Format: "ai-price-index <tag> dataModified=<date>".
	table.Tag = tagFromComment(v.TrustedComment)

	if err := checkPlausible(table, now); err != nil {
		return Result{}, err
	}

	if err := pricing.Install(table); err != nil {
		return Result{}, err
	}

	return Result{
		Updated:  true,
		Tag:      table.Tag,
		DataDate: dataDate,
		KeyName:  v.KeyName,
		Models:   len(table.Models),
	}, nil
}

// tagFromComment pulls the dataset tag out of a trusted comment, falling
// back to the whole comment so provenance is never blank.
func tagFromComment(comment string) string {
	for _, f := range splitFields(comment) {
		if len(f) > 1 && f[0] == 'v' {
			return f
		}
	}
	return comment
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		if r == ' ' || r == '\t' {
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// get performs one conditional GET.
//
// https only, a hard timeout, a redirect cap, and a body read through a
// limit reader so a hostile or misconfigured server cannot stream without
// bound. Standard HTTP_PROXY/HTTPS_PROXY/NO_PROXY are honored through the
// default transport, which matters on corporate networks and needs no
// config of ours.
func get(ctx context.Context, client *http.Client, url, etag string, limit int64) ([]byte, string, error) {
	if len(url) < 8 || url[:8] != "https://" {
		return nil, "", fmt.Errorf("%w: refusing a non-https url", ErrOffline)
	}

	if client == nil {
		client = defaultClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrOffline, err)
	}
	req.Header.Set("User-Agent", userAgent())
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrOffline, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, etag, ErrNotModified
	case resp.StatusCode != http.StatusOK:
		return nil, "", fmt.Errorf("%w: http %d", ErrOffline, resp.StatusCode)
	}

	// Read one byte past the cap so an oversized body is detected rather
	// than silently truncated into something that might still parse.
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrOffline, err)
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("%w: response exceeds %d bytes", ErrOffline, limit)
	}
	return body, resp.Header.Get("ETag"), nil
}

// userAgent identifies the client without carrying anything about the
// machine or the user.
func userAgent() string { return "budgetclaw/" + version }

// version is set by the cli package at startup so this package does not
// import it. Defaults to "dev" for tests.
var version = "dev"

// SetVersion records the running version for the User-Agent header.
func SetVersion(v string) {
	if v != "" {
		version = v
	}
}
