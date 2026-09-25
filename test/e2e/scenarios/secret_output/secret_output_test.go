//go:build e2e

package secret_output_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// These are the producer tool's outputs. secretValue is the raw bytes the
// producer emits; it is a recognizable sentinel so the leak assertion can scan
// every LLM-visible byte for it. The producer's name and the secretOutput name
// are distinct so we can tell the handle scrub from the tool catalog.
const (
	producerTool  = "mint_kubeconfig"
	secretOutName = "kubeconfig"
	secretOutDesc = "cluster-admin kubeconfig; expires in 1h. Treat as highly sensitive."
	// A multi-line sentinel that mimics a real kubeconfig but is obviously
	// fake (no real names/hosts). If ANY substring of this leaks into the
	// LLM context the leak assertion fails.
	secretValue = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://cluster.example.invalid:6443\n  name: e2e\nusers:\n- name: admin\n  user:\n    token: SENTINEL-SECRET-TOKEN-d34db33f-leak-canary\n"
)

// leakCanary is the most distinctive substring of the secret; we assert this
// (and the full value) never appears in any LLM-visible byte.
const leakCanary = "SENTINEL-SECRET-TOKEN-d34db33f-leak-canary"

// TestSecretOutput_CaptureAndLeakSafety proves the secret-output data plane
// end-to-end against the in-process harness:
//
//	#1 (PRIMARY) the secret VALUE never appears in any LLM-visible Content or
//	   tool arg across the whole session (depends only on the runner scrub).
//	#2 the producer's tool_result the LLM saw carries the so-... handle + the
//	   description, NOT the value.
//	#3 the secret value lands in the per-session secret-output Secret under
//	   Data[secretOutName] (via the operator /secret-output endpoint the
//	   harness mounts over the envtest client).
//	#4 AgentSession.status.satisfiedSecretOutputs records the handle.
//
// The producer is injected via h.SetSecretOutputProducer (the
// InProcessRunnerFactory's ExtraTools seam) because the SpiceboxToolspec CRD
// does not yet mirror the toolspec library's secretOutput field — the
// runner-side path it drives is identical to a real sandbox producer's.
func TestSecretOutput_CaptureAndLeakSafety(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
	})

	// Inject the producer tool BEFORE spawning the session. When the LLM calls
	// it, Execute returns Content=secretValue + a SecretOutput spec, which the
	// runner diverts out-of-band.
	h.SetSecretOutputProducer(producerTool, secretOutName, secretOutDesc, secretValue)

	// LLM script:
	//   user "produce"            → tool_use(mint_kubeconfig)
	//   tool_result(producer, _)  → respond_to_user("done")  [the result here
	//                                is already scrubbed to the handle line]
	//   tool_result(respond, _)   → EndTurn
	//
	// The tool_result rules are registered BEFORE the user-message rule so
	// that on a turn whose latest message is a tool_result they win — the
	// scripted LLM matches rules in registration order, and the (repeating)
	// user-message rule would otherwise re-match the original "produce" text
	// every turn and loop the producer until the turn budget is exhausted.
	//
	// .Repeating on the user-message rule guards the cold-start replay race
	// (spec.prompt replay + the fake-injected message can both reach the LLM).
	h.LLM.OnToolResult(producerTool, e2e.AnyResult()).Reply(e2e.RespondToUser("done"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())
	h.LLM.OnUserMessage("produce").
		Reply(e2e.ToolUse(producerTool, map[string]any{"_reason": "user asked for a kubeconfig"})).
		Repeating()

	h.WaitForAgentClassValid("ac-secret-output", 30*time.Second)
	h.SendUserMessage("produce")
	h.ExpectAgentReply(e2e.Contains("done"))

	// ── Assertion #1 (PRIMARY): no leak in ANY LLM-visible byte ──────────────
	// Scan every request the scripted LLM observed: system blocks (tool
	// catalog), and every message's text / tool_use args / tool_result content.
	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "scripted LLM must have observed at least one request")
	for ri, req := range reqs {
		for si, sb := range req.System {
			assertNoLeak(t, sb.Text, "request[%d].System[%d]", ri, si)
		}
		for mi, m := range req.Messages {
			for ci, cb := range m.Content {
				assertNoLeak(t, cb.Text, "request[%d].Messages[%d].Content[%d].Text", ri, mi, ci)
				if cb.ToolUse != nil {
					assertNoLeak(t, string(cb.ToolUse.Input), "request[%d].Messages[%d].Content[%d].ToolUse.Input", ri, mi, ci)
				}
				if cb.ToolResult != nil {
					assertNoLeak(t, cb.ToolResult.Content, "request[%d].Messages[%d].Content[%d].ToolResult.Content", ri, mi, ci)
				}
			}
		}
	}

	// ── Assertion #2: the producer's tool_result the LLM saw is the handle ──
	// Find the producer's tool_result across all observed requests: resolve the
	// tool_use ID for the producer, then locate the tool_result with that ID.
	resultContent, handle, found := findProducerResult(reqs)
	require.True(t, found, "the producer's tool_result must appear in some LLM request")
	assert.Contains(t, resultContent, secretOutDesc, "tool_result must carry the secretOutput description")
	assert.Contains(t, resultContent, "<secret-output", "tool_result must carry the handle marker")
	assert.True(t, strings.HasPrefix(handle, "so-"), "handle must be a so-... ref, got %q", handle)
	assert.NotContains(t, resultContent, leakCanary, "tool_result must NOT carry the secret value")

	// ── Assertion #4: status records the handle → secret name ────────────────
	ns, name := singleAgentSession(t, h.K8s)
	var rec spiceboxv1alpha1.SecretOutputCompletion
	require.Eventually(t, func() bool {
		var sess spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &sess); err != nil {
			return false
		}
		for _, c := range sess.Status.SatisfiedSecretOutputs {
			if c.Handle == handle {
				rec = c
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "AgentSession.status.satisfiedSecretOutputs must record the handle")
	assert.Equal(t, secretoutsrv.SecretOutputSecretName(name), rec.SecretName,
		"recorded SecretName matches the per-session secret-output Secret name")

	// ── Assertion #3: value landed in the per-session secret-output Secret ───
	var sec corev1.Secret
	require.Eventually(t, func() bool {
		return h.K8s.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: secretoutsrv.SecretOutputSecretName(name)}, &sec) == nil
	}, 10*time.Second, 100*time.Millisecond, "per-session secret-output Secret must exist")
	assert.Equal(t, secretValue, string(sec.Data[secretOutName]),
		"Secret.Data[%q] must hold the raw secret value", secretOutName)

	h.AssertAllRulesConsumed()
}

