package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/goei"
	"github.com/RoninForge/budgetclaw/internal/paths"
)

// newTeamCmd creates the `budgetclaw team` command tree: the join flow a teammate runs
// to link this machine to the Goei team a repo points at, plus a link helper for a lead
// who prefers the terminal to the web panel.
func newTeamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "team",
		Short: "Join a Goei team from a repo that has a committed join code",
		Long: `A Goei team lead can commit a small .budgetclaw.toml pointer to a repo.
From inside that repo, any collaborator runs:

  budgetclaw team join

which links this machine to the team after they confirm in a browser and an
owner approves the device. No API keys change hands, and no credential ever
lives in the repo: the committed file holds only routing info and a revocable
join code that authorizes a request to join, never access to any data.`,
	}
	cmd.AddCommand(newTeamJoinCmd(), newTeamLinkCmd())
	return cmd
}

func newTeamJoinCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "join",
		Short: "Request to join the Goei team this repo points at",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTeamJoin(cmd.Context(), cmd.OutOrStdout(), name)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a label for this device on the team (defaults to the OS hostname)")
	return cmd
}

func runTeamJoin(ctx context.Context, out io.Writer, deviceName string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	pointer, err := goei.FindRepoPointer(cwd)
	if err != nil {
		return fmt.Errorf("read %s: %w", goei.PointerFileName, err)
	}
	if pointer == nil {
		return fmt.Errorf("no %s with a [goei] join code was found here.\nAsk your team lead to open Goei, run \"Link a repo\", commit the file, then run this again from inside the repo", goei.PointerFileName)
	}

	machine := resolveMachine("", "")
	if deviceName == "" {
		deviceName = machine
	}

	// The endpoint comes from a file committed to the repo, not from something the user
	// typed. Show the host it resolves to before contacting it or telling the user to
	// open a URL, and flag anything other than the default so a compromised pointer
	// cannot silently redirect the join (and the browser confirm) to a look-alike host.
	if host := goei.HostOf(pointer.Endpoint); host != goei.DefaultHost {
		fmt.Fprintf(out, "Note: this repo points at a non-default Goei host: %s\n", host)
		fmt.Fprintln(out, "Only continue if you trust this repository and expect a self-hosted Goei there.")
		fmt.Fprintln(out)
	}

	start, err := goei.StartDeviceAuth(ctx, pointer.Endpoint, pointer.JoinCode, machine, deviceName)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Joining team %q.\n\n", start.Team)
	fmt.Fprintf(out, "  1. Open:   %s\n", start.VerificationURIComplete)
	fmt.Fprintf(out, "  2. Confirm this code: %s\n\n", start.UserCode)
	fmt.Fprintln(out, "Waiting for you to confirm in the browser and an owner to approve this device...")

	interval := time.Duration(start.Interval) * time.Second
	if interval < 2*time.Second {
		interval = 5 * time.Second
	}
	// The client polls for roughly the initial (pre-confirm) window. The server extends
	// the request's own lifetime once the teammate confirms, so a lead who approves after
	// this CLI gives up does not lose the queued request; the teammate just re-runs
	// `budgetclaw team join` to collect. We deliberately bound the blocking wait here
	// rather than polling for the full post-confirm window.
	deadline := time.Now().Add(time.Duration(start.ExpiresIn+30) * time.Second)

	lastStatus := ""
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for approval; run `budgetclaw team join` again to retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		poll, err := goei.PollDeviceAuth(ctx, pointer.Endpoint, start.DeviceCode)
		if err != nil {
			// A transient network error should not end the flow; keep polling until
			// the deadline, surfacing the reason once so it is diagnosable.
			if lastStatus != "error" {
				fmt.Fprintf(out, "  (still trying: %v)\n", err)
			}
			lastStatus = "error"
			continue
		}

		switch poll.Status {
		case "requested":
			if lastStatus != "requested" {
				fmt.Fprintln(out, "Confirmed. Waiting for a team owner to approve this device...")
			}
		case "denied":
			return fmt.Errorf("the request to join %q was declined", start.Team)
		case "expired":
			return fmt.Errorf("the code expired before approval; run `budgetclaw team join` again")
		case "completed":
			return fmt.Errorf("this request was already completed; run `budgetclaw team join` again for a fresh code")
		case "approved":
			if poll.Token == "" {
				return fmt.Errorf("approved, but no token was returned; run `budgetclaw team join` again")
			}
			return finishTeamJoin(ctx, out, pointer, poll.Token, start.Team)
		}
		lastStatus = poll.Status
	}
}

