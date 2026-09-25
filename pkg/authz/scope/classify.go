package scope

import (
	"fmt"
	"path/filepath"
)

// SkippedReason categorizes a request fragment that authzd refused
// to include in the applied delta. Set by ClassifySkipped, surfaced
// in approval blocks and audit records.
type SkippedReason string

const (
	ReasonOutOfEnvelopeTool     SkippedReason = "out_of_envelope_tool"
	ReasonOutOfEnvelopeRtype    SkippedReason = "out_of_envelope_resource_type"
	ReasonOutOfEnvelopePerm     SkippedReason = "out_of_envelope_permission_level"
	ReasonRequesterLacksPerm    SkippedReason = "requester_lacks_perm"
	ReasonAmbiguous             SkippedReason = "ambiguous"
	ReasonConflictsExistingDeny SkippedReason = "conflicts_with_existing_disallow"
	ReasonUnparseable           SkippedReason = "unparseable"
)

// SkippedItem is one fragment of the requested delta that was
// excluded. Explanation is LLM-composed downstream; Reason is
// always set by deterministic classification here.
type SkippedItem struct {
	// RequestFragment is the piece of the user's ask that was not applied.
	RequestFragment string `json:"requestFragment"`
	// Reason is the deterministic category; always set.
	Reason SkippedReason `json:"reason"`
	// Explanation is LLM-composed prose; empty until the composer fills it.
	Explanation string `json:"explanation,omitempty"`
}

// AgentClassEnvelope is the immutable static view of an AgentClass's
// spec.authz block that the classifier and caveat detector need.
// Populated by authzd before invoking the extractor.
type AgentClassEnvelope struct {
	Tools         []EnvelopeTool
	BoundEntities []EnvelopeBoundEntity
}

type EnvelopeTool struct {
	Name        string
	Description string
}

type EnvelopeBoundEntity struct {
	ResourceType string
	Permission   string
	Description  string
}

// HasBoundEntityType reports whether the AgentClass declares the type.
func (e AgentClassEnvelope) HasBoundEntityType(t string) bool {
	for _, be := range e.BoundEntities {
		if be.ResourceType == t {
			return true
		}
	}
	return false
}

// PermFor returns the SpiceDB permission name the AgentClass declared
// for the given resource type (empty string if not declared).
func (e AgentClassEnvelope) PermFor(t string) string {
	for _, be := range e.BoundEntities {
		if be.ResourceType == t {
			return be.Permission
		}
	}
	return ""
}

// MatchesToolName reports whether toolName matches any tool in the
// envelope. Exact match OR a glob in the input that matches an
// envelope tool's exact name.
func (e AgentClassEnvelope) MatchesToolName(toolName string) bool {
	for _, t := range e.Tools {
		if t.Name == toolName {
			return true
		}
	}
	return false
}

// RequesterPerms summarizes what the requester can do in SpiceDB.
// Populated by authzd via LookupResources. Empty map → no perms.
type RequesterPerms struct {
	AllowedIDsByType map[string]map[string]bool
}

func (rp RequesterPerms) HasPerm(rtype, id string) bool {
	t, ok := rp.AllowedIDsByType[rtype]
	if !ok {
		return false
	}
	return t[id]
}

// ClassifySkipped validates each item in `proposed` against the
// envelope and requester perms, returning (applied, skipped).
func ClassifySkipped(proposed ScopeDelta, env AgentClassEnvelope, perms RequesterPerms) (ScopeDelta, []SkippedItem) {
	var applied ScopeDelta
	var skipped []SkippedItem

	classifyResources := func(src []ResourceRef, requirePerm bool) (kept []ResourceRef) {
		for _, ref := range src {
			if !env.HasBoundEntityType(ref.ResourceType) {
				skipped = append(skipped, SkippedItem{
					RequestFragment: ref.String(),
					Reason:          ReasonOutOfEnvelopeRtype,
				})
				continue
			}
			if requirePerm && !perms.HasPerm(ref.ResourceType, ref.ID) {
				skipped = append(skipped, SkippedItem{
					RequestFragment: ref.String(),
					Reason:          ReasonRequesterLacksPerm,
				})
				continue
			}
			kept = append(kept, ref)
		}
		return kept
	}

	classifyTools := func(src []string) (kept []string) {
		for _, tool := range src {
			if env.MatchesToolName(tool) || globMatchesAnyEnvelopeTool(env, tool) {
				kept = append(kept, tool)
				continue
			}
			skipped = append(skipped, SkippedItem{
				RequestFragment: fmt.Sprintf("tool %s", tool),
				Reason:          ReasonOutOfEnvelopeTool,
			})
		}
		return kept
	}

	// Add requires requester perm on resources (widening = grant the agent
	// access to something the requester has).
	applied.Add.Resources = classifyResources(proposed.Add.Resources, true)
	applied.Add.Tools = classifyTools(proposed.Add.Tools)
	applied.Add.ResourcePatterns = append(applied.Add.ResourcePatterns, proposed.Add.ResourcePatterns...)
	applied.Add.ArgConstraints = append(applied.Add.ArgConstraints, proposed.Add.ArgConstraints...)

	// Remove is purely subtractive — no perm check, no envelope check on resources.
	applied.Remove.Resources = append(applied.Remove.Resources, proposed.Remove.Resources...)
	applied.Remove.Tools = append(applied.Remove.Tools, proposed.Remove.Tools...)
	applied.Remove.ResourcePatterns = append(applied.Remove.ResourcePatterns, proposed.Remove.ResourcePatterns...)
	applied.Remove.ArgConstraints = append(applied.Remove.ArgConstraints, proposed.Remove.ArgConstraints...)

	// HardDeny does not require requester perm — the approver can deny anything.
	// Envelope check still applies for resource type (can't deny a type that doesn't exist).
	applied.HardDeny.Resources = classifyResources(proposed.HardDeny.Resources, false)
	applied.HardDeny.Tools = classifyTools(proposed.HardDeny.Tools)
	applied.HardDeny.ResourcePatterns = append(applied.HardDeny.ResourcePatterns, proposed.HardDeny.ResourcePatterns...)
	applied.HardDeny.ArgConstraints = append(applied.HardDeny.ArgConstraints, proposed.HardDeny.ArgConstraints...)

	return applied, skipped
}

func globMatchesAnyEnvelopeTool(env AgentClassEnvelope, pattern string) bool {
	for _, t := range env.Tools {
		ok, err := filepath.Match(pattern, t.Name)
		if err == nil && ok {
			return true
		}
	}
	return false
}
