package auditgen

// Lens is one review dimension of an audit pass. Each lens becomes a subagent
// persona, dispatched in parallel (per subsystem group, for a whole-repo pass)
// during a run, and one section of the ranked report.
//
// This slice is the single source of truth for the lenses: the audit prompt
// (prompt.go) and the human-facing "Auditing" section of AGENTS.md are both
// renderings of it. A NEW lens is added here as a row — never bolted onto a
// consumer — matching this repo's registry-over-branching ethos.
type Lens struct {
	Slug    string // stable kebab-case identifier
	Title   string // display name, used as the report section heading
	Persona string // the "respond as …" framing handed to the lens subagent
	Charge  string // what this lens hunts for (rendered into the prompt)
}

// Lenses is the canonical, ordered set of audit lenses. Order is the order the
// sections appear in the report.
var Lenses = []Lens{
	{
		Slug:    "security",
		Title:   "Security",
		Persona: "a senior security engineer",
		Charge: "authorization gaps and fail-open paths; secret, credential, and token handling and leakage " +
			"(into logs, responses, transcripts, or the frontend); injection, SSRF, and unsafe deserialization; " +
			"capability leaks and confused-deputy risks; typed-nil interfaces used as security gates; privilege " +
			"escalation; missing live verification of subjects and tokens; unsanitized content rendered to a browser. " +
			"Anchor to this repo's patterns: capability-in-context, fail-closed choke points, live token verification",
	},
	{
		Slug:    "dry",
		Title:   "DRY",
		Persona: "an engineer allergic to copy-paste and duplicated logic",
		Charge: "logic duplicated across files or packages that should be factored onto a shared abstraction or " +
			"registry; parallel switch statements (if kind == \"x\") that belong as an interface method; copy-pasted " +
			"boilerplate that must change together. Prefer forcing all variants onto the shared abstraction over a " +
			"new one-off helper; distinguish real duplication from incidental line-level similarity",
	},
	{
		Slug:    "readability",
		Title:   "Readability",
		Persona: "a reviewer optimizing for the next engineer who reads and changes this code",
		Charge: "opaque or hard-to-follow control flow; unclear or misleading names; missing why-comments on " +
			"non-obvious logic (comments explain why, not what); code living in the wrong package or file; files or " +
			"functions grown too large or doing too much; magic numbers and strings needing a named constant. Offer a " +
			"concrete suggestion for each — a name, a comment to add, a place to move code",
	},
	{
		Slug:    "performance",
		Title:   "Performance",
		Persona: "a performance engineer",
		Charge: "N+1 and O(n^2) patterns; avoidable allocations in hot paths; locks held across I/O or slow calls; " +
			"redundant recomputation that could be cached or memoized; unbounded growth of slices, maps, or " +
			"goroutines; on the frontend, re-renders from unstable identities, work in render that belongs in a memo, " +
			"and unthrottled high-frequency updates. Argue why each cost is on a real hot path",
	},
	{
		Slug:    "correctness",
		Title:   "Correctness",
		Persona: "a reviewer hunting logic bugs — where the code does not do what it evidently intends",
		Charge: "logic that contradicts its evident intent; silently-dropped errors; typed-nil interface or nil-map " +
			"panics; server-side-apply idempotency violations (volatile values in applied fields); off-by-one and " +
			"boundary errors; missing cases; wrong conditionals; stale-closure state updates. Every finding needs a " +
			"concrete failure scenario tying inputs or state to a wrong outcome",
	},
	{
		Slug:    "testing",
		Title:   "Testing / coverage",
		Persona: "a test engineer who distrusts untested code",
		Charge: "production logic with no test coverage, especially error paths; tests that assert nothing meaningful; " +
			"missing negative and edge cases; table-driven opportunities (four or more sibling tests sharing shape); " +
			"code only reachable through the build-tagged integration/e2e suites that a refactor could silently break. " +
			"Grep the *_test.go files before claiming something is untested",
	},
	{
		Slug:    "conventions",
		Title:   "Conventions / repo-idioms",
		Persona: "the maintainer enforcing this repository's documented conventions (AGENTS.md)",
		Charge: "concrete violations of the AGENTS.md rulebook: silently-dropped errors (bare returns, discarded " +
			"error values with no log or user-visible surface); typed-nil pointers assigned to interface fields; " +
			"volatile values in server-side-applied fields (observations belong in status); if kind == \"x\" branches " +
			"that should be a registered interface method; single-implementation interfaces collapsed to concrete " +
			"types; real names where fakes belong; test style drift. Name the specific rule each finding breaks",
	},
	{
		Slug:    "concurrency",
		Title:   "Concurrency / races",
		Persona: "a concurrency reviewer",
		Charge: "data races on shared maps, slices, or fields (mutated without a lock, or read while written); " +
			"goroutine leaks (no exit path when the connection or context dies); deadlocks and lock-ordering issues; " +
			"locks held while sending on a channel or doing I/O; unbuffered sends that can block forever; missing or " +
			"incorrect context cancellation; send-on-closed or close races; on the frontend, effects setting state " +
			"after unmount and stale-closure timers. Each finding needs a concrete interleaving",
	},
	{
		Slug:    "durability",
		Title:   "Durability / restart survival",
		Persona: "a reviewer who assumes every process is about to be killed and asks what the user loses when it is",
		Charge: "in-memory state whose loss on a restart is SILENT and UNRECOVERABLE — the user is left wedged with " +
			"no error, no retry, and no action that recovers: a cache that is the only record of something a user is " +
			"waiting on; a process-scoped table that is never rehydrated from the durable source it was built from; " +
			"a dedup or already-published marker held only in memory (or only durably, so a restart re-does a " +
			"user-visible side effect); work in flight with no durable record that it was started. " +
			"The bar is the FAILURE, not the storage: state that degrades gracefully, is re-derivable on next use, or " +
			"is ephemeral by design (per-connection buffers, short idempotency windows, live-only sinks) is CORRECT and " +
			"must not be reported. For each finding name the restart that triggers it, what the user sees afterwards, " +
			"and why no existing path recovers. Measure against this repo's established pattern — an in-memory cache in " +
			"front of a durable record behind it (retryReported over its annotation; resolvedCache over the memapproval " +
			"reader) — and prefer rehydrate-from-the-existing-durable-source over adding a new store",
	},
	{
		Slug:    "state",
		Title:   "State management",
		Persona: "a reviewer who treats mutable state as guilty until proven safe and hunts for simpler state shapes",
		Charge: "multiple sources of truth for the same fact; state stored that could be derived (and so can drift); " +
			"mirrored or duplicated state across layers that can desync (server vs client vs URL vs component state); " +
			"diffuse or unclear ownership of a mutation; state machines with ambiguous, missing, or unreachable " +
			"transitions. Weight findings toward concrete simplifications — lift state up, collapse two stores into " +
			"one, replace stored-and-synced with derived, remove a redundant field — that eliminate whole bug classes",
	},
}

// Lookup returns the lens with the given slug, and whether it was found.
func Lookup(slug string) (Lens, bool) {
	for _, l := range Lenses {
		if l.Slug == slug {
			return l, true
		}
	}
	return Lens{}, false
}
