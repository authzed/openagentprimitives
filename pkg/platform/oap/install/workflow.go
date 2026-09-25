package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	godigest "github.com/opencontainers/go-digest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/instance"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// WorkflowHooks are the environment-specific, per-node operations surrounding
// the shared graph lifecycle. Every planning hook is called before Execute can
// perform the first cluster mutation.
type WorkflowHooks struct {
	InspectNode             func(context.Context, NodeContext) error
	ResolveImages           func(context.Context, NodeContext) (map[string]string, error)
	CapacityQuestions       func(context.Context, NodeContext, []*unstructured.Unstructured) ([]oap.Question, []string, error)
	PlanChannels            func(context.Context, NodeContext) (PlannedChannels, error)
	ResolveChannels         func(context.Context, NodeContext, PlannedChannels) error
	ApplyChannels           func(context.Context, NodeContext, PlannedChannels) error
	RollbackAppliedChannels func(context.Context, NodeContext, PlannedChannels) error
	AdoptDecision           func(context.Context, NodeContext, []Conflict) ([]string, error)
}

// NodeContext identifies one logical node and its physical install identity.
type NodeContext struct {
	Path            oap.DependencyPath
	Bundle          *oap.Bundle
	RootInstallName string
	PhysicalName    string
	Namespace       string
	ResourceNames   instance.NameMap
}

type plannedChannelExecution struct {
	sensitiveValues func() []string
	resolve         func(context.Context) error
	rollbackResolve func(context.Context) error
	apply           func(context.Context) error
	rollbackApplied func(context.Context) error
	finalize        func()
	finalizeOnce    sync.Once
}

// NewPlannedChannelsWithRollback seals both channel application and its
// invocation-owned rollback receipt. The receipt remains live until the whole
// graph succeeds so a later node failure can unwind this node first.
func NewPlannedChannelsWithRollback(
	plans []channelplan.ChannelPlan,
	sensitiveValues func() []string,
	resolve func(context.Context) error,
	rollbackResolve func(context.Context) error,
	apply func(context.Context) error,
	rollbackApplied func(context.Context) error,
	finalize func(),
) PlannedChannels {
	return PlannedChannels{
		Plans: slices.Clone(plans),
		execution: &plannedChannelExecution{
			sensitiveValues: sensitiveValues,
			resolve:         resolve, rollbackResolve: rollbackResolve,
			apply: apply, rollbackApplied: rollbackApplied, finalize: finalize,
		},
	}
}

// PlannedChannels retains a private staged channel payload inside a GraphPlan.
// Resolve seals its result before Apply may consume it. Diagnostic formatting
// reports counts only.
type PlannedChannels struct {
	Plans            []channelplan.ChannelPlan
	execution        *plannedChannelExecution
	decisionRequired bool
	conflicts        []Conflict
}

func (p PlannedChannels) String() string {
	answered := 0
	if p.execution != nil && p.execution.sensitiveValues != nil {
		answered = len(p.execution.sensitiveValues())
	}
	return fmt.Sprintf("PlannedChannels{plans:%d, answered:%d}", len(p.Plans), answered)
}
func (p PlannedChannels) GoString() string { return p.String() }

// NewPlannedChannels seals execution-only channel state behind closures. The
// sensitive callback lets Workflow register values discovered during Resolve
// with the node redactor; no execution field is serializable or exported.
func NewPlannedChannels(
	plans []channelplan.ChannelPlan,
	sensitiveValues func() []string,
	resolve func(context.Context) error,
	rollbackResolve func(context.Context) error,
	apply func(context.Context) error,
	finalize ...func(),
) PlannedChannels {
	var finalizeOne func()
	if len(finalize) > 0 {
		finalizeOne = finalize[0]
	}
	return NewPlannedChannelsWithRollback(plans, sensitiveValues, resolve, rollbackResolve, apply, nil, finalizeOne)
}

