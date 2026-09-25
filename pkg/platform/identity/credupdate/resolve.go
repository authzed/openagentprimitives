// resolve.go — resolves a failing tool's Origin() string (e.g.
// "mcpserver/github") into the single credential behind it, producing the
// refusals that precede Determine when resolution itself cannot proceed.
//
// Split out of the reconciler because it does exactly one piece of I/O (a
// lookup of the origin's backing resource) and carries several distinct
// refusal paths that each deserve their own test — see resolve_test.go.
package credupdate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/originfmt"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	clikind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/cli"
	mcpkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/mcp"
	sidecarkind "github.com/authzed/openagentprimitives/pkg/platform/identity/authkind/sidecartoolbox"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
)

// originKind is one origin kind this package can resolve a credential for.
//
// Adding a kind is a ROW in originKinds below, never a branch in ResolveOrigin
// — the whole point of the table is that the accepted set is enumerable in one
// place and can be read against the set the recording side can compose.
type originKind struct {
	// noun names the origin's backing resource in refusal prose ("MCPServer").
	// Refusals are read by an agent and the human it asks, so they name the
	// resource rather than echoing the raw origin string.
	noun string

	// namespaced is false for a cluster-scoped backing resource, dropping the "in
	// namespace %q" clause from the not-found refusal: naming a namespace a
	// SpiceboxToolkit does not live in sends a human looking in the wrong place.
	namespaced bool

	// authKind both looks the backing resource up and enumerates the credentials
	// it needs. Reusing it rather than hand-rolling a Get per kind is what makes
	// this resolve to the SAME credential `oap agent setup-identity` created and
	// the runner injects — all three go through SetupRequirements.
	authKind authkind.Kind
}

// originKinds is the set of origin kinds ResolveOrigin resolves, keyed by the
// kind half of a tool's Origin() string.
//
// Every key MUST stay an originfmt constant — the same constants the recording
// side composes origins from (pkg/agent/runner.AuthFailureOrigins). Spelling a
// key as a literal lets this table fall behind the recorder, which silently
// kills the corroboration path for that kind: observations get written and never
// read, with nothing erroring.
var originKinds = map[string]originKind{
	originfmt.KindMCPServer: {noun: "MCPServer", namespaced: true, authKind: mcpkind.New()},
	// A toolkit origin carries the toolkit's LOGICAL name (toolkit.Toolkit.Name
	// = SpiceboxToolkitSpec.Name, what SandboxTool.Origin() reads), while
	// cli.ResolveTarget addresses the CR by metadata.name. The two coincide for
	// every builtin (there is no CR) and for the hand-authored convention, but
	// not necessarily for a CR whose metadata.name carries a revision or an
	// .oap instance prefix. When they diverge this refuses "not found" — a
	// missing card, never a card naming the wrong credential.
	originfmt.KindToolkit:        {noun: "toolkit", namespaced: false, authKind: clikind.New()},
	originfmt.KindSidecarToolbox: {noun: "SidecarToolbox", namespaced: true, authKind: sidecarkind.New()},
}

// notFoundReason phrases "the origin's backing resource does not exist".
func (k originKind) notFoundReason(name, namespace string) string {
	if k.namespaced {
		return fmt.Sprintf(
			"%s %q was not found in namespace %q, so its credential cannot be resolved.",
			k.noun, name, namespace)
	}
	return fmt.Sprintf("%s %q was not found, so its credential cannot be resolved.", k.noun, name)
}

