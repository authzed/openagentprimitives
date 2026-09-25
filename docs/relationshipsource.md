# Syncing a directory into SpiceDB with RelationshipSource

`RelationshipSource` is a namespaced CRD that polls one upstream directory —
Slack, a 1Password SCIM Bridge, or a GitHub (or GitHub Enterprise Server)
organization today — and keeps SpiceDB's view of that directory's membership
current: it writes the relationships the upstream currently reports, and
prunes the ones it no longer reports. It is reconciled continuously, on an
interval, not run once.

Configure one with `oap directory configure` — it walks you through picking
a kind, a credential, an endpoint (when the kind needs one), and that kind's
own questions, then server-side-applies the resulting `RelationshipSource`
CR. `oap init` offers the same flow right after install — see "Configuring
during `oap init`", below. The YAML manifests further down in this document
are kept as **reference** — what the command produces, and what you'd write
by hand with `kubectl apply -f` if you'd rather — they are no longer the
primary set of instructions.

---

## What you need before you configure one

1. An `AgentIdentity` in the same namespace, holding a credential the
   upstream kind can use to read its directory. Create one with `oap
   identity setup` if you don't have one yet — `oap directory configure`
   refuses to run without at least one named credential in the namespace,
   and names that command in its refusal.
2. For a customer-hosted upstream (1Password's SCIM Bridge, or a GitHub
   Enterprise Server install), that upstream's own URL — the command's
   endpoint screen asks for it.
