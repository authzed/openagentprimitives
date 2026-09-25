---
name: builder-tools
description: Find, validate, apply, and test a minimal tool for each service the brief needs, trying the least-code path first.
---

You are in the Tools phase. For every service the brief named, you need to
find a way to actually reach it, prove it works, and give the agent only what
it needs — nothing extra. Work through the services one at a time.

## The order to try, every time

For each service, try these in order and stop at the first one that works:

1. **An existing connector.** Call `workshop_probe_mcp` against a known
   address for the service to see what it can already do. This is the
   cheapest path — no new code, just a connection. If the probe fails because
   the service demands login rather than because the address is wrong, don't
   stall waiting to see a tool list before you can act — go build the
   MCPServer candidate now (see "Once you have a candidate" and, for a shared
   account, "Whose access does the agent use?" below) with the address and
   auth you already know; the exact tool list can be filled in once the
   account is connected and you can reach the API for real.
2. **A command-line tool in a sandbox.** If there's no existing connector,
   find an allowlisted command-line tool for the service and call
   `workshop_cli_help` (and `workshop_probe_image` if you need to run the
   image itself to see what it exposes) to learn its interface.
3. **A small adapter we ship.** If neither of those reaches the service, ask
   the person for the service's own API documentation — or, if there isn't
   one, its address plus the handful of operations the brief actually
   needs — and an account or a key if the API requires one. Call
   `workshop_inventory` first: among the small set of helpers this cluster
   allows, look for the one named `ap-api-adapter`, and use the reference it
   reports back exactly as written — never retype it or make one up
   yourself. Then write down, for this one service: the address the API
   lives at; how it proves who's calling — say so even when it needs no
   account at all — naming whichever key or account holds the credential
   (never the credential itself) and — when it rides in a header or in the
   address's own query string — which one; and one entry for each operation
   the brief needs, naming its method, its path, the arguments it takes,
   what kind of value each one is (text, a number, true/false), and where
   each argument belongs (in the path, in the query string, in a header, or
   in the body). Anything the path stands a value in for must appear as one
   of those arguments, marked as always required. For instance: address
   `https://api.example.com`; it authenticates with a token in the
   `X-Api-Key` header, held by whichever account connected it; one
   operation, `list_events` — method GET, path `/v1/events`, with a
   whole-number `limit` argument in the query string. Written out as that
   same configuration, it looks like this:

   ```
   baseURL: https://api.example.com
   auth:
     type: header
     name: X-Api-Key
     envVar: API_KEY
   operations:
     - name: list_events
       description: List upcoming events.
       method: GET
       path: /v1/events
       params:
         - name: limit
           in: query
           type: integer
   ```

   The tools you offer afterward must name exactly those operations, no more
   and no fewer. This has more moving parts than the other two tiers, so run
   `workshop_validate_spec` before `workshop_apply` without skipping ahead —
   it checks this kind of configuration specifically, so a mismatched
   address, a mismatched credential name, or a stray tool name is caught
   before anyone sees an approval card — then `workshop_test_tool` the same
   as above.

Never ask the person for a key, token, password, or account, on the page or
in the conversation. For a shared account the ONE way is
`workshop_request_credential`: a secure card the platform sends. If they try
to paste a secret, tell them in one sentence why you can't use it and send
the card instead.

## Whose access does the agent use?

Settle this before you author any tool, because it decides what you build:

- **Each person acts with their own account** (their own GitHub, their own
  Linear). That is the agent's identity mode, not a credential you author:
  in the Agent phase the agent is set to act as the person
  (`identityMode: userPassthrough`), and each person connects their own
  account the first time they use it. The tool itself only says HOW the
  service authenticates — its auth block's `type` is `oauth`, `static`, or
  `federated` — and names no identity. It must still name the account slot,
  or there is nothing for a person to connect and every call goes out
  unauthenticated: give `auth.credential` the name each person's own linked
  account is stored under, and when `type` is `oauth` give `auth.provider`
  the sign-in they are sent to. Nothing to connect from here, including when
  they try it — so do not author an AgentIdentity for it.
- **One shared account** (a bot, a team key). Author an AgentIdentity that
  carries the credential, call `workshop_request_credential` for it so the
  person connects that account through the secure card, and in the Agent
  phase set it as the agent's own identity (`identityMode: agent`, with that
  identity named on the agent). The tool's auth block again only says how
  the service authenticates.

Whichever answer applies, the sign-in flow you name in `auth.provider` must be
one this cluster actually ships — `workshop_inventory` lists them all, so read
them from there rather than guessing. Never invent a service's own name —
`github` and `linear` are not sign-in flows, and a name inventory does not
list does not exist here.

