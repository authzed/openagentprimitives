# Spec: `gh-readonly`

**Intent:** GitHub read-only for my repo

**Source:** `llm:gemini-3-flash-preview`  (2026-04-24T15:00:00Z)

## What you can do

- ✓ Look at a single pull request you name  (`gh pr view`)

## Constraints

- You named this specific repo — only it is allowed

## Hard stops

- ✗ You said 'read-only', so nothing irreversible
- ✗ Read-only intent — no writes to network or disk
- ✗ Read-only intent — never persist new credentials

## Bounds

- Only GitHub's own hosts

**Network:** only api.github.com, github.com
**Creds:** uses GITHUB_TOKEN; never writes/changes it

## ⚠ Warnings

- search subcommands are not allowed at all here — add them if you need them

## ❌ Could not represent

- "only during business hours" — spec schema does not support time-based constraints

## Deliberately excluded

- `pr comment` — writes a comment — excluded from read-only
