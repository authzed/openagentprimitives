//go:build e2e

// Package credential_update_agent_owned_test is the end-to-end scenario for an
// agent asking a human to replace the agent's OWN shared credential.
//
// The sibling `credential_update` scenario covers a PERSON's credential: the
// card DMs its owner, and the click writes that person's master Secret. This
// one is the other half, and every step downstream of "whose credential died"
// is different:
//
//	who is asked      a standing role=monitoring admin surface, not the starter
//	who may act       agentidentity#update_credential, answered by a REAL SpiceDB
//	who performs it   the OPERATOR — webd holds no Secret access at all
//	what moves        the AgentIdentity's backing Secret, shared by every session
//
// # What only this tier can show
//
// The integration test in pkg/controllers/credentialupdaterequest drives the
// same components over one apiserver and one SpiceDB, but with no runner. What
// is added here is the loop closing around a live turn: a real LLM turn, a real
// upstream call that 401s, the real BLOCKING meta tool, a real browser POST to
// identityd carrying a real idd_session cookie, the operator's own permission
// check across a real HTTP hop, and the blocked call returning so the turn
// finishes.
//
// # The negative is the point
//
// TestAgentOwnedCredentialUpdate_ANonAdminClickIsRefusedAndNothingIsWritten is
// not a formality. The bug this slice fixed was not "the click was allowed" —
// it was that a click by ANYONE wrote the value into the CLICKER's own
// UserIdentity, showed them a success badge, left the shared credential dead,
// and let the request expire announcing that nobody had acted. So the negative
// asserts all four halves of that lie: no write to the bot's Secret, NO
// UserIdentity created for the clicker, a refusal rather than a success, and
// the request still Open.
package credential_update_agent_owned_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelinteractions/categories"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/controllers/agentsession"
	"github.com/authzed/openagentprimitives/pkg/controllers/credentialupdaterequest"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/broker/inproc"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
	"github.com/authzed/openagentprimitives/pkg/web/admind/agentcred"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the placeholder human who starts the session. They are
	// deliberately NOT a platform admin: an ordinary user cannot replace the
	// agent's shared credential, which is exactly why the card cannot simply
	// DM them the way the user-owned route does.
	starterEmail = "alice@example.com"
	// adminEmail is the platform admin who can. bystanderEmail is signed in and
	// holds nothing — the negative control.
	adminEmail     = "carol@example.com"
	bystanderEmail = "dave@example.com"

	className         = "ac-agentcred"
	channelName       = "ac-agentcred-fake"
	monitoringChannel = "ac-agentcred-monitor"
	agentIdentityName = "agentcred-support-bot"
	botSecretName     = "agentcred-bot-creds"
	credential        = "agentcred-upstream-token"
	mcpToolName       = "upmcp_list_issues"

	// deadValue is what the bot authenticates with today; freshValue is what an
	// admin pastes. Both are ghp_-shaped so they clear the provider's declared
	// token format — the question this scenario is about is whether they
	// AUTHENTICATE, which the redirected probe answers per-token.
	deadValue  = "ghp_deadSharedBotCredential0"
	freshValue = "ghp_freshSharedBotCredentl1"

	// signingKey stands in for the ONE passthrough-link HMAC key channelsd and
	// webd both hold. Unlike the sibling scenario this one really DOES dial the
	// minted link, so the key is shared between two differently-issued signers
	// exactly as production shares it between two components -- see
	// scenario.channelsdSigner.
	signingKey = "credential-update-agent-owned-e2e-signing-key"

	// admindToken stands in for the spicebox-admind-token the operator and webd
	// share. It gates every call to the operator's admin surface.
	admindToken = "credential-update-agent-owned-e2e-admind-token"

	scenarioTimeout = 60 * time.Second
)

// ---------------------------------------------------------------------------
// Scenario 1: an admin replaces the shared credential and the turn resumes
// ---------------------------------------------------------------------------

