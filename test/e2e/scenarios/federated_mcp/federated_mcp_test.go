//go:build e2e

// Package federated_mcp_test is the e2e scenario that exercises the full
// federated-credential runtime wiring:
//
//	SessionUserIdentity[type=federated] →
//	inproc.Broker.resolveFederated →
//	fake.Minter.Mint →
//	Authorization: Bearer minted-for-<resource> on the MCP stub.
//
// It proves Scenario B of the ID-JAG feature (Task 12): the credential
// descriptor path from a populated SUI through the in-process broker to
// the MCP stub's request header, plus the fail-closed revoke sub-case
// (IdP-identity Secret deleted → new session build fails closed).
//
// No real names: alice / Linear are fictional per AGENTS.md.
package federated_mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	federationfake "github.com/authzed/openagentprimitives/pkg/platform/identity/federation/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// starterEmail is the human starter the test impersonates via DefaultUser.
	// The pipeline canonicalizes it and stamps AnnotationStartedByCanonicalID.
	starterEmail = "alice@example.com"

	// fedResource is the MCPServer's spec.auth.resource — the ID-JAG audience
	// passed to the Minter. The fake Minter returns "minted-for-" + fedResource.
	fedResource = "https://linear.api.example.invalid"

	// expectedBearer is the Authorization header value the test asserts on the
	// MCP stub. Derived from the fake.Minter convention: "minted-for-"+resource.
	expectedBearer = "Bearer minted-for-" + fedResource
)

// starterSubject is the "user:<canonical>" string the pipeline stamps on the
// AgentSession as AnnotationStartedByCanonicalID. It drives the passthrough
// gate's IdP-identity Secret lookup.
//
// Canonical for "alice@example.com" = base64url("alice@example.com")
// = "YWxpY2VAZXhhbXBsZS5jb20" (verified by canonical_golden_test.go).
const starterSubject = "user:YWxpY2VAZXhhbXBsZS5jb20"

