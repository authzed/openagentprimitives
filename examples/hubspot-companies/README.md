# hubspot-companies

A read-only HubSpot CRM reporting agent, packaged as a `.oap` agent container.
On a schedule it reports HubSpot companies created inside a window (default 7d,
max 30d) that cleared a minimum fit/readiness score, and tags each company's
assigned owner using whatever native mention syntax the bound channel produces.
Contacts on a named company are gated behind an owner-approval flow.

The system prompt is intentionally channel-agnostic — it names neither Slack nor
any other platform. The runtime injects `lookup_user_for_mention`'s description
based on the Channel the session attaches to.

## Layout

```
oap.yaml                      package manifest (version + required channels)
manifests/
  agentclass.yaml             the AgentClass (system prompt, authz, budget)
  agentidentity.yaml          OAuth identity for the HubSpot MCP server
  mcpserver.yaml              read-only HubSpot MCPServer (tools + SpiceDB schema)
```

`agent.name`, `displayName`, and `description` are inherited from the bundled
AgentClass; the package manifest carries only genuinely package-level fields.

## Install

```bash
oap agent lint    examples/hubspot-companies
oap agent install examples/hubspot-companies --namespace default
```

Install applies the bundled manifests, then drives each required channel's
wizard (bento input + Slack output). It does **not** collect the HubSpot OAuth
credential — that is minted separately by the identity setup flow below, and the
agent inherits the cluster's default model (no per-agent API key).

### HubSpot OAuth credential

```bash
oap agent setup-identity hubspot-companies
```

This runs the `oauth-mcp` browser handshake for the HubSpot MCP server. The CLI
announces the redirect URI it will use; whitelist that URI on your
pre-registered HubSpot OAuth app first (HubSpot → Settings → Integrations), then
paste the app's `client_id` / `client_secret`. The browser opens for the consent
click and the resulting access/refresh tokens are written into the
`hubspot-creds` AgentIdentity. Refresh later without the full flow:

```bash
oap agent setup-identity hubspot-companies --only mcp:hubspot-companies --force
```

### Channels

Install runs each channel's wizard. If you install non-interactively, it prints
the `oap channel create` command to finish each one:

- **bento input** (`hubspot-weekly-digest`): the wizard collects the schedule
  (e.g. `0 9 * * MON`) and the Bloblang mapping whose output becomes the prompt
  (e.g. `What companies were added to HubSpot in the last 7 days?`).
- **slack output** (`hubspot-slack`): the wizard walks Slack-app setup (or paste
  existing `xoxb-…` / `xapp-…` tokens) and the target channel id.

### Verify

```bash
oap tools mcp show hubspot-companies   # Spec, Auth, Tools (4), Observed tools
kubectl get mcpserver hubspot-companies # VALID=True, REACHABLE=True
oap agent show hubspot-companies        # Conditions: Valid=True
```

If the MCPServer is **Reachable=False**, the most common cause is an expired
access token — re-run the `setup-identity … --force` command above.

## What the agent does

On each firing (or a direct message like "what was added in the past 14 days?"):

1. Resolves the time window (default 7d, max 30d). Anything over 30 days is
   refused in a one-line reply quoting the cap.
2. Calls `hubspot_search_crm_objects` for companies created in the window with
   `company_fit_score >= 80` OR `company_lead_score_v2 >= 80` and an assigned
   owner.
3. Resolves the distinct `hubspot_owner_id`s in ONE batch call
   (`objectType: "users"`, filtered on `hubspot_owner_id IN [...]`) to get each
   owner's name + email, then calls `lookup_user_for_mention` by email. As a
   side effect the call's `writesRelationships` block writes
   `hubspot_owner:<id>#user@user:<email>` into SpiceDB — the second hop the
   `contact_access` permission needs.
4. Composes one chat message: a header naming the resolved window, one bullet
   per company, owner rendered inline (`managed by <mention>` on a lookup hit,
   `managed by <owner name>` when known but unmatched, omitted when unknown).

Contacts on a named company run a separate, explicit flow (mode B in the system
prompt): resolve the company, register the owner relationship, then fetch
contacts — which a non-owner can only see after the owner approves.

## Authz

- Ships `authz.toolCalls.mode: permissive` so the agent runs before any SpiceDB
  grants are seeded. The MCPServer's `writesRelationships` blocks build the
  `crm_company → hubspot_owner → user` chain as the agent observes records; once
  those are populated, flip to `enforcing` and every contacts query is gated by
  a `contact_access` Check, with a non-owner query routed to the company owner
  as an approval request.
- `authz.session.interactPermission` names who may interact with the
  cron-spawned digest threads (the bento input has no per-user attribution).
  Replace `group:hubspot-ops#member` with a SpiceDB subject-set that exists on
  your cluster.

## Caveats

- HubSpot's MCP `search_owners` returns no owner email; owner names AND emails
  are recovered from the internal user directory via `search_crm_objects`
  (`objectType: "users"`). Deactivated owners have no `users` record, so their
  companies render with no owner clause.
- The 30-day window cap is enforced both in the prompt (UX) and in the MCPServer
  CR's CEL constraint (`now() - 744h`) — the CEL is the load-bearing limit.
- `search_crm_objects` returns at most 50 results per call; multi-page
  pagination is intentionally out of scope. The agent mentions the cap when it
  hits it.