func (p PlannedChannels) finalizeResolve() {
	if p.execution != nil && p.execution.finalize != nil {
		p.execution.finalizeOnce.Do(p.execution.finalize)
	}
}

// NewPendingPlannedChannels reports declarative channel decisions that must
// be completed by a form-based surface before an executable plan can exist.
func NewPendingPlannedChannels(plans []channelplan.ChannelPlan) PlannedChannels {
	return PlannedChannels{Plans: slices.Clone(plans), decisionRequired: true}
}

// Resolve runs provider handoffs and gathers only result-dependent fallback
// answers. It is a graph-wide pre-apply phase: callers must complete every
// node's resolution before applying any bundle-owned resource.
func (p PlannedChannels) Resolve(ctx context.Context) error {
	if p.execution == nil || p.execution.resolve == nil {
		return nil
	}
	return p.execution.resolve(ctx)
}

// RollbackResolve removes only Kubernetes prerequisites created by this
// resolution payload. Provider-side handoff effects are intentionally not
// transactional.
func (p PlannedChannels) RollbackResolve(ctx context.Context) error {
	if p.execution == nil || p.execution.rollbackResolve == nil {
		return nil
	}
	return p.execution.rollbackResolve(ctx)
}

// Apply executes the private, fully answered channel payload. A declarative
// plan with no execution payload is a no-op.
func (p PlannedChannels) Apply(ctx context.Context) error {
	if p.execution == nil || p.execution.apply == nil {
		return nil
	}
	return p.execution.apply(ctx)
}

// RollbackApplied removes only Channel/Secret objects created by this node's
// Apply. Resolution prerequisites have their own graph-wide rollback phase.
func (p PlannedChannels) RollbackApplied(ctx context.Context) error {
	if p.execution == nil || p.execution.rollbackApplied == nil {
		return nil
	}
	return p.execution.rollbackApplied(ctx)
}

// MarshalJSON exposes only the declarative channel plans. Wizard answers are
// execution-private input and must never be rendered or serialized.
func (p PlannedChannels) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Plans []channelplan.ChannelPlan `json:"plans,omitempty"`
	}{Plans: p.Plans})
}

// GraphAnswers contains already parsed graph-wide answer sources. Sets are
// keyed by DependencyPath.String() and carry local question names.
type GraphAnswers struct {
	Values          GraphValues
	Sets            map[string]map[string]string
	Interactive     bool
	QuestionOptions []ResolveOption
}

// Workflow owns graph traversal, questions, Prepare, execution, and rollback.
// Options carry surface policy, including root provenance and adoption flags;
// node-private identity/provenance is filled from the graph plan.
type Workflow struct {
	Client           client.Client
	Options          InstallOpts
	ClusterAPVersion string
	Hooks            WorkflowHooks
	// AggregateDecisions lets form-based surfaces collect missing answers and
	// conflicts from every node in one read-only pass. CLI/Desktop retain the
	// historical fail-fast behavior when it is false.
	AggregateDecisions bool
}

// MissingDecision and ConflictDecision retain the logical path for one node's
// operator decision. They never contain submitted answer values.
type MissingDecision struct {
	Path      oap.DependencyPath
	Questions []oap.Question
}

type ConflictDecision struct {
	Path      oap.DependencyPath
	Conflicts []Conflict
}

// DecisionError reports every non-fatal operator decision discovered during
// an aggregate planning pass. Workflow.Plan returns no GraphPlan alongside it.
type DecisionError struct {
	Missing      []MissingDecision
	Conflicts    []ConflictDecision
	ChannelPaths []oap.DependencyPath
}

func (e *DecisionError) Error() string {
	return fmt.Sprintf("install requires operator decisions at %d node(s)", len(e.Missing)+len(e.Conflicts)+len(e.ChannelPaths))
}

type workflowNodeState struct {
	context  NodeContext
	images   map[string]string
	channels PlannedChannels
}

