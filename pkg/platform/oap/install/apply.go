package install

import (
	"context"
	"errors"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/adoptguard"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// FieldManager is the SSA field manager used for every write Install makes:
// the bundled CRs, the Secrets materialized from SecretSpecs, and (via
// EnsureClusterDeps -> adoptkit.Adopt) the metadata-only adoption patch on a
// pre-existing shared cluster dep.
const FieldManager = "ap-agent-install"

// clusterScopedKinds lists the agentprimitives Kinds that are cluster-scoped
// (mirroring the CRDs' `scope: Cluster`, restricted to what a .oap AgentClass
// ref graph can carry). setNamespace consults it so it never stamps
// metadata.namespace onto one — the apiserver rejects that outright.
var clusterScopedKinds = map[string]bool{
	"SpiceboxClass":           true,
	"SpiceboxToolkit":         true,
	"SpiceboxToolspec":        true,
	"ClusterSkill":            true,
	"ClusterSkillSource":      true,
	"ClusterAgentSettings":    true,
	"ClusterIdentityProvider": true,
	"UserIdentity":            true,
}

// InstallOpts configures one Install call.
type InstallOpts struct {
	// ResourceNames supplies a complete resource map instead of legacy prefix naming.
	ResourceNames instance.NameMap
	// DirectNames maps explicitly embedded roster entries to physical child names.
	DirectNames map[string]string
	// RootInstallName groups every graph node under one lifecycle owner.
	RootInstallName string
	// DependencyPath identifies this node separately from root ownership.
	DependencyPath oap.DependencyPath
	// Name, when non-empty, becomes this install's identity: every bundled CR is
	// renamed with the "<Name>-" prefix via instance.Rename (which also rewrites
	// their cross-references so the graph stays consistent), and Name is what
	// instance.Stamp puts on the install labels. Empty means no renaming, and
	// the install label uses the bundled AgentClass CR's own name.
	Name string
	// Namespace, when non-empty, is written onto metadata.namespace of every
	// namespaced bundled CR (overriding whatever the manifest YAML carries)
	// and is the namespace any Secrets from SecretSpecs are created in.
	// Required whenever there is at least one Secret to create.
	Namespace string

	// SourceKind, SourceRef, and SourceDigest describe where this install's
	// bundle came from, stamped onto the AgentClass CR's
	// instance.AnnotationOapSource as provenance. SourceKind is "registry" (an
	// OCI ref; SourceRef is that ref, SourceDigest the digest the pull resolved)
	// or "file" (a local .oap or folder source; SourceRef empty, SourceDigest
	// the bundle's own oap.Digest). An empty SourceKind defaults to "file" with
	// an empty ref, rather than persisting an ambiguous one.
	SourceKind   string
	SourceRef    string
	SourceDigest string

	// ImageRewrites maps a bundled image ref (as written in the CRs) to the
	// ref install resolved for it (a registry digest after a push, or the same
	// tag after a local node/VM load). Applied to SpiceboxClass.spec.image and
	// SidecarToolbox.spec.source.image before apply. Empty = no rewrite.
	ImageRewrites map[string]string

	// additionalConflicts are graph-planned resources created outside the
	// bundle apply (currently channel credential Secrets). They join the same
	// exact-key adoption decision but are never serialized or caller-settable.
	additionalConflicts []Conflict

	// ExtraQuestions derives ADDITIONAL install questions from the CRs after the
	// manifest's own answers are overlaid — the only point at which a rule like
	// "this SpiceboxClass exceeds what this cluster can schedule" is knowable,
	// since a manifest question may itself bind to spec.resources. Install
	// resolves them through the SAME path as manifest questions, so every
	// surface gets one prompting path. Returns notices the caller surfaces
	// verbatim, and a hard error to abort with nothing written. nil skips it.
	ExtraQuestions func(ctx context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error)

	// Interactive and Sets answer those extra questions the same way the caller
	// answered the manifest's: Interactive permits prompting, Sets supplies
	// non-interactive answers (--set on the CLI, form values in admind).
	Interactive bool
	Sets        map[string]string
	// ExtraValues supplies the node-local --values projection for synthesized
	// extra questions. Direct installs leave it empty and retain their existing
	// file-based manifest question flow.
	ExtraValues map[string]any

	// QuestionOptions presents those extra questions the way the caller
	// presented the manifest's — pass the SAME PresentOver. Both sets are asked
	// over one terminal in one run, so a second presentation would style them
	// differently, ignore a --no-color the first honoured, and read the same
	// stdin through a second buffer. Empty lets Resolve build its own, which is
	// right for a caller that never prompts.
	QuestionOptions []ResolveOption

	// Adopt names pre-existing objects, as "Kind/Name", this install may seize
	// instead of refusing (see resolveConflicts). Naming a Secret here is the
	// ONLY flag-driven way to adopt one — a blanket AdoptAll never covers a
	// Secret, because adopting it overwrites its data with the answered value.
	// A key naming no actual conflict is a hard error, not a no-op.
	Adopt []string
	// AdoptAll adopts every conflicting NON-Secret object. Set by a bare
	// `--adopt`.
	AdoptAll bool
	// AdoptSecretsAllowed says this SURFACE can turn adopting a Secret into an
	// individual act — putting the specific object in front of a specific human
	// and getting a specific answer. A terminal can; an HTTP field in the same
	// POST body as everything else cannot, which is exactly what made the
	// Secret carve-out on AdoptAll meaningless over admind.
	//
	// The zero value REFUSES, so a surface added later that forgets to think
	// about it inherits the safe answer rather than the convenient one, and
	// resolveConflicts enforces it over every route into the adopted set.
	AdoptSecretsAllowed bool
	// AdoptDecision, when set, is asked which conflicts to adopt once the guards
	// have collected them all — the seam each surface fills with the affordance
	// it has (a huh multi-select on the CLI, a native dialog on the desktop;
	// admind leaves it nil and round-trips a 409 to the browser). It is called
	// ONLY when neither Adopt nor AdoptAll is set, so a scripted install never
	// becomes interactive. Returning fewer keys than there are conflicts aborts
	// with a *ConflictError; returning an error aborts with that error. Nothing
	// has been written to the cluster when it is called.
	AdoptDecision func(context.Context, []Conflict) ([]string, error)

	// decisionDiscovery is Workflow's admin-only, read-only pass. It allows
	// Prepare to discover capacity questions and ownership conflicts from the
	// supplied/default subset while guaranteeing that no Prepared value can
	// escape until every decision is resolved.
	decisionDiscovery bool
	priorMissing      *MissingAnswersError
}

// Result summarizes a completed Install.
type Result struct {
	AppliedKinds   []string
	SecretsCreated int
	Name           string
	// Adopted lists the "Kind/Name" of every pre-existing object this install
	// seized rather than created. Callers MUST surface it: adoption overwrites
	// an object the operator did not install here, and it is recorded nowhere on
	// the object itself — an applied annotation would break SSA idempotency.
	Adopted []string
	// Warnings are non-fatal issues the caller MUST surface (e.g. a required
	// shared cluster dependency that is absent and not carried by the bundle).
	// A successful Install with a non-empty Warnings is an install that landed
	// but whose agent may not function until the warned-about condition is
	// resolved — never drop these silently.
	Warnings []string
}

// Install is the compatibility entrypoint for one bundle: Prepare completes
// validation, transformations, questions, dependency checks, and ownership
// decisions before ApplyPrepared makes any cluster mutation. New objects are
// created first, then server-side-applied under FieldManager; existing owned or
// explicitly adopted objects converge through SSA. Shared cluster dependencies
// are adopted metadata-only, with their specs preserved.
//
// A partial failure retains the historical single-node behavior: earlier writes
// remain for a later reinstall to converge. ExecuteGraph adds automatic rollback
// of newly created objects across all attempted graph nodes. Result.Warnings
// and Result.Adopted carry information the caller must surface.
func Install(ctx context.Context, c client.Client, b *oap.Bundle, answers oap.Answers, secrets []SecretSpec, opts InstallOpts) (*Result, error) {
	p, err := Prepare(ctx, c, b, answers, secrets, opts)
	if err != nil {
		return nil, err
	}
	return ApplyPrepared(ctx, c, p)
}

// ApplyPrepared executes one completed preflight. A partial failure leaves its
// successful Creates recorded for RollbackPrepared; Install retains its legacy
// partial-failure behavior, while ExecuteGraph invokes rollback automatically.
func ApplyPrepared(ctx context.Context, c client.Client, p *Prepared) (result *Result, err error) {
	if p == nil || p.Bundle == nil || p.Bundle.Manifest == nil {
		return nil, fmt.Errorf("install: missing prepared bundle")
	}
	defer func() { err = redactError(p.redactor, err) }()
	depWarnings, err := EnsureClusterDeps(ctx, c, p.Bundle.Manifest.Requires.ClusterDeps, p.Owner, p.bundledNames)
	if err != nil {
		return nil, fmt.Errorf("install: ensure cluster deps: %w", err)
	}
	warnings := slices.Clone(p.Warnings)
	for _, warning := range depWarnings {
		if !slices.Contains(warnings, warning) {
			warnings = append(warnings, warning)
		}
	}
	for _, sec := range p.secretObjects {
		if err := applyPreparedObject(ctx, c, p, sec); err != nil {
			return nil, withAdoptedHint(fmt.Errorf("install: create secret %s: %w", sec.GetName(), err), p.Adopted)
		}
	}
	kinds := make([]string, 0, len(p.CRs))
	for _, cr := range p.CRs {
		if !p.skipApply[cr.GetKind()+"/"+cr.GetName()] {
			if err := applyPreparedObject(ctx, c, p, cr); err != nil {
				return nil, withAdoptedHint(fmt.Errorf("install: apply %s/%s: %w", cr.GetKind(), cr.GetName(), err), p.Adopted)
			}
		}
		kinds = append(kinds, cr.GetKind())
	}
	return &Result{AppliedKinds: kinds, SecretsCreated: len(p.secretObjects), Name: p.InstallName, Warnings: warnings, Adopted: slices.Clone(p.Adopted)}, nil
}

func applyPreparedObject(ctx context.Context, c client.Client, p *Prepared, obj *unstructured.Unstructured) error {
	key := objectKey(obj)
	identity := p.observed[key]
	if !p.Existing[key] {
		// Create, never upsert, an absent object. A foreign object appearing
		// after preflight must fail closed rather than be seized by SSA.
		created := obj.DeepCopy()
		if err := c.Create(ctx, created, client.FieldOwner(FieldManager)); err != nil {
			return err
		}
		p.created = append(p.created, createdObject{key: key, uid: created.GetUID()})
		identity = objectIdentity{uid: created.GetUID(), resourceVersion: created.GetResourceVersion()}
		p.observed[key] = identity
	}
	// Keep server-managed metadata and defaults out of the prepared payload.
	return ssaApply(ctx, c, obj.DeepCopy(), identity)
}

// RollbackPrepared deletes only objects this invocation successfully Created,
// in reverse order. Existing and adopted objects are never rollback candidates.
// UID preconditions protect objects deleted and replaced after our Create.
func RollbackPrepared(ctx context.Context, c client.Client, p *Prepared) (err error) {
	if p == nil {
		return nil
	}
	defer func() { err = redactError(p.redactor, err) }()
	var errs []error
	for i := len(p.created) - 1; i >= 0; i-- {
		created := p.created[i]
		if p.Existing[created.key] {
			continue
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(created.key.GVK)
		obj.SetNamespace(created.key.Namespace)
		obj.SetName(created.key.Name)
		var opts []client.DeleteOption
		if created.uid != "" {
			opts = append(opts, client.Preconditions(metav1.Preconditions{UID: &created.uid}))
		}
		if err := c.Delete(ctx, obj, opts...); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("rollback: delete %s %s/%s: %w", created.key.GVK.Kind, created.key.Namespace, created.key.Name, err))
		}
	}
	return errors.Join(errs...)
}

