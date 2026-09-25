package installcmd

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/builderbundle"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
	"github.com/authzed/openagentprimitives/pkg/platform/oap/install"
)

// The agent-builder's identity is fixed, not derived from any install flag:
// every cluster gets exactly one builder, at exactly this namespace/name, so
// Task 5's seeding (and any future admin surface) can hardcode the lookup
// rather than threading a --name/--namespace override through a second
// install path. builderSidecarToolbox and builderStartersQuestion mirror the
// embedded manifest's own SidecarToolbox ref and question name — kept here as
// named constants (rather than re-parsing the manifest to discover them)
// because Task 5 needs the same two strings and re-deriving them from the
// bundle would be a second source of truth for one fact.
const (
	builderClassNamespace   = "agentprimitives-system"
	builderClassName        = "agent-builder"
	builderSidecarToolbox   = "workshop"
	builderStartersQuestion = "builderStarters"
)

// applyBuilderBundle installs the embedded agent-builder .oap into the fixed
// agentprimitives-system namespace, answering the bundle's one required
// install question (who may start it) with starters.
func applyBuilderBundle(ctx context.Context, c client.Client, starters []string) (*install.Result, error) {
	b, err := builderbundle.Bundle()
	if err != nil {
		return nil, err
	}
	// Answer the required resourceList question DIRECTLY as a []string — NOT via
	// InstallOpts.Sets. A --set value is a single string that install.Resolve
	// passes through unchanged (typedAnswer does not comma-split a QResourceList),
	// so Sets{"builderStarters": "user:a,user:b"} would bind ONE bogus starter
	// "user:a,user:b". oap.Answers is map[string]any and install.Install runs
	// oap.Apply(crs, questions, answers) itself, so a []string answer overlays the
	// AgentClass's allowedStarters correctly.
	answers := oap.Answers{builderStartersQuestion: starters}
	return install.Install(ctx, c, b, answers, nil, install.InstallOpts{
		Namespace:  builderClassNamespace, // FIXED — never g.Bundle().Namespace (which defaults to "default")
		SourceKind: "builder",
	})
}

// seedBuilderClass sanctions the just-applied builder AgentClass by
// recording {builderClassNamespace, builderClassName, builderSidecarToolbox}
// in ClusterAgentSettings.spec.limits.builderClasses — the byte-exact entry
// the AgentSession reconciler's workshop gate checks against
// (BuilderClassRef, settings_common_types.go). Without this entry the class
// applyBuilderBundle just wrote is unsanctioned and no session can start a
// workshop against it.
//
// A ClusterAgentSettings singleton commonly ALREADY EXISTS by the time this
// runs — oap init's settings wizard creates one to hold pinning/budget/etc.
// ceilings — so this is a get-or-default MERGE, not a create-only write like
// pinning.go's seed: a bare Create here would fail AlreadyExists against that
// common case and silently never sanction the class. Every other Limits
// field on an existing CAS is preserved untouched; only builderClasses is
// appended to, and only if the exact entry isn't already present, so a
// re-seed (every install re-runs this) is idempotent.
func seedBuilderClass(ctx context.Context, c client.Client) error {
	ref := spiceboxv1alpha1.BuilderClassRef{
		Namespace: builderClassNamespace, Name: builderClassName, SidecarToolbox: builderSidecarToolbox,
	}

	var cas spiceboxv1alpha1.ClusterAgentSettings
	err := c.Get(ctx, client.ObjectKey{Name: spiceboxv1alpha1.ClusterAgentSettingsName}, &cas)
	switch {
	case apierrors.IsNotFound(err):
		cas = spiceboxv1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
			Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{
				BuilderClasses: &[]spiceboxv1alpha1.BuilderClassRef{ref},
			}},
		}
		return c.Create(ctx, &cas)
	case err != nil:
		return fmt.Errorf("get ClusterAgentSettings: %w", err)
	}

	if cas.Spec.Limits == nil {
		cas.Spec.Limits = &spiceboxv1alpha1.SettingsLimits{}
	}
	existing := []spiceboxv1alpha1.BuilderClassRef{}
	if cas.Spec.Limits.BuilderClasses != nil {
		existing = *cas.Spec.Limits.BuilderClasses
	}
	for _, e := range existing {
		if e == ref {
			return nil // already sanctioned — idempotent no-op
		}
	}
	existing = append(existing, ref)
	cas.Spec.Limits.BuilderClasses = &existing
	return c.Update(ctx, &cas)
}
