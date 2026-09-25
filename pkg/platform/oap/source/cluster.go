package source

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/toolkits"
)

// versionAnnotationOrLabel names the optional AgentClass annotation/label
// carrying the bundle's semantic version. Absent (or empty) on both ->
// "0.1.0".
const versionAnnotationOrLabel = "oap.agentprimitives.authzed.com/version"

// OpenCluster returns a Source that builds an oap.Bundle by walking a live
// AgentClass (namespace/name) and its ref graph via c. It never reads a
// Secret's VALUE — only Secret references, collected into
// Manifest.Requires.Secrets plus one secret Question per referenced Secret
// KEY, each carrying the createSecret target that lets install materialize the
// answer (see the loop in Bundle).
func OpenCluster(c client.Client, namespace, name string) Source {
	return &clusterSource{c: c, namespace: namespace, name: name}
}

type clusterSource struct {
	c         client.Client
	namespace string
	name      string
}

// crEntry pairs a sanitized CR with the identity used to sort the manifest
// stream deterministically (by Kind then Name) before marshaling.
type crEntry struct {
	kind string
	name string
	obj  *unstructured.Unstructured
}

// crSet is the accumulating, de-duplicated set of CRs bound for the bundle.
// De-dup is keyed by kind+"/"+name so the second-level toolkit recursion can't
// add an object twice (e.g. two toolspecs that share one toolkit).
type crSet struct {
	entries []crEntry
	seen    map[string]bool
}

func newCRSet() *crSet { return &crSet{seen: map[string]bool{}} }

// apGVK is the GroupVersionKind for a v1alpha1 CRD kind; configMapGVK is the
// core/v1 ConfigMap GVK. Both are stamped onto typed objects (which carry no
// TypeMeta after a Get) before conversion so the exported YAML is a valid,
// self-describing document.
func apGVK(kind string) schema.GroupVersionKind { return v1alpha1.SchemeGroupVersion.WithKind(kind) }

var configMapGVK = corev1.SchemeGroupVersion.WithKind("ConfigMap")

// has reports whether a CR of this GVK's kind + name is already in the set.
func (s *crSet) has(gvk schema.GroupVersionKind, name string) bool {
	return s.seen[gvk.Kind+"/"+name]
}

// add sanitizes obj (stamping gvk first) and appends it, unless a CR of the
// same kind+name is already present.
func (s *crSet) add(gvk schema.GroupVersionKind, name string, obj client.Object) error {
	key := gvk.Kind + "/" + name
	if s.seen[key] {
		return nil
	}
	u, err := toSanitizedUnstructured(obj, gvk)
	if err != nil {
		return fmt.Errorf("sanitize %s %q: %w", gvk.Kind, name, err)
	}
	s.seen[key] = true
	s.entries = append(s.entries, crEntry{kind: gvk.Kind, name: name, obj: u})
	return nil
}