// ResolveInput is everything ResolveOrigin needs to turn a failing tool's
// origin into a credential.
type ResolveInput struct {
	// Namespace is where the origin's backing resource (e.g. the MCPServer or
	// SidecarToolbox) lives. Ignored for a cluster-scoped backing resource —
	// see originKind.namespaced.
	Namespace string
	// Origin is the failing tool's tool.OriginTool.Origin() value, e.g.
	// "mcpserver/github". See CredentialUpdateRequestSpec.Origin, and
	// originKinds for the kinds resolvable here.
	Origin string
	// Identity is the runtime view of the credentials available to resolve
	// against — see credresolve.RuntimeIdentity.
	Identity credresolve.RuntimeIdentity

	// IdentityKind and IdentityName identify the identity CR Identity was
	// projected from, in CR Kind spelling ("AgentIdentity", "UserIdentity",
	// "SessionUserIdentity"). They populate ResolvedCredentialRef verbatim and
	// are the CALLER's to supply — the reconciler knows which CR it loaded.
	//
	// Never derive them from RuntimeIdentity.Label: that field carries no
	// stability contract and already reads "SessionUserIdentity <name>" for a
	// session projection, so parsing it would mislabel every passthrough
	// identity.
	//
	// SessionUserIdentity is the per-session projection of a UserIdentity, so a
	// caller asking "is this user-owned (passthrough) rather than the bot's own?"
	// must treat BOTH as user-owned; the difference is which CR is authoritative,
	// not who owns the credential.
	IdentityKind string
	IdentityName string

	// IdentityNamespace is the namespace of the identity CR named by
	// IdentityKind/IdentityName — the CALLER's to supply, for the same reason.
	//
	// NOT Identity.Namespace, which is "where each credential's Secret refs
	// resolve by default". The two coincide for AgentIdentity but diverge for
	// SessionUserIdentity, whose Secret refs resolve in
	// spiceboxv1alpha1.IdentitiesNamespace (anchoring JIT refresh / ID-JAG mint
	// to the master UserIdentity) while the CR itself lives in the session's
	// namespace. Reading Identity.Namespace here would point
	// ResolvedCredentialRef at a namespace that can never contain the CR it
	// names.
	IdentityNamespace string

	// Remap is applied to a requirement's suggested credential name before
	// lookup (ToolBundle.CredentialRemap / MCP ref remap semantics).
	Remap map[string]string
}

// ResolveResult is ResolveOrigin's outcome. A non-nil Refusal means "stop,
// write this determination" — Ref and Descriptor are zero-valued. A nil
// Refusal means "proceed to Determine" — Ref and Descriptor are populated.
type ResolveResult struct {
	Ref        spiceboxv1alpha1.ResolvedCredentialRef
	Descriptor spiceboxv1alpha1.CredentialDescriptor
	Refusal    *Outcome
}

