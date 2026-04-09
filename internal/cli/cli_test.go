package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// setupXDG points every XDG base directory at a fresh temp dir
// for the lifetime of the test. Returns the temp dir root so
// assertions can locate files by hand.
func setupXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv("XDG_CACHE_HOME", dir)
	return dir
}

// execCmd runs a fresh root cobra command with the given args.
// Stdout and stderr are captured so assertions can check them
// without shelling out to a real process.
func execCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	out := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(errBuf)
	root.SetArgs(args)
	// Propagate a real background context so RunE functions that
	// call cmd.Context() get a non-nil value.
	root.SetContext(context.Background())
	err = root.Execute()
	return out.String(), errBuf.String(), err
}

// --- init --------------------------------------------------------

func TestInitCreatesXDGDirsAndConfig(t *testing.T) {
	root := setupXDG(t)

	stdout, _, err := execCmd(t, "init")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(stdout, "budgetclaw initialized") {
		t.Errorf("stdout missing 'initialized': %q", stdout)
	}

	// Config file must now exist.
	cfgPath := filepath.Join(root, "budgetclaw", "config.toml")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Errorf("expected config at %s, got %v", cfgPath, err)
	}
}

func TestInitIdempotent(t *testing.T) {
	setupXDG(t)

	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := execCmd(t, "init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "Config already present") {
		t.Errorf("second init should report 'already present', got %q", stdout)
	}
}

// --- config path ------------------------------------------------

func TestConfigPath(t *testing.T) {
	root := setupXDG(t)
	stdout, _, err := execCmd(t, "config", "path")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "budgetclaw", "config.toml")
	if strings.TrimSpace(stdout) != want {
		t.Errorf("got %q, want %q", strings.TrimSpace(stdout), want)
	}
}

// --- limit set / list / rm --------------------------------------

func TestLimitSetListRm(t *testing.T) {
	setupXDG(t)

	// set
	stdout, _, err := execCmd(t, "limit", "set",
		"--project", "myapp", "--branch", "main",
		"--period", "daily", "--cap", "5.5", "--action", "kill")
	if err != nil {
		t.Fatalf("limit set: %v", err)
	}
	if !strings.Contains(stdout, "added limit") {
		t.Errorf("stdout = %q", stdout)
	}

	// list — one rule, the one we just added
	stdout, _, err = execCmd(t, "limit", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "myapp") || !strings.Contains(stdout, "main") {
		t.Errorf("list missing rule: %q", stdout)
	}
	if !strings.Contains(stdout, "$5.50") {
		t.Errorf("list missing cap: %q", stdout)
	}
	if !strings.Contains(stdout, "kill") {
		t.Errorf("list missing action: %q", stdout)
	}

	// rm
	stdout, _, err = execCmd(t, "limit", "rm",
		"--project", "myapp", "--branch", "main", "--period", "daily")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "removed 1") {
		t.Errorf("rm didn't remove: %q", stdout)
	}

	// list — empty
	stdout, _, err = execCmd(t, "limit", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No limit rules") {
		t.Errorf("expected 'No limit rules', got %q", stdout)
	}
}

func TestLimitSetRequiresCap(t *testing.T) {
	setupXDG(t)

	_, _, err := execCmd(t, "limit", "set", "--period", "daily")
	if err == nil {
		t.Error("expected error when --cap is missing")
	}
}

func TestLimitSetRejectsInvalidPeriod(t *testing.T) {
	setupXDG(t)

	_, _, err := execCmd(t, "limit", "set", "--period", "hourly", "--cap", "1")
	if err == nil || !strings.Contains(err.Error(), "period must be") {
		t.Errorf("expected period validation error, got %v", err)
	}
}

func TestLimitSetRejectsInvalidAction(t *testing.T) {
	setupXDG(t)

	_, _, err := execCmd(t, "limit", "set", "--cap", "1", "--action", "destroy")
	if err == nil || !strings.Contains(err.Error(), "action must be") {
		t.Errorf("expected action validation error, got %v", err)
	}
}

func TestLimitRmNoMatch(t *testing.T) {
	setupXDG(t)

	// No config → RemoveLimit returns (0, nil); CLI reports no match.
	stdout, _, err := execCmd(t, "limit", "rm", "--project", "ghost", "--period", "daily")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No matching limit") {
		t.Errorf("expected 'No matching', got %q", stdout)
	}
}

// --- status -----------------------------------------------------

func TestStatusNoActivity(t *testing.T) {
	setupXDG(t)
	// init so the db dir exists
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCmd(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No activity") {
		t.Errorf("expected 'No activity', got %q", stdout)
	}
}

// --- alerts setup + test ----------------------------------------