type workflowPlanState struct {
	workflow *Workflow
	nodes    map[string]*workflowNodeState
}

// Plan collects every node's questions and external decisions, then delegates
// read-only ownership/conflict preparation to PlanGraph. Nothing in this method
// mutates the cluster.
func (w *Workflow) Plan(ctx context.Context, root *oap.Bundle, answers GraphAnswers) (*GraphPlan, error) {
	if w == nil || w.Client == nil {
		return nil, fmt.Errorf("install workflow: missing Kubernetes client")
	}
	// Validate the complete values tree before probing, resolving, or prompting
	// any one node.
	if _, err := ProjectValues(answers.Values, nil, root); err != nil {
		return nil, err
	}
	identity, err := answeredGraphIdentity(root, w.Options.Name, answers)
	if err != nil {
		return nil, err
	}
	ownershipName, names := identity.ownershipName, identity.names
	if err := oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		if err := Preflight(ctx, bundle, w.ClusterAPVersion); err != nil {
			return graphError("preflight", ownershipName, path, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	questionNames := make(instance.GraphNames, len(names))
	for path, name := range names {
		questionNames[path] = name
	}
	// Question expansion reports the lifecycle root while probing the exact
	// physical root resource map shared with PlanGraph.
	questionNames[""] = ownershipName
	groups, notices, err := expandQuestionGroupsMapped(ctx, w.Client, root, w.Options.Namespace, questionNames, identity.resources)
	if err != nil {
		return nil, err
	}
	groupsByPath := make(map[string]QuestionGroup, len(groups))
	bundlesByPath := make(map[string]*oap.Bundle, len(groups))
	if err := oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		bundlesByPath[path.String()] = bundle
		return nil
	}); err != nil {
		return nil, err
	}
	for _, group := range groups {
		groupsByPath[group.Path.String()] = group
	}
	for pathKey, localSets := range answers.Sets {
		bundle, ok := bundlesByPath[pathKey]
		if !ok {
			return nil, fmt.Errorf("graph set: unknown dependency path %q", pathKey)
		}
		path := groupsByPath[pathKey].Path
		for key := range localSets {
			if err := validateLocalQuestionKey(bundle, path, key); err != nil {
				return nil, graphError("questions", ownershipName, path, err)
			}
		}
	}

	runner := &workflowPlanningRunner{
		workflow:      w,
		identity:      identity,
		answers:       answers,
		groups:        groupsByPath,
		rootName:      ownershipName,
		plannedByPath: make(map[string]*workflowNodeState, len(groups)),
	}
	plan, err := planGraphWithIdentity(ctx, root, w.Options.Name, runner, identity)
	if err != nil {
		return nil, err
	}
	plan.Notices = slices.Clone(notices)
	plan.workflow = &workflowPlanState{workflow: w, nodes: runner.plannedByPath}
	return plan, nil
}

