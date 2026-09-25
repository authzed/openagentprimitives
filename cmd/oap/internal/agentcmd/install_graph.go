package agentcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/imagebuild"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	oappin "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

type cliInstallOptions struct {
	name          string
	valuesFile    string
	sets          map[string]string
	fitResources  bool
	adoptAll      bool
	adoptKeys     []string
	buildMissing  bool
	noBuild       bool
	rebuild       bool
	buildSecrets  map[string]string
	buildContexts map[string]string
	platform      string
	vmSSHKey      string
	imageRegistry string
}

type cliWorkflowConfig struct {
	ctx            context.Context
	globals        *apcmd.Globals
	kube           *kube.Bundle
	root           *oap.Bundle
	sourcePath     string
	source         *installSource
	options        cliInstallOptions
	values         install.GraphValues
	sets           map[string]map[string]string
	interactive    bool
	questionDriver tui.Driver
	questionTheme  *tui.Theme
	questionOpts   []install.ResolveOption
	in             io.Reader
	out            io.Writer
	installEnv     cliInstallEnvFactory
	adoption       *cliAdoptionTracker
}

type cliInstallEnvFactory func(context.Context, *apcmd.Globals, string, buildSets, io.Writer, tui.Driver, *tui.Theme) (imagebuild.Env, error)

type cliAdoptionTracker struct {
	adoptAll    bool
	requested   map[string]bool
	matched     map[string]bool
	hadConflict bool
}

func newCLIAdoptionTracker(adoptAll bool, keys []string) *cliAdoptionTracker {
	requested := make(map[string]bool, len(keys))
	for _, key := range keys {
		requested[key] = true
	}
	return &cliAdoptionTracker{adoptAll: adoptAll, requested: requested, matched: map[string]bool{}}
}

func (t *cliAdoptionTracker) decide(_ context.Context, _ install.NodeContext, conflicts []install.Conflict) ([]string, error) {
	t.hadConflict = t.hadConflict || len(conflicts) > 0
	selected := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		key := conflict.Key()
		if t.requested[key] {
			t.matched[key] = true
			selected = append(selected, key)
			continue
		}
		if t.adoptAll && !conflict.Secret {
			selected = append(selected, key)
		}
	}
	return selected, nil
}

