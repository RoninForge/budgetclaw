// Command budgetclaw is a local telemetry reader and spend monitor for
// Claude Code. It watches the JSONL session logs Claude Code writes under
// $HOME/.claude/projects, attributes each tool-call's token cost to a
// project and git branch, and enforces budget caps by sending SIGTERM to
// the client process on breach. It never touches API traffic: zero key,
// zero prompts, zero latency added.
//
// See https://github.com/RoninForge/budgetclaw for documentation.
package main

import (
	"fmt"
	"os"

	"github.com/RoninForge/budgetclaw/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "budgetclaw:", err)
		os.Exit(1)
	}
}
