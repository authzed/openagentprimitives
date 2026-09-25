// Package spiceboxtoolspec reconciles SpiceboxToolspec CRs. The reconciler
// resolves the referenced toolkit, compiles any CEL expressions, validates
// allowSubcommands against the toolkit, and surfaces Valid=True/False with a
// reason. A Valid=False Toolspec is skipped at ToolCall validation time.
package spiceboxtoolspec

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/observe"
	"github.com/authzed/openagentprimitives/pkg/authz/observe/fromcrd"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	apreconcile "github.com/authzed/openagentprimitives/pkg/controllers/internal/reconcile"
	toolspeccel "github.com/authzed/openagentprimitives/pkg/tools/cel"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/registry"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolspecs,verbs=get;list;watch
// +kubebuilder:rbac:groups=agentprimitives.authzed.com,resources=spiceboxtoolspecs/status,verbs=get;update;patch

// Reconciler reconciles SpiceboxToolspec objects.
type Reconciler struct {
	Client   client.Client
	Registry *registry.Registry
}

// SetupWithManager registers the reconciler with the manager and arranges for
// SpiceboxToolkit changes to re-enqueue all Toolspecs.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	mapToolkitToToolspecs := func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list spiceboxv1alpha1.SpiceboxToolspecList
		if err := r.Client.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			out = append(out, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&list.Items[i]),
			})
		}
		return out
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&spiceboxv1alpha1.SpiceboxToolspec{}).
		Watches(
			&spiceboxv1alpha1.SpiceboxToolkit{},
			handler.EnqueueRequestsFromMapFunc(mapToolkitToToolspecs),
		).
		Complete(r)
}

