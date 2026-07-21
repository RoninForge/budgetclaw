# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [v1.3.0] - 2026-07-21

### Added

- Guard Mode: enforce your Goei team's budget caps locally. Opt in with `budgetclaw guard on`; `budgetclaw watch` then fetches the caps your team owner set (over your existing device token) and enforces them on this machine, even offline. A per-developer or team cap sums your whole machine's spend; a per-project cap sums that project. Only caps that are exactly computable on your machine can kill (SIGTERM the runaway, plus a lockfile that names the rule, who set it, and spend versus cap); team-aggregate caps warn only and show how stale the team figure is. It polices agents, not developers: a kill stops a runaway session, and every enforcement event that syncs back is content-free (rule id, amount, cap, machine, timestamp; never prompts or code). A device never obeys a server until you turn Guard Mode on.
- `budgetclaw guard on` / `off` / `status` manage the opt-in and show which caps are cached.
- `budgetclaw limit list` gains a SOURCE column so local rules and team (`goei`) caps are distinguishable at a glance.

### Changed

- `budgetclaw sync` now carries any queued Guard Mode audit events up with your spend and refreshes the cached policy set from the response. Nothing new leaves your machine beyond the content-free enforcement events above.

## [v1.2.1] - 2026-07-20

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.20-cb66f43`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.2.0] - 2026-07-18

### Added

- `budgetclaw sync --show-payload` prints the exact JSON request body that `sync` would POST to Goei and sends nothing. It needs no device token, so you can audit every byte that would leave your machine (daily cost and token rollups, model, project, branch, and machine label; never prompts or code) against Goei's published ingest contract before ever connecting an account.
- `budgetclaw sync --save` persists the resolved device token (and any explicit `--endpoint`/`--machine`) to the config file so later syncs run with no flags. Empty arguments are preserved, so saving just a token keeps an existing endpoint or machine.

## [v1.1.4] - 2026-07-10

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.10-cc35e0b`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.1.3] - 2026-07-09

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.09-43f905c`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.1.2] - 2026-07-08

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.08-8eaf41d`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.1.1] - 2026-07-07

### Changed

- Discovery and docs: expanded the plugin keywords and refreshed the README and spend skill copy with high-intent Claude Code cost-tracking terms (cost per project, per branch, per model; ccusage alternative), and documented that `sync` feeds a hosted team dashboard (Goei) with re-priced 12-month history across every machine and teammate. Metadata and copy only; no CLI behavior change.

## [v1.1.0] - 2026-07-02

> **Migration: existing Goei-synced users will see a one-time double-count over the first re-synced window (default 30 days) after upgrading.** Because `sync` now stamps every record with a machine identity (defaulting to your hostname) and the Goei server keeps per-machine rows separate, it no longer deletes the pre-machine untagged rows it stored before (that would discard data once you sync from more than one machine). So for the re-synced window, the old untagged rows and the new machine-tagged rows briefly coexist and add up. New installs are unaffected. The double-count is a one-time step, not an ongoing error: it does not grow with each sync, it self-limits as older days age out of the sync window, and it clears fully once every machine on the account has upgraded and re-synced, after which the stale untagged rows can be removed on the Goei side. During the transition window it may trip a single spurious budget alert.

### Added

- `budgetclaw sync` now stamps a per-machine identity on every spend record it sends to Goei (new `machine` field, `omitempty`), so the Goei server keeps two machines' rollups from colliding instead of merging the same (day, project, branch, model) from a laptop and a desktop into one row. The value resolves as `--machine` flag, then the `GOEI_MACHINE` env var, then `[goei].machine` in config, and finally falls back to the OS hostname. The hostname is not a secret, so sync stays zero-key and zero-prompt; the overrides exist for anyone who would rather send a custom label. Backward compatible: the field is additive and omitted when empty, so an older Goei server simply ignores it. `--dry-run` now also reports the machine identity being sent.

## [v1.0.8] - 2026-07-01

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.01-b9ae0e4`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.7] - 2026-07-01

### Changed

- pricing: refresh vendored ai-price-index to `v2026.07.01-c31b7c3`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.6] - 2026-06-29

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.29-f7199e3`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.5] - 2026-06-24

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.24-a997f8b`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.4] - 2026-06-22

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.22-becbe72`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.3] - 2026-06-17

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.17-4ab5db2`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.2] - 2026-06-17

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.17-2a11475`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.


## [v1.0.1] - 2026-06-16

### Changed

- pricing: refresh vendored ai-price-index to `v2026.06.16-5063362`. Point-in-time pricing means a new rate adds a new interval and does not change rows already priced at their then-effective rate, so no `backfill --rebuild` is needed.