// planBundledResources decides, for EVERY bundled CR — cluster-scoped AND
// namespaced — whether it is safe to force-apply, closing the gap where the
// final SSA loop would otherwise seize a pre-existing object it does not own.
// It returns the "Kind/Name" set the caller must SKIP force-applying (a
// pre-existing shared cluster dep this install adopted, not owns), every
// ownership collision as data, and an error only for a failed read.
//
// The namespaced arm matters under the admind install endpoint specifically:
// it runs Install under the operator's elevated ServiceAccount with a
// cluster-wide configmaps:patch grant and a caller-chosen target namespace, so
// without this check a crafted bundle carrying a ConfigMap named
// "publisher-keys" in agentprimitives-system would force-seize the audit
// tamper-evidence trust root — a confused-deputy escalation. Guarding every
// bundled resource refuses to clobber ANY foreign or other-install object.
//
// Per bundled CR (cluster-scoped Get by name, namespaced by namespace+name):
//   - Absent → not skipped (the caller force-applies = creates it).
//   - Carrying THIS install's LabelInstall → ours from a prior run; not
//     skipped, since force-apply converges it and keeps re-install idempotent.
//   - Not ours, cluster-scoped, declared in requires.clusterDeps → an intended
//     shared dep: SKIP the spec-overwriting apply so its spec is preserved,
//     and let EnsureClusterDeps pin-check + adopt it metadata-only.
//   - Not ours, anything else → collected as a *Conflict for resolveConflicts.
func planBundledResources(ctx context.Context, c client.Client, crs []*unstructured.Unstructured, installName, installNamespace string, deps []oap.RequiredClusterDep) (map[string]bool, []Conflict, error) {
	declared := make(map[string]bool, len(deps))
	for _, d := range deps {
		declared[d.Kind+"/"+d.Name] = true
	}

	skip := map[string]bool{}
	var conflicts []Conflict
	for _, cr := range crs {
		clusterScoped := clusterScopedKinds[cr.GetKind()]
		adoptable := clusterScoped && declared[cr.GetKind()+"/"+cr.GetName()]
		adopt, conflict, err := checkResourceOwnership(ctx, c, cr, installName, installNamespace, clusterScoped, adoptable)
		if err != nil {
			return nil, nil, err
		}
		if conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
		if adopt {
			skip[cr.GetKind()+"/"+cr.GetName()] = true
		}
	}
	return skip, conflicts, nil
}

