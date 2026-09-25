//go:build e2e

// Package credentials_test is the e2e scenario that exercises BOTH credential
// resolution paths — CLI (sandbox) and MCP — in a single session, through the
// in-process token broker (inproc.Broker).
//
// The session calls two tools:
//
//  1. git_git — a sandbox git-status call. The ToolCall controller resolves
//     the AgentIdentity's "git-token" credential via inproc.Broker and stamps
//     GIT_TOKEN on the exec.Request.Env. The test asserts GIT_TOKEN equals the
//     Secret value defined in manifests.yaml.
//
//  2. dcmcp_ping — an MCP call. The InProcessRunnerFactory resolves the
//     AgentIdentity's "mcp-bearer" credential via inproc.Broker and attaches
//     Authorization: Bearer <token> to every HTTP call. The MCPStub records
//     the header; the test asserts the exact bearer value.
//
// Both assertions confirm end-to-end broker resolution under the
// credential-resolution refactor (Plan 1, G2).
package credentials_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec/fake"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds/pod"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// gitToken and mcpBearer are the credential values stamped in manifests.yaml.
// Kept as constants so the assertions read as fact-checks against the fixture,
// not self-referential string comparisons.
const (
	gitToken  = "ghp-e2e-git-credential-a1b2c3d4"
	mcpBearer = "mcp-e2e-bearer-x7y8z9w0"
)