// answeredGraphIdentity resolves only questions capable of changing a
// resource's metadata.name, then freezes the complete physical identity plan
// before question probing, conflict reads, channel setup, or mutation. Other
// answers remain owned by the normal per-node preparation pass.
func answeredGraphIdentity(root *oap.Bundle, requested string, answers GraphAnswers) (*graphIdentityPlan, error) {
	type nodeObjects struct {
		authored  []*unstructured.Unstructured
		answered  []*unstructured.Unstructured
		answers   oap.Answers
		completed map[string]bool
	}
	objectsByPath := map[string]nodeObjects{}
	err := oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		values, err := projectGraphValues(answers.Values, path)
		if err != nil {
			return err
		}
		manifestValues, _ := splitGraphValues(values)
		manifestSets, _ := oap.SplitReserved(answers.Sets[path.String()])
		nameQuestions, names, err := metadataNameQuestions(bundle.Manifest.Questions)
		if err != nil {
			return graphError("plan identities", bundle.Manifest.Agent.Name, path, err)
		}
		filteredValues := filterIdentityValues(manifestValues, names)
		filteredSets := filterIdentitySets(manifestSets, names)
		var resolved oap.Answers
		if answers.Interactive {
			resolved, _, err = ResolveValues(nameQuestions, filteredValues, filteredSets, true, answers.QuestionOptions...)
		} else {
			resolved, _, _, err = resolveValuesPartial(nameQuestions, filteredValues, filteredSets, answers.QuestionOptions...)
		}
		if err != nil {
			return graphError("plan identities", bundle.Manifest.Agent.Name, path, err)
		}
		authored, err := resourcesForNames(bundle)
		if err != nil {
			return err
		}
		answered := make([]*unstructured.Unstructured, len(authored))
		for i := range authored {
			answered[i] = authored[i].DeepCopy()
		}
		if err := oap.Apply(answered, nameQuestions, resolved); err != nil {
			return fmt.Errorf("install: overlay identity answers: %w", err)
		}
		seen := map[string]bool{}
		for _, obj := range answered {
			key := obj.GetKind() + "/" + obj.GetName()
			if seen[key] {
				return fmt.Errorf("install: overlay identity answers: duplicate resource %s", key)
			}
			seen[key] = true
		}
		var completed map[string]bool
		if answers.Interactive {
			completed = names
		}
		objectsByPath[path.String()] = nodeObjects{
			authored: authored, answered: answered, answers: resolved, completed: completed,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	rootObjects := objectsByPath[""]
	rootClass, err := findAgentClass(rootObjects.answered)
	if err != nil {
		return nil, err
	}
	ownership := requested
	if ownership == "" {
		ownership = rootClass.GetName()
	}
	names, err := instance.BuildGraphNames(ownership, root)
	if err != nil {
		return nil, graphError("plan", ownership, nil, err)
	}
	if requested != "" {
		names[""] = requested + "-" + rootClass.GetName()
	} else {
		names[""] = rootClass.GetName()
	}
	plan := &graphIdentityPlan{
		ownershipName: ownership,
		names:         names,
		resources:     map[string]instance.NameMap{},
		answers:       map[string]oap.Answers{},
		completed:     map[string]map[string]bool{},
	}
	err = oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		objs := objectsByPath[path.String()]
		var mapped instance.NameMap
		if len(path) == 0 {
			mapped = make(instance.NameMap, len(objs.answered)*2)
			for _, obj := range objs.answered {
				physical := obj.GetName()
				if requested != "" {
					physical = requested + "-" + physical
				}
				mapped[obj.GetKind()+"/"+obj.GetName()] = physical
			}
		} else {
			dep, err := graphDependencyAt(root, path)
			if err != nil {
				return err
			}
			digest := dep.Descriptor.Digest
			if digest == "" && len(dep.Packed) > 0 {
				digest = godigest.FromBytes(dep.Packed).String()
			}
			if digest == "" {
				packed, err := oap.Pack(bundle)
				if err != nil {
					return err
				}
				digest = godigest.FromBytes(packed).String()
			}
			mapped, err = instance.NameMapForDependency(objs.answered, ownership, path, digest, names[path.String()])
			if err != nil {
				return err
			}
		}
		for i, authored := range objs.authored {
			answered := objs.answered[i]
			physical := mapped[answered.GetKind()+"/"+answered.GetName()]
			mapped[authored.GetKind()+"/"+authored.GetName()] = physical
		}
		plan.resources[path.String()] = mapped
		plan.answers[path.String()] = objs.answers
		if len(objs.completed) > 0 {
			plan.completed[path.String()] = objs.completed
		}
		return nil
	})
	if err != nil {
		return nil, graphError("plan identities", ownership, nil, err)
	}
	return plan, nil
}

