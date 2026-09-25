package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// recordingToolChecker is a hooks.ToolCallChecker that records the authz.Inputs
// subject + subject-list the ToolCallAuthz hook built for the SpiceDB check, so
// a test can assert WHICH principal a contained call is checked against.
type recordingToolChecker struct {
	mu          sync.Mutex
	result      authz.Result
	calls       int
	gotSubject  string
	gotSubjects []string
	gotPerm     authz.Permission
}

func (r *recordingToolChecker) CheckToolCall(_ context.Context, p authz.Permission, in authz.Inputs) authz.Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.gotSubject = in.Subject
	r.gotSubjects = append([]string(nil), in.Subjects...)
	r.gotPerm = p
	return r.result
}

func (r *recordingToolChecker) perm() authz.Permission {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotPerm
}

func (r *recordingToolChecker) subject() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gotSubject
}

func (r *recordingToolChecker) subjects() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.gotSubjects...)
}

// loopWithRecordingAuthz wires the REAL ToolCallAuthz hook (with the real
// BuildInputs closure — the code under test) into an injected executor, but
// swaps the SpiceDB checker for a recorder and disables the approval/record
// side channels so the test observes only the check subject. Enforcing mode.
func loopWithRecordingAuthz(t *testing.T, l *Loop, rec *recordingToolChecker) {
	t.Helper()
	loopWithRecordingAuthzMode(t, l, rec, "enforcing")
}

// loopWithRecordingAuthzMode is loopWithRecordingAuthz with an explicit
// toolCalls.mode, so a test can exercise permissive-mode Eval semantics.
func loopWithRecordingAuthzMode(t *testing.T, l *Loop, rec *recordingToolChecker, mode string) {
	t.Helper()
	deps := l.toolCallAuthzDeps()
	deps.Mode = mode
	deps.Checker = rec
	deps.BuildApprovalAsk = nil
	deps.RecordDecision = nil
	reg := pipeline.NewRegistry()
	reg.Register(hooks.NewToolCallAuthz(deps), hooks.OrderToolCallAuthz)
	loopWithInjectedExecutor(t, l, reg)
}

// checkPerm is a Readonly permission WITH a Check, so ToolCallAuthz actually
// runs the SpiceDB check (and thus BuildInputs) rather than short-circuiting.
func checkPerm() authz.Permission {
	return authz.Permission{
		StateImpact: authz.Readonly,
		Check:       &authz.PermissionCheck{ResourceType: "repo", Permission: "read"},
	}
}

