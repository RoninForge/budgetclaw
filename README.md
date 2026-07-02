# budgetclaw

**Local spend monitor for Claude Code.** Watches the JSONL session logs Claude Code writes under `~/.claude/projects`, attributes each tool-call's token cost to a project and git branch, and enforces budget caps by sending SIGTERM to the client process on breach. Pushes phone alerts via ntfy.

**Zero key. Zero prompts. Zero latency added.** budgetclaw never touches API traffic. It parses what Claude Code already writes to disk locally.

```sh
curl -fsSL https://roninforge.org/get | sh
```

## What it does

- Per-project and per-git-branch cost tracking from Claude Code session logs
- Hard budget caps with SIGTERM enforcement on breach
- Phone push alerts via self-hosted or public [ntfy.sh](https://ntfy.sh)
- **Daily audit against Anthropic's models API** -- new models are detected within 24 hours and surfaced via a GitHub issue, so the pricing table cannot silently lag a release
- Works offline, no account, no telemetry leaves your machine
- Single static Go binary, no runtime, no Python, no Node

## Why it exists

After April 2026, solo Claude Code users on raw API billing have no first-party way to cap spend per project or per branch. One stuck agent loop on a feature branch can burn $500 before you notice. Native `/cost` tells you the bill after the fact. budgetclaw tells you *before* and enforces the limit.

## How it works

1. A background watcher tails `~/.claude/projects/*/*.jsonl` using inotify / FSEvents.
2. Each log entry has `usage.*` token counts, `model`, `cwd`, and `timestamp`. budgetclaw reads the cwd, walks up to find `.git/HEAD`, and attributes the event to `{project, branch}`.
3. Token counts are priced against a static table (Opus, Sonnet, Haiku, cache-read, cache-creation) and written to a local SQLite rollup.
4. On each new event, the budget evaluator checks the active limit rules. If a cap is breached, budgetclaw SIGTERMs the matching `claude` process and writes a lockfile to prevent silent relaunch.
5. A phone alert fires via ntfy with the breach context.

No data leaves your machine unless you explicitly opt into a future hosted tier.

## What it does NOT do

- **Does not read your prompts or responses.** It only reads the `usage` and `cwd` fields of each JSONL line.
- **Does not see your API key.** It never talks to Anthropic's API.
- **Does not proxy, intercept, or modify requests.** It is a local log reader.
- **Does not kill arbitrary processes.** It only SIGTERMs processes whose name matches `claude`.

## Install

### One-liner

```sh
curl -fsSL https://roninforge.org/get | sh
```

### Via Homebrew (macOS, Linux)

```sh
brew install roninforge/tap/budgetclaw
```

### From source

```sh
git clone https://github.com/RoninForge/budgetclaw.git
cd budgetclaw
make build
./bin/budgetclaw version
```

### Via `go install`

```sh
go install github.com/RoninForge/budgetclaw/cmd/budgetclaw@latest
```

## Quick start

```sh
# first-run: creates config + state dirs, prints paths
budgetclaw init

# cap the "myapp" project at $5/day across all branches, kill on breach
budgetclaw limit set --project myapp --period daily --cap 5.00 --action kill

# cap the "feature/expensive" branch specifically at $1/day, warn only
budgetclaw limit set --project myapp --branch "feature/expensive" --period daily --cap 1.00 --action warn

# show today's spend by project and branch
budgetclaw status

# run the watcher in the foreground
budgetclaw watch
```

## Configuration

budgetclaw follows the [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html):

| Kind   | Path                                     |
| ------ | ---------------------------------------- |
| Config | `$XDG_CONFIG_HOME/budgetclaw/config.toml` |
| State  | `$XDG_STATE_HOME/budgetclaw/state.db`     |
| Data   | `$XDG_DATA_HOME/budgetclaw/`              |
| Cache  | `$XDG_CACHE_HOME/budgetclaw/`             |

When the XDG variables are unset, defaults are `~/.config`, `~/.local/state`, `~/.local/share`, and `~/.cache`.

See [`examples/config.toml`](examples/config.toml) for a documented template.

## Phone alerts via ntfy

budgetclaw pushes breach notifications to your phone via [ntfy.sh](https://ntfy.sh) (or any self-hosted ntfy instance). When a budget cap is breached, you get an instant push notification with the project, branch, and spend amount.

Setup takes 60 seconds:

```sh
# 1. Install the ntfy app on your phone (iOS or Android)
#    https://ntfy.sh/docs/subscribe/phone/

# 2. Generate a secret topic name
TOPIC="budgetclaw-$(openssl rand -hex 12)"
echo "Your topic: $TOPIC"

# 3. Subscribe to that topic in the ntfy app

# 4. Configure budgetclaw
budgetclaw alerts setup --server https://ntfy.sh --topic "$TOPIC"

# 5. Test delivery
budgetclaw alerts test
```

You should see a "budgetclaw test" notification on your phone. From now on, warn and kill breaches will push automatically. Kill actions use max priority to bypass Do Not Disturb.

## Sync to a Goei dashboard (optional)

[Goei](https://goei.roninforge.org) is a web dashboard that unifies AI provider costs into one view. `budgetclaw sync` pushes your locally-computed Claude Code spend to your Goei dashboard so it sits alongside your other AI costs, with the per-project and per-branch attribution budgetclaw already tracks.

This is the zero-key path: budgetclaw still only ever reads `~/.claude/projects/*.jsonl`. Sync transmits aggregated dollar amounts and token counts per project, branch, model, and day. No Anthropic key is involved, and no key leaves your machine. You never have to hand Goei an admin API key.

```sh
# 1. In Goei, go to Settings -> Device Tokens and create a token (starts with goei_dt_)

# 2. Sync the last 30 days
budgetclaw sync --token goei_dt_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx

# Or keep the token out of your shell history:
export GOEI_DEVICE_TOKEN=goei_dt_...
budgetclaw sync --days 7

# Preview what would be sent without sending it:
budgetclaw sync --dry-run
```

Or store the token in your config file so a bare `budgetclaw sync` works:

```toml
[goei]
token = "goei_dt_..."
# endpoint = "https://goei.roninforge.org/api/ingest"  # optional override for self-hosting
# machine = "my-laptop"  # optional; defaults to the OS hostname
```

Each spend record carries the branch as its own field, so Goei keeps the per-branch breakdown without mangling the project name. Re-running sync is safe. Goei deduplicates by (day, model, project, branch), so the same day re-sent overwrites rather than double-counting. Useful flags: `--days N` (default 30), `--since YYYY-MM-DD`, `--no-branch` to omit the branch field so Goei collapses every branch of a project into one project-level row, and `--dry-run`.

Sync also stamps each record with a machine identity so spend from two machines stays separate on the dashboard instead of merging. By default this is your OS hostname (not a secret, so sync stays zero-key and zero-prompt). Override it with `--machine`, the `GOEI_MACHINE` env var, or `[goei].machine` in config if you would rather send a custom label.

**Upgrading from a pre-machine version:** if you synced with an older budgetclaw, your first sync after upgrading will show a one-time double-count over the re-synced window (default 30 days). The machine identity is new, so the Goei server now keeps per-machine rows separate and no longer deletes the untagged rows it stored before (that would lose data once you sync from more than one machine). The old untagged rows and the new machine-tagged rows briefly coexist and add up. This is a one-time step, not an ongoing error: it does not grow with each sync, it self-limits as older days age out of the window, and it clears once every machine on your account has upgraded and re-synced, after which the stale untagged rows can be removed on the Goei side. It may trip a single spurious budget alert during the transition. New installs are unaffected.

## Pricing freshness

Anthropic ships new models often, and a missing model in the pricing table means events get silently skipped from the rollups (no cost recorded, no cap fired). budgetclaw guards against that with a daily GitHub Action ([`.github/workflows/pricing-audit.yml`](.github/workflows/pricing-audit.yml)) that:

1. Calls Anthropic's `/v1/models` metadata endpoint (free, no inference billed).
2. Diffs the model list against the embedded pricing table.
3. Opens a GitHub issue if a new model is detected.

A maintainer then verifies pricing on the [Anthropic pricing page](https://docs.anthropic.com/en/docs/about-claude/pricing) and ships a patch release. **Detection is automated; pricing is verified by hand** -- a wrong auto-merged price would compromise the kill action, so the verification step stays human.

If you ever notice `budgetclaw status` reporting suspiciously low spend, run `budgetclaw pricing diagnose` to see which models the local logs contain and whether any are missing from the table. The same fix flow applies: open an issue, we add the entry, you upgrade and run `budgetclaw backfill` to recover historical attribution.

The model audit catches new model IDs but not changes to existing rates -- Anthropic's `/v1/models` returns metadata only, no pricing. As a second line of defense, the maintainer cross-checks rates against the [Anthropic pricing page](https://docs.anthropic.com/en/docs/about-claude/pricing) and the community-maintained [`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm/blob/main/litellm/model_prices_and_context_window_backup.json) periodically. v0.1.4 corrected the Opus 4.5 / 4.6 / 4.7 rates from the pre-cut $15 / $75 to the current $5 / $25 after a cross-check turned up the gap. If a price correction lands, run `budgetclaw backfill --rebuild` to recompute historical rollups; without `--rebuild`, idempotent inserts leave the old rate baked into existing rows.

## Security

budgetclaw only reads from `$HOME/.claude/projects/` and only SIGTERMs processes named `claude`. It writes only to its own XDG directories. See [SECURITY.md](SECURITY.md) for the responsible-disclosure policy.

## Roadmap

- v0.1: local JSONL parser, per-project/branch rollups, budget caps, SIGTERM enforcer, ntfy alerts, Claude Code plugin manifest, zero-key sync to Goei
- v0.2: per-branch forecasting, multiple budget periods, shell completion
- v0.3: launchd/systemd daemon integration, Homebrew tap
- Later: optional hosted sync tier, Cursor per-branch attribution (on top of Cursor's native caps)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and PRs welcome.

## License

MIT. See [LICENSE](LICENSE).

## About

budgetclaw is part of [RoninForge](https://roninforge.org), a small venture building honest tools for the army of one. Source: [github.com/RoninForge/budgetclaw](https://github.com/RoninForge/budgetclaw).


## More from RoninForge

Free, local-first tools for developers working with AI coding assistants. No accounts, MIT licensed.

- [Hanko](https://roninforge.org/hanko) - validate Claude Code plugin manifests
- [Tsuba](https://roninforge.org/tsuba) - scaffold Claude Code plugins and skills
- [Goei](https://roninforge.org/goei) - unified AI provider cost dashboard

Free web tools (run in your browser, nothing uploaded):

- [GitHub Copilot AI Credits calculator](https://roninforge.org/copilot-credits-calculator) - estimate your monthly credit burn under usage-based billing
- [Copilot usage CSV analyzer](https://roninforge.org/copilot-csv-analyzer) - break your usage report down by model, day, and SKU
- [LLM API pricing comparison](https://roninforge.org/llm-pricing) - Claude, GPT, Gemini, DeepSeek, Mistral, and Grok token prices side by side
