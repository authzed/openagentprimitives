// Package agentui reconciles AgentUI CRs: it admits the page — spec.view, or
// spec.slots compiled into one by the legacy shim — validating it in one
// pass against the platform component vocabulary (pkg/web/uicomponents),
// including every oap:generative hook's own contract (a unique name, a
// registered allowlist, no nesting), and observes both the resulting hook
// table and an eligibility CEILING (pkg/web/uigrant) onto status — see
// AgentUIStatus.EligibleTools's doc comment for exactly what that ceiling
// does and does not mean; it is not the full three-way grant and must not
// be read as an authorization decision.
//
// AgentUI itself carries only the bundle author's REQUEST (spec.tools) — see
// AgentUISpec's doc comment. The deployment half of the grant lives on
// AgentClass.spec.agentUI, a different object with a different writer, so the
// two halves never share a field manager. This controller reads BOTH objects
// but writes only AgentUI's own status: it is an observer of the deployment's
// decision, never the place that decision is made.
//
// Deliberately has NO finalizer. An AgentUI whose last referencing AgentClass
// is deleted (or edited to drop the ref) simply resolves to "no referencing
// class" on the next reconcile — the same fail-closed state as an AgentUI
// that was never referenced at all. Unlike SidecarToolbox (which guards a
// live probe/pin baseline), there is no runtime state here that deletion
// could strand, so there is nothing for a finalizer to protect.
package agentui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/synthesize"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uigrant"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentuis,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentuis/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=agentclasses,verbs=get;list;watch

// Reconciler reconciles AgentUI objects.
type Reconciler struct {
	Client client.Client
}

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agentui", req.NamespacedName)

	var cr spiceboxv1alpha1.AgentUI
	if err := r.Client.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	prior := cr.DeepCopy()

	// The union of the DEPLOYMENT half of the grant
	// (AgentClass.spec.agentUI.grantedTools) across every AgentClass
	// referencing this AgentUI.
	granted, resolveErr := r.unionDeploymentGrants(ctx, &cr)
	if resolveErr != nil {
		// A List failure means this reconcile never actually READ the grant
		// input, so it must not derive a verdict from it. status.eligibleTools
		// is NOT an authorization input (see its doc comment), so persisting a
		// denial computed against an empty stand-in would be a false diagnosis,
		// not a safe default: a healthy AgentUI would flip from
		// Valid=True/eligible=[…] to Valid=False/"tool not granted" on a purely
		// transient blip, sending an operator hunting a grant problem that does
		// not exist. So: leave status.eligibleTools, status.hooks, and the
		// ToolsGranted condition exactly as they were (the last
		// successfully-observed values), set Valid=Unknown naming the failure,
		// and return the error so this reconcile retries. status.hooks in
		// particular is untouched here rather than cleared: it was last
		// computed against a spec this reconcile never re-validated, so
		// wiping it would tell an operator the page lost every hook when
		// nothing about the page actually changed.
		logger.Info("resolve AgentClass grant failed; leaving eligibleTools untouched, reporting Valid=Unknown",
			"err", resolveErr.Error())
		conditions.Set(&cr, &cr.Status.Conditions, metav1.Condition{
			Type:    spiceboxv1alpha1.AgentUIConditionValid,
			Status:  metav1.ConditionUnknown,
			Reason:  spiceboxv1alpha1.ReasonAgentUIGrantUnresolved,
			Message: fmt.Sprintf("could not resolve the AgentClass grant: %v", resolveErr),
		})
		res, err := r.writeStatusIfChanged(ctx, &cr, prior)
		if err != nil {
			return res, err
		}
		return res, resolveErr
	}

	// status.eligibleTools is a CEILING, not the three-way grant
	// uigrant.Materialize computes — see AgentUIStatus.EligibleTools's doc
	// comment for the full reasoning. uigrant.Ceiling is the two-party
	// analog: it evaluates only the two conditions this namespace-scoped
	// controller can actually see (the UI's own request, and the union of
	// every referencing AgentClass's grant), leaving the SESSION-scoped
	// origin-permits condition to the runner, which alone has live-resolved
	// origin data at session start.
	eligibleTools := uigrant.Ceiling(cr.Spec.Tools, granted)
	cr.Status.EligibleTools = eligibleTools

	eligibleSet := make(map[string]bool, len(eligibleTools))
	for _, t := range eligibleTools {
		eligibleSet[t] = true
	}

	// Gate A runs BEFORE validateDeclaration, and reports a DIFFERENT reason,
	// so it wins over validateActions' generic "tool is not granted to this
	// UI" for the likeliest authoring mistake: an action's tool that was
	// granted but never requested via spec.Tools. See
	// uigrant.UnrequestedActionTools' doc comment for the reachability chain
	// proving such a tool is dead on every click regardless of the grant.
	if unrequested := uigrant.UnrequestedActionTools(cr.Spec.Tools, actionToolNames(cr.Spec.Actions), synthesize.NormalizeName); len(unrequested) > 0 {
		conditions.SetFalse(&cr, &cr.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid,
			spiceboxv1alpha1.ReasonAgentUIActionToolNotRequested, formatUnrequestedActionTools(unrequested))
		cr.Status.Hooks = nil
	} else if decl, verr := validateDeclaration(cr.Spec, eligibleSet); verr != nil {
		conditions.SetFalse(&cr, &cr.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid,
			spiceboxv1alpha1.ReasonAgentUIInvalidDefault, verr.Error())
		cr.Status.Hooks = nil
	} else {
		conditions.SetTrue(&cr, &cr.Status.Conditions, spiceboxv1alpha1.AgentUIConditionValid,
			spiceboxv1alpha1.ReasonAgentUISpecOK)
		cr.Status.Hooks = hooksFor(decl)
	}

	// ToolsGranted is a SEPARATE fact from Valid: whether the ceiling covers
	// every tool the UI itself asked for. Gives uigrant.ExplainCeiling an
	// actual caller instead of a bundle that requests an ungranted tool
	// producing no condition, no event, nothing.
	setToolsGrantedCondition(&cr, cr.Spec.Tools, granted)

	return r.writeStatusIfChanged(ctx, &cr, prior)
}

