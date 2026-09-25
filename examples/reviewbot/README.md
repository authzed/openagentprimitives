# reviewbot — automated GitHub pull-request reviewer

An agent-owned code reviewer, packaged as a `.oap` agent container. A push to
a pull request arrives as a GitHub webhook — there is no human starting a
session — so reviewbot clones the diff **read-only**, drives an inner Claude
Code through one or both of two bundled review skills, delivers the summary
to Slack, and records the outcome as a GitHub Check Run on the pull request's
head commit.

**reviewbot never writes to a repository and never comments on a pull
request.** See "The safety boundary" below for exactly how that is enforced,
and why it is enforced twice.

## Before you start: the Docker image is not built yet

This bundle's `demo-reviewbot-codelike` sandbox class runs Claude Code from
`/opt/ap-toolchains/claude/bin/claude`, which lives in the
`oap-toolchain-claude` overlay image (`images/toolchain-claude/`). That path
is now a new `claudeshim` launcher binary added on this branch (it links
staged skills into Claude Code's discovery path, then execs the real `claude`
binary alongside it), and **no `docker build` of `images/toolchain-claude` has
been run yet on this branch — the image is unbuilt and unverified.** Build and
load/push it before installing this bundle, the same way the `go`/`node`
toolchain images are built (see `config/toolchains/README.md`).

While you're at it, re-measure `config/toolchains/claude.yaml`'s
`spec.sizeBytes` (currently `285,000,000`) against the real, built image, per
`config/toolchains/claude.yaml`'s own comment:

```bash
docker run --rm --entrypoint /bin/sh ap-toolchain-claude:dev \
  -c 'du -sk /opt/ap-toolchains/claude'
```

The baseline (256,241,664 bytes) was measured before `claudeshim` existed, when
the payload was a single native binary. `sizeBytes` was never re-measured
after `bin/claude.real` + `bin/claude` (the shim) landed side by side — a
local proxy build measured the shim itself at ~2.7 MB against the ~28 MB of
headroom already in the 285,000,000 budget, so the number is *expected* to
hold, but it has not been confirmed against the real image. Update the value
and the comment for real after the first build rather than trusting that
estimate — `spec.sizeBytes` under-counting the true on-disk usage evicts the
sandbox pod mid-copy (the kubelet enforces the emptyDir `SizeLimit` against
allocated blocks), which fails mid-session rather than at install time.

## Bundle layout

```
reviewbot/
  oap.yaml              # manifest: identity, the required Anthropic key, the two declared channels (github in, slack out), the one install question (toolchains)
  manifests/             # the CRs installed onto the cluster
    agentclass.yaml         # AgentClass demo-reviewbot — identity, the review-loop system prompt, tool bundles, budget
    agentidentity.yaml      # AgentIdentity demo-reviewbot-id — the two agent-owned credentials
    skillsource.yaml        # SkillSources context-engineering-kit + trailofbits-skills — the two review methodologies, branch-pinned
    spiceboxclasses.yaml    # SpiceboxClass demo-reviewbot-gitlike (git read-only + gh) and demo-reviewbot-codelike (claude)
    toolspecs.yaml          # SpiceboxToolspec demo-reviewbot-git-ro, demo-reviewbot-gh-review, demo-reviewbot-claude
  assets/logo.svg        # the agent's logo
  README.md
```

## Agent-owned identity model

Unlike `examples/codebot` (which borrows a human's own GitHub PAT and Claude
Code subscription token, `identityMode: userPassthrough`), reviewbot's
sessions start from a webhook, not a person — there is no channel user to
borrow a credential from. `spec.identityMode` is left at its default,
`agent`: every credential is minted or read on **reviewbot's own** behalf,
declared once in `manifests/agentidentity.yaml`.

