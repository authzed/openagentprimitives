// Package undecodable holds the one operator-facing report every Kind accessor
// owes when a stored Entry will not decode.
//
// The situation is specific to append-only Kinds, and it is permanent. Memory.Put
// validates an entry's Kind registration, the writer's authority and the ID/link
// prefixes — nothing about its SHAPE. Kind.ContentSchema() is read at exactly one
// site (memory.ContentKeys, for query field-path validation), never as a
// write-time contract, and several Kinds legitimately declare none. So an
// authorized writer can store a row no reader can parse, and nothing removes it
// afterwards: per-entry Delete is refused for an append-only Kind, DeleteScope
// skips it.
//
// Choosing a disposition is a judgement about what the row MEANS:
//
//   - Skipped — the row is part of a RECORD (a transcript, an audit list, a
//     rendering stream). Erroring instead wedges every read of the scope forever,
//     with no operator remedy.
//   - Refused — the row is an INPUT TO A GATE (a taint, a denial, a recorded
//     decision). Dropping it makes the gate forget what the row recorded, which
//     re-opens exactly what it was written to close. Fail closed.
//
// Both name the entry AND its publisher: the ID says which record is missing from
// a tamper-evident log, the publisher says who to go fix — the only remedy once
// the row cannot be deleted. Silence is not an option for either.
package undecodable

import (
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Skipped reports an entry a reader could not decode and chose to SKIP,
// degrading its result rather than failing the whole read. label is the calling
// accessor's name ("turn.ReadAll"), so the line is greppable to one call site.
func Skipped(label string, scope memory.Scope, e memory.Entry, err error) {
	slog.Info(label+": skipping an undecodable entry — the read degrades rather than halting",
		"scope", scope.ID, "entry", e.ID, "publisher", PublisherOf(e), "err", err.Error())
}

// Refused reports an entry a reader could not decode and chose to REFUSE the
// whole read over, because dropping it would weaken the gate the entry feeds.
// The caller still returns the error; this exists because that error travels to
// a caller that renders it as a verdict, so the entry ID and publisher an
// operator needs would otherwise never be written down anywhere.
func Refused(label string, scope memory.Scope, e memory.Entry, err error) {
	slog.Info(label+": refusing the read over an undecodable entry — dropping it would weaken the gate it feeds",
		"scope", scope.ID, "entry", e.ID, "publisher", PublisherOf(e), "err", err.Error())
}

// PublisherOf names the publisher that signed e. Unsigned entries predate
// verify-on-write, or came from a facade with no verifier configured; they are
// reported as such rather than as an absent field, because "nobody signed this"
// is itself the most interesting thing an operator can learn about a row that
// will not parse.
func PublisherOf(e memory.Entry) string {
	if e.Provenance == nil {
		return "(unsigned)"
	}
	return e.Provenance.Publisher
}