// finishTeamJoin persists the minted device token (and the ingest endpoint when the
// team is self-hosted), then runs a first sync with a 30-day backfill so the teammate's
// dashboard and the team rollup populate in the same session.
func finishTeamJoin(ctx context.Context, out io.Writer, pointer *goei.RepoPointer, token, team string) error {
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.SetGoeiConfig(p, token, "", ""); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	// Set the ingest endpoint EXPLICITLY (empty = the built-in default). A plain
	// SetGoeiConfig treats an empty endpoint as "leave unchanged", which would strand a
	// stale self-hosted endpoint on disk when a user joins a default-hosted team after a
	// self-hosted one, then silently misdirect the very next sync. Writing it
	// unconditionally keeps the saved endpoint matched to the team just joined.
	saveEndpoint := ""
	if ingest := goei.IngestEndpointFor(pointer.Endpoint); ingest != goei.DefaultEndpoint {
		saveEndpoint = ingest
	}
	if err := budget.SetGoeiEndpoint(p, saveEndpoint); err != nil {
		return fmt.Errorf("save endpoint: %w", err)
	}
	fmt.Fprintf(out, "\nApproved. This machine is now on team %q. Token saved to %s\n", team, p)
	fmt.Fprintln(out, "Running a first sync (last 30 days)...")
	if err := runSync(ctx, out, syncOptions{days: 30}); err != nil {
		fmt.Fprintf(out, "First sync did not complete: %v\n", err)
		fmt.Fprintln(out, "That is fine: run `budgetclaw watch` to record spend, then `budgetclaw sync`.")
	}
	return nil
}

func newTeamLinkCmd() *cobra.Command {
	var (
		joinCode string
		team     string
		endpoint string
		write    bool
	)
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Write a committed .budgetclaw.toml pointer for a repo (lead action)",
		Long: `Generate the committed pointer that lets collaborators run
"budgetclaw team join" from this repo.

The join code is minted in Goei: open your team, run "Link a repo", and copy
the join code. Then either commit the file Goei shows you, or run:

  budgetclaw team link --join-code goei_jc_... --team "your-team" --write

Without --write the pointer is printed for review. The file is safe to commit:
it carries only routing info and a revocable join code, never a device token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if joinCode == "" {
				return fmt.Errorf("--join-code is required (copy it from Goei -> your team -> Link a repo)")
			}
			base := endpoint
			if base == "" {
				base = goei.DefaultBaseURL
			}
			content := renderPointer(team, base, joinCode)
			if !write {
				fmt.Fprint(cmd.OutOrStdout(), content)
				fmt.Fprintf(cmd.OutOrStdout(), "\nReview, then commit this as %s in your repo root (or re-run with --write).\n", goei.PointerFileName)
				return nil
			}
			if _, err := os.Stat(goei.PointerFileName); err == nil {
				return fmt.Errorf("%s already exists here; edit it by hand or remove it first", goei.PointerFileName)
			}
			// 0o600 in the working tree; git commits it as a normal 100644 file, so the
			// committed pointer is readable by collaborators as intended.
			if err := os.WriteFile(goei.PointerFileName, []byte(content), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", goei.PointerFileName, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s. Commit it, and teammates can run `budgetclaw team join`.\n", goei.PointerFileName)
			return nil
		},
	}
	cmd.Flags().StringVar(&joinCode, "join-code", "", "the join code from Goei (Link a repo)")
	cmd.Flags().StringVar(&team, "team", "", "the team name to show teammates")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Goei endpoint (default "+goei.DefaultBaseURL+")")
	cmd.Flags().BoolVar(&write, "write", false, "write the pointer to ./"+goei.PointerFileName+" instead of printing it")
	return cmd
}

func renderPointer(team, endpoint, joinCode string) string {
	if team == "" {
		team = "your-team"
	}
	return fmt.Sprintf(`# %s  -  commit this to your repo root (safe to commit, no secrets)
[goei]
team = %q
endpoint = %q
join_code = %q
mode = "request"
`, goei.PointerFileName, team, endpoint, joinCode)
}

// --- discovery line ---
//
// When `budgetclaw status` runs inside a repo that has a team pointer but this machine
// has no Goei token yet, a single line tells the user a team rollup exists and how to
// join. It is a disclosure, not an ad: it is generated purely by reading the committed
// file (no network call for a non-member), it renders at most once per repo per 14
// days, and `[goei] prompts = false` in the pointer silences it forever.

const discoveryInterval = 14 * 24 * time.Hour

func maybeShowDiscovery(out io.Writer) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	pointer, err := goei.FindRepoPointer(cwd)
	if err != nil || pointer == nil || !pointer.PromptsEnabled() {
		return
	}
	// Already joined (a token is configured)? Then say nothing. Fail closed: if the
	// config cannot be read, suppress rather than risk nagging a user who may well have a
	// token in an unreadable/corrupt config.
	cfg, err := loadConfigOrDefault()
	if err != nil || cfg.GoeiToken != "" {
		return
	}
	if !discoveryDue(pointer.Path) {
		return
	}
	team := pointer.Team
	if team == "" {
		team = "this repo's team"
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "This repo shares a team spend rollup on Goei (team: %s).\n", team)
	fmt.Fprintln(out, "Your data stays on this machine until you join:  budgetclaw team join")
	markDiscoveryShown(pointer.Path)
}

func discoveryStatePath() (string, error) {
	dir, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "discovery.json"), nil
}

func loadDiscoveryState() map[string]int64 {
	p, err := discoveryStatePath()
	if err != nil {
		return map[string]int64{}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return map[string]int64{}
	}
	m := map[string]int64{}
	_ = json.Unmarshal(data, &m)
	return m
}

func discoveryDue(key string) bool {
	m := loadDiscoveryState()
	last, ok := m[key]
	if !ok {
		return true
	}
	return time.Since(time.Unix(last, 0)) >= discoveryInterval
}

func markDiscoveryShown(key string) {
	m := loadDiscoveryState()
	m[key] = time.Now().Unix()
	p, err := discoveryStatePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return
	}
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}
