package permsurface

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

// Candidate is one tool's contribution to the surface, projected by the
// caller. permsurface deliberately does not import a tool package; the runner
// projects []tool.Tool into []Candidate, and tests construct these directly.
type Candidate struct {
	ToolName   string
	Permission authz.Permission
	Variants   []authz.PermissionVariant
}

// Provenance records what produces a handle. Display-only: consumed by the
// prompt rendering and the approval card, ignored by enforcement.
type Provenance struct {
	// Tool is the LLM-visible tool name.
	Tool string
	// Condition is the variant's CEL When; "" for a tool's base permission.
	Condition string
}

// Descriptor is the resolution of a Handle against a specific envelope.
// NEVER persisted — recomputed on demand. Only Handle persists.
type Descriptor struct {
	Handle       Handle
	Permission   string // "" for tool: handles
	ResourceType string // "" for tool: handles
	ToolName     string // "" for perm: handles
	// StateImpact is the MAX severity across every producer of this handle.
	StateImpact authz.StateImpact
	// ConstantResourceID is the object id this handle names on EVERY call, when
	// the check can be resolved with no tool args — git.yaml keys git_repo
	// read/write on the literal "workspace", the session's own checked-out copy.
	// Empty when the id depends on args (a {placeholder} template or any CEL
	// expr), or when two producers of this handle disagree about it.
	//
	// It exists so an approval card can name an instance a phase will reach that
	// the agent never declared as a slot: a phase declaring git_repo:<remote URL>
	// also reads and writes git_repo:workspace, and a card showing only the
	// declared slot understates what approving it covers.
	ConstantResourceID string
	Via                []Provenance
}

// severity orders StateImpact for max-merge and sorting. stateless and
// passthrough score 0 and are excluded from the surface entirely — tool
// authors already made that judgment when choosing a StateImpact.
func severity(s authz.StateImpact) int {
	switch s {
	case authz.Readonly:
		return 1
	case authz.Readwrite:
		return 2
	case authz.External:
		return 3
	default:
		return 0
	}
}

// Enumerate maps candidates onto the deduplicated, deterministically-ordered
// surface.
//
// Fail-closed: a candidate whose components cannot mint a handle is EXCLUDED.
// tool.ValidateEnvelope surfaces such a tool at startup, and the runner refuses
// to start on one WHEN THE PLAN GATE IS ENFORCING (tool.FatalFor) — so a tool
// absent from this surface can no longer be dispatched past a gate that
// enforces against it.
//
// That escalation is why the exclusion above is safe. Without it an
// unhandleable tool was absent from the surface yet still callable, and the
// gate's own no-handle branch records OutcomeAllow — so the incompleteness of
// the surface was precisely what escaped enforcement. Under a disabled or
// logging gate it remains a warning, because nothing is being enforced against
// and refusing to start would be a regression for that cluster.
func Enumerate(cs []Candidate) []Descriptor {
	byHandle := map[Handle]*Descriptor{}
	// A handle can have several producers. They must AGREE on the constant id or
	// it is not one: claiming one tool's literal would name an instance the other
	// never touches. Tracked alongside rather than on the Descriptor so "no
	// producer set one" stays distinguishable from "producers disagreed".
	constSeen := map[Handle]string{}
	constConflict := map[Handle]bool{}

	add := func(toolName string, p authz.Permission, condition string) {
		if severity(p.StateImpact) == 0 {
			return
		}
		var (
			h                          Handle
			err                        error
			permission, resType, tName string
		)
		if p.Check != nil && p.Check.Permission != "" && p.Check.ResourceType != "" {
			h, err = NewPermHandle(p.Check.Permission, p.Check.ResourceType)
			permission, resType = p.Check.Permission, p.Check.ResourceType
		} else {
			h, err = NewToolHandle(toolName)
			tName = toolName
		}
		if err != nil {
			return // fail-closed; startup validation is what makes this safe
		}
		d, ok := byHandle[h]
		if !ok {
			d = &Descriptor{
				Handle:             h,
				Permission:         permission,
				ResourceType:       resType,
				ToolName:           tName,
				StateImpact:        p.StateImpact,
				ConstantResourceID: constantResourceID(p.Check),
			}
			byHandle[h] = d
			constSeen[h] = d.ConstantResourceID
		} else if constSeen[h] != constantResourceID(p.Check) {
			constConflict[h] = true
		}
		if severity(p.StateImpact) > severity(d.StateImpact) {
			d.StateImpact = p.StateImpact
		}
		d.Via = append(d.Via, Provenance{Tool: toolName, Condition: condition})
	}

	for _, c := range cs {
		// Variants first so their conditions lead the provenance list, matching
		// the dispatcher's first-match-wins evaluation order.
		for _, v := range c.Variants {
			add(c.ToolName, v.Check, v.When)
		}
		add(c.ToolName, c.Permission, "")
	}

	for h := range constConflict {
		byHandle[h].ConstantResourceID = ""
	}

	out := make([]Descriptor, 0, len(byHandle))
	for _, d := range byHandle {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if sa, sb := severity(a.StateImpact), severity(b.StateImpact); sa != sb {
			return sa > sb // most severe first
		}
		if a.ResourceType != b.ResourceType {
			return a.ResourceType < b.ResourceType
		}
		if a.Permission != b.Permission {
			return a.Permission < b.Permission
		}
		return a.ToolName < b.ToolName
	})
	return out
}

// Resolve returns the descriptor for h against cs. ok=false means h is INERT
// against this envelope: the tool that justified it is gone (revoked, toolkit
// removed) or was never present. Callers must treat inert as "not allowed" —
// never as "unconstrained".
func Resolve(h Handle, cs []Candidate) (Descriptor, bool) {
	for _, d := range Enumerate(cs) {
		if d.Handle == h {
			return d, true
		}
	}
	return Descriptor{}, false
}

// Digest is a stable fingerprint of what the surface ENFORCES: the sorted
// (Handle, StateImpact) pairs, and nothing else.
//
// Via is deliberately excluded. It is display-only and churns on a cosmetic
// tool rename; including it would raise spurious drift alarms against an
// approved plan. The digest must move when, and only when, what is enforceable
// moves.
func Digest(ds []Descriptor) string {
	pairs := make([]string, 0, len(ds))
	for _, d := range ds {
		pairs = append(pairs, d.Handle.String()+"\x00"+string(d.StateImpact))
	}
	// Sort defensively: Enumerate already returns sorted output, but Digest
	// must not depend on a caller preserving that.
	sort.Strings(pairs)

	sum := sha256.New()
	for _, p := range pairs {
		io.WriteString(sum, p)
		io.WriteString(sum, "\x00")
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// constantResourceID resolves a check's object id when it needs NO tool args,
// and returns "" whenever it needs any.
//
// Three ways an id is not plan-time constant, all of which must yield "":
// a CEL expr (it reads args by construction — git.yaml's fetch/push do), a
// template carrying a {placeholder}, and a template that fails to resolve.
// Returning the raw template on any of those would put a literal brace on an
// approval card as though it were an object id.
//
// The transforms run here too, so the value matches what the check will
// actually compute and hand to SpiceDB rather than a pre-normalized lookalike.
func constantResourceID(c *authz.PermissionCheck) string {
	if c == nil || c.ResourceIDExpr != "" || c.ResourceIDTemplate == "" {
		return ""
	}
	if strings.ContainsRune(c.ResourceIDTemplate, '{') {
		return ""
	}
	id, err := authz.ResolveTemplate(c.ResourceIDTemplate, nil, c.ResourceIDTransforms)
	if err != nil {
		return ""
	}
	return id
}
