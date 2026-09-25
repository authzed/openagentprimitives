# `config/networkpolicy/` — control-plane isolation

Default-deny plus a narrow allowlist for `agentprimitives-system`. Without
these, every pod in the cluster can reach NATS (4222/8222), SpiceDB (50051), and
the operator's debug/memory (8082) and gateway (8443) ports.

Rules are additive: `default-deny` closes the namespace and each sibling policy
reopens exactly one flow.

## Files

| File                   | Purpose                                                                                                                                                                                                                                                               |
| ---------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `default-deny.yaml`    | Denies all ingress + egress for every pod — the allowlist baseline.                                                                                                                                                                                                   |
| `allow-dns.yaml`       | DNS (UDP/TCP 53 to kube-system) for every pod **except** extractord, whose deny-all egress must not be reopened by a namespace-wide rule. `oap install` layers cloud-specific DNS on as `allow-dns-cloud` with the same exclusion (`pkg/platform/cloud/netcidrs.go`). |
| `nats.yaml`            | Ingress to NATS:4222 from channelsd, operator, authzd, webd, runner.                                                                                                                                                                                                  |
| `spicedb.yaml`         | Ingress to SpiceDB:50051 from operator, channelsd, authzd, webd, runner.                                                                                                                                                                                              |
| `operator.yaml`        | Ingress to operator:8082+8443 from channelsd, authzd, webd, runner.                                                                                                                                                                                                   |
| `egress-external.yaml` | Egress for channelsd/operator/authzd/webd: internal flows + external TCP 443.                                                                                                                                                                                         |
| `extractord.yaml`      | Attachment extraction, deny-all egress: ingress on :8080 from the operator only. States `policyTypes: [Ingress, Egress]` with no egress rules rather than inheriting from `default-deny`.                                                                             |

Every component that dials NATS/SpiceDB/operator must appear in the ingress
policies and in `egress-external`.
`TestNetworkPolicyAllowlistsCoverControlPlane` (`pkg/platform/manifests`) fails
on a gap — one that is invisible on a non-enforcing CNI and hangs the component
on an enforcing one.

## Caveats

**A NetworkPolicy-aware CNI is required.** Calico, Cilium, Antrea, Weave, or the
AWS VPC CNI policy add-on enforce these; flannel-without-an-add-on and kindnet
accept the objects and **silently enforce nothing**, leaving the namespace wide
open. Verify before relying on them for isolation.

**Session namespaces get POD-scoped policies, and no default-deny by default.**
These policies are scoped to `agentprimitives-system`. Sessions do NOT live
there and there is no per-session namespace: each session is created in its
**Channel's** namespace, and the built-in webchat uses `default`.

Ingress here does admit them (the `from` peers use `namespaceSelector: {}` plus
the agentsession label), and the AgentSession reconciler stamps the outbound
half per session at provisioning time — runner egress to DNS / NATS / operator /
SpiceDB / its own sidecars / external 443+6443, sandbox and sidecar egress per
`effectiveNetworkMode`. Controlled by the operator's
`--session-network-policies` (default on).

**What that does not cover.** All four per-session builders select on session
labels, so they constrain the runner, the sandboxes and the sidecars — and
nothing else in that namespace. NetworkPolicy is an allow-**union** with no
implicit default, so a pod matching no policy's selector is unrestricted in
**both** directions, even on a fully enforcing CNI. A pod created in a session
namespace by anything else therefore rides no rule at all: that includes a Job
an agent was able to create, and it included this project's own workspace Jobs
until their pod templates were labelled
(`agentprimitives.authzed.com/workspace-job`; the label makes them selectable,
it does not by itself constrain them — write a policy against it, or enable the
floor below).

`--session-namespace-default-deny` (**default off**) stamps a namespace-wide
default-deny into each session namespace. It is the only control that covers a
pod whose labels its creator chose, and it is off by default because an empty
`podSelector` selects every pod in a namespace this operator does not own — in a
shared namespace it would sever workloads unrelated to any agent. Turning it on
is a cluster-operator decision. Where sessions run in a namespace dedicated to
them, turn it on.

Known limitation: runner egress allows only the control-plane peers plus
external 443/6443, so an MCP server on any other port — including the in-cluster
`http://*.svc:<port>` URLs `MCPServerSpec` supports — is blocked on an enforcing
CNI. The symptom is a session-boot MCP probe timeout with no other pointer. Opt
out with `--session-network-policies=false` until per-MCPServer egress rules
land.

## Wiring

Referenced from the top-level `config/kustomization.yaml`, so `mage manifests`
bundles these into `pkg/platform/manifests/install.yaml`. `oap install` applies
that bundle as the first ("base") region — before NATS, SpiceDB, or channelsd —
so default-deny is in place before any control-plane pod starts.

[← config/](../README.md)
