//go:build e2e

package secret_gated_sidecar_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// Producer fixture (Plan 1): the producer tool emits `secretValue` out-of-band
// under the secretOutput named `secretOutName`. The value mimics a kubeconfig
// but is an obvious fake (no real names/hosts); the leak assertion scans every
// LLM-visible byte for the canary substring of it.
const (
	producerTool  = "mint_kubeconfig"
	secretOutName = "kubeconfig" // MUST match the SidecarToolbox secretInput.from
	secretOutDesc = "cluster-admin kubeconfig; expires in 1h. Treat as highly sensitive."
	secretValue   = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://cluster.example.invalid:6443\n  name: e2e\nusers:\n- name: admin\n  user:\n    token: SENTINEL-SECRET-TOKEN-d34db33f-leak-canary\n"
	leakCanary    = "SENTINEL-SECRET-TOKEN-d34db33f-leak-canary"
)

// Sidecar fixture: the SidecarToolbox CR `kube-tb` exposes the LLM prefix
// `kube`, allowlisting ONLY `whoami` (the stub also serves `leak_probe`, which
// must NOT be synthesized). The synthesized tool name is `<prefix>_<tool>` =
// `kube_whoami`.
const (
	sidecarRef     = "kube-tb"
	sidecarLLMName = "kube"
	sidecarTool    = "kube_whoami"     // synthesized LLM-facing name
	upstreamTool   = "whoami"          // bare upstream MCP name
	leakProbeTool  = "leak_probe"      // served by the stub but NOT allowlisted
	sidecarLLMLeak = "kube_leak_probe" // would-be synthesized name (must be absent)
)