func (t *cliAdoptionTracker) validate() error {
	if t == nil || !t.hadConflict {
		return nil
	}
	var unknown []string
	for key := range t.requested {
		if !t.matched[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("install: --adopt names %v, which is not a conflicting object in this install graph", unknown)
}

func runCLIInstallGraph(cmd *cobra.Command, g *apcmd.Globals, root *oap.Bundle, src *installSource, sourcePath string, opts cliInstallOptions) error {
	interactive := apcmd.StdinIsInteractive(os.Stdin)
	questionDriver, questionTheme := installQuestionPresentation(cmd.OutOrStdout(), os.Stdin, g.NoColor, interactive)
	var questionOpts []install.ResolveOption
	if questionDriver != nil {
		questionOpts = append(questionOpts, install.PresentOver(questionDriver, questionTheme))
	}

	if err := validateCLIGraph(cmd.Context(), root, opts.name, cmd.OutOrStdout()); err != nil {
		return err
	}
	kb, err := g.Bundle()
	if err != nil {
		return err
	}
	values, err := install.LoadGraphValues(opts.valuesFile)
	if err != nil {
		return err
	}
	sets, err := install.ParseGraphSet(opts.sets, root)
	if err != nil {
		return err
	}
	adoption := newCLIAdoptionTracker(opts.adoptAll, opts.adoptKeys)

	w := newCLIWorkflow(cliWorkflowConfig{
		ctx:     cmd.Context(),
		globals: g, kube: kb, root: root, sourcePath: sourcePath, source: src,
		options: opts, values: values, sets: sets, interactive: interactive,
		questionDriver: questionDriver, questionTheme: questionTheme, questionOpts: questionOpts,
		in: os.Stdin, out: cmd.OutOrStdout(), adoption: adoption,
	})
	plan, err := w.Plan(cmd.Context(), root, install.GraphAnswers{
		Values: values, Sets: sets, Interactive: interactive, QuestionOptions: questionOpts,
	})
	if err != nil {
		return wrapConflictError(withGraphAnswerFlagHint(err, root))
	}
	if err := adoption.validate(); err != nil {
		return err
	}
	for _, notice := range plan.Notices {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", notice)
	}
	result, err := w.Execute(cmd.Context(), plan)
	if err != nil {
		return wrapConflictError(err)
	}
	writeGraphWarnings(cmd.ErrOrStderr(), result)
	writeGraphResult(cmd.OutOrStdout(), kb.Namespace, result)
	return writeInstallPin(cmd.OutOrStdout(), src)
}

func validateCLIGraph(ctx context.Context, root *oap.Bundle, requestedName string, out io.Writer) error {
	return oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, bundle *oap.Bundle) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := checkSkillsShape(bundle); err != nil {
			return qualifyCLIGraphError(root, path, err)
		}
		instanceName := ""
		if len(path) == 0 {
			instanceName = requestedName
		}
		if err := checkDeclaredChannels(bundle, instanceName); err != nil {
			return qualifyCLIGraphError(root, path, err)
		}
		if err := install.Preflight(ctx, bundle, ""); err != nil {
			return qualifyCLIGraphError(root, path, fmt.Errorf("preflight: %w", err))
		}
		clones, err := bundle.SkillClones()
		if err != nil {
			return qualifyCLIGraphError(root, path, fmt.Errorf("inspect bundled skills: %w", err))
		}
		if notice := skillCloneNotice(clones); notice != "" {
			if len(path) > 0 {
				fmt.Fprintf(out, "For dependency %s:\n", path.String())
			}
			fmt.Fprint(out, notice)
		}
		return nil
	})
}

func qualifyCLIGraphError(root *oap.Bundle, path oap.DependencyPath, err error) error {
	if len(path) == 0 || root == nil || root.Manifest == nil {
		return err
	}
	return fmt.Errorf("install %s > %s: %w", root.Manifest.Agent.Name, path.String(), err)
}

