# How to contribute

## Communication

- Bug Reports & Feature Requests: [GitHub Issues]
- Questions: [GitHub Discussions]

All communication in these forums abides by our [Code of Conduct].

[GitHub Issues]: https://github.com/authzed/openagentprimitives/issues
[Code of Conduct]: CODE-OF-CONDUCT.md
[GitHub Discussions]: https://github.com/authzed/openagentprimitives/discussions

## Creating issues

If any part of the project has a bug or documentation mistakes, please let us
know by opening an issue. All bugs and mistakes are considered very seriously,
regardless of complexity.

Before creating an issue, please check that an issue reporting the same problem
does not already exist. To make the issue accurate and easy to understand,
please try to create issues that are:

- Unique -- do not duplicate existing bug report. Duplicate bug reports will be
  closed.
- Specific -- include as much details as possible: which version, what
  environment, what configuration, etc.
- Reproducible -- include the steps to reproduce the problem. Some issues might
  be hard to reproduce, so please do your best to include the steps that might
  lead to the problem.
- Isolated -- try to isolate and reproduce the bug with minimum dependencies. It
  would significantly slow down the speed to fix a bug if too many dependencies
  are involved in a bug report. Debugging external systems that rely on this
  project is out of scope, but guidance or help using the project itself is
  fine.
- Scoped -- one bug per report. Do not follow up with another bug inside one
  report.

It may be worthwhile to read [Elika Etemad's article on filing good bug
reports][filing-good-bugs] before creating a bug report.

Maintainers might ask for further information to resolve an issue.

[filing-good-bugs]: http://fantasai.inkedblade.net/style/talks/filing-good-bugs/

## Contribution flow

This is a rough outline of what a contributor's workflow looks like:

- Create an issue
- Fork the project
- Create a [feature branch]
- Push changes to your branch
- Submit a pull request
- Respond to feedback from project maintainers
- Rebase to squash related and fixup commits
- Get LGTM from reviewer(s)
- Merge with a merge commit

Creating new issues is one of the best ways to contribute. You have no
obligation to offer a solution or code to fix an issue that you open. If you do
decide to try and contribute something, please submit an issue first so that a
discussion can occur to avoid any wasted efforts.

[feature branch]:
  https://www.atlassian.com/git/tutorials/comparing-workflows/feature-branch-workflow

## Repository conventions

This project's conventions for both human and AI-assisted contributors live in
[AGENTS.md](./AGENTS.md) — package layout, the pluggability rules (registry vs.
dependency injection), common failure modes we've hit in production and their
fixes, and test conventions. Read it before your first substantial change; it is
the canonical rulebook, not just guidance for coding agents.

If your change adds or modifies a `+kubebuilder:rbac` marker or a CRD-shaping
`*_types.go` field, run:

```sh
mage gen:api    # regenerates CRDs, deepcopy, and config/manager/role.yaml
mage manifests  # re-bundles config/ into pkg/platform/manifests/install.yaml
```

both, in that order — a stale `install.yaml` is caught by
`TestInstallYAMLMatchesKustomize`, but only if you remember to run this first.

## Common tasks

We use [mage](https://magefile.org/#installation) to run common tasks in the
project. Install Mage with `go install github.com/magefile/mage@v1.17.2`, then
run it from the repository root with `mage`.

### Testing

In order to build and test the project, the [latest stable version of Go] and
knowledge of a [working Go environment] are required.

[latest stable version of Go]: https://golang.org/dl
[working Go environment]: https://golang.org/doc/code.html

Nothing merges until all three suites are green — see the "Ship gate" section of
[AGENTS.md](./AGENTS.md) for what each one covers and why a green
`go test ./...` alone is not sufficient:

```sh
mage test:unit         # go test -race ./pkg/... ./internal/... ./cmd/... ./test/...
mage test:integration  # -tags=integration + envtest (apiserver/etcd)
mage test:e2e          # -tags=e2e + envtest + a containerized SpiceDB
```

Run `mage -l` for the full list of targets, including narrower suites
(`test:bronze`, `test:steel`, `test:toolspec`, …).

### Formatting

Markdown and TypeScript are formatted by
[oxfmt](https://github.com/oxc-project/oxc), pinned in `magefiles/fmt.go` and
configured in `.oxfmtrc.json` (prose wrapped at 80 columns). CI runs the check,
so run the formatter before opening a pull request:

```sh
mage fmt:all      # rewrite in place
mage fmt:check    # report unformatted files, exits non-zero (what CI runs)
```

Go is formatted by `gofmt` as usual. Generated MDX reference pages, the skill
definitions under `toolkits/`, `**/SKILL.md`, and the built frontend bundles are
deliberately excluded; see `ignorePatterns` in `.oxfmtrc.json`.

### Adding dependencies

This project does not use anything other than the standard [Go modules]
toolchain for managing dependencies.

[Go modules]: https://golang.org/ref/mod

```sh
go get github.com/org/newdependency@version
```

Run `go mod tidy` after adding or removing a dependency; there is no CI check
for this yet, so please run it yourself before opening a pull request.

### Commit messages

This project follows
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) for
commit messages.
