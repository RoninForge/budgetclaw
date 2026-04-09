# Scaffold decisions

This document captures the load-bearing decisions made while scaffolding budgetclaw, so future contributors (and future us) understand why things are the way they are. Each entry is short: the decision, the alternatives considered, and the reason. If a decision turns out wrong, update this file when we change it, don't delete the history.

---

## 1. Module path: `github.com/RoninForge/budgetclaw`

**Decision:** dedicated standalone repository, not a monorepo subpath.

**Alternatives:** `github.com/RoninForge/platform/tools/budgetclaw`.

**Reason:** `go install`, Homebrew, Claude Code plugin marketplaces, and release-binary SEO all expect a clean, dedicated repo. The monorepo can still serve as the development surface, but the published artifact is a proper standalone Go module. When it's time to push, split out the subtree.

## 2. Go version floor: 1.25

**Decision:** minimum Go version is 1.25 in `go.mod`.

**Alternatives:** 1.22 (broader installed base), 1.24 (one older minor), 1.26+ (newest).

**Reason:** Originally set to 1.24. Bumped to 1.25 on 2026-04-09 when `go get modernc.org/sqlite@latest` pulled in v1.48.1, which requires Go 1.25 transitively. 1.25 is in Homebrew and all mainstream package managers by 2026. CI tracks `go-version: stable` so future bumps don't need a separate PR.

## 3. XDG Base Directory Spec, not `~/.budgetclaw/`

**Decision:** `internal/paths` resolves config/state/data/cache through `$XDG_CONFIG_HOME`, `$XDG_STATE_HOME`, `$XDG_DATA_HOME`, `$XDG_CACHE_HOME` with the spec-defined defaults.

**Alternatives:** a single `~/.budgetclaw/` dot-dir (what ccusage, claude-pulse, and many adjacent tools do).

**Reason:** XDG is the 2026 expectation for new CLI tools. It keeps home directories clean, separates regenerable cache from durable state, and lets containerized setups (`XDG_CONFIG_HOME=/etc/budgetclaw`) work out of the box. The one-folder convention is legacy. Users who prefer a single folder can set all four XDG vars to the same root.

## 4. `internal/` only, no public Go API

**Decision:** every package except `cmd/budgetclaw` lives under `internal/`. We do not export a `pkg/` API.

**Alternatives:** expose parser/pricing/budget packages so other Go programs can embed budgetclaw.

**Reason:** a public API doubles the maintenance surface. External users should use the CLI. If someone ever needs to embed budgetclaw from Go, we can export a deliberate, minimal surface then — not speculatively now. Internal-first keeps refactoring cheap.

## 5. Cobra for the CLI

**Decision:** `github.com/spf13/cobra` v1.8.

**Alternatives:** `github.com/urfave/cli`, `charmbracelet/fang`, hand-rolled `flag`.

**Reason:** cobra is the boring default. Every Go dev has seen it. Auto-generates shell completions, man pages, and subcommand trees. The alternatives are either less mature or opinionated in ways that lock us in. Viper will follow in Task #6 when we need config file parsing.

## 6. Version metadata: `-ldflags` with `runtime/debug.ReadBuildInfo` fallback

**Decision:** `internal/version` defines lowercase package-level variables set at release time via `go build -ldflags -X`. When ldflags are absent (e.g. `go install @latest`, `go run`, `go test`), fall back to `runtime/debug.ReadBuildInfo`.

**Alternatives:** only ldflags (breaks `go install`), only BuildInfo (breaks release builds with stripped `-s -w`), hard-coded constants updated per release (fragile).

**Reason:** this is the idiomatic Go pattern for CLI version data. The lowercase names force callers to go through `Get()`, which is the single source of truth. The fallback means `go install github.com/RoninForge/budgetclaw/cmd/budgetclaw@latest` produces a useful `version` output without a release pipeline.

## 7. Goreleaser v2 for releases

**Decision:** `.goreleaser.yaml` handles cross-compilation (darwin/linux × amd64/arm64), archive creation, checksums, SBOMs, and GitHub Release publication. Triggered from `.github/workflows/release.yml` on `v*` tags.

**Alternatives:** hand-rolled shell script, `ko`, manual `go build` in CI.