// guardSynthesizedSecrets gives the Secrets install synthesizes from answered
// secret questions the SAME ownership guard planBundledResources gives bundle
// CRs. They are force-applied with ForceOwnership too, so without this check a
// bundle could name a secret-question target that collides with a foreign
// Secret in the attacker-chosen namespace and have the operator's cluster-wide
// secrets:create/patch grant overwrite its data — the same confused-deputy
// class, narrower vector. Secrets are always namespaced and never an adoptable
// shared dep, so this is the plain namespaced arm.
func guardSynthesizedSecrets(ctx context.Context, c client.Client, secretObjs []*unstructured.Unstructured, installName, installNamespace string) ([]Conflict, error) {
	var conflicts []Conflict
	for _, sec := range secretObjs {
		_, conflict, err := checkResourceOwnership(ctx, c, sec, installName, installNamespace, false, false)
		if err != nil {
			return nil, err
		}
		if conflict != nil {
			// conflict.Secret is already true here: checkResourceOwnership derives
			// it from the object's own Kind (secretUnstructured always sets Kind
			// "Secret"), not from an assignment this caller makes — see
			// checkResourceOwnership's doc comment for why that matters.
			conflicts = append(conflicts, *conflict)
		}
	}
	return conflicts, nil
}

// checkResourceOwnership is the single per-object ownership decision shared by
// the bundle-CR guard and the synthesized-Secret guard. It Gets the live object
// (by name for cluster-scoped, namespace+name otherwise) and classifies it:
//   - Absent → adopt=false, nil, nil: caller creates it (force-apply is safe).
//   - Present AND carrying THIS install's LabelInstall → adopt=false, nil, nil:
//     ours from a prior run; force-apply converges it (re-install idempotent).
//   - Present, not ours, but an adoptable declared cluster dep → adopt=true:
//     EnsureClusterDeps pin-checks and adopts it metadata-only AFTER this
//     decision runs, so the caller SKIPs the spec-overwriting apply.
//   - Present, not ours, not adoptable → returned as a *Conflict: the caller
//     collects them all and runs the adopt decision (resolveConflicts) before
//     any write. An unadopted conflict is still a hard, named error.
//
// This is the ONE construction site for a Conflict, and Conflict.Secret is
// derived here from obj.GetKind() rather than left to a caller. Bundled CRs
// happen never to be Secrets today, but that is a property of
// allowedBundleKinds, not of this function: deriving the flag from the object
// keeps it truthful if a bundled Secret is ever admitted. This is a plain
// construction-site check, not a branch on the decision path.
func checkResourceOwnership(ctx context.Context, c client.Client, obj *unstructured.Unstructured, installName, installNamespace string, clusterScoped, adoptable bool) (adopt bool, conflict *Conflict, err error) {
	key := obj.GetKind() + "/" + obj.GetName()
	objKey := client.ObjectKey{Name: obj.GetName()}
	if !clusterScoped {
		objKey.Namespace = obj.GetNamespace()
	}
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(obj.GroupVersionKind())
	if gerr := c.Get(ctx, objKey, live); gerr != nil {
		if apierrors.IsNotFound(gerr) {
			return false, nil, nil // absent → create (force-apply is safe)
		}
		return false, nil, fmt.Errorf("install: check %s: %w", key, gerr)
	}
	if live.GetLabels()[instance.LabelInstall] == installName {
		// A NAMESPACED object is already separated by its own namespace in
		// objKey above, so the name match is the whole question there.
		//
		// A CLUSTER-SCOPED one is not separated by anything, and the install
		// name is a value the caller types. Pair it with the namespace the
		// stamping install ran in — the thing RBAC actually gates — so a
		// same-name install cannot converge, and force-apply over, another
		// tenant's cluster-scoped object. That path returned "ours" here,
		// ahead of resolveConflicts' cluster-scoped refusal, so the refusal
		// never saw it.
		//
		// An object stamped before this label existed carries no namespace and
		// keeps the name-only ownership it had: a cluster-scoped conflict is
		// never adoptable, so reading "absent" as "not ours" would hard-break
		// the next re-install of every install already in the field. It gets
		// strict on the first re-stamp. Stripping the label to reach the
		// fallback means editing the object, which already implies the access
		// this guard is protecting.
		stampedNs := live.GetLabels()[instance.LabelInstallNamespace]
		if !clusterScoped || stampedNs == "" || stampedNs == installNamespace {
			return false, nil, nil // ours from a prior run of this same install → converge
		}
	}
	if adoptable {
		// Intended shared dep: EnsureClusterDeps pin-checks + adopts it
		// metadata-only. Preserve its spec — do not force-apply.
		return true, nil, nil
	}
	// Not ours and not a declared shared dep: report it as a conflict rather
	// than erroring here, so Install can collect every collision and offer the
	// caller one adopt decision over all of them (see resolveConflicts).
	return false, &Conflict{
		Kind:      obj.GetKind(),
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
		// Derived from the object, not the caller — see this function's doc
		// comment.
		Secret:        obj.GetKind() == "Secret",
		ClusterScoped: clusterScoped,
	}, nil
}

