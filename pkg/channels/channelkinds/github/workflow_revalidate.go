package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	api "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type workflowClassIdentity struct {
	Generation int64  `json:"generation"`
	UID        string `json:"uid"`
}
type workflowSnapshot struct {
	ChannelUID        string                `json:"channel_uid"`
	ChannelGeneration int64                 `json:"channel_generation"`
	AgentClass        workflowClassIdentity `json:"agent_class"`
	Repository        string                `json:"repository"`
	PR                int                   `json:"pr_number"`
	HeadSHA           string                `json:"head_sha"`
	BaseSHA           string                `json:"base_sha"`
	RunHeadSHA        string                `json:"run_head_sha"`
	Runs              []workflowRun         `json:"runs"`
}

func loadWorkflowClassIdentity(ctx context.Context, c client.Client, ch *api.Channel) (workflowClassIdentity, error) {
	if ch.Spec.AgentClass == "" {
		return workflowClassIdentity{}, nil
	}
	if c == nil {
		return workflowClassIdentity{}, fmt.Errorf("workflow readiness needs a Kubernetes client to resolve AgentClass generation")
	}
	var class api.AgentClass
	if err := c.Get(ctx, client.ObjectKey{Namespace: ch.Namespace, Name: ch.Spec.AgentClass}, &class); err != nil {
		return workflowClassIdentity{}, fmt.Errorf("workflow readiness AgentClass: %w", err)
	}
	return workflowClassIdentity{Generation: class.Generation, UID: string(class.UID)}, nil
}
func revalidateWorkflowCohort(ctx context.Context, c client.Client, key client.ObjectKey, b *api.ChannelBinding, provider workflowProvider) error {
	var current api.Channel
	if err := c.Get(ctx, key, &current); err != nil {
		return fmt.Errorf("read current input Channel: %w", err)
	}
	var snapshot workflowSnapshot
	raw := []byte(b.External["workflow_runs"])
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("invalid workflow cohort snapshot: %w", err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != b.External["trigger_cohort"] {
		return fmt.Errorf("workflow cohort snapshot fingerprint mismatch")
	}
	if string(current.UID) != snapshot.ChannelUID || current.Generation != snapshot.ChannelGeneration {
		return fmt.Errorf("input Channel configuration changed")
	}
	identity, err := loadWorkflowClassIdentity(ctx, c, &current)
	if err != nil {
		return err
	}
	if identity != snapshot.AgentClass {
		return fmt.Errorf("target AgentClass identity or generation changed")
	}
	if len(snapshot.Runs) == 0 {
		return fmt.Errorf("workflow snapshot has no prerequisites")
	}
	event := workflowEvent{Action: "completed", Run: snapshot.Runs[0]}
	event.Repository.FullName = snapshot.Repository
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ready, err := admitWorkflow(ctx, &current, body, provider, identity)
	if err != nil {
		return err
	}
	for _, admission := range ready.Ready {
		if admission.ChannelKey == b.Key && admission.External["trigger_cohort"] == b.External["trigger_cohort"] {
			return nil
		}
	}
	return fmt.Errorf("PR revision, base or workflow attempt cohort superseded: %s", ready.Reason)
}
