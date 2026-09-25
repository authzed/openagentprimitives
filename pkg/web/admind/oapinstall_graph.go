package admind

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/channelplan"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
	"github.com/authzed/openagentprimitives/pkg/tools/redact"
)

// handleOapInstall keeps authorization and channel-setup ownership around one
// request, then delegates all graph traversal, preparation, apply ordering,
// and rollback to install.Workflow.
func (a *Admind) handleOapInstall(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOapInstallBody)
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	owner, ok := a.channelSetupOwner(w, r)
	if !ok {
		return
	}
	a.handleOapInstallGraph(w, r, owner)
}

func (a *Admind) handleOapInstallGraph(w http.ResponseWriter, r *http.Request, owner identity.CanonicalUserID) {
	params, err := parseOapInstallRequest(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateInstallTarget(params.Namespace, params.Name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	bundle, sourceKind, sourceRef, sourceDigest, err := loadOapInstallBundle(r.Context(), params.FileBytes, params.Ref, params.PlainHTTP)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "load bundle: "+err.Error())
		return
	}
	var channelBinding *channelSetupBinding
	if len(bundle.Dependencies) > 0 {
		rootInstall := params.Name
		if rootInstall == "" && bundle.Manifest != nil {
			rootInstall = bundle.Manifest.Agent.Name
		}
		channelBinding = &channelSetupBinding{
			RootDigest: sourceDigest, RootNamespace: params.Namespace, RootInstall: rootInstall,
		}
	}
	var clones []oap.SkillClone
	sets, err := install.ParseGraphSet(params.Values, bundle)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, redactPostedValues(err.Error(), params.Values))
		return
	}
	// An empty capacity field means the operator cleared the browser input and
	// wants to be asked again. Keep ordinary empty-string answers intact, but
	// remove reserved capacity answers before Resolve validates them.
	for _, nodeSets := range sets {
		for name, value := range nodeSets {
			if strings.HasPrefix(name, oap.ReservedQuestionPrefix) && value == "" {
				delete(nodeSets, name)
			}
		}
	}

	channelRows := map[string][]oapInstallChannel{}
	formWarnings := map[string][]string{}
	capacityDefaults := map[string]map[string]any{}
	adoption := newAdminAdoptionTracker(params.Adopt)
	capacity := a.newCapacityQuestions(r.Context())
	workflow := &install.Workflow{
		Client: a.cfg.K8s,
		Options: install.InstallOpts{
			Name: params.Name, Namespace: params.Namespace,
			SourceKind: sourceKind, SourceRef: sourceRef, SourceDigest: sourceDigest,
			Interactive: false,
		},
		AggregateDecisions: true,
		Hooks: install.WorkflowHooks{
			InspectNode: func(_ context.Context, node install.NodeContext) error {
				instanceName := ""
				if len(node.Path) == 0 {
					instanceName = params.Name
				}
				if err := channelplan.CheckDeclared(node.Bundle, instanceName); err != nil {
					return newAdminInspectionError(bundle, node.Path, err)
				}
				nodeClones, err := node.Bundle.SkillClones()
				if err != nil {
					return newAdminInspectionError(bundle, node.Path, fmt.Errorf("inspect bundled skills: %w", err))
				}
				clones = append(clones, nodeClones...)
				return nil
			},
			CapacityQuestions: func(ctx context.Context, node install.NodeContext, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
				questions, notices, err := capacity(ctx, crs)
				pathKey := node.Path.String()
				formWarnings[pathKey] = append(formWarnings[pathKey], notices...)
				// Admind has a form, so a capacity clamp remains an explicit
				// operator choice rather than silently consuming its suggestion.
				// Save that suggestion for the response before clearing it from the
				// copy Resolve sees, so the UI can still pre-fill the field.
				for i := range questions {
					if questions[i].Default != nil {
						if capacityDefaults[pathKey] == nil {
							capacityDefaults[pathKey] = map[string]any{}
						}
						capacityDefaults[pathKey][questions[i].Name] = questions[i].Default
					}
					questions[i].Default = nil
				}
				return questions, notices, err
			},
			PlanChannels: func(ctx context.Context, node install.NodeContext) (install.PlannedChannels, error) {
				if channelBinding != nil && len(node.Path) == 0 {
					// The Workflow owns physical naming. Bind every graph token to
					// the root identity it resolved instead of re-deriving it here.
					channelBinding.RootInstall = node.RootInstallName
				}
				planned, rows, warnings, err := a.declaredChannelFormForNode(ctx, node, owner, channelBinding)
				if err != nil {
					return install.PlannedChannels{}, err
				}
				channelRows[node.Path.String()] = rows
				formWarnings[node.Path.String()] = append(formWarnings[node.Path.String()], warnings...)
				return planned, nil
			},
			ResolveChannels: func(ctx context.Context, _ install.NodeContext, planned install.PlannedChannels) error {
				return planned.Resolve(ctx)
			},
			ApplyChannels: func(ctx context.Context, _ install.NodeContext, planned install.PlannedChannels) error {
				return planned.Apply(ctx)
			},
			AdoptDecision: adoption.decide,
		},
	}

	plan, err := workflow.Plan(r.Context(), bundle, install.GraphAnswers{Sets: sets, Interactive: false})
	if err != nil {
		var decisions *install.DecisionError
		if !errors.As(err, &decisions) {
			a.writeAdminGraphPlanError(w, params, clones, channelRows, formWarnings, capacityDefaults, err)
			return
		}
		if adoptionErr := adoption.validate(); adoptionErr != nil {
			writeJSONError(w, http.StatusBadRequest, adoptionErr.Error())
			return
		}
		a.writeAdminGraphPlanError(w, params, clones, channelRows, formWarnings, capacityDefaults, err)
		return
	}
	if adoptionErr := adoption.validate(); adoptionErr != nil {
		writeJSONError(w, http.StatusBadRequest, adoptionErr.Error())
		return
	}
	result, err := workflow.Execute(r.Context(), plan)
	if err != nil {
		a.cfg.Logger.Info("admind: oap graph install failed",
			"namespace", params.Namespace, "name", params.Name, "sourceKind", sourceKind, "sourceRef", sourceRef,
			"err", redactPostedValues(err.Error(), params.Values))
		writeJSONError(w, http.StatusInternalServerError, "install failed: "+redactPostedValues(err.Error(), params.Values))
		return
	}
	response := adminGraphResponse(result, clones, channelRows, formWarnings)
	for _, node := range result.Nodes {
		if node.Result == nil || len(node.Result.Adopted) == 0 {
			continue
		}
		a.cfg.Logger.Info("admind: oap install adopted pre-existing objects",
			"namespace", params.Namespace, "name", node.Name, "agentPath", node.Path.String(), "adopted", node.Result.Adopted)
	}
	writeJSON(w, http.StatusOK, response)
}

