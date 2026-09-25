package instance

import (
	"crypto/sha256"
	"fmt"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

const digestSuffixLength = 10

// GraphNames maps a logical dependency path to its physical AgentClass name.
// The empty path names the root install.
type GraphNames map[string]string

// NameMap maps a bundle-local Kind/name key to its physical resource name.
type NameMap map[string]string

// DependencyName derives a dependency's deterministic physical AgentClass
// name from the root install name and its logical path.
func DependencyName(root string, path oap.DependencyPath, artifactDigest string) (string, error) {
	return dependencyName(root, path, artifactDigest, false)
}

func dependencyName(root string, path oap.DependencyPath, artifactDigest string, forceDigest bool) (string, error) {
	parts, err := dependencyNameParts(root, path)
	if err != nil {
		return "", err
	}
	suffix, err := edgeCollisionToken(artifactDigest, path)
	if err != nil {
		return "", err
	}
	return finalName(strings.Join(parts, "-"), suffix, forceDigest)
}

func dependencyNameParts(root string, path oap.DependencyPath) ([]string, error) {
	if errs := validation.IsDNS1123Subdomain(root); len(errs) > 0 {
		return nil, fmt.Errorf("dependency name root %q is not a DNS subdomain: %s", root, strings.Join(errs, "; "))
	}
	if len(path) == 0 {
		return nil, fmt.Errorf("dependency name: empty dependency path")
	}
	parts := make([]string, 1, len(path)+1)
	parts[0] = root
	for i, component := range path {
		normalized := oap.NormalizeDependencyName(component)
		if normalized == "" {
			return nil, fmt.Errorf("dependency path component %d %q normalizes to empty", i, component)
		}
		parts = append(parts, normalized)
	}
	return parts, nil
}

func edgeCollisionToken(artifactDigest string, path oap.DependencyPath) (string, error) {
	_, err := digest.Parse(artifactDigest)
	if err != nil {
		return "", fmt.Errorf("dependency artifact digest %q: %w", artifactDigest, err)
	}
	sum := sha256.Sum256([]byte(artifactDigest + "\x00" + path.String()))
	return fmt.Sprintf("%x", sum)[:digestSuffixLength], nil
}

func finalName(base, suffix string, forceDigest bool) (string, error) {
	name := base
	if forceDigest || len(name) > validation.DNS1123SubdomainMaxLength {
		prefixLength := validation.DNS1123SubdomainMaxLength - 1 - len(suffix)
		if len(name) > prefixLength {
			name = name[:prefixLength]
		}
		name = strings.TrimRight(name, "-.") + "-" + suffix
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return "", fmt.Errorf("derived dependency name %q is not a DNS subdomain: %s", name, strings.Join(errs, "; "))
	}
	return name, nil
}

// BuildGraphNames derives names for the root and every private dependency.
// When flattened logical paths collide, every colliding dependency receives
// its artifact digest suffix.
func BuildGraphNames(root string, b *oap.Bundle) (GraphNames, error) {
	if errs := validation.IsDNS1123Subdomain(root); len(errs) > 0 {
		return nil, fmt.Errorf("graph name root %q is not a DNS subdomain: %s", root, strings.Join(errs, "; "))
	}
	if b == nil {
		return nil, fmt.Errorf("build graph names: nil root bundle")
	}

	type nodeName struct {
		path   oap.DependencyPath
		digest string
		name   string
	}
	var nodes []nodeName
	active := map[*oap.Bundle]bool{}
	var walk func(oap.DependencyPath, *oap.Bundle) error
	walk = func(parentPath oap.DependencyPath, node *oap.Bundle) error {
		if node == nil {
			return fmt.Errorf("dependency %s: nil bundle", parentPath.String())
		}
		if active[node] {
			return fmt.Errorf("dependency graph cycle at %s", parentPath.String())
		}
		active[node] = true
		defer delete(active, node)
		for _, dep := range node.Dependencies {
			if dep == nil || dep.Bundle == nil || dep.Bundle.Manifest == nil {
				return fmt.Errorf("dependency %s: unresolved child", parentPath.String())
			}
			path := dep.Path
			if len(path) == 0 {
				path = append(append(oap.DependencyPath(nil), parentPath...), dep.Bundle.Manifest.Agent.Name)
			}
			artifactDigest, err := dependencyArtifactDigest(dep)
			if err != nil {
				return fmt.Errorf("dependency %s: %w", path.String(), err)
			}
			name, err := DependencyName(root, path, artifactDigest)
			if err != nil {
				return fmt.Errorf("dependency %s: %w", path.String(), err)
			}
			nodes = append(nodes, nodeName{path: append(oap.DependencyPath(nil), path...), digest: artifactDigest, name: name})
			if err := walk(path, dep.Bundle); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(nil, b); err != nil {
		return nil, err
	}

	counts := make(map[string]int, len(nodes))
	for _, node := range nodes {
		counts[node.name]++
	}
	names := GraphNames{"": root}
	physicalPaths := make(map[string]string, len(nodes))
	for _, node := range nodes {
		name := node.name
		if counts[name] > 1 {
			var err error
			name, err = dependencyName(root, node.path, node.digest, true)
			if err != nil {
				return nil, fmt.Errorf("dependency %s: %w", node.path.String(), err)
			}
		}
		pathKey := node.path.String()
		if _, exists := names[pathKey]; exists {
			return nil, fmt.Errorf("duplicate dependency path %q", pathKey)
		}
		if prior, exists := physicalPaths[name]; exists {
			return nil, fmt.Errorf("dependency paths %q and %q derive the same physical name %q", prior, pathKey, name)
		}
		names[pathKey] = name
		physicalPaths[name] = pathKey
	}
	return names, nil
}

func dependencyArtifactDigest(dep *oap.Dependency) (string, error) {
	if dep.Descriptor.Digest != "" {
		if _, err := digest.Parse(dep.Descriptor.Digest); err != nil {
			return "", fmt.Errorf("invalid descriptor digest %q: %w", dep.Descriptor.Digest, err)
		}
		return dep.Descriptor.Digest, nil
	}
	if len(dep.Packed) > 0 {
		return digest.FromBytes(dep.Packed).String(), nil
	}
	packed, err := oap.Pack(dep.Bundle)
	if err != nil {
		return "", fmt.Errorf("pack dependency to derive digest: %w", err)
	}
	return digest.FromBytes(packed).String(), nil
}

// NameMapForDependency derives physical names for every local resource in one
// dependency artifact. The graph-resolved AgentClass name is used verbatim;
// every other resource uses that private AgentClass name as its prefix and
// the same per-edge collision token as DependencyName.
func NameMapForDependency(crs []*unstructured.Unstructured, root string, path oap.DependencyPath, artifactDigest, physicalAgentClassName string) (NameMap, error) {
	_, err := dependencyNameParts(root, path)
	if err != nil {
		return nil, err
	}
	if errs := validation.IsDNS1123Subdomain(physicalAgentClassName); len(errs) > 0 {
		return nil, fmt.Errorf("physical AgentClass name %q is not a DNS subdomain: %s", physicalAgentClassName, strings.Join(errs, "; "))
	}
	suffix, err := edgeCollisionToken(artifactDigest, path)
	if err != nil {
		return nil, err
	}
	names := make(NameMap, len(crs))
	physical := make(map[string]string, len(crs))
	for i, cr := range crs {
		if cr == nil {
			return nil, fmt.Errorf("dependency resource %d is nil", i)
		}
		old := cr.GetName()
		if old == "" {
			return nil, fmt.Errorf("dependency resource %s at index %d has no metadata.name", cr.GetKind(), i)
		}
		key := cr.GetKind() + "/" + old
		if _, exists := names[key]; exists {
			return nil, fmt.Errorf("duplicate dependency resource %s", key)
		}
		name := physicalAgentClassName
		if cr.GetKind() != "AgentClass" {
			name, err = finalName(physicalAgentClassName+"-"+old, suffix, false)
			if err != nil {
				return nil, fmt.Errorf("dependency resource %s: %w", key, err)
			}
		}
		physicalKey := cr.GetKind() + "/" + name
		if prior, exists := physical[physicalKey]; exists {
			return nil, fmt.Errorf("dependency resources %s and %s derive the same physical name %q", prior, key, name)
		}
		names[key] = name
		physical[physicalKey] = key
	}
	return names, nil
}

// RewriteSubagentRefs rewrites only roster members named in direct, preserving
// external members and any digest pin carried by a rewritten roster entry.
// Matching subagentModes keys move with the roster member.
func RewriteSubagentRefs(parent *unstructured.Unstructured, direct map[string]string) error {
	if parent == nil {
		return fmt.Errorf("rewrite subagent refs: nil parent")
	}
	for logical, physical := range direct {
		if logical == "" {
			return fmt.Errorf("rewrite subagent refs: empty logical agent name")
		}
		if errs := validation.IsDNS1123Subdomain(physical); len(errs) > 0 {
			return fmt.Errorf("rewrite subagent refs: physical name %q is not a DNS subdomain: %s", physical, strings.Join(errs, "; "))
		}
	}

	roster, rosterFound, err := unstructured.NestedStringSlice(parent.Object, "spec", "subagents")
	if err != nil {
		return fmt.Errorf("field spec.subagents: %w", err)
	}
	rewrittenRoster := append([]string(nil), roster...)
	for i, entry := range rewrittenRoster {
		name, pin, pinned := v1alpha1.SplitRosterEntry(entry)
		physical, ok := direct[name]
		if !ok {
			continue
		}
		rewrittenRoster[i] = physical
		if pinned {
			rewrittenRoster[i] += "@" + pin
		}
	}

	modes, modesFound, err := unstructured.NestedMap(parent.Object, "spec", "subagentModes")
	if err != nil {
		return fmt.Errorf("field spec.subagentModes: %w", err)
	}
	for key, value := range modes {
		values, ok := value.([]any)
		if !ok {
			return fmt.Errorf("field spec.subagentModes.%s is not a string slice", key)
		}
		for i, item := range values {
			if _, ok := item.(string); !ok {
				return fmt.Errorf("field spec.subagentModes.%s[%d] is not a string", key, i)
			}
		}
	}
	for logical, physical := range direct {
		value, exists := modes[logical]
		if !exists || logical == physical {
			continue
		}
		if _, collision := modes[physical]; collision {
			return fmt.Errorf("field spec.subagentModes: rewriting %q to %q would overwrite an existing key", logical, physical)
		}
		delete(modes, logical)
		modes[physical] = value
	}

	if rosterFound {
		if err := unstructured.SetNestedStringSlice(parent.Object, rewrittenRoster, "spec", "subagents"); err != nil {
			return fmt.Errorf("field spec.subagents: %w", err)
		}
	}
	if modesFound {
		if err := unstructured.SetNestedMap(parent.Object, modes, "spec", "subagentModes"); err != nil {
			return fmt.Errorf("field spec.subagentModes: %w", err)
		}
	}
	return nil
}