func newCLIWorkflow(cfg cliWorkflowConfig) *install.Workflow {
	capacityQuestions := NewCapacityQuestions(cfg.ctx, cfg.kube)
	installEnvFactory := cfg.installEnv
	if installEnvFactory == nil {
		installEnvFactory = newInstallEnv
	}
	options := install.InstallOpts{
		Name: cfg.options.name, Namespace: cfg.kube.Namespace,
		SourceKind: cfg.source.kind, SourceRef: cfg.source.ref, SourceDigest: cfg.source.digest,
		Interactive: cfg.interactive, QuestionOptions: cfg.questionOpts,
		AdoptSecretsAllowed: true,
	}
	var adoptDecision func(context.Context, install.NodeContext, []install.Conflict) ([]string, error)
	if len(cfg.options.adoptKeys) > 0 {
		tracker := cfg.adoption
		if tracker == nil {
			tracker = newCLIAdoptionTracker(cfg.options.adoptAll, cfg.options.adoptKeys)
		}
		adoptDecision = tracker.decide
	} else if cfg.options.adoptAll {
		options.AdoptAll = true
	} else if cfg.questionDriver != nil {
		decide := newAdoptDecision(tui.Options{
			Theme: cfg.questionTheme, Driver: cfg.questionDriver, In: cfg.in, Out: cfg.out,
		})
		adoptDecision = func(ctx context.Context, _ install.NodeContext, conflicts []install.Conflict) ([]string, error) {
			for _, conflict := range conflicts {
				if conflict.AdoptionRefused() {
					return nil, fmt.Errorf("%s cannot be adopted; choose a different channel name or remove the conflicting Secret out of band", conflict.Key())
				}
			}
			return decide(ctx, conflicts)
		}
	}

	return &install.Workflow{
		Client:  cfg.kube.Controller,
		Options: options,
		Hooks: install.WorkflowHooks{
			AdoptDecision: adoptDecision,
			ResolveImages: func(ctx context.Context, node install.NodeContext) (map[string]string, error) {
				if len(node.Bundle.Manifest.Requires.Images) == 0 {
					return nil, nil
				}
				env, err := installEnvFactory(ctx, cfg.globals, dependencyBundleDir(cfg.sourcePath, cfg.root, node.Path), buildSets{
					secrets: cfg.options.buildSecrets, contexts: cfg.options.buildContexts,
					platform: cfg.options.platform, vmSSHKey: cfg.options.vmSSHKey, registry: cfg.options.imageRegistry,
				}, cfg.out, cfg.questionDriver, cfg.questionTheme)
				if err != nil {
					return nil, err
				}
				return imagebuild.Reconcile(ctx, cfg.out, node.Bundle.Manifest.Requires.Images, env, imagebuild.Options{
					BuildMissing: cfg.options.buildMissing, NoBuild: cfg.options.noBuild,
					Rebuild: cfg.options.rebuild, Platform: cfg.options.platform,
				})
			},
			CapacityQuestions: func(ctx context.Context, node install.NodeContext, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				hook := capacityQuestions
				if !cfg.interactive && !cfg.options.fitResources {
					hook = requireCapacityConsent(hook, explicitCapacityAnswers(cfg, node.Path))
				}
				questions, notices, err := hook(ctx, crs)
				if err != nil || !cfg.options.fitResources || !cfg.interactive {
					return questions, notices, err
				}
				return applyCapacityDefaults(crs, questions, notices, explicitCapacityAnswers(cfg, node.Path))
			},
			PlanChannels: func(ctx context.Context, node install.NodeContext) (install.PlannedChannels, error) {
				if len(node.Bundle.Manifest.Requires.Channels) == 0 {
					return install.PlannedChannels{}, nil
				}
				wiring := installChannelWiring(cfg.kube, cfg.in, cfg.out, cfg.questionDriver, cfg.questionTheme)
				wiring.strat = clusterKindOrWarn(ctx, cfg.kube, cfg.out)
				instanceName := ""
				if len(node.Path) == 0 {
					instanceName = cfg.options.name
				}
				prepared, err := wiring.prepareDeclaredChannels(
					ctx, node.Bundle, node.PhysicalName, instanceName, node.ResourceNames,
					node.RootInstallName, node.Path, len(cfg.root.Dependencies) > 0,
				)
				if err != nil {
					return install.PlannedChannels{}, err
				}
				planned := install.NewPlannedChannelsWithRollback(
					prepared.plans, prepared.sensitiveValues, prepared.resolve, prepared.rollbackResolve,
					prepared.apply, prepared.rollbackApplied, prepared.finalize,
				)
				return planned.WithChannelCredentialPreflights(prepared.credentialPreflights...), nil
			},
			ResolveChannels: func(ctx context.Context, _ install.NodeContext, planned install.PlannedChannels) error {
				return planned.Resolve(ctx)
			},
			ApplyChannels: func(ctx context.Context, _ install.NodeContext, planned install.PlannedChannels) error {
				return planned.Apply(ctx)
			},
			RollbackAppliedChannels: func(ctx context.Context, _ install.NodeContext, planned install.PlannedChannels) error {
				return planned.RollbackApplied(ctx)
			},
		},
	}
}

