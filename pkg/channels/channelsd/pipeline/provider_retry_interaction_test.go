// pkg/channels/channelsd/pipeline/provider_retry_interaction_test.go
//
// decideProviderRetry — the bound decision handler for the
// provider_error_retry interaction category. Tests call p.decideProviderRetry
// directly (not through HandleInteractionDecision), since the handler performs
// no standing check of its own (the generic pipe already ran the category's
// DecideParticipant policy before invoking it — see the doc comment on
// decideProviderRetry). It just stamps the wake annotation via annotateWake,
// which is itself conflict-safe and no-ops on a non-respawn-eligible phase.
package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
)

// TestDecideProviderRetry verifies the bound handler stamps the wake annotation
// on a respawn-eligible (AwaitingRetry) session so the operator respawns the
// exited runner, and always returns Outcome{resolved, "🔄 Retrying…"} — the
// generic applied path renders that as the in-place edit that strips the button.
// On a non-respawn-eligible phase (Failed/Running) annotateWake is a logged
// no-op: no annotation is written and no error is raised (the handler does not
// add its own phase check — the phase gate lives in respawnOnWake).
func TestDecideProviderRetry(t *testing.T) {
	cases := []struct {
		name          string
		phase         string
		wantAnnotated bool
	}{
		{
			name:          "AwaitingRetry (respawn-eligible): wake annotation written",
			phase:         spiceboxv1alpha1.AgentSessionPhaseAwaitingRetry,
			wantAnnotated: true,
		},
		{
			name:          "Failed (not respawn-eligible): logged no-op, no annotation, no error",
			phase:         spiceboxv1alpha1.AgentSessionPhaseFailed,
			wantAnnotated: false,
		},
		{
			name:          "Running (live pod, not respawn-eligible): logged no-op, no annotation, no error",
			phase:         spiceboxv1alpha1.AgentSessionPhaseRunning,
			wantAnnotated: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Name: "retry-sess", Namespace: "default"},
				Status:     spiceboxv1alpha1.AgentSessionStatus{Phase: tc.phase},
			}
			p, _, _, _, cli := newPipeline(t, sess)

			out, err := p.decideProviderRetry(context.Background(), channelinteractions.Decision{
				Session: channelevents.SessionRef{Namespace: sess.Namespace, Name: sess.Name},
				Payload: channelevents.InteractionDecisionPayload{
					Category:   categories.ProviderErrorRetry,
					RequestRef: "provider-error-retry-default-retry-sess-0",
					ActionID:   "retry",
				},
			})
			require.NoError(t, err, "decideProviderRetry must never error on a resolvable session")
			assert.Equal(t, channelevents.OutcomeResolved, out.Result, "Outcome.Result")
			assert.Equal(t, "🔄 Retrying…", out.OutcomeText, "Outcome.OutcomeText (the applied in-place edit)")
			require.NoError(t, out.Validate(), "decideProviderRetry must return a valid Outcome")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, cli.Get(context.Background(), client.ObjectKeyFromObject(sess), &got), "Get after decideProviderRetry")
			if tc.wantAnnotated {
				assert.NotEmpty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
					"respawn-eligible session must get the wake annotation")
			} else {
				assert.Empty(t, got.Annotations[spiceboxv1alpha1.AnnotationWakeRequestedAt],
					"non-respawn-eligible session must NOT get the wake annotation (logged no-op)")
			}
		})
	}
}