// TestAgentOwnedCredentialUpdate_AnAdminReplacesTheSharedBotCredentialAndTheTurnResumes
// drives the whole arc: the upstream 401s, the agent calls
// request_credential_update and BLOCKS, a card reaches the monitoring surface
// (NOT the starter), a platform admin opens the link and pastes a replacement,
// the operator authorizes and performs the write, the request Fulfils, the
// blocked call returns, and the turn finishes.
func TestAgentOwnedCredentialUpdate_AnAdminReplacesTheSharedBotCredentialAndTheTurnResumes(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	// The provider answers on the token it is given: the bot's current value is
	// dead, the replacement is live. Both directions matter — the first is the
	// operator's independent evidence that anyone should be asked at all, the
	// second is what lets identityd's own re-verification of the PASTED value
	// through to the write instead of stopping at the confirm page.
	redirectVerifyProbeByToken(t, map[string]int{deadValue: 401, freshValue: 200})

	var upstreamCalls atomic.Int32
	h.MCP.OnTool("list_issues", func(map[string]any) any {
		if upstreamCalls.Add(1) == 1 {
			return map[string]any{"error": "401 Unauthorized: bad credentials"}
		}
		return map[string]any{"issues": []any{"issue-1"}, "status": "ok"}
	})
	s.applyUpstream(t)

	adminCanonical := canonicalFor(t, adminEmail)
	grantPlatformAdmin(t, ctx, h, adminCanonical)
	linkAgentIdentityToPlatform(t, ctx, h, "default", agentIdentityName)

	operatorURL := startOperatorAdmind(t, h)
	idBaseURL := startIdentityd(t, h, s.identitydSigner, operatorURL)
	startCredentialUpdateOperator(t, ctx, h)
	monitoring := startMonitoringSink(t, ctx, h)
	startCardPublisher(t, ctx, h, s.channelsdSigner, idBaseURL)

	h.LLM.OnUserMessage("list the upstream issues").Reply(callUpstream("first attempt"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("401")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "the shared bot token came back 401 Unauthorized",
		}))
	h.LLM.OnToolResult("request_credential_update", e2e.ResultContains("the credential was updated")).
		Reply(callUpstream("retry after the shared credential was replaced"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("issue-1")).
		Reply(e2e.RespondToUser("listed the issues once the shared credential was replaced"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)
	h.SendUserMessage("list the upstream issues")

	// --- the admin surface is asked, and the starter is NOT -----------------
	sess := waitForSession(t, ctx, h)
	ensurePlaceholderRunnerPod(t, ctx, h, sess)

	// Parked before anything has been put in front of an admin, and with no
	// nudge: the only thing that can enqueue this session between the meta tool
	// creating the CredentialUpdateRequest and this line is SetupWithManager's
	// Watches(&CredentialUpdateRequest{}). Asserting the park after the
	// monitoring broadcast would let that path's own writes stand in for it.
	waitForCredentialUpdateParked(t, ctx, h, sess)

	ev := monitoring.await(t)
	assert.Contains(t, ev.Summary, agentIdentityName,
		"an admin must see WHICH agent identity is broken -- that is the blast radius of the replacement")
	assert.Contains(t, ev.Summary, credential, "and which of its credentials")
	assert.Contains(t, ev.Summary, "the shared bot token came back 401 Unauthorized",
		"the agent's own why reaches the admin, attributed")

	// Polled, not sampled. The in-thread card is published AFTER the monitoring
	// event, in the same pass but behind an extra RPC + NATS + relay hop, so a
	// read taken the instant monitoring.await returns would find nothing even
	// under the revert this assertion exists to catch. Every other prompt reader
	// in this repo polls for the same reason (test/e2e/approval.go).
	assertNoStarterCardWithin(t, h, sess, 2*pipeline.CredentialUpdateWatcherInterval)

	linkURL := linkFromHint(t, ev.Hint)
	assert.True(t, strings.HasPrefix(linkURL, idBaseURL),
		"the admin's button must point at a link they can actually open")

	require.Equal(t, deadValue, botSecretValue(t, ctx, h), "precondition: the shared credential is still dead")

	// --- the admin opens the link and pastes a replacement -------------------
	//
	// The real browser path: a real idd_session cookie, a real form POST to
	// identityd, identityd's own UX gate, then the operator's authoritative
	// FullyConsistent check across a real HTTP hop before anything is written.
	resp := submitReplacement(t, idBaseURL, s.identitydSigner, linkURL, adminEmail, freshValue)
	require.Equal(t, http.StatusFound, resp.StatusCode,
		"a platform admin's paste must be accepted; body=%s", bodyOf(t, resp))

	// --- the write landed on the SHARED Secret ------------------------------
	assert.Equal(t, freshValue, botSecretValue(t, ctx, h),
		"the operator must patch the AgentIdentity's backing Secret -- the one the request watches")
	assert.False(t, userIdentityExists(t, ctx, h, adminCanonical),
		"and must NOT have written the admin's own UserIdentity: that was the bug, and it looked like success")

	// --- the request Fulfils and the blocked turn resumes -------------------
	waitForRequestPhase(t, ctx, h, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled)
	h.ExpectAgentReply(e2e.Contains("listed the issues once the shared credential was replaced"))
	assert.EqualValues(t, 2, upstreamCalls.Load(),
		"the agent retried the upstream call after the shared credential was replaced")

	// The tool_result the model was actually fed. The reply text above is a
	// scripted constant and proves only that the turn completed; THIS is the
	// evidence the blocked call returned with the platform's own verdict.
	assertToolResultSeenByModel(t, h, "the credential was updated. Retry your original tool call now.")
}

// ---------------------------------------------------------------------------
// Scenario 2: a non-admin click is refused, and nothing is written
// ---------------------------------------------------------------------------

// TestAgentOwnedCredentialUpdate_ANonAdminClickIsRefusedAndNothingIsWritten is
// the negative that gives the feature its value.
//
// The monitoring link is deliberately NOT subject-bound -- a monitoring channel
// has no single addressee, so anyone who can see the channel, or anyone the URL
// is forwarded to, holds it. A live permission check at click time is therefore
// the ONLY control, and this is where it is proven against a real SpiceDB with
// a real signed-in user who simply is not an admin.
func TestAgentOwnedCredentialUpdate_ANonAdminClickIsRefusedAndNothingIsWritten(t *testing.T) {
	s := startScenario(t)
	defer s.cancel()
	h, ctx := s.h, s.ctx

	redirectVerifyProbeByToken(t, map[string]int{deadValue: 401, freshValue: 200})

	h.MCP.OnTool("list_issues", func(map[string]any) any {
		return map[string]any{"error": "401 Unauthorized: bad credentials"}
	})
	s.applyUpstream(t)

	// An admin EXISTS in this cluster -- the bystander is refused because they
	// personally lack the permission, not because nobody could ever hold it. A
	// cluster where update_credential is unsatisfiable by everyone would also
	// produce a 403 here, and that is the failure this slice must not ship.
	grantPlatformAdmin(t, ctx, h, canonicalFor(t, adminEmail))
	linkAgentIdentityToPlatform(t, ctx, h, "default", agentIdentityName)

	operatorURL := startOperatorAdmind(t, h)
	idBaseURL := startIdentityd(t, h, s.identitydSigner, operatorURL)
	startCredentialUpdateOperator(t, ctx, h)
	monitoring := startMonitoringSink(t, ctx, h)
	startCardPublisher(t, ctx, h, s.channelsdSigner, idBaseURL)

	h.LLM.OnUserMessage("list the upstream issues").Reply(callUpstream("first attempt"))
	h.LLM.OnToolResult(mcpToolName, e2e.ResultContains("401")).
		Reply(e2e.ToolUse("request_credential_update", map[string]any{
			"tool": mcpToolName,
			"why":  "the shared bot token came back 401 Unauthorized",
		}))
	// Unlike scenario 1, the credential is NEVER replaced here -- that is the
	// whole point -- so the meta tool is still blocked when the test finishes
	// and s.cancel() releases it with "context canceled". The runner feeds that
	// result back to the model, and ScriptedLLM fails a test on any request no
	// rule matches. This rule absorbs that teardown turn; every assertion below
	// has already run by then.
	h.LLM.OnToolResult("request_credential_update", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	h.WaitForAgentClassValid(className, 30*time.Second)
	h.SendUserMessage("list the upstream issues")

	sess := waitForSession(t, ctx, h)
	ensurePlaceholderRunnerPod(t, ctx, h, sess)
	// Parked on the request's own watch event, before any card exists -- see
	// the admin-replaces scenario above for why the ordering is the assertion.
	waitForCredentialUpdateParked(t, ctx, h, sess)

	ev := monitoring.await(t)
	linkURL := linkFromHint(t, ev.Hint)

	// The bystander is properly signed in -- a valid idd_session cookie for a
	// real user. The ONLY thing they lack is the permission.
	bystanderCanonical := canonicalFor(t, bystanderEmail)
	resp := submitReplacement(t, idBaseURL, s.identitydSigner, linkURL, bystanderEmail, freshValue)
	body := bodyOf(t, resp)
	require.Equal(t, http.StatusForbidden, resp.StatusCode,
		"a signed-in user without agentidentity#update_credential must be refused; body=%s", body)

	// The 403 alone does NOT prove the click-time permission check ran.
	//
	// The monitoring link carries no Subject (a monitoring channel has no single
	// addressee), so if identityd's agent-owned branch were deleted outright,
	// resolveAgentOwnedTarget would return (nil, nil) and the fall-through
	// `!cookieOK || cookieSubject != payload.Subject` case would 403 anyway --
	// a non-empty cookie subject can never equal "". Every assertion below would
	// then pass with the control this scenario exists to prove never having run.
	//
	// This sentence is emitted ONLY by refuseAgentOwned
	// (pkg/platform/identityd/credupdate_agentowned.go), which is reachable only through
	// that branch. It is what makes the 403 the RIGHT 403.
	assert.Contains(t, body, "only a platform administrator",
		"the refusal must come from the agent-owned permission gate, not from the subject-equality gate "+
			"a subject-less monitoring link fails by construction")

	// All four halves of the bug this slice removed.
	assert.Equal(t, deadValue, botSecretValue(t, ctx, h),
		"a refused click must not move the shared credential")
	assert.False(t, userIdentityExists(t, ctx, h, bystanderCanonical),
		"and must not silently write the CLICKER's own credential instead -- the original defect, which "+
			"looked like success to the human and left the real credential dead")
	assert.Equal(t, spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
		currentRequest(t, ctx, h).Status.Phase,
		"and must not be mistaken for a fix: the request stays Open, still asking")
}

// ---------------------------------------------------------------------------
// Scenario plumbing
// ---------------------------------------------------------------------------

type scenario struct {
	h      *e2e.Harness
	ctx    context.Context
	cancel context.CancelFunc
	mcpDoc string
	// channelsdSigner mints the card's deep-link; identitydSigner mints the
	// idd_session cookie. TWO signers over ONE key, exactly as production has
	// it (internal/cmd/channelsd/main.go and internal/cmd/webd/main.go), because the iss claim is
	// load-bearing: identityd verifies a deep-link with
	// WithExpectedIssuer(channelsd) and a cookie with iss=identityd, precisely
	// so a cookie cannot be presented as a deep-link or vice versa. A single
	// option-less signer mints iss="" and every link is refused as malformed —
	// which is invisible to any test that never dials the link it was given.
	channelsdSigner *passthroughlink.Signer
	identitydSigner *passthroughlink.Signer
}

func startScenario(t *testing.T) *scenario {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	mcpDoc, rest := splitMCPServerDoc(string(raw))
	require.NotEmpty(t, mcpDoc, "manifests.yaml must contain an MCPServer document")

	h := e2e.Start(t, e2e.Options{
		DefaultUser:    starterEmail,
		ExtraManifests: []string{rest},
		DefaultTimeout: scenarioTimeout,
	})
	ctx, cancel := context.WithCancel(context.Background())
	return &scenario{
		h: h, ctx: ctx, cancel: cancel, mcpDoc: mcpDoc,
		channelsdSigner: passthroughlink.New([]byte(signingKey),
			passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
			passthroughlink.WithAudience(passthroughlink.AudienceIdentityd)),
		identitydSigner: passthroughlink.New([]byte(signingKey),
			passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
			passthroughlink.WithAudience(passthroughlink.AudienceIdentityd)),
	}
}

// applyUpstream applies the MCPServer with the live stub URL substituted in.
// Must run AFTER h.MCP.OnTool: the MCPServer controller's tools/list probe runs
// during validation.
func (s *scenario) applyUpstream(t *testing.T) {
	t.Helper()
	s.h.ApplyManifest(strings.ReplaceAll(s.mcpDoc, "{{MCP_URL}}", s.h.MCP.URL()))
}

func splitMCPServerDoc(all string) (mcpDoc, rest string) {
	var keep []string
	for _, doc := range strings.Split(all, "\n---\n") {
		if strings.Contains(doc, "kind: MCPServer") {
			mcpDoc = doc
			continue
		}
		keep = append(keep, doc)
	}
	return mcpDoc, strings.Join(keep, "\n---\n")
}

func canonicalFor(t *testing.T, email string) identity.CanonicalUserID {
	t.Helper()
	canon, err := identity.EmailReference(identity.Email(email)).Canonical()
	require.NoError(t, err, "canonicalize %q", email)
	return canon
}

// grantPlatformAdmin writes platform:platform#admin for canonical.
//
// The retry is required, not defensive: the canonical schema reaches SpiceDB
// through the Guardian compose, which runs asynchronously after Start returns,
// and a write against a not-yet-present definition fails with "object
// definition not found".
func grantPlatformAdmin(t *testing.T, ctx context.Context, h *e2e.Harness, canonical identity.CanonicalUserID) {
	t.Helper()
	eventuallyNoError(t, ctx, "grant platform admin to "+canonical.String(), func() error {
		return h.SpiceDB.TouchPlatformAdmin(ctx, canonical)
	})
}

// linkAgentIdentityToPlatform writes
// agentidentity:<ns>/<name>#platform@platform:platform -- the ONE relationship
// that makes agentidentity#update_credential satisfiable at all (its other arm,
// `editor`, ships deliberately unpopulated).
//
// # Why the test writes this and production's reconciler does not, here
//
// In production the AgentIdentity reconciler writes it on every reconcile
// (internal/cmd/operator wires its PlatformLinker). The e2e harness registers that same
// reconciler WITHOUT a PlatformLinker, so in this tier the tuple has no writer.
//
// Wiring the harness to match production was tried and reverted: the link write
// returns its error (a deliberate fail-loud choice for a permission-GRANTING
// write), and the harness starts the AgentIdentity controller BEFORE the
// Guardian compose has put the schema into SpiceDB -- so every early reconcile
// failed on "object definition `agentidentity` not found", returned before
// stamping any status, and seven unrelated AgentIdentity-dependent scenarios
// timed out waiting for a Valid condition that never arrived. That ordering
// problem is real and worth fixing, but it is a harness/startup-ordering change
// with its own blast radius, not something to smuggle in here.
//
// So this call is an explicit, honest stand-in, using the SAME production
// function the reconciler calls. What it means for what this scenario proves:
// it does NOT prove that something in production writes the tuple. That claim
// is proven at the integration tier, twice, with an isolating negative control
// -- pkg/controllers/agentidentity/platform_link_integration_test.go and
// TestAgentOwnedWrite_IsAuthorizedByTheReconcilersOwnRelationshipWrite, where an
// identity that was never reconciled refuses the very admin a reconciled one
// accepts. What THIS tier proves is everything downstream of the tuple: the
// card, the click, the operator's check, the write, and the turn resuming.
func linkAgentIdentityToPlatform(t *testing.T, ctx context.Context, h *e2e.Harness, ns, name string) {
	t.Helper()
	eventuallyNoError(t, ctx, "link "+ns+"/"+name+" to the platform object", func() error {
		return h.SpiceDB.EnsureAgentIdentityPlatform(ctx, ns, name)
	})
}

// eventuallyNoError retries fn until it succeeds, reporting the LAST error on
// timeout. Both callers race the same thing: the Guardian compose that puts the
// canonical schema into SpiceDB runs asynchronously after Start returns.
func eventuallyNoError(t *testing.T, ctx context.Context, what string, fn func() error) {
	t.Helper()
	var last error
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s: cancelled; last error: %v", what, last)
		case <-time.After(250 * time.Millisecond):
		}
	}
	t.Fatalf("could not %s within 30s: %v", what, last)
}

