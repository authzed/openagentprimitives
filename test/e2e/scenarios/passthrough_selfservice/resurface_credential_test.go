//go:build e2e

// Package passthrough_selfservice_test — Task 10 credential re-surface leg.
//
// TestResurfaceCredentialPromptOnReinteraction proves the AwaitingCredentials
// re-surface leg end-to-end: a session parked awaiting credentials gets its
// credential prompt RE-SENT when the starter re-interacts (conceptually from
// another device), driven by pipeline.resurfacePending's category-generic
// leg → the bound channelinteractions.Regenerator for credential_link
// (= CredentialRequestWatcher.ForcePublish, which bypasses the
// CredentialRequestPublished dedup condition).
//
// Task 8 rewrite (previously STALE since Task 6's flip to publishing
// interaction_request: pkg/channels/channelsd/pipeline/credential_request.go's
// doPublish now delivers via NATSPublish + the outbound relay's "interaction"
// sub-channel resolution, not a direct SubChannelSenderFor("credential_request")
// send). The watcher below is now constructed AFTER e2e.Start (not via
// PipelineExtender, which runs before h.NATSURL/h.K8s are reachable in a
// convenient form for this file) with a real NATSPublish dialed against the
// harness's embedded NATS server — see dialHarnessNATS +
// startCredentialRequestWatcher in passthrough_selfservice_test.go, shared by
// both files in this package. Assertions now poll
// Driver.InteractionPrompts() (Driver.CredentialRequests() is dead post-flip).
//
// Reuses this scenario's manifests + helpers (waitForParkedSession,
// waitForCredentialPrompt, dialHarnessNATS, startCredentialRequestWatcher,
// splitMCPServerFromManifests, testSubChannelResolver, starterEmail,
// signingKey). Unlike TestPassthroughSelfService this test never completes
// the link flow — it stops once the re-surface has fired, so no identityd
// server and no identities namespace are needed.
//
// No real names — alice@example.com (starterEmail) is fictional per AGENTS.md.
package passthrough_selfservice_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestResurfaceCredentialPromptOnReinteraction(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(string(manifests))

	// Shared HMAC signer for the credential deep-link. No identityd is started
	// in this scenario — the link is only rendered INTO the credential_request
	// envelope, never dialed — so a static, well-formed ExternalBaseURL suffices
	// (a non-empty URL is all doPublish's mint path requires).
	signer := passthroughlink.New([]byte(signingKey),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	const externalBaseURL = "https://identityd.example.com"

	// Context for the credential watcher's poll loop.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	// Apply the MCPServer now that h.MCP.URL() is known (the probe must hit the
	// in-process stub, not the {{MCP_URL}} sentinel).
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	// Wire a CredentialRequestWatcher whose poll loop (go w.Run) sends the
	// FIRST credential prompt when the operator parks the session (production
	// runs this via go crw.Run), with a real NATSPublish so the publish
	// actually reaches the fake driver via the harness's outbound relay
	// (see dialHarnessNATS).
	natsConn := dialHarnessNATS(t, h.NATSURL, "e2e-pt-resurface-crw")
	crw := startCredentialRequestWatcher(t, h.K8s, signer, externalBaseURL, natsConn.Publish)

	// Bind the credential_link regenerator exactly as internal/cmd/channelsd/main.go
	// does: resurfacePending's category-generic leg (Park: AwaitingCredentials,
	// Resurface: ResurfaceRegenerate on the credential_link category) finds the
	// cached interaction_request and calls this to re-mint + re-publish it —
	// crw.ForcePublish bypasses the CredentialRequestPublished dedup condition.
	// channelinteractions' regenerator registry is process-global (like the
	// channel-kind registry), so it must be reset on cleanup — otherwise a
	// second run of this test in the same process (-count=2, or a future
	// sibling test binding the same category) panics on the duplicate Bind.
	channelinteractions.BindRegenerator(categories.CredentialLink,
		func(ctx context.Context, sess *spiceboxv1alpha1.AgentSession, _ *channelevents.InteractionRequestPayload) error {
			return crw.ForcePublish(ctx, sess)
		})
	t.Cleanup(channelinteractions.ResetRegenerators)

	h.WaitForAgentClassValid("triage-bot", 30*time.Second)

	// First inbound spawns the session; the operator parks it AwaitingCredentials
	// (userPassthrough, linear-oauth unlinked) and writes the SUI + explanation.
	h.SendUserMessage("connect my linear")

	starterCanon, err := identity.EmailReference(starterEmail).Canonical()
	require.NoError(t, err, "canonicalize starter email")
	starterCanonical := starterCanon.Subject()
	sess := waitForParkedSession(t, ctx, h.K8s, starterCanonical)
	sessRef := sess.Namespace + "/" + sess.Name

	drv := fake.DriverFor(sess.Namespace, sess.Spec.InputChannel.Name)
	require.NotNil(t, drv, "fake.DriverFor(%s/%s) — listener may not have started",
		sess.Namespace, sess.Spec.InputChannel.Name)

	// The watcher's poll loop delivers the FIRST credential prompt.
	waitForCredentialPrompt(t, ctx, drv, sessRef)
	before := countCredentialPrompts(drv, sessRef)
	require.GreaterOrEqual(t, before, 1, "the watcher must have sent the initial credential prompt")

	// The starter's owner→interact grant hasn't landed on the parked session in
	// the harness: ResolveAndWriteOwners writes agentsession#owner=starter on
	// every operator reconcile, but a pod-less AwaitingCredentials session isn't
	// driven through that reconciliation here. Without the grant the second
	// inbound is denied by the interact check before it ever reaches
	// resurfacePending. Seed the grant directly (the same write the operator
	// makes) — this precondition is guaranteed in production.
	waitForStarterInteract(t, ctx, h, sess, starterCanon)

	// Second inbound from the same user (same default thread → same parked
	// session) while still AwaitingCredentials: resurfacePending's
	// category-generic leg → the credential_link Regenerator (=crw.ForcePublish,
	// bound above) re-sends the prompt. The poll loop won't add further sends
	// (the CredentialRequestPublished dedup condition is already stamped, so
	// ReconcileOne is a no-op), so any increase in the count is attributable to
	// the dedup-bypassing re-surface.
	h.SendUserMessage("i still need to connect")

	require.Eventually(t, func() bool {
		return countCredentialPrompts(drv, sessRef) > before
	}, 30*time.Second, 200*time.Millisecond,
		"a second credential prompt must be re-surfaced on re-interaction (initial count was %d)", before)
}

// waitForStarterInteract ensures the starter can interact with the parked
// session before the follow-up inbound. Production guarantees this: the
// operator's ResolveAndWriteOwners writes agentsession#owner=starter on every
// reconcile, and interact resolves as owner+participant−denied. A pod-less
// AwaitingCredentials session in the harness never reaches that grant, so we
// seed it directly via the same production write path (spicedb.Client.TouchOwner)
// — otherwise the follow-up inbound is denied by the interact check before it
// ever reaches resurfacePending. Fatals if interact doesn't resolve after seeding.
func waitForStarterInteract(t *testing.T, ctx context.Context, h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession, starterCanonID identity.CanonicalUserID) {
	t.Helper()
	sessRef := sess.Namespace + "/" + sess.Name
	require.NoError(t, h.SpiceDB.TouchOwner(ctx, sess.Namespace, sess.Name, "user:"+starterCanonID.String()),
		"seed agentsession#owner for the starter")
	require.Eventually(t, func() bool {
		resp, err := h.SpiceDB.CheckPermission(ctx, &v1.CheckPermissionRequest{
			Resource:    &v1.ObjectReference{ObjectType: "agentsession", ObjectId: sessRef},
			Permission:  "interact",
			Subject:     &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: "user", ObjectId: starterCanonID.String()}},
			Consistency: &v1.Consistency{Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true}},
		})
		return err == nil && resp.GetPermissionship() == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION
	}, 10*time.Second, 200*time.Millisecond, "starter must have interact after seeding agentsession#owner")
}

// countCredentialPrompts counts credential_link interaction_request envelopes
// recorded on the fake driver for sessionRef. Post-Task-6-flip counterpart of
// the old credential_request-sub-channel-based counter — see
// waitForCredentialPrompt's doc comment.
func countCredentialPrompts(drv *fake.Driver, sessionRef string) int {
	n := 0
	for _, r := range drv.InteractionPrompts() {
		if r.Payload.Category != categories.CredentialLink {
			continue
		}
		if r.Payload.AgentSessionRef.Namespace+"/"+r.Payload.AgentSessionRef.Name == sessionRef {
			n++
		}
	}
	return n
}
