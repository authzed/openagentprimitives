package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// sourcesProjector lists SkillSource (namespaced) + ClusterSkillSource
// (cluster). Both share SkillSourceStatus; the spec (repo/ref) differs by
// concrete type, so the repo/ref are passed in by the caller.
type sourcesProjector struct{}

func (sourcesProjector) Resource() string { return "sources" }

func (sourcesProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var rows []config.ResourceRow

	var srcs spiceboxv1alpha1.SkillSourceList
	if err := c.List(ctx, &srcs); err != nil {
		return nil, err
	}
	for i := range srcs.Items {
		s := &srcs.Items[i]
		rows = append(rows, sourceRow(s.Name, s.Namespace, "namespaced", "skillsource",
			"skillsource", s.Spec.RepoURL, s.Spec.Ref, &s.Status))
	}

	var csrcs spiceboxv1alpha1.ClusterSkillSourceList
	if err := c.List(ctx, &csrcs); err != nil {
		return nil, err
	}
	for i := range csrcs.Items {
		s := &csrcs.Items[i]
		rows = append(rows, sourceRow(s.Name, "", "cluster", "clusterskillsource",
			"clusterskillsource", s.Spec.RepoURL, s.Spec.Ref, &s.Status))
	}

	return rows, nil
}

func sourceRow(name, ns, scope, kindBadge, editKind, repo, ref string, status *spiceboxv1alpha1.SkillSourceStatus) config.ResourceRow {
	st, reason := projectStatus(status.Conditions, spiceboxv1alpha1.SkillSourceConditionReady, "Ready")

	// StatusReason surfaces the resolved commit (short) when a sync has landed
	// AND the source is currently healthy; otherwise the condition reason
	// explains why not. ResolvedSHA persists from the last successful sync, so a
	// source that synced then broke (Ready=False) would otherwise mask the
	// failure reason behind a stale SHA — gate the substitution on the ok word.
	if status.ResolvedSHA != "" && st == "Ready" {
		reason = shortSHA(status.ResolvedSHA)
	}

	badges := []config.Badge{{Key: "kind", Value: kindBadge}}
	if repo != "" {
		badges = append(badges, config.Badge{Key: "repo", Value: repo})
	}
	if ref != "" {
		badges = append(badges, config.Badge{Key: "ref", Value: ref})
	}

	return config.ResourceRow{
		Name:         name,
		Namespace:    ns,
		Scope:        scope,
		Status:       st,
		StatusReason: reason,
		Badges:       badges,
		Counts:       []config.Count{{Label: "discoveredSkills", Value: int(status.DiscoveredSkills)}},
		ManageCmd:    editCmd(editKind, name, ns),
	}
}

func init() { config.Register(&sourcesProjector{}) }
