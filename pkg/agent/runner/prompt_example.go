package runner

import (
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// planExample renders the worked `phases` example from THIS session's own
// permission surface.
//
// It was hardcoded git for a long time, and shipped that way to every agent —
// including ones whose declarable handles are entirely something else, rendered
// immediately after the prompt tells them these are the ONLY handles they may
// declare and anything off the list is silently dropped. That is worse than no
// example: agents transcribe this block closely (measured), so a git-shaped
// template aimed at a CRM agent is a template for handles that do not exist.
//
// The shape it teaches is the one that costs ONE approval instead of several:
//
//	look   — the readonly permissions, on the instance the user named
//	act    — the changing permissions, carrying the READ they depend on
//
// That second line is the whole point. A phase that may change a thing but not
// read it stops mid-task to ask a human for the read, and the previous example
// showed exactly that mistake.
//
// Derived, never agent-authored: every handle comes off the surface the
// runtime enumerated, so this sits on the trusted half of the prompt.
func planExample(surface []permsurface.Descriptor) string {
	reads, writes := splitByImpact(surface)
	if len(reads) == 0 && len(writes) == 0 {
		return ""
	}

	// The instance axis is worth showing on the type the ACTING phase touches —
	// that is where naming the target up front turns N approvals into one.
	slotType := ""
	if len(writes) > 0 {
		slotType = writes[0].ResourceType
	} else {
		slotType = reads[0].ResourceType
	}

	var b strings.Builder
	b.WriteString("  \u2022 Example \u2014 the shape that costs ONE approval, in your own handles:\n")
	b.WriteString("      \"phases\":[\n")

	looks := capped(onType(reads, slotType), 3)
	if len(looks) == 0 {
		looks = capped(reads, 3)
	}
	if len(looks) > 0 {
		b.WriteString("        {\"id\":\"look\",\"label\":\"Look at what you were asked about\",\n")
		b.WriteString("         \"permissions\":[" + handleList(looks, "see what is there") + "],\n")
		b.WriteString("         \"slots\":[" + slotLine(slotType, "the one the user named") + "]}")
	}
	if len(writes) > 0 {
		if len(looks) > 0 {
			b.WriteString(",\n")
		}
		// The WRITES are what this phase is for, so they are never the ones
		// trimmed. The read it depends on rides with them \u2014 omitting it here is
		// not a shorter example, it is the wrong one, and this block is copied
		// closely enough that the difference reaches production plans.
		acting := handleList(capped(onType(writes, slotType), 2), "make the change")
		if r, ok := dependedRead(reads, slotType); ok {
			acting = handleList([]permsurface.Descriptor{r}, "read what you are changing") + "," + acting
		}
		b.WriteString("        {\"id\":\"act\",\"label\":\"Make the change\",\n")
		b.WriteString("         \"permissions\":[" + acting + "],\n")
		b.WriteString("         \"slots\":[" + slotLine(slotType, "the same one") + "]}")
	}
	b.WriteString("]\n")
	if len(writes) > 0 {
		b.WriteString("    Both phases name the target, so ONE approval covers the whole task. If you cannot name it yet, declare the slot with no id \u2014 only that phase is asked about again once you find it.\n")
	}
	return b.String()
}

// splitByImpact separates the surface into what only LOOKS and what CHANGES.
// A `tool:` handle carries no resource type and is skipped: the example is
// about the permission-and-instance shape, and a tool handle has no instance.
func splitByImpact(surface []permsurface.Descriptor) (reads, writes []permsurface.Descriptor) {
	for _, d := range surface {
		if d.ResourceType == "" || d.Permission == "" {
			continue
		}
		switch d.StateImpact {
		case authz.Readonly:
			reads = append(reads, d)
		case authz.Readwrite, authz.External:
			writes = append(writes, d)
		}
	}
	byHandle := func(s []permsurface.Descriptor) {
		sort.Slice(s, func(i, j int) bool { return s[i].Handle.String() < s[j].Handle.String() })
	}
	byHandle(reads)
	byHandle(writes)
	return reads, writes
}

func onType(ds []permsurface.Descriptor, resourceType string) []permsurface.Descriptor {
	out := make([]permsurface.Descriptor, 0, len(ds))
	for _, d := range ds {
		if d.ResourceType == resourceType {
			out = append(out, d)
		}
	}
	return out
}

// handleList renders the given handles. It does NOT trim: callers decide what
// may be dropped, because the acting phase must never lose its writes.
func handleList(ds []permsurface.Descriptor, why string) string {
	parts := make([]string, 0, len(ds))
	for _, d := range ds {
		parts = append(parts, "{\"handle\":\""+d.Handle.String()+"\",\"why\":\""+why+"\"}")
	}
	return strings.Join(parts, ",")
}

// slotLine renders one slot with a placeholder id that states the RULE, not a
// value.
//
// Two things were learned the hard way here, both from measuring what planners
// actually emit. First, a concrete id gets transcribed: the old hardcoded
// example carried a real URL, and agents copy this block closely enough that
// `https://github.com/acme/app` would travel into plans with nothing to do with
// git. Second — and less obvious — a placeholder describing where the value
// COMES FROM is read as licence to use whatever the user said. Worded as "the
// exact value the user gave", every CRM trial stamped a company's display NAME
// in as its record id, because the name is exactly what the user gave. It reads
// correctly only for a type whose id the user happens to hand you, like a repo
// URL.
//
// So the placeholder names the property an id must have, and shows the deferral
// inline. The trailing prose says this too, and prose loses to the example —
// whatever is inside the JSON is what comes back.
func slotLine(resourceType, why string) string {
	return "{\"type\":\"" + resourceType +
		"\",\"id\":\"<its real id — OMIT id entirely if you must look it up first>\",\"why\":\"" + why + "\"}"
}

// capped trims a list to n, for an example that shows a shape rather than
// restating the whole ceiling the agent can already read above it.
func capped(ds []permsurface.Descriptor, n int) []permsurface.Descriptor {
	if len(ds) > n {
		return ds[:n]
	}
	return ds
}

// dependedRead picks the ONE read to carry into the acting phase: the
// permission actually named "read" when the type declares one, else the first.
//
// Not every readonly permission is a dependency of a write. git declares fetch
// as readonly, but fetching is a remote operation that has nothing to do with
// committing — carrying it into the acting phase would teach a phase to claim
// reach it does not need, which is the opposite of the point.
func dependedRead(reads []permsurface.Descriptor, resourceType string) (permsurface.Descriptor, bool) {
	onT := onType(reads, resourceType)
	if len(onT) == 0 {
		return permsurface.Descriptor{}, false
	}
	for _, d := range onT {
		if d.Permission == "read" {
			return d, true
		}
	}
	return onT[0], true
}

// AuthoredExample is one worked plan the AgentClass author supplied
// (spec.authz.planGate.examples), flattened for rendering.
//
// Mirrored into the runner rather than passing the CRD type through, so this
// package stays off the v1alpha1 import path for prompt composition — same
// reason SkillMetadata and AssetKind are mirrored.
type AuthoredExample struct {
	Task   string
	Phases []AuthoredExamplePhase
}

// AuthoredExamplePhase is one phase of an authored example.
type AuthoredExamplePhase struct {
	ID          string
	Label       string
	Permissions []string
	Slots       []string
}

// authoredExamples renders the class author's own worked plans, after the
// derived one.
//
// After, not instead: the derived example is the one guaranteed to speak this
// session's handles, and it is what carries the read-alongside-write shape.
// An authored example is for a class whose good plan is not obvious from its
// surface — where phase ORDER matters, or two types must be named together.
//
// The handles here were checked at admission (validatePlanExamples), so a class
// that reaches this point cannot be teaching a handle the agent may not
// declare. Slots render as TYPES with no id: an authored example must never
// carry a concrete instance, because a planner that copies one would address
// an object belonging to whoever the example was written about.
func authoredExamples(exs []AuthoredExample) string {
	if len(exs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("  • Worked examples for this agent specifically:\n")
	for _, ex := range exs {
		b.WriteString("      When asked to " + ex.Task + ":\n")
		b.WriteString("      \"phases\":[")
		for i, ph := range ex.Phases {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("{\"id\":\"" + ph.ID + "\"")
			if ph.Label != "" {
				b.WriteString(",\"label\":\"" + ph.Label + "\"")
			}
			if len(ph.Permissions) > 0 {
				parts := make([]string, 0, len(ph.Permissions))
				for _, h := range ph.Permissions {
					parts = append(parts, "{\"handle\":\""+h+"\"}")
				}
				b.WriteString(",\"permissions\":[" + strings.Join(parts, ",") + "]")
			}
			if len(ph.Slots) > 0 {
				parts := make([]string, 0, len(ph.Slots))
				for _, s := range ph.Slots {
					parts = append(parts, "{\"type\":\""+s+"\"}")
				}
				b.WriteString(",\"slots\":[" + strings.Join(parts, ",") + "]")
			}
			b.WriteString("}")
		}
		b.WriteString("]\n")
	}
	b.WriteString("      Fill in each permission's \"why\" and each slot's \"id\" yourself — the shapes above show WHICH to declare, not what to say about them.\n")
	return b.String()
}
