// This kind's escaping seam. Its surface is an ANSI terminal driven by
// lipgloss, which PRESERVES ANSI in the strings it renders, so untrusted text
// handed to the sink verbatim is not merely displayed — it is EXECUTED. Cursor
// control can overwrite the very lines a user reads a decision off, and OSC can
// reach outside the app's frame entirely.
//
// WHAT is dangerous lives here; WHERE the sweep runs lives in inert_sink.go —
// one Emit door every render event crosses. See NOTES.md.
package local

import (
	"bytes"

	"github.com/charmbracelet/x/ansi"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
)

// inertText makes a string safe to interpolate into a terminal frame: every
// escape sequence removed, and every control character except newline and tab.
//
// Colour is dropped here, unlike inertToolOutput — these are publisher-composed
// payload strings the SURFACE lays out, so colour in one is never something a
// reader asked for. Newline and tab stay because prose legitimately contains
// them. See NOTES.md.
func inertText(s string) string {
	if s == "" {
		return s
	}
	return string(stripTerminalControls([]byte(s), false))
}

// inertToolOutput makes a tool's raw output chunk safe to append to the
// terminal transcript, KEEPING SGR (colour and decoration) and dropping every
// other escape sequence and control byte.
//
// The asymmetry with inertText is the whole judgement call: a tool's raw bytes
// are content the user chose to run, while cursor movement, erase, scroll and
// OSC are the SURFACE's controls and not the tool's to use. Carriage return
// goes with the controls — the consumer prefixes every transcript line with its
// own gutter, and a CR would land the tool's next bytes on top of it.
//
// A chunk that kept any SGR is terminated with an explicit reset, so an
// unclosed colour run cannot bleed onto the platform-authored lines that follow
// it. See NOTES.md.
func inertToolOutput(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	return stripTerminalControls(b, true)
}

// stripTerminalControls is the one sweep both entry points share: it walks the
// input one terminal sequence at a time and keeps text, keeping SGR too when
// keepSGR is set.
//
// Sequence boundaries come from charmbracelet/x/ansi, the same VT parser the
// render layer uses — what must be dropped is "whatever the terminal would ACT
// on", so using the parser that models the terminal is what keeps the sweep and
// the surface from drifting apart. A sequence left unterminated at a chunk
// boundary is dropped, so a repaint split across two chunks cannot be
// reassembled from two halves that each looked harmless.
//
// Each dropped sequence is dropped WHOLE, printable bytes included: leaving
// "[1A" behind would flood the transcript of any tool that redraws a progress
// line. See NOTES.md.
func stripTerminalControls(b []byte, keepSGR bool) []byte {
	out := make([]byte, 0, len(b))
	p := ansi.NewParser()
	var state byte
	keptSGR := false
	for len(b) > 0 {
		seq, _, n, newState := ansi.DecodeSequence(b, state, p)
		switch {
		case keepSGR && isSGR(seq, p):
			out = append(out, seq...)
			keptSGR = true
		case !actsOnTheTerminal(seq):
			out = append(out, seq...)
		case len(seq) == 1 && (seq[0] == '\n' || seq[0] == '\t'):
			out = append(out, seq...)
		}
		state = newState
		b = b[n:]
	}
	// The trailing reset is skipped when the input already ended in one, which
	// is what makes this sweep IDEMPOTENT. It has to be: the sweep runs at a
	// single Emit door (inertSink) that cannot know whether the bytes it is
	// handed have been through here already, and a reset appended once per pass
	// would grow the chunk by four bytes every time.
	if keptSGR && !bytes.HasSuffix(out, []byte(ansi.ResetStyle)) {
		out = append(out, []byte(ansi.ResetStyle)...)
	}
	return out
}

// actsOnTheTerminal reports whether seq is something the terminal executes
// rather than prints: an escape-introduced sequence, a C0 control, DEL, or a
// C1 control in either of its spellings — raw (0x80–0x9F) or as the UTF-8
// encoding of the same code point, which a terminal in UTF-8 mode may still
// honour, and which is how a repaint can be written without an ESC at all.
func actsOnTheTerminal(seq []byte) bool {
	if len(seq) == 0 {
		return false
	}
	c := seq[0]
	switch {
	case c == ansi.ESC, c < 0x20, c == ansi.DEL:
		return true
	case c >= 0x80 && c <= 0x9f: // raw C1
		return true
	case len(seq) == 2 && c == 0xc2 && seq[1] >= 0x80 && seq[1] <= 0x9f: // UTF-8 C1
		return true
	}
	return false
}

// isSGR reports whether seq is a plain Select Graphic Rendition — colour and
// decoration, the one class of sequence a tool's own output is entitled to.
//
// "Plain" excludes a private prefix and an intermediate byte, which is what
// keeps anything else wearing an 'm' final from being read as a colour. p
// carries the unpacked command for the sequence DecodeSequence just returned.
func isSGR(seq []byte, p *ansi.Parser) bool {
	if !ansi.HasCsiPrefix(seq) {
		return false
	}
	cmd := ansi.Cmd(p.Command())
	return cmd.Final() == 'm' && cmd.Prefix() == 0 && cmd.Intermediate() == 0
}

// inertInteractionPayload returns a copy of a wire InteractionRequestPayload
// with every publisher-supplied text slot made inert.
//
// It is NOT what makes the interaction sink safe — the Emit door does that for
// every event this kind sends, this payload included. It exists for ORDERING:
// it runs BEFORE channelinteractions.RenderText composes markdown around these
// slots, so a slot ending in a bare ESC cannot swallow the renderer's own
// following characters, and the structured Payload and the rendered Text cannot
// disagree about what has been neutralised. inertText is idempotent, so the
// door's second pass changes nothing.
//
// Category / RequestRef / IDs are skipped here because they are matched rather
// than displayed; the door still sweeps them. See NOTES.md.
func inertInteractionPayload(p channelevents.InteractionRequestPayload) channelevents.InteractionRequestPayload {
	p.Lead = inertText(p.Lead)
	p.Body = inertText(p.Body)
	p.NextStep = inertText(p.NextStep)
	if len(p.Fields) > 0 {
		fields := make([]channelevents.InteractionField, len(p.Fields))
		for i, f := range p.Fields {
			f.Label = inertText(f.Label)
			f.Value = inertText(f.Value)
			fields[i] = f
		}
		p.Fields = fields
	}
	if p.Excerpt != nil {
		ex := *p.Excerpt
		ex.Label = inertText(ex.Label)
		ex.Content = inertText(ex.Content)
		p.Excerpt = &ex
	}
	if len(p.Actions) > 0 {
		actions := make([]channelevents.InteractionAction, len(p.Actions))
		for i, a := range p.Actions {
			a.Label = inertText(a.Label)
			a.URL = inertText(a.URL)
			actions[i] = a
		}
		p.Actions = actions
	}
	return p
}
