---
name: spend
description: Claude Code cost tracking via BudgetClaw. Show current Claude Code spend and usage per project and per branch, with budget caps and breach locks. Triggers on claude code cost, claude code usage, spend per project, cost per branch, cost tracking.
user-invocable: true
allowed-tools: Bash(budgetclaw *)
---

Show the current spend status:

!`budgetclaw status`

If the user asks about budget limits, also show:

!`budgetclaw limit list`

If there are active breach locks, show:

!`budgetclaw locks list`
