# codecovbot — PR CI coverage reports

This example consumes existing Go coverage artifacts after **all
install-selected PR workflows finish**. It runs a static analyzer, compares the
exact compatible base revision, and publishes its own GitHub Check Run linking
an authenticated HTML report. It never reruns tests or uses Codecov SaaS. Slack,
weekly schedules, coverage goals and cross-repository execution are outside this
flow.

The bundle declares a GitHub input with `workflow_run`, `[completed]` and
`all_completed`. Workflow identities are intentionally absent: the GitHub
channel installer discovers workflows in each selected repository and requires
an explicit nonempty selection. The producer selected by `coverageWorkflow` must
belong to that selection. Use a separate installation when repositories need
different producers/policies. All terminal conclusions settle the gate; failed
coverage CI produces unavailable analysis, never zero coverage. Missing workflow
runs remain deferred before any analysis session or worker starts.

## Install

Use the CLI, CRDs, operator, channelsd, runner, webd and sandbox images built
from this feature together. Bundle format 2 prevents older installers from
silently dropping input-handler configuration. Rebuilding the CLI alone is
insufficient. Run `oap agent lint examples/codecovbot`, then
`oap agent install examples/codecovbot --namespace default` and answer the
GitHub handler questions and the bundle's coverage questions.

Use a dedicated GitHub App/Check Run identity for coverage. The channel wizard
writes `codecovbot-gh-creds`; the AgentIdentity uses that exact Secret. The App
needs repository Contents/Metadata read, Actions read and its own Checks write,
plus `workflow_run` webhooks. Existing Apps may need owner acceptance of Actions
read. Configure a reachable signed HTTPS webhook through supported OAP ingress;
forwarding the message bus is not a webhook test.

The bundle asks for explicit lowercase `owner/repo` repository names, the
coverage producer workflow path, artifact name, policy prompt, preferred metric
(`functional` by default, or `raw`) and absolute percentage-point decrease
threshold. Noninteractive installation must provide equivalent required channel
workflow selections, `session-owner-permission` (for example
`group:ci-operators#member`), and class values. The owner subject set is
explicit because this input may have no conversational output channel. Neither
repository text nor an incoming chat message may override policy or add
repositories. Configure authorized session viewers and explicit PR memory writer
grants; repository ownership is not a memory-access shortcut. Private reports
remain authorized on every request.