| Credential name    | Type       | What it is                                                                                    | How it's provisioned |
| ------------------- | ---------- | ---------------------------------------------------------------------------------------------- | --------------------- |
| `github-app`        | `githubApp` | reviewbot's own GitHub App installation token — minted roughly hourly from the App's private key | Written by the GitHub channel wizard, run by `oap agent install` for the `demo-reviewbot-gh` channel `oap.yaml` declares (see "Wiring the channels" below), as `<channel-name>-creds` — `manifests/agentidentity.yaml`'s `secretRef` reads `demo-reviewbot-gh-creds` |
| `anthropic-api-key` | `static`    | reviewbot's own Anthropic API key — the inner Claude Code authenticates with this, not a user's subscription | **You create it manually, before installing** (see below) — there is no automated setup flow for a plain static API key |

## Prerequisites

- A Kubernetes cluster with agentprimitives installed (`oap install`).
- The `oap` CLI on your PATH.
- The `oap-toolchain-claude` image built and available to your cluster (see
  the warning above).
- An Anthropic API key for reviewbot's own account (a plain API key, not a
  Claude Code subscription token — reviewbot has no human session to borrow
  one from).
- A GitHub organization or account you can register a GitHub App under, and a
  test repository in it you can open pull requests against — this is what you
  point the GitHub Channel at.
- A Slack workspace to deliver review summaries into.

## Install

1. **The Anthropic API key is asked for during install; you do not create the
   Secret yourself.** `oap.yaml` declares it under `requires.secrets` with its
   key named, so `oap agent install` looks for `demo-reviewbot-anthropic` in the
   target namespace and, when it is absent, prompts for the key and creates the
   Secret. Nothing is echoed to the terminal.

   A Secret that already exists is left untouched — so bringing your own is
   still supported, and re-installing never overwrites the key in place:

   ```bash
   kubectl create secret generic demo-reviewbot-anthropic \
     --from-literal=api-key=<ANTHROPIC_API_KEY> \
     --namespace <ns>
   ```

   An install with nobody at the terminal (CI, a pipe) cannot prompt, so it
   refuses rather than installing an agent that could never authenticate. Answer
   it up front instead:

   ```bash
   oap agent install examples/reviewbot --namespace <ns> \
     --set requires.secrets.demo-reviewbot-anthropic.api-key=<ANTHROPIC_API_KEY>
   ```