func metadataNameQuestions(questions []oap.Question) ([]oap.Question, map[string]bool, error) {
	var selected []oap.Question
	names := map[string]bool{}
	for _, question := range questions {
		if question.Type == oap.QSecret {
			continue
		}
		for _, binding := range question.Binding {
			target, err := oap.ParseTarget(binding.Target)
			if err != nil {
				return nil, nil, fmt.Errorf("question %q binding target: %w", question.Name, err)
			}
			if len(target.Path) == 2 && target.Path[0].Field == "metadata" && target.Path[1].Field == "name" {
				selected = append(selected, question)
				names[question.Name] = true
				break
			}
		}
	}
	return selected, names, nil
}

func filterIdentityValues(values map[string]any, names map[string]bool) map[string]any {
	filtered := map[string]any{}
	for name, value := range values {
		if names[name] {
			filtered[name] = value
		}
	}
	return filtered
}

func filterIdentitySets(values map[string]string, names map[string]bool) map[string]string {
	filtered := map[string]string{}
	for name, value := range values {
		if names[name] {
			filtered[name] = value
		}
	}
	return filtered
}

// Execute resolves every node's channels before the first bundle mutation,
// then applies a complete plan leaves first. ExecuteGraph provides
// reverse-order rollback on apply failure.
func (w *Workflow) Execute(ctx context.Context, plan *GraphPlan) (*GraphResult, error) {
	if w == nil || plan == nil || plan.workflow == nil || plan.workflow.workflow != w {
		return nil, fmt.Errorf("install workflow: graph plan was not produced by this workflow")
	}
	for _, node := range plan.Nodes {
		if node == nil || node.Prepared == nil {
			return nil, fmt.Errorf("install workflow: incomplete graph plan")
		}
		if plan.workflow.nodes[node.Input.Path.String()] == nil {
			return nil, graphError("execute", plan.RootName, node.Input.Path,
				fmt.Errorf("missing planned state for dependency path %q", node.Input.Path.String()))
		}
	}
	for i, node := range plan.Nodes {
		state := plan.workflow.nodes[node.Input.Path.String()]
		var err error
		if w.Hooks.ResolveChannels != nil {
			err = w.Hooks.ResolveChannels(ctx, state.context, state.channels)
		} else {
			err = state.channels.Resolve(ctx)
		}
		registerChannelSensitive(node.Prepared, state.channels)
		if err == nil {
			continue
		}
		errs := []error{graphError("resolve channels", plan.RootName, node.Input.Path,
			redactError(node.Prepared.redactor, err))}
		errs = append(errs, rollbackResolvedChannels(ctx, plan, i)...)
		return nil, errors.Join(errs...)
	}
	runner := &workflowExecutionRunner{workflow: w, nodes: plan.workflow.nodes}
	result, err := ExecuteGraph(ctx, plan, runner)
	if err != nil {
		cleanupErrs := rollbackResolvedChannels(ctx, plan, len(plan.Nodes)-1)
		if len(cleanupErrs) == 0 {
			return nil, err
		}
		return nil, errors.Join(append([]error{err}, cleanupErrs...)...)
	}
	result.Notices = slices.Clone(plan.Notices)
	for i := range result.Nodes {
		state := runner.nodes[result.Nodes[i].Path.String()]
		if state == nil {
			continue
		}
		result.Nodes[i].Images = cloneStrings(state.images)
		result.Nodes[i].Channels = slices.Clone(state.channels.Plans)
	}
	for _, node := range plan.Nodes {
		if state := runner.nodes[node.Input.Path.String()]; state != nil {
			state.channels.finalizeResolve()
		}
	}
	return result, nil
}

