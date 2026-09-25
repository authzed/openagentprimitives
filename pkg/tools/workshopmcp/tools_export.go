// tools_export.go implements `export_draft` and `load_draft`: the workshop
// half of the .oap round trip (design spec §2.4, §6, §13's amendment). Task 6
// (export.go) already built the tuple-authorized storeDraft client and the
// operator-side route; this file bundles the builder's authored AgentClass
// into a portable .oap and stores it, and re-applies a stored .oap back into
// a (possibly fresh — the sweeper may have reaped and re-provisioned W since
// the export ran) workshop.
//
// The workshop-authored kinds a builder writes fall into two shapes, and the
// export/load boundary treats them differently — see
// pkg/controllers/webhooks/workshop/webhook.go's isClusterScopedWorkshopKind:
//
//   - AgentClass, AgentIdentity, MCPServer, SidecarToolbox, AgentUI, Skill,
//     ConfigMap: namespaced under W, with a plain unprefixed name. Nothing to
//     strip; load_draft only needs to point them at the CURRENT workshop
//     namespace.
//   - SpiceboxToolspec, SpiceboxToolkit: cluster-scoped. The admission
//     webhook's checkClusterToolObject requires metadata.name prefixed
//     "<workshopID>-" and metadata.labels[LabelWorkshopNamespace] ==
//     workshopID on CREATE. export_draft strips both (otherwise the
//     installed class trips plan-2's ReferencesWorkshopTool rule,
//     pkg/controllers/agentclass/builderclass.go, which refuses a class
//     referencing a toolspec whose LabelWorkshopNamespace names a foreign
//     workshop); load_draft re-adds both, keyed to whichever workshop it is
//     running in NOW.
package workshopmcp

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/source"
)

// Tool names announced on the MCP surface.
const (
	toolExportDraft = "export_draft"
	toolLoadDraft   = "load_draft"
)

// registerExport wires `export_draft` and `load_draft` onto mcpSrv.
func (s *Server) registerExport(mcpSrv *mcp.Server) {
	mcpSrv.AddTool(&mcp.Tool{
		Name: toolExportDraft,
		Description: "Bundle the agent this workshop's builder has authored into a portable draft " +
			"and store it, so the work is not lost even if the workshop expires — load_draft resumes " +
			"from it later. Returns artifactRef and digest (what an install request uses), plus " +
			"handle and artifactId: the draft is an artifact of this conversation. Call artifact_await " +
			"with the handle until it is ready, attach it to your reply with respond_to_user's attached, " +
			"and show it on the page with an ap:attachment naming the artifactId. " +
			"Fails clearly if nothing has been built yet.",
		InputSchema: map[string]any{"type": "object"},
	}, s.handleExportDraft)

	mcpSrv.AddTool(&mcp.Tool{
		Name: toolLoadDraft,
		Description: "Re-apply a previously exported draft's bundle into this workshop, resuming " +
			"work on it. Every piece of the draft is approved and restored together, as one step.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"oap": map[string]any{
					"type":        "string",
					"description": "The stored draft's bundle bytes, base64-encoded.",
				},
			},
			"required": []any{"oap"},
		},
	}, s.handleLoadDraft)
}

// handleExportDraft answers the `export_draft` tool call: build the bundle,
// store it, report back where it landed. Auto policy (design spec's
// export_draft row) — no approval gate on this call itself, unlike
// workshop_apply/load_draft.
func (s *Server) handleExportDraft(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	oapBytes, err := s.exportBundle(ctx)
	if err != nil {
		if isDeniedErr(err) {
			return s.deniedResult(err), nil
		}
		return s.toolErr("export_draft: %v", err), nil
	}

	stored, err := s.storeDraft(ctx, oapBytes)
	if err != nil {
		return s.toolErr("export_draft: %v", err), nil
	}

	result := map[string]any{
		"artifactRef": stored.ArtifactRef,
		"digest":      stored.Digest,
	}
	// An operator that predates the draft render answers no handle; omitting
	// the keys is a clear signal to the model, where an empty string would
	// send it into artifact_await("").
	if stored.Handle != "" {
		result["handle"] = stored.Handle
		result["artifactId"] = stored.ArtifactID
	}
	return s.jsonResult(result)
}

