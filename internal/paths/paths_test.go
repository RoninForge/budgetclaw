package paths

import (
	"path/filepath"
	"testing"
)

// withFakeEnv swaps the package-level envGet and homeDir for the
// duration of a test and restores them via t.Cleanup. This is safer
// than t.Setenv because it also lets us fake UserHomeDir() without
// poking at the real $HOME.
func withFakeEnv(t *testing.T, env map[string]string, home string) {
	t.Helper()
	origEnv := envGet
	origHome := homeDir

	envGet = func(k string) string { return env[k] }
	homeDir = func() (string, error) { return home, nil }

	t.Cleanup(func() {
		envGet = origEnv
		homeDir = origHome
	})
}

func TestXDGDefaults(t *testing.T) {
	withFakeEnv(t, map[string]string{}, "/home/fake")

	cases := map[string]struct {
		fn   func() (string, error)
		want string
	}{
		"config": {ConfigDir, filepath.Join("/home/fake", ".config", "budgetclaw")},
		"state":  {StateDir, filepath.Join("/home/fake", ".local", "state", "budgetclaw")},
		"data":   {DataDir, filepath.Join("/home/fake", ".local", "share", "budgetclaw")},
		"cache":  {CacheDir, filepath.Join("/home/fake", ".cache", "budgetclaw")},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", name, got, tc.want)
			}
		})
	}
}

func TestXDGOverrides(t *testing.T) {
	env := map[string]string{
		"XDG_CONFIG_HOME": "/xdg/config",
		"XDG_STATE_HOME":  "/xdg/state",
		"XDG_DATA_HOME":   "/xdg/data",
		"XDG_CACHE_HOME":  "/xdg/cache",
	}
	withFakeEnv(t, env, "/home/fake")

	cases := map[string]struct {
		fn   func() (string, error)
		want string
	}{
		"config": {ConfigDir, "/xdg/config/budgetclaw"},
		"state":  {StateDir, "/xdg/state/budgetclaw"},
		"data":   {DataDir, "/xdg/data/budgetclaw"},
		"cache":  {CacheDir, "/xdg/cache/budgetclaw"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tc.fn()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got != tc.want {
				t.Errorf("%s: got %q, want %q", name, got, tc.want)
			}
		})
	}
}

// TestEmptyXDGFallsBack verifies that an XDG variable set to the empty
// string is treated as unset, per the XDG spec. Some shells export
// variables with empty values and we must not treat that as a valid
// root.
func TestEmptyXDGFallsBack(t *testing.T) {
	withFakeEnv(t, map[string]string{"XDG_CONFIG_HOME": ""}, "/home/fake")
	got, err := ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/fake", ".config", "budgetclaw")
	if got != want {
		t.Errorf("empty XDG_CONFIG_HOME should fall back: got %q, want %q", got, want)
	}
}

func TestClaudeProjectsDir(t *testing.T) {
	withFakeEnv(t, map[string]string{}, "/home/fake")
	got, err := ClaudeProjectsDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/fake", ".claude", "projects")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
