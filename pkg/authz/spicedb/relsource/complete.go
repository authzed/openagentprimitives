package relsource

import (
	"errors"
	"fmt"
	"sync/atomic"
)

// complete latches true once MarkComplete has been called. CheckWrite and
// CheckDeleteFilter refuse every call until it does — see MarkComplete and
// requireComplete.
var complete atomic.Bool

// ErrClaimTableIncomplete is wrapped into requireComplete's error: this
// process has not called MarkComplete (production binaries do so by
// blank-importing pkg/authz/spicedb/relsource/imports). Deliberately
// distinct from ErrRefused (check.go) — a caller classifying errors with
// errors.Is must be able to tell "this binary forgot to link the imports
// bundle" (a wiring bug, here) from "a kind tried to write a relation
// another source legitimately owns" (a real ownership conflict, there).
// Collapsing the two used to misclassify this one as an ordinary guard
// refusal.
var ErrClaimTableIncomplete = errors.New("relsource: claim table not marked complete")

// MarkComplete declares the process's claim table complete: every in-tree
// package that owns relation claims has had its init() run and registered
// them. The one production caller is
// pkg/authz/spicedb/relsource/imports's own init(), which blank-imports
// every claim-owning package and then calls this — so linking that bundle
// is what turns the guard on.
//
// This exists because claims register from init() in each writer's own
// package, so the registry's contents depend entirely on which packages a
// given binary links. A binary that constructs a relsource-guarded
// RelWriter (spicedb.NewWriter / (*Client).Writer) but never links the
// imports bundle would otherwise see an incomplete table and silently
// allow every write — indistinguishable from a relation nobody has ever
// claimed. MarkComplete plus the fail-closed check in requireComplete turns
// that silent gap into a loud refusal on the binary's first guarded write.
//
// A test that cannot link the bundle (relsource's own tests, and
// pkg/authz/spicedb's — both would cycle back through the bundle to
// themselves) calls MarkComplete directly, after registering whatever
// fixture claims the test needs.
func MarkComplete() {
	complete.Store(true)
}

// requireComplete is CheckWrite and CheckDeleteFilter's fail-closed gate.
// See MarkComplete for why an unmarked table refuses rather than allows.
func requireComplete() error {
	if !complete.Load() {
		return fmt.Errorf("%w — this binary must blank-import github.com/authzed/openagentprimitives/pkg/authz/spicedb/relsource/imports (a test that cannot import it calls relsource.MarkComplete directly instead) before any guarded write or delete", ErrClaimTableIncomplete)
	}
	return nil
}

// IsComplete reports whether MarkComplete has been called in this process.
//
// Exported for a READER that walks relsource.All() rather than going through
// CheckWrite/CheckDeleteFilter — a claim-table read (ListSubjectIdentities,
// e.g.) has no write to refuse, but the same ambiguity MarkComplete's own doc
// warns about for writes applies to it just as much: an unlinked imports
// bundle makes All() come back empty, which is indistinguishable from "no
// source has ever claimed anything". A caller that treats that emptiness as
// a real answer would return a confident "nothing here" instead of a loud
// "this binary isn't wired". IsComplete lets such a caller fail closed the
// same way requireComplete already does for writes.
func IsComplete() bool {
	return complete.Load()
}
