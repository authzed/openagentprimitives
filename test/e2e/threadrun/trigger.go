//go:build e2e

package threadrun

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// triggerChannelKind is the one input kind the driver knows how to deliver to.
//
// Named as a constant rather than inlined because two places read it — the
// setup guard and its failure message — and because a second kind arriving is
// meant to be a visible edit here, not a silent widening: signing, headers and
// the fixture provider API are all kind-specific, and a bundle that named an
// unsupported kind must fail loudly rather than deliver something unsigned.
const triggerChannelKind = "github"

// setupTrigger prepares the fixture's INPUT Channel for a signed delivery, and
// returns it.
//
// Runs immediately after the harness boots and BEFORE any readiness barrier:
// it patches the App private key and webhook secret into the Channel's own
// credentials Secret, and a Channel whose credentials are still placeholders
// cannot become usable. Also stands up the fixture provider API and the
// production webhook route handler, and points the runner's trigger-status
// surface at the former — all of which must exist before the first session
// spawns, because the session's own tools resolve through them.
func setupTrigger(t *testing.T, h *e2e.Harness, b bt.Bundle) *spiceboxv1alpha1.Channel {
	t.Helper()

	var ch spiceboxv1alpha1.Channel
	require.NoError(t, h.K8s.Get(context.Background(),
		client.ObjectKey{Namespace: h.Namespace(), Name: b.Trigger.Channel}, &ch),
		"trigger.channel %q must name a Channel the fixture applies", b.Trigger.Channel)
	require.Equal(t, triggerChannelKind, ch.Spec.Kind,
		"the driver can only sign and deliver for kind %q; Channel %q is kind %q",
		triggerChannelKind, ch.Name, ch.Spec.Kind)
	require.NotNil(t, ch.Spec.CredentialsRef,
		"a trigger Channel needs a credentialsRef; the webhook signature is verified against its Secret")

	h.WireGitHubCredentials(t, ch.Namespace, ch.Spec.CredentialsRef.SecretName)
	if b.Trigger.HeadSHA != "" {
		h.FakeGitHub.SetHeadSHA(b.Trigger.HeadSHA)
	}
	wireGitHubAppIdentities(t, h)
	return &ch
}

// wireGitHubAppIdentities makes every type=githubApp credential the fixture
// declares resolvable, against the same stand-in provider the trigger surface
// is pointed at.
//
// Driven off the applied AgentIdentity CRs rather than off a bundle field: what
// the fixture DECLARES is what the broker will be asked to resolve, and a bundle
// restating it could name a credential the manifests do not carry (silently
// wiring nothing) or miss one they do (a dispatch failure many steps later, with
// the identity reporting healthy).
//
// A no-op for a fixture with no such credential, which is every bundle but the
// captured GitHub App agent.
func wireGitHubAppIdentities(t *testing.T, h *e2e.Harness) {
	t.Helper()
	var identities spiceboxv1alpha1.AgentIdentityList
	require.NoError(t, h.K8s.List(context.Background(), &identities, client.InNamespace(h.Namespace())))

	for i := range identities.Items {
		for _, cred := range identities.Items[i].Spec.Credentials {
			if cred.Type != githubAppCredentialType || cred.GitHubApp == nil {
				continue
			}
			h.WireGitHubAppIdentity(t, identities.Items[i].Namespace, cred.GitHubApp.SecretRef.Name)
		}
	}
}

// githubAppCredentialType is the credkind type name this driver stands a minter
// up for. It is the one entry in bt.StandInMintedCredentialTypes that this
// function serves, and TestStandInMintedCredentialTypesAreAllServed is what
// keeps the two from drifting: a name added to that list with nothing here would
// turn a loud capture refusal into a silent replay failure.
const githubAppCredentialType = "githubApp"

// deliverTrigger posts the bundle's signed event and blocks until the round it
// spawned has ended.
//
// The signature is computed from the Channel's OWN Secret rather than from a
// constant the driver holds, so a bundle cannot pass with HMAC verification
// disabled — an unsigned or wrongly-signed delivery is refused by the
// production route before the pipeline sees it, and the status assertion below
// would report 401 rather than a missing check run.
//
// Waiting on the session's PHASE rather than on an agent reply is what a
// triggered run needs: SendUserMessage's caller blocks in ExpectAgentReply, but
// a trigger bundle asserts on a check run and a channel transcript, and both
// are only complete once the round has actually finished. A round the
// completion gate pushed back would otherwise be sampled mid-flight.
func deliverTrigger(t *testing.T, h *e2e.Harness, dir string, b bt.Bundle, ch *spiceboxv1alpha1.Channel) {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir, "payloads", b.Trigger.Payload))
	require.NoError(t, err, "trigger.payload")

	resp := h.PostWebhookEvent(t, ch, b.Trigger.Event, body, h.SignWebhook(t, ch, body))
	require.Equal(t, http.StatusAccepted, resp.StatusCode,
		"the production webhook route refused the delivery; nothing downstream ran")

	sessions := h.EventuallySessions(t, b.Trigger.ChannelKey, 1, 60*time.Second)
	require.Len(t, sessions, 1)
	h.WaitForSessionPhase(sessions[0].Namespace, sessions[0].Name,
		spiceboxv1alpha1.AgentSessionPhaseIdle, 90*time.Second)
}

