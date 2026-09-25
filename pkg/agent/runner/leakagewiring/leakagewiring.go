// Package leakagewiring contains the helpers internal/cmd/runner and the e2e
// in-process runner factory both use to wire the info-leakage gate
// (`Loop.LookupToolMapping`, `Loop.RequesterCanonicalID`, `Loop.SpiceDBCheck`,
// `Loop.ChannelKindImpl`). One source of truth means the e2e tests exercise
// the production wiring path — if the helpers diverge between binaries,
// the tests catch it.
package leakagewiring

import (
	"context"
	"fmt"
	"log/slog"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	slackkind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/validator"
)

// LookupMCPToolResourceMapping resolves an LLM-facing tool name (e.g.
// "linear_get_issue") back to the MCPServer's bare-named
// ToolResourceMapping entry (declared as `tool: get_issue` in YAML).
//
// The translation reapplies the same `<prefix>_<rawName>` join +
// NormalizeName transform that synthesize.Build uses when constructing
// the LLM-facing names. Without this, the info-leakage hook looks up
// "linear_get_issue" against the bare-name keys in the toolResourceMap
// and finds nothing, then errors out as if the tool were unmapped.
func LookupMCPToolResourceMapping(
	ctx context.Context,
	c client.Client,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassMCPServerRef,
	llmToolName string,
) *spiceboxv1alpha1.ToolResourceMapping {
	for _, ref := range refs {
		var cr spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Ref}, &cr); err != nil {
			slog.Default().Info("info-leakage: MCPServer lookup failed; tool may appear unmapped",
				"mcpserver", ref.Ref, "namespace", namespace, "err", err.Error())
			continue
		}
		for i := range cr.Spec.ToolResourceMap {
			entry := &cr.Spec.ToolResourceMap[i]
			expected := synthesize.NormalizeName(ref.Name + "_" + entry.Tool)
			if expected == llmToolName {
				return entry
			}
		}
	}
	return nil
}

// UntrustedSourceEnum is the SEP-1913 `source` value that marks a tool's
// result as content the agent must not treat as instructions.
const UntrustedSourceEnum = "untrustedPublic"

// LookupMCPToolUntrustedSource answers whether a tool's result may carry
// untrusted public content, from the server's own SEP-1913
// `returnMetadata.source` declaration on the MCPServer CR.
//
// This is the INTEGRITY axis of a minted pt-tag — the trifecta's leg A — and
// it is read from a declaration that already exists rather than from a new
// field. Note the difference from `deny.trust.sourceUntrustedPublic`, which
// consumes the same signal: that one is an opt-in REFUSAL, decided per spec.
// This one is a FACT about the datum, recorded whether or not the spec chose
// to deny on it, because a tag that omitted the mark would launder untrusted
// data into a handoff that was never allowed to carry it.
//
// FAILS CLOSED, in the direction that means "untrusted". An unparseable
// metadata blob — truncated, or attacker-corrupt from a server that controls
// its own tools/list — cannot be read as a clean source: the same reasoning
// denyTrustAxis gives, and here the conservative answer is to mark the datum,
// which can only make a later gate more likely to fire.
//
// A tool with no declaration at all is NOT untrusted. Absence is the norm
// (SEP-1913 metadata is optional), and treating every unannotated read as
// untrusted would mark essentially every datum and make the axis meaningless.
func LookupMCPToolUntrustedSource(
	ctx context.Context,
	c client.Client,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassMCPServerRef,
	llmToolName string,
) bool {
	for _, ref := range refs {
		var cr spiceboxv1alpha1.MCPServer
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Ref}, &cr); err != nil {
			slog.Default().Info("info-leakage: MCPServer lookup failed; cannot read the tool's source declaration",
				"mcpserver", ref.Ref, "namespace", namespace, "err", err.Error())
			continue
		}
		for i := range cr.Spec.Tools {
			t := &cr.Spec.Tools[i]
			if synthesize.NormalizeName(ref.Name+"_"+t.Name) != llmToolName {
				continue
			}
			if t.Trust.ReturnMetadata == nil {
				return false
			}
			match, parseOK := validator.ContainsEnumFieldChecked(
				t.Trust.ReturnMetadata.Raw, "source", UntrustedSourceEnum)
			if !parseOK {
				slog.Default().Info("info-leakage: tool's SEP-1913 returnMetadata did not parse; treating its output as untrusted",
					"tool", llmToolName, "mcpserver", ref.Ref, "namespace", namespace)
				return true
			}
			return match
		}
	}
	return false
}

// LookupSidecarToolboxToolResourceMapping is the SidecarToolbox sibling of
// LookupMCPToolResourceMapping: it resolves an LLM-facing tool name back to a
// bound SidecarToolbox's ToolResourceMapping entry, using the identical
// `<prefix>_<rawName>` + NormalizeName translation. The runner consults it as a
// fallback when the MCPServer lookup returns nil, so a declared sidecar tool
// participates in per-datum egress rather than falling to the undeclared floor.
func LookupSidecarToolboxToolResourceMapping(
	ctx context.Context,
	c client.Client,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassSidecarToolboxRef,
	llmToolName string,
) *spiceboxv1alpha1.ToolResourceMapping {
	for _, ref := range refs {
		var cr spiceboxv1alpha1.SidecarToolbox
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Ref}, &cr); err != nil {
			slog.Default().Info("info-leakage: SidecarToolbox lookup failed; tool may appear unmapped",
				"sidecartoolbox", ref.Ref, "namespace", namespace, "err", err.Error())
			continue
		}
		for i := range cr.Spec.ToolResourceMap {
			entry := &cr.Spec.ToolResourceMap[i]
			if synthesize.NormalizeName(ref.Name+"_"+entry.Tool) == llmToolName {
				return entry
			}
		}
	}
	return nil
}

