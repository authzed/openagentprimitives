package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	digest "github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
)

// NodeInput supplies one edge's private identity and provenance to the same
// preparation pipeline used for a direct install. Root SourceKind/SourceRef
// remain unset: the acquiring surface supplies the original file/registry source.
type NodeInput struct {
	Path          oap.DependencyPath
	Bundle        *oap.Bundle
	PackedDigest  string
	PhysicalName  string
	DirectNames   map[string]string
	ResourceNames instance.NameMap
	SourceKind    string
	SourceRef     string
}

// NodeRunner supplies surface-specific questions and setup around the shared
// Prepare, ApplyPrepared, and RollbackPrepared lifecycle. PrepareNode must make
// no cluster mutations and must retain secret answers only in Prepared.
type NodeRunner interface {
	PrepareNode(context.Context, NodeInput) (*Prepared, error)
	ApplyNode(context.Context, *Prepared) (*Result, error)
	RollbackNode(context.Context, *Prepared) error
}

// preparationDecisionCollector is implemented by an opt-in planning runner
// that can safely continue past operator-decision errors. Fatal errors still
// return false and preserve PlanGraph's fail-fast behavior.
type preparationDecisionCollector interface {
	CollectPreparationDecision(oap.DependencyPath, error) bool
	PreparationDecisionError() error
}

// GraphPlan contains only fully prepared nodes, ordered leaves first.
type GraphPlan struct {
	RootName string
	Nodes    []*PreparedNode
	Notices  []string
	workflow *workflowPlanState
}
type PreparedNode struct {
	Input    NodeInput
	Prepared *Prepared
}
type GraphResult struct {
	RootName string
	Nodes    []NodeResult
	Notices  []string
}
type NodeResult struct {
	Path     oap.DependencyPath
	Name     string
	Result   *Result
	Images   map[string]string
	Channels []channelplan.ChannelPlan
}

// String and GoString deliberately omit Prepared internals and workflow-only
// channel answers, which may contain credentials collected by a wizard.
func (p GraphPlan) String() string {
	return fmt.Sprintf("GraphPlan{root:%q, nodes:%d, notices:%d}", p.RootName, len(p.Nodes), len(p.Notices))
}
func (p GraphPlan) GoString() string { return p.String() }

// MarshalJSON exposes only graph identity and operator-facing notices. The
// executable plan retains bundles, prepared CRs, and secret material in memory
// only; in particular, a bundle may contain a secret Question.Default.
func (p GraphPlan) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		RootName string          `json:"rootName"`
		Nodes    []*PreparedNode `json:"nodes"`
		Notices  []string        `json:"notices,omitempty"`
	}{RootName: p.RootName, Nodes: p.Nodes, Notices: p.Notices})
}

// MarshalJSON prevents direct PreparedNode serialization from walking either
// Input.Bundle or Prepared. Path and physical name are sufficient safe
// metadata for identifying the node in previews and diagnostics.
func (n PreparedNode) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Path oap.DependencyPath `json:"path,omitempty"`
		Name string             `json:"name"`
	}{Path: n.Input.Path, Name: n.Input.PhysicalName})
}

func (n PreparedNode) String() string {
	return fmt.Sprintf("PreparedNode{path:%q, name:%q}", n.Input.Path.String(), n.Input.PhysicalName)
}
func (n PreparedNode) GoString() string { return n.String() }

// PlanGraph validates and prepares every node parents first, then returns a
// leaves-first execution plan. rootName has legacy InstallOpts.Name semantics:
// empty preserves authored names; non-empty prefixes the root's resources.
func PlanGraph(ctx context.Context, root *oap.Bundle, rootName string, runner NodeRunner) (*GraphPlan, error) {
	return planGraphWithIdentity(ctx, root, rootName, runner, nil)
}

type graphIdentityPlan struct {
	ownershipName string
	names         instance.GraphNames
	resources     map[string]instance.NameMap
	answers       map[string]oap.Answers
	completed     map[string]map[string]bool
}

