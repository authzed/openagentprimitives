package channelcmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/aptest"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func demoChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-channel", Namespace: "default"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind:       "fake",
			AgentClass: "demo-agent",
		},
	}
}

// showOutput runs `oap channel show demo-channel` against a controller client
// the caller has finished configuring, and returns everything it wrote.
func showOutput(t *testing.T, c client.Client) string {
	t.Helper()
	b := &kube.Bundle{Controller: c, Namespace: "default"}
	return aptest.Run(t, newChannelShowCmd(aptest.GlobalsFor(b)), "demo-channel")
}

// TestChannelShow_SessionListFailureIsSurfaced: the recent-sessions section is
// supplementary, so a List failure must not abort the command — but it must
// not read as "no sessions" either. A channel that is busy and a channel the
// CLI could not ask about are different facts, and the user is entitled to
// know which one they are looking at.
func TestChannelShow_SessionListFailureIsSurfaced(t *testing.T) {
	c := aptest.ClientBuilder(t).
		WithObjects(demoChannel()).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, _ ...client.ListOption) error {
				if _, ok := list.(*spiceboxv1alpha1.AgentSessionList); ok {
					return errors.New("the apiserver said no")
				}
				return nil
			},
		}).Build()

	out := showOutput(t, c)
	assert.Contains(t, out, "demo-channel", "the Channel the user asked for still prints")
	assert.Contains(t, out, "warning:", "the failure is surfaced, not swallowed")
	assert.Contains(t, out, "the apiserver said no", "the warning carries the underlying cause")
	assert.NotContains(t, out, "Recent sessions:", "no section header for a list that never arrived")
}

// TestChannelShow_NoSessionsIsQuiet is the other half: an empty result is not
// a failure and must not warn, or the warning stops meaning anything.
func TestChannelShow_NoSessionsIsQuiet(t *testing.T) {
	out := showOutput(t, aptest.ClientBuilder(t).WithObjects(demoChannel()).Build())
	assert.Contains(t, out, "demo-channel", "the Channel prints")
	assert.NotContains(t, out, "warning:", "an idle channel is not an error")
	assert.NotContains(t, out, "Recent sessions:", "no section for zero sessions")
}
