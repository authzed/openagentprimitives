package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta/capability"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// FineGrainedInfoLeakageActive reports whether a class granted
// fine_grained_info_leakage (and its config parses). It is the single question
// the prompt block and the hook wiring both ask, so they cannot disagree about
// whether this session emits pt markup. A malformed config reads as inactive —
// the AgentClass controller surfaces the fault as CapabilitiesValid.
func FineGrainedInfoLeakageActive(class *spiceboxv1alpha1.AgentClass) bool {
	if class == nil {
		return false
	}
	_, active, err := capability.ActiveWithConfig(class, capability.FineGrainedInfoLeakageName)
	return active && err == nil
}

// fineGrainedLeakageDeps wires the per-datum half of the info-leakage check.
//
// Returns nil — leaving the coarse session-wide path in sole charge, exactly
// as before — unless the class granted fine_grained_info_leakage. Nil is the
// answer for every existing agent, and that is deliberate: per-datum tracking
// costs a LookupSubjects per tool call plus durable storage per datum, and it
// changes what the disclosure gate permits.
func (l *Loop) fineGrainedLeakageDeps() *hooks.FineGrainedDeps {
	if l.AgentClass == nil || l.SpiceDBLookupSubjects == nil {
		return nil
	}
	_, active, err := capability.ActiveWithConfig(l.AgentClass, capability.FineGrainedInfoLeakageName)
	if err != nil {
		// Never drop it: a malformed capability config means the operator
		// asked for per-datum provenance and is about to silently run coarse.
		// The AgentClass controller surfaces the same fault as
		// CapabilitiesValid, but the runner is where the consequence lands.
		slog.Default().Info("info-leakage: fine-grained capability config is invalid; running the coarse session-wide check instead",
			"class", l.AgentClass.Name, "err", err.Error())
		return nil
	}
	if !active {
		return nil
	}
	return &hooks.FineGrainedDeps{
		Enabled: func(context.Context) bool { return true },

		// A tag's audience is `reader` on the pt_tag object, which resolves
		// the intersection across the whole derivation tree inside SpiceDB.
		// Asking for the fully-resolved permission rather than reading
		// direct_reader tuples is what keeps the arrow semantics — and the
		// live-ness they buy — as the single source of truth: revoking access
		// to a source narrows every tag derived from it at the next check,
		// with no invalidation pass here.
		TagReaders: func(ctx context.Context, tagID string) ([]string, error) {
			return l.SpiceDBLookupSubjects(ctx, "pt_tag:"+tagID, "reader")
		},

		DestinationAudience: l.destinationAudience,

		// The runner owns the envelope format AND can read the content store, so
		// it hands the hook a parser that also does CONTENT BINDING — not just
		// nonce-matched parsing. A matched nonce proves the boundary; it does not
		// prove the id belongs to these bytes (the model authors the reply and can
		// pair a witnessed wide id with fabricated content). So each region's
		// content is verified against what the platform stored for that id; a
		// mismatch drops the whole payload to the coarse floor. Fail-closed on any
		// read error.
		ParsePayloadTags: l.verifyPayloadTags,

		// The runner owns the MCP/sandbox envelope format and the tools' schemas,
		// so it extracts the outbound CONTENT the coverage check must run against
		// — the string args minus the routing args the tool's Check consumes.
		OutboundContent: l.outboundContent,
	}
}

// verifyPayloadTags parses a payload's nonce-matched pt-untrusted regions and
// returns the ids whose content BINDS to what the platform stored for them,
// with fullyCovered true only when the payload is fully region-covered AND every
// region binds. This is the content-binding check that closes the "fabricate a
// region under a witnessed wide id" bypass.
//
// The PARSE happens here — extracting the regions from the payload the model
// authored is not privileged — but the BIND happens at the operator, over
// PtTagVerify. pt_tag_content is component-read (SessionReadable false), so this
// session-credentialed runner cannot read it to compare for itself; and doing
// the compare here would let a compromised runner declare a fabricated region a
// match. So the operator holds the bytes and answers which ids bound.
func (l *Loop) verifyPayloadTags(ctx context.Context, payload string) ([]string, bool) {
	regions, covered := toolenvelope.PtRegions(payload)
	if !covered || len(regions) == 0 {
		return nil, false
	}
	if l.PtTagVerify == nil {
		return nil, false // cannot verify → coarse floor
	}
	wire := make([]memory.PtTagRegion, 0, len(regions))
	for _, reg := range regions {
		wire = append(wire, memory.PtTagRegion{ID: reg.ID, Content: reg.Content})
	}
	resp, err := l.PtTagVerify(ctx, wire)
	if err != nil {
		return nil, false // fail-closed: unverifiable → coarse floor
	}
	if !resp.AllBound {
		// A region whose content did not match its claimed id is a
		// fabricated/mislabelled tag. Drop the whole payload to the coarse floor
		// rather than honour the ids that happened to match — a partially forged
		// payload is not one we can reason about per-datum.
		return nil, false
	}
	return resp.Verified, true
}

