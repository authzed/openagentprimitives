package oap

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcpspec "github.com/authzed/openagentprimitives/pkg/tools/mcp/spec"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

// Finding is one problem found in a bundled AgentUI. Machine-shaped rather than
// a formatted string so a caller (an admind install preview) can render it
// without re-parsing prose.
type Finding struct {
	UI     string // the AgentUI's metadata.name
	Path   string // uicomponents' own path when it produced the finding, else the CR field
	Reason string

	// Informational marks a finding worth printing but not worth failing an
	// install over — today only an AgentUI naming an origin (MCPServer) the
	// bundle does not carry, legitimate when the target cluster already has it.
	// Every other Finding here is a real authoring defect and leaves this false.
	// `oap agent lint` gates on this field, never on the Reason text.
	Informational bool
}

func (f Finding) String() string {
	return fmt.Sprintf("AgentUI/%s %s: %s", f.UI, f.Path, f.Reason)
}

// LintAgentUIs validates every AgentUI a bundle carries using the SAME
// functions pkg/controllers/agentui's reconciler calls — uigrant.Ceiling,
// uigrant.ExplainCeiling, uigrant.UnrequestedActionTools (Gate A), and
// uicomponents.Validate through uiview.DeclarationFromSpec (Gate B plus every
// node/prop/binding rule) — never a hand-rewritten copy of any of them. Gate A
// runs before Validate, the same sequence Reconcile uses, so an authoring
// mistake that trips both is reported once under Gate A's more specific message.
//
// It ALSO checks three things the reconciler structurally cannot, because they
// depend on an MCPServer reached only through an AgentClass's spec.mcpServers
// ref that the namespace-scoped reconciler has no reason to read:
//
//   - every tool in spec.tools decomposes as "<prefix>_<upstream>", where
//     <prefix> is a bundled AgentClass's spec.mcpServers[i].name referencing
//     this AgentUI and <upstream> is a tool that MCPServer declares;
//   - that MCPServer opted in (spec.mcpUiAppTools.enabled) and marked the tool
//     app-visible (visibility contains "app" and not "model" — a tool declaring
//     both stays LLM-visible and never reaches the browser registry);
//   - spec.tools[j].args.allowedFields covers every top-level argument key the
//     UI's binding and action args templates can send that tool. Top-level keys
//     are exactly what arrives: uibindings.SubstituteParams replaces VALUES and
//     never adds a KEY, so a shallow walk is correct and a deep one would be
//     wrong. Empty/unset allowedFields DENIES every argument — the static analog
//     of pkg/tools/mcp/validator's checkAllowedFields at call time — so an unset
//     list is a finding, not a default read as allow-all.
//
// An AgentUI naming an MCPServer the bundle does not carry yields an
// INFORMATIONAL finding, never an error: a bundle may legitimately expect a
// pre-existing origin on the target cluster. It is still reported, because
// silence there is indistinguishable from "checked and fine".
//
// A nil bundle, an undecodable manifest stream, or an AgentUI/AgentClass/
// MCPServer CR whose bytes will not decode into its typed v1alpha1 shape is a
// returned error — never a skip that reads as clean.
//
// Bundle.Validate neither calls this nor is called by it, so an authoring
// mistake here stays advisory rather than becoming an install-blocking error;
// see the doc note in cmd/oap/internal/agentcmd/lint.go for why.
func LintAgentUIs(b *Bundle) ([]Finding, error) {
	if b == nil {
		return nil, fmt.Errorf("oap: LintAgentUIs: nil bundle")
	}
	crs, err := b.CRs()
	if err != nil {
		return nil, fmt.Errorf("oap: LintAgentUIs: decode bundled manifests: %w", err)
	}

	var uis []*spiceboxv1alpha1.AgentUI
	var classes []*spiceboxv1alpha1.AgentClass
	mcpServers := map[string]*spiceboxv1alpha1.MCPServer{}
	for _, cr := range crs {
		switch cr.GetKind() {
		case "AgentUI":
			var ui spiceboxv1alpha1.AgentUI
			if err := DecodeBundledCR(cr, &ui); err != nil {
				return nil, fmt.Errorf("oap: LintAgentUIs: AgentUI %q: %w", cr.GetName(), err)
			}
			uis = append(uis, &ui)
		case "AgentClass":
			var ac spiceboxv1alpha1.AgentClass
			if err := DecodeBundledCR(cr, &ac); err != nil {
				return nil, fmt.Errorf("oap: LintAgentUIs: AgentClass %q: %w", cr.GetName(), err)
			}
			classes = append(classes, &ac)
		case "MCPServer":
			var m spiceboxv1alpha1.MCPServer
			if err := DecodeBundledCR(cr, &m); err != nil {
				return nil, fmt.Errorf("oap: LintAgentUIs: MCPServer %q: %w", cr.GetName(), err)
			}
			mcpServers[m.Name] = &m
		}
	}

	var findings []Finding
	for _, ui := range uis {
		fs, err := lintOneAgentUI(ui, classes, mcpServers)
		if err != nil {
			return nil, err
		}
		findings = append(findings, fs...)
	}
	return findings, nil
}

