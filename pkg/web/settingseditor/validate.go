package settingseditor

import (
	v1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	webhook "github.com/authzed/openagentprimitives/pkg/controllers/webhooks/settings"
	"github.com/authzed/openagentprimitives/pkg/platform/settings"
)

// Validate runs the same checks the admission webhook applies (isCluster
// selects the ClusterAgentSettings vs AgentSettings catalog rules), then a
// pure resolver pass so catalog/default disagreements surface as violations
// before any apply. Callers must link the pinning + contentguard registries
// (blank imports), exactly as cmd/oap/main.go does.
func Validate(spec *v1alpha1.SettingsSpec, isCluster bool) Result {
	// Nil slices marshal to JSON null; the wire contract promises arrays.
	res := Result{Errors: []string{}, Violations: []Violation{}}
	for _, msg := range []string{
		webhook.SelfConsistencyError(spec),
		webhook.PinningKindsError(spec),
		webhook.ContentInspectorsError(spec),
		webhook.ModelCatalogError(spec, isCluster),
	} {
		if msg != "" {
			res.Errors = append(res.Errors, msg)
		}
	}
	eff, viols := settings.Resolve(settings.Inputs{Cluster: spec})
	for _, v := range viols {
		res.Violations = append(res.Violations, Violation{Reason: v.Reason, Message: v.Message, Fatal: v.Fatal})
	}
	if len(res.Errors) == 0 {
		st := eff.ToStatus()
		res.Effective = &st
	}
	return res
}
