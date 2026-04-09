// Package ntfy is a minimal client for ntfy.sh-compatible push
// notification servers. It is used by the budgetclaw watcher to
// deliver phone alerts when a budget cap is warned or killed.
//
// The client talks to ntfy's header-style HTTP API:
//
//	POST https://<server>/<topic>
//	Title: Budget breach
//	Priority: 4
//	Tags: warning,money
//	Click: https://roninforge.org/budgetclaw
//	Body: myapp/main hit $5.10 daily cap
//
// On 2xx the call succeeds. On 4xx it fails immediately (client
// error: bad topic, bad server, bad headers — no point retrying).
// On 5xx or network errors it retries with exponential backoff up
// to MaxRetries times. Context cancellation aborts mid-retry.
//
// A Client with an empty Server or Topic becomes a "noop" client
// whose Send method silently returns nil. That lets the watcher
// always call ntfy.Send regardless of whether the user has
// configured alerts — no nil checks required at call sites.
package ntfy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/RoninForge/budgetclaw/internal/version"
)

// Priority levels match ntfy's scale: 1=min, 3=default, 5=max.
const (
	PriorityMin     = 1
	PriorityLow     = 2
	PriorityDefault = 3
	PriorityHigh    = 4
	PriorityMax     = 5
)

// Message is one push notification. Title, Priority, Tags, and
// Click are optional; Message is the only required field.
type Message struct {
	// Title is the notification headline (H1 on the phone).
	Title string
	// Message is the body text. Required.
	Message string
	// Priority is the ntfy priority level (1-5). Zero means "use
	// the server default" — no header sent.
	Priority int
	// Tags are shown as emoji on the notification. See
	// https://docs.ntfy.sh/emojis/ for the full list.
	Tags []string
	// Click is a URL the phone opens when the notification is
	// tapped. Empty string means no action.
	Click string
}

// Options configures a Client. Server and Topic are the only
// required fields. The rest have sensible defaults.
type Options struct {
	// Server is the base URL of the ntfy server, e.g.
	// "https://ntfy.sh" or "https://push.roninforge.org".
	Server string
	// Topic is the ntfy topic name. Treated as a shared secret:
	// anyone who knows the topic can subscribe and publish.
	Topic string
	// HTTPClient lets tests inject a custom transport. Defaults
	// to &http.Client{Timeout: 15*time.Second}.
	HTTPClient *http.Client
	// MaxRetries is the number of retry attempts on 5xx or
	// network errors. 0 means "no retry" (still one initial
	// attempt). Defaults to 3 (so up to 4 total attempts).
	MaxRetries int
	// BackoffFunc computes the sleep duration between retries.
	// Defaults to exponential: 500ms, 1s, 2s, ... capped at 10s.
	// Tests can pass a zero-duration function to skip the waits.
	BackoffFunc func(attempt int) time.Duration
}

// Client sends push notifications to a ntfy server.
type Client struct {
	server     string
	topic      string
	http       *http.Client
	maxRetries int
	backoff    func(attempt int) time.Duration
	noop       bool
}

// New constructs a Client from Options. If Server or Topic is
// empty, New returns a noop client whose Send is a silent no-op.
// This makes the "ntfy not configured" path a first-class zero
// value rather than a nil-check at every call site.
func New(opts Options) *Client {
	if opts.Server == "" || opts.Topic == "" {
		return &Client{noop: true}
	}

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	maxRetries := opts.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if opts.MaxRetries == 0 {
		maxRetries = 3
	}

	backoff := opts.BackoffFunc
	if backoff == nil {
		backoff = defaultBackoff
	}

	return &Client{
		server:     strings.TrimRight(opts.Server, "/"),
		topic:      opts.Topic,
		http:       httpClient,
		maxRetries: maxRetries,
		backoff:    backoff,
	}
}

// IsNoop reports whether this client is a configured-out noop
// (empty server or topic). Useful for `budgetclaw alerts status`
// output.
func (c *Client) IsNoop() bool { return c.noop }

// Send delivers a single notification. Returns nil on success,
// a non-nil error on irrecoverable failure, or ctx.Err() if the
// context is canceled mid-retry.
//
// A nil *Client is treated as a noop so callers who keep an
// optional client in a struct field don't need to nil-check.
func (c *Client) Send(ctx context.Context, m Message) error {
	if c == nil || c.noop {
		return nil
	}
	if m.Message == "" {
		return errors.New("ntfy: message body is required")
	}

	url := c.server + "/" + c.topic
	body := []byte(m.Message)
	headers := buildHeaders(m)

	var lastErr error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(c.backoff(attempt)):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("ntfy: build request: %w", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue // network error → retry
		}

		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return nil
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			// Client error: bad topic, bad header, unauthorized.
			// Retrying will not help — fail fast.
			return fmt.Errorf("ntfy: %s", resp.Status)
		default:
			// 5xx or weird status — retry.
			lastErr = fmt.Errorf("ntfy: %s", resp.Status)
		}
	}
	return fmt.Errorf("ntfy: send failed after %d attempts: %w", c.maxRetries+1, lastErr)
}

// SendWarn is a convenience wrapper that sets priority=high and
// adds a "warning" tag. Use for Action=warn verdicts.
func (c *Client) SendWarn(ctx context.Context, title, body string) error {
	return c.Send(ctx, Message{
		Title:    title,
		Message:  body,
		Priority: PriorityHigh,
		Tags:     []string{"warning"},
	})
}

// SendKill is a convenience wrapper that sets priority=max and
// tags the message as destructive. Use for Action=kill verdicts.
// The higher priority means iOS and Android will bypass Do Not
// Disturb for urgent breach notifications.
func (c *Client) SendKill(ctx context.Context, title, body string) error {
	return c.Send(ctx, Message{
		Title:    title,
		Message:  body,
		Priority: PriorityMax,
		Tags:     []string{"skull", "money"},
	})
}

// Test sends a diagnostic message with default priority so the
// user can verify end-to-end delivery. Wired to `budgetclaw alerts test`.
func (c *Client) Test(ctx context.Context) error {
	return c.Send(ctx, Message{
		Title:    "budgetclaw test",
		Message:  "Test alert from budgetclaw. If you see this, ntfy is configured correctly.",
		Priority: PriorityDefault,
		Tags:     []string{"white_check_mark"},
	})
}

// buildHeaders assembles the ntfy-specific HTTP headers plus our
// standard Content-Type and User-Agent. Zero-value fields in
// Message produce no header at all, letting the ntfy server apply
// its own defaults.
func buildHeaders(m Message) map[string]string {
	h := map[string]string{
		"Content-Type": "text/plain; charset=utf-8",
		"User-Agent":   "budgetclaw/" + version.Get().Version,
	}
	if m.Title != "" {
		h["Title"] = m.Title
	}
	if m.Priority > 0 {
		h["Priority"] = strconv.Itoa(m.Priority)
	}
	if len(m.Tags) > 0 {
		h["Tags"] = strings.Join(m.Tags, ",")
	}
	if m.Click != "" {
		h["Click"] = m.Click
	}
	return h
}

// defaultBackoff returns exponential backoff capped at 10 seconds.
// Sequence: 500ms, 1s, 2s, 4s, 8s, 10s, 10s, ...
func defaultBackoff(attempt int) time.Duration {
	d := 500 * time.Millisecond * time.Duration(1<<(attempt-1))
	if d > 10*time.Second {
		return 10 * time.Second
	}
	return d
}