**For `authKind: oauth-mcp` specifically, order matters:** `workshop_apply`
the MCPServer FIRST — with `spec.auth.type: oauth`, `spec.auth.credential`
naming the same credential you'll pass to `workshop_request_credential`,
and `spec.server.url` set to the service's real address — before you call
`workshop_request_credential`. That MCPServer is what the person's Connect
card discovers OAuth endpoints against; request a card for a credential
with no matching MCPServer yet and the connect link 400s ("No matching
service") when the person clicks it, with no way to tell them why. Apply
it even though you can't fully populate its `tools` yet — the address and
auth block are what the connect flow needs; refine the tool list after the
account connects and you can reach the API for real. `pat`/`static`
credentials have no such requirement — a plain paste has no discovery step
to fail.

**`oauth-mcp` only works when the service actually supports it — not every
shared account does.** It needs the service to both publish its OAuth
endpoints at a discoverable address AND let a brand-new client
self-register; several well-known services support neither, even ones that
otherwise use an OAuth sign-in elsewhere. Slack's OAuth server has no
self-registration endpoint at all — a Slack bot is authenticated with a Bot
User OAuth Token the person pastes, the `slack-bot-token` flow, `authKind:
pat`/`static`, the same as any other API key. GitHub's OAuth apps are the
same story — no discoverable endpoint to register against — so a
GitHub-backed MCP server is NOT an `oauth-mcp` fit despite GitHub using
OAuth sign-in elsewhere; use the `github-pat` flow's personal access token
pasted the same way. When the applied MCPServer's own reachability check
comes back unable to reach or authenticate against the service (look at its
status), or a connect attempt fails naming a missing registration endpoint
or missing OAuth metadata, that is the service telling you `oauth-mcp` will
never work here: switch that credential's `authKind` to `pat`/`static` and
request a token paste instead of trying the same connect kind again.

Nothing on a tool binds it to an identity — the agent's identity mode does
that. Keep the two apart: the tool names the account slot it reads from
(`auth.credential`, and `auth.provider` for an `oauth` sign-in), while the
identity mode decides whose account fills that slot — so naming the slot is
required in both answers above, and is never the thing to drop. If you find
yourself guessing field names to attach an identity to a tool, stop: that
part lives on the agent, in the Agent phase.

## Once you have a candidate

1. Call `workshop_validate_spec` on what you've put together. Fix anything it
   flags before moving on. One rule it cannot see for you, because the agent
   enforces it rather than the tool: every tool entry needs
   `permission.stateImpact` — `stateless` for a read that changes nothing,
   `passthrough` for a read that hands data onward, `external` for anything
   that changes the world. Declare it now, on every tool, or the agent will
   refuse to report itself valid later and send you back here.
   Every tool entry also needs `allowedFields` — the exact argument names the
   brief needs, copied from the schema `workshop_probe_mcp` reported — or
   `unconstrainedArgs: true`, and only for a read-only tool whose arguments are
   free text. A tool with neither refuses every argument sent to it, so leave
   both off only for a tool that takes no arguments at all.
2. Call `workshop_apply` to actually create it. This is the point where a
   human sees the tool before it becomes real — explain in one plain sentence
   what you're about to create and why, before the approval card goes out.
3. Call `workshop_test_tool` with a request drawn from the brief's examples.
   Do this automatically for anything read-only. For anything that changes
   something (sends a message, moves money, deletes a record), get the
   person's explicit okay first, and prefer testing a read-only version of
   the same capability when one exists. One exception: when the agent will act
   as each person with their own account (the per-person answer above), there
   is nothing to test with here yet — no account is linked in the workshop — so
   don't call `workshop_test_tool` for it and don't treat the gap as a failure.
   A tool like that is exercised for real in the Test phase, once the person
   connects their own account and tries the agent themselves.

Write this stage into your plan with `update_plan` and list
`perm:change:workshop_draft` on its `permissions`: that's the approval
covering every `workshop_apply` you make while you're building tools, so the
person decides once for the stage instead of once per tool. In the same
phase, declare `workshop_draft` under the phase's `slots` as well:

    {"type": "workshop_draft", "why": "the draft this stage is building"}

No name goes in it — the draft's is fixed by the workshop and is filled in
for you. You need both: the handle on its own is refused outright, and the
entry on its own would quietly approve changing the draft without your
having asked for it. If you're swapping out a tool definition you already
applied — not adding a new one — call `workshop_delete` on the old one
first, and list `perm:remove:workshop_draft` on the same phase too: taking
something the person already approved off the draft is its own decision,
separate from putting a new one on.

## Keep it minimal

Only give the agent what the brief actually needs. If a service offers fifty
capabilities and the brief needs three, allowlist three. If an argument
should only ever be one particular value or one of a short list, say so when
you author the tool rather than leaving it wide open.

## When a capability seems to be missing

If you've tried all three tiers for a service and none of them can reach what
the brief needs, don't give up on the whole build and don't declare defeat
here. Whether this is truly a gap in the platform — versus a gap in this one
service, or a tool you haven't tried yet — is governed by the `handoff`
skill's own test; that skill decides whether to end the session over it. For
now, note the limitation in the running summary and keep working the rest of
the brief.

## Exit condition

Every capability the brief needs has a minimal tool behind it, and you've
tested each one with at least one real request. Call `update_view` after
finishing the phase: paint the tool list into the `tools` hook (a table
with one row per tool — tool, source, status — and no row action), and
repaint the `phase` hook with Permissions active. Then
load the `permissions` skill and keep going in this same turn — do not pause
and do not call `agent_work_complete` here; the person is waiting for you to
continue, not to stop.
