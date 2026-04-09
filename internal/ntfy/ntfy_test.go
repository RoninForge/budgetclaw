package ntfy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// zeroBackoff makes retry tests run instantly.
func zeroBackoff(_ int) time.Duration { return 0 }

// newTestServer returns a httptest server whose handler records
// every request into a slice and replies with the provided status
// codes in sequence. The final status is reused if requests
// exceed the provided slice.
type recordedReq struct {
	method  string
	path    string
	headers http.Header
	body    string
}

func newTestServer(t *testing.T, statuses ...int) (*httptest.Server, *[]recordedReq) {
	t.Helper()
	if len(statuses) == 0 {
		statuses = []int{http.StatusOK}
	}
	var recorded []recordedReq
	var i int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		recorded = append(recorded, recordedReq{
			method:  r.Method,
			path:    r.URL.Path,
			headers: r.Header.Clone(),
			body:    string(body),
		})
		idx := int(atomic.AddInt32(&i, 1)) - 1
		if idx >= len(statuses) {
			idx = len(statuses) - 1
		}
		w.WriteHeader(statuses[idx])
	}))
	t.Cleanup(srv.Close)
	return srv, &recorded
}

// TestNewNoopWhenUnconfigured returns a noop client for empty
// Server or Topic so callers never nil-check.
func TestNewNoopWhenUnconfigured(t *testing.T) {
	cases := []Options{
		{},
		{Server: "https://ntfy.sh"},
		{Topic: "foo"},
	}
	for _, opts := range cases {
		c := New(opts)
		if !c.IsNoop() {
			t.Errorf("expected noop for %+v", opts)
		}
		// Send should succeed as a no-op.
		if err := c.Send(context.Background(), Message{Message: "hi"}); err != nil {
			t.Errorf("noop Send error: %v", err)
		}
	}
}

// TestNilClientSend exercises the nil-receiver noop path so
// optional client fields can be stored as *Client without
// nil-checks at every call site.
func TestNilClientSend(t *testing.T) {
	var c *Client
	if err := c.Send(context.Background(), Message{Message: "hi"}); err != nil {
		t.Errorf("nil client Send should be no-op, got %v", err)
	}
}

// TestSendEmptyMessageRejected guards a footgun: an empty body
// would silently succeed on ntfy's side, producing a blank
// notification on the phone.
func TestSendEmptyMessageRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	c := New(Options{Server: srv.URL, Topic: "x"})
	err := c.Send(context.Background(), Message{})
	if err == nil {
		t.Error("expected error for empty message")
	}
}

