# `pkg/agent/session`

Two things a session carries that are not the turn loop itself: the phase state
machine, and the per-session in-memory state the agent can mutate through tools.
The directory holds no Go code of its own.

## Subpackages

- [`lifecycle`](./lifecycle/) — the pure, I/O-free session state machine.
  Events fold into a `State`; transitions emit `Effect`s a caller performs.
- [`state`](./state/) — the per-session in-memory state framework: `Kind`s
  register at `init()`, the runner builds one `Registry` per session, and
  wrapped `system_note` turns replay each `Kind`'s store on resume.

## See also

- [`pkg/agent`](../) — group overview.
