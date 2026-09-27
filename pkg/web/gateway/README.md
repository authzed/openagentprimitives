# `pkg/web/gateway`

A bidirectional gRPC byte-pump that lets an agent runner drive a **live process
inside a session's sandbox pod** — typing into its stdin, reading its
stdout/stderr as it produces them, and learning how it exited.

**This is not the Kubernetes Gateway API, and not grpc-gateway.** It shares
nothing with either. Nothing here terminates ingress, routes HTTP, or transcodes
REST to gRPC. "Gateway" here means one thing: the doorway through which a client
reaches an exec that only the operator can open.

## Why it exists

Only the operator can `exec` into a session's sandbox pod — it holds the RBAC
and the transport binding. But a `ToolCall` CR is a one-shot request/result: it
can carry a command and, later, captured output, but it cannot carry a live
pipe. An interactive CLI tool needs incremental bytes in both directions for
minutes or hours.

So the operator does the exec, parks the live pipes in an in-process registry,
and hands them out over exactly one gRPC stream to the one client entitled to
them.

## Who is on each end

| Role                              | Code                                                                                                                                            |
| --------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| **Serves it**                     | `internal/cmd/operator` — `--gateway-bind-address` (default `:8443`), fronted by the `spicebox-gateway` Service (`config/manager/service.yaml`) |
| **Launches + registers the exec** | `pkg/controllers/toolcall` (`reconcileStreaming` registers; the deletion path calls `CancelAndUnregister`)                                      |
| **Dials it in production**        | `pkg/agent/tool/sandbox` (`Bridge`), running inside `internal/cmd/runner`                                                                       |
| **Dials it for debugging**        | `internal/cmd/streamclient` — claims a stream by hand and writes stdout/stderr/exit to disk                                                     |
| **Exercises it in tests**         | `test/e2e` (`harness.go`, `toolcall_wiring.go`)                                                                                                 |

Setting `--gateway-bind-address=""` disables the server outright. The operator
then publishes an empty `gatewayEndpoint` and streaming ToolCalls have nowhere
to attach.

## End-to-end flow

1. The **runner** mints a random stream token, keeps the raw value in local
   memory, and stamps only its SHA-256 onto `ToolCall.spec.streamTokenHash`
   before creating the ToolCall in `stream` or `interactive` mode.
2. The **ToolCall controller** resolves the session's sandbox executor, starts a
   `StreamExec`, and `Registry.Register`s the live pipes under
   `"namespace/name"` together with that hash.
3. The controller publishes `status.streaming.available` and
   `status.streaming.gatewayEndpoint`. **It never publishes a token.**
4. The **runner's bridge** dials the endpoint and sends
   `Hello{token, namespace, toolCallName}` as its first frame.
5. The **gateway** hashes the presented token, constant-time compares it against
   the stored hash, and claims the stream — at most once, ever. Then it pumps:
   client `stdin` frames into the process, process output back as `stdout` /
   `stderr` frames.
6. The process exits; the gateway sends exactly one `Exit` and ends the RPC. The
   controller's watcher goroutine writes terminal status and unregisters.

## The registry

`Registry` is **not** one of the repo's pluggable `interface + registry` seams
(see `AGENTS.md`). It registers no backends and drives no kind dispatch. It is a
plain in-memory lookup table: `"namespace/name"` → `ActiveStream`.

Each `ActiveStream` holds the ToolCall's key, the token _hash_, the live
`exec.Stream` pipes, and the exec's `context.CancelFunc`.

- **Registered by** the ToolCall controller, once per streaming ToolCall.
- **Read by** the gateway server, via `Claim`, to find the exec a `Hello` names.
- **Removed by** the controller's completion watcher (`Unregister`), or by its
  deletion path (`CancelAndUnregister`, which also stops the exec so a deleted
  interactive ToolCall really ends the live session).

`Claim` **marks** the entry rather than deleting it. Deleting would enforce
single-use too, but it would also put the stream out of reach of
`CancelAndUnregister` — and since the claimer is the runner's own bridge, which
claims the instant it connects, that would silently break idle teardown for
every interactive session.

## Generated code

`v1/` is **generated. Do not hand-edit it.** Regenerate it from `gateway.proto`
with `mage gen:proto`, which runs (from `magefiles/magefile.go`):

```
protoc \
  --go_out=./pkg/web/gateway/v1 --go_opt=paths=source_relative \
  --go-grpc_out=./pkg/web/gateway/v1 --go-grpc_opt=paths=source_relative \
  --proto_path=./pkg/web/gateway \
  pkg/web/gateway/gateway.proto
```

It needs `protoc`, `protoc-gen-go`, and `protoc-gen-go-grpc` on `PATH`. Any edit
to `gateway.proto` — including a comment — is only real once this has run and
`v1/` is committed alongside it.

## Things a newcomer gets wrong

- **The registry does not survive a restart.** It is process-local by design: an
  exec has no reattach story. When the operator restarts, every in-flight
  streaming ToolCall is orphaned, and the controller fails it out with
  `OperatorRestart` rather than leaving it `Running` forever.
- **A claim is single-use.** There is no resume and no reconnect. A dropped
  connection ends that session for good.
- **`stdin` frames only mean something in `interactive` mode.** A `stream`-mode
  ToolCall's stdin was fixed at exec time and is already closed, so writing to
  it fails the RPC.
- **Chunk boundaries are not line boundaries.** `BytesChunk.data` is an
  arbitrary slice of a byte stream; a consumer that needs framing imposes its
  own.
- **The transport is plaintext.** The server is a bare `grpc.NewServer()` and
  clients dial with insecure credentials. Authentication is the bearer stream
  token, and confinement is the per-session NetworkPolicy that permits runner →
  operator on 8443 (`pkg/controllers/agentsession/netpol.go`).
- **The raw token exists in exactly one place**: the runner's memory. It is
  never written to the API server, never logged, and never stored server-side —
  the operator only ever holds its SHA-256.