// rollbackResolvedChannels runs only after either channel resolution or graph
// apply has failed. Callers pass the last receipt eligible for cleanup, which
// keeps the resolution-failure and post-ExecuteGraph paths disjoint: no node
// receipt is visited twice. Cleanup ignores cancellation from the failed
// operation but remains bounded, matching ExecuteGraph's node rollback.
func rollbackResolvedChannels(ctx context.Context, plan *GraphPlan, last int) []error {
	if plan == nil || plan.workflow == nil || last < 0 {
		return nil
	}
	if last >= len(plan.Nodes) {
		last = len(plan.Nodes) - 1
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var errs []error
	for i := last; i >= 0; i-- {
		node := plan.Nodes[i]
		state := plan.workflow.nodes[node.Input.Path.String()]
		if state == nil {
			continue
		}
		rollbackErr := state.channels.RollbackResolve(cleanupCtx)
		// Rollback may discover or surface a value that was not available when
		// resolution finished. Register again before the error is made visible.
		registerChannelSensitive(node.Prepared, state.channels)
		if rollbackErr == nil {
			continue
		}
		qualified := graphError("rollback channel prerequisites", plan.RootName, node.Input.Path,
			redactError(node.Prepared.redactor, rollbackErr))
		errs = append(errs, qualified)
		log.FromContext(ctx).Error(qualified, "graph channel prerequisite rollback failed",
			"root", plan.RootName, "dependencyPath", node.Input.Path.String())
	}
	return errs
}

type workflowPlanningRunner struct {
	workflow        *Workflow
	identity        *graphIdentityPlan
	answers         GraphAnswers
	groups          map[string]QuestionGroup
	rootName        string
	plannedByPath   map[string]*workflowNodeState
	decisions       DecisionError
	pendingChannels map[string]oap.DependencyPath
}

func (r *workflowPlanningRunner) PrepareNode(ctx context.Context, input NodeInput) (*Prepared, error) {
	pathKey := input.Path.String()
	group, ok := r.groups[pathKey]
	if !ok {
		return nil, fmt.Errorf("questions: no group for dependency path %q", pathKey)
	}
	return r.prepareProjected(ctx, input, group)
}

func (r *workflowPlanningRunner) prepareProjected(ctx context.Context, input NodeInput, group QuestionGroup) (*Prepared, error) {
	// Project from the already validated nested tree without reinterpreting
	// external keys.
	values, err := projectGraphValues(r.answers.Values, input.Path)
	if err != nil {
		return nil, err
	}
	manifestValues, capacityValues := splitGraphValues(values)
	manifestSets, capacitySets := oap.SplitReserved(r.answers.Sets[input.Path.String()])
	completedIdentity := r.completedIdentityQuestions(input.Path.String())
	identityAnswers := r.identityAnswers(input.Path.String())
	skippedIdentity := make(map[string]bool, len(completedIdentity))
	for name := range completedIdentity {
		delete(manifestSets, name)
		if value, answered := identityAnswers[name]; answered {
			manifestValues[name] = value
		} else {
			delete(manifestValues, name)
			skippedIdentity[name] = true
		}
	}
	questions := excludeCompletedQuestions(group.Questions, skippedIdentity)
	node := NodeContext{
		Path: slices.Clone(input.Path), Bundle: input.Bundle, RootInstallName: r.rootName, PhysicalName: input.PhysicalName,
		Namespace: r.workflow.Options.Namespace, ResourceNames: maps.Clone(input.ResourceNames),
	}
	if r.workflow.Hooks.InspectNode != nil {
		if err := r.workflow.Hooks.InspectNode(ctx, node); err != nil {
			return nil, fmt.Errorf("inspect node: %w", err)
		}
	}
	images := cloneStrings(r.workflow.Options.ImageRewrites)
	if r.workflow.Hooks.ResolveImages != nil {
		images, err = r.workflow.Hooks.ResolveImages(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("resolve images: %w", err)
		}
	}
	channels := PlannedChannels{}
	if r.workflow.Hooks.PlanChannels != nil {
		channels, err = r.workflow.Hooks.PlanChannels(ctx, node)
		if err != nil {
			return nil, fmt.Errorf("plan channels: %w", err)
		}
	}
	if channels.decisionRequired {
		if r.pendingChannels == nil {
			r.pendingChannels = map[string]oap.DependencyPath{}
		}
		r.pendingChannels[input.Path.String()] = slices.Clone(input.Path)
	}
	var resolved oap.Answers
	var secrets []SecretSpec
	var missing *MissingAnswersError
	if r.workflow.AggregateDecisions {
		resolved, secrets, missing, err = resolveValuesPartial(questions, manifestValues, manifestSets, r.answers.QuestionOptions...)
	} else {
		resolved, secrets, err = ResolveValues(questions, manifestValues, manifestSets, r.answers.Interactive, r.answers.QuestionOptions...)
	}
	if err != nil {
		return nil, err
	}
	for name, value := range identityAnswers {
		resolved[name] = value
	}

	opts := r.workflow.Options
	opts.RootInstallName = r.rootName
	opts.DependencyPath = slices.Clone(input.Path)
	opts.ResourceNames = input.ResourceNames
	opts.DirectNames = input.DirectNames
	opts.ImageRewrites = images
	opts.Interactive = r.answers.Interactive
	opts.Sets = capacitySets
	opts.ExtraValues = capacityValues
	opts.QuestionOptions = r.answers.QuestionOptions
	opts.additionalConflicts = slices.Clone(channels.conflicts)
	opts.decisionDiscovery = r.workflow.AggregateDecisions
	opts.priorMissing = missing
	if len(input.Path) > 0 {
		opts.SourceKind, opts.SourceRef, opts.SourceDigest = input.SourceKind, input.SourceRef, input.PackedDigest
	}
	if r.workflow.Hooks.CapacityQuestions != nil {
		opts.ExtraQuestions = func(hookCtx context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
			return r.workflow.Hooks.CapacityQuestions(hookCtx, node, crs)
		}
	}
	if r.workflow.Hooks.AdoptDecision != nil {
		opts.AdoptDecision = func(hookCtx context.Context, conflicts []Conflict) ([]string, error) {
			return r.workflow.Hooks.AdoptDecision(hookCtx, node, conflicts)
		}
	}
	prepared, err := Prepare(ctx, r.workflow.Client, input.Bundle, resolved, secrets, opts)
	if err != nil {
		return nil, err
	}
	registerChannelSensitive(prepared, channels)
	r.plannedByPath[input.Path.String()] = &workflowNodeState{context: node, images: cloneStrings(images), channels: channels}
	return prepared, nil
}

func (r *workflowPlanningRunner) identityAnswers(path string) oap.Answers {
	if r == nil || r.identity == nil {
		return nil
	}
	return r.identity.answers[path]
}

func (r *workflowPlanningRunner) completedIdentityQuestions(path string) map[string]bool {
	if r == nil || r.identity == nil {
		return nil
	}
	return r.identity.completed[path]
}

func excludeCompletedQuestions(questions []oap.Question, completed map[string]bool) []oap.Question {
	if len(completed) == 0 {
		return questions
	}
	remaining := make([]oap.Question, 0, len(questions))
	for _, question := range questions {
		if !completed[question.Name] {
			remaining = append(remaining, question)
		}
	}
	return remaining
}

func (r *workflowPlanningRunner) CollectPreparationDecision(path oap.DependencyPath, err error) bool {
	if !r.workflow.AggregateDecisions {
		return false
	}
	var combined *nodeDecisionError
	if errors.As(err, &combined) {
		if combined.Missing != nil {
			r.decisions.Missing = append(r.decisions.Missing, MissingDecision{
				Path: slices.Clone(path), Questions: slices.Clone(combined.Missing.Questions),
			})
		}
		if combined.Conflicts != nil {
			r.decisions.Conflicts = append(r.decisions.Conflicts, ConflictDecision{
				Path: slices.Clone(path), Conflicts: slices.Clone(combined.Conflicts.Conflicts),
			})
		}
		return true
	}
	var missing *MissingAnswersError
	if errors.As(err, &missing) {
		r.decisions.Missing = append(r.decisions.Missing, MissingDecision{
			Path: slices.Clone(path), Questions: slices.Clone(missing.Questions),
		})
		return true
	}
	var conflicts *ConflictError
	if errors.As(err, &conflicts) {
		r.decisions.Conflicts = append(r.decisions.Conflicts, ConflictDecision{
			Path: slices.Clone(path), Conflicts: slices.Clone(conflicts.Conflicts),
		})
		return true
	}
	return false
}

func (r *workflowPlanningRunner) PreparationDecisionError() error {
	if len(r.pendingChannels) > 0 {
		paths := make([]string, 0, len(r.pendingChannels))
		for path := range r.pendingChannels {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			r.decisions.ChannelPaths = append(r.decisions.ChannelPaths, slices.Clone(r.pendingChannels[path]))
		}
	}
	if len(r.decisions.Missing) == 0 && len(r.decisions.Conflicts) == 0 && len(r.decisions.ChannelPaths) == 0 {
		return nil
	}
	return &r.decisions
}

func registerChannelSensitive(prepared *Prepared, channels PlannedChannels) {
	if prepared == nil || prepared.redactor == nil || channels.execution == nil || channels.execution.sensitiveValues == nil {
		return
	}
	for _, value := range channels.execution.sensitiveValues() {
		if value != "" {
			prepared.redactor.RegisterSensitive(value, redact.Descriptor{Description: "channel wizard answer"})
		}
	}
}

func (r *workflowPlanningRunner) ApplyNode(context.Context, *Prepared) (*Result, error) {
	return nil, fmt.Errorf("install workflow: planning runner cannot apply")
}
func (r *workflowPlanningRunner) RollbackNode(context.Context, *Prepared) error {
	return fmt.Errorf("install workflow: planning runner cannot roll back")
}

type workflowExecutionRunner struct {
	workflow *Workflow
	nodes    map[string]*workflowNodeState
}

func (r *workflowExecutionRunner) PrepareNode(context.Context, NodeInput) (*Prepared, error) {
	return nil, fmt.Errorf("install workflow: execution runner cannot prepare")
}
func (r *workflowExecutionRunner) ApplyNode(ctx context.Context, prepared *Prepared) (*Result, error) {
	result, err := ApplyPrepared(ctx, r.workflow.Client, prepared)
	if err != nil {
		return nil, err
	}
	state := r.nodes[prepared.Options.DependencyPath.String()]
	if state == nil {
		return nil, fmt.Errorf("install workflow: missing planned state for dependency path %q", prepared.Options.DependencyPath.String())
	}
	if r.workflow.Hooks.ApplyChannels != nil {
		if err := r.workflow.Hooks.ApplyChannels(ctx, state.context, state.channels); err != nil {
			return nil, withAdoptedHint(fmt.Errorf("apply channels: %w", err), result.Adopted)
		}
	}
	return result, nil
}
func (r *workflowExecutionRunner) RollbackNode(ctx context.Context, prepared *Prepared) error {
	state := r.nodes[prepared.Options.DependencyPath.String()]
	var channelErr error
	if state != nil {
		if r.workflow.Hooks.RollbackAppliedChannels != nil {
			channelErr = r.workflow.Hooks.RollbackAppliedChannels(ctx, state.context, state.channels)
		} else {
			channelErr = state.channels.RollbackApplied(ctx)
		}
	}
	return errors.Join(channelErr, RollbackPrepared(ctx, r.workflow.Client, prepared))
}

func splitGraphValues(values map[string]any) (map[string]any, map[string]any) {
	manifest := make(map[string]any, len(values))
	capacity := make(map[string]any)
	for key, value := range values {
		if strings.HasPrefix(key, oap.ReservedQuestionPrefix) {
			capacity[key] = value
		} else {
			manifest[key] = value
		}
	}
	return manifest, capacity
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}
