# Contributing to budgetclaw

Thanks for considering a contribution. This is a small tool with a sharp scope: **local spend monitoring for Claude Code**. Contributions that stay inside that scope are welcome. Contributions that expand the scope (cloud sync, multi-vendor proxies, AI features) should be discussed in an issue first.

## Ground rules

- **Keep the trust pitch intact.** budgetclaw must never touch API traffic, never read prompts or responses, never send data off the machine without opt-in. Any PR that changes this needs explicit maintainer sign-off.
- **Honest positioning only.** No marketing-speak in code, comments, or docs. No emoji in copy or UI.
- **Stay boring.** Prefer stdlib over dependencies. Prefer plain data structures over abstractions. Three similar lines are fine, premature generalization is not.

## Development setup

Requires Go 1.24 or later.

```sh
git clone https://github.com/RoninForge/budgetclaw.git
cd budgetclaw
make build     # compile ./bin/budgetclaw
make test      # go test ./... with race detector
make lint      # golangci-lint run
make fmt       # gofmt + goimports
```

## Running tests

```sh
go test ./...                 # everything
go test ./internal/paths -run TestXDG  # single package
go test -race -cover ./...    # what CI runs
```

Every new package must ship tests. Every bug fix must add a regression test.

## Commit style

Conventional Commits, but low ceremony:

```
feat(parser): attribute events to git branch via .git/HEAD
fix(enforcer): resolve lockfile race on rapid re-launch
docs: clarify XDG fallbacks in README
```

Keep commits small and focused. Squash before merge if your branch has WIP noise.

## Pull requests

1. Open an issue first for anything larger than a typo.
2. Write tests.
3. Run `make test lint` locally and make sure CI is green.
4. Describe the behavior change in the PR body, not just the diff.
5. Link the issue.

## Code layout

```
cmd/budgetclaw/   thin main() entrypoint
internal/cli/     cobra command tree
internal/version/ build-time version metadata
internal/paths/   XDG filesystem paths
internal/parser/  JSONL tail-reader  (not yet)
internal/pricing/ model → cost table (not yet)
internal/budget/  limit evaluator    (not yet)
internal/enforcer/ SIGTERM + lockfile (not yet)
internal/ntfy/    push client        (not yet)
internal/db/      SQLite rollups     (not yet)
```

Packages are `internal/` unless there's a compelling reason to export. External users should use the CLI, not the Go API.

## Reporting security issues

See [SECURITY.md](SECURITY.md). Do not file public issues for security bugs.
