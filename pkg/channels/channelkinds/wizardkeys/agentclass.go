package wizardkeys

import (
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// KeyAgentClass is the answer key the shared AgentClass-binding question lands
// under. Stable: a caller seeding answers from flags for a non-interactive run
// addresses the question by it.
const KeyAgentClass = "agentclass"

// AgentClassQuestion lists the namespace's AgentClasses and states the binding
// question over them, reporting which class the rest of the flow's defaults
// may derive from and whether that class is a fact or a guess.
//
// prompt is the ONE thing a kind supplies, because it is the one thing that
// legitimately differs: bento binds a cron trigger, github binds a review bot,
// slack binds an app, and each says so in its own words. Everything else —
// the listing, the refusal, the Enum, the ambiguity rule — is identical across
// them, and used to be three copies that could drift apart silently.
//
// A SEEDED ANSWER STILL COSTS THE LISTING, and is verified against it: a flag
// that names an AgentClass which does not exist produces a Channel that lints
// clean and never works, so what is at stake is a REFUSAL and not merely a
// prompt the operator did not have to see. A nil client is an offline dry-run
// with nothing to check against, and a seeded value then stands in as the sole
// Enum option so ValidateInputs' "a QEnum carries values" rule holds for a
// question no client will render.
//
// The second return is the class the flow's other defaults may derive from,
// and the third says whether it is a fact (the operator named it, or it is the
// only one that exists) or a guess. A WRONG default is worse than NO default,
// because a wrong one can be silently accepted — a blank answer falls back to
// it — while an omitted one cannot: a required question with no Default fails
// closed. So a caller derives a default from the second return only when the
// third is true.
//
// The question's OWN Default is set either way, and that is not the same
// decision: an enum's widget pre-selects its first option regardless of
// Default, so there is no "omit the default" available for it the way there is
// for free text.
func AgentClassQuestion(ctx context.Context, in channelkinds.WizardInput, prompt string) (oap.Question, string, bool, error) {
	classes, err := listAgentClasses(ctx, in.K8s, in.Namespace)
	if err != nil {
		return oap.Question{}, "", false, fmt.Errorf("list agent classes: %w", err)
	}

	chosen := SeededAnswer(in, KeyAgentClass)
	unambiguous := false
	switch {
	case chosen != "":
		if in.K8s != nil && !slices.Contains(classes, chosen) {
			return oap.Question{}, "", false, fmt.Errorf("AgentClass %q not found in namespace %q; a Channel cannot bind to an agent that does not exist", chosen, in.Namespace)
		}
		if len(classes) == 0 {
			classes = []string{chosen}
		}
		// The operator named it, so every default derived from it is theirs.
		unambiguous = true
	case len(classes) == 0:
		return oap.Question{}, "", false, fmt.Errorf("no AgentClasses found in namespace %q; create one before creating a Channel", in.Namespace)
	default:
		// classes[0] is what a bare accept selects: an enum's widget
		// pre-selects its first option regardless of Default, so there is no
		// "omit the default" available the way there is for free text.
		// Whether it is also the answer everything else may derive from is the
		// separate question the doc above rules on.
		chosen = classes[0]
		unambiguous = len(classes) == 1
	}

	return oap.Question{
		Name:    KeyAgentClass,
		Type:    oap.QEnum,
		Prompt:  prompt,
		Enum:    classes,
		Default: chosen,
	}, chosen, unambiguous, nil
}

// listAgentClasses returns AgentClass names in the namespace. A nil client is
// an offline dry-run — no cluster to ask — and reads as no classes rather than
// as an error, which is what lets AgentClassQuestion still state a renderable
// question for one.
//
// It lists METADATA ONLY, not the typed spec, on purpose: all this needs is the
// names, and a typed List decodes every AgentClass's whole spec — so one object
// whose stored spec no longer matches the current Go types (a field that changed
// shape across a release and was never re-applied; the apiserver validates on
// write, not read, and serves the stale bytes back verbatim) fails the entire
// List and takes channel wiring for an UNRELATED agent down with it. A
// PartialObjectMetadata request never transmits or decodes a spec, so name
// listing cannot be broken by spec drift elsewhere in the namespace.
func listAgentClasses(ctx context.Context, c client.Client, namespace string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(spiceboxv1alpha1.SchemeGroupVersion.WithKind("AgentClassList"))
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, ac := range list.Items {
		out = append(out, ac.Name)
	}
	return out, nil
}
