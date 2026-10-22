package github

import (
	"encoding/json"
	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// PublishesTriggerReports opts CI inputs into their bound Check Run report
// surface. Legacy PR inputs retain their dedicated output-channel behavior.
func (Kind) PublishesTriggerReports(ch *api.Channel) bool {
	if ch == nil || ch.Spec.GitHub == nil || ch.Spec.GitHub.Trigger == nil {
		return false
	}
	t := ch.Spec.GitHub.Trigger
	return t.Event == "workflow_run" && t.Readiness == "all_completed" && len(t.Actions) == 1 && t.Actions[0] == "completed" && len(t.Workflows) > 0
}

func (Kind) ConfigPublishesTriggerReports(raw json.RawMessage) bool {
	cfg, err := decodeHandlerConfig(raw)
	return err == nil && cfg.Trigger != nil && cfg.Trigger.Event == "workflow_run" && cfg.Trigger.Readiness == "all_completed"
}