// writeStatusIfChanged patches AgentUI's status only when it differs from
// prior — AGENTS.md's only-changed-writes rule: a byte-identical status is
// never re-sent, so the object is not churned (and watchers not
// re-triggered) on every resync when nothing actually changed.
func (r *Reconciler) writeStatusIfChanged(ctx context.Context, cr, prior *spiceboxv1alpha1.AgentUI) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(prior.Status, cr.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Client.Status().Patch(ctx, cr, client.MergeFrom(prior)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch AgentUI %s/%s status: %w", cr.Namespace, cr.Name, err)
	}
	return ctrl.Result{}, nil
}

// setToolsGrantedCondition surfaces uigrant.ExplainCeiling's diagnosis as the
// ToolsGranted condition whenever the eligible-tools ceiling does not cover
// every tool the UI itself requested.
func setToolsGrantedCondition(cr *spiceboxv1alpha1.AgentUI, requested, granted []string) {
	explanation := uigrant.ExplainCeiling(requested, granted)
	if len(explanation) == 0 {
		conditions.SetTrue(cr, &cr.Status.Conditions, spiceboxv1alpha1.AgentUIConditionToolsGranted,
			spiceboxv1alpha1.ReasonAgentUIToolsFullyGranted)
		return
	}
	conditions.SetFalse(cr, &cr.Status.Conditions, spiceboxv1alpha1.AgentUIConditionToolsGranted,
		spiceboxv1alpha1.ReasonAgentUIToolsPartiallyGranted, formatExplanation(explanation))
}

// formatExplanation joins an ExplainCeiling result into one deterministic
// message. Map iteration order is NOT deterministic in Go, so the keys are
// sorted first — an unsorted join would make the condition's Message differ
// run to run for identical inputs, defeating writeStatusIfChanged's
// only-changed-writes comparison and churning the object on every reconcile.
func formatExplanation(explanation map[string]string) string {
	keys := make([]string, 0, len(explanation))
	for k := range explanation {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, explanation[k])
	}
	return strings.Join(parts, "; ")
}

