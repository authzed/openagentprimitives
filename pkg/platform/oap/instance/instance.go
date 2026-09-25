// Package instance stamps install-identifying labels onto a bundle's CRs
// and, on --name/collision, prefixes their names and rewrites the naming
// cross-references between them so the graph stays internally consistent
// after the prefix is applied.
package instance

import (
	"encoding/json"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports" // every credkind must be registered: an unknown type fails the rename closed
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

const (
	// labelInstance is the standard Kubernetes app.kubernetes.io/instance
	// label, set to the install name.
	labelInstance = "app.kubernetes.io/instance"

	// LabelInstall is this project's own install marker, set to the same
	// install name — kept distinct from app.kubernetes.io/instance so
	// selection by "an oap install" doesn't accidentally collide with
	// unrelated uses of the generic Kubernetes recommended label. Exported so
	// callers that need to SELECT an install's resources (pkg/platform/oap/install's
	// Uninstall) key off this constant rather than hardcoding the string.
	LabelInstall = "agentprimitives.authzed.com/oap-install"

	// LabelInstallNamespace records WHERE the install that stamped this object
	// ran. It exists for CLUSTER-SCOPED objects, which have no namespace of
	// their own and were therefore separated only by LabelInstall — a value the
	// caller picks with --name. A same-name install from anywhere read as "ours
	// from a prior run" and force-applied over the other install's spec, ahead
	// of the cluster-scoped refusal, with no conflict raised.
	//
	// Namespace is what RBAC actually gates, so pairing the two makes the
	// ownership claim as strong as the caller's access. Absent on an object
	// stamped before this label existed, which the ownership check reads as the
	// name-only match it always had rather than as a conflict — a cluster-scoped
	// conflict is never adoptable, so treating "absent" as "not ours" would
	// hard-break the next re-install of every install already in the field.
	LabelInstallNamespace = "agentprimitives.authzed.com/oap-install-namespace"

	// AnnotationOapSource is the annotation key install.Install stamps onto the
	// bundled AgentClass CR, carrying the marshaled (MarshalOapSource)
	// provenance of the install. It deliberately carries NO install timestamp:
	// the value must be a pure function of the bundle so a byte-identical
	// re-install is a true server-side-apply no-op. The AgentClass controller
	// mirrors it into AgentClassStatus.OapInstall (recording the wall-clock
	// installedAt there, in controller-owned status) so provenance survives an
	// external editor stripping the annotation.
	AnnotationOapSource = "agentprimitives.authzed.com/oap-source"
)

// OapSource is the provenance of one `.oap` bundle install, marshaled into the
// AnnotationOapSource annotation value on the bundle's AgentClass CR. It
// carries NO install timestamp on purpose: the annotation must depend only on
// the bundle so re-installing the same bundle produces a byte-identical value
// (a true SSA no-op). The wall-clock install time lives in controller-owned
// status (AgentClassStatus.OapInstall.InstalledAt), stamped set-once-per-digest.
type OapSource struct {
	// Where the bundle came from, in a form meaningful to SourceKind (an OCI
	// reference for "registry"); empty for "file", which has no registry identity.
	Ref string `json:"ref,omitempty"`
	// Content digest of the installed bundle, e.g. "sha256:...".
	Digest string `json:"digest,omitempty"`
	// The bundle manifest's declared agent.version.
	Version string `json:"version,omitempty"`
	// "embedded" identifies a private child artifact (Ref is agents/<logical/path>).
	// "registry" (Ref/Digest resolved from an OCI pull) or "file" (a local .oap
	// or folder-source directory, where Ref is empty).
	SourceKind string `json:"sourceKind,omitempty"`
}

// MarshalOapSource compactly JSON-encodes s for use as an annotation value.
func MarshalOapSource(s OapSource) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("oap/instance: marshal oap-source: %w", err)
	}
	return string(b), nil
}

// ParseOapSource decodes an AnnotationOapSource annotation value produced by
// MarshalOapSource back into an OapSource.
func ParseOapSource(raw string) (OapSource, error) {
	var s OapSource
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return OapSource{}, fmt.Errorf("oap/instance: parse oap-source: %w", err)
	}
	return s, nil
}

// Stamp sets install-identifying labels on every CR: app.kubernetes.io/instance
// and agentprimitives.authzed.com/oap-install, both set to name, plus
// agentprimitives.authzed.com/oap-install-namespace set to namespace. The labels
// map is created if absent; any pre-existing labels are preserved.
//
// namespace is the install's own namespace, not the object's — the two differ
// for a cluster-scoped CR, which is the case the label exists for (see
// LabelInstallNamespace). An empty namespace stamps nothing, so a caller with
// no namespace to name leaves the object exactly as it was rather than
// stamping a blank claim that would later read as "stamped for the empty
// namespace".
func Stamp(crs []*unstructured.Unstructured, name, namespace string) {
	for _, cr := range crs {
		StampObject(cr, name, namespace)
	}
}