// TestSendHappyPath verifies the request shape: URL, method,
// headers, and body all match what we set on the Message.
func TestSendHappyPath(t *testing.T) {
	srv, reqs := newTestServer(t, http.StatusOK)
	c := New(Options{
		Server:      srv.URL,
		Topic:       "my-topic",
		BackoffFunc: zeroBackoff,
	})

	err := c.Send(context.Background(), Message{
		Title:    "Budget breach",
		Message:  "myapp/main hit $5.10",
		Priority: PriorityHigh,
		Tags:     []string{"warning", "money"},
		Click:    "https://roninforge.org/budgetclaw",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(*reqs) != 1 {
		t.Fatalf("expected 1 request, got %d", len(*reqs))
	}
	r := (*reqs)[0]

	if r.method != http.MethodPost {
		t.Errorf("method = %s, want POST", r.method)
	}
	if r.path != "/my-topic" {
		t.Errorf("path = %s, want /my-topic", r.path)
	}
	if r.body != "myapp/main hit $5.10" {
		t.Errorf("body = %q", r.body)
	}
	if got := r.headers.Get("Title"); got != "Budget breach" {
		t.Errorf("Title header = %q", got)
	}
	if got := r.headers.Get("Priority"); got != "4" {
		t.Errorf("Priority header = %q", got)
	}
	if got := r.headers.Get("Tags"); got != "warning,money" {
		t.Errorf("Tags header = %q", got)
	}
	if got := r.headers.Get("Click"); got != "https://roninforge.org/budgetclaw" {
		t.Errorf("Click header = %q", got)
	}
	if got := r.headers.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := r.headers.Get("User-Agent"); !strings.HasPrefix(got, "budgetclaw/") {
		t.Errorf("User-Agent = %q", got)
	}
}

// TestSendOptionalHeadersOmitted verifies that empty/zero
// Message fields produce no header at all, letting the server
// apply its own defaults. This matters because ntfy treats
// Priority=0 as "not set", and we shouldn't force a default on
// the client side.
func TestSendOptionalHeadersOmitted(t *testing.T) {
	srv, reqs := newTestServer(t)
	c := New(Options{Server: srv.URL, Topic: "x"})

	err := c.Send(context.Background(), Message{Message: "bare"})
	if err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	for _, h := range []string{"Title", "Priority", "Tags", "Click"} {
		if got := r.headers.Get(h); got != "" {
			t.Errorf("%s header should be absent, got %q", h, got)
		}
	}
}

// TestSendTrimsTrailingSlashFromServer ensures an accidental
// trailing "/" in the configured Server URL doesn't produce a
// double-slash in the request path.
func TestSendTrimsTrailingSlashFromServer(t *testing.T) {
	srv, reqs := newTestServer(t)
	c := New(Options{Server: srv.URL + "/", Topic: "x"})
	_ = c.Send(context.Background(), Message{Message: "hi"})
	if (*reqs)[0].path != "/x" {
		t.Errorf("path = %q, want /x", (*reqs)[0].path)
	}
}

// TestSend4xxNoRetry confirms 4xx responses fail immediately.
// Retrying a client error (bad topic, unauthorized) wastes time
// and bandwidth without any chance of recovery.
func TestSend4xxNoRetry(t *testing.T) {
	srv, reqs := newTestServer(t, http.StatusBadRequest)
	c := New(Options{
		Server: srv.URL, Topic: "x",
		MaxRetries:  5,
		BackoffFunc: zeroBackoff,
	})

	err := c.Send(context.Background(), Message{Message: "hi"})
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if len(*reqs) != 1 {
		t.Errorf("expected exactly 1 request on 4xx (no retry), got %d", len(*reqs))
	}
}

// TestSend5xxRetriesThenFails confirms we retry MaxRetries+1
// times on persistent 5xx and return the last error.
func TestSend5xxRetriesThenFails(t *testing.T) {
	srv, reqs := newTestServer(t, http.StatusInternalServerError)
	c := New(Options{
		Server: srv.URL, Topic: "x",
		MaxRetries:  2,
		BackoffFunc: zeroBackoff,
	})

	err := c.Send(context.Background(), Message{Message: "hi"})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if len(*reqs) != 3 { // initial + 2 retries
		t.Errorf("expected 3 requests, got %d", len(*reqs))
	}
}

// TestSend5xxThenSuccessRetries demonstrates recovery from a
// transient server error.
func TestSend5xxThenSuccessRetries(t *testing.T) {
	srv, reqs := newTestServer(t,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusOK,
	)
	c := New(Options{
		Server: srv.URL, Topic: "x",
		MaxRetries:  5,
		BackoffFunc: zeroBackoff,
	})

	err := c.Send(context.Background(), Message{Message: "hi"})
	if err != nil {
		t.Fatalf("expected recovery, got %v", err)
	}
	if len(*reqs) != 3 {
		t.Errorf("expected 3 requests, got %d", len(*reqs))
	}
}

// TestSendNetworkErrorRetries verifies we retry on transport
// errors (server closed, connection refused). We simulate this
// by pointing the client at a closed server.
func TestSendNetworkErrorRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // immediately close so all connections fail

	c := New(Options{
		Server: url, Topic: "x",
		MaxRetries:  2,
		BackoffFunc: zeroBackoff,
	})

	err := c.Send(context.Background(), Message{Message: "hi"})
	if err == nil {
		t.Fatal("expected error on closed server")
	}
	// We don't assert the exact attempt count here because the
	// network stack may short-circuit on some systems. We just
	// want to know the call terminated and surfaced an error.
}

// TestSendContextCancelDuringBackoff verifies that ctx.Done
// aborts a retry loop before the next attempt fires.
func TestSendContextCancelDuringBackoff(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusInternalServerError)
	c := New(Options{
		Server: srv.URL, Topic: "x",
		MaxRetries: 5,
		// Non-zero backoff so the select{} case has something to
		// wait on and can observe ctx cancellation.
		BackoffFunc: func(_ int) time.Duration { return 500 * time.Millisecond },
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay, after the first attempt has
	// returned 500 and we're waiting on backoff.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := c.Send(ctx, Message{Message: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

// TestSendWarn + TestSendKill verify the priority/tags shortcuts.
func TestSendWarn(t *testing.T) {
	srv, reqs := newTestServer(t)
	c := New(Options{Server: srv.URL, Topic: "x"})

	if err := c.SendWarn(context.Background(), "t", "b"); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if got := r.headers.Get("Priority"); got != "4" {
		t.Errorf("Priority = %q, want 4", got)
	}
	if got := r.headers.Get("Tags"); got != "warning" {
		t.Errorf("Tags = %q", got)
	}
}

func TestSendKill(t *testing.T) {
	srv, reqs := newTestServer(t)
	c := New(Options{Server: srv.URL, Topic: "x"})

	if err := c.SendKill(context.Background(), "t", "b"); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if got := r.headers.Get("Priority"); got != "5" {
		t.Errorf("Priority = %q, want 5", got)
	}
	if got := r.headers.Get("Tags"); got != "skull,money" {
		t.Errorf("Tags = %q", got)
	}
}

// TestClientTest covers the diagnostic helper.
func TestClientTest(t *testing.T) {
	srv, reqs := newTestServer(t)
	c := New(Options{Server: srv.URL, Topic: "x"})
	if err := c.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := (*reqs)[0]
	if !strings.Contains(r.body, "Test alert from budgetclaw") {
		t.Errorf("body = %q", r.body)
	}
	if got := r.headers.Get("Title"); got != "budgetclaw test" {
		t.Errorf("Title = %q", got)
	}
}

// TestDefaultBackoffSequence locks in the backoff curve so
// accidental changes to the constants fail noisily.
func TestDefaultBackoffSequence(t *testing.T) {
	cases := map[int]time.Duration{
		1: 500 * time.Millisecond,
		2: 1 * time.Second,
		3: 2 * time.Second,
		4: 4 * time.Second,
		5: 8 * time.Second,
		6: 10 * time.Second, // capped
		7: 10 * time.Second, // capped
	}
	for attempt, want := range cases {
		got := defaultBackoff(attempt)
		if got != want {
			t.Errorf("defaultBackoff(%d) = %v, want %v", attempt, got, want)
		}
	}
}

// TestMaxRetriesDefaultTo3 locks in the default so a typo in New
// doesn't silently turn retries off.
func TestMaxRetriesDefaultTo3(t *testing.T) {
	c := New(Options{Server: "https://ntfy.sh", Topic: "x"})
	if c.maxRetries != 3 {
		t.Errorf("maxRetries = %d, want 3", c.maxRetries)
	}
}
