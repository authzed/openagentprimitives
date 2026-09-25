---
name: builder-test
description: Run the finished agent live against a real example, watch what it does, and route any problem back to the phase that owns the fix.
---

You are in the Test phase. The agent applied and validated in the previous
phase now needs to prove itself against a real request, with the person
watching and talking to it directly — not you describing it on their behalf.

## Set up the test

1. Pick one of the example requests from the brief — the actual sentences the
   person gave you in Assess.
2. Call `workshop_test_link` with the agent and that request. Using
   `update_view`, paint the `testRun` hook as a stack: an `ap:status` with
   state `idle` and text "Not started"; the link from the result as an
   `ap:agentlink` with `embed` set, labelled exactly **Start the test**; the
   other example requests as plain text. No buttons yet.
3. Call `workshop_watch_test` with the agent. You will be told, as messages in
   this conversation, when the person starts the test, pauses it, comes back
   to it, ends it, or when the watch times out — one of these six lines,
   verbatim: "The person started testing the agent.", "The person paused the
   test.", "The person resumed the test.", "The test ended.", "The test
   stopped with an error.", or "The test watch timed out." — each followed by
   the time it happened, in parentheses. That time is what goes into the
   status line's `since`; you have no other clock, so never write a time the
   lines did not give you. Tell the person in one sentence that pressing Start
   the test opens a conversation with their new agent right there on the page,
   that it will ask them to connect their own accounts the first time, and
   that you will watch for the time you set. Then wait with
   `await_user_message`.

List `perm:test:workshop_draft` on this phase's `permissions` when you write
it into your plan, and declare `workshop_draft` under the phase's `slots`
beside it — the plan tool's own guidance shows the entry. Together they are
the approval covering `workshop_watch_test`, so watching stays covered
through every pause, resume, and re-arm without a fresh card each time. The
handle alone is refused; the entry alone would approve changing the draft,
which is not what this phase is asking for.

## While it's running: the page says what is true

The `testRun` hook is the source of the test's state. Repaint it at every
transition so its first line is always an `ap:status` that is true right
now; the conversation narrates, the page states — never a question in prose
about whether it is running. The status line's `text` carries no clock: the
moment goes in `since`, copied from the parentheses on the line you were
told, and the page draws it in the person's own reading of the time.

| You are told | The status line | Beneath it |
| --- | --- | --- |
| nothing yet | `idle` — text "Not started", no `since` | Start the test · the example requests |
| "The person started testing the agent." | `running` — text "Running since", `since` the time on that line | call `workshop_test_sessions` to learn the session, then an `ap:chat` whose `sessionRef` is that result's `ref` — the session's whole address, which its `name` alone is not (keep the chat if the page already shows one) · the example requests · an `ap:notice` (tone `info`) saying you are watching, with buttons `test_done` "Done testing", `thats_not_right` "That's not right", and "Stop the test" |
| "The test watch timed out." | `unwatched` — text "Still running; I stopped watching at", `since` the time on that line | the same `ap:chat` · two buttons only: `keep_watching` "Keep watching", `test_done` "Done testing" — no notice, no question |
| "The person paused the test." | `paused` — text "Waiting on you since", `since` the time on that line | the `ap:chat` · the same notice |
| "The person resumed the test." | `running` — text "Running since", `since` the time the test FIRST started — the time on the started line, not the time on this one | the same `ap:chat` · the same notice |
| "The test ended.", "The test stopped with an error.", or Done testing | `ended` — text "Ended at", `since` the time on that line; for Done testing, where no line was sent, the time of the last entry in `workshop_read_test_log` | "N messages, no tools" as the FIRST line of the markdown (N is the number of messages the person sent, counted in `workshop_read_test_log`; name the tools it called, or say "no tools") · then what the agent actually did (below) · a button `start_fresh_test` "Start a fresh test" |

At every row, also repaint the timeline so the Test step's summary states the
row in a few words (running · paused · 3 messages).

When the test ends, or the person clicks Done testing: call
`workshop_test_sessions` to find the session, then `workshop_read_test_log`
and read the real tool calls and results it made — not the words it said
back. Repaint the `testRun` hook as the `ended` row with what the agent
actually did, then summarize it in the conversation and ask — through the
`questions` hook — whether that looked right.

While a test is running never paint a second Start the test: the page
remembers the running conversation and a second start would be a second
agent. "Start a fresh test" is its own button, offered in the `ended` row;
when the person presses it, if a test is still running, read its log and stop
it first as "When something's wrong" says — then set the test up again from
step 2.

An empty log is never proof that nothing happened. A run that was stopped
before it could call anything is the commonest reason there is nothing to
read, and the same result tells you why: `failureReason`,
`failureMessage` and `auditEntries` come back beside `toolRecords`. Report
THAT — the failure message usually names the exact thing to change. Never
end a run whose reason you have not read.

## What a rehearsal with a stand-in proves — and doesn't

If the roster includes a practice double that Agent projected with
`workshop_project_agent`, the run you're watching exercises the hand-off
itself: that the new agent actually sends it work, and handles whatever
comes back. That's real, and worth watching for exactly like any other tool
call.

It does NOT prove the real agent will do the work the double stood in for.
The double holds none of the real agent's keys and none of its actual
capability, so nothing about how well it performs here says anything about
how the real agent will perform once work actually reaches it. A builder
who confuses the two ships an untested delegation — a clean run against a
double is proof the hand-off wiring works, never proof the real agent will
do the work when it's really on the other end. Say this to the person
plainly whenever a stand-in is part of what you're testing, so a clean run
against it isn't mistaken for more than it showed.

## When something's wrong

Before you touch anything, figure out which phase actually owns the fix:

- **A tool errored, or didn't do what it should.** The problem belongs back
  in `tools` — the tool itself, or how it was configured, needs another
  pass.
- **The agent behaved in a way that doesn't match what it was told to do**,
  even though the tools worked fine. That belongs back in `agent` — its
  instructions need adjusting.
- **The agent tried to do something it wasn't allowed to** and got blocked.
  That belongs back in `permissions` — either the rule was wrong, or the
  person needs to hear why it was blocked and decide whether to loosen it.
- **"I don't like this," with nothing more specific.** Ask what they
  expected instead, then route based on their answer using the three cases
  above.

Never try to fix anything while the test is still running. Stopping deletes
the test conversation and its log, so if you have not already read it this
round, call `workshop_test_sessions` and `workshop_read_test_log` first —
before you call `workshop_stop_test`, every time. Once you've read it, stop
with `workshop_stop_test` naming that session — the running conversation
ends, the person is handed back to you, and only then do you load the phase
you identified and make the change. Once it's fixed, come back here and set
up the test again from step 2.

Whichever phase you land in to make that fix, list
`perm:change:workshop_draft` on it again — and declare `workshop_draft`
under the phase's `slots` there too — before you call `workshop_apply`:
reapplying a fix is its own decision the person approves for that phase,
separate from whatever this phase already covered.

If you're about to try a sixth time on the same request without it
succeeding, stop and ask the person whether they want to keep going, take a
different approach, or step back and reconsider a piece of the brief —
don't just keep retrying silently.

## Exit condition

The person tells you, in their own words, that it looks right — whether
that comes through the `test_done` button or directly in the conversation.
Call `update_view` to record the outcome in the `testRun` hook and repaint
the `phase` hook with Deliver active, then load the `deliver` skill and keep
going in this same turn — do not pause and do not call `agent_work_complete`
here; the person is waiting for you to continue, not to stop.
