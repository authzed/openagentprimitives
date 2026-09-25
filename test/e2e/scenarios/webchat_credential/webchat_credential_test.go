//go:build e2e

// Package webchat_credential_test is the founding regression for the
// webchat-credential-hang bug (commit e6fcbab6, "fix(channelsd): publish
// credential prompts as interactions (fixes webchat hang)"). Before that
// fix, CredentialRequestWatcher delivered the "connect your accounts"
// prompt by calling SubChannelSenderFor(ctx, sess, "credential_request")
// directly against the session's bound channel kind. Client-hosted kinds
// (pkg/channels/channelkinds/browser — the browser webchat surface — and
// pkg/channels/channelkinds/local — the TUI) never implemented a "credential_request"
// sub-channel sender, so that call silently returned nil and the prompt was
// dropped: the session sat in AwaitingCredentials forever with nothing ever
// rendered to the user ("the webchat hang"). Task 6 flipped delivery to a
// NATS-published interaction_request(credential_link) envelope on the
// session's .out subject, resolved by the outbound relay's "interaction"
// sub-channel — a sub-channel every channel kind implements uniformly, so
// no kind can silently drop it anymore.
//
// This test proves that: a session on a channel-attached surface parks
// AwaitingCredentials, and the credential_link interaction_request IS
// delivered (recorded) — the prompt no longer hangs.
//
// Kind choice: the e2e harness's outbound relay (test/e2e/sender_resolver.go)
// only routes to RelayedByChannelsd()==true kinds — the harness never stands
// up a webd/browser.Host or a TUI/local.Host (those run OUTSIDE channelsd in
// production; browser's own websocket-delivery path is exercised by
// pkg/web/webui/chat's own lighter, fake-K8s-client test harness, not this
// envtest+SpiceDB one — see pkg/web/webui/chat/webchat_e2e_test.go). The fake
// kind is this harness's stand-in for "a channel-attached session whose
// delivery goes through NATSPublish + the outbound relay's generic
// 'interaction' sub-channel resolution" — exactly the mechanism the fix
// generalized to every kind, including the ones that never had a
// credential_request sender. Repairing the 4 broken passthrough_* e2e
// scenarios (also Task 8) exercises the identical mechanism this regression
// isolates in minimal form.
//
// No real names: alice / Helpdesk Bot / Notion / example.com are all
// fictional per AGENTS.md.
package webchat_credential_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human starter the test impersonates.
	starterEmail = "alice@example.com"

	// notionCred is the credential the AgentClass's referenced MCPServer
	// (notion-mcp in manifests.yaml) declares via spec.auth.credential.
	notionCred = "notion-oauth"

	// notionProvider is the user-visible service label the operator's
	// explanation builder resolves from MCPServer.spec.auth.title.
	notionProvider = "Notion"

	// signingKey is the 32-byte HMAC key the in-test CredentialRequestWatcher
	// signs deep-links with. No identityd runs in this scenario — the link
	// is only rendered INTO the interaction_request, never dialed — so any
	// well-formed key + ExternalBaseURL suffices.
	signingKey = "webchat-credential-regression-key32" // 32 bytes

	// externalBaseURL is a static, well-formed identityd-style base URL.
	// doPublish's mint path only requires it to be non-empty; nothing in
	// this test dials it.
	externalBaseURL = "https://identityd.example.com"
)

