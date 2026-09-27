# The terminal escaping seam (`inert.go`, `inert_sink.go`)

Long-form rationale for this kind's inertness sweep. The code carries the
invariants; this file carries the argument behind them.

## Why the surface is dangerous

The `local` kind's surface is an ANSI terminal driven by lipgloss, which
**preserves** ANSI in the strings it renders. Untrusted text handed to the sink
verbatim is therefore not merely displayed — it is _executed_ by the terminal.

Two consequences make this a forged-action defect rather than a cosmetic one,
both of them on the surface a decision is made from:

- `"\x1b[1A\x1b[2K"` (cursor up, erase line), repeated, overwrites the lines the
  consumer authored. The decision modal's keybinding hints are drawn one line
  _below_ the payload text, and a tool block's outcome trailer one line _above_
  the tool's own bytes. Whoever controls the untrusted text controls what the
  user believes those keys do.
- `"\x1b]0;…\x07"` (OSC) sets the terminal **window title**, outside the app's
  frame entirely.

The escape belongs in the kind, for the same reason `escapeSlackText` lives in
the Slack kind: what is dangerous is a property of the **surface**, and the kind
is the one component that knows its surface. A consumer that interpolates these
strings into a frame cannot be expected to re-derive it, and there is more than
one consumer.

The browser surface settles the right treatment rather than leaving it to taste:
it folds these identical `tool_session` bytes through Anser into structured
colour spans precisely because they are untrusted tool output. It keeps colour
and keeps no cursor control at all. This kind draws the same line.

## Why the sweep is a door, not an enumeration

Several successive passes at the inertness class each ended by claiming an
enumeration of its sinks, and each one under-counted. The pass that first gave
this kind a sweep covered four of the six sub-channel senders and left the
operation snapshot and the plan overlay live — the operation snapshot carrying,
on a cleared tick, literally the same string the notification route beside it
was inerting.

An enumeration is the wrong instrument. It is correct only for the set of sinks
that existed on the day it was written, and it fails silently: nothing about
adding a render event, a sub-channel sender, or a field to an existing wire
payload makes anyone open `inert.go`.

So the sweep sits at the boundary every render event must cross anyway.
`local.Host` is the sole construction point of every real sender this kind hands
out, all built from one `Host.sink`, and `NewHost` wraps the caller's
`EventSink` exactly once. From there:

- a new sender is swept, because it is built from the same wrapped sink;
- a new render event _type_ is swept, because the sweep walks **values**, not a
  list of types;
- a new **field** on an existing payload is swept, because it is reached by the
  same walk.

The senders' `sink` field is typed `*inertSink` rather than `EventSink`, so
`&localSender{sink: someRawSink}` does not compile. That is what stops the door
being routed around — by a future sender, and equally by a test, which is how a
guard ends up exercised only on the paths that never needed it.

## Sweeping by shape

Sweeping by shape rather than by a list of known message types is the whole
point: the default for a field nobody has thought about is "swept", so
forgetting is safe. The single exception — a tool's own output, which keeps SGR
— is named in `keepsToolSGR`, and forgetting an entry _there_ loses colour on
one field rather than opening a hole. That asymmetry in the failure direction is
what an enumeration could never give.

The walk works on a copy, and rebuilds containers rather than mutating them in
place, so a sender that still holds the payload it emitted sees its own value
and not a swept one.

`maxSweepDepth` is not about real payloads — the deepest render event this kind
emits is four levels down. It is about a value that points back at itself, which
a shape-driven walk would otherwise follow until the stack ran out and took the
whole TUI process with it. At the limit the value is **zeroed** rather than
passed through: passing it through would hand the terminal a string the sweep
never reached, and a payload nested two dozen levels deep is malformed by any
reading.

A struct from outside this module is treated as a leaf. Its fields are somebody
else's invariants (`time.Time`'s are unexported and not text at all), and
nothing in a render event carries renderable prose inside a foreign type.

## Colour: why `inertText` and `inertToolOutput` differ

`inertText` drops colour. These are payload strings a **publisher** composed for
a surface to lay out — a Lead, a body, a field value — and a publisher has no
way to know what the surface's palette is. Colour arriving in one is therefore
never something a reader asked for, and the decision modal is exactly where a
recoloured line is worth the most to an attacker. Newline and tab stay, because
prose legitimately contains them and stripping them would mangle every real card
to close a hole that neither opens.

`inertToolOutput` keeps SGR. `ToolSessionDeltaPayload.Data` is by contract the
raw bytes the tool emitted, so a coloured test summary or diff is content the
user chose to run and wants to see, while cursor movement, erase, scroll and OSC
are the _surface's_ own controls and are not the tool's to use. Dropping colour
would be a real regression on every ordinary tool run; keeping cursor control
would leave the defect open.

Carriage return goes with the control bytes rather than with newline and tab:
the consumer prefixes every transcript line with its own gutter, and a CR
returns the cursor to column 0, where the tool's next bytes land on top of that
gutter.

`ToolSessionEventPayload.Text` is the parsed leg of the same stream
`ToolSessionDeltaPayload.Data` carries raw, which is why both are in
`keepsToolSGR` and why the event payload's other strings (`ToolName`,
`OuterTool`, `Reason`, `Summary`) are not: those are labels the surface composes
into its own block header. A field renamed out from under `keepsToolSGR` loses
its colour — a visible regression, not a hole — and a test pins the names so it
is caught before a user notices their tool output went monochrome.

## Why the parser comes from `charmbracelet/x/ansi`

What must be dropped is "whatever the terminal would act on", so deciding it
with the same VT parser the render layer uses to measure and interpret these
bytes is what keeps the sweep and the surface from drifting apart. It also
brings the parts a hand-rolled scanner gets subtly wrong: grapheme clusters, C1
controls, the DCS/OSC/APC string terminators, and a sequence left unterminated
at a chunk boundary (dropped, so a repaint split across two chunks cannot be
reassembled from two halves that each looked harmless).

Each dropped sequence is dropped **whole**, printable bytes included. Removing
the ESC and leaving `[1A` behind would flood the transcript of any tool that
redraws a progress line with visible junk, which is its own kind of mangling.

## Ordering: `inertInteractionPayload`

`inertInteractionPayload` is not what makes the interaction sink safe — the Emit
door does that for every event this kind sends. It exists for **ordering**: it
runs before `channelinteractions.RenderText`, which composes markdown around
these slots into the Text floor the timeline prints. Sweeping only afterwards
would let a slot ending in a bare ESC swallow the renderer's own following
characters, and would leave the structured Payload (which the blocking modal
reads field by field) and the rendered Text able to disagree about what has been
neutralised. `inertText` is idempotent, so the door's second pass over these
same strings changes nothing.

Every slot it touches is publisher-supplied, and "publisher" reaches all the way
out to the model: `tool_approval`'s fields carry the summarizer LLM's prose over
agent-controlled tool arguments, and an Excerpt is by contract untrusted content
quoted for a human to look at. Action labels and URLs are swept too — they are
as publisher-authored as the body, and a link action's URL is rendered as text
on this surface.

Category, RequestRef and IDs are skipped there because they are matched rather
than displayed, so pre-rendering does not need them. The door still sweeps them,
and that is the right call: the sweep is a provable no-op on every well-formed
ref (none contains an ESC, a C0/C1 control or DEL), so nothing that correlates
today stops correlating. On a ref that is _not_ well-formed the sweep changes it
and the decision reply fails to match — fail-closed, and strictly better than
carrying a live escape sequence to a terminal on the theory that a malformed
identifier deserves byte-exact preservation.