func TestDualCred_CLIAndMCP_BothResolveViaTokenBroker(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join("manifests.yaml"))
	require.NoError(t, err, "read manifests")

	// Strip the MCPServer resource from the manifests applied at Start time.
	// The MCPServer spec.server.url contains the `{{MCP_URL}}` sentinel which
	// must be replaced with the live MCPStub URL — and that URL is only known
	// AFTER Start returns (the MCPStub is wired inside Start). We split the
	// apply into two phases: all non-MCPServer manifests go into ExtraManifests
	// (applied before Start returns), and the MCPServer is applied separately
	// after Start via h.ApplyManifest with the URL substituted.
	rawManifests := string(manifests)
	mcpServerDoc, nonMCPManifests := splitMCPServerFromManifests(rawManifests)

	h := e2e.Start(t, e2e.Options{
		WithToolCallController: true,
		WorkspaceStorageClass:  "rwx-test",
		ExtraManifests:         []string{nonMCPManifests},
		DefaultTimeout:         30 * time.Second,
	})

	// done channels guard against test returning while background goroutines
	// are still running (follows the join-channel pattern from p1_defaultenv).
	stamperDone := make(chan struct{})
	programDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		<-stamperDone
		<-programDone
	})

	// 1. MCP backing. Register before WaitForAgentClassValid so the MCPServer
	// controller's tools/list probe sees the tool and the AgentClass
	// binding-coverage check passes.
	h.MCP.OnTool("ping", func(args map[string]any) any {
		return map[string]any{"echo": args["message"]}
	})

	// Apply the MCPServer now that we have the live MCPStub URL. The sentinel
	// {{MCP_URL}} in the manifest is replaced with the actual httptest URL.
	h.ApplyManifest(strings.ReplaceAll(mcpServerDoc, "{{MCP_URL}}", h.MCP.URL()))

	// 2. Stamp ts-dual-git Valid=True. The e2e harness does not run the
	// SpiceboxToolspec controller, so we must stamp it manually.
	stampToolspecsValid(t, ctx, h.K8s, "ts-dual-git")

	// 3. Background stamper: sets bundle SpiceboxSessions Ready=True + PodName
	// + ResolvedClass once they appear. Required by the ToolCall controller.
	go func() {
		defer close(stamperDone)
		stampBundleSessionsReady(ctx, t, h.K8s)
	}()

	// 4. Background programmer: waits for the git bundle session's PodName and
	// then programs fakeExec with a canned git-status response.
	go func() {
		defer close(programDone)
		programGitPodResponse(ctx, t, h)
	}()

	// 5. LLM script:
	//    "run both tools" → new_operation (mint an operation_id)
	//    tool_result(new_operation, _) → git_git with the captured op id
	//    tool_result(git_git, _) → dcmcp_ping with the same captured op id
	//
	// Both sandbox (git_git) and MCP (dcmcp_ping) tools require operation_id +
	// _reason. The same operation_id from new_operation is valid for all
	// tool_uses within the same operation — SessionContext.Operations.Get checks
	// only that the id was registered, not that it was used by a different tool.
	// We capture it from the new_operation result and use it for both calls.
	var capturedOpID string
	h.LLM.OnUserMessage("run both tools").Reply(e2e.ToolUse("new_operation", map[string]any{
		"description": "run dual-credential scenario",
	}))
	h.LLM.OnToolResult("new_operation", e2e.ResultMatches(func(got any) bool {
		if m, ok := got.(map[string]any); ok {
			if id, ok := m["operation_id"].(string); ok {
				capturedOpID = id
			}
		}
		return true
	})).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("git_git", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "check repo status",
			"args":         []string{"status"},
		})}
	})
	h.LLM.OnToolResult("git_git", e2e.AnyResult()).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("dcmcp_ping", map[string]any{
			"operation_id": capturedOpID,
			"_reason":      "verify MCP credential",
			"args":         map[string]any{"message": "hello"},
		})}
	})
	h.LLM.OnToolResult("dcmcp_ping", e2e.AnyResult()).Reply(
		e2e.RespondToUser("both tools ran"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.WaitForAgentClassValid("ac-dual-cred", 30*time.Second)
	h.SendUserMessage("run both tools")
	h.ExpectAgentReply(e2e.Contains("both tools ran"))

	// 6. Assert: GIT_TOKEN reached the sandbox exec via the ToolCall
	// controller's broker path.
	calls := h.FakeExec().Calls()
	require.NotEmpty(t, calls, "fake exec should have recorded at least one call")

	var gitCall fake.Call
	found := false
	for _, c := range calls {
		if c.Container == "sandbox" {
			gitCall = c
			found = true
			break
		}
	}
	require.True(t, found, "expected an exec call against the git pod (Container=sandbox); got %+v", calls)
	assert.Equal(t, gitToken, gitCall.Request.Env["GIT_TOKEN"],
		"GIT_TOKEN must reach the sandbox exec via the broker (inproc.Broker → CredentialDescriptor → ToolCall.Spec.Credentials)")

	// 7. Assert: Authorization header reached the MCP stub via the runner's
	// MCP broker path (InProcessRunnerFactory.buildMCPTools → inproc.Broker →
	// MCPTool.SetAuth).
	mcpCalls := h.MCP.Calls()
	require.NotEmpty(t, mcpCalls, "MCP stub should have recorded at least one tools/call")

	var pingCall e2e.ToolCallRecord
	for _, c := range mcpCalls {
		if c.Name == "ping" {
			pingCall = c
			break
		}
	}
	require.Equal(t, "ping", pingCall.Name, "expected a 'ping' call on the MCP stub; got %+v", mcpCalls)
	assert.Equal(t, "Bearer "+mcpBearer, pingCall.Headers.Get("Authorization"),
		"Authorization header must carry the mcp-bearer token resolved by inproc.Broker")

	h.AssertAllRulesConsumed()
}

// stampToolspecsValid sets Status.Conditions[Valid]=True on each named
// SpiceboxToolspec so the AgentClass binding-coverage check passes.
// Mirrors the identical helper in p1_defaultenv_reaches_toolcall.
func stampToolspecsValid(t *testing.T, ctx context.Context, c client.Client, names ...string) {
	t.Helper()
	for _, name := range names {
		var ts spiceboxv1alpha1.SpiceboxToolspec
		require.NoError(t, c.Get(ctx, client.ObjectKey{Name: name}, &ts),
			"get toolspec %q", name)
		ts.Status.Conditions = []metav1.Condition{{
			Type:               spiceboxv1alpha1.SpiceboxToolspecConditionValid,
			Status:             metav1.ConditionTrue,
			Reason:             "Resolved",
			LastTransitionTime: metav1.Now(),
		}}
		require.NoError(t, c.Status().Update(ctx, &ts), "stamp toolspec %q Valid=True", name)
	}
}

// stampBundleSessionsReady polls for SpiceboxSessions and stamps each one
// Ready=True + PodName + ResolvedClass. Mirrors the identical helper in
// p1_defaultenv_reaches_toolcall — see that file for detailed commentary.
func stampBundleSessionsReady(ctx context.Context, t *testing.T, c client.Client) {
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := c.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("stampBundleSessionsReady: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			dirty := false

			if s.Status.PodName == "" {
				s.Status.PodName = s.Name + "-pod"
				s.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{
					Kind: pod.KindName,
					Ref:  s.Namespace + "/" + s.Status.PodName,
				}
				dirty = true
			}
			if s.Status.ResolvedClass == nil {
				var cls spiceboxv1alpha1.SpiceboxClass
				if err := c.Get(ctx, client.ObjectKey{Name: s.Spec.Class}, &cls); err != nil {
					if ctx.Err() != nil {
						return
					}
					t.Logf("stampBundleSessionsReady: get class %q for %s/%s: %v",
						s.Spec.Class, s.Namespace, s.Name, err)
					continue
				}
				rc := cls.Spec.DeepCopy()
				s.Status.ResolvedClass = rc
				dirty = true
			}
			if len(s.Status.EffectiveToolspecs) == 0 {
				if len(s.Spec.Toolspecs) > 0 {
					eff := make([]string, 0, len(s.Spec.Toolspecs))
					for _, tr := range s.Spec.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				} else if s.Status.ResolvedClass != nil {
					eff := make([]string, 0, len(s.Status.ResolvedClass.Toolspecs))
					for _, tr := range s.Status.ResolvedClass.Toolspecs {
						eff = append(eff, tr.Name)
					}
					s.Status.EffectiveToolspecs = eff
				}
				if len(s.Status.EffectiveToolspecs) > 0 {
					dirty = true
				}
			}
			if !meta.IsStatusConditionTrue(s.Status.Conditions, spiceboxv1alpha1.SpiceboxSessionConditionReady) {
				s.Status.Conditions = append(s.Status.Conditions, metav1.Condition{
					Type:               spiceboxv1alpha1.SpiceboxSessionConditionReady,
					Status:             metav1.ConditionTrue,
					Reason:             spiceboxv1alpha1.ReasonPodReady,
					Message:            "stamped Ready by e2e harness helper",
					LastTransitionTime: metav1.Now(),
				})
				dirty = true
			}
			if !dirty {
				continue
			}
			if err := c.Status().Update(ctx, s); err != nil {
				if ctx.Err() != nil {
					return
				}
				t.Logf("stampBundleSessionsReady: update %s/%s: %v",
					s.Namespace, s.Name, err)
			}
		}
	}
}

