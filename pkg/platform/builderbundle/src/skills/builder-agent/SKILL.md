---
name: builder-agent
description: Compose the new agent from everything gathered so far, apply it, and keep fixing it until it reports valid.
---

You are in the Agent phase. Everything you learned in Assess, Tools, and
Permissions now becomes one thing: the new agent itself. You don't need to
ask the person anything new here unless something doesn't fit together —
this phase is about putting the pieces together correctly.

## What to compose

Bring together, from what you already know:

- **A name and a short description** the person would recognize — drawn from
  the goal they confirmed in Assess.
- **A model.** Use the cluster's default unless the brief specifically needs
  something else (a harder reasoning task, a stricter budget).
- **A budget, always.** How much one conversation may spend — turns, tokens,
  wall-clock, and how long a paused conversation is kept (`budget.maxTurns`,
  `maxTokens`, `maxDuration`, `sessionExpiration`) — sized for the brief: a
  quick lookup agent needs far less than a long guided build. Never leave it
  out: an agent with no budget declared cannot report itself valid at all,
  because nothing in the platform supplies one for it, and the only sign is
  a validity check that never changes no matter what you fix.
- **Its own instructions** — a short statement of what this agent is for and
  how it should behave, written for *it*, not for the person. This is not the
  place to restate general good-agent behavior; just the role and the
  specifics this brief called for.
- **Whose access it uses** — the answer Tools settled. Each person acting as
  themselves is `identityMode: userPassthrough`; one shared account is
  `identityMode: agent` with the AgentIdentity you authored named as the
  agent's identity. This is the only place an identity attaches to anything
  — never on a tool.
- **The capabilities the brief implies** — does it need to produce artifacts,
  handle attachments, read channel history? Only include what the brief
  actually asked for.
- **Per-user preferences, when the brief calls for them.** The settings each
  person who uses this agent can set for themselves — for a team agent, this is
  how one person's choices don't quietly become everyone's. Declare them as
  `userPreferences`: for each, a short name, what kind of value it holds
  (yes/no, a whole number, one of a short list, or free text), a sensible
  default, a one-line description in the person's own words, and whether the
  value is private to that person (`visibility: self`) or one the agent may
  also read when it acts for that person by name (`visibility: class`) — a
  "ping me when you're done" opt-out is the classic `visibility: class` case.
  Declaring the list is the whole switch; there is no separate capability to
  turn on. At runtime the agent reads a person's setting with `get_preferences`
  before falling back to a default, and offers to save a new one with
  `set_preference`, which that person approves. A person who uses this agent
  from Slack also gets a settings pane for these automatically — nothing extra
  to build.
- **What "done" looks like for a single response** — the completion
  requirement that matches what the person told you in Assess (a delivered
  artifact, a completed set of steps, a concluded request — whichever shape
  fits).
- **A starter view, only if the brief wants a dashboard or something visual**
  to look at rather than a plain conversation.
- **Its own step-by-step guidance** for the procedures it needs to follow,
  written as this agent's own skills — not shared with any other agent.
- **A roster entry for any agent the brief calls for handing work off to,
  and the capability that lets this agent actually use it.** If the brief
  calls for sending part of the work to an agent reachable from your
  `workshop_agents_in_thread` result, add that agent to the roster by
  name AND turn on the subagents capability — a roster with no capability
  grant means this agent has nowhere to send the delegate tool, so the
  hand-off can never happen even though it looks configured. If the brief
  wants that hand-off kept narrow — a one-shot question rather than an open
  back-and-forth — cap how it may be used with `subagentModes` for that
  agent; leaving it unset allows the usual range. Then call
  `workshop_project_agent` for it before you apply. That projects a
  practice double of the agent — the same instructions and skills, but
  none of its keys and none of its ability to actually do the work — so
  the hand-off itself can be built and tested here, without ever running
  the real agent it stands in for. Add `perm:project:workshop_draft` to
  this phase's `permissions` whenever you do this: it's the approval
  covering `workshop_project_agent` itself, a separate decision from the
  `perm:change:workshop_draft` that covers applying the agent. Either way
  you also declare `workshop_draft` under the phase's `slots` — the plan
  tool's own guidance shows the entry; a handle with no entry beside it is
  refused, and an entry with no handle would approve changing the draft
  without your having asked.

## Apply and fix

List `perm:change:workshop_draft` on this phase's `permissions` before you
start applying, and declare `workshop_draft` under the phase's `slots` in
the same breath: the pair is what covers every `workshop_apply` call below,
retries included, so the person approves composing the agent once for the
whole loop, not once per attempt. Neither half works alone — see the plan
tool's own guidance for the entry.

Call `workshop_apply`. Read what comes back: it reports whether the agent is
valid and whether everything it references checks out. If it isn't, fix
exactly what's named — a missing piece, a reference to something that
doesn't exist, a rule that doesn't parse — and apply again. Keep going until
it comes back valid and fully checked. Don't guess at fixes you can't
explain; if you don't understand a reported problem, say so to the person in
plain terms and reason it through with them rather than trying things at
random.

## Exit condition

The agent reports back as valid, with everything it references checked and
confirmed. Call `update_view` after finishing the phase: refresh the `agent`
hook's settled facts with the composed agent as it now stands, and repaint
the `phase` hook with Test active. Then load the `test` skill and keep going
in this same turn — do not pause and do not call `agent_work_complete` here;
the person is waiting for you to continue, not to stop.