// TestExecuteToolContained_ViewerSubjectAttribution pins problem 1 (D-D1): the
// ToolCallAuthz check subject is the PER-CALL principal threaded through
// containParams — l.authSubject on the LLM path (unchanged), the widget viewer
// on the app-tool (proxy-exec) path — never the loop-level l.authSubject on the
// proxy path.
func TestExecuteToolContained_ViewerSubjectAttribution(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	t.Run("LLM path: check subject == l.authSubject (unchanged)", func(t *testing.T) {
		ft := &fakeDispatchTool{name: "srv", kind: tool.KindMCP, perm: checkPerm(), result: tool.Result{Content: "ok"}}
		l := &Loop{Tools: []tool.Tool{ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		l.authSubjects = []string{"session-subject"}
		rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
		loopWithRecordingAuthz(t, l, rec)

		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			json.RawMessage(`{"args":{}}`), "tu-1", "why",
			containParams{requester: l.authSubject, subjects: l.authSubjects})

		require.Equal(t, containRanOK, oc.Phase)
		require.Equal(t, 1, rec.calls, "the check must run for a Readonly+Check permission")
		assert.Equal(t, "session-subject", rec.subject(), "the LLM path checks against l.authSubject")
		assert.Equal(t, []string{"session-subject"}, rec.subjects())
	})

	t.Run("app-tool path: check subject == the widget viewer (D-D1)", func(t *testing.T) {
		ft := &fakeAppTool{name: "srv", perm: checkPerm(), roHint: true, result: tool.Result{Content: "ok"}}
		// Production fidelity: the app-tool lives ONLY in l.AppTools, the separate
		// registry the LLM never sees. Before the lookup fix, ResolvePermission
		// scanned l.Tools only, so it missed this tool and handed the checker the
		// ZERO permission — the per-resource Check never flowed. Placing it in
		// AppTools is what proves lookupTool now reaches it.
		l := &Loop{AppTools: map[string]tool.Tool{"srv": ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
		// The loop-level subject differs from the viewer — so a check against
		// l.authSubject (the old, wrong behavior) would fail this test.
		l.authSubject = identity.CanonicalFromTrusted("session-subject", "test fixture")
		l.authSubjects = []string{"session-subject"}
		rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
		loopWithRecordingAuthz(t, l, rec)

		var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			json.RawMessage(`{"args":{}}`), "tu-1", "why",
			containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})

		require.Equal(t, containRanOK, oc.Phase)
		require.Equal(t, 1, rec.calls)
		assert.Equal(t, "widget-viewer", rec.subject(), "the app-tool path must check the VIEWER, not l.authSubject")
		assert.Equal(t, []string{"widget-viewer"}, rec.subjects(), "the check subject-list is the viewer's")
		// The security-load-bearing assertion: the checker saw the app-tool's REAL
		// per-resource Check, not the zero permission the l.Tools-only scan handed it.
		require.NotNil(t, rec.perm().Check, "the app-tool's Check must reach the SpiceDB checker (not be dropped to zero)")
		assert.Equal(t, "repo", rec.perm().Check.ResourceType)
	})
}

// TestAppToolAuthz_PermissiveMode_NowEvaluatesRealCheck corrects the mode
// semantics of the lookup fix. Pre-fix, ResolvePermission handed the checker the
// ZERO permission for an app-tool. Under permissive mode that zero permission was
// a fail-OPEN bypass: the app-tool's real per-resource Check was never evaluated
// and the call proceeded regardless. Post-fix the app-tool resolves its REAL
// Readonly+Check permission, so the per-resource Check now actually runs (the
// checker sees repo:read) — even though permissive mode still lets a Readonly
// denial proceed (permissive downgrades readonly/readwrite denials; only external
// always gates). The fix's value under permissive is that the check runs at all;
// under enforcing (default) it is what turns the denial into a real block (see
// TestHandleAppToolCall/"readonly app-tool now enforces the viewer Check").
func TestAppToolAuthz_PermissiveMode_NowEvaluatesRealCheck(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")

	ft := &fakeAppTool{name: "srv", perm: checkPerm(), roHint: true, result: tool.Result{Content: "ok"}}
	// Production fidelity: the app-tool lives ONLY in l.AppTools.
	l := &Loop{AppTools: map[string]tool.Tool{"srv": ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	// The checker denies (an unauthorized viewer). Under permissive this is
	// downgraded to "would deny in enforcing mode" and the call proceeds.
	rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeDenied, Message: "viewer lacks repo:read"}}
	loopWithRecordingAuthzMode(t, l, rec, "permissive")

	var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
	_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
		json.RawMessage(`{"args":{}}`), "tu-1", "why",
		containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})

	// Permissive mode lets the denied readonly call proceed — this is exactly the
	// pre-fix fail-open surface, except now the real Check was evaluated first.
	require.Equal(t, containRanOK, oc.Phase, "permissive mode proceeds past a readonly denial")
	require.Equal(t, 1, rec.calls, "the checker must be invoked under permissive mode too")
	// The load-bearing correction: the checker now receives the app-tool's REAL
	// per-resource Check (repo:read). Pre-fix it received authz.Permission{} (zero,
	// Check==nil) and no per-resource authorization was ever evaluated.
	require.NotNil(t, rec.perm().Check, "permissive-mode Eval must now reach the app-tool's real permission")
	assert.Equal(t, "repo", rec.perm().Check.ResourceType)
}

// TestExecuteToolContained_ProxyExec_AuthSubjectRace is the -race proof for
// problem 1's data race: a proxy-exec contained call run concurrently with a
// goroutine flipping l.authSubject / l.authSubjects (as advanceRequester does
// between turns) is race-clean, because the app-tool path's authz check reads
// the per-call containParams.requester, never the loop-mutated field. Reverting
// BuildInputs to read l.authSubject makes this FAIL under -race.
func TestExecuteToolContained_ProxyExec_AuthSubjectRace(t *testing.T) {
	ft := &fakeDispatchTool{name: "srv", kind: tool.KindMCP, perm: checkPerm(), result: tool.Result{Content: "ok"}}
	l := &Loop{Tools: []tool.Tool{ft}, SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	l.authSubject = identity.CanonicalFromTrusted("start", "test fixture")
	l.authSubjects = []string{"start"}
	rec := &recordingToolChecker{result: authz.Result{Outcome: authz.OutcomeAllowed}}
	loopWithRecordingAuthz(t, l, rec)
	ctx := memory.WithSystemApproval(context.Background(), "test")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				s := fmt.Sprintf("flip-%d", i)
				l.authSubject = identity.CanonicalFromTrusted(s, "test fixture")
				l.authSubjects = []string{s}
			}
		}
	}()

	var viewer = identity.CanonicalFromTrusted("widget-viewer", "test fixture")
	for i := 0; i < 50; i++ {
		_, oc := l.executeToolContained(ctx, dispatchTestSession(), ft, "srv",
			json.RawMessage(`{"args":{}}`), "tu-1", "why",
			containParams{requester: viewer, subjects: []string{viewer.String()}, proxyExec: true})
		require.Equal(t, containRanOK, oc.Phase)
	}
	close(stop)
	<-done

	assert.Equal(t, "widget-viewer", rec.subject(),
		"the proxy-exec check must observe the viewer, never a flipped loop subject")
}

