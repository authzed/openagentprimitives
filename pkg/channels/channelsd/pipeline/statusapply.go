package pipeline

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentstatus"
)

// applyApprovalStatus persists the channelsd-owned approval surface changes
// (pending queues + *ApprovalPending conditions + interact-permission snapshot)
// by delegating to the shared agentstatus.WriteOwned helper. Only the fields
// that changed between original (as-read) and sess (mutated copy) are sent,
// so the operator's concurrent writes are never reverted.
func applyApprovalStatus(ctx context.Context, c client.Client, sess, original *spiceboxv1alpha1.AgentSession) error {
	return agentstatus.WriteOwned(ctx, c, sess, original, spiceboxv1alpha1.OwnerApprovals)
}