func adminGraphPath(root *oap.Bundle, path oap.DependencyPath) string {
	name := "root"
	if root != nil && root.Manifest != nil && root.Manifest.Agent.Name != "" {
		name = root.Manifest.Agent.Name
	}
	if len(path) == 0 {
		return name
	}
	return name + " > " + path.String()
}

func (a *Admind) writeAdminGraphPlanError(
	w http.ResponseWriter,
	params *oapInstallParams,
	clones []oap.SkillClone,
	channelRows map[string][]oapInstallChannel,
	formWarnings map[string][]string,
	capacityDefaults map[string]map[string]any,
	err error,
) {
	var inspection *adminInspectionError
	if errors.As(err, &inspection) {
		writeJSONError(w, http.StatusBadRequest, inspection.message)
		return
	}
	var decisions *install.DecisionError
	if errors.As(err, &decisions) {
		var questions []oap.Question
		var questionPaths []string
		var conflicts []oapInstallConflict
		for _, decision := range decisions.Missing {
			for _, question := range decision.Questions {
				if value, ok := capacityDefaults[decision.Path.String()][question.Name]; ok {
					question.Default = value
				}
				questions = append(questions, question)
				questionPaths = append(questionPaths, decision.Path.String())
			}
		}
		for _, decision := range decisions.Conflicts {
			conflicts = append(conflicts, toWireConflictsAtPath(decision.Conflicts, decision.Path.String())...)
		}
		if len(questions) > 0 || len(decisions.ChannelPaths) > 0 {
			errorMessage := "missing required question(s)"
			if len(questions) == 0 {
				errorMessage = "required channel setup is incomplete"
			}
			writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
				Error: errorMessage, Questions: questions, questionPaths: questionPaths,
				SkillClones: clones, Channels: flattenChannelRows(channelRows), Warnings: flattenGraphWarnings(formWarnings), Conflicts: conflicts,
			})
			return
		}
		a.cfg.Logger.Info("admind: oap install blocked by pre-existing objects",
			"namespace", params.Namespace, "name", params.Name, "conflicts", len(conflicts))
		writeJSON(w, http.StatusConflict, oapInstallConflictsResponse{
			Error: "install would overwrite pre-existing object(s)", Conflicts: conflicts, SkillClones: clones,
			Channels: flattenChannelRows(channelRows), Warnings: flattenGraphWarnings(formWarnings),
		})
		return
	}
	path, _ := install.DependencyPathFromError(err)
	pathKey := path.String()
	var missing *install.MissingAnswersError
	if errors.As(err, &missing) {
		questions := slices.Clone(missing.Questions)
		for i := range questions {
			if value, ok := capacityDefaults[pathKey][questions[i].Name]; ok {
				questions[i].Default = value
			}
		}
		paths := make([]string, len(missing.Questions))
		for i := range paths {
			paths[i] = pathKey
		}
		writeJSON(w, http.StatusBadRequest, oapInstallMissingQuestionsResponse{
			Error: "missing required question(s)", Questions: questions, questionPaths: paths,
			SkillClones: clones, Channels: flattenChannelRows(channelRows), Warnings: flattenGraphWarnings(formWarnings),
		})
		return
	}
	var conflicts *install.ConflictError
	if errors.As(err, &conflicts) {
		a.cfg.Logger.Info("admind: oap install blocked by pre-existing objects",
			"namespace", params.Namespace, "name", params.Name, "agentPath", pathKey, "conflicts", len(conflicts.Conflicts))
		writeJSON(w, http.StatusConflict, oapInstallConflictsResponse{
			Error:     "install would overwrite pre-existing object(s)",
			Conflicts: toWireConflictsAtPath(conflicts.Conflicts, pathKey), SkillClones: clones,
			Channels: flattenChannelRows(channelRows), Warnings: flattenGraphWarnings(formWarnings),
		})
		return
	}
	writeJSONError(w, http.StatusBadRequest, redactPostedValues(err.Error(), params.Values))
}