// LookupSidecarToolboxToolUntrustedSource is the SidecarToolbox sibling of
// LookupMCPToolUntrustedSource, reading the same SEP-1913
// `returnMetadata.source` declaration off the toolbox's tools. Same fail-closed
// posture (unparseable → untrusted; absent → not untrusted).
func LookupSidecarToolboxToolUntrustedSource(
	ctx context.Context,
	c client.Client,
	namespace string,
	refs []spiceboxv1alpha1.AgentClassSidecarToolboxRef,
	llmToolName string,
) bool {
	for _, ref := range refs {
		var cr spiceboxv1alpha1.SidecarToolbox
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Ref}, &cr); err != nil {
			slog.Default().Info("info-leakage: SidecarToolbox lookup failed; cannot read the tool's source declaration",
				"sidecartoolbox", ref.Ref, "namespace", namespace, "err", err.Error())
			continue
		}
		for i := range cr.Spec.Tools {
			t := &cr.Spec.Tools[i]
			if synthesize.NormalizeName(ref.Name+"_"+t.Name) != llmToolName {
				continue
			}
			if t.Trust.ReturnMetadata == nil {
				return false
			}
			match, parseOK := validator.ContainsEnumFieldChecked(
				t.Trust.ReturnMetadata.Raw, "source", UntrustedSourceEnum)
			if !parseOK {
				slog.Default().Info("info-leakage: sidecar tool's SEP-1913 returnMetadata did not parse; treating its output as untrusted",
					"tool", llmToolName, "sidecartoolbox", ref.Ref, "namespace", namespace)
				return true
			}
			return match
		}
	}
	return false
}

// RequesterSubjectRef converts a canonical identity (the base64-email or
// kind:team:externalID form produced by identity.Principal.Canonical()) into a
// SpiceDB-ready subject reference of the form "user:<canonical>". Returns
// "" when canonical is empty (kubectl-driven sessions with no channel
// identity).
//
// The leakage hook's Loop.RequesterCanonicalID is documented to return a
// SpiceDB subject ref; without this prefix the downstream SpiceDB.SplitObject
// call rejects the value as "must be of the form type:id".
//
// It delegates to identity.CanonicalUserID.SubjectRef rather than prefixing
// directly, because not every canonical reaching here is a human's: a session
// whose inbound carried no starting user acts as its input Channel's declared
// "service:<id>", which already names its own object type. SubjectRef is the
// one place that discriminator lives.
func RequesterSubjectRef(canonical string) string {
	// Formatting only: the caller resolved this canonical, and SubjectRef makes
	// no trust decision.
	return identity.CanonicalFromTrusted(canonical,
		"canonical resolved by the caller; formatted for a subject reference").SubjectRef().String()
}

// NewSpiceDBCheck returns the SpiceDB check function wired to the
// info-leakage gate. Both subject and resource are "type:id" strings; the
// function splits each, builds the CheckPermissionRequest, and fails closed
// on malformed input or transient SpiceDB errors.
func NewSpiceDBCheck(cl *spicedb.Client) func(context.Context, string, string, string) (bool, error) {
	return func(ctx context.Context, subject, permission, resource string) (bool, error) {
		subjectType, subjectID, serr := spicedb.SplitObject(subject)
		if serr != nil {
			return false, fmt.Errorf("info-leakage: SpiceDB check: subject %q: %w", subject, serr)
		}
		resourceType, resourceID, rerr := spicedb.SplitObject(resource)
		if rerr != nil {
			return false, fmt.Errorf("info-leakage: SpiceDB check: resource %q: %w", resource, rerr)
		}
		resp, err := cl.CheckPermission(ctx, &spicedbv1.CheckPermissionRequest{
			Resource:    &spicedbv1.ObjectReference{ObjectType: resourceType, ObjectId: resourceID},
			Permission:  permission,
			Subject:     &spicedbv1.SubjectReference{Object: &spicedbv1.ObjectReference{ObjectType: subjectType, ObjectId: subjectID}},
			Consistency: &spicedbv1.Consistency{Requirement: &spicedbv1.Consistency_MinimizeLatency{MinimizeLatency: true}},
		})
		if err != nil {
			return false, fmt.Errorf("info-leakage: SpiceDB check: %w", err)
		}
		return resp.GetPermissionship() == spicedbv1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
	}
}

// AudienceLookup is the narrow SpiceDB interface the slack Kind's
// SetAudienceResolver consumes. Defined here (not imported from
// pkg/authz/spicedb) so test fakes can satisfy it structurally without
// dragging the full client surface into a unit test.
type AudienceLookup interface {
	LookupSubjects(ctx context.Context, subjectRef string) ([]string, error)
}

// WireChannelKindAudienceResolvers walks every registered channel kind
// and binds the runner's SpiceDB client into kinds that need a
// SetAudienceResolver-style hook. Mirror of internal/cmd/channelsd/main.go's
// equivalent wiring — without this, the write-side info-leakage gate at
// respond_to_user time calls Kind.ResolveAudience on a kind whose
// internal resolver is nil, returning "audience resolver not wired" and
// failing every channel send.
//
// Idempotent: safe to call multiple times. Kinds that don't expose
// SetAudienceResolver are silently skipped.
func WireChannelKindAudienceResolvers(sdb AudienceLookup) {
	for _, k := range registry.All() {
		if sk, ok := k.(*slackkind.Kind); ok {
			sk.SetAudienceResolver(sdb)
			continue
		}
		// Future: add other channel kinds that expose audience-resolver
		// wiring here. The local kind, for instance, does not need
		// SpiceDB — it's CapabilitySingleUser and resolves directly to
		// the session initiator.
	}
}
