package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/goei"
	"github.com/RoninForge/budgetclaw/internal/policy"
)

// envToken is the environment variable checked for a Goei device token
// when --token is not passed. Lets users keep the token out of the
// config file and out of shell history.
const envToken = "GOEI_DEVICE_TOKEN" // #nosec G101 -- env var name, not a credential

// envMachine is the environment variable checked for the per-machine
// identity stamped on synced spend records when --machine is not
// passed. Lets a machine set its identity without editing the config.
const envMachine = "GOEI_MACHINE"

// newSyncCmd creates the `budgetclaw sync` command. It reads the local
// rollups and pushes per-(project, branch, model, day) aggregates to a
// Goei dashboard's device-token ingest endpoint. No API key is
// involved: budgetclaw still only reads local logs, and only the cost
// summary is transmitted.
func newSyncCmd() *cobra.Command {
	var (
		token       string
		endpoint    string
		machine     string
		days        int
		since       string
		noBranch    bool
		dryRun      bool
		save        bool
		showPayload bool
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

Each record is stamped with a machine identity so spend from two
machines is kept separate on the dashboard instead of merged. By
default this is your OS hostname (not a secret, so sync stays zero-key
and zero-prompt). Override it with --machine, ` + envMachine + `, or
[goei].machine in config if you would rather send a custom label.

Re-running sync is safe: Goei deduplicates by day, so the same day
re-sent overwrites rather than double-counting.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSync(cmd.Context(), cmd.OutOrStdout(), syncOptions{
				token:       token,
				endpoint:    endpoint,
				machine:     machine,
				days:        days,
				since:       since,
				noBranch:    noBranch,
				dryRun:      dryRun,
				save:        save,
				showPayload: showPayload,
			})
		},
	}

	cmd.Flags().StringVar(&token, "token", "", "Goei device token (falls back to "+envToken+" env, then config [goei].token)")
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Goei ingest endpoint (default "+goei.DefaultEndpoint+")")
	cmd.Flags().StringVar(&machine, "machine", "", "machine identity stamped on each record (falls back to "+envMachine+" env, then config [goei].machine, then the OS hostname)")
	cmd.Flags().IntVar(&days, "days", 30, "sync spend from the last N days")
	cmd.Flags().StringVar(&since, "since", "", "explicit start date (YYYY-MM-DD); overrides --days")
	cmd.Flags().BoolVar(&noBranch, "no-branch", false, "aggregate at project level instead of per git branch")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print a summary of what would be sent without sending it")
	cmd.Flags().BoolVar(&showPayload, "show-payload", false, "print the exact JSON request body that would be sent, then exit (sends nothing, needs no token)")
	cmd.Flags().BoolVar(&save, "save", false, "persist the resolved token to the config file so later syncs need no --token")

	return cmd
}

