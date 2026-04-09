# testdata/

Go's [testdata convention](https://pkg.go.dev/cmd/go#hdr-Test_packages) treats this directory as off-limits to the build system. Use it for fixtures loaded by `*_test.go` files.

Planned fixtures:

- `sample_session.jsonl` — a scrubbed, minimal Claude Code session log with 10-20 tool calls across two models, used by `internal/parser` tests
- `pricing_v1.json` — a frozen snapshot of the pricing table, used to detect accidental pricing regressions
- `git_worktree/` — a fake `.git/HEAD` fixture for `internal/gitattr` tests

When adding fixtures from real sessions, scrub anything sensitive (absolute paths, machine names, usernames) before committing.
