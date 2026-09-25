package oap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

const (
	maxDependencyDepth          = 8
	maxDependencyArtifacts      = 64
	maxDependencyPackedBytes    = 512 << 20
	maxDependencyExtractedBytes = 1 << 30
)

// DependencyPath is the logical route from the root bundle to a private
// dependency. The root itself has an empty path.
type DependencyPath []string

func (p DependencyPath) String() string { return strings.Join(p, " > ") }

// Dependency is one resolved private child edge. Equal Bundle content on two
// edges remains two dependencies; callers must not globally deduplicate it.
type Dependency struct {
	Descriptor RequiredAgent
	Path       DependencyPath
	Bundle     *Bundle
	Packed     []byte
}

// Resolver resolves and verifies a dependency descriptor for its parent.
type Resolver interface {
	Resolve(context.Context, *Bundle, RequiredAgent) (*Bundle, []byte, error)
}

// WalkOrder controls whether a graph visitor sees a parent before or after its
// direct and transitive dependencies.
type WalkOrder int

const (
	ParentsFirst WalkOrder = iota
	LeavesFirst
)

// NormalizeDependencyName maps a logical agent name to the shared Kubernetes
// name component used for collision checks and private instance naming.
func NormalizeDependencyName(name string) string {
	var out strings.Builder
	separator := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			if separator && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteByte(c)
			separator = false
			continue
		}
		separator = true
	}
	return out.String()
}

// WalkDependencies visits the root and all of its descendants in deterministic
// declaration order. It rejects a cyclic in-memory graph instead of recursing
// forever; packed-artifact digest cycle checks are added at resolution time.
func WalkDependencies(root *Bundle, order WalkOrder, visit func(DependencyPath, *Bundle) error) error {
	if order != ParentsFirst && order != LeavesFirst {
		return fmt.Errorf("unknown dependency walk order %d", order)
	}
	if root == nil {
		return fmt.Errorf("walk dependencies: nil root bundle")
	}
	active := map[*Bundle]bool{}
	var walk func(DependencyPath, *Bundle) error
	walk = func(path DependencyPath, node *Bundle) error {
		if node == nil {
			return fmt.Errorf("dependency %s: nil bundle", path.String())
		}
		if active[node] {
			return fmt.Errorf("dependency graph cycle at %s", graphDisplayPath(root, path))
		}
		active[node] = true
		defer delete(active, node)
		if order == ParentsFirst {
			if err := visit(cloneDependencyPath(path), node); err != nil {
				return err
			}
		}
		for _, dep := range node.Dependencies {
			if dep == nil || dep.Bundle == nil {
				return fmt.Errorf("dependency %s: unresolved child", graphDisplayPath(root, path))
			}
			childPath := dep.Path
			if len(childPath) == 0 {
				childPath = appendDependencyPath(path, dep.Bundle.Manifest.Agent.Name)
			}
			if err := walk(childPath, dep.Bundle); err != nil {
				return err
			}
		}
		if order == LeavesFirst {
			if err := visit(cloneDependencyPath(path), node); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(nil, root)
}

// ValidateDependencyGraph validates every bundle independently and then checks
// parent/child composition, logical-name collisions, cycles, and graph limits.
func ValidateDependencyGraph(root *Bundle) error {
	if root == nil {
		return fmt.Errorf("validate dependency graph: nil root bundle")
	}
	budget := &graphBudget{}
	active := map[*Bundle]bool{}
	var validate func(DependencyPath, *Bundle) error
	validate = func(path DependencyPath, node *Bundle) error {
		if node == nil {
			return fmt.Errorf("dependency %s: nil bundle", graphDisplayPath(root, path))
		}
		if active[node] {
			return fmt.Errorf("dependency graph cycle at %s", graphDisplayPath(root, path))
		}
		if err := budget.enter(path); err != nil {
			return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, path), err)
		}
		if err := budget.addExtracted(bundleExtractedBytes(node)); err != nil {
			return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, path), err)
		}
		if err := node.Validate(); err != nil {
			return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, path), err)
		}
		if len(node.Manifest.Requires.Agents) != len(node.Dependencies) {
			return fmt.Errorf("dependency %s: manifest declares %d agents but %d embedded dependencies were resolved",
				graphDisplayPath(root, path), len(node.Manifest.Requires.Agents), len(node.Dependencies))
		}

		roster, err := dependencyRoster(node)
		if err != nil {
			return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, path), err)
		}
		active[node] = true
		defer delete(active, node)
		names := make(map[string]string, len(node.Dependencies))
		normalized := make(map[string]string, len(node.Dependencies))
		for i, dep := range node.Dependencies {
			if dep == nil || dep.Bundle == nil || dep.Bundle.Manifest == nil {
				return fmt.Errorf("dependency %s: requires.agents[%d] is unresolved", graphDisplayPath(root, path), i)
			}
			childName := dep.Bundle.Manifest.Agent.Name
			childPath := appendDependencyPath(path, childName)
			if prior, exists := names[childName]; exists {
				return fmt.Errorf("dependency %s: duplicate sibling dependency name %q (also %s)",
					graphDisplayPath(root, childPath), childName, prior)
			}
			names[childName] = graphDisplayPath(root, childPath)
			normalName := NormalizeDependencyName(childName)
			if prior, exists := normalized[normalName]; exists {
				return fmt.Errorf("dependency %s: sibling names %q and %q normalize to the same Kubernetes name %q",
					graphDisplayPath(root, path), prior, childName, normalName)
			}
			normalized[normalName] = childName
			if !roster[childName] {
				return fmt.Errorf("dependency %s: embedded agent %q is absent from its parent's spec.subagents roster",
					graphDisplayPath(root, childPath), childName)
			}
			declared := node.Manifest.Requires.Agents[i]
			if err := validateDependencyEdge(declared, dep, childName); err != nil {
				return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, childPath), err)
			}
			if err := budget.addPacked(int64(len(dep.Packed))); err != nil {
				return fmt.Errorf("dependency %s: %w", graphDisplayPath(root, childPath), err)
			}
			if err := validate(childPath, dep.Bundle); err != nil {
				return err
			}
		}
		return nil
	}
	return validate(nil, root)
}

