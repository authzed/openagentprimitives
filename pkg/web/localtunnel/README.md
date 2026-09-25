# `pkg/web/localtunnel`

Exposes an in-cluster TCP port as a public HTTPS URL, for clusters that have no
ingress of their own.

```go
type Tunnel interface {
	Start(ctx context.Context, localAddr string) (publicURL string, err error)
	Stop() error
}
```

`Start` blocks until the tunnel is ready or the context cancels, and the
provider keeps that context for the tunnel's whole life. `Stop` is idempotent
and safe from any goroutine, returning an error only on unrecoverable shutdown
failure.

## Selection is a registry

[`registry`](registry) maps a `PublicEndpointSpec.Provider` name to the
`Factory` that builds a `Tunnel` for it, so `pkg/controllers/publicendpoint`
resolves `spec.provider` without branching on its value. Each provider
registers itself from an `init()`; the blank import belongs in the binary that
needs it.

```go
type Factory func(opts Options) localtunnel.Tunnel
```

The factory yields a **fresh** tunnel per call — one per `PublicEndpoint`,
since a shared instance would let two reconciles fight over one provider
session.

| Backend | For |
| ------- | --- |
| [`ngrok`](ngrok) | Production. Reads `Options.AuthToken`, falling back to `NGROK_AUTHTOKEN`. Blank-imported by `internal/cmd/operator`. |
| [`stub`](stub) | Tests. Deterministic, with injectable `URL`/`StartErr`/`StopErr` and `Started()`/`Stopped()`/`LocalAddr()` inspectors. Deliberately never blank-imported into a production binary. |

## Why it exists

A `local` or `desktop` cluster has no ingress, so nothing outside the machine
can reach webd — and a GitHub App whose webhook URL is `127.0.0.1` is one
GitHub refuses outright. A `PublicEndpoint` promotes such a cluster from
locally-reachable to publicly-reachable, and this package is how.

## The tunnel runs in the operator

`pkg/controllers/publicendpoint` holds the live `Tunnel` in the operator
process, keyed by CR name, and publishes the assigned URL on `status.url` —
from which it derives webd's external-URL ConfigMap. `internal/cmd/operator` is
the only binary that links a provider.

The tunnel and the address the cluster advertises therefore have the same
owner and the same lifetime: a reconciler that outlives any command, and that
is still there to correct the published value when the tunnel restarts under
it. Note that the tunnel's traffic reaches webd as a pod-to-pod dial, so it
needs an explicit NetworkPolicy allow on both sides — see
`config/networkpolicy/webd.yaml`.

## `ngrok` details worth knowing

- **If neither `AuthToken` nor `NGROK_AUTHTOKEN` is set, `Start` returns a clear
  error** rather than attempting an anonymous connect.
- Double-start is refused with an explicit error.
- `Stop` closes the forwarder *and* disconnects the agent, **collecting both
  errors** rather than dropping one. A nil-forwarder `Stop` is a no-op.
- A failed forward does a best-effort disconnect and surfaces both errors.
- The free tier grants one static dev domain, so an account supports one open
  tunnel at a time.