// startOperatorAdmind serves the REAL pkg/web/admind handler -- the operator's half
// of the write path, and the only component in this scenario with Secret
// access. Its Checker is the harness's live SpiceDB, so the permission decision
// is a real one.
func startOperatorAdmind(t *testing.T, h *e2e.Harness) string {
	t.Helper()
	a, err := admind.New(admind.Config{
		Mem:     memory.NewLocal(inmem.NewBackend()),
		K8s:     h.K8s,
		Checker: h.SpiceDB,
		Token:   admindToken,
	})
	require.NoError(t, err, "build the operator's admin surface")
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// startIdentityd mounts identityd on the webui framework, as internal/cmd/webd does, and
// wires the two credential-update deps webd supplies only when SpiceDB is
// configured. Absent them every agent-owned click fails closed, so a scenario
// that omitted them would "pass" its negative for entirely the wrong reason.
func startIdentityd(t *testing.T, h *e2e.Harness, signer *passthroughlink.Signer, operatorURL string) string {
	t.Helper()
	var srv http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.ServeHTTP(w, r)
	}))
	ts.Start()
	t.Cleanup(ts.Close)

	deps := harnessWebDeps{
		k8s:             h.K8s,
		linkSigner:      signer,
		externalBaseURL: func() string { return ts.URL },
		authenticators: map[string]channelkinds.WebAuthenticator{
			"fake": (fakekind.Kind{}).WebAuthenticator(channelkinds.WebAuthDeps{ExternalBaseURL: ts.URL}),
		},
		authz:  h.SpiceDB,
		writer: agentcred.New(operatorURL, admindToken),
	}
	host := func() string { return ts.URL }
	ws, err := webui.NewServer(
		func(*http.Request) (string, bool) { return "framework-subject", true },
		nil,
		host,
		func() string { return "sandbox.invalid" },
		nil,
		deps,
		[]webui.WebUI{identityd.New()},
	)
	require.NoError(t, err, "mount identityd on the webui framework")
	srv = ws
	return ts.URL
}

