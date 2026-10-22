package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

const keyWorkflows = "workflows"
const keySessionOwnerPermission = "session-owner-permission"

type handlerArguments struct {
	Trigger *api.GitHubTriggerConfig `json:"trigger,omitempty"`
}

func decodeHandlerConfig(raw json.RawMessage) (*api.GitHubChannelConfig, error) {
	if len(raw) == 0 {
		return &api.GitHubChannelConfig{}, nil
	}
	if string(bytes.TrimSpace(raw)) == "null" {
		return nil, fmt.Errorf("github handler config must be an object")
	}
	var args handlerArguments
	if err := strictDecode(raw, &args); err != nil {
		return nil, fmt.Errorf("github handler config: %w", err)
	}
	cfg := &api.GitHubChannelConfig{Trigger: args.Trigger}
	if args.Trigger == nil {
		return cfg, nil
	}
	t := args.Trigger
	if t.Event != "workflow_run" {
		return nil, fmt.Errorf("github trigger event %q is unsupported; omit trigger for legacy pull_request events", t.Event)
	}
	if len(t.Actions) != 1 || t.Actions[0] != "completed" {
		return nil, fmt.Errorf("github workflow_run requires actions: [completed]")
	}
	if t.Readiness != "all_completed" {
		return nil, fmt.Errorf("github workflow_run requires readiness: all_completed")
	}
	if len(t.Workflows) > 0 {
		if err := validateWorkflowPaths(t.Workflows); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

func strictDecode(raw []byte, value any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

// ValidateHandlerConfig validates the portable declaration before installation.
// Workflow selections may be omitted here because they are required install answers.
func (Kind) ValidateHandlerConfig(raw json.RawMessage) error {
	_, err := decodeHandlerConfig(raw)
	return err
}

var repositoryNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validateWorkflowPaths(workflows map[string][]string) error {
	for repo, paths := range workflows {
		parts := strings.Split(repo, "/")
		if len(parts) != 2 || !ownerLoginPattern.MatchString(parts[0]) || !repositoryNamePattern.MatchString(parts[1]) || parts[1] == "." || parts[1] == ".." {
			return fmt.Errorf("github workflows: %q must be owner/repository", repo)
		}
		if len(paths) == 0 {
			return fmt.Errorf("github workflows: %s requires at least one workflow", repo)
		}
		seen := map[string]bool{}
		for _, p := range paths {
			if !strings.HasPrefix(p, ".github/workflows/") || path.Clean(p) != p || strings.Count(p, "/") != 2 || (!strings.HasSuffix(p, ".yml") && !strings.HasSuffix(p, ".yaml")) || seen[p] {
				return fmt.Errorf("github workflows: %s has invalid or duplicate workflow path %q", repo, p)
			}
			seen[p] = true
		}
	}
	return nil
}

func effectiveHandlerConfig(in channelkinds.WizardInput, answers map[string]string) (*api.GitHubChannelConfig, error) {
	cfg, err := decodeHandlerConfig(in.Config)
	if err != nil {
		return nil, err
	}
	if len(in.Config) == 0 && in.Existing != nil && in.Existing.Spec.GitHub != nil {
		// Deep-copy through JSON: generated deepcopy is regenerated with the API.
		raw, err := json.Marshal(in.Existing.Spec.GitHub)
		if err != nil {
			return nil, fmt.Errorf("copy existing github config: %w", err)
		}
		if err := json.Unmarshal(raw, cfg); err != nil {
			return nil, err
		}
	}
	if cfg.Trigger == nil {
		if answers[keyWorkflows] != "" {
			return nil, fmt.Errorf("workflows requires a workflow_run trigger")
		}
		return cfg, nil
	}
	if len(cfg.Trigger.Workflows) == 0 && in.Existing != nil && in.Existing.Spec.GitHub != nil && in.Existing.Spec.GitHub.Trigger != nil && in.Existing.Spec.GitHub.Trigger.Event == cfg.Trigger.Event {
		cfg.Trigger.Workflows = in.Existing.Spec.GitHub.Trigger.Workflows
	}
	if raw, supplied := answers[keyWorkflows]; supplied {
		cfg.Trigger.Workflows = nil
		if err := strictDecode([]byte(raw), &cfg.Trigger.Workflows); err != nil {
			return nil, fmt.Errorf("github workflows answer: %w", err)
		}
	}
	if len(cfg.Trigger.Workflows) == 0 {
		return nil, fmt.Errorf("github workflows: select required workflows for every repository before installation")
	}
	if err := validateWorkflowPaths(cfg.Trigger.Workflows); err != nil {
		return nil, err
	}
	cfg.Repositories = nil
	for repo := range cfg.Trigger.Workflows {
		cfg.Repositories = append(cfg.Repositories, repo)
	}
	sort.Strings(cfg.Repositories)
	// Legacy action defaults must never enable pull_request alongside workflow_run.
	cfg.Events = nil
	return cfg, nil
}

func workflowQuestions(in channelkinds.WizardInput) ([]oap.Question, error) {
	cfg, err := decodeHandlerConfig(in.Config)
	if err != nil {
		return nil, err
	}
	if cfg.Trigger == nil && in.Existing != nil && in.Existing.Spec.GitHub != nil {
		cfg = in.Existing.Spec.GitHub
	}
	if cfg.Trigger == nil {
		return nil, nil
	}
	if len(cfg.Trigger.Workflows) == 0 && in.Existing != nil && in.Existing.Spec.GitHub != nil && in.Existing.Spec.GitHub.Trigger != nil && in.Existing.Spec.GitHub.Trigger.Event == cfg.Trigger.Event {
		cfg.Trigger.Workflows = in.Existing.Spec.GitHub.Trigger.Workflows
	}
	var selected any
	if len(cfg.Trigger.Workflows) > 0 {
		raw, err := json.Marshal(cfg.Trigger.Workflows)
		if err != nil {
			return nil, err
		}
		selected = string(raw)
	}
	var ownerDefault any
	if in.Existing != nil && in.Existing.Spec.Owner != nil && in.Existing.Spec.Owner.Ownerless != nil {
		ownerDefault = in.Existing.Spec.Owner.Ownerless.Permission
	}
	return []oap.Question{{Name: keyWorkflows, Type: oap.QString, Prompt: "Which workflows must finish before this input starts the agent?", Description: `Required JSON mapping for each selected repository: {"owner/repository":[".github/workflows/test.yml"]}. Use workflow paths from that repository's Actions page. Every listed workflow must complete for the current PR revision.`, Default: selected}, {Name: keySessionOwnerPermission, Type: oap.QString, Prompt: "Which subject set owns sessions started by this input?", Description: "An explicit SpiceDB subject-set reference, for example group:ci-operators#member. Members may approve and manage these sessions. No conversational output channel is required.", Default: ownerDefault}}, nil
}

func workflowMode(in channelkinds.WizardInput) (bool, error) {
	cfg, err := decodeHandlerConfig(in.Config)
	if err != nil {
		return false, err
	}
	if cfg.Trigger == nil && in.Existing != nil && in.Existing.Spec.GitHub != nil {
		cfg = in.Existing.Spec.GitHub
	}
	return cfg.Trigger != nil && cfg.Trigger.Event == "workflow_run", nil
}

// HandlerConfigMatches preserves per-install workflow answers when a portable
// bundle omits them, while refusing to skip an incompatible installed handler.
func (Kind) HandlerConfigMatches(raw json.RawMessage, existing *api.Channel) error {
	wanted, err := decodeHandlerConfig(raw)
	if err != nil {
		return err
	}
	if existing == nil || existing.Spec.GitHub == nil {
		return fmt.Errorf("GitHub configuration is missing")
	}
	actual := existing.Spec.GitHub.Trigger
	if wanted.Trigger == nil {
		if actual != nil {
			return fmt.Errorf("installed event differs from declared legacy pull_request event")
		}
		return nil
	}
	if actual == nil || wanted.Trigger.Event != actual.Event || wanted.Trigger.Readiness != actual.Readiness || !slices.Equal(wanted.Trigger.Actions, actual.Actions) {
		return fmt.Errorf("installed trigger differs from declaration")
	}
	if len(actual.Workflows) == 0 {
		return fmt.Errorf("installed workflow selection is empty")
	}
	if err := validateWorkflowPaths(actual.Workflows); err != nil {
		return err
	}
	if len(wanted.Trigger.Workflows) > 0 && !workflowMapsEqual(wanted.Trigger.Workflows, actual.Workflows) {
		return fmt.Errorf("installed workflow selections differ from declaration")
	}
	return nil
}

func workflowMapsEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for repo, paths := range a {
		left, right := slices.Clone(paths), slices.Clone(b[repo])
		slices.Sort(left)
		slices.Sort(right)
		if !slices.Equal(left, right) {
			return false
		}
	}
	return true
}

func validatePersistedTrigger(cfg *api.GitHubChannelConfig) error {
	if cfg.Trigger == nil {
		return nil
	}
	raw, err := json.Marshal(handlerArguments{Trigger: cfg.Trigger})
	if err != nil {
		return fmt.Errorf("encode trigger: %w", err)
	}
	if _, err := decodeHandlerConfig(raw); err != nil {
		return err
	}
	if len(cfg.Events) > 0 {
		return fmt.Errorf("workflow_run trigger must not configure legacy pull_request events")
	}
	if len(cfg.Trigger.Workflows) == 0 {
		return fmt.Errorf("workflow_run trigger requires workflow paths for every selected repository")
	}
	if len(cfg.Repositories) > 0 {
		if len(cfg.Repositories) != len(cfg.Trigger.Workflows) {
			return fmt.Errorf("repositories and trigger.workflows must select the same repositories")
		}
		seen := map[string]bool{}
		for _, repo := range cfg.Repositories {
			if seen[repo] || len(cfg.Trigger.Workflows[repo]) == 0 {
				return fmt.Errorf("repository %q has no matching workflow paths or is duplicated", repo)
			}
			seen[repo] = true
		}
	}
	return nil
}

func workflowSessionOwner(in channelkinds.WizardInput, answers map[string]string) (*api.ChannelOwnerPolicy, error) {
	value := strings.TrimSpace(answers[keySessionOwnerPermission])
	if value == "" && in.Existing != nil && in.Existing.Spec.Owner != nil {
		return in.Existing.Spec.Owner.DeepCopy(), nil
	}
	kind, _, _ := strings.Cut(value, ":")
	if err := authz.ValidateSubjectSet(value, authz.SubjectType(kind)); err != nil || !strings.Contains(value, "#") {
		return nil, fmt.Errorf("github %s must name a subject set such as group:ci-operators#member", keySessionOwnerPermission)
	}
	return &api.ChannelOwnerPolicy{Ownerless: &api.ChannelOwnerlessSource{Permission: value}}, nil
}
