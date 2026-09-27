# `pkg/authz/spicedb`

The SpiceDB backend half of [`pkg/authz`](../). Everything wire-shaped about an
authorization _decision_ lives here so the root stays backend-neutral — the root
package imports no `authzed-go` at all. This is not the only `authzed-go`
consumer in the group: [`../guardian/grants`](../guardian/grants/),
[`../guardian/approval`](../guardian/approval/) and
[`../relwrites`](../relwrites/) build their own relationship writes against the
same SDK.

All identities are expressed at the canonical user level (`user:<canonicalID>`,
produced by `identity.Principal.Canonical()`). Platform-specific principals — a
Slack user ID, an email — are canonicalized at the channelsd layer _before_ any
call in here. There is deliberately no platform-indirection definition in the
schema: every check goes through SpiceDB at the user level so a divergence
between write-shape and check-shape cannot hide.

| File                                                           | Owns                                                                                                                                                                                                                        |
| -------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`client.go`](client.go)                                       | The wrapped `authzed` client and the channels-specific operations (`TouchStartedBy`, `CheckInteract`, …).                                                                                                                   |
| [`objectid.go`](objectid.go)                                   | Composing SpiceDB object ids from Kubernetes coordinates — and refusing the ones that cannot be composed. See below.                                                                                                        |
| [`lookup_resources.go`](lookup_resources.go)                   | One round trip for "which resources may this subject act on?" — the forward direction `LookupInteractSubjects` does not answer. Lets a session list ask once instead of issuing a fully-consistent check per candidate row. |
| [`schema.go`](schema.go)                                       | **Parses** schema text. (The schema text _itself_ is [`schema/`](schema/).)                                                                                                                                                 |
| [`grant_writer.go`](grant_writer.go)                           | Adapts `*authzed.Client` to the option-free `Writer` surface `guardian/grants` uses. Centralized so the runner and channelsd adapt against the same code.                                                                   |
| [`bootstrap.go`](bootstrap.go), [`envconfig.go`](envconfig.go) | Bootstrap application and `SPICEDB_*` environment configuration.                                                                                                                                                            |

## Subpackages

| Package                   | What it does                                                                                                                     |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| [`schema`](schema/)       | The canonical `.zed` schema text, embedded. Single source of truth.                                                              |
| [`toolcheck`](toolcheck/) | The tool-call authorization gate: request construction, consistency selection, the ZedToken cache, the per-session grant caveat. |

## Object ids: a permanent failure wearing a transient failure's clothes

SpiceDB's `object_id` grammar is **narrower** than a Kubernetes object name's.
An object id must match `^(([a-zA-Z0-9/_|\-=+]{1,1024})|\*)$`; a Kubernetes name
is a DNS-1123 subdomain, which also permits `.`.

So a perfectly legal CR named `support.bot` produces a relationship write
SpiceDB rejects with `InvalidArgument` — forever, for that object's whole life,
since a name cannot be edited. The gRPC error looks like any other write error,
so a caller that requeues on error retries it every backoff interval until the
object is deleted.

**Compose ids through [`objectid.go`](objectid.go).** It makes the impossibility
detectable (`errors.Is` against `ErrUnrepresentableObjectID`) so callers can
surface it instead of spinning.
