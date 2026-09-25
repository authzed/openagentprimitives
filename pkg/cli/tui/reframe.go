package tui

// Reframer is a Driver that draws chrome around the groups it presents and can
// hand back a driver drawing a DIFFERENT frame over the same terminal.
//
// It is an interface rather than a method on Driver because most drivers draw
// no chrome at all — the line-oriented one prompts field by field, the
// fail-closed one renders nothing — and a frame is meaningless to them. Making
// every implementation (including the test doubles this repo is full of) answer
// a question only one of them can act on would be a method that mostly returns
// its receiver.
type Reframer interface {
	// Reframe returns a driver presenting over the same terminal, the same
	// stdin buffer and the same answers, but drawing ch around every group
	// with ch's step stepID marked active. An empty stepID keeps each screen's
	// own ID as the rail position, which is the ordinary case.
	Reframe(ch *Chrome, stepID string) Driver
}

// Reframe re-frames a caller-owned driver for a sub-run.
//
// It exists for a command that presents SEVERAL runs over ONE driver and wants
// them to read as one pass. Sharing the driver is not optional — the
// line-oriented driver buffers the stream it reads, so a second driver over one
// stdin loses every answer after the first (see install.PresentOver, which
// exists for exactly that reason) — but a shared driver also carries the chrome
// it was built with, so every run inherits the FIRST run's title and the first
// run's rail. `oap agent install` is the worked example: its bundle questions
// build the driver, and the channels it then wires were framed as more install
// questions, retitled by nobody and railed by nothing.
//
// Merging those runs into one is not available: `oap agent install` interleaves
// a browser round trip and a network resolve BETWEEN a channel's question
// batches, and a single Run held open across that is not a run. So the
// PRESENTATION is shared instead of the run — one Chrome, built once from what
// the caller already knows, handed to each run in turn with a different step
// marked active.
//
// stepID is what makes the rail span the pass rather than restart with it: the
// steps are the CALLER's units of work (a channel, say), while the screens
// presented under them are a kind's own questions, whose IDs the chrome has
// never heard of. Pinning the step is what keeps the highlight on the unit the
// operator is working through instead of resolving to -1 and marking every step
// pending.
//
// A driver that draws no chrome is returned UNCHANGED, and so is a nil ch. That
// is not a silent no-op papering over a failure: the plain and fail-closed
// drivers render no frame at any time, so there is nothing for a frame to
// change — and rewriting the screen ID they report a refusal against WOULD
// change something, for the worse, by naming a caller's unit of work where the
// fail-closed driver's message names the screen nobody answered.
func Reframe(d Driver, ch *Chrome, stepID string) Driver {
	r, ok := d.(Reframer)
	if !ok || ch == nil {
		return d
	}
	return r.Reframe(ch, stepID)
}

// MaxRailLabelColumns caps how much of a step's name the rail shows.
//
// The rail column is sized by its WIDEST label, and Chrome drops the rail
// entirely once it can no longer fit a usable body column beside it
// (Chrome.showsRail) — so one unbounded label costs every step its rail, not
// just its own row. At this cap the rail column is at most 20 columns, which
// leaves showsRail satisfied from 43 columns of terminal upwards.
const MaxRailLabelColumns = 18

// RailLabel cuts s to what the rail can carry. It is exported, and it is the
// ONE answer to "how long may a step label be", because the labels come from
// two unrelated vocabularies — a bundle author's question prompt and a
// declared Channel's name — and the budget belongs to the rail that has to
// draw them rather than to either source.
func RailLabel(s string) string { return Truncate(s, MaxRailLabelColumns) }