func (s *clusterSource) Bundle(ctx context.Context) (*oap.Bundle, error) {
	logger := logr.FromContextOrDiscard(ctx)

	var ac v1alpha1.AgentClass
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: s.name}, &ac); err != nil {
		return nil, fmt.Errorf("get AgentClass %s/%s: %w", s.namespace, s.name, err)
	}

	crs := newCRSet()
	if err := crs.add(apGVK("AgentClass"), ac.Name, &ac); err != nil {
		return nil, err
	}

	// spec.systemPrompt.configMapRef: when the prompt lives in a ConfigMap,
	// follow it. ConfigMaps are NON-secret (safe to embed verbatim). Required
	// when set — a class whose prompt CR is missing re-applies with a dangling
	// prompt.
	if cmRef := ac.Spec.SystemPrompt.ConfigMapRef; cmRef != nil && cmRef.Name != "" {
		if err := s.addConfigMap(ctx, crs, cmRef.Name, fmt.Sprintf("AgentClass %s/%s spec.systemPrompt.configMapRef", s.namespace, ac.Name)); err != nil {
			return nil, err
		}
	}

	// First-level walk: every Get-by-name owning ref on AgentClass.spec. Track
	// the fetched SpiceboxToolspecs (to follow each to its SpiceboxToolkit) and
	// SidecarToolboxes (to follow inline-source ConfigMaps).
	var toolspecs []*v1alpha1.SpiceboxToolspec
	var sidecars []*v1alpha1.SidecarToolbox
	for _, row := range refDescriptors {
		for _, depName := range dedupNonEmpty(row.Names(&ac)) {
			key := client.ObjectKey{Name: depName}
			if !row.ClusterScoped {
				key.Namespace = s.namespace
			}

			obj := row.NewObj()
			if err := s.c.Get(ctx, key, obj); err != nil {
				if apierrors.IsNotFound(err) {
					if row.Required {
						return nil, fmt.Errorf("%s %q not found (referenced by AgentClass %s/%s)", row.Kind, depName, s.namespace, s.name)
					}
					logger.Info("oap: optional ref not found, skipping", "kind", row.Kind, "name", depName, "agentClass", s.name)
					continue
				}
				return nil, fmt.Errorf("get %s %q: %w", row.Kind, depName, err)
			}

			switch typed := obj.(type) {
			case *v1alpha1.SpiceboxToolspec:
				toolspecs = append(toolspecs, typed)
			case *v1alpha1.SidecarToolbox:
				sidecars = append(sidecars, typed)
			}
			if err := crs.add(apGVK(row.Kind), depName, obj); err != nil {
				return nil, err
			}
		}
	}

	// Second-level: follow each SpiceboxToolspec to its SpiceboxToolkit. A
	// toolkit that is a compile-time registry builtin needs no CR; a
	// non-builtin toolkit is a REQUIRED ref (a missing one re-applies broken —
	// the Toolspec would go Valid=False "no toolkit named %q at revision %q").
	if err := s.walkToolkits(ctx, toolspecs, crs); err != nil {
		return nil, err
	}

	// Second-level: follow each inline-source SidecarToolbox to the ConfigMap
	// backing its script. Required when set (the sidecar can't build without it).
	if err := s.walkSidecarInlineConfigMaps(ctx, sidecars, crs); err != nil {
		return nil, err
	}

	// Channels are NOT exported: a Channel is per-install deployment config
	// (which workspace, which tokens) chosen in the install UI, never part of a
	// portable agent container — and it is not an allowed bundle kind, so
	// emitting one would fail Bundle.Validate below.

	sort.Slice(crs.entries, func(i, j int) bool {
		if crs.entries[i].kind != crs.entries[j].kind {
			return crs.entries[i].kind < crs.entries[j].kind
		}
		return crs.entries[i].name < crs.entries[j].name
	})

	manifestsYAML, err := marshalCRStream(crs.entries)
	if err != nil {
		return nil, err
	}

	// Derive the inheritable fields (agent identity, skills, secrets) from the
	// assembled CR set with the SAME engine FromFolder uses, so a
	// cluster-exported and a hand-authored bundle of the same graph produce
	// identical Agent/Requires. The entries are already sanitized unstructured;
	// their spec (credentials, skills) is intact, which is what DeriveInherited
	// reads. Version is not an AgentClass fact, so this path supplies it from
	// the version annotation/label (resolveVersion).
	uList := make([]*unstructured.Unstructured, len(crs.entries))
	for i := range crs.entries {
		uList[i] = crs.entries[i].obj
	}
	agent, requires, err := oap.DeriveInherited(uList, logger)
	if err != nil {
		return nil, err
	}
	agent.Version = resolveVersion(&ac)

	// TODO: Manifest.Requires.Images is intentionally NOT populated. Image refs
	// (MCPServer/SidecarToolbox images, the runner/sandbox images) ride inline
	// inside the exported CR specs; OCI push/vendoring and install will need an
	// extracted, deduped RequiredImage list collected from the fetched dep specs.

	m := &oap.Manifest{
		OapFormatVersion: "1",
		Agent:            agent,
		Requires:         requires,
	}

	// One secret Question per referenced KEY, each naming the Secret name+key
	// the exported CRs read. Without that target install.Resolve has nowhere to
	// put the collected value and drops it, so the bundle applies CRs
	// referencing a Secret the install never created — invisible when
	// re-installed into the namespace exported from, and broken in exactly the
	// cross-cluster case this exporter exists for.
	//
	// A secret referenced as a WHOLE (no key — the fixed multi-key OAuth
	// convention) gets no question: one typed line cannot reconstruct an
	// access/refresh token set, and inventing a key would materialize a Secret
	// the credential resolver cannot read. Its RequiredSecret then carries no
	// Question, which is what tells preflight the identity's own setup flow
	// satisfies it.
	for i := range m.Requires.Secrets {
		rs := &m.Requires.Secrets[i]
		keyCount := len(rs.Keys)
		for _, k := range rs.Keys {
			q := oap.Question{
				Name:   secretQuestionName(rs.Name, k, keyCount),
				Type:   oap.QSecret,
				Prompt: secretQuestionPrompt(rs.Name, k, keyCount),
				Secret: &oap.SecretQuestion{
					CreateSecret: &oap.SecretTarget{Name: rs.Name, Key: k},
					// The Secret may already exist in the target namespace —
					// re-installing into the namespace this was exported from
					// is the common case.
					OrExisting: true,
				},
			}
			if rs.Question == "" {
				// RequiredSecret carries one question pointer; the first key's
				// question is the one it names. Every key still gets its own
				// question above, so nothing is uncollectable.
				rs.Question = q.Name
			}
			m.Questions = append(m.Questions, q)
		}
	}

	b := &oap.Bundle{
		Manifest:  m,
		Manifests: manifestsYAML,
		// Asset extraction (a logo referenced off the AgentClass) is not
		// implemented by this adapter.
		Assets: map[string][]byte{},
	}
	if err := b.Validate(); err != nil {
		return nil, fmt.Errorf("built bundle failed validation: %w", err)
	}
	return b, nil
}