// exportBundle finds the sole AgentClass this workshop's builder has
// authored, walks its ref graph live via source.OpenCluster, strips every
// exported CR of this workshop's identity (stripWorkshopIdentity), stamps
// the root AgentClass with its oap-source provenance, and packs the result
// into .oap bytes. Returns a plain error on every failure —
// handleExportDraft, its only caller, decides toolErr vs. deniedResult via
// isDeniedErr; this function never builds a CallToolResult itself.
func (s *Server) exportBundle(ctx context.Context) ([]byte, error) {
	className, err := s.soleAuthoredClassName(ctx)
	if err != nil {
		return nil, err
	}

	bundle, err := source.OpenCluster(s.K8s, s.Identity.Namespace, className).Bundle(ctx)
	if err != nil {
		return nil, fmt.Errorf("building bundle for agent %q: %w", className, err)
	}

	crs, err := bundle.CRs()
	if err != nil {
		return nil, fmt.Errorf("decoding bundled manifests: %w", err)
	}
	if err := stripWorkshopIdentity(crs, s.Identity.WorkshopID); err != nil {
		return nil, fmt.Errorf("stripping workshop identity: %w", err)
	}
	if err := stampOapSource(crs, s.Identity.WorkshopID); err != nil {
		return nil, fmt.Errorf("stamping oap-source provenance: %w", err)
	}

	manifests, err := marshalCRStream(crs)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling stripped manifests: %w", err)
	}
	bundle.Manifests = manifests
	if err := bundle.Validate(); err != nil {
		return nil, fmt.Errorf("stripped bundle failed validation: %w", err)
	}

	oapBytes, err := oap.Pack(bundle)
	if err != nil {
		return nil, fmt.Errorf("packing .oap: %w", err)
	}
	return oapBytes, nil
}

// soleAuthoredClassName finds the one AgentClass this workshop's builder has
// authored in W. Zero is "nothing built yet"; more than one is refused with
// the candidate names rather than guessing which to export — v1's builder
// authors a single agent per workshop (design spec §12), so this should be
// the common case, and a second AgentClass appearing is unusual enough to
// name explicitly rather than silently pick one.
//
// Excludes any AgentClass named in the workshop's own Workshop CR
// status.standins registry — a credential-free rehearsal stand-in
// (pkg/web/workshopprojectsrv), not something this workshop's builder
// authored. Deliberately NOT spiceboxv1alpha1.AnnotationStandinSource: that
// annotation is a builder-writable field this workshop's own workshop_apply
// tool can stamp onto anything (the admission webhook never inspects
// annotations), so trusting its presence would let a builder (or a
// prompt-injected model driving one) exclude its own authored class from
// export just by forging the marker. status.standins is operator-owned —
// this sidecar's Role grants it get, never update, on this object's status
// subresource (pkg/controllers/workshop/rbac.go) — so it cannot be forged
// from here either.
//
// A stand-in is deliberately an ORDINARY, same-namespace AgentClass (so
// owner-ref GC, the roster, and this very namespace scan all stay
// unremarkable) — which means a workshop that has authored one draft AND
// rehearsed a hand-off to one other agent now holds TWO AgentClass objects
// in W, and without this exclusion this function would refuse export
// outright ("more than one agent exists") the moment a stand-in exists
// alongside the draft it was projected to help write. That is not a
// hypothetical: projecting a stand-in and then authoring/exporting the
// draft that references it is the intended sequence this feature exists
// for.
//
// If the status.standins read itself fails, this refuses to export rather
// than falling back to guessing (e.g. treating the read failure as "no
// stand-ins" and risking a real stand-in shipping as a real agent, or the
// reverse) — a failed read must never silently widen or narrow which class
// counts as authored.
func (s *Server) soleAuthoredClassName(ctx context.Context) (string, error) {
	var list spiceboxv1alpha1.AgentClassList
	if err := s.K8s.List(ctx, &list, client.InNamespace(s.Identity.Namespace)); err != nil {
		return "", fmt.Errorf("listing this workshop's agents: %w", err)
	}

	standins, err := s.standinNames(ctx)
	if err != nil {
		return "", fmt.Errorf("reading this workshop's stand-in registry: %w", err)
	}

	var authored []string
	for _, ac := range list.Items {
		if standins[ac.Name] {
			continue
		}
		authored = append(authored, ac.Name)
	}
	switch len(authored) {
	case 0:
		return "", fmt.Errorf("nothing to export yet — no agent has been authored in this workshop")
	case 1:
		return authored[0], nil
	default:
		sort.Strings(authored)
		return "", fmt.Errorf(
			"more than one agent exists in this workshop (%s); export_draft exports the sole agent a workshop authors and cannot yet choose among several",
			strings.Join(authored, ", "))
	}
}

