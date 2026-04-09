// Package budget loads budget rules from TOML and evaluates events
// against them.
//
// The evaluator is deliberately stateless and dependency-free: it
// takes a Config, a new event's (project, branch, timestamp), and a
// SpendFunc that queries current-period spend. It returns one
// Verdict per matching rule. The caller decides what to do
// (warn, kill, log).
//
// Keeping the evaluator a pure function means:
//   - we can test it with synthetic spend fakes (no sqlite needed)
//   - it never accidentally mutates state
//   - warning de-duplication belongs to the enforcer/notifier, not here
//
// Config format (TOML):
//
//	[general]
//	timezone = "Asia/Bangkok"  # optional; defaults to UTC
//
//	[alerts.ntfy]              # optional; re-exported for the ntfy client
//	server = "https://ntfy.sh"
//	topic  = "some-token"
//	min_cost_usd = 0.50
//
//	[[limit]]
//	project = "*"              # "*" or glob (feature/*) or exact
//	branch  = "*"
//	period  = "daily"          # daily | weekly | monthly
//	cap_usd = 10.00
//	action  = "warn"           # warn | kill
package budget

import (
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"time"

	"github.com/BurntSushi/toml"
)

// Period is the rolling window over which a cap is enforced.
type Period int

const (
	// PeriodDaily resets at local-midnight each day.
	PeriodDaily Period = iota
	// PeriodWeekly resets Monday 00:00 local time (ISO 8601 week start).
	PeriodWeekly
	// PeriodMonthly resets on the 1st of each local-calendar month.
	PeriodMonthly
)

// String returns the canonical TOML name for a period.
func (p Period) String() string {
	switch p {
	case PeriodDaily:
		return "daily"
	case PeriodWeekly:
		return "weekly"
	case PeriodMonthly:
		return "monthly"
	default:
		return "unknown"
	}
}

// Action is what the enforcer should do when a rule's cap is breached.
type Action int

const (
	// ActionWarn fires a notification only. Non-destructive.
	ActionWarn Action = iota
	// ActionKill SIGTERMs the matching claude process and writes a
	// lockfile to prevent silent relaunch.
	ActionKill
)

// String returns the canonical TOML name for an action.
func (a Action) String() string {
	switch a {
	case ActionWarn:
		return "warn"
	case ActionKill:
		return "kill"
	default:
		return "unknown"
	}
}

// Rule is one [[limit]] entry from the config file.
type Rule struct {
	Project string  // "*" or glob or exact project name
	Branch  string  // "*" or glob or exact branch name
	Period  Period
	CapUSD  float64
	Action  Action
}

// Matches reports whether this rule applies to the given
// (project, branch) pair. Glob semantics follow path.Match:
// "*" matches any sequence except "/", "?" matches one character,
// "[abc]" matches one of the characters in the set. The bare "*"
// and the empty string are both treated as "match anything,
// including paths with slashes" so users don't have to think about
// it.
func (r Rule) Matches(project, branch string) bool {
	return matchGlob(r.Project, project) && matchGlob(r.Branch, branch)
}

// Specificity returns a score used to order matching rules from
// most-specific to least-specific. Exact strings are worth more
// than wildcards. Ties are broken by config-order when Match sorts
// stably.
//
//	both exact     → 3
//	only project   → 2
//	only branch    → 1
//	both wildcard  → 0
func (r Rule) Specificity() int {
	score := 0
	if !isWildcard(r.Project) {
		score += 2
	}
	if !isWildcard(r.Branch) {
		score++
	}
	return score
}

// matchGlob wraps path.Match. The empty string and bare "*" are
// short-circuited to "match anything" so a rule with project="*"
// correctly matches branch names containing slashes (which
// path.Match would otherwise reject).
func matchGlob(pattern, s string) bool {
	if isWildcard(pattern) {
		return true
	}
	ok, err := path.Match(pattern, s)
	return err == nil && ok
}

func isWildcard(p string) bool { return p == "" || p == "*" }