3. Enough OAuth scopes / bridge access to enumerate what you want synced —
   see each kind's section below. Whatever the credential *cannot* see is
   simply invisible to the sync (see "Each kind sees only what its
   credential can see", further down) — it is not an error, and nothing
   tells you a channel or group was skipped for this reason.

## `oap directory list` and `oap directory configure`

```bash
oap directory list
```

Lists every `RelationshipSource` already configured in the namespace and —
always, whether or not anything is configured yet — every directory-sync
kind this build of `oap` has registered, so you know what you could
configure next.

```bash
oap directory configure
```

Walks four questions, in order, over one shared rail:

1. **Kind** — which registered kind to configure (`slack`, `onepassword`,
   `github` today), or "none — configure nothing" as a real, first-class
   answer.
2. **Credential** — which `AgentIdentity`/credential pair in this namespace
   to sync with. Every named credential on every `AgentIdentity` in the
   namespace is offered; this step does not filter by `allowedHosts` — the
   controller enforces that once the source exists (see each kind's
   "Credential" section below).
3. **Endpoint** — `spec.baseURL`. Offered for every kind, not only the ones
   that need it: blank means "the vendor default", and blank is refused
   only for a kind with no default (1Password's SCIM Bridge). GitHub
   defaults to `github.com`; a GitHub Enterprise Server operator sets one
   here even though the kind itself doesn't strictly require it.
4. **Configuration** — whatever the chosen kind asks beyond that. Slack asks
   nothing. 1Password asks nothing beyond the endpoint above (its group set
   is upstream state, not something you enumerate here). GitHub asks for an
   explicit, required list of organization logins — see "`spec.config.orgs`
   is required, and explicit", below; there is no "sync everything this
   token can see" answer. **A correctly configured, fully-synced GitHub
   source still grants nobody anything until people link verified GitHub
   credentials** — this is intended fail-closed behavior, not a bug; see
   "Kind: `github`" below for the full explanation.

Two flags mirror `oap channel create`: `--answer key=value` (repeatable)
pre-seeds one screen's answer, and `--non-interactive` fails at the first
screen whose answer wasn't pre-seeded, instead of prompting for it.

### Re-running against an already-configured kind

`configure` edits the same `RelationshipSource` in place rather than
creating a second one: on a second run it reads what's already there and
prefills every screen from it, and entering straight through — accepting
every default — reproduces the same source. `spec.config` is always
re-derived from the answers you give via the kind's own `BuildConfig`,
never echoed back from the old object, so a byte-identical re-run still
produces a byte-identical server-side apply rather than carrying forward
whatever bytes happened to be stored.

**More than one source per kind is legitimate — github.com alongside a GHES
host, say — and `configure` refuses to touch it.** If more than one
`RelationshipSource` in the namespace already names the kind you picked,
the command stops and names every one of them rather than guessing which
you meant: editing one member of a set it doesn't model would silently drop
the others' fields on a forced apply. Delete or rename down to one before
re-running `configure`, or manage the extras by hand with `kubectl apply
-f`.

**No credential in the namespace → the wizard refuses, rather than writing
a source that never becomes ready.** Configuring a kind with no
`AgentIdentity` credential available in the namespace fails outright and
names `oap identity setup` as the fix, instead of writing a
`RelationshipSource` that would report `Ready=False` forever.

## Configuring during `oap init`

`oap init` offers this same flow right after install, driven by the same
mode as the settings wizard:

| Mode | What happens |
| --- | --- |
| Not a TTY, no flags | Skipped. |
| `--defaults` | Configures nothing. Unlike the settings wizard, `--defaults` does not run this screen at all: there is no recommended set of directories to sync — which orgs, vaults, or workspace to point at is a deployment decision nobody can guess on the operator's behalf, and the credential it would need may not even exist yet. |
| `--wizard` | Runs `oap directory configure` as part of install. |
| Plain TTY, no flags | Offered with a `[Y/n]` prompt; `y` or Enter runs it, anything else skips. |

A failure here — a refusal, a cluster error — warns and does **not** fail
`oap init`; the rest of the install has already succeeded.

---

## Kind: `slack`

`oap directory configure` asks nothing beyond the credential — this kind
has no endpoint and no configuration screen; it syncs whatever channels and
members its bot token reaches.

### Credential

A Slack bot token, delivered as a `static` `AgentCredential` — the same
shape any other Slack bot token uses elsewhere in this codebase.

The bot's OAuth scopes must include:

- **`channels:read`, `groups:read`** — required together. These are what
  `spec.kind: slack` actually needs to enumerate; both are declared by the
  Slack channel kind's `DirectorySync` feature. Missing either one fails
  every `conversations.list` / `conversations.info` / `conversations.members`
  call with `missing_scope`, and the sync writes nothing to SpiceDB.
- **`users:read`, `users:read.email`** — needed to resolve each channel
  member to a platform identity by email, and declared by the same
  `DirectorySync` feature. `users:read.email` is the one to check first when
  a sync reports success and writes nothing: Slack does not refuse
  `users.info` without it, it answers and omits `profile.email`, so every
  member resolves to nobody and is dropped as a join miss.

Add scopes under **your Slack app → OAuth & Permissions → Bot Token Scopes**,
then **Reinstall to Workspace** — or let `oap directory configure` generate an
app manifest that already requests all four, and create the app from it.

### What it enumerates, and what it doesn't

- Walks `conversations.list` (public and private channels the bot is a
  member of, excluding archived channels), then per channel,
  `conversations.members` and `users.info`.
- A **private channel the bot has not been invited to is invisible** —
  there is no way to ask Slack for channels the bot isn't in.
- **Bot members are excluded outright**, not counted as a failure — this
  sync only tracks human channel membership.
- A member with no email on their Slack profile is **dropped, not
  written** (this counts toward the pass's join-miss total, but nothing
  in `status` surfaces that count today — see "Sync status", below).
- Workspace membership (`slack_workspace` → member) is *derived*, not
  independently enumerated: there is no `users.list` call. A workspace
  member who happens to sit in zero channels visible to this bot's token
  is never asserted a workspace member at all.

### Example manifest — reference, what the command produces

This is the shape `oap directory configure` server-side-applies for this
kind (`spec.config` is omitted entirely for slack — its wizard asks
nothing beyond the credential), and what you'd hand-write for `kubectl
apply -f` instead.

```yaml
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: slack-directory-bot
  namespace: default
spec:
  description: "Bot token used only to sync the workspace's channel directory into SpiceDB."
  credentials:
    - name: bot-token
      type: static
      static:
        secretRef:
          name: slack-directory-bot-token
          key: token
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: RelationshipSource
metadata:
  name: slack-directory
  namespace: default
spec:
  kind: slack
  auth:
    agentIdentity: slack-directory-bot
    credential: bot-token
  sync:
    interval: 15m       # optional; 15m is the controller default when omitted
    # maxScopesPerPass is omitted here deliberately — see "maxScopesPerPass"
    # below before you set it.
```

---

## Kind: `onepassword`

Syncs 1Password's provisioned "Managed Groups" against the **SCIM Bridge** —
the customer-hosted service 1Password's own automated-provisioning setup
deploys (to GCP, Kubernetes, or elsewhere). There is no public,
1Password-hosted API this can talk to instead: 1Password's Connect API and
its service-account CLI both explicitly exclude group listing and
membership (`op group`, `op user provision`, etc. are unsupported under a
service account), so the SCIM Bridge is not one option among several — it
is the only surface that can do this.

`oap directory configure` asks for the credential and the bridge's endpoint
(required — see below), and nothing else: this kind's configuration screen
is empty, since the group set to sync is entirely upstream state (1Password's
own "Managed Groups" list, further down), not something you enumerate here.

### `spec.baseURL` is required for this kind

Unlike Slack (a fixed public API host baked into the client library), the
bridge's address is chosen by whoever deployed it — e.g.
`https://scim.example.com`, or an in-cluster Service DNS name like
`http://scim.1password.svc.cluster.local`. Set it on `spec.baseURL`. The
kind refuses to run rather than guess when it's empty.

### Credential

There is no OAuth scope grant for this kind — nothing analogous to Slack's
`channels:read`. The credential is the **bearer token** minted alongside the
bridge's `scimsession` file at setup time, delivered the same way as
Slack's: a `static` `AgentCredential` whose secret holds the token. It is
sent as `Authorization: Bearer <token>`.

**The credential must declare `allowedHosts`, and they must admit your
`spec.baseURL`.** Unlike Slack, whose destination is a constant the kind owns,
`spec.baseURL` is a field anyone who can write the `RelationshipSource` chooses
— and the operator reads the Secret with its own cluster-wide credentials and
sends the token there. Pairing an unscoped credential with a
tenant-chosen destination is credential exfiltration, so the controller refuses
it: set `spec.credentials[].allowedHosts` on the credential to the bridge's
host. A source with no `spec.baseURL` (Slack) is unaffected and needs no scope.

A credential that is unscoped, or scoped somewhere else, surfaces as
`Ready=False` with reason `AuthResolutionFailed` and a message naming what to
set — and the Secret is never read.

What the bearer token can see is controlled entirely on 1Password's side:
an account admin must explicitly add each group to the **"Managed Groups"**
set on the provisioning settings page. A group not added there — and any
group holding the `Recover Accounts` or `Manage All Groups` permission,
which 1Password excludes from automated provisioning outright — is never
returned by `GET /scim/Groups`, and this sync has no way to see it or
report that it exists.

### What it enumerates

`GET /scim/Groups` (paged) to list managed groups, `GET /scim/Groups/{id}`
(paged) for one group's membership, `GET /scim/Users/{id}` to resolve a
member to a primary email. Unlike Slack, there is no intermediate
`onepassword_user` identity resource — a resolved member is written
directly as `onepassword_group#member` with the platform user as the
subject. A member the Users endpoint no longer recognizes, or one with no
email, is dropped and counted as a join miss, same as Slack.

### Example manifest — reference, what the command produces

This is the shape `oap directory configure` server-side-applies for this
kind (`spec.baseURL` set from the endpoint screen, `spec.config` omitted —
this kind's wizard asks no further questions), and what you'd hand-write
for `kubectl apply -f` instead.

```yaml
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: onepassword-bridge
  namespace: default
spec:
  description: "SCIM Bridge bearer token used to sync 1Password's Managed Groups into SpiceDB."
  credentials:
    - name: bridge-token
      type: static
      # Required whenever the RelationshipSource sets spec.baseURL: it scopes
      # the token to the bridge, so a rewritten baseURL cannot redirect it.
      allowedHosts: ["scim.example.com"]
      static:
        secretRef:
          name: onepassword-bridge-token
          key: token
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: RelationshipSource
metadata:
  name: onepassword-directory
  namespace: default
spec:
  kind: onepassword
  baseURL: https://scim.example.com
  auth:
    agentIdentity: onepassword-bridge
    credential: bridge-token
  sync:
    interval: 15m
```

### Joining a synced group to a `group` — without this, the sync grants nothing

The sync writes `onepassword_group:<group id>#member@user:<canonical>`, and the
SpiceDB schema's `group.member` unions `onepassword_group#member` so that
consumers keep checking `group:<name>#member` and never learn which directory
produced a member.

**The tuple that connects the two is not written by anything.** No controller
writes it, and no `oap` command creates it. Until you write it yourself, a fully
synced 1Password group authorizes exactly nothing: the memberships are in
SpiceDB, and nothing checks them.

Write it declaratively with a `SpiceDBBootstrap` CR, one relationship per
`group`-to-1Password-group join:

```yaml
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceDBBootstrap
metadata:
  name: onepassword-group-joins
  namespace: default
spec:
  relationships:
    - resource:
        type: group
        id: engineering
      relation: member
      subject:
        type: onepassword_group
        id: "0123456789abcdef"
        relation: member
```

After this, `group:engineering#member` resolves to every member the sync writes
into `onepassword_group:0123456789abcdef`, and anything already granted on
`group:engineering#member` — an `AgentClass.spec.authz.session.interactPermission` of
`"group:engineering#member"`, say — starts admitting them, with no further
changes.

Two things to get right:

- **`subject.id` is the SCIM group id, not the display name.** It is the `id`
  field of the group's `GET /scim/Groups` entry — the same value the sync uses as
  the SpiceDB object id — and it is generally an opaque string, not
  `"Engineering"`. `kubectl get relationshipsource` will not tell you; read it
  from the bridge, or from the `onepassword_group` objects already in SpiceDB.
- **`resource.id` is yours to choose.** The `group` object is not synced from
  anywhere and does not have to be named after the 1Password group. Several
  1Password groups can be joined into one `group` by adding more relationships.

`group#member` is deliberately claimed by no sync source, which is what lets a
`SpiceDBBootstrap` CR write it (and what keeps a hand-made `group` writable). The
sync owns `onepassword_group#member` and `onepassword_group#relhash` exclusively,
and prunes those — it never touches the join above, so re-syncs leave it alone.

---

## Kind: `github`

Syncs a GitHub (or GitHub Enterprise Server) organization's teams,
repositories, and their collaborator/team grants into SpiceDB, joined to
platform users through the attested `github_user` identity edges.

`oap directory configure` asks for the credential, an optional endpoint
(blank defaults to `github.com`; set it for a GHES install), and one
required configuration question: an explicit, comma-separated list of
organization logins to sync (`spec.config.orgs` — see "`spec.config.orgs`
is required, and explicit", below). There is no "sync everything this
token can see" answer, on the CLI or in the CRD.

**Read this before you conclude a freshly configured sync is broken — it is
almost certainly not, and the two things below are why it looks that way.**

### 1. A repository's roles resolve only for people who have linked a verified GitHub credential

The sync never resolves an identity itself. It writes every role as
`github_repo:<id>#<role>@github_user:<forge account id>#sole_user` — a forge
numeric account id, not a platform user. The join from that account id to an
actual `user:<canonical>` is made entirely separately, by the UserIdentity
reconciler's attested edges, and only for someone who has linked a GitHub
credential AP can verify.

**A sync that is fully `Ready=Synced` and has written every tuple it should
still grants nobody anything until people link.** This is intended
fail-closed behavior, not a defect: there is no safe way to guess which
platform user a forge account belongs to, so the sync simply doesn't try.
Expect this on day one of any rollout — it is the "the sync works but grants
nothing" phase, not a stuck or misconfigured source.

### 2. A second platform subject claiming one GitHub account costs BOTH of them repository access

`sole_user` — the relation every repository role above traverses — is
written only while **exactly one** platform subject is bound to a given
GitHub account, and withdrawn from **both** the instant a second one
appears. This is derived, level-triggered state: every UserIdentity
reconcile pass recomputes the binding count from scratch and writes or
deletes `sole_user` accordingly, so unlinking the extra claimant restores
access automatically — nothing needs to be re-synced or re-approved.

Unlinking the **last** claimant removes access rather than restoring it, and
that direction runs on its own path: a pass recomputes an account only by way
of a credential that names it, so a catalog that has stopped naming an account
sweeps whatever `sole_user` it still holds (and a UserIdentity finalizer gives
a deleted catalog the one pass that sweep needs). Removing the credential, or
deleting the UserIdentity, is therefore enough — there is no separate revoke
step to run.

This is deliberate, not a bug to route around: nothing can tell which of two
claimants legitimately owns the account, so the safe answer is to trust
neither until there is only one again. It does not touch `agentsession`
membership (`github_user#user`), a separate relation this kind never
writes — a duplicate claim can still start and use a session; it just loses
durable repository authority until resolved.

### A `spec.authz.slots` entry on a synced permission is a third, separate reason for the same symptom

Distinct from both of the above, and covered in full under "Four things that
will surprise you" (#4) below: an `AgentClass` that declares a slot on
`github_repo_url`'s `read`, `write`, or `admin` permission does **not** get
forge-backed access through that permission — slot composition replaces the
permission's expression outright, so for a slotted permission the slot grant
and `owner` are what resolve, and the directory sync contributes nothing to
it, however completely and correctly it has synced. Read #4 for the
mechanism and what to do about it; this note exists only so the two
explanations stay consistent rather than duplicating (or contradicting) one
another.

### `spec.config.orgs` is required, and explicit

The organizations to sync are named on `spec.config.orgs` — a plain JSON list
of org logins, e.g. `{"orgs": ["my-org", "my-other-org"]}`. There is no
"sync every org this token can see" mode: the synced set is defined by the
manifest, not by a credential's reach, so a token that later gains
visibility into a new org does not silently start writing tuples for it. An
empty or missing `orgs` list is refused outright, not treated as "sync
nothing" — see the `Ready` reason table below.

Org logins are matched case-insensitively (GitHub's own logins are
case-insensitive on the wire), so `"My-Org"` and `"my-org"` resolve to the
same `github_org` object regardless of which casing you type or GitHub
reports back.

### GitHub Enterprise Server: `spec.baseURL` and the `allowedHosts` it requires

Pointed at `github.com` by default (`spec.baseURL` empty). For a GHES
install, set `spec.baseURL` to its API base, e.g.
`https://ghe.example.com/api/v3`.

The same gate 1Password's `baseURL` triggers applies here, unchanged:
**the credential must declare `allowedHosts` admitting that host**, or the
source refuses with `Ready=False`/`AuthResolutionFailed` and the Secret is
never read. `spec.baseURL` is a field anyone who can write the
`RelationshipSource` chooses, and the operator reads the Secret with its own
cluster-wide credentials, so an unscoped credential paired with a
tenant-chosen destination would be credential exfiltration — the same
reasoning as the 1Password section above, and the same code path
(`credhost.Check`, gated in the controller before the Secret is ever read).
A source with no `spec.baseURL` (plain github.com) is unaffected and needs no
`allowedHosts`.

### Credential

A GitHub personal access token — classic or fine-grained — delivered as a
`static` `AgentCredential`, sent as `Authorization: Bearer <token>`. The
token needs, at minimum:

- **`read:org`** — to list an org's teams and members
  (`GET /orgs/{org}/teams`, `GET /orgs/{org}/members`,
  `GET /teams/{team_id}`, `GET /orgs/{org}/teams/{team_slug}/members`). A
  fine-grained token needs **organization members: read** instead.
- **`repo`** — to list an org's repositories and read their collaborators and
  team access (`GET /orgs/{org}/repos`,
  `GET /repos/{owner}/{repo}/collaborators`,
  `GET /repos/{owner}/{repo}/teams`). A fine-grained token needs
  **repository metadata: read**.

Missing either scope fails the corresponding calls with a `403`, and the
sync writes nothing for the org/team/repo scopes those calls would have
populated — see `channelfeatures.DirectorySync`'s declared requirement in
`pkg/channels/channelkinds/github/kind.go`, pinned against the real call set
by `TestDirectorySyncFeatureDeclaresTheScopesItsCallsNeed`
(`relsync_scopes_test.go`).

### What it enumerates, and the two repository types

Per configured org: the org's own membership (`GET /orgs/{org}/members`),
every team and its direct membership and parent/child nesting
(`GET /orgs/{org}/teams`, `GET /teams/{team_id}`,
`GET /orgs/{org}/teams/{team_slug}/members`), and every repository's direct
collaborators and team-granted access
(`GET /orgs/{org}/repos`, `GET /repos/{owner}/{repo}/collaborators`,
`GET /repos/{owner}/{repo}/teams`).

**There are two repository object types, and that split is deliberate, not
duplication.** A running agent names a repository the only way it can — by
URL, read straight out of a tool call's own arguments — which a permission
check has to resolve locally, with no network round-trip. GitHub names the
same repository by a numeric id, the only part that survives a rename or
transfer. Neither can adopt the other's key:

- **`github_repo_url`** (`toolkits/gh.yaml`), keyed by base64url of the
  canonical `https://github.com/<owner>/<name>` URL. This is the type every
  `gh` toolkit check resolves against, and the one an `AgentClass` grant
  (or slot) names.
- **`github_repo`** (RawZed in `toolkits/gh.yaml`), keyed by GitHub's forge
  numeric repository id. This is the type the sync writes to — the five
  collaborator roles (`admin`, `maintain`, `write`, `triage`, `reader`) and
  the org/team containers they traverse through.

The sync writes exactly one bridging tuple per repository —
`github_repo_url:<b64>#repo@github_repo:<id>` — tying whichever URL an agent
typed to the forge object carrying its actual roles. `github_repo_url`'s
permissions are `owner + repo->can_*`, never `repo->can_*` alone, so a sync
that hasn't run yet, or is stale, can never take away an `owner` grant that
already works.

Only **internal**-visibility repositories get a `#org` edge (so org members
inherit `can_read`); private repositories get none, and public repositories
get no wildcard reader either — a `user:*` arm would arrive as the literal
subject id `"*"` in this codebase's authorization plane and match nobody, so
it is correctly omitted rather than written and silently useless.

### Example manifest — reference, what the command produces

This is the shape `oap directory configure` server-side-applies for this
kind (`spec.config.orgs` from the configuration screen's comma-separated
answer, `spec.baseURL` set only when the endpoint screen wasn't left
blank), and what you'd hand-write for `kubectl apply -f` instead.

```yaml
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: AgentIdentity
metadata:
  name: github-directory-bot
  namespace: default
spec:
  description: "PAT used only to sync org/team/repo structure into SpiceDB."
  credentials:
    - name: org-token
      type: static
      # Only needed when spec.baseURL is set below (GHES); omit entirely for
      # github.com.
      # allowedHosts: ["ghe.example.com"]
      static:
        secretRef:
          name: github-directory-bot-token
          key: token
---
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: RelationshipSource
metadata:
  name: github-directory
  namespace: default
spec:
  kind: github
  # baseURL: https://ghe.example.com/api/v3   # GHES only; omit for github.com
  auth:
    agentIdentity: github-directory-bot
    credential: org-token
  config:
    orgs: ["my-org"]
  sync:
    interval: 15m
```

---

## Reading `status`: is this cycle healthy, stalled, or still running?

`kubectl describe relationshipsource <name>` shows two things: the `Ready`
condition, and `status.sync`.

### `status.conditions[type=Ready]`

| `reason` | Meaning |
| --- | --- |
| `Synced` | The last pass enumerated (fully or partially, if resumed) and ran with no fatal error. This is the healthy steady state — it does **not** mean every scope succeeded; per-scope failures don't flip Ready to False (see below). |
| `KindUnregistered` | `spec.kind` names no kind this binary has registered. Check for a typo, or that this build actually includes the kind you want. |
| `KindClaimed` | Another `RelationshipSource` (in any namespace) already claims this `spec.kind`. Only one `RelationshipSource` per kind is ever active **cluster-wide** — the oldest one (by creation time) wins, and every other CR of the same kind is parked in this state, doing nothing, until you delete or repoint one of them. |
| `AuthResolutionFailed` | The `AgentIdentity`, the named credential, or its backing `Secret` couldn't be resolved. Check `spec.auth` and that the Secret/key exist. Also covers a credential that may not be sent to `spec.baseURL` — unscoped, or scoped to a different host; the message names the credential and what to set. |
| `EnumerationFailed` | The pass enumerated **nothing at all** — zero scopes, incomplete. This is what a revoked token, a `missing_scope` error, or a network outage looks like: distinct from `Synced`, because a source that has never written a single tuple should not read as healthy. |
| `PassFailed` | Reserved for a failure the sync algorithm cannot recover from at all. Not reachable in the current implementation — every anticipated failure folds into one of the reasons above or into a per-scope error instead. |

A single scope failing to fetch (a transient upstream timeout, a rate
limit) does **not** flip `Ready` away from `Synced` — it's logged and
retried next pass, not treated as this source's failure. If you need to see
that kind of thing, it's on the operator monitoring channel your cluster
has wired up (channel events under the `reconcile` category), not in
`status`.

### `status.sync`

| Field | Meaning |
| --- | --- |
| `resumeAfter` | The last scope this pass fully completed, this cycle. Empty means the next pass starts from the beginning. A **non-empty** `resumeAfter` alongside `enumComplete: false` means the cycle is still in progress and will pick up where it left off on the next reconcile — this is normal for a large directory, not stuck. |
| `enumComplete` | Whether the upstream enumeration finished this cycle. Pruning (see below) only ever runs once this is `true`. |
| `cycleStartedAt` | When the in-progress (or most recently completed) cycle began. |
| `lastSyncTime` | The last cycle that ran to completion (`enumComplete: true`). If this is old relative to your sync interval and `resumeAfter` keeps advancing without ever going back to empty, the source has more scopes than one pass's budget can finish — see `maxScopesPerPass` below. |

A pass also does a full re-verify (ignoring its own cached hashes, so it
re-reads and re-diffs every unchanged scope) once every 10 cycles — roughly
every 2.5 hours at the default 15-minute interval. This is automatic; there
is no separate setting for it.

---

## Four things that will surprise you

### 1. The first complete pass is authoritative, and it deletes

The very first time `enumComplete` reaches `true` for a `RelationshipSource`,
the sync sweeps **every object of that kind's resource types that the
credential did not enumerate** — for `slack`, every `slack_channel` object;
for `onepassword`, every `onepassword_group` object. This is not a bug: the
sync engine's whole premise is that it is now the source of truth for what
this credential can currently see.

The practical consequence: if you are pointing a new `RelationshipSource` at
a directory that already has relationships in SpiceDB from somewhere
else — an out-of-tree prototype, a hand-seeded fixture, an earlier manual
bootstrap — **that first complete pass deletes them**, for every object the
new credential's token doesn't currently have visibility into. Expect this
sweep on adoption; it is not data loss you need to recover from, but it
will look like data loss if you don't know it's coming.

One safety valve: if the very first enumeration comes back **completely
empty** (zero scopes) rather than merely small, the pass refuses to prune
anything and reports it as an error instead of trusting that as "this
directory genuinely has nothing." An all-empty read is treated as the
upstream API being unreliable, not as ground truth.

### 2. Each kind sees only what its credential can see

A Slack bot only enumerates channels it has been invited into. A 1Password
bearer token only enumerates groups an admin explicitly added to "Managed
Groups". Neither is a bug in this sync — it's a direct reflection of what
each upstream credential is capable of seeing at all.

Combined with #1, this has teeth: a channel or group the credential loses
visibility into (the bot is removed from a channel; a group is taken out of
Managed Groups) is swept on the next complete pass, exactly as if it had
been deleted upstream. There is no distinction in SpiceDB between "this
channel was archived" and "the bot can no longer see this channel" — both
end the same way.

### 3. `maxScopesPerPass` defers cross-resource reaping — it is not a tuning knob

`spec.sync.maxScopesPerPass` bounds how many scopes one reconcile processes,
so one enormous `RelationshipSource` cannot starve every other one your
cluster is running (they share the same reconcile budget). Leave it unset
unless a source's scope count genuinely requires the cap.

Setting it to anything less than the source's total scope count means the
cross-resource reap — the half that catches an identity or workspace-membership
edge no scope still asserts (e.g. Slack's `slack_workspace` membership,
which rides on every channel scope rather than being its own scope) — can
never run. That reap only fires on a pass that fetched *every currently
enumerated* scope in one go, which a persistent budget smaller than the
source prevents from ever happening again. The practical effect: a member
who has left every channel the bot can see keeps their workspace-membership
edge **indefinitely**, not just until the next full pass. This is a real,
standing trade-off a budget makes — an explicit opt-in for a source too
large to run unbounded, not a default-safe setting to reach for.

### 4. A slot on a synced permission takes the slot's answer, not the directory's

This one is worth reading before you conclude a sync is broken, because the
symptom is the sync appearing to do nothing at all.

A toolkit that consumes synced data declares the union — `gh`'s
`github_repo_url` is the shipped example:

```
permission read  = owner + repo->can_read
permission write = owner + repo->can_write
```

`owner` is the local grant that worked before any sync existed; `repo->can_*`
is the arm that reaches the synced repository. The union is deliberate: a sync
that is stale, half-written, or not running yet must never be able to take away
access that already works.

**An `AgentClass` that declares a `spec.authz.slots` entry on one of those
permissions loses the `repo->can_*` arm.** Slot composition OWNS a slot
permission's expression and rewrites it to
`slot_grant_<permission>->interact + owner`. For that permission, the slot
grant and direct `owner` standing are what resolve, and the directory
contributes nothing — however complete and healthy the sync is.

This is not a bug in the sync, and it is not configurable. Replacing the
expression is what stops an `AgentClass` author widening a slot permission (and
what removes a wildcard leaf from one); the composer cannot presently tell a
toolkit-authored expression, which ships at compile time and is reviewed, from
a tenant-authored one. `repo->can_read` is a widening; it is just a legitimate
one, and it goes with the rest.

What to do about it today:

- **Check whether the permission you care about is slotted.** `kubectl get
  agentclass <name> -o jsonpath='{.spec.authz.slots}'`. The blast radius is
  per-permission: a slot on `read` leaves `write` and `admin` unions intact.
- **If you want forge-backed access on a permission, do not slot it.** Grant
  instance scope through the synced data instead — that is what the sync is
  for — and keep slots for permissions whose instances a human picks.
- **If you want both**, you cannot have them on the same permission yet. The
  fix is provenance-aware composition of a toolkit-authored permission
  expression with a slot.

The erasure is pinned by `TestComposeSlots_ErasesTheGhForgeTraversal`, so if
the composer is ever fixed, that test fails and points back at this section.

---

## Field reference

### `spec`

| Field | Required | Meaning |
| --- | --- | --- |
| `kind` | yes | Which registered upstream to sync: `slack`, `onepassword`, or `github` today. |
| `auth.agentIdentity` | yes | Name of the `AgentIdentity` (same namespace) holding the credential. |
| `auth.credential` | yes | Name of the `AgentCredential` on that identity. |
| `baseURL` | kind-dependent | The upstream's own endpoint, for a customer-hosted upstream (1Password's SCIM Bridge, or a GitHub Enterprise Server install). Ignored by kinds with a fixed vendor host (Slack, github.com). When set, the credential in `spec.auth` must declare `allowedHosts` admitting this host, or the sync is refused — see the 1Password "Credential" section, and the github "GitHub Enterprise Server" section. |
| `config` | kind-dependent | The kind's own configuration, opaque to the CRD. Slack and 1Password need none; github requires `{"orgs": [...]}` — see the github "`spec.config.orgs`" section. |
| `sync.interval` | no | Re-poll cadence. Zero/omitted → 15 minutes. |
| `sync.maxScopesPerPass` | no | See "Four things that will surprise you" (#3) above before setting this. Zero/omitted → unbounded, which is the recommended default. |

### `status`

| Field | Meaning |
| --- | --- |
| `observedGeneration` | The `spec` generation this status reflects. |
| `sync.resumeAfter` | See "Reading `status`" above. |
| `sync.enumComplete` | See "Reading `status`" above. |
| `sync.cycleStartedAt` | See "Reading `status`" above. |
| `sync.lastSyncTime` | See "Reading `status`" above. |
| `conditions[type=Ready]` | See the reason table above. |

`kubectl get relsrc` (short name) or `kubectl get relationshipsources` lists
every `RelationshipSource` with its `Kind`, `Ready` status, and age as
columns.
