# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **Opus 4.5 / 4.6 / 4.7 input + output rates corrected from $15 / $75 to $5 / $25 per MTok.** Anthropic moved Opus 4.5+ to a new lower tier; the original maintainer's snapshot from 2026-04-09 captured the pre-cut prices, and prior releases inherited the wrong rate by tier-pattern. Discovered by cross-checking against BerriAI/litellm's `model_prices_and_context_window.json`, then confirmed against the maintainer's screenshot of the Anthropic pricing page. Existing rollups are not auto-corrected because `Insert` is idempotent on uuid; run `budgetclaw backfill --rebuild` to wipe rollups and recompute every historical event at the corrected rates. Opus 4.1 stays at $15 / $75 (still on the legacy tier per Anthropic's published pricing).

### Added

- `budgetclaw backfill --rebuild` flag truncates events and rollups before scanning so a pricing correction is reflected in historical totals. Without `--rebuild`, idempotent inserts leave the old rate baked into existing rollup rows.
- `budgetclaw pricing rates [--json]` subcommand prints every model with its input + output rate per MTok. Used by the new cross-check workflow.
- The daily `pricing-audit` workflow now also cross-checks every embedded rate against BerriAI/litellm's `model_prices_and_context_window.json`. Closes the gap that v0.1.4 surfaced: `/v1/models` returns IDs only, so price changes on existing models would slip past the model-only audit. The two checks run as one workflow because new-model launches typically coincide with price cuts on older models in the same tier (Opus 4.7 + simultaneous 4.5/4.6 price cut), so bundling them keeps maintainer triage to one issue, one PR, one release. The standalone `pricing-cross-check.yml` from earlier in this release was rolled into `pricing-audit.yml`.

### Changed

- Pricing table comment now names the cross-check methodology (Anthropic page + LiteLLM JSON) and the date, so the next maintainer knows where to verify.

## [v0.1.3] - 2026-04-28

### Added

- `budgetclaw backfill` subcommand walks `$HOME/.claude/projects/**/*.jsonl`, prices every assistant event, and inserts rollups into the local state DB. Safe to run repeatedly thanks to the existing `ON CONFLICT(uuid) DO NOTHING` constraint. Use after upgrading to a release that adds new model pricing to recover attribution for events the prior watcher dropped.
- Pricing table now recognizes two more dated Opus variants the daily audit detected on Anthropic's `/v1/models`: `claude-opus-4-5-20251101` and `claude-opus-4-1-20250805`. (Initial rates were Opus tier at the time; v0.1.4 corrects 4-5-20251101 to $5/$25.)

### Changed

- Issue body opened by `pricing-audit` workflow now correctly says "daily" rather than "weekly" (the cron schedule was changed to daily on the same day as v0.1.2; the body template was overlooked).

### Fixed

- Pricing table now recognizes `claude-opus-4-7`. Previously the watcher logged `pricing: unknown model, skipping event` for every Opus 4.7 tool-call, causing silent under-attribution: spend was not counted, rollups stayed at $0, and `kill`-action caps could never fire on Opus 4.7 sessions. Rates set to the established Opus tier ($15 input / $75 output per MTok), matching 4-5 and 4-6.
- Watcher no longer floods stderr with one `pricing: unknown model` line per skipped event. Each unknown model now produces a single loud WARN per watcher run (with a hint pointing at `budgetclaw pricing diagnose`); subsequent events for the same model log at DEBUG level only.

### Added

- `budgetclaw pricing list` subcommand prints every model ID in the embedded pricing table (`--json` for scripting).
- `budgetclaw pricing diagnose` subcommand walks `$HOME/.claude/projects/**/*.jsonl`, counts every model seen, and flags any missing from the pricing table. Returns a non-zero exit code when missing models are detected so it composes cleanly into shell pipelines.
- Daily `pricing-audit` GitHub Action calls Anthropic's `/v1/models` endpoint, diffs the result against the embedded pricing table, and opens an issue when a new model is detected. Detection is automated; pricing remains a manual step because a wrong auto-merged price would compromise the kill action.
- Initial project scaffold: cobra CLI, `version` subcommand, XDG path helpers, build/test/lint/release pipeline
- MIT license
- Issue templates, PR template, dependabot, CI + release + CodeQL workflows
- `.goreleaser.yaml` with multi-arch binary builds for darwin-amd64, darwin-arm64, linux-amd64, linux-arm64
- XDG-first filesystem layout with tests
- README with trust-pitch framing and honest feature list
- JSONL parser for Claude Code session logs (`internal/parser`)
- Static pricing table with Anthropic-published rates and cache multipliers (`internal/pricing`)
- SQLite-backed event and rollup persistence with WAL mode (`internal/db`)
- TOML budget config: per-project/branch rules, glob matching, daily/weekly/monthly periods (`internal/budget`)
- Period-aware budget evaluator with timezone support (`internal/budget`)
- SIGTERM-based process killer with gopsutil process enumeration (`internal/enforcer`)
- Filesystem-backed lockfile store for breach persistence across restarts (`internal/enforcer`)
- ntfy.sh push notification client with exponential backoff and retry (`internal/ntfy`)
- fsnotify-based JSONL file tailer with append-tailing and truncation detection (`internal/watcher`)
- Full data pipeline wiring all packages into a single watcher handler (`internal/pipeline`)
- `budgetclaw watch` long-running daemon mode
- `budgetclaw status` tabular spend view by project and branch
- `budgetclaw limit set/list/rm` budget rule management
- `budgetclaw alerts setup/test` ntfy configuration
- `budgetclaw unlock` and `budgetclaw locks list/path` lock management
- `budgetclaw init` XDG directory setup and default config generation
- `budgetclaw config path` diagnostic helper
- Claude Code plugin manifest with `/spend` skill and session-start hook

[Unreleased]: https://github.com/RoninForge/budgetclaw/compare/HEAD...HEAD
