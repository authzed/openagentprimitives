# Private goals

Goals describe outcomes you want an agent to work toward over time. They remain
available across conversations with that agent, so ending a chat does not lose
the goal or mark it complete.

Goals are private to you and the chosen agent. Your administrator must enable
goals for that agent before you can use them.

## Managing a goal

Ask the agent to create a goal with a clear title and desired outcome. You can
review your goals, change their descriptions or due dates, pause work, resume
it, or cancel a goal you no longer need.

A goal starts as a draft and can become active when you are ready. Pausing an
active goal stops scheduled work. A completed or cancelled goal cannot be
reopened; create a new goal if you want to pursue it again.

Completing a goal requires a summary and evidence of the outcome. An agent's
completion report records what it claims to have achieved; it is not independent
proof of success. A successful reminder session also does not automatically
complete the broader goal.

## Scheduling a private reminder

Activating a goal or setting its due date does not authorize the agent to run
later. Scheduling requires your approval of a specific execution or a finite
series of runs.

For example, you can ask:

> Create an active goal to remind me privately to stand up and stretch. Request
> one reminder for tomorrow at 10 a.m. in my timezone, and include the
> scheduling request in your plan for me to review.

The approval shows the goal, timing, private recipient, permitted action,
execution limits, and expected evidence. Review these before approving. The
agent can include several reminder requests in one plan, allowing you to approve
the plan and its listed reminders together. You can open the exact requests
included in that approval for a fuller view.

At the scheduled time, the agent starts a separate private session. That session
creates a fresh action plan before delivering the reminder. By default, you
approve that plan in the new session. Open the session promptly: an unanswered
approval can time out. You can instead explicitly authorize unattended private
delivery when approving the schedule, as described below.

In the browser, a new goal session shows an alert with a link to open it. The
alert stays visible until you open the session or dismiss it, including after
refreshing the page. It leaves your current conversation open.

Changing or pausing the goal invalidates its scheduled authorization. Resuming
it requires a new scheduling request. Denied, cancelled, or expired requests do
not authorize later work.

## Recurring reminders and quiet hours

You can request reminders at regular intervals, daily, or on chosen days of the
week. Choose your timezone, an end date, and a maximum number of reminders. Each
approval covers a finite series of up to 100 scheduled times within 30 days. By
default, each session asks you to approve its fresh action plan before sending.

For example:

> Remind me privately to drink water every weekday at 10 a.m. in New York, for
> the next two weeks. Avoid reminders between 10 p.m. and 8 a.m., and include
> the schedule in your plan for me to review.

Quiet hours use the schedule's timezone. Reminders due during quiet hours move
to the next allowed time; reminders that move to the same time become one
session. A session's window also ends when quiet hours begin, so approving it
late cannot cause a reminder during that period. Runs whose windows are missed
appear as skipped in history, instead of sending a burst of reminders after
downtime.

Daily and weekly reminders follow local clock time when daylight saving changes.
A time that does not exist on a spring-forward day is skipped. A repeated local
time on a fall-back day runs once. The approval's exact details include the
resolved run times for the series.

Changing the schedule or quiet hours requires a new scheduling approval.

## Watching for changes

A goal can also respond to matching events from an available, trusted source.
Approve what to watch, when monitoring ends, and the maximum number of private
reports. A watch does not add access to accounts or authorize other actions.

You can ask naturally, for example, “I have an upcoming flight on FA1234; let me
know if anything changes.” Your agent must have a suitable connected source. It
can discover sources configured for it and ask for missing details, such as the
departure date and when to stop watching. If no suitable source is available, it
should explain that monitoring needs a connection first.

Sources can send updates when something happens or collect them on a schedule. A
scheduled collector goal can also report private observations for another goal
to watch. You approve the collector’s reporting scope and schedule separately
from the notifications. An observation preserves what the collector reported; it
is not independent proof of an airline’s status. Simulated updates are labeled
as test data. Collection does not add access to tools or external services. The
watch reacts to those updates; approving notifications does not itself connect
an account or authorize a source to collect data.

Each matching event creates a separate goal session. You can choose a fresh
approval for each report, or explicitly approve unattended private delivery. The
browser alerts you when the session appears, and its exact instructions remain
available to inspect.

Quiet hours can defer an event report only within its original deadline. Expired
events are skipped. Additional events arriving while a launch is pending are
also skipped; they do not replace its evidence. Duplicate events do not create
extra reports or spend another run. Pause, cancel, or change the goal to stop
the watch; resuming requires renewed approval. Monitoring also stops if source
access is revoked or the source is replaced.

## Unattended private reminders

You can approve private delivery for a finite schedule without approving each
reminder again. Ask the agent to include this choice in the scheduling request:

> Remind me privately to stretch at 10 a.m. every weekday for the next two
> weeks. Ask me to approve the schedule and unattended delivery together, so I
> do not need to approve each reminder when it arrives.

