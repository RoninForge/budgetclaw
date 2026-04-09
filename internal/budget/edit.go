package budget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// defaultConfigTOML is what `budgetclaw init` writes when no
// config file yet exists. Kept short and over-commented so new
// users can read it top-to-bottom and understand every line.
const defaultConfigTOML = `# budgetclaw configuration
# https://github.com/RoninForge/budgetclaw

[general]
# IANA timezone for day/week/month boundaries. Defaults to UTC.
# timezone = "Asia/Bangkok"

# Optional phone alerts via ntfy.
# Generate a long, unguessable topic name:
#   openssl rand -hex 24
#
# [alerts.ntfy]
# server = "https://ntfy.sh"
# topic  = "budgetclaw-REPLACE-ME"
# min_cost_usd = 0.50

# Example: warn when any project exceeds $10 of daily spend.
[[limit]]
project = "*"
branch  = "*"
period  = "daily"
cap_usd = 10.00
action  = "warn"
`

// WriteDefault creates the config file at `path` containing a
// minimal documented default. Does nothing if `path` already
// exists — re-running `budgetclaw init` is safe and never
// overwrites a user's hand-edited config.
//
// Parent directories are created with mode 0755.
func WriteDefault(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(defaultConfigTOML), 0o644)
}

// AddLimit loads the config file, appends a new [[limit]] rule,
// and writes the whole file back atomically. If the file does
// not exist it is created with just the new rule (and no
// [general] or [alerts] sections).
//
// Writing back destroys any hand-written comments — users who
// maintain their config by hand should prefer editing directly.
func AddLimit(path string, r Rule) error {
	t, err := loadTOML(path)
	if err != nil {
		return err
	}
	t.Limit = append(t.Limit, tomlLimit{
		Project: r.Project,
		Branch:  r.Branch,
		Period:  r.Period.String(),
		CapUSD:  r.CapUSD,
		Action:  r.Action.String(),
	})
	return writeTOML(path, t)
}

// RemoveLimit removes every [[limit]] whose (project, branch, period)
// match the given selector after default-normalization (empty
// project/branch become "*", empty period becomes "daily").
// Returns the number of rules removed.
//
// Returns (0, nil) if the file does not exist, treating "nothing
// to remove" as a successful no-op.
func RemoveLimit(path string, project, branch string, period Period) (int, error) {
	if project == "" {
		project = "*"
	}
	if branch == "" {
		branch = "*"
	}

	t, err := loadTOML(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	kept := make([]tomlLimit, 0, len(t.Limit))
	removed := 0
	wantPeriod := period.String()

	for _, l := range t.Limit {
		lp := l.Project
		if lp == "" {
			lp = "*"
		}
		lb := l.Branch
		if lb == "" {
			lb = "*"
		}
		lper := l.Period
		if lper == "" {
			lper = "daily"
		}

		if lp == project && lb == branch && lper == wantPeriod {
			removed++
			continue
		}
		kept = append(kept, l)
	}

	if removed == 0 {
		return 0, nil
	}
	t.Limit = kept
	if err := writeTOML(path, t); err != nil {
		return 0, err
	}
	return removed, nil
}

// SetNtfyConfig rewrites the [alerts.ntfy] section. An empty
// server or topic string clears the configured value, turning
// the alerts layer back into a noop.
func SetNtfyConfig(path, server, topic string, minCostUSD float64) error {
	t, err := loadTOML(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if t == nil {
		t = &tomlConfig{}
	}
	t.Alerts.Ntfy.Server = server
	t.Alerts.Ntfy.Topic = topic
	t.Alerts.Ntfy.MinCostUSD = minCostUSD
	return writeTOML(path, t)
}

// loadTOML reads and decodes the config file into a tomlConfig.
// Returns (empty tomlConfig, nil) for a missing file so callers
// can treat "no config" and "empty config" identically.
func loadTOML(path string) (*tomlConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &tomlConfig{}, nil
		}
		return nil, err
	}
	var t tomlConfig
	if err := toml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return &t, nil
}

// writeTOML encodes the tomlConfig back to disk atomically via
// tmp-file-plus-rename so a crash mid-write cannot leave a
// corrupted config.
func writeTOML(path string, t *tomlConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}

	enc := toml.NewEncoder(f)
	if err := enc.Encode(t); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("encode toml: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
