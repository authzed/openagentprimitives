package pipeline

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ApplyApprovalStatusForTest exposes applyApprovalStatus for integration tests
// in package pipeline_test.
func ApplyApprovalStatusForTest(ctx context.Context, c client.Client, sess, original *spiceboxv1alpha1.AgentSession) error {
	return applyApprovalStatus(ctx, c, sess, original)
}