func TestAlertsSetup(t *testing.T) {
	setupXDG(t)

	stdout, _, err := execCmd(t, "alerts", "setup",
		"--server", "https://ntfy.sh",
		"--topic", "my-test-topic",
		"--min-cost-usd", "0.50")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "configured ntfy") {
		t.Errorf("stdout = %q", stdout)
	}
}

func TestAlertsSetupRequiresTopic(t *testing.T) {
	setupXDG(t)

	_, _, err := execCmd(t, "alerts", "setup", "--server", "https://ntfy.sh")
	if err == nil {
		t.Error("expected error when --topic is missing")
	}
}

func TestAlertsTestUnconfigured(t *testing.T) {
	setupXDG(t)
	// init so config exists but without ntfy
	if _, _, err := execCmd(t, "init"); err != nil {
		t.Fatal(err)
	}

	_, _, err := execCmd(t, "alerts", "test")
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Errorf("expected 'not configured' error, got %v", err)
	}
}

func TestAlertsTestConfigured(t *testing.T) {
	setupXDG(t)

	// Point ntfy at a test server.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if _, _, err := execCmd(t, "alerts", "setup",
		"--server", srv.URL, "--topic", "x"); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := execCmd(t, "alerts", "test")
	if err != nil {
		t.Fatalf("alerts test: %v", err)
	}
	if !strings.Contains(stdout, "sent test notification") {
		t.Errorf("stdout = %q", stdout)
	}
}

// --- unlock / locks list -----------------------------------------

func TestUnlockNonExistent(t *testing.T) {
	setupXDG(t)

	stdout, _, err := execCmd(t, "unlock", "ghost-project")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "not locked") {
		t.Errorf("expected 'not locked', got %q", stdout)
	}
}

func TestLocksListEmpty(t *testing.T) {
	setupXDG(t)

	stdout, _, err := execCmd(t, "locks", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No active locks") {
		t.Errorf("expected 'No active locks', got %q", stdout)
	}
}

// --- root help ---------------------------------------------------

func TestRootHelpListsCommands(t *testing.T) {
	// Help output should advertise every top-level command we
	// registered in newRootCmd. This is a regression guard against
	// accidental AddCommand omissions.
	root := newRootCmd()
	cmds := map[string]bool{}
	for _, c := range root.Commands() {
		cmds[c.Name()] = true
	}
	for _, want := range []string{
		"init", "status", "limit", "alerts", "unlock",
		"locks", "config", "watch", "version",
	} {
		if !cmds[want] {
			t.Errorf("missing subcommand %q", want)
		}
	}
}

// --- version still works -----------------------------------------

func TestVersionStillWorks(t *testing.T) {
	stdout, _, err := execCmd(t, "version", "--short")
	if err != nil {
		t.Fatal(err)
	}
	// Smoke test: any non-empty output is fine — the exact value
	// depends on whether tests run against ldflags or BuildInfo.
	if strings.TrimSpace(stdout) == "" {
		t.Error("version --short produced empty output")
	}
}

// --- ensures common.configPath honors XDG override --------------

func TestConfigPathHelperHonorsXDG(t *testing.T) {
	root := setupXDG(t)
	got, err := configPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "budgetclaw", "config.toml")
	if got != want {
		t.Errorf("configPath = %q, want %q", got, want)
	}
}

// --- prune output on watch (compile check only) ------------------

// TestNewWatchCmdRegistered confirms the watch command can be
// instantiated without panics. We do NOT run `Run` because it
// would block on fsnotify events indefinitely in CI.
func TestNewWatchCmdRegistered(t *testing.T) {
	cmd := newWatchCmd()
	if cmd.Use != "watch" {
		t.Errorf("watch cmd Use = %q", cmd.Use)
	}
	// Verify the --verbose flag is wired.
	if cmd.Flag("verbose") == nil {
		t.Error("watch cmd missing --verbose flag")
	}
}

// Defensive: ensure cobra.Command.SilenceUsage is set on root so
// errors returned by RunE don't spew help text on top of the
// error message.
func TestRootSilencesUsageOnError(t *testing.T) {
	root := newRootCmd()
	if !root.SilenceUsage {
		t.Error("SilenceUsage should be true")
	}
}

// Assertion on exact help formatting is fragile; skip it.
// But do make sure Help returns without error.
func TestRootHelpExecutes(t *testing.T) {
	root := newRootCmd()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	// Trigger help via the "help" subcommand.
	root.SetArgs([]string{"help"})
	if err := root.Execute(); err != nil {
		t.Errorf("help: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("help produced no output")
	}
}

// avoid unused-import warning when cobra is only referenced in
// function types above.
var _ = cobra.Command{}