// harnessWebDeps satisfies identityd.WebDeps AND identityd.WebAuthzDeps, so the
// agent-owned routes mount with a real authorization client and a real operator
// client -- mirroring internal/cmd/webd when SPICEDB_ENDPOINT is set.
type harnessWebDeps struct {
	k8s             client.Client
	linkSigner      *passthroughlink.Signer
	externalBaseURL func() string
	authenticators  map[string]channelkinds.WebAuthenticator
	authz           identityd.AgentIdentityAuthz
	writer          identityd.AgentCredentialWriter
}

func (d harnessWebDeps) K8s() client.Client                  { return d.k8s }
func (d harnessWebDeps) LinkSigner() *passthroughlink.Signer { return d.linkSigner }
func (d harnessWebDeps) ExternalBaseURL() string             { return d.externalBaseURL() }
func (d harnessWebDeps) IconHandler() http.Handler           { return nil }
func (d harnessWebDeps) Authenticators() map[string]channelkinds.WebAuthenticator {
	return d.authenticators
}

// InsecureTrustLinks is false: every request here carries a real idd_session
// cookie, so the legacy trust-the-link fallback must never be the reason a
// click succeeds.
func (d harnessWebDeps) InsecureTrustLinks() bool { return false }

func (d harnessWebDeps) AgentIdentityAuthz() identityd.AgentIdentityAuthz       { return d.authz }
func (d harnessWebDeps) AgentCredentialWriter() identityd.AgentCredentialWriter { return d.writer }

