# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
