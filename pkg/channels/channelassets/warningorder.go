package channelassets

import (
	"slices"
	"strings"
)

// The canonical order of a warning list.
//
// # Why this exists
//
// A renderer discovers what it stripped by DIFFING two maps of counts, and Go
// randomizes map iteration, so the warnings came out in a different order on
// every render of identical bytes. Nothing looked wrong — the set was always
// right — but the list is not an internal detail: it is returned to the MODEL
// in artifact_prepare's result and mirrored onto ArtifactRender.status.warnings,
// so two identical renders produced two different results.
//
// It surfaced when a captured session was replayed and the only difference in
// the whole result was the order of four warnings. That is the shape of bug
// this kind of replay exists to find: a value nobody asserted on, differing run
// to run, in bytes a model reads.
//
// # Why a key rather than a comparator
//
// The same order has to be applied to the CRD mirror type
// (spicebox/v1alpha1.SanitizerWarning), which cannot import this package. A
// key function crosses that boundary where a Less over this package's type
// cannot, so there is one definition of the order rather than two that drift.

// WarningSortKey is the canonical sort key of a warning, from the three fields
// that identify one.
//
// Count and Note are deliberately absent: they are what the warning REPORTS,
// not which warning it is, and two warnings never share a (kind, name, action)
// within one render — the renderer aggregates by name before emitting.
//
// The separator is a byte that cannot appear in any of the three, so a name
// ending where the next field begins cannot make two different warnings
// compare equal.
func WarningSortKey(kind, name, action string) string {
	return kind + "\x00" + name + "\x00" + action
}

// SortWarnings puts ws into the canonical order, in place.
//
// Stable, so a renderer that emits two warnings this key cannot tell apart
// keeps them in the order it found them rather than swapping them at random —
// which would reintroduce exactly the nondeterminism this fixes.
func SortWarnings(ws []Warning) {
	slices.SortStableFunc(ws, func(a, b Warning) int {
		return strings.Compare(
			WarningSortKey(a.Kind, a.Name, a.Action),
			WarningSortKey(b.Kind, b.Name, b.Action))
	})
}