// StampObject applies the same install identity as Stamp to one typed or
// unstructured Kubernetes object. Surfaces that create graph-owned resources
// outside the bundle apply loop use this instead of restating label keys.
func StampObject(obj metav1.Object, name, namespace string) {
	if obj == nil {
		return
	}
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelInstance] = name
	labels[LabelInstall] = name
	if namespace != "" {
		labels[LabelInstallNamespace] = namespace
	}
	obj.SetLabels(labels)
}

// Rename prefixes every CR's metadata.name with prefix, then rewrites the
// naming cross-references between the bundled CRs so a renamed dependent is
// still referenced correctly. A ref naming a CR NOT present in this bundle
// (external, pre-existing in the cluster) is left untouched.
//
// The rewritten forward-ref set (AgentClass -> dependents) mirrors
// source/cluster_refs.go's refDescriptors table, the authoritative
// AgentClass -> dependent-Kind ref graph:
//
//   - spec.agentIdentity                -> AgentIdentity
//   - spec.toolBundles[].agentIdentity  -> AgentIdentity (per-bundle override)
//   - spec.agentUI.ref                  -> AgentUI (browser-UI grant target)
//   - spec.mcpServers[].ref             -> MCPServer
//   - spec.sidecarToolboxes[].ref       -> SidecarToolbox
//   - spec.toolBundles[].class          -> SpiceboxClass
//   - spec.toolBundles[].toolspecs[]    -> SpiceboxToolspec
//
// It ALSO rewrites the skill and second-level refs the bundle carries beyond
// that table — otherwise a --name install leaves them naming the unprefixed
// originals, mis-binding or dangling:
//
//   - SkillSource.spec.auth.agentIdentity   -> AgentIdentity (skill clone creds)
//   - SpiceboxToolspec.spec.toolkit.name    -> SpiceboxToolkit (second-level)
//   - AgentIdentity.spec.credentials[]      -> Secret, at whichever path the
//     credential's own registered kind declares (credkind.Kind.SecretRefPath),
//     so every type is covered — including ones added later — rather than the
//     two that happened to be enumerated here
//
// The Secret refs resolve against the Secrets install materializes from its
// SecretSpecs (passed to Rename alongside the CRs), so each --name instance
// references its own prefixed Secret rather than one shared fixed-name Secret.
//
// The .ref/.class/.toolspecs fields are the ones that actually name a dependent
// CR; the sibling .name fields on AgentClassMCPServerRef /
// AgentClassSidecarToolboxRef / ToolBundle are the LLM-facing tool prefix
// (pattern [a-z0-9_-]{1,32}) and are deliberately left untouched.
//
// Returns an error only on a structural surprise — a ref field present with an
// unexpected type. An absent field is skipped, not an error.
func Rename(crs []*unstructured.Unstructured, prefix string) error {
	return RenameMapped(crs, renamedNames(crs, prefix), nil)
}

// RenameMapped renames every CR from the complete Kind/original-name map,
// rewrites bundle-internal references from that same map, and applies the
// caller-supplied direct child AgentClass mapping to parent rosters.
func RenameMapped(crs []*unstructured.Unstructured, names NameMap, externalAgents map[string]string) error {
	seen := make(map[string]string, len(crs))
	for i, cr := range crs {
		if cr == nil {
			return fmt.Errorf("oap: rename resource %d: nil object", i)
		}
		old := cr.GetName()
		if old == "" {
			continue
		}
		key := cr.GetKind() + "/" + old
		newName, ok := names[key]
		if !ok {
			return fmt.Errorf("oap: rename resource %s: no mapped name", key)
		}
		if newName == "" {
			return fmt.Errorf("oap: rename resource %s: mapped name is empty", key)
		}
		physicalKey := cr.GetKind() + "/" + newName
		if prior, exists := seen[physicalKey]; exists {
			return fmt.Errorf("oap: rename resources %s and %s to the same name %q", prior, key, newName)
		}
		seen[physicalKey] = key
	}

	for _, cr := range crs {
		if old := cr.GetName(); old != "" {
			cr.SetName(names[cr.GetKind()+"/"+old])
		}
	}

	for _, cr := range crs {
		var err error
		switch cr.GetKind() {
		case "AgentClass":
			if err = rewriteAgentClassRefs(cr, names); err == nil {
				err = RewriteSubagentRefs(cr, externalAgents)
			}
		case "SkillSource":
			err = rewriteSkillSourceRefs(cr, names)
		case "SpiceboxToolspec":
			err = rewriteToolspecRefs(cr, names)
		case "AgentIdentity":
			err = rewriteAgentIdentityRefs(cr, names)
		}
		if err != nil {
			return fmt.Errorf("oap: rewrite refs on %s %q: %w", cr.GetKind(), cr.GetName(), err)
		}
	}
	return nil
}

