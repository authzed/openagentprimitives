package install

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	digest "github.com/opencontainers/go-digest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// GraphValues is the external values shape for one complete install graph.
// Root question keys remain at the top level; each dependency is nested under
// agents.<logical-name>, recursively.
type GraphValues map[string]any

// QuestionGroup is one node's local authored and synthesized questions. Path
// supplies external addressing; Questions retain local names for oap.Apply and
// Secret creation.
type QuestionGroup struct {
	Path      oap.DependencyPath
	Agent     oap.Agent
	Questions []oap.Question
}

// LoadGraphValues reads a nested graph values YAML file. An empty path is the
// same optional empty input accepted by Resolve.
func LoadGraphValues(path string) (GraphValues, error) {
	values, err := loadValues(path)
	if err != nil {
		return nil, err
	}
	return GraphValues(values), nil
}

// ProjectValues validates the complete nested agents tree, then returns only
// path's local question values. Validation happens first so a caller cannot
// resolve or prompt an early node before discovering a malformed later one.
func ProjectValues(values GraphValues, path oap.DependencyPath, root *oap.Bundle) (map[string]any, error) {
	if err := oap.ValidateDependencyGraph(root); err != nil {
		return nil, fmt.Errorf("graph values: %w", err)
	}
	if values == nil {
		values = GraphValues{}
	}
	if err := validateGraphValuesNode(root, map[string]any(values), nil, root.Manifest.Agent.Name); err != nil {
		return nil, err
	}
	if _, err := graphBundleAt(root, path); err != nil {
		return nil, err
	}
	return projectGraphValues(values, path)
}

func projectGraphValues(values GraphValues, path oap.DependencyPath) (map[string]any, error) {
	current := map[string]any(values)
	for _, component := range path {
		rawAgents, ok := current["agents"]
		if !ok {
			return map[string]any{}, nil
		}
		agents, _ := graphValueMap(rawAgents) // the complete-tree validation checked this shape.
		rawChild, ok := agents[component]
		if !ok {
			return map[string]any{}, nil
		}
		current, _ = graphValueMap(rawChild)
	}
	projected := make(map[string]any, len(current))
	for key, value := range current {
		if key == "agents" {
			if _, dependencyTree := graphValueMap(value); dependencyTree {
				continue
			}
		}
		projected[key] = value
	}
	return projected, nil
}

// ParseGraphSet translates externally qualified --set/form keys into local
// maps keyed by DependencyPath.String(). Dots in a local question name remain
// part of that name; only explicit agents.<child> segments traverse the graph.
func ParseGraphSet(raw map[string]string, root *oap.Bundle) (map[string]map[string]string, error) {
	if err := oap.ValidateDependencyGraph(root); err != nil {
		return nil, fmt.Errorf("graph set: %w", err)
	}
	parsed := map[string]map[string]string{}
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, external := range keys {
		path, local, bundle, err := parseGraphSetKey(root, external)
		if err != nil {
			return nil, err
		}
		if err := validateLocalQuestionKey(bundle, path, local); err != nil {
			return nil, fmt.Errorf("graph set %q: %w", external, err)
		}
		pathKey := path.String()
		if parsed[pathKey] == nil {
			parsed[pathKey] = map[string]string{}
		}
		parsed[pathKey][local] = raw[external]
	}
	return parsed, nil
}

// ExpandQuestionGroups returns every node's questions in parent-first order.
// Required Secret probes use graph-planned physical Secret names, while the
// returned questions retain bundle-local createSecret targets.
func ExpandQuestionGroups(ctx context.Context, c client.Client, root *oap.Bundle, namespace string, names instance.GraphNames, rootResourceNames instance.NameMap) ([]QuestionGroup, []string, error) {
	return expandQuestionGroupsMapped(ctx, c, root, namespace, names, map[string]instance.NameMap{"": rootResourceNames})
}

