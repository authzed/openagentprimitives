---
name: builder-deliver
description: Export the finished agent as a portable draft, and offer to have a platform admin install it for real.
---

You are in the Deliver phase. The person has already told you the agent
looks right — your job now is to make sure they leave with something, and to
find out whether they want it to actually go live.

## Always export the draft

Call `workshop_export_draft`. It bundles everything you've built into a
portable copy of the agent that's theirs to keep, independent of whether
anyone ever installs it for real, and returns a `handle` and an
`artifactId` for that copy. Then, in this order:

1. Call `artifact_await` with the `handle` until it reports ready. If it
   reports a failure instead, tell the person the draft could not be
   packaged, repeat what the failure says, and note that asking to install
   it for real still works from what was exported.
2. Reply with `respond_to_user`, putting the `handle` in `attached`, and say
   plainly what the file is: a saved copy of exactly what they just tested.
3. Paint the `deliver` hook with `update_view` as the outcome: an
   `ap:heading` "<the agent's name> is ready"; one `ap:markdown` line "A
   saved copy of exactly what you tested. Install it to run for real, or
   keep the draft."; the `ap:attachment` whose `artifact` is the
   `artifactId`; then three buttons — `install_for_real` labelled
   "Install for real", `hand_off` labelled "Hand the draft to someone",
   `test_again` labelled "Test it again"; and a closing `ap:markdown`
   line: "Installing asks for your approval and any accounts the agent
   needs. Nothing runs until then." Set the Deliver step's summary to
   "draft saved".

## Offer to install it for real

The three buttons you just painted are the offer — "Install for real" is the
ask, so putting the same question on the page beside them asks it twice. Wait
for the answer with `await_user_message`. Only if the person replies in prose
without choosing one of them, put a single `ap:question` in the `questions`
hook (kind choice: "Install it for real" / "Keep the draft for now") to pin it
down, and clear the `questions` hook once it is answered.

- **If yes:** call `workshop_request_install` with a suggested name for the
  agent and whatever else it asks for. Explain in plain terms what happens
  next — a platform admin reviews the request and installs it, and the
  person will be told once it's live. This is not something you can do
  yourself; it's someone else's decision to make. If the roster includes a
  hand-off you rehearsed with a practice double (`workshop_project_agent`),
  say so plainly: that hand-off only works after install if a real agent of
  that same name already exists wherever this one lands — otherwise that
  part of the finished agent will not work, and the person should know that
  before they confirm.
- **If no, or "not yet":** that's a complete answer. Note that they declined
  for now, and remind them the exported draft is still theirs — they can ask
  for it to be installed later, or hand it to someone else who can.

Either way, record which one happened — don't leave it ambiguous whether an
install was actually requested.

## When the person hands the draft to someone

Say in one sentence that the download beside the draft is the whole agent,
that whoever receives it can ask for it to be installed where they are, and
that nothing about their own accounts travels with it.

## When the person wants to test again

First repaint the `phase` hook with Test active and Deliver back to upcoming:
the `testRun` hook belongs to the Test step and is off the stage until you do,
so without it the person presses "Test it again" and watches nothing happen.
Then load the `test` skill and set the test up again from its step 2; come
back here when they are satisfied. Deliver's step summary reads "draft saved"
throughout.

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

The draft has been exported, and you know — and have recorded in the
`deliver` hook via `update_view`, with Deliver marked done in the `phase`
hook — whether the person wanted it installed for real. If they're done
for now, wrap up in your own words and call `agent_work_complete` only now:
this is the one point in a build where ending the session is right, and
there is no further phase to load from here in the ordinary build.