// standinNames reads the AUTHORITATIVE set of stand-in AgentClass names this
// workshop has recorded, from the Workshop CR's own status.standins —
// never spiceboxv1alpha1.AnnotationStandinSource, which a
// builder's own apply tool can forge onto any class it authored (see
// soleAuthoredClassName's own doc). The sidecar's Role grants it get on
// exactly its own Workshop CR (pkg/controllers/workshop/rbac.go), so this Get
// never leaves the workshop's own authorization boundary; the SAME Role
// grants it no update on that object's status subresource, so nothing this
// sidecar does can forge an entry here either.
func (s *Server) standinNames(ctx context.Context) (map[string]bool, error) {
	var ws spiceboxv1alpha1.Workshop
	sessNS, sessName := s.sessionRef()
	key := client.ObjectKey{Namespace: sessNS, Name: spiceboxv1alpha1.WorkshopName(sessName)}
	if err := s.K8s.Get(ctx, key, &ws); err != nil {
		return nil, fmt.Errorf("get Workshop %s: %w", key, err)
	}
	names := make(map[string]bool, len(ws.Status.Standins))
	for _, st := range ws.Status.Standins {
		names[st.Name] = true
	}
	return names, nil
}

// stampOapSource stamps the exported bundle's root AgentClass CR with its
// oap-source provenance — SourceKind "workshop", Ref the originating
// workshop's ID — so an install (or a later re-export) can trace the bundle
// back to the build space it came from. No timestamp: the annotation must be
// a pure function of workshopID alone, matching every other applied field's
// SSA-idempotency requirement (CLAUDE.md's "keep applied fields idempotent" —
// mirrors instance.AnnotationOapSource's own doc comment on why install
// never embeds a wall-clock value there either).
func stampOapSource(crs []*unstructured.Unstructured, workshopID string) error {
	src, err := instance.MarshalOapSource(instance.OapSource{SourceKind: "workshop", Ref: workshopID})
	if err != nil {
		return fmt.Errorf("marshal oap-source: %w", err)
	}
	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		annotations := cr.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[instance.AnnotationOapSource] = src
		cr.SetAnnotations(annotations)
	}
	return nil
}

// clusterScopedWorkshopKinds is the closed set of CR kinds a workshop build
// space may author cluster-scoped — mirrors
// pkg/controllers/webhooks/workshop/webhook.go's isClusterScopedWorkshopKind.
// These are the only two kinds whose metadata.name ever carries the
// ws-<id>- prefix and whose metadata.labels ever carries
// LabelWorkshopNamespace; every other kind an AgentClass's ref graph can
// pull in is namespaced under W with a plain, unprefixed name.
var clusterScopedWorkshopKinds = map[string]bool{
	"SpiceboxToolspec": true,
	"SpiceboxToolkit":  true,
}

// stripWorkshopIdentity removes this workshop's ws-<id>- prefix and
// workshop labels (LabelWorkshopNamespace/LabelWorkshopSessionNamespace/
// LabelWorkshopSessionName) from every CR export pulled out of W. Without
// this, an installed class trips plan-2's ReferencesWorkshopTool rule
// (pkg/controllers/agentclass/builderclass.go): a SpiceboxToolspec still
// carrying LabelWorkshopNamespace looks, to that rule, like a foreign
// workshop's tool that the installed class has no business referencing.
func stripWorkshopIdentity(crs []*unstructured.Unstructured, workshopID string) error {
	for _, cr := range crs {
		labels := cr.GetLabels()
		if len(labels) == 0 {
			continue
		}
		delete(labels, spiceboxv1alpha1.LabelWorkshopNamespace)
		delete(labels, spiceboxv1alpha1.LabelWorkshopSessionNamespace)
		delete(labels, spiceboxv1alpha1.LabelWorkshopSessionName)
		if len(labels) == 0 {
			labels = nil
		}
		cr.SetLabels(labels)
	}
	return rewriteToolCrossRefs(crs, func(_, oldName string) string {
		return strings.TrimPrefix(oldName, workshopID+"-")
	})
}

// reprefixWorkshopIdentity is stripWorkshopIdentity's inverse, run by
// load_draft before it re-applies a previously exported bundle: it re-adds
// THIS workshop's ws-<id>- prefix (and LabelWorkshopNamespace) to the two
// cluster-scoped tool kinds a workshop may author, and points every other
// (namespaced) CR at this workshop's own namespace. Necessary because
// "resume in a fresh workshop" (design spec §7's sweeper-expiry row) can
// mean load_draft runs against a DIFFERENT workshop namespace/ID than the
// one that produced the draft — so the SA token's RBAC boundary and the
// admission webhook's checkClusterToolObject rule both need the CURRENT
// identity, never whatever export left baked into the bundle.
func reprefixWorkshopIdentity(crs []*unstructured.Unstructured, namespace, workshopID string) error {
	if err := rewriteToolCrossRefs(crs, func(_, oldName string) string {
		return workshopID + "-" + oldName
	}); err != nil {
		return err
	}
	for _, cr := range crs {
		if clusterScopedWorkshopKinds[cr.GetKind()] {
			labels := cr.GetLabels()
			if labels == nil {
				labels = map[string]string{}
			}
			labels[spiceboxv1alpha1.LabelWorkshopNamespace] = workshopID
			cr.SetLabels(labels)
			continue
		}
		cr.SetNamespace(namespace)
	}
	return nil
}

