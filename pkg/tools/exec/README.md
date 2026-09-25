# exec

The seam through which a tool command runs **inside a sandbox**. `Executor` is
already bound to exactly one sandbox — obtained from that sandbox's backend — so
a `Request` carries argv, env, stdin and timeouts but _no sandbox identity_, and
a bound executor cannot address a different sandbox.

| Package             | Purpose                                                                                                                                                                                    |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| [`remote`](remote/) | The production transport: the Kubernetes `pods/exec` subresource via client-go's `remotecommand`. A `Binder` holds the cluster connection; `For` binds an `Executor` to one pod container. |
| [`fake`](fake/)     | In-process fake for controller and e2e tests. Mirrors `remote`'s `Binder`/`For` shape so the two are interchangeable at construction sites.                                                |

## Constraints

- **`Request.Env` values are secret.** The kubelet encodes `PodExecOptions` —
  argv included — as _query parameters_ on the exec URL, so anything in argv is
  written verbatim to the apiserver audit log, to every proxy in front of it,
  and to `/proc/<pid>/cmdline` inside the sandbox, where a concurrent tool call
  that was never granted the credential can read it. Tool env vars are
  broker-resolved credentials, so [`remote/env.go`](remote/env.go) delivers them
  through a per-call file staged over stdin instead: one extra exec writes an
  `export` script, and the tool's argv is rewritten to source it, unlink it, and
  `exec` the original command — preserving pid, exit status and the three
  streams. That file's residual exposure window is documented at length in
  `env.go`; read it before changing the mechanism.
- Captured stdout/stderr are capped at `MaxStreamBytes` (1 MiB) per stream.
  Overflow bytes are discarded and the corresponding `Truncated` flag is set,
  rather than the buffer growing unbounded.

Part of [`pkg/tools`](../README.md). See the root
[`README.md`](../../../README.md) and [`AGENTS.md`](../../../AGENTS.md).