// kindNameSet returns the set of "Kind/Name" for every CR in crs — used to
// distinguish a required cluster dep the bundle itself creates from one that is
// genuinely missing.
func kindNameSet(crs []*unstructured.Unstructured) map[string]bool {
	set := make(map[string]bool, len(crs))
	for _, cr := range crs {
		set[cr.GetKind()+"/"+cr.GetName()] = true
	}
	return set
}

// findAgentClass returns the bundle's AgentClass CR — a .oap describes exactly
// one, so the first found is returned. Absent is a fail-closed error: Install
// has nothing to name the install after and no owner for dep adoption.
func findAgentClass(crs []*unstructured.Unstructured) (*unstructured.Unstructured, error) {
	for _, cr := range crs {
		if cr.GetKind() == "AgentClass" {
			return cr, nil
		}
	}
	return nil, fmt.Errorf("install: bundle has no AgentClass CR")
}

// stampOapSource sets instance.AnnotationOapSource on agentClass, recording
// where the bundle came from and the manifest's declared version. It stamps NO
// install timestamp: the annotation must be a pure function of the bundle so a
// byte-identical re-install is a true SSA no-op (the wall-clock installedAt
// lives in controller-owned status). An empty opts.SourceKind defaults to
// "file" with an empty ref — see InstallOpts.SourceKind.
func stampOapSource(agentClass *unstructured.Unstructured, version string, opts InstallOpts) error {
	kind := opts.SourceKind
	if kind == "" {
		kind = "file"
	}
	value, err := instance.MarshalOapSource(instance.OapSource{
		Ref:        opts.SourceRef,
		Digest:     opts.SourceDigest,
		Version:    version,
		SourceKind: kind,
	})
	if err != nil {
		return err
	}
	annotations := agentClass.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[instance.AnnotationOapSource] = value
	agentClass.SetAnnotations(annotations)
	return nil
}