// unionDeploymentGrants unions GrantedTools across every AgentClass in the
// same namespace whose spec.agentUI.ref names cr. The union — not an
// arbitrary pick — is SOUND IN THE REJECT DIRECTION for a ceiling: a tool
// absent from every referencing class's GrantedTools is one that NO class
// could ever authorize, so excluding it from status.eligibleTools (and
// therefore rejecting a Tier-0 default's binding to it) is never a false
// negative. This union would be the WRONG choice if this value were itself
// an authorization decision — which is exactly why it isn't one; see
// AgentUIStatus.EligibleTools's doc comment.
func (r *Reconciler) unionDeploymentGrants(ctx context.Context, cr *spiceboxv1alpha1.AgentUI) ([]string, error) {
	var classes spiceboxv1alpha1.AgentClassList
	if err := r.Client.List(ctx, &classes, client.InNamespace(cr.Namespace)); err != nil {
		return nil, fmt.Errorf("list AgentClasses: %w", err)
	}
	var granted []string
	for _, ac := range classes.Items {
		if ac.Spec.AgentUI != nil && ac.Spec.AgentUI.Ref == cr.Name {
			granted = append(granted, ac.Spec.AgentUI.GrantedTools...)
		}
	}
	return granted, nil
}

// wireDeclaration wraps a single raw JSON node — spec.View, or one slot's
// Default — as {"view": <node>} so it strict-decodes through ParseDeclaration
// exactly like an author-submitted page does, instead of unmarshalling into
// uicomponents.Node directly (which would silently accept a typo'd
// structural key inside it rather than rejecting it). See
// validateDeclaration's doc comment for why the per-slot path wraps a slot's
// Default alone rather than a whole legacy slots document — that choice is
// also why this type carries no Slots field: nothing this package builds
// ever needs one.
type wireDeclaration struct {
	// The page tree — spec.View verbatim, or one slot's Default wrapped alone.
	View json.RawMessage `json:"view,omitempty"`
}

// actionsToDeclaration converts the CRD's typed AgentUIAction list into
// uicomponents.Action, field-for-field. Unlike a slot's Default, an
// AgentUIAction's Name/Tool/Inputs are already typed Go fields on the CRD —
// the apiserver's structural schema (derived from these same types) already
// rejects an unrecognized key at write time, so there is no opaque-JSON
// layer here for ParseDeclaration's DisallowUnknownFields to re-check. Only
// Args stays raw JSON, for the same reason AgentUISlot.Default does: CRD
// types must not depend on pkg/web/uicomponents.
func actionsToDeclaration(actions []spiceboxv1alpha1.AgentUIAction) []uicomponents.Action {
	if len(actions) == 0 {
		return nil
	}
	out := make([]uicomponents.Action, len(actions))
	for i, a := range actions {
		var args json.RawMessage
		if a.Args != nil {
			args = json.RawMessage(a.Args.Raw)
		}
		out[i] = uicomponents.Action{
			Name: a.Name,
			Tool: a.Tool,
			// Dropping Prompt here made every prompt action arrive at Validate
			// declaring neither a tool nor a prompt, which it correctly
			// rejects — so an AgentUI that is valid as written reconciled
			// Valid=False with a message describing a document nobody wrote.
			// A field added to the CRD has to be carried by EVERY conversion
			// out of it; this one and pkg/web/uiview's are separate paths to the
			// same struct.
			Prompt: a.Prompt,
			Args:   args,
			Inputs: a.Inputs,
		}
	}
	return out
}

// actionToolNames collects each action's own Tool, in declaration order,
// duplicates and all — uigrant.UnrequestedActionTools dedupes on its own, so
// this stays a plain projection rather than re-deriving that logic here.
func actionToolNames(actions []spiceboxv1alpha1.AgentUIAction) []string {
	if len(actions) == 0 {
		return nil
	}
	// An action with no Tool is a PROMPT action — it asks the agent rather than
	// calling anything, so it has no tool to request and contributes nothing
	// here. Emitting its empty Tool made this gate fail the AgentUI over a
	// missing grant for the tool `""`, which names nothing and cannot be fixed
	// by adding anything to spec.tools.
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		if a.Tool == "" {
			continue
		}
		out = append(out, a.Tool)
	}
	return out
}

// formatUnrequestedActionTools renders Gate A's author-facing message,
// naming every offending tool and the ONE field that fixes all of them.
func formatUnrequestedActionTools(tools []string) string {
	quoted := make([]string, len(tools))
	for i, t := range tools {
		quoted[i] = strconv.Quote(t)
	}
	return fmt.Sprintf(
		"actions naming tools that spec.tools does not request: %s. Add each to spec.tools — only tools listed there become callable from this UI.",
		strings.Join(quoted, ", "))
}

