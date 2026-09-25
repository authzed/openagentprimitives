package podspec_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/podspec"
)

func TestWorkspaceClaimName(t *testing.T) {
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "review-bot", Namespace: "default"},
	}
	assert.Equal(t, "review-bot-workspace", podspec.WorkspaceClaimName(sess))
}