// completeSlotStringField sets slot[key] to def when unset or empty, so the
// installer writes what the apiserver would have defaulted. A non-string
// value is an error naming the slot index — a malformed slot must refuse,
// not be silently overwritten.
func completeSlotStringField(slot map[string]interface{}, i int, key, def string) (bool, error) {
	v, _, err := unstructured.NestedString(slot, key)
	if err != nil {
		return false, fmt.Errorf("read spec.authz.slots[%d].%s: %w", i, key, err)
	}
	if v != "" {
		return false, nil
	}
	slot[key] = def
	return true, nil
}

// completeAuthzSlotDefaults fills spec.authz.slots[].membership,
// .occupancy, and .rebind with their v1alpha1.AuthzSlot*Default constants
// wherever a slot leaves one absent or empty — which is the normal case,
// since no bundle author spells out a value that only ever mirrors the
// field's own +kubebuilder:default.
//
// It exists because AgentClassSpec.Authz.Slots is +listType=atomic
// (pkg/apis/v1alpha1/agentclass_types.go): SSA treats the whole list as one
// value, so a re-applied slot list REPLACES the live one at merge time rather
// than merging element by element. A slot that omits one of these fields
// therefore applies as a slot the apiserver has since defaulted; the merged
// object differs from the live one by exactly that field, the field manager
// records a change even though the apiserver re-defaults the field on the
// way to storage and the stored spec ends up unchanged. That is a real
// violation of "a byte-identical re-apply must be a no-op" (AGENTS.md), even
// though nothing in the stored spec ever moves. Writing the defaults into
// the payload ourselves keeps what install applies identical to what the
// apiserver would store either way.
//
// A slot that already sets a field — default or not — is left exactly as
// declared; a class with no authz.slots at all is left untouched rather than
// gaining a fabricated empty list (itself an applied-field change).
func completeAuthzSlotDefaults(agentClass *unstructured.Unstructured) error {
	slots, found, err := unstructured.NestedSlice(agentClass.Object, "spec", "authz", "slots")
	if err != nil {
		return fmt.Errorf("read spec.authz.slots: %w", err)
	}
	if !found {
		return nil
	}

	changed := false
	for i, s := range slots {
		slot, ok := s.(map[string]interface{})
		if !ok {
			return fmt.Errorf("spec.authz.slots[%d] is %T, not an object", i, s)
		}
		for _, field := range []struct {
			key string
			def string
		}{
			{"membership", v1alpha1.AuthzSlotMembershipDefault},
			{"occupancy", v1alpha1.AuthzSlotOccupancyDefault},
			{"rebind", v1alpha1.AuthzSlotRebindDefault},
		} {
			fieldChanged, err := completeSlotStringField(slot, i, field.key, field.def)
			if err != nil {
				return err
			}
			changed = changed || fieldChanged
		}
	}
	if !changed {
		return nil
	}
	if err := unstructured.SetNestedSlice(agentClass.Object, slots, "spec", "authz", "slots"); err != nil {
		return fmt.Errorf("write completed spec.authz.slots: %w", err)
	}
	return nil
}