// checkTriggerStatus asserts on what the run left on the triggering resource,
// read from the fixture provider.
//
// The owner/repo it addresses come from the bundle's own channelKey, which is
// the same string the trigger-status surface parses to decide where to write —
// so an assertion here and the code under test cannot disagree about which pull
// request the session was about.
func checkTriggerStatus(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	ts := b.Assert.TriggerStatus
	if ts == nil {
		return
	}
	require.NotNil(t, b.Trigger, "assert.triggerStatus needs a trigger to assert about")

	owner, repo := ownerRepoFromChannelKey(t, b.Trigger.ChannelKey)
	run := h.FakeGitHub.CheckRunIn(owner, repo, b.Trigger.HeadSHA, b.Trigger.StatusName)
	require.NotNil(t, run,
		"nothing was published for %s/%s@%s under name %q — the review answered nobody",
		owner, repo, b.Trigger.HeadSHA, b.Trigger.StatusName)

	if ts.Status != "" {
		assert.Equal(t, ts.Status, run.Status,
			"a review that finished must not leave the status in progress")
	}
	if ts.Conclusion != "" {
		assert.Equal(t, ts.Conclusion, run.Conclusion,
			"the framework outcome maps onto the provider's own vocabulary in the kind, not in the prompt")
	}
	if ts.ExternalID != "" {
		assert.Equal(t, ts.ExternalID, run.ExternalID,
			"the commit answered for is recorded by the kind; the agent never supplied it")
	}
	for _, want := range ts.SummaryContains {
		if assert.NotNil(t, run.Output, "the published status carried no output block") {
			assert.Contains(t, run.Output.Summary, want,
				"the summary a human reads on the pull request must carry this")
		}
	}

	creates, updates := h.FakeGitHub.CheckRunCalls()
	if ts.Creates > 0 {
		assert.Equal(t, ts.Creates, creates,
			"the claim opens the status exactly once; a second create means the conclusion "+
				"lost track of the run the claim opened and stranded it")
	}
	if ts.Updates > 0 {
		assert.Equal(t, ts.Updates, updates,
			"the conclusion patches the run the claim opened; zero updates is a check "+
				"left in progress, which is the defect this surface exists for")
	}
}

// ownerRepoFromChannelKey splits "pr:<owner>/<repo>#<n>".
//
// A parse rather than two more bundle fields: the channelKey is already the
// bundle's statement of which resource the run is about, and a second spelling
// of the same fact is a second thing to keep in step.
func ownerRepoFromChannelKey(t *testing.T, key string) (owner, repo string) {
	t.Helper()
	rest, ok := strings.CutPrefix(key, "pr:")
	require.True(t, ok, "channelKey %q does not name a pull request", key)
	full, _, ok := strings.Cut(rest, "#")
	require.True(t, ok, "channelKey %q does not name a pull request number", key)
	owner, repo, ok = strings.Cut(full, "/")
	require.True(t, ok && owner != "" && repo != "",
		"channelKey %q does not name an owner/repository", key)
	return owner, repo
}

// checkChannelPosts asserts on what the bound OUTPUT channel actually received.
//
// Reads the surface's own record rather than the outbound bus: an envelope that
// was published and never rendered told nobody anything, and the gap between
// those two is where a missing sub-channel sender hides. It is the same
// reasoning checkNoticesPublished rests on, one surface over.
func checkChannelPosts(t *testing.T, h *e2e.Harness, b bt.Bundle) {
	t.Helper()
	ca := b.Assert.Channel
	if ca == nil {
		return
	}
	channelID := outputSlackChannelID(t, h)

	// postCount is the BARRIER, not merely a claim: a post travels runner →
	// NATS → the channelsd relay → the sender, asynchronously with the tool
	// call that raised it, so the last of them can still be in flight when the
	// session's own phase has settled. Without a count to wait for, a
	// postsContain assertion would sample the transcript mid-delivery and fail
	// intermittently on a perfectly correct run.
	require.NotZero(t, ca.PostCount,
		"assert.channel needs a postCount: it is what waits for delivery to finish, "+
			"and every other claim here reads the posts it waited for")
	posts := h.EventuallySlackPostsIn(t, channelID, ca.PostCount, 30*time.Second)

	var joined strings.Builder
	for _, p := range posts {
		joined.WriteString(p.Text)
		joined.WriteString("\n")
	}
	for _, want := range ca.PostsContain {
		assert.Contains(t, joined.String(), want,
			"no post on the bound output channel carried this; the run said it to the model and to nobody else")
	}

	if ca.ThreadRootedByFirstPost {
		require.NotEmpty(t, posts, "no posts at all, so nothing rooted the thread")
		root := h.SlackThreadTSIn(t, channelID, 0)
		assert.Empty(t, posts[0].ThreadTS,
			"the first post must BE the thread root; it arrived as a reply to something else")
		for i := 1; i < len(posts); i++ {
			assert.Equal(t, root, h.SlackThreadTSIn(t, channelID, i),
				"post %d landed outside the thread the session opened; to a human in a busy "+
					"channel that reads as an unrelated message, and its own text would not show it", i)
		}
	}
}

// outputSlackChannelID resolves the id the fixture's OUTPUT Channel posts into,
// off the Channel CR itself.
//
// Derived rather than declared in the bundle: the CR is what the sender
// actually reads, so a bundle restating the id could assert against a channel
// nothing posted to and fail with "0 posts" while the run was working
// perfectly.
func outputSlackChannelID(t *testing.T, h *e2e.Harness) string {
	t.Helper()
	var channels spiceboxv1alpha1.ChannelList
	require.NoError(t, h.K8s.List(context.Background(), &channels, client.InNamespace(h.Namespace())))

	var ids []string
	for i := range channels.Items {
		ch := &channels.Items[i]
		if ch.Spec.Kind != "slack" || ch.Spec.Slack == nil || ch.Spec.Slack.OutputDefaults == nil {
			continue
		}
		if id := ch.Spec.Slack.OutputDefaults.ChannelID; id != "" {
			ids = append(ids, id)
		}
	}
	require.Len(t, ids, 1,
		"assert.channel reads the one kind=slack output Channel's channelId; the fixture declares %d", len(ids))
	return ids[0]
}