**Reason:** goreleaser is the de-facto standard for Go CLI releases. It handles every detail (SBOM, checksum file format, release notes grouping by conventional-commit prefix) that a hand-rolled script gets wrong. Homebrew tap support is pre-wired, commented out until the tap repo exists.

## 8. `CGO_ENABLED=0` for all builds

**Decision:** Go binaries are built with `CGO_ENABLED=0`.

**Alternatives:** enable cgo for the sqlite driver.

**Reason:** we use `modernc.org/sqlite` (pure Go, no cgo) specifically so we can ship a single static binary that works on every glibc/musl flavor without runtime dependencies. cgo would break cross-compilation and complicate the release matrix.

## 9. Curated `golangci-lint` set (not `default: all`)

**Decision:** `.golangci.yml` explicitly enables 12 linters (errcheck, govet, ineffassign, staticcheck, unused, misspell, gosec, revive, gocritic, unconvert, unparam), default off.

**Alternatives:** `enable-all`, default preset.

**Reason:** the all-linters approach generates noise (style nits, deprecated rules, contradictory advice) that drowns real signal. A curated set that catches correctness bugs without ceremony is better than a stricter set that everyone learns to ignore. gosec is enabled because budgetclaw SIGTERMs processes — security-adjacent code deserves static checks.

## 10. Test pattern: package-level indirection for `os` helpers

**Decision:** `internal/paths` defines `var envGet = os.Getenv` and `var homeDir = os.UserHomeDir` as package-level variables. Tests swap them in `withFakeEnv`.

**Alternatives:** `t.Setenv` + real `os.UserHomeDir`, dependency-inject an `io.FS` or equivalent.

**Reason:** `t.Setenv` would force every test to clean up after itself and can leak between parallel tests. Dependency injection would be overkill for a package this small. The two-variable indirection is the minimum viable seam: testable without a framework, zero runtime cost, one-line override in tests.

## 11. CI matrix: ubuntu-latest + macos-latest

**Decision:** CI runs tests on both Ubuntu and macOS. No Windows.

**Alternatives:** Linux only; full matrix with Windows.

**Reason:** our target audience is Claude Code users, which per Anthropic docs is macOS + Linux primarily. Windows is nominally supported by goreleaser but not tested because we have no launch-day Windows users to serve. Adding Windows to CI is cheap to do later if demand materializes.

## 12. MIT license

**Decision:** MIT.

**Alternatives:** Apache-2.0 (patent grant), GPL-3 (copyleft), BSL (no open-source competitor protection).

**Reason:** budgetclaw has no meaningful IP that a patent grant would protect. GPL/BSL create friction for the "drop it in your dotfiles" install path. MIT is the lowest-friction license that matches the OSS-beachhead positioning. If a patent situation ever emerges, we can relicense.

## 13. No emoji in code, docs, or UI

**Decision:** zero emoji anywhere in the repo.

**Alternatives:** emoji in CLI output, README headers, issue templates.

**Reason:** dev-tool audience finds emoji spam amateurish. Every comparison tool on the reference list (ollama, tailscale, fly, charm.sh, htmx) ships with no emoji in copy or UI. This is a deliberate choice baked into the brand voice, not a preference.

## 14. POSIX `sh` installer, not `bash`

**Decision:** `scripts/install.sh` is `#!/bin/sh` and uses only POSIX-compliant constructs.

**Alternatives:** `#!/usr/bin/env bash` with modern features.

**Reason:** the installer runs via `curl -fsSL ... | sh`. Users pipe into whatever `sh` happens to be, which on Debian is `dash`, on Alpine is `ash`. Bashisms break silently. POSIX sh is a minor writing constraint and catches the bug at edit time instead of after a user reports a mysterious install failure on their Alpine Docker image.

## 15b. Use Claude Code's `gitBranch` field verbatim (no `.git/HEAD` walking)

**Decision:** `parser.Event.GitBranch` is populated directly from the JSONL `gitBranch` field. We never walk `.git/HEAD` ourselves.

**Alternatives:** Walk the filesystem from each event's `cwd` up to find `.git/HEAD`, parse the ref, cache by cwd.

**Reason:** Verified against 38,911 real assistant events across 727 JSONL files in 3 projects (2026-04-09): Claude Code writes `gitBranch` on every assistant line, zero missing. Detached HEAD appears literally as the string `"HEAD"`. Walking the filesystem adds zero value, adds an entire package to maintain, and would fail for users who clean up their `.git/` directories between sessions. Task #4 in the venture plan was deleted as a result. If a future Claude Code version stops writing this field, we'll fall back to the walker then.