// ResolveOrigin resolves in.Origin to the single credential behind it, for
// every origin kind in originKinds — which is every kind a tool's Origin() can
// produce and the corroboration recorder can therefore observe.
//
// Refusals are outcomes, not errors: every "we cannot proceed" path returns
// (ResolveResult{Refusal: ...}, nil). Only a genuine infrastructure failure —
// a non-NotFound error from the API server — returns a non-nil error, so the
// caller (the CredentialUpdateRequest reconciler) requeues instead of writing
// a refusal it cannot yet justify.
//
// in.Remap is the CALLER's to supply. The reconciler supplies it only for
// mcpserver origins, so a toolkit origin whose bundle declares a
// ToolBundle.CredentialRemap resolves the UNMAPPED name here. That usually lands
// on the missing-credential refusal, but NOT always: an identity holding an
// unrelated credential under the unmapped name would resolve to that one.
// Closing it is the caller's job — "toolkit/<name>" names no bundle, and several
// bundles with different remaps may answer to one toolkit name.
func ResolveOrigin(ctx context.Context, c client.Client, in ResolveInput) (ResolveResult, error) {
	logger := log.FromContext(ctx)

	// Step 1 — never guess at an origin's shape: exactly two non-empty
	// "/"-separated parts, or refuse as malformed. This is the defense against a
	// hand-written CR (sandbox and meta tools have no Origin() at all).
	kind, name, ok := SplitOrigin(in.Origin)
	if !ok {
		return ResolveResult{Refusal: malformedOriginRefusal(in.Origin)}, nil
	}

	// Step 2 — the kind must be one this package resolves credentials for. Kept
	// SEPARATE from the malformed refusal: collapsing "not an origin at all" and
	// "names a kind we do not resolve" into one sentence makes a test aimed at
	// either pass on the other.
	resolver, known := originKinds[kind]
	if !known {
		return ResolveResult{Refusal: unrecognizedOriginKindRefusal(in.Origin, kind)}, nil
	}

	// Step 3 — resolve the origin's backing resource through its authkind.
	// ErrTargetNotFound refuses by name; any other error is infrastructure
	// trouble the caller must requeue on, never a refusal we're not entitled
	// to make.
	tgt, err := resolver.authKind.ResolveTarget(ctx, c, in.Namespace, name)
	if err != nil {
		if errors.Is(err, authkind.ErrTargetNotFound) {
			return ResolveResult{Refusal: &Outcome{
				Tier:          TierNone,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
				Reason:        resolver.notFoundReason(name, in.Namespace),
			}}, nil
		}
		logger.Info("credupdate: ResolveOrigin failed to resolve the origin's backing resource",
			"origin", in.Origin, "kind", kind, "namespace", in.Namespace, "name", name, "err", err.Error())
		return ResolveResult{}, fmt.Errorf("credupdate: resolve %s %q for origin %q: %w",
			resolver.noun, name, in.Origin, err)
	}

	// Step 4-6 — build requirements, resolve descriptors, refuse on the two
	// ways resolution can fail to land on exactly one credential.
	reqs := resolver.authKind.SetupRequirements(ctx, tgt)

	descs, refusal, err := resolveDescriptors(reqs, in.Identity, in.Remap, resolver.noun, name)
	if err != nil {
		logger.Info("credupdate: ResolveOrigin failed to resolve descriptors",
			"origin", in.Origin, "kind", kind, "namespace", in.Namespace, "name", name, "err", err.Error())
		return ResolveResult{}, fmt.Errorf("credupdate: resolve descriptors for %s %q: %w", resolver.noun, name, err)
	}
	if refusal != nil {
		return ResolveResult{Refusal: refusal}, nil
	}

	// Step 6 — exactly one. Populate the result; no refusal. IdentityKind/
	// Name/Namespace are the caller's own values, carried through verbatim —
	// never inferred from Identity, whose fields document unrelated facts.
	req := reqs[0]
	credName := remappedName(req.SuggestedName, in.Remap)

	return ResolveResult{
		Ref: spiceboxv1alpha1.ResolvedCredentialRef{
			IdentityKind: in.IdentityKind,
			Namespace:    in.IdentityNamespace,
			Name:         in.IdentityName,
			Credential:   credName,
			ProviderID:   req.ProviderID,
		},
		Descriptor: descs[0],
	}, nil
}

// resolveDescriptors builds the CredentialDescriptor(s) behind reqs and
// classifies the three ways that can fail to land on exactly one:
//
//   - a (remapped) name absent from the identity refuses NoCredential;
//   - a (remapped) name matching MORE THAN ONE credential in the identity's
//     catalog refuses as ambiguous — credresolve.Descriptors' single-match
//     lookup would silently pick the first, and guessing is what this package
//     exists not to do;
//   - more than one descriptor overall, same refusal. Reachable today: a toolkit
//     declaring two sensitive env vars yields two requirements, and
//     "toolkit/<name>" does not say which of its credentials failed.
//
// noun/name identify the origin's backing resource for the refusal prose, which
// is phrased about that resource rather than about origin strings.
//
// Returns a non-nil error only for an unexpected failure out of
// credresolve.Descriptors — the "cannot resolve" shapes come back as refusals.
func resolveDescriptors(
	reqs []authkind.CredentialRequirement,
	id credresolve.RuntimeIdentity,
	remap map[string]string,
	noun string,
	name string,
) ([]spiceboxv1alpha1.CredentialDescriptor, *Outcome, error) {
	for _, r := range reqs {
		credName := remappedName(r.SuggestedName, remap)
		if n := countCredentialsNamed(id.Credentials, credName); n > 1 {
			return nil, ambiguousRefusal(noun, name), nil
		}
	}

	descs, err := credresolve.Descriptors(reqs, id, remap)
	if err != nil {
		if errors.Is(err, credresolve.ErrCredentialMissing) {
			return nil, &Outcome{
				Tier:          TierNone,
				Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
				Reason: fmt.Sprintf(
					"%s %q requires a credential that is not present in this agent's identity, "+
						"so there is no credential to update.", noun, name),
			}, nil
		}
		return nil, nil, err
	}

	switch {
	case len(descs) == 0:
		return nil, &Outcome{
			Tier:          TierNone,
			Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
			Reason: fmt.Sprintf(
				"%s %q uses no managed credential, so there is no credential to update.", noun, name),
		}, nil
	case len(descs) > 1:
		return nil, ambiguousRefusal(noun, name), nil
	default:
		return descs, nil, nil
	}
}