func flattenChannelRows(byPath map[string][]oapInstallChannel) []oapInstallChannel {
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var rows []oapInstallChannel
	for _, path := range paths {
		rows = append(rows, byPath[path]...)
	}
	return rows
}

func flattenGraphWarnings(byPath map[string][]string) []string {
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var warnings []string
	for _, path := range paths {
		for _, warning := range byPath[path] {
			if path == "" {
				warnings = append(warnings, warning)
			} else {
				warnings = append(warnings, "install "+path+": "+warning)
			}
		}
	}
	return warnings
}

func adminGraphResponse(result *install.GraphResult, clones []oap.SkillClone, channelRows map[string][]oapInstallChannel, formWarnings map[string][]string) oapInstallResponse {
	if result == nil {
		return oapInstallResponse{}
	}
	byPath := make(map[string]install.NodeResult, len(result.Nodes))
	children := make(map[string][]install.NodeResult)
	for _, node := range result.Nodes {
		byPath[node.Path.String()] = node
		parent := ""
		if len(node.Path) > 0 {
			parent = oap.DependencyPath(node.Path[:len(node.Path)-1]).String()
			children[parent] = append(children[parent], node)
		}
	}
	var build func(install.NodeResult) oapInstallResponse
	build = func(node install.NodeResult) oapInstallResponse {
		response := oapInstallResponse{
			AgentPath: node.Path.String(), Name: node.Name, Channels: slices.Clone(channelRows[node.Path.String()]),
			Warnings: appendUniqueWarnings(nil, formWarnings[node.Path.String()]...),
		}
		if node.Result != nil {
			response.Name = node.Result.Name
			response.AppliedKinds = slices.Clone(node.Result.AppliedKinds)
			response.SecretsCreated = node.Result.SecretsCreated
			response.Warnings = appendUniqueWarnings(response.Warnings, node.Result.Warnings...)
			response.Adopted = slices.Clone(node.Result.Adopted)
		}
		next := slices.Clone(children[node.Path.String()])
		sort.SliceStable(next, func(i, j int) bool { return next[i].Path.String() < next[j].Path.String() })
		for _, child := range next {
			response.Agents = append(response.Agents, build(child))
		}
		return response
	}
	root, ok := byPath[""]
	if !ok {
		return oapInstallResponse{}
	}
	response := build(root)
	response.SkillClones = slices.Clone(clones)
	response.Warnings = appendUniqueWarnings(response.Warnings, result.Notices...)
	return response
}

