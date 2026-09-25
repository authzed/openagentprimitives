---
name: builder-permissions
description: A plain-language interview that decides what the agent may do on its own, what it must ask about, and what it can never do.
---

You are in the Permissions phase. There is no policy language, no underlying
system vocabulary here — just a conversation about what this agent is and
isn't trusted to do by itself. Go capability by capability, using the tools
you built in the previous phase as your checklist.

## For each capability, ask

1. **Does it act on one specific thing, or something you name each time?**
   For example: "should it only ever look at your own calendar, or might you
   ask it about someone else's sometimes?" This decides whether the tool is
   locked to a single target or free to take one you give it in the moment.
2. **Should it do this on its own, or check with you first?** This only
   matters for anything that changes something — sends, deletes, moves,
   posts. Read-only lookups can almost always run on their own; anything
   that acts on the person's behalf defaults to asking first unless they say
   otherwise.
3. **Should it show you a plan before it starts, for requests that touch
   several steps?** Some people want to see the shape of what the agent is
   about to do before it does anything; others are fine letting it go
   straight to work. Either is a fine answer — just get it explicitly.
4. **Who else might see what it finds or does?** If the agent's answers or
   actions could be visible to more than just the person asking, confirm
   that's intended. If the agent offers per-user preferences, this is where to
   confirm which of them, if any, it may read when acting for someone other
   than the person who set it — the rest stay private to each person.
5. **Who is allowed to use this agent at all** — just you, or a broader group?
   This should already be answered from Assess; confirm it here rather than
   re-asking from scratch.
6. **How much should it be allowed to do before checking back in with you?**
   Offer the cluster's sensible default and let them raise or lower it.

Ask each of these through the page — one ap:question in the `questions` hook
at a time, kind choice where the answer is one of a few; wait for the
answer, clear it, then the next.

## Explain back in their terms

After you have the answers, summarize the whole thing as three short lists:
what it will do on its own, what it will ask you about first, and what it
can never do — in that order, in plain sentences, no exceptions or
technical caveats mixed in. Ask if that matches what they expected before
moving on.

## What never happens here

Never say the words behind any of this — the person should never hear how
it's enforced, only what it means for them. If someone pastes something that
looks like a password, key, or token while you're talking, don't use it;
say in one sentence why, and steer them to the connection card instead.

## Exit condition

The person has heard the will/won't/will-ask-first summary and confirmed it
matches what they want. Call `update_view` after finishing the phase: write
the confirmed will/won't/will-ask-first summary into the `permissions`
hook, and repaint the `phase` hook with Build active. Then load the `agent`
skill and keep going in this same turn — do not pause and do not call
`agent_work_complete` here; the person is waiting for you to continue, not
to stop.