// Reconcile resolves the referenced toolkit, compiles CEL, validates
// allowSubcommands, and sets the Valid condition.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ts spiceboxv1alpha1.SpiceboxToolspec
	if cont, err := apreconcile.LoadInto(ctx, r.Client, req.NamespacedName, &ts); !cont {
		return ctrl.Result{}, err
	}
	if ts.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Always stamp ObservedGeneration. Phases set ResolvedToolkit when
	// they have a meaningful value; on early failure we want it cleared
	// to avoid a stale resolution from a prior pass leaking through.
	ts.Status.ObservedGeneration = ts.Generation
	ts.Status.ResolvedToolkit = ""

	var tk *toolkit.Toolkit

	resolveToolkit := func(ctx context.Context) apreconcile.Outcome {
		got, err := r.Registry.Resolve(ctx, ts.Spec.Toolkit.Name, ts.Spec.Toolkit.Revision)
		if err != nil {
			if errors.Is(err, registry.ErrNotFound) {
				conditions.SetFalse(&ts, &ts.Status.Conditions,
					spiceboxv1alpha1.SpiceboxToolspecConditionValid,
					spiceboxv1alpha1.ReasonToolkitMissing,
					fmt.Sprintf("no toolkit named %q at revision %q", ts.Spec.Toolkit.Name, ts.Spec.Toolkit.Revision))
				return apreconcile.StopAfter()
			}
			return apreconcile.FailWith(err)
		}
		tk = got
		return apreconcile.Continue()
	}

	loadSpec := func(ctx context.Context) apreconcile.Outcome {
		if _, err := ts.Spec.ToSpec(); err != nil {
			conditions.SetFalse(&ts, &ts.Status.Conditions,
				spiceboxv1alpha1.SpiceboxToolspecConditionValid,
				spiceboxv1alpha1.ReasonSpecLoadFailed,
				fmt.Sprintf("spec.LoadBytes: %v", err))
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	compileConstraints := func(ctx context.Context) apreconcile.Outcome {
		for i, c := range ts.Spec.Constraints {
			if _, err := toolspeccel.Compile(c.CEL); err != nil {
				conditions.SetFalse(&ts, &ts.Status.Conditions,
					spiceboxv1alpha1.SpiceboxToolspecConditionValid,
					spiceboxv1alpha1.ReasonCELCompileError,
					fmt.Sprintf("constraints[%d]: %v", i, err))
				return apreconcile.StopAfter()
			}
		}
		return apreconcile.Continue()
	}

	compileExceptions := func(ctx context.Context) apreconcile.Outcome {
		for i, e := range ts.Spec.Exceptions {
			if e.When == "" {
				continue
			}
			if _, err := toolspeccel.Compile(e.When); err != nil {
				conditions.SetFalse(&ts, &ts.Status.Conditions,
					spiceboxv1alpha1.SpiceboxToolspecConditionValid,
					spiceboxv1alpha1.ReasonCELCompileError,
					fmt.Sprintf("exceptions[%d].when: %v", i, err))
				return apreconcile.StopAfter()
			}
		}
		return apreconcile.Continue()
	}

	// compileObserves compile-checks every observes block through
	// observe.ValidateBlock, the same package (and, transitively, the same
	// relwrites CEL env) that will evaluate these blocks at dispatch. The
	// motivating tool for `observes` is a SpiceboxToolspec, not an MCPServer,
	// so this half matters more than its MCPServer sibling: a toolspec that
	// reports Valid=True with an uncompilable observes block would be
	// executed unchecked, and the malformed-block-with-no-subject shape this
	// closes is the exact laundering the design exists to prevent. The CRD's
	// MinItems=1 on subjects already refuses that shape at admission; this is
	// the second, redundant refusal for a CR applied before that schema
	// shipped or through a client that skips validation.
	compileObserves := func(ctx context.Context) apreconcile.Outcome {
		for i, ob := range ts.Spec.Observes {
			if err := observe.ValidateBlock(fromcrd.FromCRD(ob)); err != nil {
				conditions.SetFalse(&ts, &ts.Status.Conditions,
					spiceboxv1alpha1.SpiceboxToolspecConditionValid,
					spiceboxv1alpha1.ReasonCELCompileError,
					fmt.Sprintf("observes[%d]: %v", i, err))
				return apreconcile.StopAfter()
			}
		}
		return apreconcile.Continue()
	}

	validateAllowSubcommands := func(ctx context.Context) apreconcile.Outcome {
		if msg := unknownSubcommands(tk, ts.Spec.AllowSubcommands); msg != "" {
			conditions.SetFalse(&ts, &ts.Status.Conditions,
				spiceboxv1alpha1.SpiceboxToolspecConditionValid,
				spiceboxv1alpha1.ReasonUnknownSubcommand, msg)
			return apreconcile.StopAfter()
		}
		return apreconcile.Continue()
	}

	finalSuccess := func(ctx context.Context) apreconcile.Outcome {
		resolvedName := "<builtin>"
		if !isBuiltin(r.Registry, tk) {
			var list spiceboxv1alpha1.SpiceboxToolkitList
			if err := r.Client.List(ctx, &list); err == nil {
				for _, item := range list.Items {
					if item.Spec.Name == tk.Name && item.Spec.ToolkitRevision == tk.ToolkitRevision {
						resolvedName = item.Name
						break
					}
				}
			}
		}
		ts.Status.ResolvedToolkit = resolvedName
		conditions.SetTrue(&ts, &ts.Status.Conditions,
			spiceboxv1alpha1.SpiceboxToolspecConditionValid, "Resolved")
		return apreconcile.Continue()
	}

	phases := []apreconcile.Phase{
		resolveToolkit,
		loadSpec,
		compileConstraints,
		compileExceptions,
		compileObserves,
		validateAllowSubcommands,
		finalSuccess,
	}
	return ctrl.Result{}, apreconcile.RunPhases(ctx, r.Client, &ts, phases)
}

func unknownSubcommands(tk *toolkit.Toolkit, allow []string) string {
	known := map[string]bool{}
	for _, s := range tk.Subcommands {
		known[strings.Join(s.Path, " ")] = true
	}
	for _, name := range allow {
		if !known[name] {
			return fmt.Sprintf("allowSubcommands: %q is not a subcommand of toolkit %q", name, tk.Name)
		}
	}
	return ""
}

func isBuiltin(reg *registry.Registry, tk *toolkit.Toolkit) bool {
	for _, b := range reg.Builtins() {
		if b.Name == tk.Name && b.ToolkitRevision == tk.ToolkitRevision {
			return true
		}
	}
	return false
}
