package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/goei"
)

// envToken is the environment variable checked for a Goei device token
// when --token is not passed. Lets users keep the token out of the
// config file and out of shell history.
const envToken = "GOEI_DEVICE_TOKEN" // #nosec G101 -- env var name, not a credential

// newSyncCmd creates the `budgetclaw sync` command. It reads the local
// rollups and pushes per-(project, branch, model, day) aggregates to a
// Goei dashboard's device-token ingest endpoint. No API key is
// involved: budgetclaw still only reads local logs, and only the cost
// summary is transmitted.
func newSyncCmd() *cobra.Command {
	var (
		token    string
		endpoint string
		days     int
		since    string
		noBranch bool
		dryRun   bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Push local spend to a Goei dashboard (no API key, no keys leave your machine)",
		Long: `sync sends your locally-computed Claude Code spend to a Goei
dashboard so you can see it alongside your other AI provider costs.

budgetclaw still only ever reads ~/.claude/projects/*.jsonl. sync
transmits aggregated dollar amounts and token counts per project,
branch, model, and day. No Anthropic key is needed, and no key leaves
your machine. This is the zero-key alternative to handing Goei an
admin API key.

Get a device token from your Goei settings (Settings -> Device Tokens),
then either pass --token, set ` + envToken + `, or add it to your
config file:

    [goei]
    token = "goei_dt_..."

Re-running sync is safe: Goei deduplicates by day, so the same day
re-sent overwrites rather than double-counting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd.Context(), cmd.OutOrStdout(), syncOptions{
				token:    token,
				endpoint: endpoint,
				days:     days,
				since:    since,
				noBranch: noBranch,
				dryRun:   dryRun,
			})
		},
	}

	cmd.Flags().StringVar(&token, "token", "", "Goei device token (falls back to "+envToken+" env, then config [goei].token)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Goei ingest endpoint (default "+goei.DefaultEndpoint+")")
	cmd.Flags().IntVar(&days, "days", 30, "sync spend from the last N days")
	cmd.Flags().StringVar(&since, "since", "", "explicit start date (YYYY-MM-DD); overrides --days")
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "aggregate at project level instead of per git branch")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be sent without sending it")

	return cmd
}

type syncOptions struct {
	token    string
	endpoint string
	days     int
	since    string
	noBranch bool
	dryRun   bool
}

func runSync(ctx context.Context, out io.Writer, opts syncOptions) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}

	// Resolve token: flag > env > config.
	token := opts.token
	if token == "" {
		token = os.Getenv(envToken)
	}
	if token == "" {
		token = cfg.GoeiToken
	}
	if token == "" {
		return fmt.Errorf("no Goei device token: pass --token, set %s, or add [goei].token to %s\ncreate one in Goei under Settings -> Device Tokens", envToken, mustConfigPath())
	}
	if !goei.ValidToken(token) {
		return fmt.Errorf("device token has the wrong format (expected goei_dt_ followed by 32 characters)")
	}

	// Resolve endpoint: flag > config > default.
	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = cfg.GoeiEndpoint
	}

	// Resolve the start of the sync window.
	start, err := resolveSince(opts.since, opts.days, cfg.Timezone, time.Now())
	if err != nil {
		return err
	}

	store, err := db.Open("")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = store.Close() }()

	aggs, err := store.SyncAggregates(ctx, start)
	if err != nil {
		return fmt.Errorf("read local spend: %w", err)
	}
	if len(aggs) == 0 {
		fmt.Fprintf(out, "Nothing to sync since %s. Run `budgetclaw watch` to record spend.\n", start.UTC().Format("2006-01-02"))
		return nil
	}

	gAggs := make([]goei.Aggregate, len(aggs))
	for i, a := range aggs {
		gAggs[i] = goei.Aggregate(a)
	}
	payloads := goei.BuildPayloads(gAggs, !opts.noBranch)

	var spendCount, usageCount int
	var totalUSD float64
	for _, p := range payloads {
		spendCount += len(p.Spend)
		usageCount += len(p.Usage)
		for _, s := range p.Spend {
			totalUSD += float64(s.AmountCents) / 100
		}
	}

	if opts.dryRun {
		fmt.Fprintf(out, "Dry run: would send %d spend + %d usage records (%s total) in %d request(s) to %s\n",
			spendCount, usageCount, fmtUSD(totalUSD), len(payloads), endpointOrDefault(endpoint))
		return nil
	}

	client := goei.New(endpoint, token)
	var stored int
	for i, p := range payloads {
		n, err := client.Push(ctx, p)
		if err != nil {
			return fmt.Errorf("sync request %d/%d failed: %w", i+1, len(payloads), err)
		}
		stored += n
	}

	fmt.Fprintf(out, "Synced %d spend + %d usage records (%s) to %s\n",
		spendCount, usageCount, fmtUSD(totalUSD), endpointOrDefault(endpoint))
	fmt.Fprintln(out, "View your dashboard at https://goei.roninforge.org")
	return nil
}

// resolveSince computes the start of the sync window. An explicit
// --since date (YYYY-MM-DD, interpreted at local midnight) wins;
// otherwise it is now minus days. days < 1 is treated as 1.
func resolveSince(since string, days int, loc *time.Location, now time.Time) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if since != "" {
		t, err := time.ParseInLocation("2006-01-02", since, loc)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid --since %q (want YYYY-MM-DD): %w", since, err)
		}
		return t, nil
	}
	if days < 1 {
		days = 1
	}
	return now.AddDate(0, 0, -days), nil
}

func endpointOrDefault(endpoint string) string {
	if endpoint == "" {
		return goei.DefaultEndpoint
	}
	return endpoint
}

func fmtUSD(v float64) string { return fmt.Sprintf("$%.2f", v) }

// mustConfigPath returns the config path for an error message, or a
// readable placeholder if it cannot be resolved.
func mustConfigPath() string {
	p, err := configPath()
	if err != nil {
		return "your config file"
	}
	return p
}