// ambiguousRefusal is the one refusal reason both ambiguity checks in
// resolveDescriptors produce — the reason text is identical regardless of
// which check tripped, since the agent-facing instruction is the same
// either way: name the failing service in prose so a human can disambiguate.
func ambiguousRefusal(noun, name string) *Outcome {
	return &Outcome{
		Tier:          TierNone,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationAmbiguousCred,
		Reason: fmt.Sprintf(
			"%s %q resolves to more than one credential in this agent's identity, so we cannot "+
				"safely guess which one needs updating. Describe which upstream service actually failed "+
				"so a human can pick the right one.", noun, name),
	}
}

// malformedOriginRefusal is the refusal for a value that is not an origin at
// all: empty, or not exactly two non-empty "/"-separated parts. Its sentence
// must stay DIFFERENT from unrecognizedOriginKindRefusal's — they share a Tier
// and Determination, so the reason is the only thing telling them apart.
func malformedOriginRefusal(origin string) *Outcome {
	return &Outcome{
		Tier:          TierNone,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
		Reason: fmt.Sprintf(
			"The failing tool's origin (%q) is not a well-formed \"<kind>/<name>\" origin, so there "+
				"is no credential to update.", origin),
	}
}

// unrecognizedOriginKindRefusal is the refusal for a well-formed origin naming
// a kind absent from originKinds. This is the fail-closed default for the
// whole table: an unknown kind is never guessed at, and adding a kind to
// originfmt without adding it here lands every one of its origins right here.
func unrecognizedOriginKindRefusal(origin, kind string) *Outcome {
	return &Outcome{
		Tier:          TierNone,
		Determination: spiceboxv1alpha1.CredentialUpdateDeterminationNoCredential,
		Reason: fmt.Sprintf(
			"The failing tool's origin (%q) names an origin kind (%q) this platform does not resolve "+
				"credentials for, so there is no credential to update.", origin, kind),
	}
}

// SplitOrigin splits an Origin() string ("mcpserver/github") into kind and name.
// Anything not exactly two non-empty "/"-separated parts is rejected — never a
// best-effort guess. It is the inverse of originfmt's composers.
//
// Exported because the CredentialUpdateRequest reconciler must parse the same
// origin to find the per-MCPServer credentialRemap BEFORE calling ResolveOrigin
// (the remap is an INPUT here). Two parsers for one wire format is how the two
// sides silently disagree about what "mcpserver/x" names.
func SplitOrigin(origin string) (kind, name string, ok bool) {
	parts := strings.Split(origin, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// remappedName applies remap to a requirement's suggested name, mirroring
// credresolve.Descriptors' own remap semantics exactly (same lookup, same
// fallback to the unmapped name).
func remappedName(suggested string, remap map[string]string) string {
	if m, ok := remap[suggested]; ok {
		return m
	}
	return suggested
}

// countCredentialsNamed counts how many entries in creds are named name.
func countCredentialsNamed(creds []spiceboxv1alpha1.AgentCredential, name string) int {
	n := 0
	for _, c := range creds {
		if c.Name == name {
			n++
		}
	}
	return n
}