// TestSeqForEmit_AppToolCtx_LastAssistantTurnIndexRace is the -race proof for
// problem 2: seqForEmit invoked with the app-tool synthetic-IDs ctx takes the
// IDsFromCtx branch and NEVER reads l.lastAssistantTurnIndex, so it is
// race-clean against Run's writes to that field. What is asserted here is the
// SEQ, not merely race-cleanliness: the field is atomic now, so a ctx WITHOUT
// the synthetic IDs (the pre-fix shape) would no longer trip -race — it would
// silently stamp the widget's out-of-band emit with whatever turn the loop
// happened to be on, which is the ordering bug the synthetic IDs exist to
// prevent.
func TestSeqForEmit_AppToolCtx_LastAssistantTurnIndexRace(t *testing.T) {
	l := &Loop{SessionKey: memory.NamespacedName{Namespace: "default", Name: "disp"}}
	sess := &tool.SessionContext{Namespace: "default", Name: "disp", AgentSessionUID: "uid-1"}
	ctx := l.appToolExecCtx(context.Background(), sess, "op-1")

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				l.setLastAssistantTurnIndex(i)
			}
		}
	}()

	wantSeq := channelevents.PackSeq(0, channelevents.SeqBlockEnd)
	for i := 0; i < 200; i++ {
		seq, uid := l.seqForEmit(ctx, false)
		assert.Equal(t, wantSeq, seq, "the synthetic-IDs ctx must drive the IDs-path Seq, not lastAssistantTurnIndex")
		assert.Equal(t, "uid-1", uid, "the synthetic IDs carry the real session UID")
	}
	close(stop)
	<-done
}

// TestBuildToolCallApprovalAsk_ProxyExec_UsesViewerRequester pins problem 3
// (D-D1 display): on the app-tool (proxy-exec) path the approval-card Requester
// is the decoded widget viewer; on the LLM path it stays the session's
// LastInbound channel-native identity (byte-for-byte unchanged).
func TestBuildToolCallApprovalAsk_ProxyExec_UsesViewerRequester(t *testing.T) {
	l := loopWithConsolidatedTimeout(t)
	l.Tools = []tool.Tool{&csFakeTool{name: "do_thing"}}
	l.ChannelKind = "slack"
	l.LastInboundExternalID = "U-session"
	ctx := memory.WithSystemApproval(context.Background(), "test")
	perm := authz.Permission{StateImpact: authz.External}

	// The viewer's canonical subject is "user:<base64url(email)>" (bare form,
	// as HandleAppToolCall strips the "user:" prefix into subj).
	const email = "viewer@example.com"
	viewer := identity.CanonicalFromTrusted(base64.RawURLEncoding.EncodeToString([]byte(email)), "test fixture")

	t.Run("proxyExec: Requester is the decoded viewer (idp)", func(t *testing.T) {
		ask, err := l.buildToolCallApprovalAsk(ctx, "do_thing", map[string]any{}, perm, "tu-1", "because", viewer, true)
		require.NoError(t, err)
		require.NotNil(t, ask)
		req, ok := ask.Payload["requester"].(channelevents.ExternalIdentity)
		require.True(t, ok, "payload requester must be a channelevents.ExternalIdentity")
		assert.Equal(t, identity.Kind("idp"), req.Kind, "the viewer is an idp-authenticated browser user")
		assert.Equal(t, identity.Email(email), req.Email, "DecodeForDisplay recovers the viewer's email")
		assert.Equal(t, identity.RawExternalID(email), req.ExternalID)
	})

	t.Run("LLM path: Requester is the session LastInbound (unchanged)", func(t *testing.T) {
		ask, err := l.buildToolCallApprovalAsk(ctx, "do_thing", map[string]any{}, perm, "tu-1", "because", identity.CanonicalFromTrusted("", "test fixture"), false)
		require.NoError(t, err)
		require.NotNil(t, ask)
		req, ok := ask.Payload["requester"].(channelevents.ExternalIdentity)
		require.True(t, ok)
		assert.Equal(t, identity.Kind("slack"), req.Kind)
		assert.Equal(t, identity.RawExternalID("U-session"), req.ExternalID)
	})
}