// startCredentialUpdateOperator runs the operator's determination loop. The
// harness's controller manager is not extensible from a scenario, so this
// drives Reconcile on a tick.
func startCredentialUpdateOperator(t *testing.T, ctx context.Context, h *e2e.Harness) {
	t.Helper()
	r := &credentialupdaterequest.Reconciler{
		Client:  h.K8s,
		Broker:  inproc.New(h.K8s),
		IdleTTL: 10 * time.Minute, // this scenario never exercises expiry
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				var list spiceboxv1alpha1.CredentialUpdateRequestList
				if err := h.K8s.List(ctx, &list); err != nil {
					if ctx.Err() == nil {
						t.Logf("credential-update operator: list requests: %v", err)
					}
					continue
				}
				for i := range list.Items {
					it := &list.Items[i]
					if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
						Namespace: it.Namespace, Name: it.Name,
					}}); err != nil && ctx.Err() == nil {
						t.Logf("credential-update operator: reconcile %s/%s: %v", it.Namespace, it.Name, err)
					}
				}
			}
		}
	}()
	t.Cleanup(func() { <-done })
}

// startCardPublisher runs channelsd's real CredentialUpdateWatcher, including
// the Authz seam the agent-owned route needs. Its ExternalBaseURL is identityd's
// real URL, so the link it mints is one this test can actually dial.
func startCardPublisher(t *testing.T, ctx context.Context, h *e2e.Harness,
	signer *passthroughlink.Signer, idBaseURL string) {
	t.Helper()
	conn := dialHarnessNATS(t, h.NATSURL, "e2e-agentcred-publisher")
	w := &pipeline.CredentialUpdateWatcher{
		K8s:             h.K8s,
		LinkSigner:      signer,
		ExternalBaseURL: func() string { return idBaseURL },
		NATSPublish:     conn.Publish,
		Authz:           h.SpiceDB,
	}
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Logf("agent-owned card publisher did not exit within 5s")
		}
	})
}

