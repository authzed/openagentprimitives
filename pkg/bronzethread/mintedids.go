package bronzethread

import (
	"fmt"
	"sync"

	memrev "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifactrevision"
	"github.com/authzed/openagentprimitives/pkg/platform/artifacts"
)

// Ids the SYSTEM mints during a run and hands back to the model.
//
// A bundle is data, and several tools require an id the system generated a step
// earlier — new_operation mints an operation_id, update_plan mints one per item
// it moves to in_progress, artifact_prepare mints an artifact_id — so no
// authored transcript can spell one that a later run will also produce.
//
// The bundle answers that by pinning the ids INSTEAD of rewriting them: it
// records, per family and in mint order, exactly what the run minted, and the
// replay's minters hand those same values back. Recorded arguments then keep
// their literal values and are correct by construction, with no substitution
// layer between the transcript and the call.
//
// That inverts an earlier scheme which rewrote each minted id into a `$`
// sentinel and substituted it back at replay. It could not be made to work:
// one sentinel stood for one remembered value, and a single update_plan mints
// one operation per in-progress item, so a plan with three items produced three
// distinct ids that one `$operationID` could not tell apart.
//
// # The sequence is an assertion, not just plumbing
//
// Because the replay draws from a fixed list in order:
//
//   - asking for MORE ids than were recorded exhausts the family, and the
//     minter says so, naming the family and the index;
//   - asking for FEWER leaves ids unused, which Unused reports — the replay's
//     plan opened fewer operations than the captured one did;
//   - asking in a different ORDER hands a recorded argument an id that now
//     addresses something else, and the step's own divergence check fires.
//
// A run that mints exactly what the capture minted, in the same order, is the
// only shape that passes all three.
const (
	// FamilyOperation covers the operation registry's ids (`op-…`), minted by
	// new_operation and by update_plan for each item entering in_progress.
	FamilyOperation = "operation"
	// FamilyArtifact covers logical-artifact head ids (`artifact-…`), minted by
	// artifact_prepare for a new artifact.
	FamilyArtifact = "artifact"
	// FamilyArtifactRevision covers revision ids (`artrev-…`), derived inside
	// artifacts.FinalizeRevision from the render CR's UID.
	//
	// A DERIVED id rather than a drawn one, and the distinction is the whole of
	// what makes its seam different: the derivation is what makes
	// prepare->await idempotent, so the replay must be keyed too. This family
	// is wired through KeyedMinter, never through Minter.
	FamilyArtifactRevision = "artifactRevision"
	// FamilyRenderHandle covers ArtifactRender CR names (`ar-<session>-<6 hex>`),
	// minted by artifact_prepare and by artifact_offer_view's preview child.
	//
	// It is model-facing: the name leaves as artifact_prepare's `handle` and
	// comes back as the argument to artifact_await and to `revises`.
	FamilyRenderHandle = "renderHandle"
)

// MintedIDFamily is one kind of minted id a bundle can pin.
type MintedIDFamily struct {
	// Name is the bundle's mintedIDs key.
	Name string
	// Prefix is what the minted id starts with, separator included. It is what
	// the family is named by in an error, and what the default shape test is
	// built from.
	Prefix string
	// Is reports whether s is an id of this family, matched against the WHOLE
	// string.
	//
	// nil takes the DEFAULT shape, which is what the memory-kind mint sites
	// emit: Prefix followed by mintedIDBodyLen lowercase hex digits (8 random
	// bytes, hex-encoded). Three of the four families are that shape.
	//
	// It is a field rather than a second prefix rule because one family is not
	// that shape at all — a render CR name carries the session between its
	// prefix and its tail — and the alternative was to transcribe that format
	// here, beside the mint site that already spells it. The row points at the
	// owning package's own inverse instead, so a reworded format cannot leave a
	// stale matcher behind: there is only ever one spelling.
	Is func(s string) bool
}

// MintedIDFamilies is the table both directions read: the steelthread capture
// files each id it observed under the family whose shape it matches, and the
// replay wires that family's recorded list to the component that mints it.
//
// It lives in the package that owns the bundle format, rather than in the
// driver, for the same reason the sentinel table used to: two copies would
// drift, and the symptom would be a captured bundle replaying an id nothing
// registered — which reads as a product bug rather than as the capture bug it
// is.
//
// A family is added here plus at the component's own newID seam. Nothing else
// branches on the name.
var MintedIDFamilies = []MintedIDFamily{
	{Name: FamilyOperation, Prefix: "op-"},
	{Name: FamilyArtifact, Prefix: "artifact-"},
	{Name: FamilyArtifactRevision, Prefix: memrev.IDPrefix},
	{Name: FamilyRenderHandle, Prefix: "ar-", Is: artifacts.IsRenderName},
}

// mintedIDBodyLen is the number of hex digits following a family prefix: 8
// random bytes, hex-encoded, at every mint site.
const mintedIDBodyLen = 16

// FamilyOf reports which family s is a minted id of.
//
// Matching is on the WHOLE string and on the exact minted SHAPE. Both halves
// matter:
//
//   - whole-string, because an id mentioned in passing inside a _reason or a
//     log line is prose about the run, not a value anything minted;
//   - exact shape, because a hand-authored bundle's "op-1" is an id the author
//     chose, and treating it as minted would file it into a sequence the replay
//     then has to reproduce.
func FamilyOf(s string) (string, bool) {
	for _, f := range MintedIDFamilies {
		if f.Is != nil {
			if f.Is(s) {
				return f.Name, true
			}
			continue
		}
		if len(s) != len(f.Prefix)+mintedIDBodyLen {
			continue
		}
		if s[:len(f.Prefix)] != f.Prefix {
			continue
		}
		if !isLowerHex(s[len(f.Prefix):]) {
			continue
		}
		return f.Name, true
	}
	return "", false
}

