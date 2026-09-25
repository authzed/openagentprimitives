//go:build e2e

package policy_test

import (
	"context"
	"errors"
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
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/secretoutsrv"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// Fake SRE producer fixture. secretValue mimics a cluster-admin kubeconfig but
// is an obvious fake (no real names/hosts). If ANY substring of it — the canary
// especially — leaks into an LLM-visible byte, the leak assertion fails.
const (
	sreProducerTool = "sre_fetch_kubeconfig" // LLM-facing tool name
	sreSecretOut    = "kubeconfig"
	sreSecretDesc   = "cluster-admin kubeconfig; expires in 1h. Treat as highly sensitive."
	sreSecretValue  = "apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://cluster.example.invalid:6443\n  name: e2e\nusers:\n- name: admin\n  user:\n    token: SENTINEL-SRE-KUBECONFIG-d34db33f-leak-canary\n"
	sreLeakCanary   = "SENTINEL-SRE-KUBECONFIG-d34db33f-leak-canary"

	// Slug-shaped cluster ids (Task 7 contract: ^[a-z0-9][a-z0-9_-]{1,63}$).
	sreCluster1 = "fake-cluster-1"
	sreCluster2 = "fake-cluster-2"
)

// TestSREPinning_SecretOutputToPinToWriteOnce proves the SRE pinning pipeline
// end-to-end against the in-process harness with a FAKE producer tool — no
// tailscale, no docker, no authzed-cli:
//
//	#1 the first fetch's tool_result the LLM saw carries the secret-output handle
//	   marker, NOT the raw kubeconfig bytes (runner divert/scrub).
//	#2 the raw value landed in the per-session secret-output Secret under key
//	   `kubeconfig` (the operator /secret-output endpoint the harness mounts).
//	#3 the writesRelationships pin landed in the harness's real SpiceDB:
//	   cluster:fake-cluster-1#pinned@agentsession:<ns>/<session> = HAS_PERMISSION.
//	#4 a SECOND fetch for a DIFFERENT cluster fast-fails write-once (IsError,
//	   content mentions "write-once").
//	#5 the bogus second pin was NEVER written:
//	   cluster:fake-cluster-2#pinned@agentsession:<ns>/<session> = NO_PERMISSION —
//	   the Task 6 fast-fail (which runs BEFORE the pin block) regression check.
//	#6 (leak-safety) the secret value never appears in any LLM-visible byte.
//
// Sidecar-pod assertion (brief Step 1.5) is deliberately SKIPPED here: the
// in-process harness does NOT reconcile SidecarToolboxes into real Pods — the
// secret-gated separate-pod flow (test/e2e/scenarios/secret_gated_sidecar) fakes
// operator readiness via h.MarkSidecarReady and lets the MCP stub stand in for
// the pod. The secret-gated sidecar POD shape (secretInput file: projection,
// secret materialization) is covered by unit + envtest in
// pkg/controllers/agentsession (sidecarpod_test.go, sidecar_inject_envtest_test.go).
func TestSREPinning_SecretOutputToPinToWriteOnce(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join(e2e.TestdataDir("sre_pinning"), "manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
	})

	// Barrier: the SpiceDBBootstrap forces the guardian to compose+write the
	// base platform schema (which defines `cluster` with debug_target/pinned)
	// into the harness SpiceDB, and this blocks until it is applied. Without it
	// the producer's pin WriteRelationships could race a not-yet-written
	// `cluster` definition and fail with "object definition not found".
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	// Inject the fake SRE producer BEFORE spawning the session. On call it emits
	// the fake kubeconfig as a secretOutput (runner diverts it) AND, on success,
	// JIT-pins cluster:<argv[1]>#debug_target@agentsession:<ns>/<session> via the
	// real relwrites.SpiceDBWriter. A second call fast-fails write-once.
	h.SetSREKubeconfigProducer(sreProducerTool, sreSecretOut, sreSecretDesc, sreSecretValue)

	argv1 := []any{"get-kubeconfig", sreCluster1}
	argv2 := []any{"get-kubeconfig", sreCluster2}

	// LLM script (tool_result rules FIRST so they win on tool_result turns — the
	// scripted LLM matches in registration order, and the .Repeating user rule
	// would otherwise re-match the original "pin" text every turn):
	//   user "pin"                          → producer(argv fake-cluster-1)
	//   producer result (handle, success)   → producer(argv fake-cluster-2)
	//   producer result (write-once error)  → respond_to_user("done")
	//   respond_to_user result              → end_turn
	//
	// The two producer result rules disambiguate by content: the first fetch's
	// result carries the scrubbed "<secret-output …>" handle; the second is the
	// write-once IsError string. They are mutually exclusive, so their relative
	// order is irrelevant.
	h.LLM.OnToolResult(sreProducerTool, func(c any) bool {
		return strings.Contains(fmt.Sprintf("%v", c), "<secret-output")
	}).Reply(e2e.ToolUse(sreProducerTool, map[string]any{"argv": argv2, "_reason": "pin the second cluster"}))
	h.LLM.OnToolResult(sreProducerTool, func(c any) bool {
		return strings.Contains(fmt.Sprintf("%v", c), "write-once")
	}).Reply(e2e.RespondToUser("done"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())
	h.LLM.OnUserMessage("pin").
		Reply(e2e.ToolUse(sreProducerTool, map[string]any{"argv": argv1, "_reason": "pin the first cluster"})).
		Repeating()

	h.WaitForAgentClassValid("ac-sre-pinning", 30*time.Second)
	h.SendUserMessage("pin")
	h.ExpectAgentReply(e2e.Contains("done"))

	ns, name := singleAgentSessionSRE(t, h.K8s)
	sessionSubj := "agentsession:" + ns + "/" + name

	reqs := h.LLM.Requests()
	require.NotEmpty(t, reqs, "scripted LLM must have observed at least one request")

	// ── #6 (leak-safety): no secret byte in ANY LLM-visible content ──────────
	for ri, req := range reqs {
		for si, sb := range req.System {
			assertNoSRELeak(t, sb.Text, "request[%d].System[%d]", ri, si)
		}
		for mi, m := range req.Messages {
			for ci, cb := range m.Content {
				assertNoSRELeak(t, cb.Text, "request[%d].Messages[%d].Content[%d].Text", ri, mi, ci)
				if cb.ToolUse != nil {
					assertNoSRELeak(t, string(cb.ToolUse.Input), "request[%d].Messages[%d].Content[%d].ToolUse.Input", ri, mi, ci)
				}
				if cb.ToolResult != nil {
					assertNoSRELeak(t, cb.ToolResult.Content, "request[%d].Messages[%d].Content[%d].ToolResult.Content", ri, mi, ci)
				}
			}
		}
	}

	// ── #1 first fetch: scrubbed handle on the tool_result, value absent ─────
	firstResult, found := findToolResultContainingSRE(reqs, "<secret-output")
	require.True(t, found, "the first fetch's handle tool_result must appear in some LLM request")
	assert.Contains(t, firstResult, sreSecretDesc, "tool_result must carry the secretOutput description")
	assert.NotContains(t, firstResult, sreLeakCanary, "tool_result must NOT carry the raw kubeconfig value")

	// ── #2 value landed in the per-session secret-output Secret ──────────────
	var sec corev1.Secret
	require.Eventually(t, func() bool {
		return h.K8s.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: secretoutsrv.SecretOutputSecretName(name)}, &sec) == nil
	}, 10*time.Second, 100*time.Millisecond, "per-session secret-output Secret must exist")
	assert.Equal(t, sreSecretValue, string(sec.Data[sreSecretOut]),
		"Secret.Data[%q] must hold the raw kubeconfig value", sreSecretOut)

	// ── #3 the pin landed: cluster-1 is pinned to this session ───────────────
	h.AssertSpiceDB("cluster:"+sreCluster1, "pinned", identity.Subject(sessionSubj), true)

	// ── #4 second fetch fast-failed write-once ───────────────────────────────
	secondResult, found := findToolResultContainingSRE(reqs, "write-once")
	require.True(t, found, "the second fetch's write-once error tool_result must appear")
	assert.Contains(t, secondResult, "write-once", "second fetch must be an IsError mentioning write-once")

	// ── #5 the bogus second pin was NEVER written (Task 6 fast-fail order) ────
	h.AssertSpiceDB("cluster:"+sreCluster2, "pinned", identity.Subject(sessionSubj), false)

	h.AssertAllRulesConsumed()
}