// applyCapacityDefaults preserves --fit-resources' root-install behavior on a
// terminal: authored questions remain interactive, while capacity questions
// without an explicit --set/--values answer take their suggested defaults.
// The graph workflow has one interactivity bit for every question class, so
// the capacity hook applies only those defaults directly and returns the
// explicitly answered questions to the normal resolver for type validation.
func applyCapacityDefaults(crs []*unstructured.Unstructured, questions []oap.Question, notices []string, explicit map[string]string) ([]oap.Question, []string, error) {
	var unresolved []oap.Question
	var defaulted []oap.Question
	answers := oap.Answers{}
	for _, question := range questions {
		if _, ok := explicit[question.Name]; ok || question.Default == nil {
			unresolved = append(unresolved, question)
			continue
		}
		defaulted = append(defaulted, question)
		answers[question.Name] = question.Default
	}
	if len(defaulted) > 0 {
		if err := oap.Apply(crs, defaulted, answers); err != nil {
			return nil, nil, fmt.Errorf("apply --fit-resources defaults: %w", err)
		}
	}
	return unresolved, notices, nil
}

func explicitCapacityAnswers(cfg cliWorkflowConfig, path oap.DependencyPath) map[string]string {
	answers := map[string]string{}
	if values, err := install.ProjectValues(cfg.values, path, cfg.root); err == nil {
		for key := range values {
			if strings.HasPrefix(key, oap.ReservedQuestionPrefix) {
				answers[key] = "set"
			}
		}
	}
	for key := range cfg.sets[path.String()] {
		if strings.HasPrefix(key, oap.ReservedQuestionPrefix) {
			answers[key] = "set"
		}
	}
	return answers
}

func dependencyBundleDir(sourcePath string, root *oap.Bundle, path oap.DependencyPath) string {
	info, err := os.Stat(sourcePath)
	if err != nil || !info.IsDir() {
		return ""
	}
	dir, bundle := sourcePath, root
	for _, component := range path {
		found := false
		for _, dependency := range bundle.Dependencies {
			if dependency == nil || dependency.Bundle == nil || dependency.Bundle.Manifest == nil || dependency.Bundle.Manifest.Agent.Name != component {
				continue
			}
			if dependency.Descriptor.Path == "" {
				return ""
			}
			dir = filepath.Join(dir, dependency.Descriptor.Path)
			bundle = dependency.Bundle
			found = true
			break
		}
		if !found {
			return ""
		}
	}
	return dir
}

func withGraphAnswerFlagHint(err error, root *oap.Bundle) error {
	var missing *install.MissingAnswersError
	if !errors.As(err, &missing) {
		return err
	}
	var matched oap.DependencyPath
	_ = oap.WalkDependencies(root, oap.ParentsFirst, func(path oap.DependencyPath, _ *oap.Bundle) error {
		if len(path) > len(matched) && strings.Contains(err.Error(), " > "+path.String()+":") {
			matched = slices.Clone(path)
		}
		return nil
	})
	if len(matched) == 0 {
		return withAnswerFlagHint(err, graphSecretQuestionHints(root, missing.Names))
	}
	prefix := "agents." + strings.Join(matched, ".agents.") + "."
	keys := make([]string, 0, len(missing.Names))
	for _, name := range missing.Names {
		keys = append(keys, prefix+name)
	}
	sort.Strings(keys)
	return fmt.Errorf("%v; this install cannot prompt for them (stdin is not a terminal). Answer each with --set %s=<value>, or nest it under the same agents path in a --values file", err, strings.Join(keys, "=<value> --set "))
}

func graphSecretQuestionHints(bundle *oap.Bundle, missing []string) []oap.Question {
	if bundle == nil || bundle.Manifest == nil {
		return nil
	}
	missingSet := make(map[string]bool, len(missing))
	for _, name := range missing {
		missingSet[name] = true
	}
	var questions []oap.Question
	for _, required := range bundle.Manifest.Requires.Secrets {
		if required.Question != "" {
			continue
		}
		for _, key := range required.Keys {
			name := oap.RequiredSecretQuestionName(required.Name, key)
			if !missingSet[name] {
				continue
			}
			questions = append(questions, oap.Question{
				Name: name,
				Secret: &oap.SecretQuestion{CreateSecret: &oap.SecretTarget{
					Name: required.Name,
					Key:  key,
				}},
			})
		}
	}
	return questions
}