// TestCredentialPromptDeliveredOnClientHostedSession is the founding
// regression: proves a credential_link interaction_request is delivered
// (recorded) for a parked session, instead of being silently dropped. See
// the package doc comment for why this guards the webchat-credential-hang
// bug and why the harness uses the fake kind as its channelsd-routed
// stand-in for a client-hosted surface.
func TestCredentialPromptDeliveredOnClientHostedSession(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(string(manifests))

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that h.MCP.URL() is known — the probe must
	// hit the in-process stub, not the {{MCP_URL}} sentinel.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Wire the CredentialRequestWatcher with a real NATSPublish so its
	// publish actually reaches the harness's already-running outbound
	// relay (subscribed to ap.session.*.*.out.>) — exactly the mechanism
	// Task 6 introduced and Task 8 repaired across the 4 passthrough_*
	// scenarios. See dialHarnessNATS.
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-webchat-cred-regression")
	startCredentialRequestWatcher(t, h.K8s, signer, externalBaseURL, natsConn.Publish)

	// Minimal LLM script: the session never actually reaches the runner in
	// this test (it parks before one is spawned), but ScriptedLLM fatals on
	// any unmatched request, so keep a vacuous safety-net rule matching the
	// convention of the other passthrough_* scenarios.
	h.LLM.OnUserMessage("connect my notion").Reply(e2e.RespondToUser("ok"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid("helpdesk-bot", 30*time.Second)

	// Drive the inbound that spawns the parked session.
	h.SendUserMessage("connect my notion")

	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err)
	starterCanonical := starterCanon.Subject()
	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)

	var sui spiceboxv1alpha1.SessionUserIdentity
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &sui),
		"get SessionUserIdentity %s/%s", sess.Namespace, sess.Name)
	assert.Equal(t, []string{notionCred}, sui.Status.MissingCredentials,
		"MissingCredentials must list the unlinked credential")

	// THE REGRESSION ASSERTION: the credential_link interaction_request must
	// actually be delivered — recorded on the fake driver via the
	// NATS-publish + outbound-relay + "interaction" sub-channel path.
	// Before Task 6's fix, the equivalent direct-send call
	// (SubChannelSenderFor("credential_request")) returned nil for any
	// channel kind that never implemented that specific sub-channel
	// (browser/webchat, local/TUI), and the prompt was silently dropped —
	// the session hung in AwaitingCredentials with nothing ever rendered to
	// the user. A timeout inside waitForCredentialPrompt below is exactly
	// what this test exists to catch.
	drv := fake.DriverFor(sess.Namespace, sess.Spec.InputChannel.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		sess.Namespace, sess.Spec.InputChannel.Name)
	rec := waitForCredentialPrompt(t, ctx, drv, sess.Namespace+"/"+sess.Name)

	assert.Equal(t, categories.CredentialLink, rec.Payload.Category)
	assert.Equal(t, sess.Namespace+"/"+sess.Name,
		rec.Payload.AgentSessionRef.Namespace+"/"+rec.Payload.AgentSessionRef.Name)
	require.NotNil(t, rec.Payload.Audience.Requester,
		"credential_link interaction_request must address a requester")
	reqCanon, canonErr := rec.Payload.Audience.Requester.Principal().AllowSynthetic().Canonical()
	require.NoError(t, canonErr, "credential_link requester canonical")
	assert.Equal(t, starterCanon, reqCanon, "credential_link interaction_request's requester is the starter")
	require.Len(t, rec.Payload.Actions, 1,
		"credential_link interaction_request carries exactly one connect action")
	linkAction := rec.Payload.Actions[0]
	assert.Equal(t, channelevents.ActionKindLink, linkAction.Kind)
	assert.True(t, strings.HasPrefix(linkAction.URL, externalBaseURL+"/link?d="),
		"link action URL should be a fully-formed /link URL: got %q", linkAction.URL)
	require.Len(t, rec.Payload.Fields, 1,
		"Fields has one entry per missing credential — no filtering")
	assert.Equal(t, notionProvider, rec.Payload.Fields[0].Label)
}

// ----- helpers -----------------------------------------------------------

// startCredentialRequestWatcher constructs a pipeline.CredentialRequestWatcher
// pointed at the test's signer + external base URL, then runs it in a
// goroutine bound to its own context. Senders is deliberately left nil: per
// CredentialRequestWatcher.Senders's doc comment, it is unused by the
// watcher's own publish path post-Task-6-flip (delivery goes entirely
// through NATSPublish + the outbound relay's "interaction" sub-channel
// resolution).
func startCredentialRequestWatcher(
	t *testing.T,
	c client.Client,
	signer *passthroughlink.Signer,
	baseURL string,
	natsPublish channelevents.PublishFunc,
) {
	t.Helper()
	w := &pipeline.CredentialRequestWatcher{
		K8s:             c,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return baseURL },
		NATSPublish:     natsPublish,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Logf("credential request watcher did not exit within 2s")
		}
	})
}

