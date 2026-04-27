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

## Security

budgetclaw only reads from `$HOME/.claude/projects/` and only SIGTERMs processes named `claude`. It writes only to its own XDG directories. See [SECURITY.md](SECURITY.md) for the responsible-disclosure policy.

## Roadmap

- v0.1: local JSONL parser, per-project/branch rollups, budget caps, SIGTERM enforcer, ntfy alerts, Claude Code plugin manifest
- v0.2: per-branch forecasting, multiple budget periods, shell completion
- v0.3: launchd/systemd daemon integration, Homebrew tap
- Later: optional hosted sync tier, Cursor per-branch attribution (on top of Cursor's native caps)

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Bug reports and PRs welcome.

## License

MIT. See [LICENSE](LICENSE).

## About

budgetclaw is part of [RoninForge](https://roninforge.org), a small venture building honest tools for the army of one. Source: [github.com/RoninForge/budgetclaw](https://github.com/RoninForge/budgetclaw).