// setNamespace stamps metadata.namespace = ns onto every namespaced CR,
// skipping cluster-scoped kinds (clusterScopedKinds) entirely — setting a
// namespace on those is rejected by the apiserver.
func setNamespace(crs []*unstructured.Unstructured, ns string) {
	for _, cr := range crs {
		if clusterScopedKinds[cr.GetKind()] {
			continue
		}
		cr.SetNamespace(ns)
	}
}

// secretUnstructured builds the corev1 Secret (as unstructured, so it can
// share instance.Stamp and ssaApply with the bundled CRs) for one resolved
// SecretSpec. The plaintext value is placed only in stringData, never logged
// or included in any error this package returns.
//
// It carries adoptguard.AdoptedLabel from creation. The operator's manager
// cache label-filters its Secret informer on that label, and the label is
// otherwise only ever stamped BY a reconcile — so a Secret without it fires no
// watch event, and the AgentIdentity referencing it converges only on that
// reconciler's own timed re-check, with a later rotation of the value invisible
// to the watch entirely. Install knows the CRs it is about to apply reference
// this Secret, which is exactly the "an object the operator creates" case
// WithAdoptedLabel exists for; cmd/oap's central model-token Secret is stamped
// the same way. The label is constant, so a re-apply stays byte-identical.
func secretUnstructured(s SecretSpec, ns string) (*unstructured.Unstructured, error) {
	sec := &unstructured.Unstructured{}
	sec.SetAPIVersion("v1")
	sec.SetKind("Secret")
	sec.SetName(s.Name)
	sec.SetNamespace(ns)
	adoptguard.WithAdoptedLabel(sec)
	if err := unstructured.SetNestedStringMap(sec.Object, map[string]string{s.Key: s.Value}, "stringData"); err != nil {
		return nil, err
	}
	return sec, nil
}

