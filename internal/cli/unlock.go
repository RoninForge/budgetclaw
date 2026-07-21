package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/enforcer"
	"github.com/RoninForge/budgetclaw/internal/goei"
)

// newUnlockCmd creates the `budgetclaw unlock` command for
// manually releasing a budget-breach lock before its auto-expiry.
// Useful after increasing a cap or when the user intentionally
// wants to resume work on a locked project.
func newUnlockCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "unlock <project>",
		Short: "Release a budget-breach lock",
		Long: `Remove the active lock for (project, branch) so that the
next Claude Code run in that project is not killed on startup.

Branch defaults to "main". Use --branch for feature branches.

Locks also auto-expire when their budget period rolls over, so
manual unlock is only needed when you want to resume work early
or when you have increased the cap.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUnlock(cmd.OutOrStdout(), args[0], branch)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "main", "branch to unlock")
	return cmd
}

func runUnlock(out io.Writer, project, branch string) error {
	ls, err := enforcer.NewLockStore()
	if err != nil {
		return fmt.Errorf("open lock store: %w", err)
	}
	lk, err := ls.IsLocked(project, branch)
	if err != nil {
		return fmt.Errorf("check lock: %w", err)
	}
	if lk == nil {
		fmt.Fprintf(out, "%s/%s is not locked.\n", project, branch)
		return nil
	}
	if err := ls.Release(project, branch); err != nil {
		return fmt.Errorf("release lock: %w", err)
	}
	fmt.Fprintf(out, "unlocked %s/%s (was: %s)\n", project, branch, lk.Reason)

	// A lock from a remote Guard Mode policy carries its policy id. Queue an
	// override audit event so the team owner sees the cap was lifted; it ships
	// on the next sync. Best-effort: a failure never blocks the unlock.
	if lk.PolicyID != "" {
		queueOverrideEvent(out, lk, project)
	}
	return nil
}

// queueOverrideEvent records a content-free "override" audit event for a
// remote-policy lock the user just released.
func queueOverrideEvent(out io.Writer, lk *enforcer.Lock, project string) {
	store, err := db.Open("")
	if err != nil {
		fmt.Fprintf(out, "note: could not record override for your team: %v\n", err)
		return
	}
	defer func() { _ = store.Close() }()

	machine, _ := os.Hostname()
	nowIso := time.Now().UTC().Format(time.RFC3339)
	ev := goei.GuardEvent{
		PolicyID:    lk.PolicyID,
		Action:      "override",
		ScopeType:   "project",
		ScopeValue:  project,
		Machine:     machine,
		AmountCents: int(lk.CurrentUSD*100 + 0.5),
		CapCents:    int(lk.CapUSD*100 + 0.5),
		At:          nowIso,
		DedupKey:    lk.PolicyID + ":override:" + nowIso,
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return
	}
	if _, err := store.QueueGuardEvent(context.Background(), ev.DedupKey, string(raw)); err != nil {
		fmt.Fprintf(out, "note: could not record override for your team: %v\n", err)
	}
}