func planGraphWithIdentity(ctx context.Context, root *oap.Bundle, rootName string, runner NodeRunner, identity *graphIdentityPlan) (*GraphPlan, error) {
	if runner == nil {
		return nil, fmt.Errorf("install: missing node runner")
	}
	if err := oap.ValidateDependencyGraph(root); err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	var ownershipName string
	var names instance.GraphNames
	var rootResourceNames instance.NameMap
	var err error
	if identity != nil {
		ownershipName, names = identity.ownershipName, identity.names
		rootResourceNames = identity.resources[""]
	} else {
		ownershipName, names, err = graphNamesForInstall(root, rootName)
		if err != nil {
			return nil, err
		}
		rootResourceNames, err = rootResourceNamesForInstall(root, rootName)
		if err != nil {
			return nil, graphError("plan", ownershipName, nil, err)
		}
	}

	inputs := map[string]NodeInput{}
	// Derive metadata before prompting, so malformed paths and resource maps
	// are rejected without making the user answer questions for an invalid graph.
	err = oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		if err := ctx.Err(); err != nil {
			return graphError("plan", ownershipName, path, err)
		}
		packed, err := oap.Pack(bundle)
		if err != nil {
			return graphError("plan", ownershipName, path, err)
		}
		artifactDigest := digest.FromBytes(packed).String()
		if len(path) == 0 {
			artifactDigest, err = oap.Digest(packed)
			if err != nil {
				return graphError("plan", ownershipName, path, err)
			}
		} else {
			// The descriptor hashes the exact embedded tar bytes. Prefer those
			// bytes' digest over a repack, which may normalize older metadata.
			parent := inputs[oap.DependencyPath(path[:len(path)-1]).String()].Bundle
			for _, dep := range parent.Dependencies {
				if dep.Bundle.Manifest.Agent.Name == path[len(path)-1] {
					if dep.Descriptor.Digest != "" {
						artifactDigest = dep.Descriptor.Digest
					} else if len(dep.Packed) > 0 {
						artifactDigest = digest.FromBytes(dep.Packed).String()
					}
					break
				}
			}
		}
		in := NodeInput{Path: slices.Clone(path), Bundle: bundle, PackedDigest: artifactDigest, PhysicalName: names[path.String()], DirectNames: map[string]string{}}
		for _, dep := range bundle.Dependencies {
			childPath := append(slices.Clone(path), dep.Bundle.Manifest.Agent.Name)
			if len(dep.Path) > 0 && !slices.Equal(dep.Path, childPath) {
				return graphError("plan", ownershipName, childPath, fmt.Errorf("resolved dependency path %q does not match its logical edge", dep.Path.String()))
			}
			in.DirectNames[dep.Bundle.Manifest.Agent.Name] = names[childPath.String()]
		}
		objects, err := resourcesForNames(bundle)
		if err != nil {
			return graphError("plan", ownershipName, path, err)
		}
		if identity != nil {
			in.ResourceNames = maps.Clone(identity.resources[path.String()])
			if len(path) > 0 {
				in.SourceKind, in.SourceRef = "embedded", "agents/"+strings.Join(path, "/")
			}
		} else if len(path) == 0 {
			in.ResourceNames = rootResourceNames
		} else {
			in.SourceKind, in.SourceRef = "embedded", "agents/"+strings.Join(path, "/")
			in.ResourceNames, err = instance.NameMapForDependency(objects, ownershipName, path, artifactDigest, in.PhysicalName)
			if err != nil {
				return graphError("plan", ownershipName, path, err)
			}
		}
		inputs[path.String()] = in
		return nil
	})
	if err != nil {
		return nil, err
	}

	prepared := map[string]*PreparedNode{}
	physicalObjects := map[ObjectKey]oap.DependencyPath{}
	err = oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, _ *oap.Bundle) error {
		in := inputs[path.String()]
		p, err := runner.PrepareNode(ctx, in)
		if err != nil {
			if collector, ok := runner.(preparationDecisionCollector); ok && collector.CollectPreparationDecision(path, err) {
				return nil
			}
			return graphError("prepare", ownershipName, path, err)
		}
		if p == nil {
			return graphError("prepare", ownershipName, path, fmt.Errorf("node runner returned no prepared node"))
		}
		objects := append(slices.Clone(p.CRs), p.secretObjects...)
		for _, obj := range objects {
			if p.skipApply[obj.GetKind()+"/"+obj.GetName()] {
				continue
			}
			key := objectKey(obj)
			if prior, exists := physicalObjects[key]; exists {
				return graphError("prepare", ownershipName, path, fmt.Errorf("resource %s %s/%s also belongs to node %s", key.GVK.Kind, key.Namespace, key.Name, displayGraphPath(ownershipName, prior)))
			}
			physicalObjects[key] = slices.Clone(path)
		}
		prepared[path.String()] = &PreparedNode{Input: in, Prepared: p}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if collector, ok := runner.(preparationDecisionCollector); ok {
		if err := collector.PreparationDecisionError(); err != nil {
			return nil, err
		}
	}
	plan := &GraphPlan{RootName: ownershipName}
	if err := oap.WalkDependencies(root, oap.LeavesFirst, func(path oap.DependencyPath, _ *oap.Bundle) error {
		plan.Nodes = append(plan.Nodes, prepared[path.String()])
		return nil
	}); err != nil {
		return nil, err
	}
	return plan, nil
}

func graphNamesForInstall(root *oap.Bundle, requested string) (string, instance.GraphNames, error) {
	rootCRs, err := root.CRs()
	if err != nil {
		return "", nil, err
	}
	rootClass, err := findAgentClass(rootCRs)
	if err != nil {
		return "", nil, err
	}
	ownershipName := requested
	if ownershipName == "" {
		ownershipName = rootClass.GetName()
	}
	names, err := instance.BuildGraphNames(ownershipName, root)
	if err != nil {
		return "", nil, graphError("plan", ownershipName, nil, err)
	}
	if requested != "" {
		names[""] = requested + "-" + rootClass.GetName()
	}
	return ownershipName, names, nil
}