// renamedNames builds the old-name -> new-name map, keyed by "Kind/name" so
// a cluster-scoped kind (e.g. SpiceboxClass) can't collide in name with a
// namespaced one. Only names of CRs actually present in this bundle are
// recorded — that's what makes an external ref distinguishable from a
// bundled one during the rewrite pass.
func renamedNames(crs []*unstructured.Unstructured, prefix string) NameMap {
	renamed := make(NameMap, len(crs))
	for _, cr := range crs {
		if old := cr.GetName(); old != "" {
			renamed[cr.GetKind()+"/"+old] = prefix + old
		}
	}
	return renamed
}

// rewriteAgentClassRefs rewrites every AgentClass ref field listed in
// Rename's doc comment on a single AgentClass CR, in place.
func rewriteAgentClassRefs(cr *unstructured.Unstructured, renamed map[string]string) error {
	if err := rewriteStringField(cr.Object, renamed, "AgentIdentity", "spec", "agentIdentity"); err != nil {
		return err
	}
	if err := rewriteListElementStringField(cr.Object, renamed, "MCPServer", "ref", "spec", "mcpServers"); err != nil {
		return err
	}
	if err := rewriteListElementStringField(cr.Object, renamed, "SidecarToolbox", "ref", "spec", "sidecarToolboxes"); err != nil {
		return err
	}
	if err := rewriteListElementStringField(cr.Object, renamed, "SpiceboxClass", "class", "spec", "toolBundles"); err != nil {
		return err
	}
	if err := rewriteListElementStringField(cr.Object, renamed, "AgentIdentity", "agentIdentity", "spec", "toolBundles"); err != nil {
		return err
	}
	if err := rewriteListElementStringSliceField(cr.Object, renamed, "SpiceboxToolspec", "toolspecs", "spec", "toolBundles"); err != nil {
		return err
	}
	if err := rewriteStringField(cr.Object, renamed, "AgentUI", "spec", "agentUI", "ref"); err != nil {
		return err
	}
	return nil
}

// rewriteSkillSourceRefs rewrites the lone naming cross-reference a bundled
// SkillSource carries — spec.auth.agentIdentity, naming the AgentIdentity whose
// credential clones the skill's repo — at the renamed identity, in place. The
// sibling spec.auth.credential is a credential NAME on that identity, not a CR
// name, so it is deliberately left untouched.
func rewriteSkillSourceRefs(cr *unstructured.Unstructured, renamed map[string]string) error {
	return rewriteStringField(cr.Object, renamed, "AgentIdentity", "spec", "auth", "agentIdentity")
}

// rewriteToolspecRefs rewrites a bundled SpiceboxToolspec's second-level
// spec.toolkit.name reference at the SpiceboxToolkit it names, in place.
func rewriteToolspecRefs(cr *unstructured.Unstructured, renamed map[string]string) error {
	return rewriteStringField(cr.Object, renamed, "SpiceboxToolkit", "spec", "toolkit", "name")
}

// rewriteAgentIdentityRefs rewrites the Secret ref each of a bundled
// AgentIdentity's credentials carries at the Secrets this install
// materializes, in place — so a --name instance points at its own prefixed
// Secret rather than a shared fixed-name one.
//
// WHERE that ref lives is asked of the credential's own registered kind
// (credkind.Kind.SecretRefPath), keyed on the element's `type` — never
// enumerated here. It used to be: two hardcoded paths, "static" and "oauth",
// which meant every githubApp credential in a --name install kept pointing at
// the un-prefixed Secret name. The Secret it named was never created under
// that name, so the AgentIdentity went Valid=False/SecretMissing and no token
// was ever minted. Neither credential-dispatch guard could catch it, because
// the arms were unstructured path strings rather than a .Type switch or
// .Static/.OAuth field selectors.
//
// An unregistered type is an ERROR, not a skip: skipping is exactly the
// silent-dangling-ref failure this replaced, and a bundle carrying a
// credential type this binary does not know cannot be renamed correctly by
// guesswork.
func rewriteAgentIdentityRefs(cr *unstructured.Unstructured, renamed map[string]string) error {
	const field = "spec.credentials"
	creds, found, err := unstructured.NestedSlice(cr.Object, "spec", "credentials")
	if err != nil {
		return fmt.Errorf("field %s: %w", field, err)
	}
	if !found {
		return nil
	}

	changed := false
	for i, item := range creds {
		elem, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("field %s[%d]: element is not an object", field, i)
		}
		credType, _, err := unstructured.NestedString(elem, "type")
		if err != nil {
			return fmt.Errorf("field %s[%d].type: %w", field, i, err)
		}
		k, err := credkindregistry.Get(credType)
		if err != nil {
			return fmt.Errorf("field %s[%d]: %w", field, i, err)
		}
		path := k.SecretRefPath()
		if len(path) == 0 {
			continue // this type has no backing Secret to repoint
		}
		val, ok, err := unstructured.NestedString(elem, path...)
		if err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", field, i, strings.Join(path, "."), err)
		}
		if !ok || val == "" {
			continue
		}
		newName, ok := renamed["Secret/"+val]
		if !ok {
			continue // an EXTERNAL Secret this bundle does not create; leave it alone
		}
		if err := unstructured.SetNestedField(elem, newName, path...); err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", field, i, strings.Join(path, "."), err)
		}
		changed = true
	}
	if changed {
		if err := unstructured.SetNestedSlice(cr.Object, creds, "spec", "credentials"); err != nil {
			return fmt.Errorf("field %s: %w", field, err)
		}
	}
	return nil
}