// ssaApply server-side-applies obj under FieldManager, forcing ownership of
// any conflicting field — the same pattern pkg/tools/adoptkit.Adopt and
// cmd/oap/internal/toolscli.ApplyStream use. Idempotent: applying the same obj
// twice is a no-op update, which is what makes Install safe to re-run.

func ssaApply(ctx context.Context, c client.Client, obj *unstructured.Unstructured, identity objectIdentity) error {
	// UID prevents a same-name replacement from being seized. ResourceVersion
	// makes the apply conditional on the exact state Prepare approved (or Create
	// returned), so disappearance, replacement, or an intervening update fails
	// closed instead of turning SSA into an untracked create/overwrite.
	obj.SetUID(identity.uid)
	obj.SetResourceVersion(identity.resourceVersion)
	if err := c.Patch(ctx, obj, client.Apply, client.FieldOwner(FieldManager), client.ForceOwnership); err != nil {
		return fmt.Errorf("ssa-apply %s/%s: %w", obj.GetKind(), obj.GetName(), err)
	}
	return nil
}

// withAdoptedHint augments a write-loop error with the keys resolveConflicts
// already decided to adopt in this same Install call. Adoption is recorded
// NOWHERE on the object itself (Result.Adopted is the only record), and a
// write-loop failure returns (nil, err), discarding Result — so without this a
// mid-loop failure would silently lose the fact that this run force-seized a
// pre-existing object. A nil/empty adopted leaves err untouched.
func withAdoptedHint(err error, adopted []string) error {
	if len(adopted) == 0 {
		return err
	}
	return fmt.Errorf("%w (already adopted: %v)", err, adopted)
}

// rewriteImageRefs replaces bundled image refs with the refs install resolved
// (built+pushed digests, or the same tag after a local load). Only the two
// image-bearing fields a .oap bundle can carry are touched: SpiceboxClass
// spec.image and SidecarToolbox spec.source.image. A ref absent from rewrites
// (or an identity mapping) is left exactly as written.
func rewriteImageRefs(crs []*unstructured.Unstructured, rewrites map[string]string) error {
	if len(rewrites) == 0 {
		return nil
	}
	for _, cr := range crs {
		switch cr.GetKind() {
		case "SpiceboxClass":
			if err := rewriteImageField(cr, rewrites, "spec", "image"); err != nil {
				return err
			}
		case "SidecarToolbox":
			if err := rewriteImageField(cr, rewrites, "spec", "source", "image"); err != nil {
				return err
			}
		}
	}
	return nil
}

func rewriteImageField(cr *unstructured.Unstructured, rewrites map[string]string, path ...string) error {
	cur, found, err := unstructured.NestedString(cr.Object, path...)
	if err != nil {
		return fmt.Errorf("install: read %s image field %v: %w", cr.GetKind(), path, err)
	}
	if !found {
		return nil
	}
	if next, ok := rewrites[cur]; ok && next != cur {
		if err := unstructured.SetNestedField(cr.Object, next, path...); err != nil {
			return fmt.Errorf("install: rewrite %s image field %v: %w", cr.GetKind(), path, err)
		}
	}
	return nil
}
