package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// directoryProjector lists RelationshipSource — the directory syncs (Slack,
// GitHub, 1Password) that write group and identity edges into SpiceDB.
//
// Distinct from the "sources" slug, which is SkillSource/ClusterSkillSource
// (skills synced from git). The two are unrelated despite both being "sources"
// in English.
type directoryProjector struct{}

func (directoryProjector) Resource() string { return "directory" }

func (directoryProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var list spiceboxv1alpha1.RelationshipSourceList
	if err := c.List(ctx, &list); err != nil {
		return nil, err
	}
	rows := make([]config.ResourceRow, 0, len(list.Items))
	for i := range list.Items {
		src := &list.Items[i]
		status, reason := projectRelationshipSourceStatus(src.Status.Conditions)

		badges := []config.Badge{{Key: "kind", Value: src.Spec.Kind}}
		if src.Spec.Auth.AgentIdentity != "" || src.Spec.Auth.Credential != "" {
			badges = append(badges, config.Badge{
				Key:   "credential",
				Value: src.Spec.Auth.AgentIdentity + "/" + src.Spec.Auth.Credential,
			})
		}
		if src.Spec.BaseURL != "" {
			badges = append(badges, config.Badge{Key: "endpoint", Value: src.Spec.BaseURL})
		}

		// Counts are omitted entirely until a pass has been recorded: rendering
		// zeros for a source that has never run would read as "ran and found
		// nothing", which is a different and much less alarming claim.
		var counts []config.Count
		if lp := src.Status.Sync.LastPass; lp != nil {
			counts = []config.Count{
				{Label: "scopes", Value: int(lp.ScopesProcessed)},
				{Label: "written", Value: int(lp.Written)},
				{Label: "pruned", Value: int(lp.Pruned)},
				{Label: "joinMisses", Value: int(lp.JoinMisses)},
				// The count that made an invisible failure visible: scopes
				// processed can read 156 while every one of them failed, and
				// nothing else on this row distinguishes the two.
				{Label: "errors", Value: int(lp.ScopeErrors)},
			}
		}

		rows = append(rows, config.ResourceRow{
			Name:         src.Name,
			Namespace:    src.Namespace,
			Scope:        "namespaced",
			Status:       status,
			StatusReason: reason,
			Badges:       badges,
			Counts:       counts,
			ManageCmd:    "oap directory configure",
		})
	}
	return rows, nil
}

func init() { config.Register(&directoryProjector{}) }
