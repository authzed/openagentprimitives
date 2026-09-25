package oap

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// DeriveInherited computes the inheritable Agent identity fields and the
// derivable Requires (skills, secrets) from a decoded CR set: the single
// AgentClass and the bundled dependency graph it references. It performs no
// I/O, and returns the SAME result for a given CR set whether that set was
// assembled by a live-cluster walk (source.OpenCluster) or read from a bundled
// manifests stream (FromFolder) — that shared engine is what keeps a
// hand-authored and a cluster-exported bundle of the same graph identical.
//
// Agent.Version is intentionally left empty: it is the package's own release
// version, not an AgentClass fact, so each caller supplies it (the folder from
// the authored oap.yaml, the cluster from resolveVersion's annotation).
func DeriveInherited(crs []*unstructured.Unstructured, logger logr.Logger) (Agent, Requires, error) {
	acU, err := singleAgentClass(crs)
	if err != nil {
		return Agent{}, Requires{}, err
	}

	// AgentClass fields are read via unstructured accessors, NOT a strict typed
	// decode. A typed decode is all-or-nothing: a bundle mid-migration (e.g. the
	// pre-migration bare-string spec.skills shape) fails to decode as a whole,
	// which would abort FromFolder before the dedicated shape lint
	// (checkSkillsShape / LintSkillsShape) could refuse it with its migration
	// hint. Reading leniently here lets the loader succeed and leaves shape
	// diagnosis to the validators built for it — the same tolerance
	// DecodeBundledCR relies on.
	agent := Agent{
		Name:        acU.GetName(),
		DisplayName: apNestedString(acU, "spec", "displayName"),
		Description: apNestedString(acU, "spec", "description"),
	}

	var req Requires
	// A bare-string (old shape) skills entry is skipped here and refused by the
	// skills-shape lint with a migration hint; a well-formed entry is a map.
	if skills, ok, _ := unstructured.NestedSlice(acU.Object, "spec", "skills"); ok {
		for _, s := range skills {
			m, ok := s.(map[string]any)
			if !ok {
				continue
			}
			if ref, ok := m["ref"].(string); ok && ref != "" {
				req.Skills = append(req.Skills, RequiredSkill{Canonical: ref})
			}
		}
	}

	// Index every bundled AgentIdentity by name; the class names which it uses.
	// Each is decoded typed because credkind.SecretRef takes a typed credential;
	// one that does not decode is logged and skipped rather than aborting the
	// whole derivation.
	identities := map[string]*v1alpha1.AgentIdentity{}
	for _, u := range crs {
		if !isAPKind(u, "AgentIdentity") {
			continue
		}
		var ai v1alpha1.AgentIdentity
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ai); err != nil {
			logger.Info("oap: AgentIdentity does not decode; its secrets are not derived",
				"agentIdentity", u.GetName(), "err", err.Error())
			continue
		}
		identities[ai.Name] = &ai
	}

	secrets := newSecretSet()
	// spec.model.apiKey has no owning credential name, so it contributes no purpose.
	if name := apNestedString(acU, "spec", "model", "apiKey", "name"); name != "" {
		secrets.add(name, apNestedString(acU, "spec", "model", "apiKey", "key"), "")
	}
	for _, idName := range referencedIdentityNames(acU) {
		ai := identities[idName]
		if ai == nil {
			// A class that names an identity absent from the bundle is malformed;
			// its secrets go uncollected and the dangling ref surfaces at
			// lint/install. The cluster walk cannot reach here (a missing
			// required identity already failed its fetch).
			logger.Info("oap: AgentClass names an AgentIdentity absent from the CR set; its secrets are not derived",
				"agentClass", acU.GetName(), "agentIdentity", idName)
			continue
		}
		collectIdentitySecrets(logger, ai, secrets)
	}

	// credential name -> "why we need this" prose, for RequiredSecret.Purpose.
	reasons := map[string]string{}
	if ces, ok, _ := unstructured.NestedSlice(acU.Object, "spec", "credentialExplanations"); ok {
		for _, ce := range ces {
			m, ok := ce.(map[string]any)
			if !ok {
				continue
			}
			if cred, ok := m["credential"].(string); ok && cred != "" {
				reason, _ := m["reason"].(string)
				reasons[cred] = reason
			}
		}
	}
	req.Secrets = secrets.toRequired(reasons)

	return agent, req, nil
}

// apNestedString reads a string at the given path, returning "" when the path
// is absent or not a string (never an error — DeriveInherited reads leniently).
func apNestedString(u *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(u.Object, fields...)
	return s
}