// dialHarnessNATS opens a fresh *nats.Conn against the e2e harness's own
// embedded NATS server (h.NATSURL). The harness's outbound relay already
// subscribes to ap.session.*.*.out.> on its own (unexported) connection —
// scenario tests can't reach that connection directly, but NATS pub/sub
// works fine across separate client connections to the same embedded
// server, so a watcher publishing on this connection is still picked up
// and routed to the fake kind's "interaction" sub-channel sender. Mirrors
// the dial pattern used by the repaired passthrough_* scenarios (Task 8)
// and test/e2e/scenarios/passthrough_revoke_propagation.
func dialHarnessNATS(t *testing.T, natsURL, name string) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.Name(name))
	require.NoError(t, err, "dial harness NATS as %q", name)
	t.Cleanup(func() {
		if err := conn.Drain(); err != nil {
			t.Logf("%s: nats drain on cleanup: %v", name, err)
		}
	})
	return conn
}

// waitForParkedSession polls until an AgentSession in the default namespace
// reaches AwaitingCredentials AND carries the expected starter annotation.
// Returns the parked session. Mirrors the same helper across the other
// passthrough_* scenarios.
func waitForParkedSession(t *testing.T, ctx context.Context, c client.Client, starterCanonical identity.Subject) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last spiceboxv1alpha1.AgentSession
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			t.Logf("waitForParkedSession: list: %v", err)
			time.Sleep(150 * time.Millisecond)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			last = *s
			if s.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID] != starterCanonical.String() {
				continue
			}
			if s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials {
				return s
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("waitForParkedSession: no session reached AwaitingCredentials within 30s; last observed: %s/%s phase=%q starter=%q",
		last.Namespace, last.Name, last.Status.Phase,
		last.Annotations[spiceboxv1alpha1.AnnotationStartedByCanonicalID])
	return nil
}

// waitForCredentialPrompt polls the fake Driver's recorded interaction-prompt
// queue until a credential_link interaction_request for sessionRef appears.
// Returns the recorded entry. A timeout here is precisely the failure mode
// this regression exists to catch — see the package + test doc comments.
func waitForCredentialPrompt(t *testing.T, ctx context.Context, drv *fake.Driver, sessionRef string) fake.InteractionPrompt {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range drv.InteractionPrompts() {
			if r.Payload.Category != categories.CredentialLink {
				continue
			}
			if r.Payload.AgentSessionRef.Namespace+"/"+r.Payload.AgentSessionRef.Name == sessionRef {
				return r
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waitForCredentialPrompt: context cancelled before envelope for %s arrived", sessionRef)
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("waitForCredentialPrompt: no credential_link interaction_request for %s within 30s (recorded %d total) — THE PROMPT WAS DROPPED",
		sessionRef, len(drv.InteractionPrompts()))
	return fake.InteractionPrompt{}
}

// splitMCPServerFromManifests separates the MCPServer YAML document from the
// rest of the multi-doc YAML. Mirrors the helper in the other passthrough_*
// scenarios — the MCPServer's spec.server.url contains the {{MCP_URL}}
// sentinel substituted with h.MCP.URL() after the harness has started.
func splitMCPServerFromManifests(yamlBlob string) (mcpServerDoc, remaining string) {
	docs := strings.Split(yamlBlob, "\n---")
	var mcp, rest []string
	for _, doc := range docs {
		if strings.Contains(doc, "kind: MCPServer") {
			mcp = append(mcp, doc)
		} else {
			rest = append(rest, doc)
		}
	}
	return strings.Join(mcp, "\n---"), strings.Join(rest, "\n---")
}