func writeGraphWarnings(out io.Writer, result *install.GraphResult) {
	if result == nil {
		return
	}
	for _, node := range result.Nodes {
		if node.Result == nil {
			continue
		}
		for _, warning := range node.Result.Warnings {
			fmt.Fprintf(out, "warning: install %s: %s\n", graphResultPath(result.RootName, node.Path), warning)
		}
	}
}

func writeGraphResult(out io.Writer, namespace string, result *install.GraphResult) {
	if result == nil {
		return
	}
	byPath := make(map[string]install.NodeResult, len(result.Nodes))
	children := make(map[string][]install.NodeResult)
	for _, node := range result.Nodes {
		byPath[node.Path.String()] = node
		if len(node.Path) > 0 {
			parent := oap.DependencyPath(node.Path[:len(node.Path)-1]).String()
			children[parent] = append(children[parent], node)
		}
	}
	root, ok := byPath[""]
	if !ok || root.Result == nil {
		return
	}
	fmt.Fprintf(out, "Installed %s into namespace %s\n", root.Result.Name, namespace)
	writeNodeResultDetails(out, "  ", root)
	var walk func(string, string)
	walk = func(parent, indent string) {
		nodes := children[parent]
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Path.String() < nodes[j].Path.String() })
		for i, node := range nodes {
			last := i == len(nodes)-1
			connector, childIndent := "├─", indent+"│  "
			if last {
				connector, childIndent = "└─", indent+"   "
			}
			logicalName := node.Path[len(node.Path)-1]
			fmt.Fprintf(out, "%s%s %s → %s\n", indent, connector, logicalName, node.Name)
			writeNodeResultDetails(out, childIndent, node)
			walk(node.Path.String(), childIndent)
		}
	}
	walk("", "")
}

func writeNodeResultDetails(out io.Writer, indent string, node install.NodeResult) {
	if node.Result == nil {
		return
	}
	fmt.Fprintf(out, "%skinds applied:   %v\n", indent, node.Result.AppliedKinds)
	fmt.Fprintf(out, "%ssecrets created: %d\n", indent, node.Result.SecretsCreated)
	if adopted := adoptedNotice(node.Result.Adopted); adopted != "" {
		if indent == "  " {
			fmt.Fprint(out, adopted)
		} else {
			for _, line := range strings.Split(strings.TrimSuffix(adopted, "\n"), "\n") {
				fmt.Fprintf(out, "%s%s\n", indent, strings.TrimPrefix(line, "  "))
			}
		}
	}
	if len(node.Images) > 0 {
		refs := make([]string, 0, len(node.Images))
		for source, resolved := range node.Images {
			refs = append(refs, source+" → "+resolved)
		}
		sort.Strings(refs)
		fmt.Fprintf(out, "%simages:          %s\n", indent, strings.Join(refs, ", "))
	}
	if len(node.Channels) > 0 {
		channels := make([]string, 0, len(node.Channels))
		for _, channel := range node.Channels {
			channels = append(channels, channel.Required.Name+" ("+channel.Required.Kind+")")
		}
		fmt.Fprintf(out, "%schannels:        %s\n", indent, strings.Join(channels, ", "))
	}
}

func graphResultPath(root string, path oap.DependencyPath) string {
	if len(path) == 0 {
		return root
	}
	return root + " > " + path.String()
}

func writeInstallPin(out io.Writer, src *installSource) error {
	if src.kind != installSourceKindRegistry {
		return nil
	}
	ref, err := (&oappin.Kind{}).ParseRef(src.ref)
	if err != nil {
		return fmt.Errorf("record oap pin: %w", err)
	}
	frozen, err := oappin.FrozenFromDigest(src.ref, src.digest)
	if err != nil {
		return fmt.Errorf("record oap pin: %w", err)
	}
	fmt.Fprintf(out, "  oap pin:         %s (%s)\n", src.ref, ref.Strength)
	fmt.Fprintf(out, "    digest:        %s\n", frozen.Digest)
	if frozen.Version != "" {
		fmt.Fprintf(out, "    version:       %s\n", frozen.Version)
	}
	return nil
}