func expandQuestionGroupsMapped(ctx context.Context, c client.Client, root *oap.Bundle, namespace string, names instance.GraphNames, resourceMaps map[string]instance.NameMap) ([]QuestionGroup, []string, error) {
	if err := oap.ValidateDependencyGraph(root); err != nil {
		return nil, nil, fmt.Errorf("expand graph questions: %w", err)
	}
	rootName := names[""]
	if rootName == "" {
		return nil, nil, fmt.Errorf("expand graph questions: graph names omit the root")
	}
	var groups []QuestionGroup
	var notices []string
	err := oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		if err := ctx.Err(); err != nil {
			return graphError("questions", rootName, path, err)
		}
		if names[path.String()] == "" {
			return graphError("questions", rootName, path, fmt.Errorf("no physical name was planned"))
		}
		resourceNames := resourceMaps[path.String()]
		if len(resourceNames) == 0 {
			var err error
			resourceNames, err = graphQuestionResourceNames(root, bundle, path, names, resourceMaps[""])
			if err != nil {
				return graphError("questions", rootName, path, err)
			}
		}
		secretQuestions, nodeNotices, err := requiredSecretQuestionsMapped(ctx, c, bundle.Manifest, namespace, resourceNames)
		if err != nil {
			return graphError("questions", rootName, path, err)
		}
		questions := append(slices.Clone(bundle.Manifest.Questions), secretQuestions...)
		groups = append(groups, QuestionGroup{Path: slices.Clone(path), Agent: bundle.Manifest.Agent, Questions: questions})
		for _, notice := range nodeNotices {
			notices = append(notices, fmt.Sprintf("install %s: %s", displayGraphPath(rootName, path), notice))
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return groups, notices, nil
}

func validateGraphValuesNode(bundle *oap.Bundle, values map[string]any, path oap.DependencyPath, rootName string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "agents" {
			continue
		}
		if err := validateLocalQuestionKey(bundle, path, key); err != nil {
			return fmt.Errorf("graph values at %s: unknown question %q", displayGraphPath(rootName, path), key)
		}
	}
	rawAgents, ok := values["agents"]
	if !ok {
		return nil
	}
	agents, ok := graphValueMap(rawAgents)
	if !ok {
		if err := validateLocalQuestionKey(bundle, path, "agents"); err == nil {
			return nil
		}
		return fmt.Errorf("graph values at %s: agents must be a map, got %T", displayGraphPath(rootName, path), rawAgents)
	}
	childNames := make([]string, 0, len(agents))
	for childName := range agents {
		childNames = append(childNames, childName)
	}
	sort.Strings(childNames)
	for _, childName := range childNames {
		rawChild := agents[childName]
		child, ok := directGraphChild(bundle, childName)
		if !ok {
			return fmt.Errorf("graph values at %s: unknown child agent %q", displayGraphPath(rootName, path), childName)
		}
		childValues, ok := graphValueMap(rawChild)
		if !ok {
			return fmt.Errorf("graph values at %s: agent values must be a map, got %T", displayGraphPath(rootName, append(slices.Clone(path), childName)), rawChild)
		}
		childPath := append(slices.Clone(path), childName)
		if err := validateGraphValuesNode(child, childValues, childPath, rootName); err != nil {
			return err
		}
	}
	return nil
}

func validateLocalQuestionKey(bundle *oap.Bundle, path oap.DependencyPath, key string) error {
	for _, q := range bundle.Manifest.Questions {
		if q.Name == key {
			return nil
		}
	}
	if strings.HasPrefix(key, oap.ReservedQuestionPrefix) {
		return nil
	}
	for _, required := range bundle.Manifest.Requires.Secrets {
		if required.Question != "" {
			continue
		}
		for _, secretKey := range required.Keys {
			if oap.RequiredSecretQuestionName(required.Name, secretKey) == key {
				return nil
			}
		}
	}
	return fmt.Errorf("unknown question %q", key)
}

func graphValueMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case GraphValues:
		return map[string]any(typed), true
	default:
		return nil, false
	}
}

