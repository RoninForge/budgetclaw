package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/policy"
)

// newGuardCmd creates the `budgetclaw guard` command tree: the opt-in and
// status for Guard Mode, where a Goei team owner's budget caps are enforced
// locally on this machine. Opting in is deliberate and explicit — a device
// never obeys a server until the user turns Guard Mode on.
func newGuardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "guard",
		Short: "Manage Guard Mode (remote team budget enforcement)",
		Long: `Guard Mode lets a Goei team owner set budget caps that budgetclaw
enforces on this machine (warn, or SIGTERM a runaway) even offline, even at
3am, even on an agent nobody is watching. It polices agents, not developers.

It is opt-in: a device never obeys a server until you turn it on.

  budgetclaw guard on      # accept your team's budget caps and enforce them
  budgetclaw guard off     # stop enforcing remote caps on this machine
  budgetclaw guard status  # show whether it is on and which caps are cached`,
	}
	cmd.AddCommand(newGuardOnCmd(), newGuardOffCmd(), newGuardStatusCmd())
	return cmd
}

func newGuardOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Turn Guard Mode on for this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGuardSet(cmd.OutOrStdout(), true)
		},
	}
}

func newGuardOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Turn Guard Mode off for this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGuardSet(cmd.OutOrStdout(), false)
		},
	}
}

func runGuardSet(out io.Writer, accept bool) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.SetAcceptRemotePolicies(path, accept); err != nil {
		return fmt.Errorf("update config: %w", err)
	}
	if accept {
		fmt.Fprintln(out, "Guard Mode is ON. `budgetclaw watch` will fetch and enforce your team's budget caps locally.")
		fmt.Fprintln(out, "Ensure a device token is saved: `budgetclaw sync --save --token goei_dt_...`.")
	} else {
		fmt.Fprintln(out, "Guard Mode is OFF. Remote budget caps are no longer enforced on this machine.")
	}
	fmt.Fprintf(out, "config: %s\n", path)
	return nil
}

func newGuardStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show Guard Mode state and cached policies",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runGuardStatus(cmd.OutOrStdout())
		},
	}
}

func runGuardStatus(out io.Writer) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}
	state := "off"
	if cfg.AcceptRemotePolicies {
		state = "on"
	}
	fmt.Fprintf(out, "Guard Mode: %s\n", state)
	if cfg.GoeiToken == "" {
		fmt.Fprintln(out, "Token: none (run `budgetclaw sync --save --token goei_dt_...`)")
	} else {
		fmt.Fprintln(out, "Token: configured")
	}

	b, err := policy.Load()
	if err != nil {
		return err
	}
	if b == nil || len(b.Policies) == 0 {
		fmt.Fprintln(out, "Cached policies: none")
		return nil
	}
	if b.FetchedAt != "" {
		fmt.Fprintf(out, "Last refreshed: %s\n", b.FetchedAt)
	}
	fmt.Fprintf(out, "Cached policies: %d\n", len(b.Policies))
	for _, p := range b.Policies {
		enforcement := "warn-only"
		if p.IsLocalExact() {
			enforcement = "enforced locally"
		}
		fmt.Fprintf(out, "  - %s %s: $%.2f/%s [%s, %s]\n",
			p.ScopeType, guardScopeLabel(p), p.CapUSD(), p.Period, p.Action, enforcement)
	}
	return nil
}

// guardScopeLabel renders a policy's scope for human output.
func guardScopeLabel(p policy.Policy) string {
	switch p.ScopeType {
	case "team":
		return "(whole team)"
	case "dev":
		return "(this developer)"
	default:
		return p.ScopeValue
	}
}
