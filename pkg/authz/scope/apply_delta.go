package scope

import "time"

// ApplyDelta returns a new Scope with d applied. Pure; safe to call
// concurrently on shared inputs. ScopeVersion is always bumped by 1
// (even on empty deltas — the bump is the "ack" signal to cache invalidators).
// `addSource` tags any newly-added ScopeResource entry, and an existing one
// that carries no tag yet. It never overwrites a tag already there — see
// applyAdd. Pass "" to add IDs while asserting no provenance at all.
func ApplyDelta(s Scope, d ScopeDelta, addSource Source, now time.Time) Scope {
	next := cloneScope(s)
	applyAdd(&next, d.Add, addSource)
	applyRemove(&next, d.Remove)
	for _, deny := range d.HardDeny.Tools {
		next.Tools.Deny = appendUnique(next.Tools.Deny, deny)
	}
	// HardDeny.Resources populate next.Disallow (Layer 2), enforced at dispatch
	// by the Scope hook regardless of toolCalls.mode. This is the SOLE hard-deny
	// enforcement surface. The IDs list holds both exact and glob forms
	// (e.g. "ENG-*"); ResourceDisallowed matches either.
	for _, ref := range d.HardDeny.Resources {
		idx := findDisallowType(&next, ref.ResourceType)
		if idx < 0 {
			next.Disallow = append(next.Disallow, ScopeResource{
				ResourceType: ref.ResourceType,
				IDs:          []string{ref.ID},
				Source:       addSource,
			})
			continue
		}
		next.Disallow[idx].IDs = appendUnique(next.Disallow[idx].IDs, ref.ID)
	}
	next.ScopeVersion = s.ScopeVersion + 1
	return next
}

func cloneScope(s Scope) Scope {
	out := s
	out.Resources = make([]ScopeResource, len(s.Resources))
	for i, r := range s.Resources {
		out.Resources[i] = ScopeResource{
			ResourceType: r.ResourceType,
			IDs:          append([]string(nil), r.IDs...),
			Patterns:     append([]ResourcePattern(nil), r.Patterns...),
			Source:       r.Source,
		}
	}
	out.Disallow = make([]ScopeResource, len(s.Disallow))
	for i, r := range s.Disallow {
		out.Disallow[i] = ScopeResource{
			ResourceType: r.ResourceType,
			IDs:          append([]string(nil), r.IDs...),
			Patterns:     append([]ResourcePattern(nil), r.Patterns...),
			Source:       r.Source,
		}
	}
	out.Tools.Allow = append([]string(nil), s.Tools.Allow...)
	out.Tools.Deny = append([]string(nil), s.Tools.Deny...)
	out.ArgConstraints = append([]ArgConstraint(nil), s.ArgConstraints...)
	return out
}

// findDisallowType returns the index of the Disallow entry for type t, or -1.
func findDisallowType(s *Scope, t string) int {
	for i, r := range s.Disallow {
		if r.ResourceType == t {
			return i
		}
	}
	return -1
}

func applyAdd(s *Scope, p ScopePartial, src Source) {
	for _, ref := range p.Resources {
		idx := findResourceType(s, ref.ResourceType)
		if idx < 0 {
			s.Resources = append(s.Resources, ScopeResource{
				ResourceType: ref.ResourceType,
				IDs:          []string{ref.ID},
				Source:       src,
			})
			continue
		}
		s.Resources[idx].IDs = appendUnique(s.Resources[idx].IDs, ref.ID)
		// FIRST NON-EMPTY SOURCE WINS. An existing Source is never overwritten.
		//
		// Source is per-TYPE while IDs accumulate into that one entry from
		// several fill sources, so the field can never be accurate per-ID; the
		// only question is which lossy answer is least harmful. "Last writer
		// wins" was the harmful one: it makes the field a record of the most
		// recent writer, and a source that RE-RUNS — PromoteObservedSlots runs
		// after every dispatch round once a fact exists — then re-tags an entry
		// a human cleared as `approved` with its own `observed`. The audit trail
		// would say an observation put in scope what a person waived a gate for,
		// which is the misattribution SourceObserved was given its own value to
		// prevent, running backwards.
		//
		// Set-once instead means the field answers "what FIRST put this type in
		// scope" — stable, monotone, and unable to rewrite a human decision as
		// an automated one. Deliberately not an authority ORDERING over the five
		// sources that actually reach here (default, extracted, observed,
		// approved, metaagent-approved — SourceInitialAsk and
		// SourceApproverDenyConv are never passed as addSource): ranking them
		// would be invention, and it is not needed to close this. Per-ID
		// provenance is not lost either way — the approval record, the slot grant
		// and the authz decision log each carry it.
		//
		// An empty existing Source is filled rather than preserved: there is no
		// provenance there to protect, and an entry written before this field
		// meant anything should get a real answer the first time one arrives.
		if src != "" && s.Resources[idx].Source == "" {
			s.Resources[idx].Source = src
		}
	}
	for _, pat := range p.ResourcePatterns {
		// Pattern adds attach to the first matching ScopeResource entry.
		// Callers typically supply patterns scoped to a single type;
		// typed patterns are a follow-on improvement.
		if len(s.Resources) > 0 {
			s.Resources[0].Patterns = append(s.Resources[0].Patterns, pat)
		}
	}
	for _, tool := range p.Tools {
		s.Tools.Allow = appendUnique(s.Tools.Allow, tool)
	}
	s.ArgConstraints = append(s.ArgConstraints, p.ArgConstraints...)
}

func applyRemove(s *Scope, p ScopePartial) {
	for _, ref := range p.Resources {
		idx := findResourceType(s, ref.ResourceType)
		if idx < 0 {
			continue
		}
		s.Resources[idx].IDs = removeString(s.Resources[idx].IDs, ref.ID)
		// A type that now names nothing carries no provenance either. Its
		// entry STAYS — absence of a type from Resources reads as "scope does
		// not narrow this type", so dropping it would widen rather than revoke
		// (see TestApplyDelta_RemoveAllIDs_LeavesEmptyEntry) — but the tag it
		// keeps would be a claim about instances that are gone.
		//
		// This matters because applyAdd is now set-once: a stale tag left here
		// would be PERMANENT, and the next source to re-populate the type would
		// silently inherit a predecessor's provenance. Clearing restores the
		// "no provenance yet" state that lets the next writer tag it honestly.
		//
		// Patterns are checked too: an entry with no IDs but a live pattern
		// still reaches resources, so its provenance is still a true claim.
		if len(s.Resources[idx].IDs) == 0 && len(s.Resources[idx].Patterns) == 0 {
			s.Resources[idx].Source = ""
		}
	}
	for _, tool := range p.Tools {
		s.Tools.Allow = removeString(s.Tools.Allow, tool)
	}
}

func findResourceType(s *Scope, t string) int {
	for i, r := range s.Resources {
		if r.ResourceType == t {
			return i
		}
	}
	return -1
}

func appendUnique(haystack []string, needle string) []string {
	for _, s := range haystack {
		if s == needle {
			return haystack
		}
	}
	return append(haystack, needle)
}

func removeString(haystack []string, needle string) []string {
	out := make([]string, 0, len(haystack))
	for _, s := range haystack {
		if s != needle {
			out = append(out, s)
		}
	}
	return out
}