The coverage program is owned by this example, in
[`coverage-tool`](coverage-tool/README.md). It is an independent Go module and
Docker image, with no dependency on OAP's Go module or CLI. The bundle's
`requires.images` recipe builds/loads it during install (or pushes it through
the installer's configured registry). Both sandbox classes use that image; no
coverage code, command, or toolchain ships in the OAP install. Git/gh and
artifact fetching use only the repository-scoped App token. The separate
analysis sandbox has no network or credentials. Toolkit/tool specification
contracts allow exact read operations, never API writes, dependency
installation, arbitrary scripts or test execution. Check Run publication uses
the bound trigger-status capability.

Build and test the program independently:

```sh
cd examples/codecovbot/coverage-tool
go test -race ./...
go build ./cmd/codecovbot-tool
docker build -t codecovbot-tool:2.0.0 .
```

The executable provides `codecovbot-tool fetch-artifact` and
`codecovbot-tool analyze`; the agent invokes them through the example's two
SpiceboxToolkits. No `oap coverage` command is needed.

## Notification preferences

Like reviewbot, codecovbot declares a class-visible per-user `notifications`
preference: `always`, `on_problems` (default), or `never`. On a connected
conversational output, the default mentions the PR author only for a comparable
coverage regression. `always` also mentions on clean or unavailable reports;
`never` suppresses mentions. Missing author identity, preference lookup
failures, and unresolved mention tokens fall back to a plain login. The author
is resolved through a provider-attributed non-noreply commit email, as in
reviewbot.

To receive these summaries, add a `role: output` Channel targeting this
AgentClass and configure that channel kind's output destination. It is optional;
when present, workflow sessions bind it for their conversational output.

The Check Run and HTML report are published regardless of this setting. A
GitHub-only installation sends no extra conversational notification. The
preference governs OAP mentions, not GitHub's own notification subscriptions.

## CI evidence contract

The producer uploads one artifact (default `coverage-report`) only after its
selected tests succeed. It contains `metadata.json`, `coverage.out`, a textual
summary and `source/` with the exact measured Go source/module definition. The
profile uses ordinary instrumented Go statement coverage, not branch coverage.

```json
{
  "schema_version": "v1",
  "repository": "owner/repo",
  "pr_number": 1,
  "head_sha": "verified PR head",
  "base_sha": "verified tested base",
  "tested_sha": "exact checkout (possibly synthetic merge)",
  "run_id": 123,
  "run_attempt": 1,
  "workflow_ref": "owner/repo/.github/workflows/coverage.yml@refs/pull/1/merge",
  "produced_at": "RFC3339 timestamp",
  "scope": "./...",
  "instrumentation": "go-cover-v1",
  "toolchain": "go1.26.6",
  "profile": "coverage.out",
  "source": "source",
  "test_result": "success",
  "measurement_definition": {
    "version": 1,
    "command": ["exact", "instrumented", "test", "argv"],
    "env": {},
    "metric": "go-statement-coverage",
    "scope": ["./..."],
    "exclusions": [],
    "toolchains": { "go": "go1.26.6" }
  }
}
```

Provenance is verified against authenticated provider run/PR evidence. A
metadata claim alone is not authority. Downloads use run/artifact IDs and
validate ZIP size, traversal and source/profile paths; downloaded code is never
executed. Multiple job profiles must be merged by identical block
coordinates/counts with coverage combined using OR, never by averaging job
percentages.

Compare only an artifact for the **exact tested base SHA** with compatible
measurement scope, instrumentation/toolchain, analyzer version and policy hash.
Never use arbitrary latest-main coverage. Missing, expired, failed, partial or
incompatible evidence displays current coverage with **baseline unavailable**;
it cannot establish a fabricated delta or replace a successful baseline.

## Metrics and publication

Raw coverage measures every instrumented statement. Functional coverage removes
only the blocks excluded by the install-defined policy, from both covered and
total counts. The definition belongs to
`AgentClass.spec.config.coveragePolicyPrompt`, configured during installation;
the program and platform do not impose a definition.

The example suggests excluding pure error propagation, but other installations
can retain it, exclude diagnostic output, or define their own project policy.
For example:

```yaml
spec:
  config:
    coveragePolicyPrompt: >-
      Retain error handling. Exclude blocks used only for diagnostic logging;
      retain blocks that also validate input, change state, or return a result.
    preferredMetric: functional
    decreaseThreshold: "2"
```

The standalone program defaults to excluding nothing. The agent uses the
optional pure-return recognizer only when explicitly requested by the class.
Other definitions use `describe-blocks` to obtain source-bound identities
without hit counts, inspect exact source, and submit an audited exclusions
manifest. Each exclusion records its digest, exact coordinates, statement count,
and a reason tied to the policy. Multi-statement blocks qualify only when every
statement matches; mixed or uncertain blocks remain included. Unsupported or
ambiguous policies produce unavailable analysis rather than a substituted
metric. The same policy applies to covered and uncovered blocks and to both
revisions.

The deterministic analyzer accepts policy text and optional exclusions, emits
separate raw/functional/excluded counts, policy hash and analyzer version, and
computes comparisons from exact counts. A zero denominator is unavailable. A
regression requires a positive decrease at least the configured absolute
threshold (82% to 80% is 2 points). Display values come from analyzer output.
Changing policy/scope/version starts a new comparable series.

The report includes both metrics, chosen comparison, threshold, policy,
exclusions, source/run/revision links and unavailable-evidence reasons.
`artifact_prepare` creates HTML; `conclude_trigger_status` takes its
`artifact_id`, verifies the claimed revision/cohort and publishes a
framework-owned authenticated details link. Report delivery is recorded only
after the provider succeeds. There is no fake browser binding or required Slack
channel. Superseded results cannot move to a newer PR head. Authorized PR-memory
observations preserve immutable evidence; failed writes remain visible and
failed analysis preserves prior successes.

## Testing an installation

Start with a small Go repository and independent coverage and prerequisite
workflows. Select both during installation, establish successful default-branch
CI, and open PRs that add an uncovered branch and then cover it. Also test a
policy that retains every statement (functional must equal raw), and a custom
policy with exclusions other than error returns. Both public and private
repositories are supported; private repositories exercise App permissions and
report access controls.

Verify no agent session starts before the last workflow settles, one invocation
per latest cohort, reruns and duplicate deliveries, worker restart recovery,
superseded pushes, exact-base compatibility, visible missing-artifact handling,
and report denial for unauthorized viewers.