// rootResourceNamesForInstall is the single source of truth for the physical
// root resource names PlanGraph passes to Prepare. Question probing consumes
// this exact map instead of attempting to recover the --name prefix from an
// AgentClass name, which is ambiguous when the requested name equals or
// contains the authored class name.
func rootResourceNamesForInstall(root *oap.Bundle, requested string) (instance.NameMap, error) {
	objects, err := resourcesForNames(root)
	if err != nil {
		return nil, err
	}
	names := make(instance.NameMap, len(objects))
	for _, obj := range objects {
		physical := obj.GetName()
		if requested != "" {
			physical = requested + "-" + physical
		}
		names[obj.GetKind()+"/"+obj.GetName()] = physical
	}
	return names, nil
}

// resourcesForNames includes generated-resource placeholders only for name
// planning; no values are ever materialized into NodeInput or the bundle.
func resourcesForNames(bundle *oap.Bundle) ([]*unstructured.Unstructured, error) {
	objects, err := bundle.CRs()
	if err != nil {
		return nil, err
	}
	seen := kindNameSet(objects)
	add := func(apiVersion, kind, name string) {
		key := kind + "/" + name
		if name == "" || seen[key] {
			return
		}
		seen[key] = true
		obj := &unstructured.Unstructured{}
		obj.SetAPIVersion(apiVersion)
		obj.SetKind(kind)
		obj.SetName(name)
		objects = append(objects, obj)
	}
	addSecret := func(name string) { add("v1", "Secret", name) }
	for _, q := range bundle.Manifest.Questions {
		if q.Secret != nil && q.Secret.CreateSecret != nil {
			addSecret(q.Secret.CreateSecret.Name)
		}
	}
	for _, req := range bundle.Manifest.Requires.Secrets {
		addSecret(req.Name)
	}
	for _, required := range bundle.Manifest.Requires.Channels {
		add("agentprimitives.authzed.com/v1alpha1", "Channel", required.Name)
		addSecret(required.Name + "-creds")
	}
	return objects, nil
}

// ExecuteGraph applies leaves first. Failure rolls back the partially applied
// node first, then successful nodes in reverse order, retaining every error.
func ExecuteGraph(ctx context.Context, plan *GraphPlan, runner NodeRunner) (*GraphResult, error) {
	if plan == nil || runner == nil {
		return nil, fmt.Errorf("install: missing graph plan or node runner")
	}
	for _, node := range plan.Nodes {
		if node == nil || node.Prepared == nil {
			return nil, fmt.Errorf("install: incomplete graph plan")
		}
	}
	result := &GraphResult{RootName: plan.RootName}
	for i, node := range plan.Nodes {
		res, err := runner.ApplyNode(ctx, node.Prepared)
		if err == nil {
			result.Nodes = append(result.Nodes, NodeResult{Path: slices.Clone(node.Input.Path), Name: node.Input.PhysicalName, Result: res})
			continue
		}
		errs := []error{graphError("apply", plan.RootName, node.Input.Path, redactError(node.Prepared.redactor, err))}
		// Adoption changes survive rollback. Do not discard the only record
		// of earlier nodes' adopted objects when no GraphResult is returned.
		for _, applied := range result.Nodes {
			if applied.Result != nil && len(applied.Result.Adopted) > 0 {
				errs = append(errs, graphError("retained adopted objects", plan.RootName, applied.Path, fmt.Errorf("%v", applied.Result.Adopted)))
			}
		}
		// Cancellation must not prevent cleanup, but cleanup is bounded.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for j := i; j >= 0; j-- {
			prior := plan.Nodes[j]
			if err := runner.RollbackNode(cleanupCtx, prior.Prepared); err != nil {
				qualified := graphError("rollback", plan.RootName, prior.Input.Path, redactError(prior.Prepared.redactor, err))
				errs = append(errs, qualified)
				log.FromContext(ctx).Error(qualified, "graph rollback failed", "root", plan.RootName, "dependencyPath", prior.Input.Path.String())
			}
		}
		return nil, errors.Join(errs...)
	}
	return result, nil
}

func displayGraphPath(root string, path oap.DependencyPath) string {
	return append(oap.DependencyPath{root}, path...).String()
}

type dependencyPathError struct {
	operation string
	root      string
	path      oap.DependencyPath
	err       error
}

func (e *dependencyPathError) Error() string {
	return fmt.Sprintf("install %s: %s: %v", displayGraphPath(e.root, e.path), e.operation, e.err)
}

func (e *dependencyPathError) Unwrap() error { return e.err }

// DependencyPathFromError returns the logical node path attached by graph
// planning/execution. Surfaces use it to address forms and conflicts without
// parsing error text or traversing the graph independently.
func DependencyPathFromError(err error) (oap.DependencyPath, bool) {
	var pathErr *dependencyPathError
	if !errors.As(err, &pathErr) {
		return nil, false
	}
	return slices.Clone(pathErr.path), true
}

func graphError(operation, root string, path oap.DependencyPath, err error) error {
	return &dependencyPathError{operation: operation, root: root, path: slices.Clone(path), err: err}
}