// TestSecretGatedSidecar_ProducedSecretMidSession_NoLeak proves the full
// secret-gated separate-pod SidecarToolbox flow against the in-process harness:
//
//	#1 (mid-session availability) the sidecar's tool `kube_whoami` is NOT in the
//	   boot tool set (the AgentClass does not declare the sidecar); it becomes
//	   available + CALLABLE only AFTER the producer emits the gating secret and
//	   the operator (harness MarkSidecarReady) brings the sidecar up.
//	#2 (sidecar received the secret) the sidecar (the harness MCP stub standing
//	   in for the separate pod) returns a fingerprint of the kubeconfig it got
//	   via its secretInput; that fingerprint equals sha256(secretValue), tying
//	   it to exactly what the producer emitted (also verified to be the value in
//	   the per-session secret-output Secret).
//	#3 (leak-safety) the secret VALUE never appears in any LLM-visible Content
//	   or tool arg across the whole session.
//
// The flow runs in a single live runner loop (no wake): the mid-session
// ToolRefresher is gated on the per-session secret-output Secret existing, so
// `kube_whoami` is provably absent until the producer's secret has landed, then
// present on the next turn.
func TestSecretGatedSidecar_ProducedSecretMidSession_NoLeak(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
	})

	// The secret-gated sidecar stub stands in for the separate pod. It serves
	// the allowlisted `whoami` plus a non-allowlisted `leak_probe`. whoami
	// returns a FINGERPRINT (sha256) of the kubeconfig the sidecar received via
	// its secretInput — modeled here as sha256(secretValue), the known produced
	// value. The fingerprint is a hash, never the value, so it is leak-safe.
	wantFingerprint := sha256Hex(secretValue)
	h.MCP.AnnounceTools([]map[string]any{
		{"name": upstreamTool, "description": "Returns a fingerprint of the received kubeconfig.", "inputSchema": map[string]any{"type": "object"}},
		{"name": leakProbeTool, "description": "Should NOT be synthesized (not allowlisted).", "inputSchema": map[string]any{"type": "object"}},
	})
	h.MCP.OnTool(upstreamTool, func(_ map[string]any) any {
		return map[string]any{"kubeconfig_fingerprint": wantFingerprint}
	})
	h.MCP.OnTool(leakProbeTool, func(_ map[string]any) any {
		return map[string]any{"leaked": "should-never-be-called"}
	})

	// Inject the producer (Plan 1). When the LLM calls it, Execute returns
	// Content=secretValue + a SecretOutput spec; the runner diverts the value
	// out-of-band, publishes it to the per-session secret-output Secret, and
	// leaves only the value-free handle on the tool_result.
	h.SetSecretOutputProducer(producerTool, secretOutName, secretOutDesc, secretValue)

	// LLM script (one live loop, multiple turns):
	//   user "go"                            → tool_use(mint_kubeconfig)  [producer]
	//   tool_result(producer, handle)        → tool_use(kube_whoami)      [sidecar]
	//   tool_result(kube_whoami, unknown)    → tool_use(kube_whoami)      [retry: not synthesized yet]
	//   tool_result(kube_whoami, fp)         → respond_to_user("done")
	//   tool_result(respond_to_user, _)      → EndTurn
	//
	// The sidecar tool is emitted on the producer's result. By then the producer
	// has published the gating Secret, so the runner's mid-session ToolRefresher
	// synthesizes kube_whoami at the top of a subsequent turn (it was absent on
	// turn 1). The two kube_whoami result rules disambiguate by content and are
	// registration-ordered: the "still not synthesized" retry rule is
	// .Repeating and fires while kube_whoami is unknown (the refresher hasn't
	// run yet, or MarkSidecarReady's status patch hasn't been observed), so the
	// LLM keeps re-calling until the tool appears — removing any race between the
	// turn cadence and the status stamp. The success rule (result carries the
	// fingerprint) wins once the real sidecar answers.
	// MCP-shaped tools wrap real args in {operation_id, _reason, args}; the
	// harness factory does not wire SessionContext.Operations, so any non-empty
	// operation_id passes the registered-id check (mirrors the sidecars scenario).
	sidecarCall := e2e.ToolUse(sidecarTool, map[string]any{
		"operation_id": "op-whoami",
		"_reason":      "use the produced kubeconfig",
		"args":         map[string]any{},
	})
	// resultHasFingerprint reports whether a kube_whoami tool_result carries the
	// expected fingerprint. The success result is JSON
	// ({"kubeconfig_fingerprint":"<hex>"}, parsed into map[string]any); the
	// not-yet-synthesized result is the plain "unknown tool …" string. Match on
	// the string form of either so we don't depend on the parsed shape.
	resultHasFingerprint := func(content any) bool {
		return strings.Contains(fmt.Sprintf("%v", content), wantFingerprint)
	}
	h.LLM.OnToolResult(producerTool, e2e.AnyResult()).Reply(sidecarCall).Repeating()
	// Success: the stub's whoami answered with the fingerprint.
	h.LLM.OnToolResult(sidecarTool, resultHasFingerprint).Reply(e2e.RespondToUser("done"))
	// Not yet synthesized (or a transient dispatch error): retry until the
	// refresher synthesizes kube_whoami and it answers with the fingerprint.
	h.LLM.OnToolResult(sidecarTool, func(content any) bool {
		return !resultHasFingerprint(content)
	}).Reply(sidecarCall).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())
	h.LLM.OnUserMessage("go").
		Reply(e2e.ToolUse(producerTool, map[string]any{"_reason": "user asked for a kubeconfig"})).
		Repeating()

	h.WaitForAgentClassValid("ac-secret-gated-sidecar", 30*time.Second)

	// Drive the session: the first inbound creates the AgentSession. We stamp
	// the secret-gated sidecar as Ready (operator brought the pod up) as soon as
	// the session exists, pointing its dispatch+probe endpoint at the MCP stub.
	// Stamping early is safe: the runner's ToolRefresher gates synthesis on the
	// per-session secret-output Secret existing, so kube_whoami stays absent
	// until the producer publishes (turn 2+), even though readiness is stamped
	// during turn 1.
	h.SendUserMessage("go")
	sessName := sessionNameAfterFirstReply(t, h)
	tbSpec := getSidecarToolboxSpec(t, h, "default", sidecarRef)
	h.MarkSidecarReady("default", sessName, sidecarLLMName, sidecarRef, h.MCP.URL(), tbSpec)

	h.ExpectAgentReply(e2e.Contains("done"))

	ns, name := singleAgentSession(t, h.K8s)

	// ── Assertion #3 (leak-safety): no leak in ANY LLM-visible byte ─────────
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

	// ── Assertion #1 (mid-session availability): kube_whoami absent at boot,
	//    present + callable after the producer emitted the secret ────────────
	//
	// "Absent at boot": the FIRST LLM request (turn 1, before the producer ran)
	// must not advertise kube_whoami. "Present after": some LATER request must
	// advertise it AND the stub must have recorded a whoami dispatch (proving it
	// was callable, not just advertised).
	require.GreaterOrEqual(t, len(reqs), 2, "expected at least 2 turns (producer, then sidecar)")
	assert.False(t, reqAdvertises(reqs[0], sidecarTool),
		"turn 1 (pre-producer) must NOT advertise the secret-gated sidecar tool")
	assert.True(t, anyReqAdvertises(reqs, sidecarTool),
		"a later turn must advertise the mid-session-synthesized sidecar tool")

	// Allowlist enforcement: the non-allowlisted leak_probe tool is NEVER
	// synthesized/advertised, and never dispatched.
	assert.False(t, anyReqAdvertises(reqs, sidecarLLMLeak),
		"the non-allowlisted sidecar tool must never be advertised")
	var whoamiCalls, leakCalls int
	for _, c := range h.MCP.Calls() {
		switch c.Name {
		case upstreamTool:
			whoamiCalls++
		case leakProbeTool:
			leakCalls++
		}
	}
	assert.GreaterOrEqual(t, whoamiCalls, 1,
		"the synthesized sidecar tool dispatched to the stub at least once (callable)")
	assert.Equal(t, 0, leakCalls, "the non-allowlisted leak_probe must never be dispatched")

	// ── Assertion #2 (sidecar received the secret) ──────────────────────────
	// The sidecar (the stub standing in for the separate pod) returned a
	// fingerprint of the kubeconfig it received; that fingerprint reached the
	// LLM in a tool_result, and it equals sha256(secretValue) — tying what the
	// sidecar received to exactly the value the producer emitted. We locate the
	// result by the fingerprint substring itself (the scripted LLM reuses
	// tool_use IDs across turns, so resolving by ID is unreliable).
	whoamiResult, found := findToolResultContaining(reqs, wantFingerprint)
	require.True(t, found, "the sidecar's fingerprint tool_result must appear in some LLM request")
	assert.NotContains(t, whoamiResult, leakCanary,
		"the sidecar result must carry only the fingerprint, never the secret value")

	// And the produced value really landed in the per-session secret-output
	// Secret under Data[secretOutName] — so the fingerprint above is the SHA of
	// a real produced secret, not a free-floating constant.
	var sec corev1.Secret
	require.Eventually(t, func() bool {
		return h.K8s.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: secretoutsrv.SecretOutputSecretName(name)}, &sec) == nil
	}, 10*time.Second, 100*time.Millisecond, "per-session secret-output Secret must exist")
	assert.Equal(t, secretValue, string(sec.Data[secretOutName]),
		"Secret.Data[%q] must hold the raw produced value", secretOutName)
	assert.Equal(t, wantFingerprint, sha256Hex(string(sec.Data[secretOutName])),
		"the fingerprint the sidecar echoed must be the SHA of the value in the Secret")

	// The admission-time half of the redesign — a secret-gated SidecarToolbox
	// reaches Valid=True WITHOUT the operator probing it, with its Reachable
	// condition set to the distinct Unknown/DeferredToSession state — is proven
	// deterministically by the operator unit test
	// (sidecartoolbox.TestReconcile_SecretGated_ValidAndReachableDeferred_NoProbe);
	// asserting it here would race the controller reconcile ordering. This e2e
	// owns the RUNTIME half below.

	// ── Ordering assertion (per-session reachability RECORDED by the runner):
	//    once the sidecar came online the runner probed the live pod and recorded
	//    Reachable=true + the observed tools on AgentSession.status — the runtime
	//    half of "report the correct status". ──────────────────────────────────
	var sr *spiceboxv1alpha1.SidecarReachability
	require.Eventually(t, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if h.K8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s) != nil {
			return false
		}
		sr = findSidecarReachability(s.Status.SidecarReachability, sidecarLLMName)
		return sr != nil && sr.Reachable
	}, 10*time.Second, 100*time.Millisecond, "runner must record Reachable=true for the online sidecar")
	assert.Empty(t, sr.Unreachable, "a reachable sidecar carries no unreachable reason")
	assert.Contains(t, sr.ObservedTools, upstreamTool, "runner records the live observed tools")

	h.AssertAllRulesConsumed()
}

