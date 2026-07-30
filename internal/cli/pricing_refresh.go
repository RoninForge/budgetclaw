package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/db"
	"github.com/RoninForge/budgetclaw/internal/pricing"
	"github.com/RoninForge/budgetclaw/internal/pricing/refresh"
	"github.com/RoninForge/budgetclaw/internal/reconcile"
)

// `budgetclaw pricing auto on|off` and `budgetclaw pricing refresh`.
//
// Off by default. Fully offline operation stays the default posture and a
// supported mode, not a degraded one.

func newPricingAutoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Opt in to refreshing the price table over the network",
		Long: `Prices ship inside each budgetclaw release. When a new model is
released after your build, budgetclaw has no rate for it: your spend on
that model is still recorded in full, but it shows as unpriced until you
upgrade.

Turning this on lets budgetclaw fetch the published price table directly,
so a new model starts pricing without waiting for a release.

Off by default, and offline operation stays fully supported.`,
	}
	cmd.AddCommand(newPricingAutoOnCmd(), newPricingAutoOffCmd())
	return cmd
}

func newPricingAutoOnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "on",
		Short: "Turn price auto-update on for this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPricingAutoSet(cmd.OutOrStdout(), true)
		},
	}
}

func newPricingAutoOffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "off",
		Short: "Turn price auto-update off for this machine",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPricingAutoSet(cmd.OutOrStdout(), false)
		},
	}
}

func runPricingAutoSet(out io.Writer, enabled bool) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.SetAutoUpdatePricing(path, enabled); err != nil {
		return fmt.Errorf("update config: %w", err)
	}

	if enabled {
		// State the exact terms at the moment of consent, not in a doc
		// somewhere. This is the whole disclosure.
		fmt.Fprintln(out, "Price auto-update is ON.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "What happens now:")
		fmt.Fprintf(out, "  budgetclaw sends an HTTPS GET to %s\n", refresh.DefaultBundleURL)
		fmt.Fprintln(out, "  (and the same URL plus .minisig) about once a day, and when it")
		fmt.Fprintln(out, "  meets a model it cannot price.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "What is sent: nothing. No key, no token, no identifier, no usage")
		fmt.Fprintln(out, "  data, no query string, no cookie. The request carries a User-Agent")
		fmt.Fprintln(out, "  and an If-None-Match, and that is the complete list. The one thing")
		fmt.Fprintln(out, "  a request unavoidably reveals is your IP address, to our server.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "The file is a public CC-BY price list, signed with Ed25519 and")
		fmt.Fprintln(out, "  verified against a key compiled into this binary. Data that does")
		fmt.Fprintln(out, "  not verify is discarded and your built-in table stays in force.")
		fmt.Fprintln(out, "  Verify it yourself with the standard minisign tool: see")
		fmt.Fprintln(out, "  https://github.com/RoninForge/ai-price-index#verify-a-release")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Your API traffic is still never touched, and nothing is added to the")
		fmt.Fprintln(out, "  path of a Claude Code call. Turn this off any time with")
		fmt.Fprintln(out, "  `budgetclaw pricing auto off`.")
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Run `budgetclaw pricing refresh` to fetch now.")
	} else {
		fmt.Fprintln(out, "Price auto-update is OFF. budgetclaw makes no network requests.")
		fmt.Fprintln(out, "Prices come from this build's table; a model released after it will")
		fmt.Fprintln(out, "show as unpriced until you upgrade. Nothing is lost either way: the")
		fmt.Fprintln(out, "events are stored and price automatically once the table knows them.")
	}
	fmt.Fprintf(out, "config: %s\n", path)
	return nil
}

func newPricingRefreshCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Fetch the signed price table now",
		Long: `Fetch, verify and install the published price table.

Requires the opt-in: turn it on with ` + "`budgetclaw pricing auto on`" + `.
Use --force to fetch once without changing the saved setting.

The table is only installed if its Ed25519 signature verifies against a
key compiled into this binary AND the contents pass plausibility checks.
Otherwise the table already in force is kept, which is stale but never
wrong.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPricingRefresh(cmd.Context(), cmd.OutOrStdout(), force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "fetch once even if auto-update is off")
	return cmd
}

func runPricingRefresh(ctx context.Context, out io.Writer, force bool) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}
	if !cfg.AutoUpdatePricing && !force {
		fmt.Fprintln(out, "Price auto-update is off, so nothing was fetched.")
		fmt.Fprintln(out, "Turn it on with `budgetclaw pricing auto on`, or fetch once with")
		fmt.Fprintln(out, "`budgetclaw pricing refresh --force`.")
		return nil
	}

	res, err := refresh.Refresh(ctx, cfg.PricingURL, time.Now())
	switch {
	case errors.Is(err, refresh.ErrNotModified):
		_, tag, dataDate := pricing.ActiveTable()
		fmt.Fprintf(out, "Already current: %s (data %s).\n", tag, dataDate)
		return nil
	case errors.Is(err, refresh.ErrOffline):
		// Not a failure of the tool: the table in force is untouched.
		fmt.Fprintf(out, "Could not reach the price table: %v\n", err)
		fmt.Fprintln(out, "Keeping the current table. Nothing changed.")
		return nil
	case errors.Is(err, refresh.ErrBadSignature):
		return fmt.Errorf("refusing the downloaded price table: %w", err)
	case errors.Is(err, refresh.ErrRejected):
		return fmt.Errorf("refusing the downloaded price table: %w", err)
	case err != nil:
		return err
	}

	fmt.Fprintf(out, "Installed %s (data %s): %d models, signature verified against %s.\n",
		res.Tag, res.DataDate, res.Models, res.KeyName)

	// Anything previously stored without a price may now be priceable.
	if n, usd, rerr := repriceAfterRefresh(ctx); rerr == nil && n > 0 {
		fmt.Fprintf(out, "Repriced %d previously unpriced event(s): $%.2f recovered.\n", n, usd)
	}
	return nil
}

// repriceAfterRefresh runs the reconcile pass so events stored without a
// price get one immediately, rather than at the next command.
func repriceAfterRefresh(ctx context.Context) (int, float64, error) {
	store, err := db.Open("")
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = store.Close() }()

	// Forced: the effective table just changed under us, and the caller
	// already knows that, so do not re-derive it from the watermark.
	res, err := reconcile.Run(ctx, store, true)
	if err != nil {
		return 0, 0, err
	}
	return res.Repriced, res.Recovered, nil
}

// usePricingCache installs the last verified price table for a one-shot
// command, so a CLI read prices with the same table `budgetclaw watch` is
// using. Without this, a machine whose daemon had fetched a newer table
// would still show stale rates from `status` or `pricing rates`.
//
// No network: this is a local file read plus a signature check, which is
// sub-millisecond for a 22 KB bundle. Silent on failure by design; the
// compiled-in table is a correct fallback, and `pricing refresh` is where
// a user goes to see why a fetch is not working.
func usePricingCache(cfg *budget.Config) {
	if cfg == nil || !cfg.AutoUpdatePricing {
		return
	}
	_, _ = refresh.LoadCachedTable(time.Now())
}