// destinationAudience answers who will be able to read the object a tool call
// sends data to.
//
// The destination is DERIVED from the authorization the call already performs
// rather than from a new declaration. A tool whose StateImpact is readwrite or
// external must carry a Check, and authz.ResolveResourceID resolves that
// Check's resource id from the very same args — so the object the call is
// authorized against IS the object the data lands in. Deriving it keeps one
// declaration instead of two that can disagree, with no way to tell which is
// right.
//
// Returns resolved=false for anything it cannot establish. The caller treats
// that as UNKNOWN — not as an empty audience, which would be vacuously safe —
// and applies the session's information-leakage Mode, the same dial an
// unmapped ToolReads already answers to.
func (l *Loop) destinationAudience(ctx context.Context, toolName string, args map[string]any) ([]string, bool, error) {
	if l.SpiceDBLookupSubjects == nil {
		return nil, false, nil
	}
	// MCP and sandbox tools wrap their input in the {operation_id, _reason, args}
	// envelope, and a Check's resourceIDExpr (and any PerCallPermission `when`) is
	// written against the INNER args — exactly as the dispatch-time authz path
	// unwraps them (loop_dispatch.go → unwrapToolArgs). Unwrap here too: without
	// it string(args.board_id) evaluates against the outer envelope, the
	// destination id never resolves, and the leak gate blocks every egress call
	// with "audience could not be determined" (observed live on post_to_board).
	args = unwrapToolArgs(args)
	t, ok := l.lookupTool(toolName)
	if !ok {
		return nil, false, nil
	}
	perm, err := l.permissionForTool(t, args)
	if err != nil {
		return nil, false, err
	}
	// Only a state-CHANGING call has a destination this derivation can find.
	// A readonly tool also carries its arguments outward — a query string can
	// contain tagged data — but its Check names what it reads FROM, not where
	// the data goes, so there is nothing here to measure against. Unknown, and
	// the Mode decides.
	if perm.StateImpact != authz.Readwrite && perm.StateImpact != authz.External {
		return nil, false, nil
	}
	if perm.Check == nil {
		return nil, false, nil
	}
	id, err := authz.ResolveResourceID(*perm.Check, args)
	if err != nil || id == "" {
		return nil, false, err
	}

	// The audience is who can READ the destination, which is NOT the Check's
	// own permission. A Check names what the CALLER needs — "write", "admin" —
	// and expanding that would return the writers, a strictly smaller set than
	// the readers. Smaller is the dangerous direction here: the safety rule is
	// audience ⊆ readers, so a too-small audience passes checks it should not.
	//
	// So the read permission comes from the declarations that already name one
	// for this resource type: the ToolReads mappings. Derived from what an
	// operator already wrote, never guessed from the permission's name.
	readPerm, ok := l.readPermissionForResourceType(perm.Check.ResourceType)
	if !ok {
		return nil, false, nil
	}
	subs, err := l.SpiceDBLookupSubjects(ctx, perm.Check.ResourceType+":"+id, readPerm)
	if err != nil {
		return nil, false, err
	}
	// An empty subject set is UNKNOWN, not "an audience of nobody".
	//
	// LookupSubjects answers empty for two situations this call cannot tell
	// apart: a destination that genuinely has no readers, and an object id
	// that is not modelled at all — SpiceDB does not error on an unknown
	// object, so a mistyped or unmodelled id looks exactly like a real one
	// nobody can read.
	//
	// Reporting that as resolved was a fail-open, and a quiet one: an empty
	// audience is unauthorized-by-nobody, so provenance.Unauthorized returns
	// nothing and BOTH the per-datum and the coarse floor allow the send.
	// That is the reading this hook's own contract forbids — "returning one
	// for 'I don't know' would read as permission to send anywhere".
	//
	// Neither reading justifies allowing: a destination with no readers is
	// not one worth trusting data to either. So report unresolved and let the
	// Mode decide, which is what every other unresolvable destination does.
	if len(subs) == 0 {
		return nil, false, nil
	}
	return subs, true, nil
}

