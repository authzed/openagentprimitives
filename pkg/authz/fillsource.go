package authz

// FillSource names a way an instance may come to occupy a slot.
//
// The values match AgentClass.spec.authz.slots[].fillFrom. They live here
// rather than in v1alpha1 because every consumer that has to ask "may this slot
// be filled this way" is on the pkg/authz side of the adapter boundary, and a
// second copy of the vocabulary is how the two drift.
type FillSource string

const (
	// FillDefault binds class-pinned IDs at session start.
	FillDefault FillSource = "default"
	// FillQuery pulls an instance from the request or the tool args. The
	// extractor path.
	FillQuery FillSource = "query"
	// FillAsk has the agent ask the user, blocking on the answer.
	FillAsk FillSource = "ask"
	// FillChannelThread seeds candidates from the thread the session was minted
	// from, governed by autoGrantFrom.
	FillChannelThread FillSource = "channel_thread"
	// FillMetaagent binds through ambient scope intent, human-approved.
	FillMetaagent FillSource = "metaagent"
	// FillObserved binds an instance a tool result recorded a fact about. It
	// gates the ROUTE, not the value: a slot listing only observed cannot be
	// occupied by a class-pinned default, a user-named instance, or a thread
	// seed -- every one of which would put an instance in the slot with no
	// observation ever made about it.
	FillObserved FillSource = "observed"
	// FillTrigger binds the instances a VERIFIED webhook delivery names, at
	// session mint. The delivery has one author — the forge whose HMAC
	// verified — so there is no autoGrantFrom analogue; the opt-in is this
	// fillFrom value itself. Unlike every other source, that opt-in must be
	// EXPLICIT: gate eligibility for this source with ExplicitlyAllowsFill,
	// never AllowsFill — an unset fillFrom must never make a slot
	// trigger-eligible. See ExplicitlyAllowsFill's doc comment for why.
	FillTrigger FillSource = "trigger"
)

// fillSourceSpellings maps each source to every fillFrom value that selects it.
//
// query and extract are ONE source under two names — the CRD enum accepts both
// and the design's fill-source table writes it "query / extract". Treating them
// as aliases here means a class that wrote either spelling gets the behavior it
// asked for, instead of one of them silently gating nothing.
var fillSourceSpellings = map[FillSource][]string{
	FillDefault:       {"default"},
	FillQuery:         {"query", "extract"},
	FillAsk:           {"ask"},
	FillChannelThread: {"channel_thread"},
	FillMetaagent:     {"metaagent"},
	FillObserved:      {"observed"},
	FillTrigger:       {"trigger"},
}

// FillFromVocabulary returns every fillFrom string value a slot may declare —
// every spelling in fillSourceSpellings, flattened and deduplicated (query
// and extract both belong to FillQuery, so each appears once, not attached to
// a source twice).
//
// This is THE Go-side list, and the twin of the CRD's kubebuilder
// items:Enum marker on AuthzSlot.FillFrom
// (pkg/apis/v1alpha1/agentclass_types.go). A caller that needs the set as a
// plain []string or map — a reconciler-side defense-in-depth check, an error
// message — builds it from here rather than retyping it: fillSourceSpellings
// used to be the only copy, a reconciler-side mirror was hand-typed
// separately, and the CRD gained "trigger" one fix wave before the mirror
// did — a class naming it was refused at every reconcile with no path to
// Valid=True, and nothing but a live envtest run ever exercised the mismatch.
// Order is unspecified; sort at the call site if you need determinism.
func FillFromVocabulary() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, spellings := range fillSourceSpellings {
		for _, s := range spellings {
			if _, dup := seen[s]; dup {
				continue
			}
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// AllowsFill reports whether a slot declaring the given fillFrom may be filled
// from src.
//
// fillFrom is a NARROWING declaration: an unset list narrows nothing and every
// source is allowed. That is deliberate in both directions. It preserves the
// behavior of every class authored before the field existed — the field was
// added without one, so reading absence as "nothing may fill this" would have
// silently disarmed them. And it keeps the field's meaning one-way: writing a
// fillFrom can only REMOVE ways an instance can bind, never add one. A reader
// who sees the field never has to ask whether it widened anything.
func AllowsFill(fillFrom []string, src FillSource) bool {
	if len(fillFrom) == 0 {
		return true
	}
	spellings := fillSourceSpellings[src]
	for _, declared := range fillFrom {
		for _, s := range spellings {
			if declared == s {
				return true
			}
		}
	}
	return false
}

// AllowsFill reports whether this slot may be filled from src.
func (s BoundEntitySpec) AllowsFill(src FillSource) bool {
	return AllowsFill(s.FillFrom, src)
}

// ExplicitlyAllowsFill reports whether fillFrom NAMES src, with no
// unset-means-allowed fallback: an empty list is false here, never true.
//
// AllowsFill's "unset narrows nothing" default is right for every source
// that still sits behind a second gate before an instance gains standing: a
// query/ask bind still runs the extractor and a Check; a channel_thread seed
// still resolves through autoGrantFrom's owner-only default; a default is
// class-pinned by the class author at authoring time, not derived from an
// unattributed inbound at runtime. FillTrigger has no such second gate — a
// verified delivery becomes a SpiceDB slot grant directly, at session mint,
// with no human anywhere in the loop. An unset fillFrom silently opting a
// slot into THAT is exactly the implicit standing-authorization this
// function exists to refuse: trigger must be spelled out in fillFrom, or a
// slot never binds from one, however permissive its other fillFrom entries
// (or their absence) reads for every other source.
//
// Use AllowsFill for every source other than FillTrigger; nothing else is
// expected to call this.
func ExplicitlyAllowsFill(fillFrom []string, src FillSource) bool {
	if len(fillFrom) == 0 {
		return false
	}
	spellings := fillSourceSpellings[src]
	for _, declared := range fillFrom {
		for _, s := range spellings {
			if declared == s {
				return true
			}
		}
	}
	return false
}

// AllowsExtractedBinding reports whether an instance the extractor found in a
// user message may bind this slot.
//
// TWO SOURCES SHARE ONE BINDING PATH. `query` is the user naming an instance
// unprompted; `ask` is the agent prompting and waiting for the answer. They
// differ in who started the exchange, not in how the value binds — either way
// the instance arrives in a user message, the extractor proposes it, and it
// binds only if the requester already has standing.
//
// So `ask` needs no mechanism of its own: the agent asks with respond_to_user
// and waits with await_user_message, both of which it already has, and the
// reply is an ordinary turn. Adding a second extraction path would have been
// two implementations of one idea, and a slot declaring `ask` would otherwise
// be un-fillable — a control in the YAML that binds nothing.
//
// Callers gating the extractor path MUST use this rather than AllowsFill(query)
// directly, or a slot declaring only `ask` silently stops working.
func AllowsExtractedBinding(fillFrom []string) bool {
	return AllowsFill(fillFrom, FillQuery) || AllowsFill(fillFrom, FillAsk)
}