// monitoringSink collects the MonitoringEvents the watcher broadcasts.
//
// It subscribes to the NATS subject directly rather than reading them out of
// the fake Channel driver, because the per-Channel fan-out that would put them
// there lives in internal/cmd/channelsd (package main) and is therefore not importable
// from a scenario. That fan-out's precondition -- that a role=monitoring
// Channel exists AND its kind can really deliver -- is covered at the
// integration tier, and this scenario still carries a deliverable monitoring
// Channel because without one channelsd refuses to publish at all.
type monitoringSink struct {
	events chan channelevents.MonitoringEvent
}

func startMonitoringSink(t *testing.T, ctx context.Context, h *e2e.Harness) *monitoringSink {
	t.Helper()
	conn := dialHarnessNATS(t, h.NATSURL, "e2e-agentcred-monitoring")
	sink := &monitoringSink{events: make(chan channelevents.MonitoringEvent, 8)}
	sub, err := conn.Subscribe(channelevents.MonitoringEventSubject, func(m *nats.Msg) {
		var ev channelevents.MonitoringEvent
		if err := json.Unmarshal(m.Data, &ev); err != nil {
			t.Logf("monitoring sink: decode: %v", err)
			return
		}
		// The same structural gate the relay applies before fanning an event
		// out: a malformed broadcast must not be counted as an admin being
		// asked.
		if err := ev.Validate(); err != nil {
			t.Logf("monitoring sink: invalid event: %v", err)
			return
		}
		select {
		case sink.events <- ev:
		default:
			t.Logf("monitoring sink: dropping event, buffer full")
		}
	})
	require.NoError(t, err, "subscribe to monitoring events")
	t.Cleanup(func() {
		if err := sub.Unsubscribe(); err != nil && ctx.Err() == nil {
			t.Logf("monitoring sink: unsubscribe: %v", err)
		}
	})
	return sink
}

// await blocks for the credential broadcast. A timeout here means no admin was
// ever asked -- the silent-hang failure this route exists to prevent.
func (s *monitoringSink) await(t *testing.T) channelevents.MonitoringEvent {
	t.Helper()
	deadline := time.After(scenarioTimeout)
	for {
		select {
		case ev := <-s.events:
			if ev.Category == "credential" {
				return ev
			}
		case <-deadline:
			t.Fatalf("no credential monitoring broadcast reached the admin surface within %v -- "+
				"NO ADMIN WAS EVER ASKED to replace the shared bot credential", scenarioTimeout)
		}
	}
}

func dialHarnessNATS(t *testing.T, natsURL, name string) *nats.Conn {
	t.Helper()
	conn, err := nats.Connect(natsURL,
		nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1), nats.Name(name))
	require.NoError(t, err, "dial harness NATS")
	t.Cleanup(func() {
		if err := conn.Drain(); err != nil {
			t.Logf("%s: nats drain on cleanup: %v", name, err)
		}
	})
	return conn
}