// DecodeBundledCR decodes one bundled CR's bytes into a typed v1alpha1 struct
// via a JSON round-trip rather than runtime.DefaultUnstructuredConverter's
// reflective path: several fields read here (AgentUISlot.Default and
// AgentUIAction.Args's *apiextensionsv1.JSON, MCPServerSpec.CallTimeout's
// metav1.Duration) carry their own Marshal/UnmarshalJSON, and encoding/json is
// the one interpretation guaranteed to run them — the same one the apiserver's
// REST codec gives them.
//
// Exported so a sibling package linting the same bundle (oap/channelplan,
// which decodes bundled AgentIdentities) gets that interpretation rather than
// a second copy of this round-trip that could drift to the reflective one.
func DecodeBundledCR(cr *unstructured.Unstructured, out any) error {
	raw, err := json.Marshal(cr.Object)
	if err != nil {
		return fmt.Errorf("re-encode: %w", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// mcpOrigin is one AgentClass.spec.mcpServers entry that references THIS
// AgentUI's owning class(es): the LLM-prefix a bundle author chose plus the
// MCPServer CR name it points at.
type mcpOrigin struct {
	prefix string // AgentClassMCPServerRef.Name, the LLM-prefix
	ref    string // AgentClassMCPServerRef.Ref, the MCPServer CR name
}

// lintOneAgentUI runs every check for one AgentUI: the reconciler-mirroring
// ones (ExplainCeiling, Gate A, Validate, in that order) plus the three
// lint-only checks over the tool vocabulary. Findings accumulate rather than
// stopping at the first, so an author sees every problem in one pass.
func lintOneAgentUI(ui *spiceboxv1alpha1.AgentUI, classes []*spiceboxv1alpha1.AgentClass, mcpServers map[string]*spiceboxv1alpha1.MCPServer) ([]Finding, error) {
	var findings []Finding

	// The union of the DEPLOYMENT half of the grant, plus the origins this UI's
	// referencing AgentClass(es) declare — mirrors the reconciler's
	// unionDeploymentGrants, computed from the bundle's bytes, not a List call.
	var granted []string
	origins := map[string]mcpOrigin{}
	for _, ac := range classes {
		if ac.Spec.AgentUI == nil || ac.Spec.AgentUI.Ref != ui.Name {
			continue
		}
		granted = append(granted, ac.Spec.AgentUI.GrantedTools...)
		for _, ref := range ac.Spec.MCPServers {
			origins[synthesize.NormalizeName(ref.Name)] = mcpOrigin{prefix: ref.Name, ref: ref.Ref}
		}
	}

	// The ToolsGranted-equivalent: uigrant.ExplainCeiling is the exact function
	// pkg/controllers/agentui's setToolsGrantedCondition calls to diagnose a
	// request the ceiling does not cover. Sorted by tool name for determinism.
	explanation := uigrant.ExplainCeiling(ui.Spec.Tools, granted)
	for _, tool := range slices.Sorted(maps.Keys(explanation)) {
		findings = append(findings, Finding{UI: ui.Name, Path: "spec.tools", Reason: explanation[tool]})
	}

	eligible := uigrant.Ceiling(ui.Spec.Tools, granted)
	eligibleSet := make(map[string]bool, len(eligible))
	for _, t := range eligible {
		eligibleSet[t] = true
	}

	decl, err := uiview.DeclarationFromSpec(ui)
	if err != nil {
		return nil, fmt.Errorf("oap: LintAgentUIs: AgentUI %q: %w", ui.Name, err)
	}

	// Gate A, then Validate — the SAME sequence Reconcile uses: a tool Gate A
	// flagged as unrequested is by construction also missing from eligibleSet,
	// so running Validate anyway would report the same mistake a second time
	// under a more generic "tool is not granted" message.
	unrequested := uigrant.UnrequestedActionTools(ui.Spec.Tools, actionToolNames(ui.Spec.Actions), synthesize.NormalizeName)
	if len(unrequested) > 0 {
		findings = append(findings, Finding{UI: ui.Name, Path: "spec.actions", Reason: formatUnrequestedActionTools(unrequested)})
	} else {
		opts := uicomponents.DefaultOptions()
		opts.GrantedTools = eligibleSet
		opts.ReadonlyTools = eligibleSet
		opts.NormalizeToolName = synthesize.NormalizeName
		if verr := uicomponents.Validate(decl, opts); verr != nil {
			var ve *uicomponents.ValidationError
			if errors.As(verr, &ve) {
				findings = append(findings, Finding{UI: ui.Name, Path: ve.Path, Reason: ve.Reason})
			} else {
				findings = append(findings, Finding{UI: ui.Name, Path: "", Reason: verr.Error()})
			}
		}
	}

	// The three lint-only checks, independent of whether Gate A/Validate
	// found anything above — they inspect a DIFFERENT object (the MCPServer
	// an origin points at), which the reconciler never reads at all.
	argKeys, err := collectArgKeysByTool(decl, synthesize.NormalizeName)
	if err != nil {
		return nil, fmt.Errorf("oap: LintAgentUIs: AgentUI %q: %w", ui.Name, err)
	}
	findings = append(findings, lintToolVocabulary(ui, origins, mcpServers, argKeys)...)

	return findings, nil
}

// actionToolNames collects each action's own Tool in declaration order,
// duplicates and all — uigrant.UnrequestedActionTools dedupes. Mirrors
// pkg/controllers/agentui's private copy: a plain projection of CRD fields, not
// a rule, so an independent copy carries none of the drift risk a rule would.
func actionToolNames(actions []spiceboxv1alpha1.AgentUIAction) []string {
	if len(actions) == 0 {
		return nil
	}
	// An action with no Tool is a PROMPT action — it asks the agent rather than
	// calling anything. Emitting its empty Tool made the gate report a missing
	// grant for the tool `""`, which nothing added to spec.tools could satisfy.
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		if a.Tool == "" {
			continue
		}
		out = append(out, a.Tool)
	}
	return out
}

// formatUnrequestedActionTools renders Gate A's author-facing message. Mirrors
// pkg/controllers/agentui's private wording — duplicated because that function
// is unexported, not because uigrant.UnrequestedActionTools is reimplemented.
func formatUnrequestedActionTools(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = strconv.Quote(t)
	}
	return fmt.Sprintf(
		"actions naming tools that spec.tools does not request: %s. Add each to spec.tools — only tools listed there become callable from this UI.",
		strings.Join(quoted, ", "))
}

// collectArgKeysByTool walks every tool-sourced data binding plus every declared
// action's args template and returns, per NORMALIZED tool ref, the sorted,
// deduplicated top-level argument keys the UI could ever send. Top-level only:
// uibindings.SubstituteParams replaces a placeholder VALUE and never adds a KEY,
// so a deep walk would over-collect keys the tool never sees as an argument.
func collectArgKeysByTool(decl uicomponents.Declaration, normalize func(string) string) (map[string][]string, error) {
	sets := map[string]map[string]struct{}{}
	add := func(ref string, args json.RawMessage) error {
		if len(args) == 0 {
			return nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(args, &m); err != nil {
			return fmt.Errorf("args template for tool %q is not a JSON object: %w", ref, err)
		}
		norm := ref
		if normalize != nil {
			norm = normalize(ref)
		}
		if sets[norm] == nil {
			sets[norm] = map[string]struct{}{}
		}
		for k := range m {
			sets[norm][k] = struct{}{}
		}
		return nil
	}
	for _, bp := range uicomponents.WalkBindings(decl) {
		if bp.Binding.Source != "tool" {
			continue
		}
		if err := add(bp.Binding.Ref, bp.Binding.Args); err != nil {
			return nil, err
		}
	}
	for _, a := range decl.Actions {
		if err := add(a.Tool, a.Args); err != nil {
			return nil, err
		}
	}
	out := make(map[string][]string, len(sets))
	for ref, set := range sets {
		out[ref] = slices.Sorted(maps.Keys(set))
	}
	return out, nil
}

// lintToolVocabulary runs the three lint-only checks over every tool in
// ui.Spec.Tools. Gate A and Validate already require every binding and action
// tool to be a member of that list, so checking it alone covers every tool the
// document could name.
func lintToolVocabulary(ui *spiceboxv1alpha1.AgentUI, origins map[string]mcpOrigin, mcpServers map[string]*spiceboxv1alpha1.MCPServer, argKeys map[string][]string) []Finding {
	var findings []Finding
	seen := map[string]struct{}{}
	for _, t := range ui.Spec.Tools {
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		findings = append(findings, lintOneTool(ui.Name, t, origins, mcpServers, argKeys[t])...)
	}
	return findings
}

// lintOneTool decomposes tool (a spec.Tools entry, already in the NormalizeName
// alphabet per the CRD pattern) against origins — the mcpServers[i].name every
// AgentClass referencing this UI declares — and, on a match, checks the
// MCPServer that prefix points at for app-visibility, the mcpUiAppTools opt-in,
// and allowedFields coverage. origins is iterated in sorted key order so an
// ambiguous match (one declared prefix a literal prefix of another) resolves
// the same way every run.
func lintOneTool(uiName, tool string, origins map[string]mcpOrigin, mcpServers map[string]*spiceboxv1alpha1.MCPServer, keys []string) []Finding {
	for _, normPrefix := range slices.Sorted(maps.Keys(origins)) {
		o := origins[normPrefix]
		full := normPrefix + "_"
		if !strings.HasPrefix(tool, full) {
			continue
		}

		mcp, present := mcpServers[o.ref]
		if !present {
			return []Finding{{
				UI:            uiName,
				Path:          "spec.tools",
				Reason:        fmt.Sprintf("tool %q names origin %q (MCPServer/%s), which is not carried by this bundle; the target cluster must already have it installed", tool, o.prefix, o.ref),
				Informational: true,
			}}
		}

		var found *spiceboxv1alpha1.MCPServerTool
		for i := range mcp.Spec.Tools {
			if synthesize.NormalizeName(o.prefix+"_"+mcp.Spec.Tools[i].Name) == tool {
				found = &mcp.Spec.Tools[i]
				break
			}
		}
		if found == nil {
			return []Finding{{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
				"MCPServer/%s does not expose a tool that normalizes to %q", o.ref, tool)}}
		}

		var out []Finding
		appVisible := slices.Contains(found.Visibility, "app") && !slices.Contains(found.Visibility, "model")
		if !appVisible {
			out = append(out, Finding{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
				"tool %q is not app-visible on MCPServer/%s (spec.tools[].visibility must include \"app\" and not \"model\"); it would go to the LLM, never the browser",
				tool, o.ref)})
		}
		if mcp.Spec.MCPUIAppTools == nil || !mcp.Spec.MCPUIAppTools.Enabled {
			out = append(out, Finding{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
				"MCPServer/%s has not opted into spec.mcpUiAppTools.enabled; none of its tools are browser-callable",
				o.ref)})
		}
		if len(keys) > 0 && !found.Args.UnconstrainedArgs {
			allowed := make(map[string]bool, len(found.Args.AllowedFields))
			for _, f := range found.Args.AllowedFields {
				allowed[f] = true
			}
			// The rule itself lives at the gate that enforces it
			// (mcpspec.RefusesEveryArgument); this only knows that arguments ARE
			// going to be sent, which is what makes the refusal a finding here.
			if mcpspec.RefusesEveryArgument(found.Args.AllowedFields, found.Args.UnconstrainedArgs) {
				out = append(out, Finding{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
					"MCPServer/%s tool %q has no allowedFields configured; it denies every argument the UI's args templates would send (%s) — set allowedFields or unconstrainedArgs",
					o.ref, found.Name, strings.Join(keys, ", "))})
			} else {
				var missing []string
				for _, k := range keys {
					if !allowed[k] {
						missing = append(missing, strconv.Quote(k))
					}
				}
				if len(missing) > 0 {
					out = append(out, Finding{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
						"MCPServer/%s tool %q: allowedFields %v does not cover %s sent by the UI's args templates",
						o.ref, found.Name, found.Args.AllowedFields, strings.Join(missing, ", "))})
				}
			}
		}
		return out
	}
	return []Finding{{UI: uiName, Path: "spec.tools", Reason: fmt.Sprintf(
		"tool %q names a prefix no bundled AgentClass declares for this UI", tool)}}
}
