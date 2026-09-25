# `pkg/` — the library tree

Everything the binaries are assembled from. `pkg/` is grouped into
super-packages: a new package joins the group that owns its concern, and adding
a new top-level `pkg/<thing>/` is the exception that needs a reason.

Each group below has its own README describing what it holds and how its
subpackages relate. Substantial subpackages have one too.

| Group | Owns |
| ----- | ---- |
| [`apis`](apis/README.md) | The CRD types. Everything depends on it; it depends on nothing else here. `zz_generated.deepcopy.go` and `config/crds/` come from it via `mage gen:api`. |
| [`agent`](agent/README.md) | The runner: turn loop, harness, LLM providers, session state, and tools as the agent calls them. |
| [`authz`](authz/README.md) | Authorization. The root is backend-neutral; `spicedb/` holds the SpiceDB half. Also guardian, revocation, pinning, and the content/tool guards. |
| [`channels`](channels/README.md) | Transports and the conversation surface: channel kinds, `channelsd`, assets, events, interactions. |
| [`controllers`](controllers/README.md) | The reconcilers, plus webhooks and status/condition helpers. Its `+kubebuilder:rbac` markers generate the operator ClusterRole. |
| [`memory`](memory/README.md) | Storage, ranked search, knowledge graph, and the append-only signed audit kinds. |
| [`platform`](platform/README.md) | Cluster and install substrate: cloud kinds, manifests, identity, workspaces, artifacts, NATS, the lifecycle pipeline. |
| [`tools`](tools/README.md) | Tool definition and execution: toolspec, toolchains, MCP, sandbox kinds, skills. |
| [`web`](web/README.md) | Everything served over HTTP: the browser UI, the admin console backend, the UI packages, the sandbox gateway. |
| [`cli`](cli/README.md) | Shared CLI presentation. `tui` serves `cmd/oap`; `clikit` serves the server binaries. |
| [`gen`](gen/README.md) | Code and doc generators driven by magefiles. |
| [`x`](x/README.md) | Single-purpose utilities with no better home. Read its README before writing a helper — that is what it is for. |

## Where the rest of the code lives

- **`cmd/oap/`** — the user-facing CLI, and the only binary a user runs. Command
  families live under `cmd/oap/internal/<family>cmd/`; subpackages cannot import
  `main`, so shared helpers move down into `internal/`, never up.
- **`internal/cmd/`** — the nine cluster components (operator, runner, channelsd,
  webd, authzd, extractord, apguest, streamclient, leadflowfake). Nobody invokes
  these directly.
- **`test/`** — the suite harnesses (envtest reaping, the SpiceDB container
  fixture, the suite lock) and the e2e scenarios. At the repo root, not under
  `pkg/`.
- **`web/`** — the TypeScript frontend source. Distinct from `pkg/web`, which is
  the Go serving side; `mage web:build` compiles the former into the latter's
  committed `webassets/dist/`.

## Conventions that bite

- **Registries over branching.** Most aspects here are `interface + registry`.
  An `if kind == "x"` outside the kind's own package means the abstraction is
  missing a method. See [AGENTS.md](../AGENTS.md#pluggability-is-a-primary-design-goal).
- **Generated files are marked** in the README of the directory holding them.
  `install.yaml`, `zz_generated.deepcopy.go`, `config/crds/`, `webassets/dist/`
  and `pkg/web/gateway/v1/` are all generated; regenerate, do not hand-edit.
- **`pkg/apis/v1alpha1/agentclass_types.go` is deliberately not gofmt-clean.**
  gofmt rewrites the paired single quotes in its CEL validation marker into
  smart quotes, and the generated CRD then fails to install.

Full rulebook: [AGENTS.md](../AGENTS.md). Project overview: [README.md](../README.md).
