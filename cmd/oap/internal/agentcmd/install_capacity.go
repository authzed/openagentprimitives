package agentcmd

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	"github.com/authzed/openagentprimitives/pkg/platform/capacityfit/hook"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// NewCapacityQuestions resolves this cluster's scheduling ceiling once and
// returns an install.InstallOpts.ExtraQuestions hook over it — a thin
// cmd/oap-local adapter over the shared pkg/platform/capacityfit/hook.New (the actual
// ceiling-resolution + installed-class lookup logic, shared with admind,
// which calls hook.New directly since it cannot import cmd/oap). Kept as a
// named function so both cmd/oap callers (the CLI in agent_install.go, the
// desktop installer in desktop_oapinstall_darwin.go) read
// "NewCapacityQuestions" rather than each spelling out kb.Typed/kb.Controller.
func NewCapacityQuestions(ctx context.Context, kb *kube.Bundle) func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
	return hook.New(ctx, kb.Typed, kb.Controller)
}

// requireCapacityConsent wraps a capacity ExtraQuestions hook so that any
// question it proposes which --set doesn't already answer, AND whose Default
// would be a NEW decision (oap.Question.Unchanged is false), ABORTS the
// install (nothing applied) instead of letting install.Resolve's ordinary
// non-interactive Default-fallback silently adopt pkg/platform/capacityfit's suggested
// clamp. A question whose Default merely reaffirms the value already on the
// cluster (Unchanged=true — a re-install where nothing about the clamp has
// changed) is let through: the resulting apply is byte-identical to what is
// already there, so aborting would buy no safety, only friction, and would
// defeat the whole point of the adoption path (a CI job re-installing an
// unchanged bundle must keep working with no flag). The caller
// (newAgentInstallCmd) only wraps the hook with this when stdin is not a TTY
// AND --fit-resources was not passed — i.e. exactly the case where nothing
// else in the pipeline would ever surface a REAL clamp decision to a human
// before it lands.
//
// This policy lives HERE, in cmd/oap only — not in pkg/platform/capacityfit or
// pkg/platform/oap/install, which stay policy-free and keep answering a Default when
// nothing else is supplied — because the other two .oap install surfaces
// already have their own consent-equivalent and must keep auto-fitting
// exactly as before: the desktop installer can never prompt at all (see
// desktop_oapinstall_darwin.go) and admind surfaces the question as a form
// field the operator fills in (see pkg/web/admind/oapinstall.go). Only a bare CLI
// invocation with nothing attached to prompt AND no explicit flag has no
// other signal reaching the operator — shipping an agent SMALLER than the
// bundle declared, in CI, with only a warning line, is exactly the silent
// failure this whole feature exists to prevent.
func requireCapacityConsent(hook func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error), sets map[string]string) func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
	return func(ctx context.Context, crs []*unstructured.Unstructured) ([]oap.Question, []string, error) {
		qs, notices, err := hook(ctx, crs)
		if err != nil {
			return nil, nil, err
		}
		for _, q := range qs {
			if _, answered := sets[q.Name]; answered {
				continue
			}
			if q.Unchanged {
				continue
			}
			// q.Prompt already names the class, dimension, declared value, and
			// ceiling (see pkg/platform/capacityfit's capacityQuestion) — reuse it
			// verbatim instead of re-deriving the same facts from q.Name here.
			return nil, nil, fmt.Errorf(
				"%s this install cannot prompt for an answer (stdin is not a terminal); pass --fit-resources to accept the suggested value, or --set %s=<value> to choose your own",
				q.Prompt, q.Name)
		}
		return qs, notices, nil
	}
}