## 15c. Skip `<synthetic>` model entries at parse time

**Decision:** `parser.Parse` returns `(nil, nil)` for any assistant message whose `message.model` starts with `<`.

**Alternatives:** Let them through and handle them downstream, or use an explicit skip list.

**Reason:** Claude Code writes `<synthetic>` assistant messages for framework internals (retries, cache probes) with `service_tier: null`. Anthropic does not bill them. Skipping at parse time keeps `cost_usd = 0` impossible to accidentally attribute to a project, and keeps pricing/db code from having to carry a "was this really billed" flag. The `<...>` convention is the documented sentinel pattern, so any future sentinel will match the prefix check automatically.

## 15d. UUID as the dedupe key (not log path + byte offset)

**Decision:** `events.uuid` is the primary key, with `ON CONFLICT DO NOTHING` for idempotent inserts.

**Alternatives:** Composite `(log_path, byte_offset)` key, as originally planned in the venture doc.

**Reason:** The original design assumed we'd track log files by position. Real data showed every assistant line already carries a per-message UUID. Using it as the key means we're resilient to:
- Log file rotation or rename
- Users syncing logs between machines
- Re-parsing a file from scratch after an upgrade
- Claude Code changing its file layout

Cost: none — the UUID is already there. Upside: the dedupe guarantee survives any filesystem reshuffle.

## 15e. Costs frozen at insert time; db package has no pricing dependency

**Decision:** `db.Insert(ctx, event, costUSD)` takes the already-computed cost as an explicit argument. The `db` package does not import `pricing`.

**Alternatives:** `db.Insert(event)` that looks up rates internally, or a shared `cost` package that bundles both.

**Reason:** Historical costs should match what the user saw when the event happened. If Anthropic changes prices next month, yesterday's rollups should reflect yesterday's rates. Passing cost in at insert time guarantees frozen historical fact. It also keeps package boundaries clean: `parser → pricing → db`, no cycles, each testable in isolation.

## 15f. Rollup day column is TEXT in UTC (YYYY-MM-DD)

**Decision:** `rollups.day` is `TEXT NOT NULL` containing an ISO 8601 date like `2026-04-09`. All day boundaries are computed in UTC.

**Alternatives:** Integer unix timestamps at 00:00:00, a separate (year, month, day) tuple, or local-timezone boundaries.

**Reason:** SQLite has no native DATE type — everything is TEXT or INTEGER. ISO 8601 text sorts lexically, compares correctly with `BETWEEN`, and is debuggable by eye in `sqlite3` shells. UTC boundaries keep the rollup consistent across travel and DST; the budget evaluator is responsible for translating a user's local "today" into a UTC time range via `RollupSum`. This matters for Thailand users (UTC+7) whose local day is 7 hours ahead of UTC, but the translation belongs in one place (the evaluator), not scattered through the storage layer.

## 15g. WAL journal mode on file-backed databases only

**Decision:** `PRAGMA journal_mode=WAL` and `PRAGMA synchronous=NORMAL` are applied only when the database is file-backed. In-memory (`:memory:`) databases get the default rollback journal.

**Alternatives:** Apply WAL unconditionally; apply neither.

**Reason:** WAL's benefits (concurrent readers, crash safety, no reader-writer blocking) are meaningless for `:memory:` where there's no disk. Some driver versions also reject WAL on in-memory databases with a "cannot change to wal mode" error. Gating the pragmas on file-backed paths avoids both problems. In-memory databases also get `SetMaxOpenConns(1)` so every statement reuses the same connection, which is the only way to keep `:memory:` state coherent across queries.

## 15h. Commit style: Conventional Commits, grouped in changelog

**Decision:** conventional-commits prefixes (`feat`, `fix`, `docs`, etc.) and `.goreleaser.yaml` groups the auto-generated release notes accordingly.

**Alternatives:** free-form commit messages, separate CHANGELOG edits.

**Reason:** conventional commits are low-ceremony once you're used to them, and goreleaser's group rules mean the release notes write themselves. Hand-maintaining CHANGELOG.md on every PR is tedious and drifts from reality.