func validateDependencyEdge(declared RequiredAgent, dep *Dependency, childName string) error {
	if declared.Path != "" {
		if dep.Descriptor.Path != declared.Path {
			return fmt.Errorf("resolved dependency path %q does not match declaration %q", dep.Descriptor.Path, declared.Path)
		}
		return nil
	}
	if declared.Name != childName {
		return fmt.Errorf("descriptor name %q does not match embedded agent %q", declared.Name, childName)
	}
	if declared.Version != dep.Bundle.Manifest.Agent.Version {
		return fmt.Errorf("descriptor version %q does not match embedded agent version %q", declared.Version, dep.Bundle.Manifest.Agent.Version)
	}
	return nil
}

func dependencyRoster(bundle *Bundle) (map[string]bool, error) {
	crs, err := bundle.CRs()
	if err != nil {
		return nil, fmt.Errorf("decode bundled manifests: %w", err)
	}
	class, err := singleAgentClass(crs)
	if err != nil {
		return nil, err
	}
	entries, _, err := unstructured.NestedStringSlice(class.Object, "spec", "subagents")
	if err != nil {
		return nil, fmt.Errorf("AgentClass %q spec.subagents: %w", class.GetName(), err)
	}
	roster := make(map[string]bool, len(entries))
	for _, entry := range entries {
		name, _, _ := v1alpha1.SplitRosterEntry(entry)
		roster[name] = true
	}
	return roster, nil
}

type graphBudget struct {
	artifacts      int
	packedBytes    int64
	extractedBytes int64
	rootName       string
}

func (b *graphBudget) enter(path DependencyPath) error {
	if len(path) > maxDependencyDepth {
		return fmt.Errorf("maximum depth %d exceeded", maxDependencyDepth)
	}
	b.artifacts++
	if b.artifacts > maxDependencyArtifacts {
		return fmt.Errorf("maximum artifact count %d exceeded", maxDependencyArtifacts)
	}
	return nil
}

func (b *graphBudget) addPacked(n int64) error {
	b.packedBytes += n
	if b.packedBytes > maxDependencyPackedBytes {
		return fmt.Errorf("maximum aggregate packed bytes %d exceeded", maxDependencyPackedBytes)
	}
	return nil
}

func (b *graphBudget) addExtracted(n int64) error {
	b.extractedBytes += n
	if b.extractedBytes > maxDependencyExtractedBytes {
		return fmt.Errorf("maximum aggregate extracted bytes %d exceeded", maxDependencyExtractedBytes)
	}
	return nil
}

func bundleExtractedBytes(bundle *Bundle) int64 {
	total := int64(len(bundle.Manifests) + len(bundle.Readme))
	for _, data := range bundle.Assets {
		total += int64(len(data))
	}
	return total
}

type dependencyPathError struct{ err error }

func (e *dependencyPathError) Error() string { return e.err.Error() }
func (e *dependencyPathError) Unwrap() error { return e.err }

func qualifyDependencyError(rootName string, path DependencyPath, err error) error {
	var qualified *dependencyPathError
	if errors.As(err, &qualified) {
		return err
	}
	parts := make(DependencyPath, 0, len(path)+1)
	if rootName != "" {
		parts = append(parts, rootName)
	}
	parts = append(parts, path...)
	return &dependencyPathError{err: fmt.Errorf("dependency %s: %w", parts.String(), err)}
}

func graphDisplayPath(root *Bundle, path DependencyPath) string {
	rootName := "<root>"
	if root != nil && root.Manifest != nil && root.Manifest.Agent.Name != "" {
		rootName = root.Manifest.Agent.Name
	}
	return appendDependencyPath(DependencyPath{rootName}, path...).String()
}

func appendDependencyPath(path DependencyPath, names ...string) DependencyPath {
	out := make(DependencyPath, 0, len(path)+len(names))
	out = append(out, path...)
	out = append(out, names...)
	return out
}

func cloneDependencyPath(path DependencyPath) DependencyPath {
	return append(DependencyPath(nil), path...)
}
