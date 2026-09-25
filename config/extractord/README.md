# `config/extractord/` — attachment text extraction

`extractord` is a small, deliberately powerless HTTP service on the inbound
attachment path. `POST /extract` takes raw file bytes and returns
`{"text","pages"}`. It is the one component that parses hostile,
attacker-controlled file bytes, so it is built to be worth nothing if it is
compromised: no egress, no mounted Secret, no Kubernetes API access, and no
projected ServiceAccount token.

## Files

- `serviceaccount.yaml` — the `agentprimitives-extractord` ServiceAccount, with
  `automountServiceAccountToken: false`. The missing Role/RoleBinding is
  deliberate; so is the disabled token projection (an empty RBAC binding does
  not stop the kubelet from mounting a live bearer). **Do not "fix" either.**
- `deployment.yaml` — the Deployment. Mounts only an emptyDir at `/tmp`.
- `service.yaml` — ClusterIP on :8080, dialed by the operator.

Egress is denied by `../networkpolicy/extractord.yaml`, and
`../networkpolicy/allow-dns.yaml` deliberately excludes this pod so the
namespace-wide DNS rule cannot silently reopen it.

Nothing here is generated.

[← config/](../README.md)