## [v1.0.0] - 2026-06-16

> **Migration: after upgrading, run `budgetclaw backfill --rebuild` once.** v1.0.0 re-prices every event at its then-effective rate. Existing rollups were priced at whatever the table said when the event was first seen; the rebuild wipes and replays so every historical event reflects the rate that was in effect on its own timestamp.

### Changed

- **Pricing is now sourced from the vendored, pinned [ai-price-index](https://github.com/RoninForge/ai-price-index) dataset** (tag `v2026.06.16-662bfa9`) instead of a hand-maintained rate table. The dataset's anthropic artifacts are committed under `internal/pricing/index/**` (with sha256 provenance) and code-generated into `internal/pricing/table_gen.go` at build time. Still zero-key, zero-latency, and fully offline: the price table is embedded in the binary, there is no runtime network access. Cache-write/read multipliers stay engine constants.
- **Point-in-time pricing.** Each event is now priced at the rate that was in effect on its own timestamp, not at today's rate. An event recorded while a model was on an older tier is priced at that older tier, so historical cost stays frozen as fact. The `watch` pipeline and `backfill` both re-price this way. Events for a known model with no rate covering their timestamp (a retired model, or an event older than the model's earliest recorded price) are skipped non-fatally, like unknown models.

### Added

- `budgetclaw pricing history <model> [--json]` prints the full point-in-time price table for one model: each interval's effective-from date, through date (open when current), and input/output rate per MTok. Accepts canonical ids and aliases.
- `budgetclaw pricing provenance [--json]` prints the pinned ai-price-index dataset tag and the exact repo commit the embedded rates were generated from, so every rate traces back to an upstream commit.
- `budgetclaw sync` now sends a per-(day, project, branch, model) token rollup inline on each spend record (`tokens: {input, output, cache_read, cache_write_5m, cache_write_1h}`), at the same grain as the dollar amount, so a future Goei server can re-price tokens at its own point-in-time rate. `amountCents` is still sent; the change is backward compatible (the current server ignores the new field). `--dry-run` now also reports the total token count.
- CI freshness gate: a `pricing-codegen` job runs `go generate ./...` against the vendored dataset and fails if the committed `internal/pricing/table_gen.go` drifted from what the data produces, keeping the embedded rates hermetic.

### Fixed

- `budgetclaw pricing rates` no longer errors when the dataset includes retired models. `KnownModels()` now spans retired models that have no current rate; `pricing rates`/`pricing rates --json` skip those so the current-rates output (consumed by the pricing-audit workflow) stays limited to currently-priced models with an unchanged JSON shape.

## [v0.1.9] - 2026-06-10

### Fixed

- **Cost overcount of 2.5-2.85x, all prior versions affected.** Claude Code writes the same assistant API response across multiple JSONL lines (identical `message.id` and `requestId`, distinct line uuids), and budgetclaw counted every line as a separate event. Events carrying a message id now dedup on `(message_id, request_id)` with last-line-wins semantics, matching how the same response is reconciled by Anthropic billing. Verified against an independent reference computed from the same logs: all closed days matched exactly after the fix. **After upgrading, run `budgetclaw backfill --rebuild` once** to clean an existing database; until then, totals remain inflated.

### Added