// programGitPodResponse polls for the git bundle's SpiceboxSession, waits for
// PodName to be set by stampBundleSessionsReady, then programs the fakeExec
// with a canned git-status response. Mirrors the identical helper in
// p1_defaultenv_reaches_toolcall.
func programGitPodResponse(ctx context.Context, t *testing.T, h *e2e.Harness) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		var list spiceboxv1alpha1.SpiceboxSessionList
		if err := h.K8s.List(ctx, &list, client.InNamespace("default")); err != nil {
			if ctx.Err() != nil {
				return
			}
			t.Logf("programGitPodResponse: list: %v", err)
			continue
		}
		for i := range list.Items {
			s := &list.Items[i]
			if s.Labels["agentprimitives.authzed.com/agentbundle"] != "git" {
				continue
			}
			if s.Status.PodName == "" {
				continue
			}
			key := s.Namespace + "/" + s.Status.PodName + ":sandbox"
			h.FakeExec().Program(key, fake.Response{
				Stdout:   []byte("nothing to commit, working tree clean\n"),
				ExitCode: 0,
			})
			return
		}
	}
}

// splitMCPServerFromManifests separates the MCPServer YAML document from the
// rest of the multi-doc YAML. Returns (mcpServerDoc, remainingDocs).
//
// The MCPServer spec.server.url contains the `{{MCP_URL}}` sentinel that must
// be replaced with the live MCPStub URL before applying. Since the MCPStub URL
// is only known AFTER the harness is started, the MCPServer document must be
// applied separately (after Start) with the sentinel substituted.
//
// Detection: a document is the MCPServer if it contains "kind: MCPServer".
func splitMCPServerFromManifests(yaml string) (mcpServerDoc, remaining string) {
	// Split on YAML document separators.
	docs := strings.Split(yaml, "\n---")
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
