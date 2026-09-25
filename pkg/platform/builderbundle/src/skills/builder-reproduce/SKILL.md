---
name: builder-reproduce
description: Pick up a conversation that's already happened — read what took place and who else was in it, draft the brief from that instead of an interview, confirm it with the person, then continue into Assess.
---

You are in the Reproduce phase — an alternate way into Assess, loaded when the
person is asking you to do again something that already happened in this same
conversation, rather than describing a new agent from a blank page. Your job
is the same as Assess's: leave with a clear brief. The difference is where the
answers come from — you find them in what already happened, not by asking one
question at a time.

## 1. Read what already happened

Call `read_thread_history` and read back through the conversation before you
arrived. Other agents may have said, in their own words, what they were doing,
what they connected to, and what the person actually asked for — that's the
raw material for the brief, not something you'll get by asking. If you need
context from beyond this one thread — the wider channel it's in — and
`read_channel_history` happens to be available to you, use it as an optional
follow-up; it isn't offered in every setup, so don't count on it.

## 2. Find out who else worked this conversation

Call `workshop_agents_in_thread`. It lists the agents directly bound to this
exact conversation — never any other one — plus, for each, any subagent it
has ever delegated to over its whole history, not only work done here. Treat
the first group as solid evidence of who was actually in this conversation;
treat the second as a weaker hint at what an agent is capable of, not proof
it acted here — a direct participant may have delegated to a helper for
something entirely unrelated. If the call comes back with no agents at all,
say so plainly — this conversation has no other agents recorded — and fall
back to Assess's own questions instead of drafting from nothing. Use whatever
it does return together with what you read in step 1 to piece together how
many agents were genuinely involved here and what each one was actually
doing.

## 3. Draft the brief from what happened

Put together the same brief Assess would: the goal, which services it
touches, where the agent should live, who gets to use it, three to five
example requests in the person's own words, and what "done" looks like. Fill
every field from what you read and found in steps 1 and 2 — draft it, don't
interview for it. Anything the history and the agent list genuinely don't
tell you, set aside as a real gap rather than guessing at it.

Describe what you're proposing as a new agent that does the same work the
earlier conversation did — a self-contained agent of its own, with its own
tools and its own instructions. It may still hand off part of that work to
one of the agents you found in step 2, the same way any brief can call for
handing work to an agent that already exists — if the earlier conversation
had it send work to one of the others, say so in the brief rather than
folding their work into the new agent's own.

When the brief calls for a hand-off like that, the Agent phase projects a
practice double of that other agent with `workshop_project_agent` so the
hand-off can be rehearsed without ever running the real one — the double
behaves like the agent it stands in for but holds none of its keys and
cannot do its actual work. Note which agent(s) need a double projected this
way when you carry the brief forward, so Agent knows to build one.

## 4. Confirm it with the person

Say back the drafted brief in plain language — what you think happened, and
what you're proposing to build from it — and ask them to correct anything
before you move on. Then ask about whatever you set aside as a gap in step 3.
This is the same confirmation Assess would get one question at a time; here
you're checking your reading of what already happened instead.

## Exit condition

The person has confirmed the brief matches what they want reproduced, and any
gaps from step 3 are answered. Move to the Assess skill: its own first steps
already have your draft to work from, so treat anything it would otherwise ask
as something to confirm, not a blank question — only its grounding check and
whatever you flagged as a genuine gap still need a real answer.
