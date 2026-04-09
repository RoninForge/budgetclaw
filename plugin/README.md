# BudgetClaw Claude Code Plugin

Adds a `/spend` slash command and a session-start hook to Claude Code.

## What it does

- **`/spend`** - Shows your current spend by project and branch, active budget limits, and any breach locks
- **Session start hook** - Prints a brief spend summary when you start a new Claude Code session (or a "not installed" notice if budgetclaw is missing)

## Prerequisites

BudgetClaw must be installed:

```sh
curl -fsSL roninforge.org/get | sh
budgetclaw init
```

## Install the plugin

```sh
claude plugin install ./plugin
```

Or for development, point Claude Code at this directory:

```sh
claude --plugin-dir ./plugin
```

## Plugin structure

```
plugin/
  .claude-plugin/
    plugin.json          manifest
  hooks/
    hooks.json           session-start hook
  skills/
    spend/
      SKILL.md           /spend slash command
```
