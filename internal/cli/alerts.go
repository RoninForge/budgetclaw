package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/RoninForge/budgetclaw/internal/budget"
	"github.com/RoninForge/budgetclaw/internal/ntfy"
)

// newAlertsCmd creates the `budgetclaw alerts` parent command
// with setup and test subcommands.
func newAlertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alerts",
		Short: "Manage phone push notifications via ntfy",
	}
	cmd.AddCommand(newAlertsSetupCmd(), newAlertsTestCmd())
	return cmd
}

func newAlertsSetupCmd() *cobra.Command {
	var (
		server  string
		topic   string
		minCost float64
	)
	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Configure the ntfy server and topic",
		Long: `Store ntfy server URL, topic, and minimum-cost threshold in
the config file. Install the ntfy app on your phone and subscribe
to the same topic to receive push notifications.

Generate a long unguessable topic with:
  openssl rand -hex 24

Example:
  budgetclaw alerts setup \
    --server https://ntfy.sh \
    --topic  budgetclaw-$(openssl rand -hex 24)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAlertsSetup(cmd.OutOrStdout(), server, topic, minCost)
		},
	}
	cmd.Flags().StringVar(&server, "server", "https://ntfy.sh", "ntfy server base URL")
	cmd.Flags().StringVar(&topic, "topic", "", "ntfy topic name (required)")
	cmd.Flags().Float64Var(&minCost, "min-cost-usd", 0, "suppress alerts below this USD threshold")
	_ = cmd.MarkFlagRequired("topic")
	return cmd
}

func runAlertsSetup(out io.Writer, server, topic string, minCost float64) error {
	if topic == "" {
		return errors.New("topic is required")
	}
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := budget.SetNtfyConfig(path, server, topic, minCost); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Fprintf(out, "configured ntfy: server=%s topic=%s min_cost_usd=$%.2f\n",
		server, topic, minCost)
	fmt.Fprintf(out, "config: %s\n", path)
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Next: subscribe to the topic in the ntfy mobile app.")
	fmt.Fprintln(out, "Test delivery with:  budgetclaw alerts test")
	return nil
}

func newAlertsTestCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "test",
		Short: "Send a test notification to verify delivery",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAlertsTest(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func runAlertsTest(ctx context.Context, out io.Writer) error {
	cfg, err := loadConfigOrDefault()
	if err != nil {
		return err
	}

	client := ntfy.New(ntfy.Options{
		Server: cfg.NtfyServer,
		Topic:  cfg.NtfyTopic,
	})
	if client.IsNoop() {
		return errors.New("ntfy not configured. Run `budgetclaw alerts setup --topic X` first")
	}

	if err := client.Test(ctx); err != nil {
		return fmt.Errorf("send test notification: %w", err)
	}

	fmt.Fprintf(out, "sent test notification to %s (topic: %s)\n",
		cfg.NtfyServer, cfg.NtfyTopic)
	fmt.Fprintln(out, "Check your phone.")
	return nil
}
