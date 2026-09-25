package projectors

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/admind/config"
)

// skillsProjector lists Skill (namespaced) + ClusterSkill (cluster) under one
// slug. Both carry the same SkillSpec/SkillStatus, so the row mapping is
// shared; the kind badge + scope differ.
type skillsProjector struct{}

func (skillsProjector) Resource() string { return "skills" }

func (skillsProjector) List(ctx context.Context, c client.Client) ([]config.ResourceRow, error) {
	var rows []config.ResourceRow

	var skills spiceboxv1alpha1.SkillList
	if err := c.List(ctx, &skills); err != nil {
		return nil, err
	}
	for i := range skills.Items {
		s := &skills.Items[i]
		rows = append(rows, skillRow(s.Name, s.Namespace, "namespaced", "skill", "skill",
			&s.Spec, &s.Status))
	}

	var cskills spiceboxv1alpha1.ClusterSkillList
	if err := c.List(ctx, &cskills); err != nil {
		return nil, err
	}
	for i := range cskills.Items {
		s := &cskills.Items[i]
		rows = append(rows, skillRow(s.Name, "", "cluster", "clusterskill", "clusterskill",
			&s.Spec, &s.Status))
	}

	return rows, nil
}

func skillRow(name, ns, scope, kindBadge, editKind string, spec *spiceboxv1alpha1.SkillSpec, status *spiceboxv1alpha1.SkillStatus) config.ResourceRow {
	st, reason := projectStatus(status.Conditions, spiceboxv1alpha1.SkillConditionValid, "Valid")

	delivery := "instruction"
	if spec.Bundle != nil {
		delivery = "executable"
	}
	badges := []config.Badge{
		{Key: "kind", Value: kindBadge},
		{Key: "delivery", Value: delivery},
	}
	// Pin strength is omitted when no PinRecord is stamped -- an unparseable
	// canonical name records none, and the Valid condition carries why.
	if strength := skillPinStrength(status); strength != "" {
		badges = append(badges, config.Badge{Key: "pin", Value: strength})
	}

	return config.ResourceRow{
		Name:         name,
		Namespace:    ns,
		Scope:        scope,
		Status:       st,
		StatusReason: reason,
		Badges:       badges,
		ManageCmd:    editCmd(editKind, name, ns),
	}
}

func skillPinStrength(status *spiceboxv1alpha1.SkillStatus) string {
	if status.Pin == nil {
		return ""
	}
	return status.Pin.Strength
}

func init() { config.Register(&skillsProjector{}) }