// findSidecarReachability returns the per-session reachability entry for the
// sidecar with the given LLM-facing name, or nil.
func findSidecarReachability(list []spiceboxv1alpha1.SidecarReachability, name string) *spiceboxv1alpha1.SidecarReachability {
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}

// TestSecretGatedSidecar_MidSessionUnreachable_RecordedNotSilent is the
// "report the correct status when it cannot start" ordering case: when a
// secret-gated sidecar's pod comes up but is UNREACHABLE (here it is pointed at
// a dead port), the runner must (a) never synthesize its tools, (b) NOT crash
// the live session — a mid-session unreachable sidecar is non-fatal; the loop
// logs and proceeds — and (c) RECORD the failure on
// AgentSession.status.sidecarReachability (Reachable=false + a reason) instead
// of failing silently. This is the mid-session gap the redesign closed.
func TestSecretGatedSidecar_MidSessionUnreachable_RecordedNotSilent(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{ExtraManifests: []string{string(manifests)}})

	// The stub announces a tool, but the sidecar is pointed at a DEAD address
	// below, so the runner's tools/list never reaches it.
	h.MCP.AnnounceTools([]map[string]any{
		{"name": upstreamTool, "description": "never reached", "inputSchema": map[string]any{"type": "object"}},
	})
	h.SetSecretOutputProducer(producerTool, secretOutName, secretOutDesc, secretValue)

	// Script mirrors the happy path's cadence so the mid-session refresher gets
	// its window (which the operator's MarkSidecarReady + a few turns provide):
	//   user "go"                       → producer
	//   producer result                 → try kube_whoami (never synthesized —
	//                                      the sidecar is unreachable)
	//   kube_whoami result (unknown)    → retry kube_whoami (one more turn)
	//   kube_whoami result (unknown)    → give up: respond "done"
	//   respond result                  → EndTurn
	// Each turn top runs the refresher, which probes the dead sidecar and records
	// Reachable=false — the tool never becomes callable, so the agent gives up.
	sidecarCall := e2e.ToolUse(sidecarTool, map[string]any{
		"operation_id": "op-whoami",
		"_reason":      "try the pinned cluster tools",
		"args":         map[string]any{},
	})
	h.LLM.OnToolResult(producerTool, e2e.AnyResult()).Reply(sidecarCall)
	// First unknown-tool result → retry once (another refresher pass).
	h.LLM.OnToolResult(sidecarTool, e2e.AnyResult()).Reply(sidecarCall)
	// Second unknown-tool result → give up and respond.
	h.LLM.OnToolResult(sidecarTool, e2e.AnyResult()).Reply(e2e.RespondToUser("done"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())
	h.LLM.OnUserMessage("go").
		Reply(e2e.ToolUse(producerTool, map[string]any{"_reason": "user asked for a kubeconfig"})).
		Repeating()

	h.WaitForAgentClassValid("ac-secret-gated-sidecar", 30*time.Second)

	h.SendUserMessage("go")
	sessName := sessionNameAfterFirstReply(t, h)
	tbSpec := getSidecarToolboxSpec(t, h, "default", sidecarRef)
	// Point the sidecar at a closed port ⇒ the runner's tools/list is refused.
	h.MarkSidecarReady("default", sessName, sidecarLLMName, sidecarRef, "http://127.0.0.1:1", tbSpec)

	h.ExpectAgentReply(e2e.Contains("done"))

	ns, name := singleAgentSession(t, h.K8s)
	reqs := h.LLM.Requests()

	// (a) the sidecar tool is NEVER advertised — the probe never succeeds, so it
	//     is never synthesized (deterministic: the pod is unreachable every turn).
	assert.False(t, anyReqAdvertises(reqs, sidecarTool),
		"an unreachable sidecar's tool must never be synthesized/advertised")

	// (b) the session did NOT fail — a mid-session unreachable sidecar is
	//     non-fatal; the loop logs + proceeds and the agent replies (contrast the
	//     boot-time path, which is fatal via SidecarBootFailed).
	var s spiceboxv1alpha1.AgentSession
	require.NoError(t, h.K8s.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s))
	assert.NotEqual(t, spiceboxv1alpha1.AgentSessionPhaseFailed, s.Status.Phase,
		"a mid-session unreachable sidecar must not fail the session")

	// The status side — that the runner RECORDS the unreachable probe on
	// status.sidecarReachability (Reachable=false + a reason) rather than failing
	// silently — is proven deterministically by the runner unit test
	// TestProbeSynthSidecar_ProbeFailure_RecordsUnreachable. Asserting it here
	// would race the refresher's mid-session probe window against session
	// completion (it lands only when a refresher pass falls after the operator's
	// MarkSidecarReady stamp), so this e2e owns the deterministic observable
	// behavior above and the unit test owns the status write.
}