// secretQuestionName names the question that collects one Secret key's value.
// The common shape is a Secret referenced for a single key, and there the
// question keeps the Secret's own name: what the operator recognizes, what
// `--set` takes, and what keeps the RequiredSecret's Question pointer equal to
// its Name. Distinct keys of one Secret need distinct names, so those are
// qualified by key.
func secretQuestionName(secretName, key string, keyCount int) string {
	if keyCount <= 1 {
		return secretName
	}
	return secretName + "." + key
}

// secretQuestionPrompt is what the person installing the bundle is asked. The
// key is named only when one Secret needs more than one answer, so the common
// case reads as one credential rather than as a Secret-shaped structure.
func secretQuestionPrompt(secretName, key string, keyCount int) string {
	if keyCount <= 1 {
		return fmt.Sprintf("Secret value for %q", secretName)
	}
	return fmt.Sprintf("Secret value for %q key %q", secretName, key)
}

// walkToolkits follows each SpiceboxToolspec's Spec.Toolkit.{Name,Revision} to
// its backing SpiceboxToolkit CR, adding non-builtin toolkits to crs. Builtin
// toolkits (compile-time embedded, toolkits.All) are resolved in-process and
// carry no CR. A non-builtin toolkit with no matching CR is a fail-closed
// required-ref error.
func (s *clusterSource) walkToolkits(ctx context.Context, toolspecs []*v1alpha1.SpiceboxToolspec, crs *crSet) error {
	if len(toolspecs) == 0 {
		return nil
	}
	builtins := builtinToolkitKeys()

	// List SpiceboxToolkits once. The Toolspec references a toolkit by its
	// SPEC (name, revision) — NOT the CR's metadata.name — so resolution is a
	// List+filter on the spec fields, mirroring toolspec/registry.Resolve.
	var list v1alpha1.SpiceboxToolkitList
	listErr := s.c.List(ctx, &list)

	for _, ts := range toolspecs {
		ref := ts.Spec.Toolkit
		if _, ok := builtins[toolkitKey{name: ref.Name, revision: ref.Revision}]; ok {
			continue
		}
		if listErr != nil {
			return fmt.Errorf("list SpiceboxToolkits (resolving toolkit %q@%q for Toolspec %q): %w", ref.Name, ref.Revision, ts.Name, listErr)
		}
		var matched *v1alpha1.SpiceboxToolkit
		for i := range list.Items {
			if list.Items[i].Spec.Name == ref.Name && list.Items[i].Spec.ToolkitRevision == ref.Revision {
				matched = &list.Items[i]
				break
			}
		}
		if matched == nil {
			return fmt.Errorf("SpiceboxToolkit %q at revision %q not found (referenced by SpiceboxToolspec %q)", ref.Name, ref.Revision, ts.Name)
		}
		if err := crs.add(apGVK("SpiceboxToolkit"), matched.Name, matched); err != nil {
			return err
		}
	}
	return nil
}

