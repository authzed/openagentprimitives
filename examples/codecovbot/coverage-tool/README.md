# Deterministic Go statement coverage

`codecovbot-tool analyze --profile FILE --source DIR` performs static analysis
of an exact checkout, without kubeconfig, network access, builds or source
execution. `DIR` contains the checkout's `go.mod`. Profiles may use
module-qualified or module-relative filenames. Absolute paths, escaping symlinks
and traversal are refused. Concatenated profiles must use the same `set`,
`count` or `atomic` mode. Identical blocks require identical statement counts
and are covered if any input hits them. Duplicate counts are never summed.
Counts weight executable statements, not blocks, functions or branches. A zero
denominator has `percent: null` and `display_percent: "unavailable"`.

The `go-functional-v2` analyzer has no default exclusions: functional equals raw
until an explicit AgentClass policy excludes source blocks. The AgentClass's
`spec.config.coveragePolicyPrompt` defines what is functional for an
installation; there is no platform-wide definition. For example, one class may
exclude plain error propagation, another may retain it, and another may exclude
diagnostic output. The agent interprets that policy; this program validates
source-bound exclusions and performs deterministic arithmetic.

The optional `exclude_pure_error_returns` shortcut recognizes a complete
single-statement block for `if err != nil { return err }` (also simple zero
values before `err`). It runs only when explicitly enabled by the class policy.
`--policy-text` records provenance and does not enable any exclusion. Specify
structured policy with `--policy FILE` or `--policy-json JSON`:

```json
{
  "schema_version": "v1",
  "exclude_pure_error_returns": false,
  "text": "Operator policy"
}
```

For arbitrary class policies, first inspect exact block identities without hits:

```sh
codecovbot-tool describe-blocks --profile coverage.out --source ./source --limit 100
```

Use `--file MODULE_RELATIVE_PATH` to narrow the list, and follow `next_offset`
with `--offset` for later pages. Read the corresponding exact source to decide
eligibility; never choose exclusions from test hits or a target percentage.

Additional reviewed exclusions use `--exclusions FILE` or
`--exclusions-json JSON`, containing:

```json
{
  "schema_version": "v1",
  "exclusions": [
    {
      "file": "unit.go",
      "source_digest": "sha256:HEX",
      "start_line": 3,
      "start_column": 16,
      "end_line": 5,
      "end_column": 3,
      "statements": 1,
      "reason": "Reviewed operator policy reason"
    }
  ]
}
```

The digest is SHA-256 of the complete exact source file bytes. Every manifest
entry must match an existing whole block, digest and statement count. A block
may contain multiple statements only when the class policy excludes all of them;
retain partially eligible or uncertain blocks. Reasons identify the class policy
clause. Duplicates, stale entries, mismatched counts and missing reasons fail.
Both manifest flags and both policy flags are mutually exclusive. JSON rejects
unknown fields and trailing values. Reports are stable JSON with schema `v1`,
analyzer version, policy, policy hash, exclusions hash, raw/functional/excluded
metrics and every exclusion with its source digest, coordinates, count and
reason. The policy fingerprint binds analyzer version and policy. The separate
exclusions fingerprint binds the sorted reviewed manifest; source-dependent
classifications are kept separate so unchanged policy remains comparable across
revisions.

`--base-report FILE --preferred-metric functional|raw --threshold NUMBER` adds a
comparison. The threshold is percentage points; a positive drop at least that
threshold is a regression. Threshold zero means any positive drop. Arithmetic
uses exact rational statement ratios, without rounded percentages or integer
overflow. Missing baselines, incompatible schema/analyzer/policy and zero or
invalid denominators produce `compatible: false`, `regression: false`, and
`drop_pp: null`. Callers must independently authenticate the exact tested base
revision and identical measurement scope/instrumentation before supplying a base
report. Profiles themselves cannot authenticate source or provider provenance.
`--output FILE` writes report JSON; otherwise it goes to stdout.

## Authenticated artifact retrieval

`codecovbot-tool fetch-artifact --repository OWNER/REPO --run-id N --attempt N --artifact-name NAME --output NEW_DIR [--expected-sha SHA]`
uses `GITHUB_TOKEN`. It verifies the provider's run/attempt/repository/head,
selects exactly one nonexpired named artifact from that run, and cross-checks
`metadata.json`:

```json
{
  "schema_version": "v1",
  "repository": "OWNER/REPO",
  "run_id": 123,
  "run_attempt": 1,
  "head_sha": "SHA",
  "tested_sha": "SHA",
  "base_sha": "SHA",
  "profile": "coverage.out",
  "source": "source"
}
```

Head and tested checkout SHA remain separate. `--expected-sha` checks the
PR/source head, using the authenticated run PR association when present;
`run_head_sha` retains the provider run commit independently. A different
synthetic checkout commit requires authenticated source/base merge-parent
evidence. Scope, toolchain, instrumentation, measurement definition/hash and
production/workflow metadata are retained when present. A result includes
provider artifact identity, local profile/source paths and metadata. Credentials
are never forwarded to redirected download URLs. Downloads allow GitHub HTTPS
origins and Azure Blob/Actions signed storage hosts. Archives are limited to 32
MiB compressed, 128 MiB total uncompressed, 32 MiB per entry, and 512 entries.
Extraction rejects absolute/traversing/backslash paths, symlinks, special
entries, duplicates and existing output directories. It writes into a private
staging directory and renames only a fully validated extraction. Downloaded code
is never executed. Metadata assertions beyond the provider cross-checks,
including PR/base/merge associations and comparable scope, remain the
authenticated input handler's/caller's responsibility.