type syncOptions struct {
	token       string
	endpoint    string
	machine     string
	days        int
	since       string
	noBranch    bool
	dryRun      bool
	save        bool
	showPayload bool
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
	// --show-payload sends nothing, so it needs no token: a skeptical user can
	// inspect exactly what would be transmitted before ever creating one.
	if token == "" && !opts.showPayload {
		return fmt.Errorf("no Goei device token: pass --token, set %s, or add [goei].token to %s\ncreate one in Goei under Settings -> Device Tokens", envToken, mustConfigPath())
	}
	if token != "" && !goei.ValidToken(token) {
		return fmt.Errorf("device token has the wrong format (expected goei_dt_ followed by 32 characters)")
	}

	// --save persists the resolved token (and any explicit endpoint/machine) so
	// later syncs run with no flags. Runs before the send so the token is stored
	// even if the network push later fails.
	if opts.save {
		if token == "" {
			return fmt.Errorf("--save needs a token: pass --token or set %s", envToken)
		}
		p, err := configPath()
		if err != nil {
			return err
		}
		if err := budget.SetGoeiConfig(p, token, opts.endpoint, opts.machine); err != nil {
			return fmt.Errorf("save token to config: %w", err)
		}
		fmt.Fprintf(out, "Saved Goei token to %s\n", p)
	}

	// Resolve endpoint: flag > config > default.
	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = cfg.GoeiEndpoint
	}

	// Resolve the per-machine identity stamped on every record.
	machine := resolveMachine(opts.machine, cfg.GoeiMachine)

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
	payloads := goei.BuildPayloads(gAggs, !opts.noBranch, machine)

	// --show-payload prints the exact request body (sends nothing), so a user can
	// audit every byte that would leave the machine against the ingest contract.
	if opts.showPayload {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		for i, p := range payloads {
			fmt.Fprintf(out, "# request %d of %d -> POST %s\n", i+1, len(payloads), endpointOrDefault(endpoint))
			if err := enc.Encode(p); err != nil {
				return fmt.Errorf("encode payload: %w", err)
			}
		}
		return nil
	}

	var spendCount, usageCount, totalTokens int
	var totalUSD float64
	for _, p := range payloads {
		spendCount += len(p.Spend)
		usageCount += len(p.Usage)
		for _, s := range p.Spend {
			totalUSD += float64(s.AmountCents) / 100
			if s.Tokens != nil {
				totalTokens += s.Tokens.Input + s.Tokens.Output +
					s.Tokens.CacheRead + s.Tokens.CacheWrite5m + s.Tokens.CacheWrite1h
			}
		}
	}

	if opts.dryRun {
		fmt.Fprintf(out, "Dry run: would send %d spend + %d usage records (%s total, %d tokens)%s in %d request(s) to %s\n",
			spendCount, usageCount, fmtUSD(totalUSD), totalTokens, machineNote(machine), len(payloads), endpointOrDefault(endpoint))
		return nil
	}

	client := goei.New(endpoint, token)

	// Flush any queued Guard Mode audit events with the first request. The
	// ingest endpoint requires a non-empty spend array, so events ride along
	// with real spend rather than in a request of their own.
	var flushedIDs []int64
	if len(payloads) > 0 {
		pending, perr := store.PendingGuardEvents(ctx, 200)
		if perr != nil {
			fmt.Fprintf(out, "warning: could not read queued guard events: %v\n", perr)
		} else if len(pending) > 0 {
			evs := make([]goei.GuardEvent, 0, len(pending))
			for _, pe := range pending {
				var ev goei.GuardEvent
				if json.Unmarshal([]byte(pe.JSON), &ev) != nil {
					continue
				}
				evs = append(evs, ev)
				flushedIDs = append(flushedIDs, pe.ID)
			}
			payloads[0].GuardEvents = evs
		}
	}

	var stored int
	var lastPolicy *goei.PolicyResponse
	for i, p := range payloads {
		n, pr, err := client.Push(ctx, p)
		if err != nil {
			return fmt.Errorf("sync request %d/%d failed: %w", i+1, len(payloads), err)
		}
		stored += n
		if pr != nil {
			lastPolicy = pr
		}
		// The first request carried the queued audit events; once it is
		// accepted they are recorded server-side (dedup-safe), so clear them.
		if i == 0 && len(flushedIDs) > 0 {
			if derr := store.DeleteGuardEvents(ctx, flushedIDs); derr != nil {
				fmt.Fprintf(out, "warning: could not clear queued guard events: %v\n", derr)
			}
		}
	}

	// Cache the piggybacked policy set (opt-in only) so `limit list` and the
	// next `watch` reflect it. Preserve the existing ETag: it belongs to the
	// GET poll, and the piggyback response carries none.
	if cfg.AcceptRemotePolicies && lastPolicy != nil {
		etag := ""
		if existing, err := policy.Load(); err == nil && existing != nil {
			etag = existing.ETag
		}
		bundle := policy.BundleFromResponse(lastPolicy, etag, time.Now().UTC().Format(time.RFC3339))
		if err := policy.Save(bundle); err != nil {
			fmt.Fprintf(out, "warning: could not cache policies: %v\n", err)
		}
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

// resolveMachine picks the per-machine identity stamped on every synced
// spend record so the Goei server keeps two machines' rollups from
// colliding. Precedence mirrors token resolution: the --machine flag,
// then the GOEI_MACHINE env var, then [goei].machine in config, then
// the OS hostname as a stable default. The hostname is not a secret, so
// this keeps sync zero-key and zero-prompt; the overrides exist for
// anyone who considers their hostname sensitive. An empty result is
// fine: the server treats "" as legacy/unknown.
func resolveMachine(flag, config string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv(envMachine); env != "" {
		return env
	}
	if config != "" {
		return config
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return ""
}

// machineNote renders the machine identity for sync output, or an empty
// string when no machine is set (the server treats that as legacy).
func machineNote(machine string) string {
	if machine == "" {
		return ""
	}
	return fmt.Sprintf(" as machine %q", machine)
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
