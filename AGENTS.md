# AGENTS.md - BudgetClaw

> Guidance for AI coding agents working on this repository. Human contributors should read CONTRIBUTING.md first; this file exists to give agents the same context without spelunking.

## What this is

BudgetClaw is a **local spend monitor for Claude Code**. It reads the JSONL session logs Claude Code writes under `~/.claude/projects/`, attributes each tool call's token cost to a `{project, branch}` pair, enforces budget caps by sending SIGTERM to the `claude` process on breach, and pushes phone alerts via ntfy.

Single static Go binary. MIT licensed. Part of the [RoninForge](https://roninforge.org) toolkit.

## Trust pledge (load-bearing - never break this)

The whole product hinges on three claims. **Never write code, copy, or comments that contradict them.**

1. **Zero keys.** BudgetClaw never reads, stores, or uses an Anthropic API key.
2. **Zero prompts.** BudgetClaw reads only the `usage.*` and `cwd` fields from JSONL log lines. Never the message bodies.
3. **Zero latency added.** BudgetClaw is a local log reader. It never sits between an editor and the Anthropic API.

**Never call BudgetClaw a "proxy."** That word is legally dead under Anthropic ToS §D.4. Always say "usage guard," "spend monitor," or "telemetry reader." If you find existing copy or comments using "proxy," fix them.

## Layout

    cmd/budgetclaw/        Cobra entrypoint
    internal/cli/          Subcommand handlers (init, limit, status, watch, alerts, unlock, pricing, backfill)
    internal/db/           SQLite (modernc.org/sqlite, no cgo) - events + rollups
    internal/pipeline/     JSONL parser + token-to-cost mapper
    internal/pricing/      Embedded pricing table (Opus 4.x, Sonnet, Haiku, cache multipliers)
    internal/watcher/      fsnotify-based file tailer
    internal/budget/       Limit evaluation + SIGTERM
    internal/notify/       ntfy push client
    examples/config.toml   Documented config template (TOML, not YAML)
    testdata/              Real-shape JSONL fixtures
    .github/workflows/     CI + daily pricing audit

## Build, test, lint

    make check    # fmt + vet + lint + test (run before any commit)
    make build    # → ./bin/budgetclaw
    make test     # race detector + coverage
    make snapshot # local goreleaser dry-run, no publish

CI (`.github/workflows/ci.yml`) runs `make check` on every push/PR. Don't merge red.

## Daily pricing audit

`.github/workflows/pricing-audit.yml` runs daily, hits Anthropic's `/v1/models`, cross-checks against LiteLLM's public price table, and opens a GitHub issue when a new model appears or a price drifts. **If you add or rename a model in `internal/pricing/pricing.go`, also touch the comment line `// Last updated: <date> (vX.Y.Z — <reason>)` so the audit-loop story stays honest.**

## Verify against real data before shipping

When you change anything that touches the pricing table or pipeline parsing, **run the binary against a real `~/.claude/projects/` JSONL tree** before claiming the change works. v0.1.1 silently dropped 8914 Opus 4.7 events for a week because the pricing table lagged the model release. The fix is automated (the audit workflow), but human verification stays in the loop:

    make build
    ./bin/budgetclaw pricing diagnose --dir ~/.claude/projects

`diagnose` flags any model ID found in JSONL that is not in the pricing table. Empty output = good.

## Style

- Go 1.22+. Use `errors.Is` / `errors.As`, not string matching.
- No `panic` outside `init()` and `main`.
- File-scope tests live next to the code (`pricing_test.go` next to `pricing.go`).
- No emoji in code, comments, or copy. The audience is dev-tool skeptical.
- No em dashes in any user-facing strings (AI-detection giveaway). Use hyphens or restructure.

## What you should NOT do

- Do not introduce network calls in the binary. The watcher, parser, and CLI must stay offline. The only network surface is `notify/ntfy.go` (ntfy push) and the `pricing audit` GitHub Action (CI only, not in the binary).
- Do not add cgo dependencies. modernc.org/sqlite is pure-Go on purpose; cross-compilation breaks if you bring in cgo.
- Do not `kill -9` arbitrary processes. The breach action must SIGTERM only processes whose name matches `claude`. The matching logic lives in `internal/budget/`; if you change it, write a test.
- Do not ship a release with `pricing diagnose` reporting unknown models on a representative JSONL tree.

## Releasing

Tag push triggers goreleaser:

    git tag v0.1.X
    git push origin v0.1.X

goreleaser builds the static binaries, pushes to GitHub Releases, and updates the Homebrew tap (`roninforge/homebrew-tap`).

## More context

- Site: https://roninforge.org/budgetclaw
- Markdown digest for AI fetchers: https://roninforge.org/budgetclaw.md
- Tutorial: https://roninforge.org/tutorials/how-to-set-a-hard-spend-cap-on-claude-code
- Sibling tools (same trust pledge): [Hanko](https://github.com/RoninForge/hanko), [Tsuba](https://github.com/RoninForge/tsuba)