func TestFederatedMCP_MintsAndInjectsBearer(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Strip the MCPServer doc so its {{MCP_URL}} sentinel can be replaced
	// after Start returns with the live MCPStub URL.
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{nonMCPManifests},
		DefaultTimeout: 30 * time.Second,
		DefaultUser:    starterEmail,
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// 1. Create the identities namespace. The passthrough gate checks for
	// the IdP-identity Secret there; envtest doesn't auto-create namespaces.
	createIdentitiesNamespace(t, ctx, h.K8s)

	// 2. Create the IdP-identity Secret. Its presence signals the controller
	// that alice is logged in via the enterprise IdP. The fake.Minter does
	// not inspect the Secret values (it returns a deterministic token), but
	// subjectMaterial must be able to read the Secret for resolveFederated
	// to proceed.
	idpSecretName := useridentity.IdPIdentitySecretName(starterSubject)
	idpSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      idpSecretName,
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		},
		Data: map[string][]byte{
			"access_token":   []byte("user-id-token"),
			"refresh_token":  []byte("rt"),
			"token_endpoint": []byte("https://idp.example.invalid/token"),
			"client_id":      []byte("ap-client"),
			"client_secret":  []byte("ap-secret"),
			// No expires_at → never expires → no JIT refresh needed.
		},
	}
	require.NoError(t, h.K8s.Create(ctx, idpSecret), "create IdP-identity Secret")

	// 3. Register the MCP stub tool and apply the MCPServer with the live URL.
	h.MCP.OnTool("ping", func(args map[string]any) any {
		return map[string]any{"pong": args["message"]}
	})
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	// 4. Wire the fake Minter BEFORE the session spawns. The Minter is
	// consumed inside buildMCPTools (per-session), so it must be set before
	// SendUserMessage triggers session creation.
	fakeMinter := &federationfake.Minter{TTL: time.Hour}
	h.SetMinter(fakeMinter)

	// 5. Script the LLM: receive user message → call ping directly → reply → end.
	// No new_operation step: this session has no tool bundle so sctx.Operations
	// is nil, which bypasses the registered-operation check in MCP dispatch
	// (dispatch.go:84). operation_id and _reason must still be non-empty
	// (dispatch.go:81), but any value is accepted when Operations is nil.
	// Mirror how other non-bundle MCP scenarios (centerdot, sidecars) call
	// their tools directly without a preceding new_operation.
	h.LLM.OnUserMessage("ping linear").Reply(e2e.ToolUse("linear_ping", map[string]any{
		"operation_id": "fedmcp-op-1",
		"_reason":      "verify federated credential",
		"args":         map[string]any{"message": "hello"},
	}))
	h.LLM.OnToolResult("linear_ping", e2e.AnyResult()).Reply(
		e2e.RespondToUser("federated ping ok"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	// 6. Wait for the AgentClass to become Valid=True (MCPServer controller
	// stamps Valid unconditionally after a probe attempt — the probe result
	// itself doesn't matter).
	h.WaitForAgentClassValid("ac-fedmcp", 30*time.Second)

	// 7. Drive the session.
	h.SendUserMessage("ping linear")
	h.ExpectAgentReply(e2e.Contains("federated ping ok"))

	// 8. Assert: the MCP stub saw exactly one "ping" call carrying the
	// minted bearer token.
	mcpCalls := h.MCP.Calls()
	require.NotEmpty(t, mcpCalls, "MCP stub must have recorded at least one call")

	var pingCall e2e.ToolCallRecord
	for _, c := range mcpCalls {
		if c.Name == "ping" {
			pingCall = c
			break
		}
	}
	require.Equal(t, "ping", pingCall.Name,
		"expected a 'ping' call on the MCP stub; got %+v", mcpCalls)
	assert.Equal(t, expectedBearer, pingCall.Headers.Get("Authorization"),
		"Authorization header must carry the fake-minted bearer token from resolveFederated")

	// 9. Bonus: the fake Minter recorded the Mint call. Verify the resource
	// and URL passed through correctly.
	require.NotEmpty(t, fakeMinter.Calls, "fake Minter must have recorded at least one Mint call")
	assert.Equal(t, fedResource, fakeMinter.Calls[0].Resource,
		"Mint call must carry the MCPServer's spec.auth.resource as Resource")

	// 10. The session must NOT have parked (no AwaitingCredentials phase)
	// since the IdP-identity Secret was present at park-check time.
	var sessList spiceboxv1alpha1.AgentSessionList
	require.NoError(t, h.K8s.List(ctx, &sessList, client.InNamespace("default")),
		"list AgentSessions")
	for i := range sessList.Items {
		s := &sessList.Items[i]
		assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseAwaitingCredentials, s.Status.Phase,
			"session %s must not have parked: IdP-identity Secret was present", s.Name)
	}

	h.AssertAllRulesConsumed()

	// ── Revoke sub-case ──────────────────────────────────────────────────────
	// Delete the IdP-identity Secret to simulate revocation. A new session
	// (triggered by a fresh message on the archived+idle session) must fail
	// closed: the in-process broker's resolveFederated cannot read the Secret,
	// buildMCPTools fails, RunnerFactory.Start returns an error, and the
	// controller requeues without advancing. The agent never replies.

	require.NoError(t, h.K8s.Delete(ctx, idpSecret), "delete IdP-identity Secret")

	// Wire LLM rules for the revoke attempt. Since the runner never starts
	// (buildMCPTools errors out), no LLM requests will arrive. Mark rules
	// Repeating so they are excluded from any future AssertAllRulesConsumed
	// call and don't cause a test failure if never matched.
	h.LLM.OnUserMessage("ping linear again").Reply(e2e.RespondToUser("should not happen")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn()).Repeating()

	mcpCallCountBefore := len(h.MCP.Calls())

	h.SendUserMessage("ping linear again")

	// Wait a conservative window for the controller to attempt and fail the
	// runner start. The test asserts the negative: no new MCP call arrives.
	// Use a shorter window than the full session timeout.
	time.Sleep(5 * time.Second)

	assert.Equal(t, mcpCallCountBefore, len(h.MCP.Calls()),
		"revoke sub-case: no new MCP calls must reach the stub after the IdP-identity Secret is deleted")
}

// createIdentitiesNamespace creates the agentprimitives-identities namespace.
// envtest doesn't auto-create namespaces; the passthrough gate's IdP-identity
// Secret lookup fails with "namespace not found" otherwise.
func createIdentitiesNamespace(t *testing.T, ctx context.Context, c client.Client) {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.IdentitiesNamespace},
	}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create %s namespace: %v", spiceboxv1alpha1.IdentitiesNamespace, err)
	}
}

// splitMCPServerFromManifests extracts the first MCPServer document from a
// multi-doc YAML blob and returns (mcpServerDoc, remainingDocs). The MCPServer
// doc must be applied AFTER Start so {{MCP_URL}} can be substituted with the
// live MCPStub URL. Mirrors the identical helper in the credentials scenario.
func splitMCPServerFromManifests(yaml string) (mcpServerDoc, remaining string) {
	docs := strings.Split(yaml, "\n---")
	var mcp, rest []string
	for _, d := range docs {
		if strings.Contains(d, "kind: MCPServer") {
			mcp = append(mcp, d)
		} else {
			rest = append(rest, d)
		}
	}
	return strings.Join(mcp, "\n---"), strings.Join(rest, "\n---")
}