// singleAgentClass returns the one AgentClass in crs, erroring on zero or many.
// Shared by Bundle.Validate and FromFolder so the one-class rule has a single
// implementation.
func singleAgentClass(crs []*unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var found *unstructured.Unstructured
	n := 0
	for _, u := range crs {
		if isAPKind(u, "AgentClass") {
			n++
			found = u
		}
	}
	if n != 1 {
		return nil, fmt.Errorf("a .oap must contain exactly one AgentClass; found %d", n)
	}
	return found, nil
}

// isAPKind reports whether u is the given Kind in this project's CRD group.
func isAPKind(u *unstructured.Unstructured, kind string) bool {
	if u.GetKind() != kind {
		return false
	}
	gv, err := schema.ParseGroupVersion(u.GetAPIVersion())
	if err != nil {
		return false
	}
	return gv.Group == v1alpha1.GroupName
}

// referencedIdentityNames is the class-default identity (spec.agentIdentity)
// plus every per-bundle override (spec.toolBundles[].agentIdentity), deduped in
// first-seen order (empty names dropped). Read via unstructured accessors so a
// malformed sibling field never aborts the walk.
func referencedIdentityNames(acU *unstructured.Unstructured) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(n string) {
		if n == "" {
			return
		}
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	add(apNestedString(acU, "spec", "agentIdentity"))
	if tbs, ok, _ := unstructured.NestedSlice(acU.Object, "spec", "toolBundles"); ok {
		for _, tb := range tbs {
			if m, ok := tb.(map[string]any); ok {
				if n, ok := m["agentIdentity"].(string); ok {
					add(n)
				}
			}
		}
	}
	return out
}

// collectIdentitySecrets adds each credential's backing Secret ref to secrets,
// dispatching through the credkind registry rather than switching on the
// credential's populated block (see credkind/guard_test.go Guard 6). A
// credential of an unregistered type is logged and skipped — never silently
// dropped.
func collectIdentitySecrets(logger logr.Logger, ai *v1alpha1.AgentIdentity, secrets *secretSet) {
	for _, cred := range ai.Spec.Credentials {
		k, err := credkindregistry.Get(cred.Type)
		if err != nil {
			logger.Info("oap: credential has an unregistered type; its Secret is omitted from derivation",
				"agentIdentity", ai.Name, "credential", cred.Name, "type", cred.Type, "err", err.Error())
			continue
		}
		if ref := k.SecretRef(cred); ref != nil {
			secrets.add(ref.Name, ref.Key, cred.Name)
		}
		// federated's IdP Secret is excluded from credkind.SecretRef by design
		// (it supplies subject material, not the credential's own store), so it
		// is collected here as its own field — matching source/cluster.go.
		if cred.Federated != nil {
			secrets.add(cred.Federated.IdPSecretRef.Name, "", cred.Name)
		}
	}
}

// secretSet accumulates, per Secret name, the referenced keys and the
// credential names that produced the refs (the latter maps to purpose prose).
type secretSet struct {
	order  []string
	byName map[string]*secretEntry
}

type secretEntry struct {
	keys  map[string]struct{}
	creds map[string]struct{}
}

func newSecretSet() *secretSet { return &secretSet{byName: map[string]*secretEntry{}} }

func (s *secretSet) add(name, key, cred string) {
	if name == "" {
		return
	}
	e := s.byName[name]
	if e == nil {
		e = &secretEntry{keys: map[string]struct{}{}, creds: map[string]struct{}{}}
		s.byName[name] = e
		s.order = append(s.order, name)
	}
	if key != "" {
		e.keys[key] = struct{}{}
	}
	if cred != "" {
		e.creds[cred] = struct{}{}
	}
}

func (s *secretSet) toRequired(reasons map[string]string) []RequiredSecret {
	names := append([]string(nil), s.order...)
	sort.Strings(names)
	var out []RequiredSecret
	for _, n := range names {
		e := s.byName[n]
		out = append(out, RequiredSecret{
			Name:    n,
			Keys:    sortedSet(e.keys),
			Purpose: joinReasons(e.creds, reasons),
		})
	}
	return out
}

func sortedSet(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// joinReasons collects the explanation prose for the credentials that produced
// a Secret, deduped and sorted, joined with "; ". Empty when no credential that
// touched this Secret has an explanation (common with remap-only names, where
// CredentialExplanationSpec.Credential is a post-remap name that need not equal
// the AgentIdentity credential's Name — see the spec's "Secret purpose" note).
func joinReasons(creds map[string]struct{}, reasons map[string]string) string {
	var got []string
	seen := map[string]struct{}{}
	for c := range creds {
		r := reasons[c]
		if r == "" {
			continue
		}
		if _, dup := seen[r]; dup {
			continue
		}
		seen[r] = struct{}{}
		got = append(got, r)
	}
	sort.Strings(got)
	return strings.Join(got, "; ")
}
