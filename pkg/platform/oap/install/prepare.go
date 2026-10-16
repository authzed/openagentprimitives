package install

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// AnnotationDependencyPath identifies the logical node independently of root ownership.
const AnnotationDependencyPath = "agentprimitives.authzed.com/oap-dependency-path"

// ObjectKey identifies a cluster object without retaining its contents.
type ObjectKey struct {
	GVK       schema.GroupVersionKind
	Namespace string
	Name      string
}

// Prepared holds a read-only preflight result. Secret payloads remain private.
// Options is runtime execution state and must never appear in plan serialization.
type Prepared struct {
	Bundle        *oap.Bundle
	CRs           []*unstructured.Unstructured
	Options       InstallOpts `json:"-"`
	InstallName   string
	Owner         types.NamespacedName
	Adopted       []string
	Warnings      []string
	Existing      map[ObjectKey]bool `json:"-"`
	observed      map[ObjectKey]objectIdentity
	secretObjects []*unstructured.Unstructured
	skipApply     map[string]bool
	bundledNames  map[string]bool
	created       []createdObject
	redactor      *redact.Redactor
}

type createdObject struct {
	key ObjectKey
	uid types.UID
}

type objectIdentity struct {
	uid             types.UID
	resourceVersion string
}

// String and GoString keep private Secret payloads out of ordinary diagnostic formatting.
func (p Prepared) String() string {
	return fmt.Sprintf("Prepared{name:%q, resources:%d, secrets:%d}", p.InstallName, len(p.CRs), len(p.secretObjects))
}
func (p Prepared) GoString() string { return p.String() }

type redactedError struct {
	message string
}

func (e *redactedError) Error() string    { return e.message }
func (e *redactedError) GoString() string { return e.message }

func redactError(r *redact.Redactor, err error) error {
	if r == nil || err == nil {
		return err
	}
	message := r.RedactInString(err.Error())
	if message == err.Error() {
		return err
	}
	// Do not retain or unwrap to err: it contains the plaintext value the
	// redactor just removed. Non-disclosure takes precedence over preserving an
	// unsafe error chain; callers still receive the safe message and any outer
	// graph path classification.
	return &redactedError{message: message}
}

func objectKey(obj client.Object) ObjectKey {
	return ObjectKey{GVK: obj.GetObjectKind().GroupVersionKind(), Namespace: obj.GetNamespace(), Name: obj.GetName()}
}

// snapshotClient records the very reads used for ownership classification;
// a later second read could classify a concurrently created foreign object as ours.
type snapshotClient struct {
	client.Client
	existing map[ObjectKey]bool
	observed map[ObjectKey]objectIdentity
}

func (c *snapshotClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if err == nil || apierrors.IsNotFound(err) {
		objectKey := ObjectKey{GVK: obj.GetObjectKind().GroupVersionKind(), Namespace: key.Namespace, Name: key.Name}
		c.existing[objectKey] = err == nil
		if err == nil {
			c.observed[objectKey] = objectIdentity{uid: obj.GetUID(), resourceVersion: obj.GetResourceVersion()}
		}
	}
	return err
}

func stampDependencyPath(objects []*unstructured.Unstructured, path oap.DependencyPath) {
	for _, obj := range objects {
		stampDependencyPathObject(obj, path)
	}
}