func appendUniqueWarnings(dst []string, values ...string) []string {
	seen := make(map[string]bool, len(dst)+len(values))
	for _, warning := range dst {
		seen[warning] = true
	}
	for _, warning := range values {
		if warning == "" || seen[warning] {
			continue
		}
		seen[warning] = true
		dst = append(dst, warning)
	}
	return dst
}

type adminInspectionError struct{ message string }

func (e *adminInspectionError) Error() string { return e.message }

func newAdminInspectionError(root *oap.Bundle, path oap.DependencyPath, err error) error {
	if len(path) == 0 {
		return &adminInspectionError{message: err.Error()}
	}
	return &adminInspectionError{message: fmt.Sprintf("install %s: %v", adminGraphPath(root, path), err)}
}

type adminAdoptionTracker struct {
	requested map[string]bool
	matched   map[string]bool
}

func newAdminAdoptionTracker(keys []string) *adminAdoptionTracker {
	requested := make(map[string]bool, len(keys))
	for _, key := range keys {
		requested[key] = true
	}
	return &adminAdoptionTracker{requested: requested, matched: map[string]bool{}}
}

func (t *adminAdoptionTracker) decide(_ context.Context, _ install.NodeContext, conflicts []install.Conflict) ([]string, error) {
	selected := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		if !t.requested[conflict.Key()] {
			continue
		}
		t.matched[conflict.Key()] = true
		if conflict.Secret {
			return nil, fmt.Errorf("install: refusing to adopt %s from the admin surface: adopting a Secret overwrites its data; resolve it out of band or install from the CLI", conflict.Key())
		}
		selected = append(selected, conflict.Key())
	}
	return selected, nil
}

func (t *adminAdoptionTracker) validate() error {
	var stale []string
	for key := range t.requested {
		if !t.matched[key] {
			stale = append(stale, key)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	sort.Strings(stale)
	return fmt.Errorf("install: adopt names %v, which is not a conflicting object in this install graph", stale)
}

func redactPostedValues(message string, values map[string]string) string {
	r := redact.New()
	for key, value := range values {
		r.RegisterSensitive(value, redact.Descriptor{Description: "admin install answer", Kind: "form", Name: key})
	}
	return r.RedactInString(message)
}

func toWireConflictsAtPath(conflicts []install.Conflict, path string) []oapInstallConflict {
	wire := toWireConflicts(conflicts)
	for i := range wire {
		wire[i].AgentPath = path
	}
	return wire
}
