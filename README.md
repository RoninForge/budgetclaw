# budgetclaw

RoninForge BudgetClaw is a local spend monitor for Claude Code that tracks token cost per
project and per git branch and stops a runaway agent before a budget cap is breached. It is
MIT licensed, runs entirely on your machine, and requires no API keys.

Part of [RoninForge.org](https://roninforge.org), an independent open-source workshop that
keeps dated, reproducible records of the AI developer tooling economy.

BudgetClaw watches the JSONL session logs Claude Code already writes under `~/.claude/projects`, attributes each response's token cost to a `{project, branch}` pair, and when a cap is breached it sends SIGTERM to the Claude Code process and pushes a phone alert via ntfy.

**Zero API keys. Zero prompts. Zero latency added.** budgetclaw never touches API traffic. It reads local log files that already exist on your disk.

Docs, the ccusage comparison, the team guide and the pricing methodology: **<https://roninforge.org/budgetclaw/>**

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

| Kind   | Path                                      |
| ------ | ----------------------------------------- |
| Config | `$XDG_CONFIG_HOME/budgetclaw/config.toml` |
| State  | `$XDG_STATE_HOME/budgetclaw/state.db`     |
| Data   | `$XDG_DATA_HOME/budgetclaw/`              |
| Cache  | `$XDG_CACHE_HOME/budgetclaw/`             |

When the XDG variables are unset, defaults are `~/.config`, `~/.local/state`, `~/.local/share`, and `~/.cache`.

See [`examples/config.toml`](examples/config.toml) for a documented template.

## Phone alerts via ntfy

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

Works with ntfy.sh or any self-hosted ntfy instance. Kill actions use max priority so they bypass Do Not Disturb.

## Pricing

Rates come from the open [ai-price-index](https://roninforge.org/data/ai-price-index/) dataset (CC BY 4.0), embedded in the binary at build time, so pricing works offline by default. Each event is priced at the rate that was effective on its own date, not today's rate.

An event whose model the table does not recognise is **stored with its full token counts** rather than discarded, and prices itself once the table learns the model. `budgetclaw status` marks any total covering unpriced events with a trailing `+` and names the models involved.

```sh
# which models your logs contain, and whether each has a rate
budgetclaw pricing diagnose

# opt in to fetching the signed price table over the network (off by default)
budgetclaw pricing auto on
budgetclaw pricing refresh
```

A fetched table is only used if its Ed25519 signature verifies against a key compiled into your binary and the contents pass plausibility checks; otherwise it is discarded and the table already in force is kept.

> **Do not run `backfill --rebuild` after a price change.** It wipes the database and replays from Claude Code's session logs, which are pruned after roughly a month while the database keeps everything, so it can discard months of spend. It refuses when it would, and needs `--force` to override. Its remaining purpose is repairing a database written by a pre-dedupe binary. Nothing needs running after a price change: [why that is](https://roninforge.org/budgetclaw/#pricing-freshness).

## Sync to Goei

One command pushes your locally computed rollup to [Goei](https://roninforge.org/goei/), the hosted dashboard that dedupes spend across machines and teammates.

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

Or store the token in config so a bare `budgetclaw sync` works:

```toml
[goei]
token = "goei_dt_..."
# endpoint = "https://goei.roninforge.org/api/ingest"  # optional override for self-hosting
# machine = "my-laptop"  # optional; defaults to the OS hostname
```

Only aggregate dollar and token totals per project, branch, model and day are transmitted. No Anthropic key is involved and none leaves your machine. Re-running sync is safe: Goei deduplicates by `(day, model, project, branch)`, so re-sending a day overwrites rather than double-counts.

Flags: `--days N` (default 30), `--since YYYY-MM-DD`, `--machine LABEL`, `--no-branch` to collapse every branch into one project row, `--dry-run`.

Upgrading from a version before per-machine identity shows a one-time double-count over the re-synced window: [what to expect and why](https://roninforge.org/budgetclaw/#team).

## Scope and security

- Reads only the `usage`, `model`, `cwd` and `timestamp` fields of `~/.claude/projects/*.jsonl`. It does not read prompts or responses.
- Never sees your API key. It never talks to Anthropic's API and never sits between your editor and it.
- Only sends SIGTERM to processes named `claude`. It writes only to its own XDG directories.
- Makes no network request until you turn one on. `budgetclaw sync` and `budgetclaw pricing auto on` are both opt-in and off by default.

See [SECURITY.md](SECURITY.md) for the responsible-disclosure policy.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and PRs welcome.

## License

MIT. See [LICENSE](LICENSE).

## Docs

- [How to set a hard spend cap on Claude Code](https://roninforge.org/tutorials/how-to-set-a-hard-spend-cap-on-claude-code/)
- [How to track your Claude Code spend over time](https://roninforge.org/tutorials/how-to-track-claude-code-spend-over-time/)
- [How to minimize Claude Code costs](https://roninforge.org/tutorials/how-to-minimize-claude-code-costs/)
- [Track Claude Code spend across a team](https://roninforge.org/goei/track-claude-code-spend-across-team/)
- [Goei vs ccusage](https://roninforge.org/goei/vs-ccusage/)
- [Why a price change should not restate your recorded history](https://roninforge.org/data/ai-price-index/back-dating/)

budgetclaw is part of [RoninForge.org](https://roninforge.org).
