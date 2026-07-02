package cli

import (
	"os"
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

// TestResolveMachine asserts the override precedence for the per-machine
// identity: flag > GOEI_MACHINE env > [goei].machine config > OS
// hostname. This mirrors how the token resolves (flag > env > config).
func TestResolveMachine(t *testing.T) {
	t.Run("flag wins over env and config", func(t *testing.T) {
		t.Setenv(envMachine, "env-host")
		if got := resolveMachine("flag-host", "cfg-host"); got != "flag-host" {
			t.Errorf("resolveMachine = %q, want flag-host", got)
		}
	})
	t.Run("env wins over config", func(t *testing.T) {
		t.Setenv(envMachine, "env-host")
		if got := resolveMachine("", "cfg-host"); got != "env-host" {
			t.Errorf("resolveMachine = %q, want env-host", got)
		}
	})
	t.Run("config wins over hostname default", func(t *testing.T) {
		t.Setenv(envMachine, "")
		if got := resolveMachine("", "cfg-host"); got != "cfg-host" {
			t.Errorf("resolveMachine = %q, want cfg-host", got)
		}
	})
	t.Run("falls back to OS hostname", func(t *testing.T) {
		t.Setenv(envMachine, "")
		want, err := os.Hostname()
		if err != nil {
			t.Skip("hostname unavailable on this platform")
		}
		if got := resolveMachine("", ""); got != want {
			t.Errorf("resolveMachine = %q, want hostname %q", got, want)
		}
	})
}

func TestEndpointOrDefault(t *testing.T) {
	if got := endpointOrDefault(""); got == "" {
		t.Error("empty endpoint should fall back to default")
	}
	if got := endpointOrDefault("https://x"); got != "https://x" {
		t.Errorf("explicit endpoint = %q, want passthrough", got)
	}
}