func parseGraphSetKey(root *oap.Bundle, external string) (oap.DependencyPath, string, *oap.Bundle, error) {
	if external == "" {
		return nil, "", nil, fmt.Errorf("graph set: empty answer key")
	}
	// Root question keys predate graph addressing. An exact authored key wins
	// even when it begins with "agents.", preserving the legacy meaning of dots
	// inside ordinary root question names.
	for _, question := range root.Manifest.Questions {
		if question.Name == external {
			return nil, external, root, nil
		}
	}
	if !strings.HasPrefix(external, "agents.") {
		return nil, external, root, nil
	}
	rest := strings.TrimPrefix(external, "agents.")
	path := oap.DependencyPath{}
	bundle := root
	for {
		childName, child, remaining, ok := matchGraphChild(bundle, rest)
		if !ok {
			return nil, "", nil, fmt.Errorf("graph set %q: unknown child agent beneath %s", external, displayGraphPath(root.Manifest.Agent.Name, path))
		}
		path = append(path, childName)
		bundle = child
		if remaining == "" {
			return nil, "", nil, fmt.Errorf("graph set %q: dependency path has no question name", external)
		}
		// As at the root, an exact authored key wins over the traversal
		// sentinel. Dots are legal inside a question name, including a legacy
		// name beginning with "agents.".
		for _, question := range bundle.Manifest.Questions {
			if question.Name == remaining {
				return path, remaining, bundle, nil
			}
		}
		if strings.HasPrefix(remaining, "agents.") {
			rest = strings.TrimPrefix(remaining, "agents.")
			continue
		}
		return path, remaining, bundle, nil
	}
}

func matchGraphChild(bundle *oap.Bundle, rest string) (string, *oap.Bundle, string, bool) {
	type candidate struct {
		name   string
		bundle *oap.Bundle
	}
	var candidates []candidate
	for _, dependency := range bundle.Dependencies {
		if dependency == nil || dependency.Bundle == nil || dependency.Bundle.Manifest == nil {
			continue
		}
		name := dependency.Bundle.Manifest.Agent.Name
		if rest == name || strings.HasPrefix(rest, name+".") {
			candidates = append(candidates, candidate{name: name, bundle: dependency.Bundle})
		}
	}
	if len(candidates) == 0 {
		return "", nil, "", false
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i].name) > len(candidates[j].name) })
	selected := candidates[0]
	remaining := strings.TrimPrefix(rest, selected.name)
	remaining = strings.TrimPrefix(remaining, ".")
	return selected.name, selected.bundle, remaining, true
}

func directGraphChild(bundle *oap.Bundle, name string) (*oap.Bundle, bool) {
	for _, dependency := range bundle.Dependencies {
		if dependency != nil && dependency.Bundle != nil && dependency.Bundle.Manifest != nil && dependency.Bundle.Manifest.Agent.Name == name {
			return dependency.Bundle, true
		}
	}
	return nil, false
}

func graphBundleAt(root *oap.Bundle, path oap.DependencyPath) (*oap.Bundle, error) {
	bundle := root
	for _, component := range path {
		child, ok := directGraphChild(bundle, component)
		if !ok {
			return nil, fmt.Errorf("graph values: unknown dependency path %q", path.String())
		}
		bundle = child
	}
	return bundle, nil
}

func graphQuestionResourceNames(root, bundle *oap.Bundle, path oap.DependencyPath, names instance.GraphNames, rootResourceNames instance.NameMap) (instance.NameMap, error) {
	if len(path) == 0 {
		if len(rootResourceNames) == 0 {
			return nil, fmt.Errorf("root resource names were not planned")
		}
		return rootResourceNames, nil
	}
	objects, err := resourcesForNames(bundle)
	if err != nil {
		return nil, err
	}
	physicalAgent := names[path.String()]
	dependency, err := graphDependencyAt(root, path)
	if err != nil {
		return nil, err
	}
	artifactDigest := dependency.Descriptor.Digest
	if artifactDigest == "" && len(dependency.Packed) > 0 {
		artifactDigest = digest.FromBytes(dependency.Packed).String()
	}
	if artifactDigest == "" {
		packed, err := oap.Pack(dependency.Bundle)
		if err != nil {
			return nil, fmt.Errorf("pack dependency %s: %w", path.String(), err)
		}
		artifactDigest = digest.FromBytes(packed).String()
	}
	return instance.NameMapForDependency(objects, names[""], path, artifactDigest, physicalAgent)
}

func graphDependencyAt(root *oap.Bundle, path oap.DependencyPath) (*oap.Dependency, error) {
	if len(path) == 0 {
		return nil, fmt.Errorf("root has no incoming dependency edge")
	}
	parent, err := graphBundleAt(root, path[:len(path)-1])
	if err != nil {
		return nil, err
	}
	for _, dependency := range parent.Dependencies {
		if dependency != nil && dependency.Bundle != nil && dependency.Bundle.Manifest != nil && dependency.Bundle.Manifest.Agent.Name == path[len(path)-1] {
			return dependency, nil
		}
	}
	return nil, fmt.Errorf("dependency path %q is unresolved", path.String())
}