The approval explicitly states that delivery is unattended. It still identifies
the timing, recipient, limits, and end date. Approving an ordinary schedule does
not turn on unattended delivery.

Each run creates a fresh plan and checks that it fits your approval. The session
explains that private delivery is authorized under your approved schedule, and
**View exact instructions** lets you inspect the reviewed terms. This permission
covers private reminders and reports; other actions need separate approval.

You can pause or cancel the goal to stop future work. Changes to the goal or its
schedule require renewed authorization. Delivery also stops if your access or
the agent's approved configuration changes.

## Group goals

A group goal belongs to a multi-person conversation, such as a Slack channel,
instead of one person. Anyone in the group can see its goals, and the agent runs
them on a schedule and posts the results into the group.

Your administrator opts an agent in with `spec.capabilities.goals.groups: true`.
The conversation's channel kind must also host group goals; in a kind that does
not, the agent cannot create one. Group goals require the agent to run with
`identityMode: agent`; other identity modes are refused.

### Members and managers

Each group goal has its own managers. Whoever creates a goal is its first
manager, and managers can add others. A manager who leaves the group stops being
a manager. When no manager is left in the group, platform administrators act as
the fallback.

- **Members** can create goals, list them, and pause them.
- **Managers** can also approve, resume, renew and remove a goal.

### Standing plans

A manager approves a goal once with a standing plan, and that approval covers
each later run whose own plan asks for no more. A standing plan names concrete
resources only, never a wildcard, and allows at most one resource per slot type
per run.

- A run that stays within the standing plan is covered, and is approved
  automatically.
- A run that needs more posts an approval card to the group. Only a manager can
  decide it, and the decision covers that run only. If nobody answers the card,
  the run ends `blocked`.

Each run is its own session and posts into the group. Members can see and talk
in the run's thread, but only managers can approve what it asks for.

While a run holds access to a resource its approved plan names, everyone in the
group counts as able to see that resource. This matters for the check that stops
an agent from posting something to people who could not otherwise see it: for a
group run, posting about those resources into the group passes that check. This
is deliberate. The manager who approved the plan approved posting about those
resources into this group. The access ends when the run's access to the resource
expires or is removed, and it never reaches anyone outside the group.

### Renewal

Consent lasts at most 90 days. The agent offers renewal 72 hours before expiry.
A manager renews with the previous terms prefilled. Once consent expires, the
goal is blocked and does not run until a manager renews it.

### App Home

In Slack, the App Home lists the group goals you can see. Each goal has **View
plan** and **Pause**. **Resume**, **Renew** and **Remove** open a confirmation
modal, and appear for managers, when they apply.

Resume and Renew are approvals: the modal shows the goal and the plan you would
approve, and submitting it is the decision. If the plan changes between opening
the modal and submitting it, nothing is approved, and you are asked to reopen
it.

### Hosting group goals on another kind

A kind that hosts multi-person conversations supports group goals by
implementing `channelkinds.GroupGoalHost`. The goal packages contain no
kind-specific code, and a test (`pkg/agent/goals/genericity_test.go`) keeps it
that way.

## Reviewing a session

A scheduled session opens with a short explanation, such as:

> Session created to meet goal Stretch break: Remind me to stand up and stretch.

Use **View exact instructions** to inspect the complete instructions behind that
summary. The browser and Slack both provide this control.

Newly recorded approval cards retain their review text and approval outcome when
you refresh or reopen the conversation. Older cards may lack enough recorded
history to reproduce their original appearance.

## Results and costs

Goal run history records what happened during each execution, including the
agent's reported result, available delivery evidence, and why the session
stopped. Results remain available after the execution session is cleaned up.

For private browser reminders, recorded delivery means the message was accepted
into your conversation history. It does not prove you read it. If delivery
cannot be confirmed, the history preserves that uncertainty.

Run history can also show an estimated cost. Estimates may be partial or missing
when pricing or accounting is unavailable, and they are not billed amounts.
Unknown cost is not treated as zero. Execution limits bound duration, turns, and
tokens; they are not a dollar spending limit.

## Current scope

Scheduled goals support explicitly approved one-time and recurring private
reminders and reports, with timezone-aware quiet hours. Durable delivery
evidence is supported for private browser replies. External actions and delivery
evidence for other transports remain future work.

### Private suggestions

You can also enable suggestions from a specific source for a limited time. The
approval shows what the agent may look for and how many questions it may ask.
Access to a source alone does not enable suggestions.

When the agent notices something in that scope, it can ask whether you would
like it monitored. Review the proposed outcome, watch, end time and private
reporting limits before approving. The question includes a way to inspect the
exact terms and the evidence behind it. Answering a different conversation with
“yes” does not approve the suggestion.

A suggestion is separate from your goals until you approve it. Declining it or
letting it expire prevents duplicate questions about the same item. Stopping
suggestions prevents new questions; goals you already accepted can still be
paused or cancelled separately. Monitoring never creates missing access or
permission to take other actions.