// isLowerHex reports whether every byte is a lowercase hex digit.
func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// MintedIDSequence hands a replay the ids the captured run minted, per family
// and in mint order.
//
// Guarded because the components drawing from it run on whichever goroutine
// dispatched the tool call — the runner loop for a meta tool, a NATS goroutine
// for a widget app-tool — while the test goroutine constructed it.
type MintedIDSequence struct {
	mu     sync.Mutex
	ids    map[string][]string
	next   map[string]int
	byKey  map[string]string // family\x00key → the id already handed out for it
	report func(string)
}

// NewMintedIDSequence builds the sequence for one replay.
//
// report is how an exhausted family becomes a visible failure. It is called on
// the goroutine that asked for the id — a test wires it to t.Errorf rather than
// t.Fatalf, because Fatalf's Goexit there would abandon the runner's stack
// rather than the test's. A nil report PANICS instead of returning quietly: an
// exhaustion that nothing reports is exactly the silent ride-through this
// format replaced.
func NewMintedIDSequence(ids map[string][]string, report func(string)) *MintedIDSequence {
	if report == nil {
		report = func(msg string) { panic("bronzethread: " + msg) }
	}
	return &MintedIDSequence{ids: ids, next: map[string]int{}, byKey: map[string]string{}, report: report}
}

// Minter returns the func to wire into a component's newID seam, or nil when
// the bundle pinned no ids for this family.
//
// nil is the compatibility path AND the honest one: a bundle that records no
// operation ids is making no claim about how many operations the run opens, so
// the component mints its own exactly as it does in production. Only a family
// the capture actually recorded is held to the sequence.
func (s *MintedIDSequence) Minter(family string) func() string {
	if len(s.ids[family]) == 0 {
		return nil
	}
	return func() string { return s.mint(family) }
}

// KeyedMinter returns the func to wire into a component's seam for a family
// whose id is DERIVED from something rather than drawn fresh, or nil when the
// bundle pinned no ids for it.
//
// # Why a keyed minter has to exist at all
//
// Some ids a run hands the model are not "the next value" — they are a pure
// function of a key, and the code around them DEPENDS on that. The live case is
// the artifact revision id, derived from the render CR's UID: artifact_prepare
// finalizes a revision and artifact_await re-finalizes the SAME CR, and the two
// agreeing on one id is what makes that handoff a repair rather than a second
// revision (see artifacts.WithRevisionIDMinter, which states the contract).
//
// Wiring such a seam to Minter would satisfy the FIRST call and quietly break
// the property on the second: the replay would draw a fresh id, finalize a
// second revision of one render, and double-count the head. That is a worse
// defect than the divergence the pinning was added to fix, and it would show up
// nowhere near here.
//
// So the key is memoized. First ask for a key draws the next recorded id for
// the family and remembers it; every later ask with the same key returns that
// same id, with no draw. The sequence assertion survives intact, because it now
// counts DISTINCT keys — which is exactly the number of revisions the run
// finalized, the thing the capture recorded.
//
// Keys are namespaced by family, so two families keyed on the same string
// (a UID, a name) never alias.
func (s *MintedIDSequence) KeyedMinter(family string) func(key string) string {
	if len(s.ids[family]) == 0 {
		return nil
	}
	return func(key string) string {
		s.mu.Lock()
		if id, ok := s.byKey[family+"\x00"+key]; ok {
			s.mu.Unlock()
			return id
		}
		s.mu.Unlock()

		id := s.mint(family)

		s.mu.Lock()
		defer s.mu.Unlock()
		// Re-check under the second lock: two goroutines finalizing the same CR
		// concurrently would otherwise both draw. The FIRST id recorded wins and
		// the loser's draw is deliberately not put back — a returned id would
		// have to un-advance the counter, and a sequence that can move backwards
		// stops being the mint log its exhaustion/leftover assertions read.
		// Losing one to a race shows up as a leftover, which is a visible,
		// correctly-reported symptom rather than a silent aliasing.
		if existing, ok := s.byKey[family+"\x00"+key]; ok {
			return existing
		}
		s.byKey[family+"\x00"+key] = id
		return id
	}
}

// mint hands out the next recorded id for a family, or reports exhaustion.
func (s *MintedIDSequence) mint(family string) string {
	s.mu.Lock()
	i := s.next[family]
	s.next[family] = i + 1
	recorded := s.ids[family]
	s.mu.Unlock()

	if i < len(recorded) {
		return recorded[i]
	}
	s.report(fmt.Sprintf(
		"bundle mintedIDs[%q] is exhausted: the replay asked for %s id #%d but the capture recorded %d — "+
			"this run minted MORE %s ids than the captured session did, so the recorded arguments "+
			"downstream of here address operations the run never opened",
		family, family, i+1, len(recorded), family))
	// Deliberately NOT a recorded value and deliberately not the minted shape:
	// repeating one would silently alias two operations, and looking minted
	// would make the wrong id plausible in whatever error surfaces next.
	return family + "-exhausted-" + fmt.Sprint(i+1)
}

// Unused returns, per family, the recorded ids the replay never asked for, in
// recorded order. Empty when every family was drawn to its end.
//
// The mirror of exhaustion: leftovers mean the replay minted FEWER ids than the
// capture did — most often a plan that opened fewer operations because an item
// no longer moves to in_progress.
func (s *MintedIDSequence) Unused() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out map[string][]string
	for family, recorded := range s.ids {
		if i := s.next[family]; i < len(recorded) {
			if out == nil {
				out = map[string][]string{}
			}
			out[family] = recorded[i:]
		}
	}
	return out
}