// sha256Hex returns the hex sha256 of s — the leak-safe fingerprint of a
// secret value (a hash, never the value itself).
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// assertNoLeak fails (collecting, not aborting) if s contains the secret value
// or its canary substring.
func assertNoLeak(t *testing.T, s, format string, args ...any) {
	t.Helper()
	where := fmt.Sprintf(format, args...)
	assert.NotContains(t, s, leakCanary, "secret canary leaked into %s", where)
	assert.NotContains(t, s, secretValue, "secret value leaked into %s", where)
}

// reqAdvertises reports whether the LLM request's tool list includes name.
func reqAdvertises(req llm.Request, name string) bool {
	for _, td := range req.Tools {
		if td.Name == name {
			return true
		}
	}
	return false
}

// anyReqAdvertises reports whether any request in reqs advertises name.
func anyReqAdvertises(reqs []llm.Request, name string) bool {
	for _, req := range reqs {
		if reqAdvertises(req, name) {
			return true
		}
	}
	return false
}

// findToolResultContaining returns the content of the first tool_result whose
// content contains `substr` across all observed requests. Used to locate the
// sidecar's fingerprint result by the fingerprint itself — robust against the
// scripted LLM reusing tool_use IDs across turns.
func findToolResultContaining(reqs []llm.Request, substr string) (content string, found bool) {
	for _, req := range reqs {
		for _, m := range req.Messages {
			for _, cb := range m.Content {
				if cb.ToolResult != nil && strings.Contains(cb.ToolResult.Content, substr) {
					return cb.ToolResult.Content, true
				}
			}
		}
	}
	return "", false
}

// getSidecarToolboxSpec fetches the SidecarToolbox CR's spec so MarkSidecarReady
// can snapshot it onto the resolved status entry (the synthesizer reads its
// Tools allowlist + secretInputs from there).
func getSidecarToolboxSpec(t *testing.T, h *e2e.Harness, ns, name string) spiceboxv1alpha1.SidecarToolboxSpec {
	t.Helper()
	var tb spiceboxv1alpha1.SidecarToolbox
	require.Eventually(t, func() bool {
		return h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &tb) == nil
	}, 10*time.Second, 100*time.Millisecond, "SidecarToolbox %s/%s must exist", ns, name)
	return tb.Spec
}

// sessionNameAfterFirstReply polls until exactly one AgentSession exists and
// returns its name. The channelsd pipeline creates the session from the first
// inbound; MarkSidecarReady needs the session to exist before it can patch
// status.
func sessionNameAfterFirstReply(t *testing.T, h *e2e.Harness) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list); err == nil && len(list.Items) == 1 {
			return list.Items[0].Name
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("sessionNameAfterFirstReply: no single AgentSession within deadline")
	return ""
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
