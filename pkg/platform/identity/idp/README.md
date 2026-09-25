# `pkg/platform/identity/idp`

The pluggable **human-login** identity-provider seam. Cluster config (the
singleton `ClusterIdentityProvider` CR) names a kind; the `Kind` constructs a
`Provider`; [`identityd`](../../identityd/) drives `Begin`/`Complete` and
enforces email-verified plus allowed-domain policy on the result.

`contract.go` holds `Config`, `Provider`, `Kind`, `Wizard`, and the optional
`PasswordVerifier` capability. Each kind is a subpackage that self-registers.

## Subpackages

| Package | Kind name | What it is |
| ------- | --------- | ---------- |
| [`oidckind`](oidckind/) | `oidc` | The generic OIDC kind — issuer discovery, code exchange, ID-token verification via go-oidc. Has its own README. |
| [`googlekind`](googlekind/) | `google` | `oidckind` preset with Google's pinned issuer and an `hd=` login hint, plus a server-side re-check of the `hd` claim. The hint is UX; the claim check is the enforcement. |
| [`passwordkind`](passwordkind/) | `password` | A non-federated, password-only local IdP for the single-user macOS desktop's `/admin` console. |
| [`fakekind`](fakekind/) | — | An in-process kind for tests and the e2e harness. |
| [`idpscreens`](idpscreens/) | — | The wizard questions more than one kind asks — which client was registered, its secret, who may sign in — described once. |
| [`registry`](registry/) | — | The process-wide kind registry over `pkg/x/kindregistry`. |

## Constraints

- **`Complete` must verify the ID token** — signature, issuer, audience, nonce.
  Never trust the userinfo endpoint alone. `EmailVerified` mirrors the provider's
  claim; policy enforcement is identityd's job, not the kind's.
- **`ValidateSpec` and `DiscoveryURL` exist so the validity controller has no
  per-kind branching.** A new kind adds its spec validation and readiness probe
  here, once; the controller reaches both through `registry.Get`. A `""`
  `DiscoveryURL` means the kind needs no remote probe.
- **`AllowedNonLocal()` is a structural fence, not documentation.** A kind with
  no external issuer and weak anti-brute-force posture (`password`) returns
  false, and the validity controller refuses it with `Valid=False` on any
  non-local cluster. "Local" comes from the cluster kind `oap install` stamped —
  never from something that merely correlates with it, such as the memory
  backend.
- **`PasswordVerifier` is type-asserted and fails closed.** identityd asserts a
  resolved `Provider` to it at the password-verify endpoint; a missed assertion
  denies rather than falls through.
- A `Wizard` *describes* its questions and reads answers back out of `State`;
  `cmd/oap` owns presentation. Same two-call shape as `channelkinds.Wizard`.