2. **The bundled skill sources already point at real upstream repositories —
   nothing to fork or create.** `manifests/skillsource.yaml` declares two
   `SkillSource`s:

   | `SkillSource`               | Repository                                    | `ref`    | Skill (default handle)                          |
   | ---------------------------- | ---------------------------------------------- | -------- | ------------------------------------------------ |
   | `context-engineering-kit`    | `github.com/NeoLabHQ/context-engineering-kit`  | `master` | `review-pr` — the default, for an ordinary PR    |
   | `trailofbits-skills`         | `github.com/trailofbits/skills`                | `main`   | `differential-review` — for security-sensitive PRs |

   Each upstream `SKILL.md`'s frontmatter `name` matches its local handle
   (`review-pr`, `differential-review` — `manifests/agentclass.yaml`'s
   `spec.skills[].name`) exactly, which is what the inner Claude Code needs
   to discover them at `/skills/review-pr/SKILL.md` and
   `/skills/differential-review/SKILL.md`. If an upstream repository ever
   renames its skill's frontmatter out from under this bundle, staging fails
   loudly at session start, naming both values — not silently ignored
   (`pkg/controllers/agentsession/skills.go`).

   **Both `ref`s are a branch (`master` / `main`), not a tag or commit SHA —
   and `oap agent lint` stays clean either way.** `pkg/tools/skills/
   canonical.Name.PinStrength` treats a named branch exactly like a tag
   (`PinNamed`); only an *absent* `@ref` (`PinUnpinned`) is flagged, because a
   branch and a tag are syntactically indistinguishable without querying the
   remote. The real trade-off is reproducibility, not lint cleanliness: a
   branch ref means the review methodology can change under a rerun of the
   exact same pull-request commit, so a review is reproducible only as far as
   `master` / `main` stay stable upstream. **For a production install, pin
   each `SkillSource.spec.ref` to a tag or commit SHA instead** — resolve the
   commit you want to pin to and replace `master` / `main` with it in
   `manifests/skillsource.yaml`, then update the matching `@master` / `@main`
   suffix on `manifests/agentclass.yaml`'s `spec.skills[].ref` to the same
   value (the canonical name embeds the SkillSource's `ref` verbatim, so the
   two must move together — see that file's comment).

3. **Lint, then install:**

   ```bash
   oap agent lint    examples/reviewbot
   oap agent install examples/reviewbot --namespace <ns>
   ```

   The install prompts one question — **which language toolchains** the
   review sandbox should get, so the inner Claude Code can run the reviewed
   repository's own build/test suite (per whichever review skill is used)
   instead of reviewing the diff by static reading alone:

   ```
   ? Which language toolchains should the inner Claude Code have?
     > [x] go
       [x] node
   ```

   For automation, answer non-interactively the same way codebot's README
   does: `--set toolchains=go`, or `--values <(printf 'toolchains: []\n')` for
   the minimal sandbox.

   This is also the step that wires the two channels below — see "Wiring the
   channels" for what it asks and what a non-interactive install does
   instead.

## Wiring the channels

A Channel is still per-install deployment config, not part of the portable
container — deliberately not something a `.oap` bundle can carry (see
`pkg/platform/oap/bundle.go`'s `allowedBundleKinds`). What `oap.yaml` *does*
carry is a `requires.channels` declaration naming the two this agent needs —
inbound `github` (role `input`, named `demo-reviewbot-gh`) and outbound
`slack` (role `output`, named `demo-reviewbot-slack`) — so `oap agent install`
(step 3, above) can create and wire both itself, right after the bundle's CRs
land, instead of leaving that to a separate manual step.

Installing drives each kind's own setup wizard — the identical flow `oap
channel create --kind github` / `--kind slack` walks on its own — with two
answers already supplied from the declaration and never asked: the Channel's
name, and which AgentClass to bind (`demo-reviewbot`). For github, you are
still asked for the GitHub organization the App is registered under and the
base URL GitHub can reach your cluster's webhook at, and the SpiceDB
`authzSubject` (a pull request's author has no AP identity of their own, so
there is no per-user attribution to derive one from); then it walks creating
the GitHub App itself — an automated loopback flow that submits GitHub's
App-manifest form for you, with a manual fallback (GitHub's plain "New GitHub
App" form, filled in by hand from a printed reference manifest) for a
headless environment or a blocked port — and finally installing the App onto
your organization and pasting back the installation ID; that last step is
always manual, because the App's manifest sets no Setup URL for GitHub to
redirect back through.

**A non-interactive install (`--set`/`--values`, nobody at the terminal)
cannot run either wizard.** It skips both channels, says so in the install
summary, and still exits 0 — a scripted install is not expected to answer an
interactive setup flow. For each skipped channel it prints the exact command
that finishes it:

```bash
oap channel create --kind github --name demo-reviewbot-gh    --namespace <ns> --role input
oap channel create --kind slack  --name demo-reviewbot-slack --namespace <ns> --role output
```

(Do not run either with a different `--name` — see "Why `demo-reviewbot-gh`
is not a free choice", below.)

`--role` is not decoration on the slack line. No channel wizard asks for a
role, so a `oap channel create` without one leaves the Channel on
`spec.role`'s `both` default — and a `both` Channel is deliberately not an
output-binding candidate, so the github Channel above would report
`Valid=False` / `ChannelOutputBindingUnresolvable` and reviewbot would never
deliver a review. The role each command needs is the one `requires.channels`
declares, which is why the install prints it for you.

Once wired, opening (or pushing to) a pull request in a repository the App is
installed on delivers a webhook, HMAC-verified against a secret the wizard
generated, and starts (or continues) one AgentSession per pull request — every
new push to the same PR is a new turn in the same session, not a new one, so
the redelivery/dedup logic in the AgentClass's system prompt (step 1: "have I
already reviewed this commit?") has a single Check Run history to read.

**Fork pull requests are not filtered out at the webhook.** They are
deliberately delivered like any other PR — declining an interesting PR is
different from ignoring an uninteresting event, and only the former can be
made visible. The AgentClass's system prompt (step 0, the "fork gate")
recognizes a fork head branch and declines to check out or run its code,
concluding the trigger `could_not_finish` with an explanation, plus a one-line
note to the Slack thread — never a silent drop.

### Why `demo-reviewbot-gh` is not a free choice

The github wizard writes the App's credentials into a Secret named
`<channel-name>-creds` (`pkg/channels/channelkinds/github/wizard.go`'s
`credsSecretName`), and `manifests/agentidentity.yaml`'s `github-app`
credential already points at `demo-reviewbot-gh-creds` — which is exactly why
`oap.yaml` declares this channel's name as `demo-reviewbot-gh` rather than
leaving it to be typed in later. `oap agent lint` cross-checks the two and
fails, naming both strings, if they are ever made to disagree — so a typo in
either place is caught before install, not on the first webhook.

Note also that `oap agent install --name <instance>` is refused outright for
this bundle, unconditionally: an instance rename prefixes every bundled CR but
not a declared channel name, so a second named install would find the first
instance's Channel already wired and bind to it, leaving the second agent
receiving nothing. Always install this bundle without `--name`; to run more
than one instance, give each its own namespace instead of a distinct name —
each keeps the same declared `demo-reviewbot-gh` / `demo-reviewbot-slack`
Channel names, which is fine as long as the namespaces differ.

## The safety boundary: reviewbot cannot write to a repository or comment on a PR

This is enforced by **two independent rings**, and only one of them is where
the real guarantee lives:

1. **The GitHub App's permission set — this is the real, enforced boundary.**
   The App reviewbot's GitHub Channel creates/registers requests exactly four
   permissions (`pkg/channels/channelkinds/github/appprovision/manifest.go`,
   pinned there by an exact-equality test):

   | Permission      | Access  |
   | --------------- | ------- |
   | `contents`      | `read`  |
   | `pull_requests` | `read`  |
   | `metadata`      | `read`  |
   | `checks`        | `write` |

   There is no `contents: write` and no `issues: write` (pull-request comments
   are an Issues-API permission on GitHub). Whatever the inner Claude Code is
   told to do, whatever a malicious pull-request title or body tries to talk
   it into (see the prompt-injection framing in
   `pkg/channels/channelkinds/github/receiver.go`), the App's installation
   token is **structurally incapable** of pushing a commit, merging anything,
   or posting a PR comment. This is what actually survives a successful
   injection attempt — the worst outcome is a wrong Check Run, not a modified
   repository.

2. **The `demo-reviewbot-gh-review` toolspec's CEL constraints — defense in
   depth, not a second independent guarantee.** `manifests/toolspecs.yaml`'s
   `demo-reviewbot-gh-review` toolspec allowlists `gh api`, `pr view`,
   `pr checks`, and `auth status` as distinct read operations, and further
   constrains `gh api` to check-runs and read-only PR/commit endpoints via a
   CEL expression — `gh api` never writes the check run. Check-run history is
   read separately, through the allowlisted `gh pr checks` subcommand, to
   support the diff-since-last-review logic in step 2 of the prompt. `gh api`
   will POST anything the underlying token permits, so this allowlist is not
   a boundary on its own — it narrows what the agent will even attempt,
   sitting *behind* the permission set above, not instead of it. Likewise
   `demo-reviewbot-git-ro` allows only `clone fetch checkout diff log status
   show` — no `push`, `commit`, `add`, `reset`, `tag`, or `remote` — so
   reviewbot's own tooling never offers a write path even though
   `contents: read` would refuse one anyway.

reviewbot's Check Run is written exclusively through the trigger-status seam
(`claim_trigger_status` / `conclude_trigger_status`), never through `gh api`.
The class also opts into `trigger_status: {publishedText: composed}`
(`manifests/agentclass.yaml`'s `spec.capabilities`), which changes what that
seam is even willing to publish: `conclude_trigger_status` takes only the
outcome — `clean`, `problems_found`, or `could_not_finish` — the published
summary is a fixed per-outcome text, and the details link is always the
framework's own artifact-view link, never a model-supplied one. Together with
the App's permission set above (no `issues:write` / `pull_requests:write`)
and the read-only `gh api` constraint, every byte reviewbot can place on
GitHub is enum-derived, provider-derived, or framework-composed — a security
finding in the review cannot leak onto the public pull request, no matter
what the model writes. The findings themselves travel only in the attached
report and the Slack thread.

## How it works

1. A pull request event — `opened`, `synchronize`, `reopened`, or
   `ready_for_review` by default (`spec.github.events`, and draft PRs are
   skipped unless the Channel opts in with `skipDrafts: false`) — delivers a
   verified webhook. reviewbot claims the review with `claim_trigger_status`,
   a no-argument call: the github channel kind owns the Check Run on the head
   commit, so which pull request, which commit, and where the status lives all
   come from the event rather than from anything the agent assembles.
2. It clones the repository read-only (`gitlike_git`) — a **blobless partial**
   clone (`--filter=blob:none`) that keeps the full commit history — and checks
   out the head commit, diffing against a previously reviewed SHA (read back
   from the prior Check Run's `external_id`) when this is a re-review of a new
   push, not the first look at the PR. The workspace is NFS-backed, so a full
   clone of a large monorepo is prohibitively slow (~10 minutes) — but the slow
   part is the file blobs, not the history. `--filter=blob:none` defers each
   file's contents until a diff or checkout reads it, so the clone stays small
   while keeping every commit. That history is load-bearing: the staged review
   skills scope themselves with a mix of two-dot (`<base>..<head>`), three-dot
   (`origin/<base>...HEAD`, via a merge-base) and `git log <range>` — all of
   which need the base branch's history present. A `--depth=1` / `--single-branch`
   shallow clone drops it, the merge-base and three-dot diff fail, and the
   review falls back to reading the whole tree; a blobless full clone avoids that.
3. It drives the inner Claude Code (`codelike_claude`) over the checked-out
   code, following `review-pr` (the default) or `differential-review` (for a
   security-sensitive change — see the AgentClass system prompt for the
   criteria) — or both, for a large pull request that is only partly
   security-sensitive. Because `review-pr`'s own methodology assumes it can
   post inline PR comments and reviewbot cannot (see "The safety boundary"
   below), the prompt redirects any such finding into the artifact report
   instead, with its file:line.
4. It builds an HTML artifact report and delivers the summary to the Slack
   thread (`respond_to_user`) — this is the review, and it happens **before**
   the Check Run is marked complete, so a delivery that never reaches Slack
   can never be recorded as done. The summary opens by @-mentioning the pull
   request's author in Slack, resolved server-side (the author's login and
   display name from `gh pr view --json author`, a commit email GitHub
   attributes to that login from `gh api .../pulls/N/commits`) through the
   `lookup_user_for_mention` tool — falling back to the author's plain login
   when no Slack account matches, never a guessed mention. A requested
   review (asked for in the Slack thread) pings the author the same way.
Alongside the review itself, the framework (not the agent) grants the pull
request's author standing on the session: channelsd derives the author's
numeric GitHub account id from the verified webhook payload, and the operator
writes `agentsession#owner@github_user:<id>#user`. That subject resolves to a
platform user only through the attested identity edge minted when the author
links a verified GitHub credential to their own UserIdentity — until then the
grant is inert, and the moment they link, they can open the session's
artifact-view links (the review report) retroactively, with nothing rewritten.
The policy-resolved owner from the channel wizard remains in place either way;
the author is an additional owner, never a replacement.

The report's audience is deliberately wider than the session's: the class
opts into `spec.authz.session.artifactVisibility: organization`
(`manifests/agentclass.yaml`), so anyone who can authenticate against the
cluster's IdP may open this class's artifact-view links — a review is written
for the whole engineering org, not only the author. That opt-in grants view
of the artifacts and nothing else: the session's conversation, plan, and
send standing stay with the owner, author, and participants above, and
removing the line revokes org-wide access on every session of the class —
finished ones included — at the next reconcile.

5. It concludes with `conclude_trigger_status`, supplying only a judgement —
   `clean` when nothing blocking was found, `problems_found` when something
   was, `could_not_finish` when it could not review at all (fork PR, clone
   failure, wedged tests, budget exhausted). That is the whole call: the
   published summary and details link are composed for it, per the outcome,
   never model-written. The github kind maps the outcome onto the Check
   Run's own conclusion and records the reviewed commit in its
   `external_id`. When the outcome is `could_not_finish`, the reason is
   always posted to the Slack thread as well — the thread, not the check
   run, is where it is visible.

## Deferred / known limitations

- **The `oap-toolchain-claude` image is unbuilt and its `sizeBytes` is
  unverified** — see the warning at the top of this document.
- **`SpiceboxClass` and `SpiceboxToolspec` are both cluster-scoped**
  (`+kubebuilder:resource:scope=Cluster` on both CRDs), so two bundles that
  happen to declare the same class or toolspec name collide at install time —
  whichever installs second silently overwrites the other's definition, no
  error, no warning. This bundle's two classes were originally named
  `gitlike-bundle` / `codelike-bundle`, identical to `examples/codebot`'s own
  classes but with opposite tool access (`demo-reviewbot-git-ro` +
  `demo-reviewbot-gh-review`, read-only, vs. codebot's `git-rw` + `gh-pr`,
  which can push and comment on a PR). Installing codebot after reviewbot on
  the same cluster would have silently handed reviewbot's sandbox codebot's
  write/comment tools — the App's permission set would still have blocked the
  actual write (no `contents: write` / `issues: write`), but the
  toolspec-level ring this README's safety-boundary section describes as
  defense in depth would have quietly disappeared, and failures would have
  surfaced as confusing permission-denied API errors rather than a clean
  refusal. Fixed by prefixing both classes with the agent name
  (`demo-reviewbot-gitlike` / `demo-reviewbot-codelike`). The three
  `SpiceboxToolspec`s had the same exposure — an unprefixed `claude` toolspec
  would collide with any other bundle that also declared a
  `SpiceboxToolspec/claude` — and are now prefixed
  the same way (`demo-reviewbot-git-ro`, `demo-reviewbot-gh-review`,
  `demo-reviewbot-claude`). Prefixing the toolspec CR's own name does not
  change any LLM-facing tool name: that name is
  `<toolBundle.name>_<class-tool-name>` (e.g. `codelike_claude`), assembled
  from fields the toolspec CR name never feeds into. **Any future bundle
  should do the same** for every `SpiceboxClass`, `SpiceboxToolspec` (and any
  other cluster-scoped kind) it declares, rather than reusing a generic name
  another example might also use.
- **No webhook replay dedup.** GitHub retries a failed delivery, and a retry
  whose publish succeeded but whose HTTP response was lost is reprocessed.
  This produces a repeated turn in the same session (bounded by the
  already-reviewed-this-commit check in step 1 of the prompt), not a
  divergent one — annoying under retry storms, not corrupting.
- **The GitHub App manifest sets no Setup URL**, which is why the
  "install the App to your organization" step in the channel wizard is
  paste-the-installation-ID-by-hand rather than a loopback redirect capture.
