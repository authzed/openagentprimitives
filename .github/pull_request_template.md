<!--
Reviewers start from this description. A PR with empty sections, or one
that mixes unrelated work, goes back to its author before anyone reads the
diff.

Agent-authored PRs are welcome. The "Provenance" section tells reviewers
how the change was produced and checked, which shapes how they read it.
-->

## Motivation

<!-- The concrete situation that prompted this: a failing test, a bug seen
     in a running cluster, something the system could not express. Name the
     symptom, not the goal. If it came from a session or an incident,
     describe the steps that led there and what you observed. -->

## Summary of changes

<!-- A short description of the diff itself. Keep the reasoning in
     Motivation. -->

## Alternatives considered

<!-- Other designs you weighed and what ruled each one out. Evidence from a
     prototype or experiment is especially useful. "None" is a fine answer
     when it is true. -->

## Provenance

- Author (person, or model and version):
- Harness or tooling, with version:
- Person who read the diff:

## Ship gate

<!-- AGENTS.md: nothing merges until all three are green. A green
     `mage test:unit` alone is NOT sufficient — integration and e2e are
     behind build tags that `go test ./...` never compiles, so a change can
     leave the whole e2e suite red while unit stays green. Quote the result;
     don't assume. -->

- [ ] `mage test:unit`
- [ ] `mage test:integration`
- [ ] `mage test:e2e`

Results:

```
paste the tail of each run
```

<!-- If a suite flaked: name which tests failed, re-run them in isolation
     (`go test -tags=e2e -run '^TestName$' ./pkg/...`), and say you did. A
     real break fails the same tests every time; contention fails a
     different set each run. Check `uptime` before blaming the change. -->

## Regeneration

<!-- Tick what applies; say "n/a" for the rest. -->

- [ ] Changed a `+kubebuilder:rbac` marker or a CRD-shaping field in
      `pkg/apis/v1alpha1/*_types.go` → ran `mage gen:api` **and**
      `mage manifests`
- [ ] Edited anything under `config/**` → ran `mage manifests`
- [ ] Changed a cobra command or a CRD schema → ran `mage docs:cli` /
      `mage docs:crd`
- [ ] Ran `mage fmt:all` until `mage fmt:check` is clean

## Coverage

- [ ] This adds user-visible behavior, and a bronzethread bundle exercises it
      (`test/e2e/bronzethread/testdata/<name>/`), **or** it does not add
      user-visible behavior
- [ ] This touches authorization, the SpiceDB schema, or the approver model, and
      `mage test:integration` + `mage test:e2e` are green, **or** it touches
      none of those

<!-- Bundles are where this repo's most expensive misses were caught, and
     nothing in the gate output says "you added behavior and no scenario
     exercises it." Assert on the TOOL RESULT, not the agent's reply. -->

## Before requesting review

- [ ] The PR holds a single change. If it has several parts, they cannot land
      independently (explain why below).
- [ ] A person has read every line of the diff, or the note below explains how
      to check the parts they did not.

<!-- Generated or mechanical commits can be impractical to read line by
     line. Name the commit and give a reviewer a way to confirm it, such as
     re-running the generator and checking that the output is identical. -->