// validateDeclaration turns the CR into ONE uicomponents.Declaration — the
// action table, plus either spec.view or the compiled spec.slots — and
// validates it in a single Validate call, so every cross-region guarantee
// (unique hook names, aggregate MaxNodes, the action table against every
// control) applies to the whole page at once rather than being checked once
// per region and missing what only shows up across them. It returns the
// normalized declaration on success so Reconcile can observe its hooks onto
// status without a second parse.
//
// EXACTLY one of spec.View and spec.Slots is required, and both halves of
// that are checked here, first. Both set is refused because two descriptions
// of one page cannot be reconciled by favoring one (Normalize refuses it too,
// but "declare either view or slots, not both" is a better message at Path
// "spec" than whatever Path Normalize's own generic error would carry for a
// CR that never went through a single top-level parse). NEITHER set is
// refused because the alternative is the quietest failure the page model has:
// the slots path would compile an empty list into an empty root stack, the CR
// would go Valid=True describing no page, status.hooks would be empty, and
// every update_view call would be refused for naming a hook that does not
// exist — with nothing anywhere saying why. It is also what a misspelled
// top-level key looks like by the time it reaches this function, since the
// CRD prunes unknown fields in spec, so the author who typed one gets a
// condition naming the mistake instead of a green CR.
//
// The view path parses spec.View whole: it is already one document, and
// ParseDeclaration's own strict decode plus Validate's walk supply every
// Path a caller needs.
//
// The slots path is different SPECIFICALLY because attribution matters:
// parsing the CR's whole spec.Slots as one legacy wire document (i.e.
// marshaling {"slots": [...]}, the wire shape CompileSlots itself compiles)
// would still work, but a malformed Default would then be reported against
// the COMPILED tree's node — a hook's own children[0] for a writable slot —
// with no way back to which of the CR's real slots produced it, since
// several slots share one call. So each slot's Default is parsed on its own,
// wrapped as its OWN one-node document ({"view": <raw default>}) through the
// same wireDeclaration + ParseDeclaration path spec.View uses, giving back
// exactly the parsed Default node with no compilation and nothing to dig out
// of. A marshal or parse failure is then attributed to "slots[i]" — the
// CR's real index — before anything is compiled. Only once every slot's
// Default has been parsed this way are the CR's typed Name/AgentWritable
// fields and the parsed Default assembled into uicomponents.Slot values and
// handed to Normalize ONCE, which is what actually runs CompileSlots and
// produces the tree Validate walks — so a Validate error from THAT point on
// reports the compiled path ("view.children[1].children[0]…"), which is
// where the author's default actually sits on the page the runner serves,
// not the CR's slot index.
//
// eligibleTools — spec.Tools ∩ the AgentClass grant, Reconcile's own
// uigrant.Ceiling result — is passed for BOTH opts.GrantedTools and
// opts.ReadonlyTools, with no widening for the action table. Widening would be
// provably vacuous: Gate A (Reconcile's uigrant.UnrequestedActionTools
// pre-check, run BEFORE this function) already REQUIRES every action tool to
// appear in spec.Tools, and anything reaching Valid=True also has it in the
// AgentClass grant via validateActions' own check below — which is exactly
// membership in eligibleTools. See
// TestEveryValidAgentUIHasItsActionToolsInEligibleTools for that property.
//
// ReadonlyTools stays eligibleTools for the reason it always did: this
// reconciler is namespace-scoped and has no session, so it can see neither a
// tool's StateImpact nor its origin's readOnlyHint — the two halves of the
// readonly predicate. Passing the ceiling makes uicomponents' readonly check
// vacuous at this layer rather than rejecting every Tier-0 tool binding on
// evidence this reconciler does not have; the unconditional gate is
// runner.handleAppToolCallReq, which re-evaluates the predicate against the
// live tool on every call. Same honestly-scoped posture as
// AgentUIStatus.EligibleTools, and for the same reason.
func validateDeclaration(spec spiceboxv1alpha1.AgentUISpec, eligibleTools map[string]bool) (uicomponents.Declaration, *uicomponents.ValidationError) {
	if spec.View != nil && len(spec.Slots) > 0 {
		return uicomponents.Declaration{}, &uicomponents.ValidationError{
			Path: "spec", Reason: "declare either view or slots, not both",
		}
	}
	if spec.View == nil && len(spec.Slots) == 0 {
		return uicomponents.Declaration{}, &uicomponents.ValidationError{
			Path: "spec", Reason: "declare either view or slots",
		}
	}

	opts := uicomponents.DefaultOptions()
	opts.GrantedTools = eligibleTools
	opts.ReadonlyTools = eligibleTools
	opts.NormalizeToolName = synthesize.NormalizeName

	decl := uicomponents.Declaration{Actions: actionsToDeclaration(spec.Actions)}
	switch {
	case spec.View != nil:
		node, verr := parseWireNode(spec.View.Raw, "view")
		if verr != nil {
			return uicomponents.Declaration{}, verr
		}
		decl.View = node
	default:
		for i, slot := range spec.Slots {
			s := uicomponents.Slot{Name: slot.Name, AgentWritable: slot.AgentWritable}
			if slot.Default != nil {
				node, verr := parseWireNode(slot.Default.Raw, fmt.Sprintf("slots[%d]", i))
				if verr != nil {
					return uicomponents.Declaration{}, verr
				}
				s.Default = node
			}
			decl.Slots = append(decl.Slots, s)
		}
		normalized, err := uicomponents.Normalize(decl)
		if err != nil {
			return uicomponents.Declaration{}, &uicomponents.ValidationError{Path: "slots", Reason: err.Error()}
		}
		decl = normalized
	}

	if err := uicomponents.Validate(decl, opts); err != nil {
		var verr *uicomponents.ValidationError
		if errors.As(err, &verr) {
			return uicomponents.Declaration{}, verr
		}
		return uicomponents.Declaration{}, &uicomponents.ValidationError{Path: "spec", Reason: err.Error()}
	}
	return decl, nil
}