// walkSidecarInlineConfigMaps follows each SidecarToolbox whose source is an
// inline script to the ConfigMap backing that script. Required when set: a
// sidecar with an inline source can't build without its script ConfigMap.
// ConfigMaps are NON-secret (safe to embed).
func (s *clusterSource) walkSidecarInlineConfigMaps(ctx context.Context, sidecars []*v1alpha1.SidecarToolbox, crs *crSet) error {
	for _, sbx := range sidecars {
		inline := sbx.Spec.Source.Inline
		if inline == nil {
			continue
		}
		cmName := inline.Script.ConfigMapRef.Name
		if cmName == "" {
			continue
		}
		if err := s.addConfigMap(ctx, crs, cmName, fmt.Sprintf("SidecarToolbox %s/%s source.inline.script.configMapRef", s.namespace, sbx.Name)); err != nil {
			return err
		}
	}
	return nil
}

// addConfigMap Gets a ConfigMap in the AgentClass's namespace, Sanitizes it,
// and adds it to crs. A missing ConfigMap is a fail-closed required-ref error
// naming its referrer. Idempotent: a ConfigMap already in the set is skipped
// without a redundant Get. ConfigMaps hold non-secret config (prompt text,
// sidecar scripts), so their VALUES are embedded — unlike Secrets.
func (s *clusterSource) addConfigMap(ctx context.Context, crs *crSet, name, referencedBy string) error {
	if crs.has(configMapGVK, name) {
		return nil
	}
	var cm corev1.ConfigMap
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: name}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("ConfigMap %q not found (referenced by %s)", name, referencedBy)
		}
		return fmt.Errorf("get ConfigMap %q: %w", name, err)
	}
	return crs.add(configMapGVK, name, &cm)
}

// toolkitKey identifies a toolkit by the (name, revision) pair a Toolspec
// references it with — matching both the builtin registry and the
// SpiceboxToolkit CR's spec fields.
type toolkitKey struct{ name, revision string }

// builtinToolkitKeys returns the set of compile-time registry builtins,
// mirroring pkg/controllers/spiceboxtoolspec's isBuiltin check so this adapter
// agrees with the operator on which toolkits need no CR.
func builtinToolkitKeys() map[toolkitKey]struct{} {
	set := map[toolkitKey]struct{}{}
	for _, tk := range toolkits.All() {
		set[toolkitKey{name: tk.Name, revision: tk.ToolkitRevision}] = struct{}{}
	}
	return set
}

// toSanitizedUnstructured converts a typed client.Object into Sanitize()d
// unstructured form. A typed object read back via controller-runtime's Get
// does not carry TypeMeta, so the GVK is stamped first (same pattern as
// cmd/oap/internal/toolscli/apply.go) to keep the output YAML a valid,
// self-describing CR.
func toSanitizedUnstructured(obj client.Object, gvk schema.GroupVersionKind) (*unstructured.Unstructured, error) {
	obj.GetObjectKind().SetGroupVersionKind(gvk)
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	Sanitize(u)
	return u, nil
}

// marshalCRStream renders crs (already sorted by the caller) as a
// deterministic multi-document YAML stream.
func marshalCRStream(crs []crEntry) ([]byte, error) {
	var buf bytes.Buffer
	for i, e := range crs {
		if i > 0 {
			buf.WriteString("---\n")
		}
		data, err := yaml.Marshal(e.obj.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal %s/%s: %w", e.kind, e.name, err)
		}
		buf.Write(data)
	}
	return buf.Bytes(), nil
}

// resolveVersion returns the AgentClass's version annotation/label, or
// "0.1.0" when neither is set.
func resolveVersion(ac *v1alpha1.AgentClass) string {
	if v := ac.Annotations[versionAnnotationOrLabel]; v != "" {
		return v
	}
	if v := ac.Labels[versionAnnotationOrLabel]; v != "" {
		return v
	}
	return "0.1.0"
}

// dedupNonEmpty drops empty strings and duplicate entries, preserving first
// occurrence order.
func dedupNonEmpty(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
