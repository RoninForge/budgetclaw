package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintVersionLong verifies the long form includes all expected
// fields. We do not assert exact version strings because they come from
// ldflags at build time and will differ between `go test` and release
// builds.
func TestPrintVersionLong(t *testing.T) {
	var buf bytes.Buffer
	if err := printVersion(&buf, false); err != nil {
		t.Fatalf("printVersion: %v", err)
	}
	out := buf.String()

	for _, want := range []string{"budgetclaw", "commit:", "built:", "go:", "os/arch:"} {
		if !strings.Contains(out, want) {
			t.Errorf("long output missing %q\n--- full output ---\n%s", want, out)
		}
	}
}

// TestPrintVersionShort verifies the short form is exactly one line and
// contains no extra keys. Useful for shell scripts that do
// `budgetclaw version --short`.
func TestPrintVersionShort(t *testing.T) {
	var buf bytes.Buffer
	if err := printVersion(&buf, true); err != nil {
		t.Fatalf("printVersion: %v", err)
	}
	out := strings.TrimSpace(buf.String())

	if strings.Count(out, "\n") != 0 {
		t.Errorf("short output should be one line, got %q", out)
	}
	if out == "" {
		t.Error("short output is empty")
	}
	for _, forbidden := range []string{"commit:", "built:", "go:"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("short output should not contain %q, got %q", forbidden, out)
		}
	}
}

// TestVersionCmdRegistered verifies cobra sees the version subcommand.
// Guards against accidentally losing the AddCommand call in newRootCmd.
func TestVersionCmdRegistered(t *testing.T) {
	root := newRootCmd()
	if _, _, err := root.Find([]string{"version"}); err != nil {
		t.Fatalf("version subcommand not registered: %v", err)
	}
}