// parseWireNode strict-decodes one raw JSON node (spec.View, or a single
// slot's Default) by wrapping it as a one-node wireDeclaration and running
// it through ParseDeclaration — the same strict decode an author-submitted
// page gets, so a typo'd structural key inside raw is rejected rather than
// silently dropped. path attributes a marshal or parse failure to the
// caller's real location (e.g. "view" or "slots[2]") rather than a location
// synthesized by this helper.
func parseWireNode(raw []byte, path string) (*uicomponents.Node, *uicomponents.ValidationError) {
	wire, err := json.Marshal(wireDeclaration{View: json.RawMessage(raw)})
	if err != nil {
		return nil, &uicomponents.ValidationError{
			Path: path, Reason: fmt.Sprintf("marshal wire declaration: %v", err),
		}
	}
	one, err := uicomponents.ParseDeclaration(wire)
	if err != nil {
		return nil, &uicomponents.ValidationError{Path: path, Reason: err.Error()}
	}
	return one.View, nil
}

// hooksFor projects the page's hooks onto the status shape: a pure function
// of the already-validated declaration, so a re-reconcile of an unchanged CR
// observes byte-identical hooks and writeStatusIfChanged sends nothing.
func hooksFor(d uicomponents.Declaration) []spiceboxv1alpha1.AgentUIHook {
	hooks := uicomponents.Hooks(d)
	if len(hooks) == 0 {
		return nil
	}
	out := make([]spiceboxv1alpha1.AgentUIHook, len(hooks))
	for i, h := range hooks {
		out[i] = spiceboxv1alpha1.AgentUIHook{Name: h.Name, Intent: h.Intent, AllowedComponents: h.AllowedComponents}
	}
	return out
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.AgentUI{}).
		// A change to an AgentClass's spec.agentUI.grantedTools must re-observe
		// every AgentUI it references without waiting for that AgentUI's own
		// resync — an operator editing the grant expects the AgentUI's status to
		// catch up promptly, per the task's "seen on apply, not on a dead
		// button" goal.
		Watches(&spiceboxv1alpha1.AgentClass{}, handler.EnqueueRequestsFromMapFunc(r.MapAgentClassToAgentUIs)).
		Complete(r)
}

// MapAgentClassToAgentUIs re-enqueues the AgentUI an AgentClass references via
// spec.agentUI.ref, if any. AgentUI and AgentClass are both namespace-scoped,
// so only the same namespace is considered.
func (r *Reconciler) MapAgentClassToAgentUIs(ctx context.Context, o client.Object) []reconcile.Request {
	ac, ok := o.(*spiceboxv1alpha1.AgentClass)
	if !ok || ac.Spec.AgentUI == nil || ac.Spec.AgentUI.Ref == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{
		Namespace: ac.Namespace,
		Name:      ac.Spec.AgentUI.Ref,
	}}}
}
