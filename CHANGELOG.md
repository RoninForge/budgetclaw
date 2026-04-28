# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `budgetclaw backfill` subcommand walks `$HOME/.claude/projects/**/*.jsonl`, prices every assistant event, and inserts rollups into the local state DB. Safe to run repeatedly thanks to the existing `ON CONFLICT(uuid) DO NOTHING` constraint. Use after upgrading to a release that adds new model pricing to recover attribution for events the prior watcher dropped.
- Pricing table now recognizes two more dated Opus variants the daily audit detected on Anthropic's `/v1/models`: `claude-opus-4-5-20251101` and `claude-opus-4-1-20250805`. Both at the established Opus tier ($15 input / $75 output per MTok).

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
