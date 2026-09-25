package agentsession_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
)

// userlessParentSession is a session whose inbound carried no human: it has no
// started-by identity of any kind, and the subject it acts as is the one its
// input Channel declared.
func userlessParentSession(t *testing.T) *v1alpha1.AgentSession {
	t.Helper()
	return &v1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "webhook-parent",
			Namespace: "ns",
			Annotations: map[string]string{
				v1alpha1.AnnotationAuthzServiceSubject: "service:demo-reviewbot-github",
			},
		},
		Spec: v1alpha1.AgentSessionSpec{
			Class: "demo",
			InputChannel: &v1alpha1.ChannelBinding{
				Name: "gh", Kind: "github", Key: "pr:demo-org/platform#42",
				NATSSubjectPrefix: "ap.sess.ns.webhook-parent",
			},
		},
	}
}

// TestBuildChildSession_RestartCarriesTheServiceSubjectForward pins the
// restart-survival half of the webhook-session subject fix. A child that lost
// the annotation would come back up with no acting principal at all, so every
// governed tool call after the restart would authorize as nobody — the exact
// defect the annotation exists to close, reintroduced by the one event most
// likely to follow a failure.
//
// It rides the same carry list as the started-by trio, and for the same
// reason: restart and inherit continue the SAME session's identity.
func TestBuildChildSession_RestartCarriesTheServiceSubjectForward(t *testing.T) {
	child := agentsession.BuildChildSession(userlessParentSession(t), &v1alpha1.PendingRestart{
		CutTurnIndex:      3,
		TargetSessionName: "webhook-parent-fkabc",
	})
	assert.Equal(t, "service:demo-reviewbot-github",
		child.Annotations[v1alpha1.AnnotationAuthzServiceSubject],
		"the child acts as the same non-human subject its parent did")
}

// TestBuildChildSession_TakeoverDoesNotCarryTheServiceSubject is the other
// side of the same rule the started-by branch already follows: a takeover
// hands the thread to a DIFFERENT principal, and the child is theirs. Carrying
// the parent's service subject would leave a fallback armed behind the new
// owner, ready to attribute their work to a service identity the moment their
// own attribution went missing — which is exactly when the call must deny.
func TestBuildChildSession_TakeoverDoesNotCarryTheServiceSubject(t *testing.T) {
	child := agentsession.BuildChildSession(userlessParentSession(t), &v1alpha1.PendingRestart{
		CutTurnIndex:       3,
		Mode:               v1alpha1.PendingRestartModeTakeover,
		NewOwnerExternalID: "U-NEWOWNER",
		TriggeredBy:        "user:bmV3b3duZXJAZXhhbXBsZS50ZXN0",
		TargetSessionName:  "webhook-parent-fkdef",
	})
	assert.NotContains(t, child.Annotations, v1alpha1.AnnotationAuthzServiceSubject,
		"a taken-over session belongs to its new human owner, not to the parent's service identity")
	assert.Equal(t, "user:bmV3b3duZXJAZXhhbXBsZS50ZXN0",
		child.Annotations[v1alpha1.AnnotationStartedByCanonicalID],
		"the new owner is the child's starter")
}