// Config is the parsed, validated budget configuration.
type Config struct {
	// Timezone is used for period boundary computation. Defaults to
	// time.UTC when [general].timezone is unset.
	Timezone *time.Location

	// Rules is the ordered list of [[limit]] entries. Config.Match
	// returns matches sorted by specificity; Rules preserves input
	// order for diagnostic purposes.
	Rules []Rule

	// Alerts config is re-exported from the [alerts.ntfy] section so
	// the CLI only has to load one file. The budget package itself
	// doesn't use these fields.
	NtfyServer     string
	NtfyTopic      string
	NtfyMinCostUSD float64
}

// tomlConfig mirrors the TOML schema for deserialization. Keep it
// private so callers can't depend on the wire format.
type tomlConfig struct {
	General struct {
		Timezone string `toml:"timezone"`
	} `toml:"general"`
	Alerts struct {
		Ntfy struct {
			Server     string  `toml:"server"`
			Topic      string  `toml:"topic"`
			MinCostUSD float64 `toml:"min_cost_usd"`
		} `toml:"ntfy"`
	} `toml:"alerts"`
	Limit []tomlLimit `toml:"limit"`
}

type tomlLimit struct {
	Project string  `toml:"project"`
	Branch  string  `toml:"branch"`
	Period  string  `toml:"period"`
	CapUSD  float64 `toml:"cap_usd"`
	Action  string  `toml:"action"`
}

// ErrConfig wraps all config validation errors. Callers can use
// errors.Is to distinguish parse/validation failures from I/O errors.
var ErrConfig = errors.New("budget config")

// Parse decodes TOML bytes into a validated Config.
func Parse(data []byte) (*Config, error) {
	var t tomlConfig
	if err := toml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}

	cfg := &Config{
		NtfyServer:     t.Alerts.Ntfy.Server,
		NtfyTopic:      t.Alerts.Ntfy.Topic,
		NtfyMinCostUSD: t.Alerts.Ntfy.MinCostUSD,
	}

	if t.General.Timezone == "" {
		cfg.Timezone = time.UTC
	} else {
		loc, err := time.LoadLocation(t.General.Timezone)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid timezone %q: %v", ErrConfig, t.General.Timezone, err)
		}
		cfg.Timezone = loc
	}

	for i, raw := range t.Limit {
		r, err := parseLimit(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: limit rule %d: %v", ErrConfig, i+1, err)
		}
		cfg.Rules = append(cfg.Rules, r)
	}

	return cfg, nil
}

func parseLimit(l tomlLimit) (Rule, error) {
	if l.CapUSD < 0 {
		return Rule{}, fmt.Errorf("cap_usd must be >= 0, got %v", l.CapUSD)
	}

	r := Rule{
		Project: l.Project,
		Branch:  l.Branch,
		CapUSD:  l.CapUSD,
	}
	if r.Project == "" {
		r.Project = "*"
	}
	if r.Branch == "" {
		r.Branch = "*"
	}

	switch l.Period {
	case "daily", "":
		r.Period = PeriodDaily
	case "weekly":
		r.Period = PeriodWeekly
	case "monthly":
		r.Period = PeriodMonthly
	default:
		return Rule{}, fmt.Errorf("period must be daily/weekly/monthly, got %q", l.Period)
	}

	switch l.Action {
	case "warn", "":
		r.Action = ActionWarn
	case "kill":
		r.Action = ActionKill
	default:
		return Rule{}, fmt.Errorf("action must be warn/kill, got %q", l.Action)
	}

	return r, nil
}

// LoadFile reads and parses a TOML config file. Returns the
// underlying os error (e.g. os.ErrNotExist) for I/O failures and a
// wrapped ErrConfig for parse/validation failures.
func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Match returns every rule that applies to (project, branch),
// ordered most-specific first. Stable sort preserves config order
// among ties so users can predict which rule wins when
// specificities are equal.
//
// Returns nil (not empty slice) when no rules match, so the common
// `if len(matches) == 0` check is cheap.
func (c *Config) Match(project, branch string) []Rule {
	if c == nil {
		return nil
	}
	var matches []Rule
	for _, r := range c.Rules {
		if r.Matches(project, branch) {
			matches = append(matches, r)
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Specificity() > matches[j].Specificity()
	})
	return matches
}