// assertNoLeak fails (collecting, not aborting) if s contains the secret value
// or its canary substring.
func assertNoLeak(t *testing.T, s, format string, args ...any) {
	t.Helper()
	where := fmt.Sprintf(format, args...)
	assert.NotContains(t, s, leakCanary, "secret canary leaked into %s", where)
	assert.NotContains(t, s, secretValue, "secret value leaked into %s", where)
}

// findProducerResult locates the producer tool's (scrubbed) tool_result content
// across all observed requests, plus the so-... handle parsed from it.
func findProducerResult(reqs []llm.Request) (content, handle string, found bool) {
	for _, req := range reqs {
		// Map producer tool_use IDs.
		producerIDs := map[string]bool{}
		for _, m := range req.Messages {
			for _, cb := range m.Content {
				if cb.ToolUse != nil && cb.ToolUse.Name == producerTool {
					producerIDs[cb.ToolUse.ID] = true
				}
			}
		}
		for _, m := range req.Messages {
			for _, cb := range m.Content {
				if cb.ToolResult != nil && producerIDs[cb.ToolResult.ToolUseID] {
					return cb.ToolResult.Content, parseHandle(cb.ToolResult.Content), true
				}
			}
		}
	}
	return "", "", false
}

// parseHandle pulls the so-... ref out of `<secret-output name=".." ref="so-.." bytes=N>`.
func parseHandle(content string) string {
	const marker = `ref="`
	i := strings.Index(content, marker)
	if i < 0 {
		return ""
	}
	rest := content[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

// singleAgentSession returns the (ns, name) of the one AgentSession the
// channelsd pipeline created for this test.
func singleAgentSession(t *testing.T, c client.Client) (ns, name string) {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &list), "list AgentSessions")
	require.Len(t, list.Items, 1, "expected exactly one AgentSession")
	return list.Items[0].Namespace, list.Items[0].Name
}