// rewriteToolCrossRefs renames every SpiceboxToolspec/SpiceboxToolkit CR's
// metadata.name via nameFor(kind, oldName), then repoints the one cross-ref
// field in the bundle that can ever name one of them BY metadata.name:
// AgentClass.spec.toolBundles[].toolspecs[] (-> SpiceboxToolspec).
//
// SpiceboxToolkit is renamed (its metadata.name, for the ws-<id>- prefix the
// admission webhook's checkClusterToolObject requires on it) but never
// needs a referrer rewritten at it: nothing in the bundle names a
// SpiceboxToolkit by metadata.name. SpiceboxToolspec.spec.toolkit.name looks
// like it should be that reference, but it is not — per ToolspecToolkitRef's
// own doc ("the SpiceboxToolkit name (or a builtin toolkit's name)") and
// source/cluster.go's walkToolkits, it names the toolkit's LOGICAL identity,
// resolved by matching SpiceboxToolkit.spec.name+spec.toolkitRevision — a
// value wholly independent of the CR's metadata.name, and so never renamed
// in either direction.
//
// No other AgentClass ref field (agentIdentity/mcpServers/
// sidecarToolboxes/agentUI) ever names a cluster-scoped, prefix-bearing CR,
// and a workshop-authored AgentClass can never set toolBundles[].class at
// all (the admission webhook's checkAgentClass refuses it outright) — so
// toolBundles[].toolspecs[] is the full cross-ref surface either direction
// (strip or reprefix) needs, narrower than
// pkg/platform/oap/instance.Rename's table, which additionally handles the
// broader (non-workshop) .oap install case.
func rewriteToolCrossRefs(crs []*unstructured.Unstructured, nameFor func(kind, oldName string) string) error {
	renamed := make(map[string]string, len(crs))
	for _, cr := range crs {
		kind := cr.GetKind()
		if !clusterScopedWorkshopKinds[kind] {
			continue
		}
		old := cr.GetName()
		if old == "" {
			continue
		}
		newName := nameFor(kind, old)
		renamed[kind+"/"+old] = newName
		cr.SetName(newName)
	}
	if len(renamed) == 0 {
		return nil
	}
	for _, cr := range crs {
		if cr.GetKind() != "AgentClass" {
			continue
		}
		if err := rewriteToolspecRefsOnAgentClass(cr, renamed); err != nil {
			return fmt.Errorf("rewrite AgentClass %q toolspec refs: %w", cr.GetName(), err)
		}
	}
	return nil
}

// rewriteToolspecRefsOnAgentClass repoints every entry of
// spec.toolBundles[].toolspecs[] naming a renamed SpiceboxToolspec, in
// place. Absent toolBundles, or an element with no toolspecs, is a no-op.
func rewriteToolspecRefsOnAgentClass(cr *unstructured.Unstructured, renamed map[string]string) error {
	bundles, found, err := unstructured.NestedSlice(cr.Object, "spec", "toolBundles")
	if err != nil {
		return fmt.Errorf("field spec.toolBundles: %w", err)
	}
	if !found {
		return nil
	}

	changed := false
	for i, item := range bundles {
		elem, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("field spec.toolBundles[%d]: element is not an object", i)
		}
		specs, ok, err := unstructured.NestedStringSlice(elem, "toolspecs")
		if err != nil {
			return fmt.Errorf("field spec.toolBundles[%d].toolspecs: %w", i, err)
		}
		if !ok {
			continue
		}
		innerChanged := false
		for j, name := range specs {
			if newName, ok := renamed["SpiceboxToolspec/"+name]; ok {
				specs[j] = newName
				innerChanged = true
			}
		}
		if !innerChanged {
			continue
		}
		if err := unstructured.SetNestedStringSlice(elem, specs, "toolspecs"); err != nil {
			return fmt.Errorf("field spec.toolBundles[%d].toolspecs: %w", i, err)
		}
		changed = true
	}
	if changed {
		if err := unstructured.SetNestedSlice(cr.Object, bundles, "spec", "toolBundles"); err != nil {
			return fmt.Errorf("field spec.toolBundles: %w", err)
		}
	}
	return nil
}