// TestSREPinning_ExclusiveWriteOnceIsAtomic proves the atomic write-once
// invariant the LLM-driven test (above) cannot see: the SpiceDB Exclusive
// MUST_NOT_MATCH precondition is the source-of-truth backstop that rejects a
// second pin regardless of the runner-side fast-fail, concurrent same-turn
// dispatch, or a best-effort-status failure.
//
// It drives the harness's REAL relwrites.SpiceDBWriter (the same wiring the
// runner uses, via h.NewSRERelWriter) directly against the harness SpiceDB
// container — the deterministic path from the brief's A6. A fake writer cannot
// model SpiceDB preconditions, so this is the only faithful proof that a second
// exclusive pin for the SAME session but a DIFFERENT cluster is atomically
// rejected and never written.
//
// (The multi-tool-per-turn concurrent path was NOT used: the runner dispatches
// a turn's tool_use blocks concurrently, so which cluster "wins" — plus the
// secret-Secret 409 race — is nondeterministic, and the brief says not to force
// it. This direct-writer path exercises the exact atomic gate that closes the
// Critical, deterministically.)
func TestSREPinning_ExclusiveWriteOnceIsAtomic(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join(e2e.TestdataDir("sre_pinning"), "manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
	})

	// Barrier: force the base platform schema (defining `cluster` with
	// debug_target/pinned, and `agentsession`) into the harness SpiceDB before
	// we write relationships against those definitions.
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	w := h.NewSRERelWriter()
	require.NotNil(t, w, "harness must expose a real SpiceDB-backed relwrites.Writer")

	ctx := context.Background()
	// A synthetic session subject: SpiceDB requires only that the `cluster` and
	// `agentsession` definitions exist (they do, from the bootstrap schema); no
	// AgentSession CR is needed to write a relationship. This isolates the test
	// from the channelsd session-spawn pipeline.
	sessSubj := "agentsession:default/atomic-pin-session"

	// First exclusive pin: the subject holds no debug_target yet, so the
	// MUST_NOT_MATCH precondition passes and the tuple lands.
	require.NoError(t, w.WriteRelationships(ctx, []relwrites.ResolvedTuple{{
		Resource: "cluster:" + sreCluster1, Relation: "debug_target", Subject: sessSubj, Exclusive: true,
	}}), "first exclusive pin must succeed")

	// Second exclusive pin, SAME session, DIFFERENT cluster: the subject now
	// holds debug_target on cluster-1, so the MUST_NOT_MATCH precondition
	// matches → SpiceDB writes NOTHING and returns FAILED_PRECONDITION, which
	// the writer maps to the distinguishable ErrWriteOnceConflict. This is the
	// atomic backstop the Critical demanded.
	err = w.WriteRelationships(ctx, []relwrites.ResolvedTuple{{
		Resource: "cluster:" + sreCluster2, Relation: "debug_target", Subject: sessSubj, Exclusive: true,
	}})
	require.Error(t, err, "second exclusive pin for a different cluster must be rejected")
	require.True(t, errors.Is(err, relwrites.ErrWriteOnceConflict),
		"want ErrWriteOnceConflict from the atomic precondition, got %v", err)

	// Exactly one pin exists: cluster-1 pinned, cluster-2 never written.
	h.AssertSpiceDB("cluster:"+sreCluster1, "pinned", identity.Subject(sessSubj), true)
	h.AssertSpiceDB("cluster:"+sreCluster2, "pinned", identity.Subject(sessSubj), false)
}

// assertNoSRELeak fails (collecting, not aborting) if s contains the secret
// value or its canary substring.
func assertNoSRELeak(t *testing.T, s, format string, args ...any) {
	t.Helper()
	where := fmt.Sprintf(format, args...)
	assert.NotContains(t, s, sreLeakCanary, "secret canary leaked into %s", where)
	assert.NotContains(t, s, sreSecretValue, "secret value leaked into %s", where)
}

// findToolResultContainingSRE returns the content of the first tool_result whose
// content contains substr across all observed requests. Robust against the
// scripted LLM reusing tool_use IDs across turns (mirrors secret_gated_sidecar's
// findToolResultContaining).
func findToolResultContainingSRE(reqs []llm.Request, substr string) (string, bool) {
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

// singleAgentSessionSRE returns the (ns, name) of the one AgentSession the
// channelsd pipeline created for this test.
func singleAgentSessionSRE(t *testing.T, c client.Client) (ns, name string) {
	t.Helper()
	var list spiceboxv1alpha1.AgentSessionList
	require.NoError(t, c.List(context.Background(), &list), "list AgentSessions")
	require.Len(t, list.Items, 1, "expected exactly one AgentSession")
	return list.Items[0].Namespace, list.Items[0].Name
}
