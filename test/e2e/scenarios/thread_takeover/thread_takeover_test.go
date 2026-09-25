//go:build e2e

package thread_takeover_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	tkChannelID = "C-takeover"
	aliceID     = "U-alice"
	aliceEmail  = "alice@example.com"
	bobID       = "U-bob"
	bobEmail    = "bob@example.com"
)

func seedUser(h *e2e.Harness, id, email string) {
	h.SlackFake().SeedUser(&slackapi.User{
		ID:      id,
		TeamID:  "T-test",
		Profile: slackapi.UserProfile{Email: email},
	})
}

// splitMCP separates the MCPServer doc (its url is templated with {{MCP_URL}}
// and applied after the harness's MCP stub is up) from the rest.
func splitMCP(yamlBlob string) (mcp, rest string) {
	var m, r []string
	for _, doc := range strings.Split(yamlBlob, "\n---") {
		if strings.Contains(doc, "kind: MCPServer") {
			m = append(m, doc)
		} else {
			r = append(r, doc)
		}
	}
	return strings.Join(m, "\n---"), strings.Join(r, "\n---")
}

func canonicalOf(t *testing.T, email string) string {
	t.Helper()
	c, err := identity.EmailReference(identity.Email(email)).Canonical()
	require.NoError(t, err, "canonicalize %s", email)
	return c.Subject().String()
}

// sessionOwnedBy returns the AgentSession whose started-by-canonical annotation
// matches subject, or nil if none exists yet.
func sessionOwnedBy(t *testing.T, h *e2e.Harness, subject string) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(context.Background(), &list, client.InNamespace("default")), "list AgentSessions")
	for i := range list.Items {
		if list.Items[i].Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] == subject {
			return &list.Items[i]
		}
	}
	return nil
}

// The nudgeAll helper that used to live here — bumping a benign annotation on
// every AgentSession inside both require.Eventually bodies, four times a second,
// to force operator reconciles — has been deleted. It was documented as
// UNVERIFIED and asked to be measured; it was, and it does nothing:
//
//   - The credential-link deadline is self-triggering. The AwaitingCredentials
//     park returns ctrl.Result{RequeueAfter: remaining} (agentsession/
//     passthrough.go), so the operator wakes itself exactly when the deadline
//     elapses. No external nudge is needed to make the sweep fire.
//   - The trigger gap the identity tier's copy of this idiom was masking is
//     already closed: the harness's placeholder runner Pod now carries an
//     ownerReference (inprocess_runner_factory.go), so Owns(&corev1.Pod{})
//     enqueues the session.
//   - Bob's takeover is driven by the Slack inbound pipeline, which is
//     event-driven end to end.
//
// It also swallowed every Update error (`_ =`), against the repo's
// no-silent-errors rule, and each Update rewrote a spec-side annotation on a
// live object four times a second — churn that races the controller's own
// writes rather than helping them.
// TestThreadTakeover_CredsTimeout_DifferentUserBecomesOwner is the headline
// scenario over the real Slack path: alice starts a session that times out
// waiting for credentials, then bob @mentions the SAME thread and takes it over
// in a new session HE owns, with an in-thread notice.
func TestThreadTakeover_CredsTimeout_DifferentUserBecomesOwner(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")
	mcpDoc, rest := splitMCP(string(raw))

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{rest},
		DefaultTimeout: 90 * time.Second,
	})
	h.ApplyManifest(strings.ReplaceAll(mcpDoc, "{{MCP_URL}}", h.MCP.URL()))
	h.WaitForAgentClassValid("tk-agent", 45*time.Second)

	seedUser(h, aliceID, aliceEmail)
	seedUser(h, bobID, bobEmail)
	aliceCanonical := canonicalOf(t, aliceEmail)
	bobCanonical := canonicalOf(t, bobEmail)

	// 1. Alice @mentions the bot → session parks AwaitingCredentials (the MCP
	//    server needs linear-oauth she never links) → fails after the 5s
	//    credentialLinkTimeout.
	rootTS := h.SendSlackMention(aliceID, tkChannelID, "@bot look at my tickets")

	// The credential-link-timeout path writes Phase=Failed + FailureReason
	// (not the Failed condition), so assert on those. The operator wakes itself
	// when the deadline elapses (RequeueAfter from the AwaitingCredentials
	// park), so this only has to poll.
	var aliceSess *spiceboxv1alpha1.AgentSession
	require.Eventually(t, func() bool {
		aliceSess = sessionOwnedBy(t, h, aliceCanonical)
		return aliceSess != nil &&
			aliceSess.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseFailed &&
			aliceSess.Status.FailureReason == spiceboxv1alpha1.ReasonCredentialLinkTimeout
	}, 60*time.Second, 250*time.Millisecond, "alice's session must fail with CredentialLinkTimeout")

	// 2. Bob @mentions the SAME thread → different-user takeover: a NEW session
	//    bob owns, in the same thread.
	h.SendSlackMentionInThread(bobID, tkChannelID, "@bot let me take this over", rootTS)

	var bobSess *spiceboxv1alpha1.AgentSession
	require.Eventually(t, func() bool {
		bobSess = sessionOwnedBy(t, h, bobCanonical)
		return bobSess != nil && bobSess.Name != aliceSess.Name
	}, 90*time.Second, 250*time.Millisecond, "bob must own a NEW session after taking over the terminal thread")

	assert.NotEqual(t, aliceSess.Name, bobSess.Name, "takeover creates a new session, distinct from alice's")
	assert.Equal(t, bobCanonical, bobSess.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID],
		"the new session is owned by bob (the taking-over user)")
	require.NotNil(t, bobSess.Spec.InputChannel)
	assert.Equal(t, aliceSess.Name, bobSess.Spec.InputChannel.InheritFrom,
		"the takeover child records its terminal predecessor")

	// 3. An in-thread takeover notice was posted (ordinary terminal → inheriting
	//    takeover wording).
	found := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && !found {
		for _, m := range h.SlackFake().Replies(tkChannelID, rootTS) {
			if strings.Contains(m.Text, "new session") || strings.Contains(m.Text, "continuing") {
				found = true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	assert.True(t, found, "an in-thread takeover notice must be posted under the thread root")
}
