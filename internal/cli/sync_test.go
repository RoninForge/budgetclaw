package cli

import (
	"testing"
	"time"
)

func TestResolveSinceExplicitDate(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	got, err := resolveSince("2026-06-01", 30, loc, now)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("resolveSince explicit = %v, want %v", got, want)
	}
}

func TestResolveSinceDaysWindow(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	got, err := resolveSince("", 7, loc, now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.AddDate(0, 0, -7)
	if !got.Equal(want) {
		t.Errorf("resolveSince days = %v, want %v", got, want)
	}
}

func TestResolveSinceClampsNonPositiveDays(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 6, 10, 15, 0, 0, 0, loc)
	got, err := resolveSince("", 0, loc, now)
	if err != nil {
		t.Fatal(err)
	}
	want := now.AddDate(0, 0, -1)
	if !got.Equal(want) {
		t.Errorf("resolveSince(0 days) = %v, want %v (clamped to 1)", got, want)
	}
}

func TestResolveSinceBadDate(t *testing.T) {
	if _, err := resolveSince("not-a-date", 30, time.UTC, time.Now()); err == nil {
		t.Error("expected error for malformed --since")
	}
}

func TestEndpointOrDefault(t *testing.T) {
	if got := endpointOrDefault(""); got == "" {
		t.Error("empty endpoint should fall back to default")
	}
	if got := endpointOrDefault("https://x"); got != "https://x" {
		t.Errorf("explicit endpoint = %q, want passthrough", got)
	}
}
