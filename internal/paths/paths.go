// Package paths resolves filesystem locations for budgetclaw's config,
// state, data, and cache, following the XDG Base Directory Specification.
//
// XDG is the 2026 expectation for new CLI tools: Linux, macOS, and modern
// shells all respect it, and it keeps user home directories clean.
// Legacy tools that drop a single ~/.toolname directory are increasingly
// seen as anti-patterns. budgetclaw goes XDG-first from day one.
//
// Layout:
//
//	$XDG_CONFIG_HOME/budgetclaw/   config.toml, limit rules
//	$XDG_STATE_HOME/budgetclaw/    state.db (rollups, events)
//	$XDG_DATA_HOME/budgetclaw/     lockfiles/, plugin bundles
//	$XDG_CACHE_HOME/budgetclaw/    pricing table cache
//
// Defaults when XDG_* are unset (per the spec):
//
//	XDG_CONFIG_HOME → $HOME/.config
//	XDG_STATE_HOME  → $HOME/.local/state
//	XDG_DATA_HOME   → $HOME/.local/share
//	XDG_CACHE_HOME  → $HOME/.cache
//
// Every function returns an absolute path with "budgetclaw" appended.
// Callers are responsible for MkdirAll before writing.
package paths

import (
	"os"
	"path/filepath"
)

// appName is the directory leaf appended to every XDG root.
const appName = "budgetclaw"

// xdgOrDefault returns $envVar if non-empty, otherwise $HOME/<fallback>.
// Exposed to tests via envGetter / homeGetter variables below.
func xdgOrDefault(envVar, fallback string) (string, error) {
	if v := envGet(envVar); v != "" {
		return filepath.Join(v, appName), nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback, appName), nil
}

// ConfigDir returns the directory holding config.toml and limit rules.
// Honors XDG_CONFIG_HOME, defaults to $HOME/.config/budgetclaw.
func ConfigDir() (string, error) {
	return xdgOrDefault("XDG_CONFIG_HOME", ".config")
}

// StateDir returns the directory holding state.db and rollups.
// Honors XDG_STATE_HOME, defaults to $HOME/.local/state/budgetclaw.
func StateDir() (string, error) {
	return xdgOrDefault("XDG_STATE_HOME", filepath.Join(".local", "state"))
}

// DataDir returns the directory holding lockfiles and plugin bundles.
// Honors XDG_DATA_HOME, defaults to $HOME/.local/share/budgetclaw.
func DataDir() (string, error) {
	return xdgOrDefault("XDG_DATA_HOME", filepath.Join(".local", "share"))
}

// CacheDir returns the directory holding the pricing table cache and
// any other regenerable artifacts. Honors XDG_CACHE_HOME, defaults to
// $HOME/.cache/budgetclaw.
func CacheDir() (string, error) {
	return xdgOrDefault("XDG_CACHE_HOME", ".cache")
}

// ClaudeProjectsDir returns $HOME/.claude/projects, the directory
// Claude Code writes its session JSONL logs to. This path is
// Claude-Code-owned and not configurable via XDG.
func ClaudeProjectsDir() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// --- indirection for tests ---
//
// We do not call os.Getenv / os.UserHomeDir directly from the exported
// functions; we go through these package-level variables so tests can
// inject fakes without touching the real environment.

var (
	envGet  = os.Getenv
	homeDir = os.UserHomeDir
)
