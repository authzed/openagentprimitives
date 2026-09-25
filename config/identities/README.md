# `config/identities/` — the credential namespace

One file, one resource: the `agentprimitives-identities` Namespace
(`v1alpha1.IdentitiesNamespace`). It exists to keep per-user credential material
out of `agentprimitives-system`, so that reading a user's secrets requires a
grant scoped to this namespace rather than to the control plane at large.

What lands here at runtime (created by controllers, not by this directory):

- **Master Secrets** — one per `(UserIdentity, credential)` pair, named
  `<useridentity>-<credential>` (`useridentity.MasterSecretName`).
- **IdP identity Secrets** — the user's captured refresh token, the federation
  subject-token source, named `<useridentity>-idp-identity`.

`UserIdentity` names are a hash of the canonical subject (`u-<48 hex>`), because
a canonical subject can exceed the 63-character DNS-label limit.

`webd` is the main reader/writer here; its grant is
[`../webd/role.yaml`](../webd/role.yaml), scoped to this namespace only.

Nothing here is generated.

[← config/](../README.md)