// marshalCRStream renders crs as a deterministic multi-document YAML stream,
// sorted by (kind, name) — mirrors pkg/platform/oap/source/cluster.go's own
// marshalCRStream, reimplemented here because that one is unexported and
// this file needs to re-marshal AFTER stripWorkshopIdentity has already
// renamed some of the entries cluster.go handed back.
func marshalCRStream(crs []*unstructured.Unstructured) ([]byte, error) {
	sort.Slice(crs, func(i, j int) bool {
		if crs[i].GetKind() != crs[j].GetKind() {
			return crs[i].GetKind() < crs[j].GetKind()
		}
		return crs[i].GetName() < crs[j].GetName()
	})
	var buf bytes.Buffer
	for i, cr := range crs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		data, err := yaml.Marshal(cr.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", cr.GetKind(), cr.GetName(), err)
		}
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

// loadDraftArgs is load_draft's own argument shape. Oap decodes from a
// base64-encoded JSON string into raw bytes via encoding/json's standard
// []byte handling. The simplest shape per the design spec: the bundle bytes
// ride directly in the argument; fetching by artifact ref is deferred.
type loadDraftArgs struct {
	Oap []byte `json:"oap"`
}

// handleLoadDraft answers the `load_draft` tool call: unpack the bundle,
// re-key it to THIS workshop's identity, re-apply every object through the
// same approved path workshop_apply uses (applyCR), and report back with one
// summary of everything that was restored — never one card per object
// (design spec §13's amendment to §2.4: "a resume that asks twenty times is
// one nobody finishes").
func (s *Server) handleLoadDraft(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a loadDraftArgs
	if err := decodeArgs(req, &a); err != nil {
		return s.toolErr("load_draft: decode arguments: %v", err), nil
	}
	if len(a.Oap) == 0 {
		return s.toolErr("load_draft: oap is required"), nil
	}

	bundle, err := oap.Unpack(a.Oap)
	if err != nil {
		return s.toolErr("load_draft: unpacking the draft: %v", err), nil
	}
	crs, err := bundle.CRs()
	if err != nil {
		return s.toolErr("load_draft: decoding the draft's resources: %v", err), nil
	}
	if len(crs) == 0 {
		return s.toolErr("load_draft: the draft carries nothing to load"), nil
	}
	if err := reprefixWorkshopIdentity(crs, s.Identity.Namespace, s.Identity.WorkshopID); err != nil {
		return s.toolErr("load_draft: %v", err), nil
	}

	applied := make([]*unstructured.Unstructured, 0, len(crs))
	for _, obj := range crs {
		result, err := s.applyCR(ctx, obj)
		if err != nil {
			if isDeniedErr(err) {
				return s.deniedResult(err), nil
			}
			return s.toolErr("load_draft: applying %s %q: %v", obj.GetKind(), obj.GetName(), err), nil
		}
		applied = append(applied, result)
	}

	summary, err := summarizeLoadedBundle(applied)
	if err != nil {
		// Every object above just applied cleanly; a failure here would be a
		// structural surprise (a bundled kind's shape not round-tripping
		// through FromUnstructured) rather than a partial apply — surface it
		// rather than silently reporting no summary at all.
		return s.toolErr("load_draft: summarizing the restored draft: %v", err), nil
	}
	return s.jsonResult(map[string]any{
		"summary": summary,
		"objects": len(applied),
	})
}

// summarizeLoadedBundle renders ONE plain-language paragraph describing
// every object load_draft just applied, reusing render_summary's own
// per-kind Copy-rule sentences (renderSummary, tools_render.go) rather than
// inventing a second wording convention — joined into one paragraph so a
// resume never asks once per object. A kind renderSummary doesn't have a
// dedicated sentence for (AgentIdentity, AgentUI, SpiceboxToolkit, ConfigMap,
// Skill) falls through to its own generic "part of this agent's setup"
// sentence, which needs no typed decode at all.
func summarizeLoadedBundle(objs []*unstructured.Unstructured) (string, error) {
	sentences := make([]string, 0, len(objs))
	for _, o := range objs {
		var described client.Object = o
		if typed, err := newObjectForKind(o.GetKind()); err == nil {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, typed); err != nil {
				return "", fmt.Errorf("decode %s %q: %w", o.GetKind(), o.GetName(), err)
			}
			described = typed
		}
		sentences = append(sentences, renderSummary(described))
	}
	return "The draft has been restored. " + strings.Join(sentences, " "), nil
}
