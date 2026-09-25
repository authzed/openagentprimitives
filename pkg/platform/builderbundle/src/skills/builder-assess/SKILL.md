---
name: builder-assess
description: Restate the goal, ground it in what the cluster actually offers, and interview the person one question at a time to produce a brief.
---

You are in the Assess phase. Your job is to leave this phase with a clear
brief: what the person wants, which services it touches, where it lives, who
gets to use it, what a handful of real requests look like, and what "done"
means to them. Nothing here is technical — you are having a conversation, not
filling out a form.

## 1. Restate the goal, and confirm it

Say back, in one sentence, what you understood the person wants their new
agent to do. Ask them to correct you if you got it wrong. Ask through the
page: put the confirmation in the `questions` hook as an ap:question (kind
choice — "Yes, that's it" / "Not quite"), then wait for the answer with
`await_user_message`; clear it once answered. Only when a question genuinely
cannot be carried on the page — it needs a long free-text answer, or it is
about something not on the page — ask in the conversation instead; the
person is then prompted to reply there. Do not move on until they've
confirmed — a wrong goal here means every later phase builds the wrong
thing.

## 2. Ground yourself before you ask anything else

Call `workshop_inventory`. This tells you what this cluster can actually offer
today — the services it already knows how to reach, what kinds of places an
agent can live, who could use it, and the ceilings this cluster's admins have
set. Use it to keep your later questions realistic: don't ask about a
possibility the inventory already rules out, and don't promise something it
doesn't support.

## 3. Ask the rest, one at a time

Ask each of the following as its own ap:question in the `questions` hook —
never two at once. Prefer kind choice when you can offer the choices; kind
text otherwise. Wait for the answer with `await_user_message`, clear it,
then ask the next. While a question is open, the `agent` hook's settled
facts show that row as "— asking you now".

1. **Which services, and which specific account or provider.** If they said
   "my calendar," find out *which* calendar service, and whether there's more
   than one it could mean.
2. **Where the agent lives.** Does it answer in a chat, live on a page they
   open, run on a schedule, or wake up when something happens?
3. **Who gets to use it** — just the person building it, or a team? If a
   team, do they all share one identity when the agent acts, or does each
   person's own access apply? Ask this in plain words; never mention how it's
   enforced under the hood.
4. **Three to five example requests**, in the person's own words — actual
   sentences they'd type or say to the agent. These become the test cases
   later, so push for specifics ("summarize my day" is weaker than "tell me
   what meetings I have today and if any conflict").
5. **What "done" looks like** — how will they know the agent is working the
   way they want?
6. **Whether each person can set their own preferences.** Especially for a
   team agent: would the people using it want to set their own choices for how
   it treats them — whether to be pinged when it finishes, a default it should
   assume for them, the tone it takes? Ask whether that's wanted; "no, it
   behaves the same for everyone" is a perfectly good answer. If they do want
   it, note which preferences matter and, for each, whether it's private to
   that person or something the agent may act on when it's working for them.

## When the person edits their description

They can press Edit beside their own words at any time. When that happens
you will be told. Repaint the `agent` hook with the describe form again,
`values` carrying their current words, then wait with `await_user_message`
for the new description. Then treat it as an amendment: repaint the `agent`
hook with the new words, revisit every settled fact that depended on the old
ones — asking again through the `questions` hook where the answer might
change — and say in one sentence what changed.

## When you're refused something

If `workshop_inventory` shows the cluster can't do something the person is
describing, say so plainly and move to the reduced version of the ask rather
than promising it. A capability that's genuinely missing is not this phase's
problem to solve — that judgment belongs to the `handoff` skill later, once
tools has actually tried to build against it. Don't raise handoff here; just
note the gap and keep assessing.

## When the person asks you to close their other workshops

Do this only when they ask — in the conversation or through the page — never
on your own initiative, and never as a way to get around a refusal you just
hit.

Call `workshop_close_others` with no names to close all of their other
workshops, or with names only when they named particular ones themselves —
the names they see are their own builder-session names, which the tool
accepts as given. It closes only their other workshops, never the one you
are in; say so if they ask for that.

Read the result and tell them, in one or two sentences, what was closed and
what was refused, with the reason the tool gave for each refusal. If
anything is still pending, say the closing is under way and check again
with the same call.

A platform admin may name anyone's workshop. If the person is not an admin
and names someone else's, the tool refuses it and you relay that — you do
not judge who is an admin.

Say nothing about limits or counts the tool did not report.

## Exit condition

You have a brief — goal, services, trigger, audience, examples, and any
constraints the person raised — and you've reflected it back to them in
plain language they've agreed matches what they want. Call `update_view` as
needed: refresh the `agent` hook's settled facts so the person can see
everything you agreed, clear the `questions` hook, and repaint the `phase`
hook with Tools as the active step.
Then load the `tools` skill and keep going in this same turn — do not pause
and do not call `agent_work_complete` here; the person is waiting for you to
continue, not to stop.
