# codebot — passthrough coding agent

A coding agent, packaged as a `.oap` agent container, that clones a repository,
applies changes with the **user's own Claude Code** (subscription token),
commits **as the user** (GitHub PAT), and opens a pull request — all without the
platform ever holding the user's credentials server-side.

## Bundle layout

```
codebot/
  oap.yaml            # manifest: identity + the one install question (toolchains). NO secrets.
  manifests/          # the CRs installed onto the cluster
    agentclass.yaml       # AgentClass codebot (userPassthrough, system prompt, tool bundles, budget)
    spiceboxclasses.yaml  # SpiceboxClass gitlike-bundle + codelike-bundle (codelike has no toolchains — the question fills it)
    toolspecs.yaml        # SpiceboxToolspec git-rw + gh-pr + claude-oauth
  assets/logo.svg     # the agent's logo
  README.md
  required_test.go    # asserts the bundle requires exactly {github-token, anthropic-oauth}
```

## Passthrough model

`identityMode: userPassthrough` means:

- The **platform** holds no per-agent credential. The orchestrator LLM uses the
  cluster default-model token (no model is pinned); Slack credentials are
  supplied when you attach a channel.
- The **user** links two credentials once via the identityd portal. After that,
  every session injects those credentials into the sandbox at start time:

  | Credential name   | What it is                         | Linked by         |
  | ----------------- | ---------------------------------- | ----------------- |
  | `github-token`    | GitHub Personal Access Token (PAT) | user at identityd |
  | `anthropic-oauth` | Claude Code subscription token     | user at identityd |

  `git` declares `GIT_TOKEN → git-token`; the gitlike bundle remaps
  `git-token → github-token` via `credentialRemap`, so git and gh share a
  single GitHub PAT. Claude Code gets the user's own subscription token — the
  platform API key is never used for the inner Claude process.

  (`required_test.go` asserts the bundle resolves to exactly these two
  user-linked credentials.)

- Under `userPassthrough` the session owner is forced to the person who started
  the session. A plain Slack channel is sufficient — no per-user channel
  routing needed.

## Prerequisites

- A Kubernetes cluster with agentprimitives installed (`oap install`) and a
  **cluster default model** configured (`oap init` / `oap settings wizard`) — this
  agent pins no model, so the orchestrator uses the cluster's default-model
  token.
- The `oap` CLI on your PATH.
- Each user must have:
  - A GitHub PAT with `repo` scope.
  - A Claude Code subscription. Run `claude setup-token` locally; the token is
    displayed on stdout. Copy it for the identityd link step.

## Install

```bash
oap agent lint    examples/codebot
oap agent install examples/codebot --namespace default
```

The install prompts one question — **which language toolchains** the codelike
sandbox should get:

```
? Which language toolchains should Claude Code have?
  > [x] go
    [x] node
```

Select any combination (or none). Your choice fills `spec.toolchains` on the
`codelike-bundle` SpiceboxClass. For automation, answer non-interactively:

```bash
# both (the default):
oap agent install examples/codebot --namespace default --values <(printf 'toolchains: [go, node]\n')
# just Go:
oap agent install examples/codebot --namespace default --set toolchains=go
# none (minimal sandbox):
oap agent install examples/codebot --namespace default --values <(printf 'toolchains: []\n')
```

After install, the identityd portal prompts each user to link their
`github-token` and `anthropic-oauth` credentials the first time they start a
session (with inline `claude setup-token` guidance).

## About the toolchains question

The sandbox base image ships **no compilers by default** — it is deliberately
minimal, so every class stays small and low-attack-surface. Selecting `go`
and/or `node` composes that cluster-scoped `SpiceboxToolchain` onto every
codelike pod as a **read-only** overlay at `/opt/ap-toolchains/<name>`, adds its
`bin/` dirs to `PATH`, and points `GOCACHE`/`GOMODCACHE` at a shared, writable,
disk-backed cache at `/var/ap-cache`. This lets Claude Code actually
build/vet/test the code it edits (`go build`, `pnpm install`, `tsc --noEmit`)
instead of producing text that merely looks like it compiles.

`go` and `node` ship by default with `oap install`; list what's available in your
cluster with `kubectl get spiceboxtoolchains`. Toolchains **compose** — a
full-stack repo selects both, mounted side by side. They are frozen per-session
at first bind, so changing the selection (re-install, or edit
`spec.toolchains`) affects new sessions only.

## Attach a channel

Channels are per-install config, not part of the container. Wire codebot to
Slack after installing:

```bash
oap channel create --kind slack
```

Pick `codebot` when asked which AgentClass to bind.

## How it works

1. **User sends a task** in the Slack channel, e.g.
   `"Add input validation to the login form in github.com/example/webapp"`

2. **Orchestrator** (the cluster-default model, driven by the platform's
   default-model token) receives the task and runs the tool loop:
   - `gitlike_git clone https://github.com/example/webapp /workspace/webapp`
   - `gh api user` → derives `login` and `id` for commit attribution
   - `codelike_claude --print --output-format stream-json … "<task>"`
   - `gitlike_git … add … commit … push`
   - `gitlike_gh pr create …`

3. **Reply** in Slack with the PR URL.

Git commits are attributed to the user's GitHub identity (non-cryptographically
signed, "Unverified" label expected in v1).

## Deferred / known limitations

- **Verified commits** — commits are attributed but not GPG/SSH-signed.
  Signing requires either a user-supplied signing key (out of scope for v1) or
  a signed-commit sidecar.
- **AP-driven Anthropic OAuth** — `anthropic-oauth` is a user-supplied token
  from `claude setup-token`, not a server-side OAuth flow. Claude Code's OAuth
  client is loopback-only, audience-bound, and ToS-restricted to interactive
  use; AP cannot drive the authorization-code flow on the user's behalf.
- **Orchestrator-LLM passthrough** — the orchestrator could itself use the
  user's Anthropic account (avoiding the platform default-model token). Deferred
  pending Anthropic API support for user-delegated keys.
- **Workspace persistence** — the shared `/workspace` volume is ephemeral;
  cloned repos vanish when the session ends.