func stampDependencyPathObject(obj metav1.Object, path oap.DependencyPath) {
	if obj == nil {
		return
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[AnnotationDependencyPath] = path.String()
	obj.SetAnnotations(annotations)
}

// StampGraphOwnership joins a resource created by a surface-specific setup
// flow to the same root lifecycle and logical dependency path as bundle
// resources prepared by this package.
func StampGraphOwnership(obj metav1.Object, rootName, namespace string, path oap.DependencyPath) {
	instance.StampObject(obj, rootName, namespace)
	stampDependencyPathObject(obj, path)
}

// Prepare validates and transforms one bundle and completes every read and
// consent decision without mutating the cluster.
func Prepare(ctx context.Context, c client.Client, b *oap.Bundle, answers oap.Answers, secrets []SecretSpec, opts InstallOpts) (prepared *Prepared, err error) {
	redactor := redact.New()
	for _, secret := range secrets {
		redactor.RegisterSensitive(secret.Value, redact.Descriptor{Description: "secret answer"})
	}
	defer func() { err = redactError(redactor, err) }()
	if b == nil || b.Manifest == nil {
		return nil, fmt.Errorf("install: bundle has no manifest")
	}
	// Defensive: any failure applies nothing, even when a caller reaches Install
	// without the CLI's Preflight (this is the library entrypoint).
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("install: bundle invalid: %w", err)
	}

	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("install: decode bundle CRs: %w", err)
	}
	authoredKeys := make(map[*unstructured.Unstructured]string, len(crs))
	for _, cr := range crs {
		authoredKeys[cr] = cr.GetKind() + "/" + cr.GetName()
	}
	// Snapshot the bundle's Kind/Name set from the ORIGINAL (pre-overlay,
	// pre-rename) names, so EnsureClusterDeps can distinguish resources the
	// bundle creates even when a valid binding targets metadata.name.
	bundledNames := kindNameSet(crs)

	if err := oap.Apply(crs, b.Manifest.Questions, answers); err != nil {
		return nil, fmt.Errorf("install: overlay answers: %w", err)
	}

	if err := rewriteImageRefs(crs, opts.ImageRewrites); err != nil {
		return nil, err
	}

	agentClass, err := findAgentClass(crs)
	if err != nil {
		return nil, err
	}
	// Resolve the install's identity name from the bundled AgentClass's OWN
	// (pre-rename) name when --name is unset.
	installName := opts.Name
	if installName == "" {
		installName = agentClass.GetName()
	}

	if len(secrets) > 0 && opts.Namespace == "" {
		return nil, fmt.Errorf("install: %d secret(s) to create but no target namespace given (opts.Namespace); pass --namespace", len(secrets))
	}
	secretObjs := make([]*unstructured.Unstructured, 0, len(secrets))
	secretsByName := make(map[string]*unstructured.Unstructured)
	for _, s := range secrets {
		if sec, exists := secretsByName[s.Name]; exists {
			if err := unstructured.SetNestedField(sec.Object, s.Value, "stringData", s.Key); err != nil {
				return nil, fmt.Errorf("install: build secret %s: %w", s.Name, err)
			}
			continue
		}
		sec, err := secretUnstructured(s, opts.Namespace)
		if err != nil {
			return nil, fmt.Errorf("install: build secret %s: %w", s.Name, err)
		}
		secretObjs = append(secretObjs, sec)
		secretsByName[s.Name] = sec
		authoredKeys[sec] = sec.GetKind() + "/" + s.Name
	}

	if opts.Namespace != "" {
		setNamespace(crs, opts.Namespace)
	}
	if len(opts.ResourceNames) > 0 || opts.Name != "" {
		// Rename covers the bundled CRs AND the created Secrets in one pass, so a
		// Secret's prefixed name and every ref naming it (AgentIdentity credential
		// secretRefs) move together, along with the CRs' own cross-refs. A fresh
		// slice keeps crs's backing array intact; the elements are shared
		// pointers, so both crs and secretObjs are renamed in place.
		renameSet := make([]*unstructured.Unstructured, 0, len(crs)+len(secretObjs))
		renameSet = append(renameSet, crs...)
		renameSet = append(renameSet, secretObjs...)
		var err error
		if len(opts.ResourceNames) > 0 {
			// ResourceNames is the caller's already-frozen physical identity plan.
			// A binding may validly target metadata.name, so retain both authored
			// and post-overlay aliases while rewriting references. Workflow plans
			// these aliases from the answered graph identity; this fallback also
			// keeps direct Prepare callers with authored-key maps compatible.
			effectiveNames := maps.Clone(opts.ResourceNames)
			for _, obj := range renameSet {
				authoredKey := authoredKeys[obj]
				physical, ok := opts.ResourceNames[authoredKey]
				if !ok {
					continue
				}
				currentKey := obj.GetKind() + "/" + obj.GetName()
				if prior, exists := effectiveNames[currentKey]; exists && prior != physical {
					return nil, fmt.Errorf("install: overlay answers: resource %s changed metadata.name to authored identity %s", authoredKey, currentKey)
				}
				effectiveNames[currentKey] = physical
			}
			// Declared materializable Secrets retain their mapped references
			// on reinstall even when no answer writes the existing Secret.
			err = instance.RenameMapped(renameSet, effectiveNames, opts.DirectNames)
			installName = agentClass.GetName()
		} else {
			err = instance.Rename(renameSet, opts.Name+"-")
		}
		if err != nil {
			return nil, fmt.Errorf("install: rename CRs: %w", err)
		}
	}

	ownershipName := installName
	if opts.RootInstallName != "" {
		ownershipName = opts.RootInstallName
	}
	instance.Stamp(crs, ownershipName, opts.Namespace)
	instance.Stamp(secretObjs, ownershipName, opts.Namespace)
	stampDependencyPath(crs, opts.DependencyPath)
	stampDependencyPath(secretObjs, opts.DependencyPath)

	// Stamp provenance on the AgentClass CR ONLY, BEFORE the SSA-apply loop, so
	// it lands in the same write rather than a follow-up patch. agentClass
	// aliases the *unstructured.Unstructured held in crs, so annotating it here
	// is what ssaApply later writes.
	if err := stampOapSource(agentClass, b.Manifest.Agent.Version, opts); err != nil {
		return nil, fmt.Errorf("install: stamp oap-source annotation: %w", err)
	}

	// Same reasoning, same place: complete every slot's membership, occupancy,
	// and rebind BEFORE the SSA-apply loop so the applied payload matches what
	// the apiserver would store, keeping a byte-identical re-install a true
	// no-op. See completeAuthzSlotDefaults's doc comment for why this is
	// necessary at all (spec.authz.slots is +listType=atomic).
	if err := completeAuthzSlotDefaults(agentClass); err != nil {
		return nil, fmt.Errorf("install: complete authz slot defaults: %w", err)
	}

	owner := types.NamespacedName{Namespace: agentClass.GetNamespace(), Name: agentClass.GetName()}

	// ---- All read-only ownership guards + the adopt decision run BEFORE any
	// cluster write: a pin/ownership conflict must abort before a stray Secret,
	// CR, or cluster-dep adoption patch lands. ----
	// warnings is hoisted here so ExtraQuestions' notices land in the same slice
	// Result.Warnings returns rather than being shadowed and dropped.
	var warnings []string
	missing := cloneMissingAnswers(opts.priorMissing)
	var extra []oap.Question
	if opts.ExtraQuestions != nil {
		// The conflict ExtraQuestions checks (e.g. "this SpiceboxClass exceeds
		// what this cluster can schedule") is only knowable HERE — after the
		// answer overlay, since a manifest question can itself bind to
		// spec.resources. Reusing the manifest questions' own Resolve keeps every
		// install surface (CLI, desktop, admind) on one prompting path.
		var notices []string
		extra, notices, err = opts.ExtraQuestions(ctx, crs)
		if err != nil {
			return nil, fmt.Errorf("install: %w", err)
		}
		warnings = append(warnings, notices...)
	}
	// Gate on supplied values too, not just len(extra) > 0: ResolveValues is the
	// only place a --set/--values key is checked against "does this name a real
	// question" (rejectUnknownKeys). This validation must run even when no
	// ExtraQuestions hook is installed; otherwise a capacity.* value is silently
	// dropped instead of rejected.
	if len(extra) > 0 || len(opts.Sets) > 0 || len(opts.ExtraValues) > 0 {
		var extraAns oap.Answers
		var extraSecrets []SecretSpec
		if opts.decisionDiscovery {
			var extraMissing *MissingAnswersError
			extraAns, extraSecrets, extraMissing, err = resolveValuesPartial(extra, opts.ExtraValues, opts.Sets, opts.QuestionOptions...)
			missing = mergeMissingAnswers(missing, extraMissing)
		} else {
			extraAns, extraSecrets, err = ResolveValues(extra, opts.ExtraValues, opts.Sets, opts.Interactive, opts.QuestionOptions...)
		}
		if err != nil {
			return nil, fmt.Errorf("install: %w", err)
		}
		if len(extraSecrets) > 0 {
			// A secret-typed extra question is a programming error in the
			// generator, not a user error: install's Secret pipeline
			// (opts.Namespace, guardSynthesizedSecrets) only ever runs over the
			// caller's own `secrets` argument, never over extras — silently
			// dropping these would mean an answered secret never gets created.
			return nil, fmt.Errorf("install: extra questions must not declare secrets")
		}
		if err := oap.Apply(crs, extra, extraAns); err != nil {
			return nil, fmt.Errorf("install: apply extra answers: %w", err)
		}
	}
	// Ownership is decided FIRST, over every bundled CR and synthesized Secret
	// at once. Both guards are pure reads, so a declined adopt leaves the
	// cluster byte-identical — which is why EnsureClusterDeps (whose adoption
	// patch is a WRITE) runs after this, not before it.
	existing := make(map[ObjectKey]bool)
	observed := make(map[ObjectKey]objectIdentity)
	reader := &snapshotClient{Client: c, existing: existing, observed: observed}
	skipApply, crConflicts, err := planBundledResources(ctx, reader, crs, ownershipName, opts.Namespace, b.Manifest.Requires.ClusterDeps)
	if err != nil {
		return nil, err
	}
	secretConflicts, err := guardSynthesizedSecrets(ctx, reader, secretObjs, ownershipName, opts.Namespace)
	if err != nil {
		return nil, err
	}
	allConflicts := append(crConflicts, secretConflicts...)
	allConflicts = append(allConflicts, opts.additionalConflicts...)
	adopted, conflictErr := resolveConflicts(ctx, allConflicts, opts)
	var conflicts *ConflictError
	if conflictErr != nil {
		if !opts.decisionDiscovery || !errors.As(conflictErr, &conflicts) {
			return nil, conflictErr
		}
	}

	depWarnings, err := inspectClusterDeps(ctx, c, b.Manifest.Requires.ClusterDeps, bundledNames)
	if err != nil {
		return nil, fmt.Errorf("install: ensure cluster deps: %w", err)
	}
	warnings = append(warnings, depWarnings...)
	if opts.decisionDiscovery && (missing != nil || conflicts != nil) {
		return nil, &nodeDecisionError{Missing: missing, Conflicts: conflicts}
	}
	return &Prepared{
		Bundle: b, CRs: crs, Options: opts, InstallName: installName, Owner: owner,
		Adopted: adopted, Warnings: warnings, Existing: existing,
		observed: observed, secretObjects: secretObjs, skipApply: skipApply, bundledNames: bundledNames, redactor: redactor,
	}, nil
}

type nodeDecisionError struct {
	Missing   *MissingAnswersError
	Conflicts *ConflictError
}

func (e *nodeDecisionError) Error() string {
	parts := make([]string, 0, 2)
	if e.Missing != nil {
		parts = append(parts, e.Missing.Error())
	}
	if e.Conflicts != nil {
		parts = append(parts, e.Conflicts.Error())
	}
	return strings.Join(parts, "; ")
}

func cloneMissingAnswers(in *MissingAnswersError) *MissingAnswersError {
	if in == nil {
		return nil
	}
	return &MissingAnswersError{Names: slices.Clone(in.Names), Questions: slices.Clone(in.Questions)}
}

func mergeMissingAnswers(a, b *MissingAnswersError) *MissingAnswersError {
	if a == nil {
		return cloneMissingAnswers(b)
	}
	if b == nil {
		return a
	}
	a.Names = append(a.Names, b.Names...)
	a.Questions = append(a.Questions, b.Questions...)
	return a
}
