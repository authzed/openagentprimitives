package admind

import (
	"context"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/authzed/openagentprimitives/pkg/platform/capacityfit/hook"
	"github.com/authzed/openagentprimitives/pkg/platform/oap"
)

// newCapacityQuestions is admind's install.InstallOpts.ExtraQuestions hook, a
// thin adapter over capacityfit/hook.New — the shared ceiling-resolution and
// installed-class lookup the CLI and desktop installer also use, factored out
// because this package cannot import cmd/oap. EVERY failure path (no Clientset,
// cloud detection, the ceiling read) yields a hook emitting no questions and
// one explanatory notice, so a capacity read that cannot run never blocks an
// install.
func (a *Admind) newCapacityQuestions(ctx context.Context) func(context.Context, []*unstructured.Unstructured) ([]oap.Question, []string, error) {
	return hook.New(ctx, a.cfg.Clientset, a.cfg.K8s)
}

// unansweredCapacityQuestions returns every capacity question in qs that
// capacityValues does not already answer with a non-empty value.
//
// It deliberately does NOT treat a Default-bearing question as answered, unlike
// missingRequiredQuestions — right for a bundle's manifest questions, whose
// Default is the author's considered suggestion, but capacityfit sets a Default
// on EVERY question it synthesizes, so reusing that rule would silently
// auto-apply every capacity clamp instead of surfacing it. The desktop
// installer does auto-fit from that Default because it has no form to ask with;
// admind has one, so the Default is a pre-filled suggestion, not consent.
//
// An EMPTY STRING counts as unanswered, not just an absent key: the UI writes
// "" for a field the operator cleared, and treating that as an answer sends an
// empty quantity into install.Resolve's CEL validation instead of re-asking.
func unansweredCapacityQuestions(qs []oap.Question, capacityValues map[string]string) []oap.Question {
	var unanswered []oap.Question
	for _, q := range qs {
		if v, ok := capacityValues[q.Name]; ok && v != "" {
			continue
		}
		unanswered = append(unanswered, q)
	}
	sort.Slice(unanswered, func(i, j int) bool { return unanswered[i].Name < unanswered[j].Name })
	return unanswered
}