// redirectVerifyProbeByToken points the provider-catalog verify probe at a
// local server that answers according to the TOKEN it is handed.
//
// A call-count-based switch would be racy here: the operator's loop re-probes
// on every pass, so "the Nth call" is not a stable way to say "the value
// changed". Answering on the credential itself is both deterministic and a
// faithful model of a real provider.
//
// Mutates package-level state in pkg/platform/identity/setup/builtins, so tests using it
// must not run in parallel with each other.
func redirectVerifyProbeByToken(t *testing.T, statusByToken map[string]int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		for token, status := range statusByToken {
			if strings.Contains(auth, token) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"probe":"token matched","status":%d}`, status)))
				return
			}
		}
		// An unrecognized token is dead. Defaulting to 200 would let a
		// regression that probes the WRONG value look like success.
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse probe server URL")
	builtins.SetVerifyHTTPClient(func() *http.Client {
		return &http.Client{Transport: &redirectTransport{target: u}}
	})
	t.Cleanup(func() { builtins.SetVerifyHTTPClient(nil) })
}

// redirectTransport rewrites every outbound request at the real catalog
// provider's hardcoded endpoint to the local server instead.
type redirectTransport struct{ target *url.URL }

func (t *redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Scheme = t.target.Scheme
	r2.URL.Host = t.target.Host
	r2.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// ---------------------------------------------------------------------------
// The click
// ---------------------------------------------------------------------------

// submitReplacement performs the browser's POST: a real idd_session cookie for
// `who`, and the signed link reassembled from the card's own URL.
//
// The cookie is minted with the same signer identityd verifies it with, as
// identityd's own OIDC callback would -- so what is under test is the
// permission check, not a forged session.
func submitReplacement(t *testing.T, idBaseURL string, signer *passthroughlink.Signer,
	linkURL, who, token string) *http.Response {
	t.Helper()
	parsed, err := url.Parse(linkURL)
	require.NoError(t, err, "parse the card's link URL")
	d, sig := parsed.Query().Get("d"), parsed.Query().Get("sig")
	require.NotEmpty(t, d, "the card's link must carry a signed payload")
	require.NotEmpty(t, sig, "the card's link must carry a signature")

	cookieRaw, err := signer.Mint(passthroughlink.Payload{
		Issuer:    passthroughlink.IssuerIdentityd,
		Audience:  passthroughlink.AudienceIdentityd,
		Subject:   canonicalFor(t, who).Subject(),
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint the idd_session cookie for %s", who)

	form := url.Values{"link": {d + "." + sig}, "credential": {credential}, "token": {token}}
	req, err := http.NewRequest(http.MethodPost, idBaseURL+"/link/submit", strings.NewReader(form.Encode()))
	require.NoError(t, err, "build the submit request")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "idd_session", Value: cookieRaw})

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Do(req)
	require.NoError(t, err, "POST /link/submit")
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// linkFromHint extracts the URL from the monitoring event's hint, which is the
// only place an admin reading that channel is given one.
func linkFromHint(t *testing.T, hint string) string {
	t.Helper()
	idx := strings.Index(hint, "http")
	require.GreaterOrEqual(t, idx, 0, "the monitoring hint must carry a link an admin can open; got %q", hint)
	return strings.TrimSpace(hint[idx:])
}

// bodyOf reads the WHOLE response body.
//
// It used to take one 2048-byte Read, which is not enough for an assertion on
// the page copy: the refusal sentence sits in a rendered HTML page behind a
// head full of inline CSS, and a single Read may in any case return fewer bytes
// than asked for. Callers must invoke it at most once per response -- the body
// is consumed.
func bodyOf(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}

// ---------------------------------------------------------------------------
// LLM script helpers
// ---------------------------------------------------------------------------

// callUpstream is the failing tool call. operation_id is a literal: this class
// declares no tool bundles, so the runner leaves SessionContext.Operations nil
// and MCP dispatch skips the is-it-registered check while still requiring the
// envelope fields to be present.
func callUpstream(query string) e2e.ReplyPart {
	return e2e.ToolUse(mcpToolName, map[string]any{
		"operation_id": "op-agent-owned-credential-update-e2e",
		"_reason":      "read the upstream issue list",
		"args":         map[string]any{"query": query},
	})
}

// ---------------------------------------------------------------------------
// Assertions
// ---------------------------------------------------------------------------

func waitForSession(t *testing.T, ctx context.Context, h *e2e.Harness) *spiceboxv1alpha1.AgentSession {
	t.Helper()
	var found *spiceboxv1alpha1.AgentSession
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(ctx, &list); err != nil || len(list.Items) == 0 {
			return false
		}
		found = &list.Items[0]
		return true
	}, "an AgentSession must be created for the inbound message")
	return found
}

// starterCards returns any credential_update card delivered into the SESSION's
// own channel. For an agent-owned credential this must stay empty.
func starterCards(h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession) []fakekind.InteractionPrompt {
	drv := fakekind.DriverFor(sess.Namespace, channelName)
	if drv == nil {
		return nil
	}
	var out []fakekind.InteractionPrompt
	for _, p := range drv.InteractionPrompts() {
		if p.Payload.Category != categories.CredentialUpdate {
			continue
		}
		out = append(out, p)
	}
	return out
}

// assertNoStarterCardWithin fails the moment a credential_update card appears
// in the SESSION's own channel, and otherwise keeps watching for d.
//
// A negative like this cannot be a single read. The publish it must catch
// happens on a later hop than the monitoring event the caller just awaited
// (publishAgentOwnedCard emits the broadcast, then the in-thread card, then
// records delivery), so sampling immediately would report "no card" for a card
// that is merely still in flight -- and would keep reporting it after the
// routing split was reverted, which is the one thing this must never do.
//
// d is two watcher ticks: the publish is driven by CredentialUpdateWatcher's
// own loop, so a window shorter than its interval could miss the pass entirely.
func assertNoStarterCardWithin(t *testing.T, h *e2e.Harness, sess *spiceboxv1alpha1.AgentSession, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cards := starterCards(h, sess); len(cards) > 0 {
			t.Fatalf("the agent's OWN credential was DM'd to the session's starter, who cannot replace it: %+v\n"+
				"before this slice a click by them wrote the pasted value into their own personal identity, "+
				"showed them a success badge, and left the shared credential dead", cards[0].Payload)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func botSecretValue(t *testing.T, ctx context.Context, h *e2e.Harness) string {
	t.Helper()
	var sec corev1.Secret
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: "default", Name: botSecretName}, &sec),
		"read the agent identity's backing Secret")
	return string(sec.Data[credential])
}

// userIdentityExists reports whether a UserIdentity was created for canonical.
//
// This is the load-bearing NEGATIVE assertion in both scenarios: the defect this
// slice removed wrote the pasted value into the CLICKER's own UserIdentity, so
// "the shared Secret didn't move" alone would not distinguish "nothing was
// written" from "the wrong thing was written".
func userIdentityExists(t *testing.T, ctx context.Context, h *e2e.Harness, canonical identity.CanonicalUserID) bool {
	t.Helper()
	var ui spiceboxv1alpha1.UserIdentity
	err := h.K8s.Get(ctx, client.ObjectKey{Name: useridentity.NameForSubject(canonical.Subject())}, &ui)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err, "get UserIdentity for %s", canonical)
	return true
}

func currentRequest(t *testing.T, ctx context.Context, h *e2e.Harness) *spiceboxv1alpha1.CredentialUpdateRequest {
	t.Helper()
	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, h.K8s.List(ctx, &list), "list CredentialUpdateRequests")
	require.Len(t, list.Items, 1, "exactly one credential-update request expected")
	return &list.Items[0]
}

func waitForRequestPhase(t *testing.T, ctx context.Context, h *e2e.Harness, want string) {
	t.Helper()
	var last string
	e2e.Eventually(t, scenarioTimeout, func() bool {
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		if err := h.K8s.List(ctx, &list); err != nil || len(list.Items) != 1 {
			return false
		}
		last = list.Items[0].Status.Phase
		return last == want
	}, "the request must reach phase "+want+" once the operator's write moves the Secret it recorded "+
		"(last phase seen is logged below)")
	if t.Failed() {
		t.Logf("last observed phase: %q", last)
	}
}

// waitForCredentialUpdateParked waits for the session to park on the open
// request. The park keeps the runner alive while the meta tool blocks
// in-process; tearing it down would destroy the blocked call.
//
// It does NOT nudge. Callers must have created the placeholder runner Pod
// first (see ensurePlaceholderRunnerPod); given that, the CredentialUpdate-
// Request watch registered in agentsession.SetupWithManager is the production
// trigger and is sufficient, and a timeout here means that watch stopped
// re-enqueueing the session -- exactly the wedge a nudge would hide.
func waitForCredentialUpdateParked(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	parked := false
	deadline := time.Now().Add(scenarioTimeout)
	for time.Now().Before(deadline) && !parked {
		var got spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got); err == nil {
			for _, c := range got.Status.Conditions {
				if c.Type == spiceboxv1alpha1.AgentSessionConditionCredentialUpdatePending &&
					c.Status == metav1.ConditionTrue {
					parked = true
				}
			}
		}
		if !parked {
			time.Sleep(250 * time.Millisecond)
		}
	}
	if !parked {
		var got spiceboxv1alpha1.AgentSession
		_ = h.K8s.Get(ctx, client.ObjectKeyFromObject(sess), &got)
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		_ = h.K8s.List(ctx, &list)
		reqs := make([]string, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, list.Items[i].Name+"="+list.Items[i].Status.Phase)
		}
		t.Fatalf("the session never parked on the open credential-update request within %v; "+
			"session phase=%q conditions=%+v requests=%v",
			scenarioTimeout, got.Status.Phase, got.Status.Conditions, reqs)
	}
}

// ensurePlaceholderRunnerPod creates the inert Pod the AgentSession reconciler
// expects under RunnerPodName. In the in-process harness the runner is a
// goroutine, and without a Pod Reconcile stops at the runner-spawn step --
// hundreds of lines before the credential-update park.
//
// CALL THIS BEFORE THE REQUEST EXISTS, i.e. right after the session appears.
// The Pod is created out-of-band with no ownerReference, so unlike the Pod the
// PodRunnerFactory creates in production it fires no Owns(&corev1.Pod{}) event.
// Created after the CredentialUpdateRequest is already Open, the watch event
// that would have driven the park has been and gone, the session sits with no
// pending event, and the park only lands tens of seconds later off some
// unrelated write -- which is what the old nudge in
// waitForCredentialUpdateParked was papering over.
func ensurePlaceholderRunnerPod(t *testing.T, ctx context.Context, h *e2e.Harness,
	sess *spiceboxv1alpha1.AgentSession) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      agentsession.RunnerPodName(sess),
			Namespace: sess.Namespace,
			Labels:    map[string]string{"agentprimitives.authzed.com/agentsession": sess.Name},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers:    []corev1.Container{{Name: "runner", Image: "e2e-agentcred-placeholder"}},
		},
	}
	if err := h.K8s.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err, "create placeholder runner pod")
	}
}

// assertToolResultSeenByModel searches every request the runner sent to the LLM
// for a tool_result containing want. Deliberately not an assertion on the reply
// text, which is a scripted constant and would stay green even if the tool
// returned something else. Substring, because the runner wraps tool results in
// an untrusted-output envelope.
func assertToolResultSeenByModel(t *testing.T, h *e2e.Harness, want string) {
	t.Helper()
	var seen []string
	for _, req := range h.LLM.Requests() {
		for _, m := range req.Messages {
			for _, block := range m.Content {
				if block.ToolResult == nil {
					continue
				}
				if strings.Contains(block.ToolResult.Content, want) {
					return
				}
				seen = append(seen, block.ToolResult.Content)
			}
		}
	}
	t.Fatalf("no tool_result fed to the model contained %q; tool results seen:\n%s",
		want, strings.Join(seen, "\n---\n"))
}
