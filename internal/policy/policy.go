// Package policy holds Guard Mode's remote budget policies: the caps a
// Goei team owner set that this device enforces locally. Policies are
// fetched from the Goei server (via GET /api/policy or the sync-response
// piggyback), cached on disk so enforcement survives a network blip and a
// restart, and never invent a stricter rule than the server sent.
//
// Remote policies are kept OUT of the user's config.toml on purpose: that
// file is hand-editable and its TOML round-trip drops comments, so mixing
// server-owned rules into it would clobber user intent. The cache is a
// separate, machine-managed JSON file.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/goei"
	"github.com/RoninForge/budgetclaw/internal/paths"
)

// Policy is one remote budget cap, resolved for this device.
type Policy struct {
	ID               string `json:"id"`
	ScopeType        string `json:"scopeType"`  // team | project | dev
	ScopeValue       string `json:"scopeValue"` // "" | project name
	Period           string `json:"period"`     // day | week | month
	CapCents         int    `json:"capCents"`
	Enforcement      string `json:"enforcement"` // local_exact | server_aggregate
	Action           string `json:"action"`      // warn | kill
	ServerSpentCents int    `json:"serverSpentCents"`
	AsOf             string `json:"asOf"`
	SetBy            string `json:"setBy"`
}

// Bundle is the cached policy set plus the ETag needed for a cheap
// conditional refresh and the time it was fetched (shown in `guard status`).
type Bundle struct {
	PolicyVersion int      `json:"policyVersion"`
	ETag          string   `json:"etag"`
	FetchedAt     string   `json:"fetchedAt"`
	Policies      []Policy `json:"policies"`
}

// FromWire converts one server WirePolicy into a Policy.
func FromWire(w goei.WirePolicy) Policy {
	return Policy{
		ID:               w.ID,
		ScopeType:        w.Scope.Type,
		ScopeValue:       w.Scope.Value,
		Period:           w.Period,
		CapCents:         w.CapCents,
		Enforcement:      w.Enforcement,
		Action:           w.Action,
		ServerSpentCents: w.ServerSpentCents,
		AsOf:             w.AsOf,
		SetBy:            w.SetBy,
	}
}

// BundleFromResponse builds a cacheable Bundle from a server response.
func BundleFromResponse(pr *goei.PolicyResponse, etag, fetchedAt string) *Bundle {
	b := &Bundle{PolicyVersion: 0, ETag: etag, FetchedAt: fetchedAt}
	if pr != nil {
		b.PolicyVersion = pr.PolicyVersion
		for _, w := range pr.Policies {
			b.Policies = append(b.Policies, FromWire(w))
		}
	}
	return b
}

// IsLocalExact reports whether this policy is enforceable against the local
// rollup (and thus kill-eligible). Server-aggregate policies are warn-only.
func (p Policy) IsLocalExact() bool { return p.Enforcement == "local_exact" }

// CapUSD is the cap in dollars.
func (p Policy) CapUSD() float64 { return float64(p.CapCents) / 100 }

// MapPeriod maps the server's period string to budgetclaw's period enum.
func MapPeriod(s string) budget.Period {
	switch s {
	case "day":
		return budget.PeriodDaily
	case "week":
		return budget.PeriodWeekly
	case "month":
		return budget.PeriodMonthly
	default:
		return budget.PeriodMonthly
	}
}

// LocalExact returns only the locally-enforceable policies.
func LocalExact(policies []Policy) []Policy {
	var out []Policy
	for _, p := range policies {
		if p.IsLocalExact() {
			out = append(out, p)
		}
	}
	return out
}

// Aggregate returns only the server-aggregate (warn-only) policies.
func Aggregate(policies []Policy) []Policy {
	var out []Policy
	for _, p := range policies {
		if !p.IsLocalExact() {
			out = append(out, p)
		}
	}
	return out
}

const cacheFile = "policies.json"

func cachePath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(dir, cacheFile), nil
}

// Load reads the cached policy bundle. A missing cache is not an error: it
// returns an empty bundle so a first run behaves like "no policies".
func Load() (*Bundle, error) {
	path, err := cachePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Bundle{}, nil
		}
		return nil, fmt.Errorf("read policy cache: %w", err)
	}
	var b Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		// A corrupt cache should not wedge the watcher; treat as empty.
		return &Bundle{}, nil
	}
	return &b, nil
}

// Save writes the policy bundle atomically (temp-file-plus-rename), mode
// 0600, creating the state dir if needed.
func Save(b *Bundle) error {
	path, err := cachePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal policy cache: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write policy cache: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename policy cache: %w", err)
	}
	return nil
}