- `budgetclaw sync` pushes locally-computed daily spend and token aggregates to a Goei dashboard (https://goei.roninforge.org) using a device token. Zero-key: only dollar-and-token rollups leave the machine, never prompts or API keys. Flags: `--days`, `--since`, `--no-branch`, `--dry-run`; token via `--token`, `GOEI_DEVICE_TOKEN`, or `[goei].token` in config.
- Spend records sent to Goei carry the git branch as a dedicated field, with the project name kept bare, enabling clean per-project and per-branch attribution server-side. `--no-branch` omits the field so branches collapse into one project row.

### Changed

- modernc.org/sqlite bumped from 1.50.1 to 1.51.0.

## [v0.1.8] - 2026-06-10

### Added

- Claude Fable 5 (`claude-fable-5`) added to the pricing table at $10/MTok input, $50/MTok output, a new flagship tier above Opus introduced in the 2026-06-09 Fable 5 / Mythos 5 launch. Before this, Fable 5 events were silently skipped as an unknown model (verified against live local logs: 322 `claude-fable-5` events were going unpriced). Re-verified every existing rate against the live Anthropic pricing page on 2026-06-10: no older model changed price, cache multipliers unchanged. Mythos 5 ships at the same $10/$50 rate but is restricted-access (Project Glasswing), has no published API model ID, and never reaches Claude Code's JSONL, so it is intentionally not added.

## [v0.1.7] - 2026-05-29

### Added

- Claude Opus 4.8 (`claude-opus-4-8`) added to the pricing table at $5/MTok input, $25/MTok output (the same tier as Opus 4.5/4.6/4.7). Before this, Opus 4.8 events were silently skipped as an unknown model. Re-verified every existing rate against the live Anthropic pricing page on 2026-05-29: no older model changed price, cache multipliers unchanged.

## [v0.1.6] - 2026-05-18

### Changed

- Dependency bumps: `github.com/shirou/gopsutil/v4` 4.26.3 -> 4.26.4 (#15), `modernc.org/sqlite` 1.50.0 -> 1.50.1 (#18), `github.com/fsnotify/fsnotify` 1.9.0 -> 1.10.1 (#17, applied manually on top of #15 and #18 to resolve a `go.sum` conflict).

### Added

- `AGENTS.md` at the repo root for AI coding agents (Claude Code, Cursor) operating on the codebase.

### Changed

- CI: model-audit and the LiteLLM cross-check are now a single bundled workflow (`pricing-audit.yml`). Reduces maintainer triage to one issue, one PR, one release when a new Anthropic launch coincides with a price cut on the legacy tier (the Opus 4.7 launch + 4.5 / 4.6 price-cut pattern, see v0.1.4).

## [v0.1.5] - 2026-04-28

### Added

- `budgetclaw pricing rates [--json]` subcommand prints every model with its input + output rate per MTok. Feeds the daily LiteLLM cross-check workflow.
- The daily `pricing-audit` workflow now also cross-checks every embedded rate against BerriAI/litellm's `model_prices_and_context_window.json`. Closes the gap that v0.1.4 surfaced: `/v1/models` returns IDs only, so price changes on existing models would slip past the model-only audit. (Bundled into a single `pricing-audit.yml` workflow in v0.1.6.)

## [v0.1.4] - 2026-04-28

### Fixed

- **Opus 4.5 / 4.6 / 4.7 input + output rates corrected from $15 / $75 to $5 / $25 per MTok.** Anthropic moved Opus 4.5+ to a new lower tier; the original maintainer's snapshot from 2026-04-09 captured the pre-cut prices, and prior releases inherited the wrong rate by tier-pattern. Discovered by cross-checking against BerriAI/litellm's `model_prices_and_context_window.json`, then confirmed against the maintainer's screenshot of the Anthropic pricing page. Existing rollups are not auto-corrected because `Insert` is idempotent on uuid; run `budgetclaw backfill --rebuild` to wipe rollups and recompute every historical event at the corrected rates. Opus 4.1 stays at $15 / $75 (still on the legacy tier per Anthropic's published pricing).

### Added

- `budgetclaw backfill --rebuild` flag truncates events and rollups before scanning so a pricing correction is reflected in historical totals. Without `--rebuild`, idempotent inserts leave the old rate baked into existing rollup rows.

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

[Unreleased]: https://github.com/RoninForge/budgetclaw/compare/v1.2.1...HEAD
[v1.2.1]: https://github.com/RoninForge/budgetclaw/compare/v1.1.4...v1.2.1
[v1.1.4]: https://github.com/RoninForge/budgetclaw/compare/v1.1.3...v1.1.4
[v1.1.3]: https://github.com/RoninForge/budgetclaw/compare/v1.1.2...v1.1.3
[v1.1.2]: https://github.com/RoninForge/budgetclaw/compare/v1.0.8...v1.1.2
[v1.0.8]: https://github.com/RoninForge/budgetclaw/compare/v1.0.7...v1.0.8
[v1.0.7]: https://github.com/RoninForge/budgetclaw/compare/v1.0.6...v1.0.7
[v1.0.6]: https://github.com/RoninForge/budgetclaw/compare/v1.0.5...v1.0.6
[v1.0.5]: https://github.com/RoninForge/budgetclaw/compare/v1.0.4...v1.0.5
[v1.0.4]: https://github.com/RoninForge/budgetclaw/compare/v1.0.3...v1.0.4
[v1.0.3]: https://github.com/RoninForge/budgetclaw/compare/v1.0.2...v1.0.3
[v1.0.2]: https://github.com/RoninForge/budgetclaw/compare/v1.0.1...v1.0.2
[v1.0.1]: https://github.com/RoninForge/budgetclaw/compare/v1.0.0...v1.0.1
[v1.0.0]: https://github.com/RoninForge/budgetclaw/compare/v0.1.9...v1.0.0