// readPermissionForResourceType returns the permission an existing ToolReads
// declaration names for this resource type — the type's established "who may
// see one of these".
//
// Scans the mappings rather than taking a per-tool answer because the question
// is about the TYPE, not about any one tool: whoever declared how to read a
// linear_issue already answered it, and a second answer here could disagree
// with the coarse path's own audience checks on the same type.
func (l *Loop) readPermissionForResourceType(resourceType string) (string, bool) {
	if l.LookupToolMapping == nil || resourceType == "" {
		return "", false
	}
	// Snapshot the slice header under toolsMu. This runs OFF the Run
	// goroutine — the NATS app-tool handler reaches it through
	// handleAppToolCallReq → executeToolContained → the InfoLeakAudience hook
	// → destinationAudience — while Run may be assigning l.Tools from
	// applyToolRefresh when a secret-gated sidecar is re-synthesized
	// mid-session. A slice header is not written atomically, so an unlocked
	// read can see a new pointer with a stale length: a torn read inside the
	// per-datum egress gate's audience derivation, or a panic in the handler.
	l.toolsMu.RLock()
	tools := l.Tools
	l.toolsMu.RUnlock()
	for _, t := range tools {
		m := l.LookupToolMapping(t.Name())
		if m == nil || m.Reads == nil {
			continue
		}
		if m.Reads.ResourceType == resourceType && m.Reads.Permission != "" {
			return m.Reads.Permission, true
		}
	}
	return "", false
}

// celArgRef / tmplArgRef pick the arg names a Check references — `args.NAME` in a
// CEL resourceIDExpr, `{NAME}` in a resourceIDTemplate. Those are the ROUTING
// args: the destination id the check resolves the call against. They are what
// outboundContent excludes, so a routing param (a board id, an issue key) may be
// untagged while any OTHER untagged string still breaks coverage.
var (
	celArgRef  = regexp.MustCompile(`args\.([A-Za-z_][A-Za-z0-9_]*)`)
	tmplArgRef = regexp.MustCompile(`\{([A-Za-z_][A-Za-z0-9_]*)\}`)
)

func checkReferencedArgNames(c *authz.PermissionCheck) map[string]bool {
	refs := map[string]bool{}
	if c == nil {
		return refs
	}
	for _, m := range celArgRef.FindAllStringSubmatch(c.ResourceIDExpr, -1) {
		refs[m[1]] = true
	}
	for _, m := range tmplArgRef.FindAllStringSubmatch(c.ResourceIDTemplate, -1) {
		refs[m[1]] = true
	}
	return refs
}

// outboundContent is FineGrainedDeps.OutboundContent: it extracts the DATA an
// egress tool call carries out, so the coverage check runs against the content
// and not the wire JSON.
//
// in.Tool.Args is the wire form. An MCP/sandbox tool wraps its input in the
// {operation_id,_reason,args:{…}} envelope, and any pt-untrusted markup the model
// carried into a content field is JSON-ESCAPED there — so scanning the raw bytes
// finds no regions and every enveloped egress tool would fall to the coarse
// floor. Here the runner unwraps the envelope (it owns that format) and joins the
// model-filled STRING args, EXCLUDING the routing args the tool's own Check
// consumes (checkReferencedArgNames). The excluded destination arg may thus stay
// untagged; every remaining string must be tagged or coverage fails — which is
// what keeps "untagged data out" from becoming a silent per-datum pass.
//
// Non-JSON args (a plain reply payload, or unparseable bytes) are returned as-is;
// the caller scans them directly.
func (l *Loop) outboundContent(_ context.Context, toolName string, rawArgs []byte) string {
	var raw map[string]any
	if len(rawArgs) == 0 || json.Unmarshal(rawArgs, &raw) != nil {
		return string(rawArgs)
	}
	args := unwrapToolArgs(raw)

	routing := map[string]bool{}
	if t, ok := l.lookupTool(toolName); ok {
		if perm, err := l.permissionForTool(t, args); err == nil {
			routing = checkReferencedArgNames(perm.Check)
		}
	}

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order so coverage does not depend on map iteration
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		// Routing exclusion is TOP-LEVEL ONLY. The Check consumes this
		// argument, so it may stay untagged — but a nested field that happens
		// to share its name is ordinary content, and excluding it by name at
		// any depth would hand the model a way to opt data out of measurement
		// by choosing a key.
		if routing[k] {
			continue
		}
		parts = appendStrings(parts, args[k])
	}
	return strings.Join(parts, "\n")
}

// appendStrings walks v and appends every string it contains, at any depth.
//
// The walk is recursive because coverage measures what the call carries OUT,
// and a string is carried out whether it sits at the top level or inside
// blocks[].text. Scanning only top-level values let one tagged top-level
// string make the whole payload look covered while the narrow datum rode out
// nested underneath it, under the top-level tags' audience — a silent pass
// against this function's own rule that every remaining string must be tagged
// or coverage fails.
//
// Only strings matter: numbers and booleans carry no pt-untrusted markup, and
// map KEYS are structure the model does not use to smuggle content.
func appendStrings(out []string, v any) []string {
	switch t := v.(type) {
	case string:
		return append(out, t)
	case []any:
		for _, e := range t {
			out = appendStrings(out, e)
		}
		return out
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // same determinism as the top level
		for _, k := range keys {
			out = appendStrings(out, t[k])
		}
		return out
	default:
		return out
	}
}