// rewriteStringField rewrites the top-level string field at fields, in
// place, when present and its value names a bundled CR of kind. Absent is a
// no-op; present-but-not-a-string is a structural-surprise error (surfaced
// by unstructured.NestedString itself).
func rewriteStringField(obj map[string]any, renamed map[string]string, kind string, fields ...string) error {
	val, found, err := unstructured.NestedString(obj, fields...)
	if err != nil {
		return fmt.Errorf("field %s: %w", strings.Join(fields, "."), err)
	}
	if !found || val == "" {
		return nil
	}
	newName, ok := renamed[kind+"/"+val]
	if !ok {
		return nil
	}
	if err := unstructured.SetNestedField(obj, newName, fields...); err != nil {
		return fmt.Errorf("field %s: %w", strings.Join(fields, "."), err)
	}
	return nil
}

// rewriteListElementStringField rewrites the string field elemField inside
// each element of the list at listFields, in place, when present and its
// value names a bundled CR of kind. An absent list, or an absent elemField
// on an element, is a no-op; a present-but-wrong-typed list, element, or
// elemField is a structural-surprise error.
func rewriteListElementStringField(obj map[string]any, renamed map[string]string, kind, elemField string, listFields ...string) error {
	list, found, err := unstructured.NestedSlice(obj, listFields...)
	if err != nil {
		return fmt.Errorf("field %s: %w", strings.Join(listFields, "."), err)
	}
	if !found {
		return nil
	}

	changed := false
	for i, item := range list {
		elem, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("field %s[%d]: element is not an object", strings.Join(listFields, "."), i)
		}
		val, ok, err := unstructured.NestedString(elem, elemField)
		if err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", strings.Join(listFields, "."), i, elemField, err)
		}
		if !ok || val == "" {
			continue
		}
		newName, ok := renamed[kind+"/"+val]
		if !ok {
			continue
		}
		if err := unstructured.SetNestedField(elem, newName, elemField); err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", strings.Join(listFields, "."), i, elemField, err)
		}
		changed = true
	}
	if changed {
		if err := unstructured.SetNestedSlice(obj, list, listFields...); err != nil {
			return fmt.Errorf("field %s: %w", strings.Join(listFields, "."), err)
		}
	}
	return nil
}

// rewriteListElementStringSliceField rewrites each entry of the string-slice
// field elemField inside every element of the list at listFields, in place,
// when an entry names a bundled CR of kind. Same absent/no-op vs.
// present-but-wrong-typed/error rules as rewriteListElementStringField.
func rewriteListElementStringSliceField(obj map[string]any, renamed map[string]string, kind, elemField string, listFields ...string) error {
	list, found, err := unstructured.NestedSlice(obj, listFields...)
	if err != nil {
		return fmt.Errorf("field %s: %w", strings.Join(listFields, "."), err)
	}
	if !found {
		return nil
	}

	changed := false
	for i, item := range list {
		elem, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("field %s[%d]: element is not an object", strings.Join(listFields, "."), i)
		}
		inner, ok, err := unstructured.NestedStringSlice(elem, elemField)
		if err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", strings.Join(listFields, "."), i, elemField, err)
		}
		if !ok {
			continue
		}
		innerChanged := false
		for j, s := range inner {
			if s == "" {
				continue
			}
			if newName, ok := renamed[kind+"/"+s]; ok {
				inner[j] = newName
				innerChanged = true
			}
		}
		if !innerChanged {
			continue
		}
		if err := unstructured.SetNestedStringSlice(elem, inner, elemField); err != nil {
			return fmt.Errorf("field %s[%d].%s: %w", strings.Join(listFields, "."), i, elemField, err)
		}
		changed = true
	}
	if changed {
		if err := unstructured.SetNestedSlice(obj, list, listFields...); err != nil {
			return fmt.Errorf("field %s: %w", strings.Join(listFields, "."), err)
		}
	}
	return nil
}
