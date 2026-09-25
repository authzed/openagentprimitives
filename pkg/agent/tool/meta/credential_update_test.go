package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Fixed fixture names shared by every test in this file.
const (
	curNS       = "demo-ns"
	curSessName = "demo-session"
	curStarter  = "demo-user"
)

const curSessUID types.UID = "demo-session-uid"

// curSessUIDReal is deliberately DIFFERENT from curSessUID. curSessUID is
// what curSessCtx() puts on SessionContext.AgentSessionUID (used only by the
// reattach List-filter, ownedByAgentSession); curSessUIDReal is what the
// fixture AgentSession object itself carries as its real .UID. Keeping them
// distinct in TestCredentialUpdate_CreatesRequestWithOriginAndCappedWhy pins
// that the created ownerReference's UID comes from a live Client.Get of the
// AgentSession, not from trusting sess.AgentSessionUID -- an implementation
// that read the latter instead would silently pass if the two happened to
// share one value.
const curSessUIDReal types.UID = "demo-session-real-uid"

func credUpdateScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s), "AddToScheme")
	return s
}

// stubOriginTool is a minimal tool.Tool + tool.OriginTool double standing in
// for a tool that belongs to a shared upstream (an MCP server, a sidecar
// toolbox) with a managed credential.
type stubOriginTool struct {
	name   string
	origin string
}

func (s stubOriginTool) Name() string               { return s.name }
func (stubOriginTool) Kind() tool.Kind              { return tool.KindMCP }
func (stubOriginTool) Description() string          { return "stub origin tool" }
func (stubOriginTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (stubOriginTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (stubOriginTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (s stubOriginTool) Origin() string                              { return s.origin }
func (stubOriginTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

// stubPlainTool implements tool.Tool but deliberately NOT tool.OriginTool --
// e.g. a sandbox or meta tool with no shared-upstream credential.
type stubPlainTool struct{ name string }

func (s stubPlainTool) Name() string               { return s.name }
func (stubPlainTool) Kind() tool.Kind              { return tool.KindSandbox }
func (stubPlainTool) Description() string          { return "stub plain tool" }
func (stubPlainTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (stubPlainTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (stubPlainTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (stubPlainTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

func lookupFor(tools ...tool.Tool) func(string) (tool.Tool, bool) {
	return func(name string) (tool.Tool, bool) {
		for _, tl := range tools {
			if tl.Name() == name {
				return tl, true
			}
		}
		return nil, false
	}
}

func curSessionFixture() *spiceboxv1alpha1.AgentSession {
	return &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{
			Name: curSessName, Namespace: curNS, UID: curSessUID,
			Annotations: map[string]string{
				spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:" + curStarter,
			},
		},
	}
}

func curSessCtx() *tool.SessionContext {
	return &tool.SessionContext{Namespace: curNS, Name: curSessName, AgentSessionUID: curSessUID}
}

// driveCredentialUpdateRequestPhase polls until a CredentialUpdateRequest
// exists in ns, then patches its status to phase/reason. Deliberately takes
// no *testing.T: it runs on a goroutine the test doesn't join, and calling
// t.Fatal/t.FailNow off the test goroutine is unsafe. A request that never
// appears is surfaced instead by the caller's own assertions timing out via
// the tool's MaxWait.
func driveCredentialUpdateRequestPhase(c client.Client, ns, name, phase, reason string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var cr spiceboxv1alpha1.CredentialUpdateRequest
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cr); err == nil {
			cr.Status.Phase = phase
			cr.Status.Reason = reason
			_ = c.Status().Update(context.Background(), &cr)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// driveFirstCredentialUpdateRequestPhase is driveCredentialUpdateRequestPhase
// for a CR created BY the tool under test, whose generated name the caller
// doesn't know in advance -- it lists rather than Gets by name.
func driveFirstCredentialUpdateRequestPhase(c client.Client, ns, phase, reason string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.CredentialUpdateRequestList
		if err := c.List(context.Background(), &list, client.InNamespace(ns)); err == nil && len(list.Items) > 0 {
			cr := list.Items[0]
			cr.Status.Phase = phase
			cr.Status.Reason = reason
			_ = c.Status().Update(context.Background(), &cr)
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestCredentialUpdate_UnknownToolRefusesInline(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).WithObjects(curSessionFixture()).Build()
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(), PollInterval: 5 * time.Millisecond, MaxWait: time.Second,
	})
	args, err := json.Marshal(map[string]any{"tool": "no-such-tool", "why": "401 unauthorized on every call"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "unknown tool must produce IsError")
	assert.Contains(t, res.Content, "no-such-tool", "error must name the unknown tool")

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
	assert.Empty(t, list.Items, "an unknown tool must not create a CredentialUpdateRequest")
}

func TestCredentialUpdate_OriginlessToolRefusesInline(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).WithObjects(curSessionFixture()).Build()
	plain := stubPlainTool{name: "local_sandbox_tool"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(plain), PollInterval: 5 * time.Millisecond, MaxWait: time.Second,
	})
	args, err := json.Marshal(map[string]any{"tool": "local_sandbox_tool", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "an origin-less tool must produce IsError")
	assert.Contains(t, strings.ToLower(res.Content), "no managed credential", "error must explain the tool uses no managed credential")

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
	assert.Empty(t, list.Items, "an origin-less tool must not create a CredentialUpdateRequest")
}

// TestCredentialUpdate_EmptyOriginToolRefusesInline covers a tool that DOES
// implement tool.OriginTool but returns "" -- sandbox.SandboxTool's
// documented origin-less convention when it has no toolkit (see
// pkg/agent/runner/pipeline_wiring.go's toolGuardLookup, which treats ""
// identically to "no OriginTool at all"). A check that only asserts the
// interface, without also checking the returned value, would pass this
// tool through and write a CR with spec.origin="" -- caught eventually by
// the reconciler's own NoCredential refusal, but only after a wasted CR
// write and poll/reconcile round trip.
func TestCredentialUpdate_EmptyOriginToolRefusesInline(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).WithObjects(curSessionFixture()).Build()
	toolkitless := stubOriginTool{name: "toolkitless_tool", origin: ""}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(toolkitless), PollInterval: 5 * time.Millisecond, MaxWait: time.Second,
	})
	args, err := json.Marshal(map[string]any{"tool": "toolkitless_tool", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")

	res, err := tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "a tool whose Origin() is empty must produce IsError")
	assert.Contains(t, strings.ToLower(res.Content), "no managed credential", "error must explain the tool uses no managed credential")

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
	assert.Empty(t, list.Items, "an empty-origin tool must not create a CredentialUpdateRequest")
}

func TestCredentialUpdate_CreatesRequestWithOriginAndCappedWhy(t *testing.T) {
	// The fixture AgentSession's real UID (curSessUIDReal) deliberately
	// differs from curSessCtx()'s AgentSessionUID (curSessUID) -- see
	// curSessUIDReal's doc comment.
	sessionFixture := curSessionFixture()
	sessionFixture.UID = curSessUIDReal

	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(sessionFixture).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 20 * time.Millisecond,
	})
	longWhy := strings.Repeat("a", 500)
	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": longWhy})
	require.NoError(t, err, "marshal args")

	// Nothing drives the CR to a terminal phase here -- this test is about
	// the CR's shape, not the poll outcome -- so Execute times out. That's
	// expected and not itself under test.
	_, err = tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
	require.Len(t, list.Items, 1, "exactly one CredentialUpdateRequest must be created")
	got := list.Items[0]

	assert.Equal(t, "mcpserver/github", got.Spec.Origin, "spec.origin must equal the tool's Origin()")
	assert.Equal(t, "github_create_issue", got.Spec.ToolName, "spec.toolName must equal the tool's Name()")
	assert.Len(t, []rune(got.Spec.Why), 280, "why must be capped to exactly 280 runes")
	assert.Equal(t, strings.Repeat("a", 280), got.Spec.Why, "why must be a straight prefix, not paraphrased")
	assert.Equal(t, curNS, got.Spec.SessionRef.Namespace, "spec.sessionRef.namespace must be the session's OWN namespace")
	assert.Equal(t, curSessName, got.Spec.SessionRef.Name)
	assert.Equal(t, identity.Subject("user:"+curStarter), got.Spec.RequestedBy, "requestedBy must be the session starter's canonical subject")

	require.Len(t, got.OwnerReferences, 1, "the CR must carry a genuine ownerReference to the AgentSession")
	ref := got.OwnerReferences[0]
	assert.Equal(t, "AgentSession", ref.Kind)
	assert.Equal(t, curSessName, ref.Name)
	require.NotEqual(t, curSessUID, curSessUIDReal, "sanity: the fixture's two UIDs must actually differ")
	assert.Equal(t, curSessUIDReal, ref.UID,
		"ownerReference UID must come from a live Get of the AgentSession (curSessUIDReal), not from sess.AgentSessionUID (curSessUID) -- without a genuine ownerReference every request refuses NoCredential")
}

func TestCredentialUpdate_PollsToFulfilled(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 2 * time.Second,
	})

	go driveFirstCredentialUpdateRequestPhase(c, curNS, spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, "")

	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")
	require.False(t, res.IsError, "Fulfilled must not be an error: %s", res.Content)
	assert.True(t, res.Trusted, "result must be Trusted -- content is entirely platform-authored")
	assert.Contains(t, strings.ToLower(res.Content), "retry", "the agent must be told to retry its original call")
}

func TestCredentialUpdate_PollsToRefusedSurfacesReasonVerbatim(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 2 * time.Second,
	})

	const reason = "This credential authenticated successfully just now; the failure was a permissions or scope problem, not an expired token."
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var list spiceboxv1alpha1.CredentialUpdateRequestList
			if err := c.List(context.Background(), &list, client.InNamespace(curNS)); err == nil && len(list.Items) > 0 {
				cr := list.Items[0]
				cr.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseRefused
				cr.Status.Determination = spiceboxv1alpha1.CredentialUpdateDeterminationCredentialLive
				cr.Status.Reason = reason
				_ = c.Status().Update(context.Background(), &cr)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")
	res, err := tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")
	assert.True(t, res.IsError, "a Refused determination must be IsError")
	assert.Contains(t, res.Content, reason, "the platform-authored reason must be surfaced VERBATIM")
}

func TestCredentialUpdate_MaxWaitReturnsIsError(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 20 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")

	done := make(chan struct{})
	var res tool.Result
	go func() {
		res, _ = tl.Execute(context.Background(), args, curSessCtx())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return within 2s of a 20ms MaxWait -- it is hanging instead of respecting MaxWait")
	}
	assert.True(t, res.IsError, "leaving the request Open for the whole MaxWait must return IsError, not hang forever")
	assert.Contains(t, res.Content, "no card was ever delivered",
		"nothing ever set InteractionRef in this test -- the MaxWait message must say nobody was asked yet (Task 7 review, New-Important-1), not imply a human silently ignored a card")
}

// TestCredentialUpdate_MaxWaitAfterDeliveryUsesGenericTimeoutMessage pins the
// other half of New-Important-1: once channelsd HAS delivered a card
// (InteractionRef stamped non-empty, exactly as CredentialUpdateWatcher does
// in the same patch as CardDelivered=True) but the human still hasn't acted
// by MaxWait, the tool's timeout message must NOT claim nobody was asked --
// it uses the original generic "still open" wording instead.
func TestCredentialUpdate_MaxWaitAfterDeliveryUsesGenericTimeoutMessage(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 100 * time.Millisecond,
	})

	// Simulate channelsd having delivered the card: stamp InteractionRef
	// non-empty shortly after the CR is created, then leave it Open for the
	// rest of MaxWait so the tool times out anyway.
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			var list spiceboxv1alpha1.CredentialUpdateRequestList
			if err := c.List(context.Background(), &list, client.InNamespace(curNS)); err == nil && len(list.Items) > 0 {
				cr := list.Items[0]
				cr.Status.InteractionRef = "req-delivered-fake"
				_ = c.Status().Update(context.Background(), &cr)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")

	done := make(chan struct{})
	var res tool.Result
	go func() {
		res, _ = tl.Execute(context.Background(), args, curSessCtx())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Execute did not return in time -- it is hanging instead of respecting MaxWait")
	}
	assert.True(t, res.IsError, "leaving the request Open for the whole MaxWait must return IsError")
	assert.NotContains(t, res.Content, "no card was ever delivered",
		"InteractionRef was stamped -- a card WAS delivered, so the message must not claim otherwise")
	assert.Contains(t, res.Content, "timed out waiting for a human decision",
		"a delivered-but-unresolved request keeps the original generic timeout wording")
}

// TestCredentialUpdate_MaxWaitOnACollapsedRequest pins the give-up outcome
// collapsing created, and BOTH falsehoods available on this path.
//
// A FOLLOWER -- a request whose credential already has an ask in flight for
// another session -- is never delivered a card of its own, deliberately: it
// carries no InteractionRef, precisely so the expiry wording elsewhere cannot
// claim a human was shown something. That makes it fall past the delivered
// branch, and with reads succeeding it once landed in the final branch, which
// tells the agent "no card was ever delivered to a human, so nobody has been
// asked yet. The request is still open". Both clauses are wrong for a follower:
// something IS in flight for this credential, and the request is Collapsed, not
// Open.
//
// The SECOND falsehood is the one the first fix introduced. "A human has
// already been asked" is a claim about publication, which happens in channelsd
// -- a separate process, with its own silent skip paths. A follower whose
// CANONICAL was never delivered has had nobody asked on its behalf at all, so
// the strong wording has to be gated on the canonical's own delivery mark
// rather than on this request's phase. The two rows are exactly that gate: same
// follower, same reads, differing ONLY in whether the canonical carries an
// InteractionRef.
//
// This is routine, not a corner: MaxWait is the runner-side tool wait and is
// far shorter than the window a human gets, so a follower times out here while
// its canonical's ask is still perfectly live.
//
// Each row asserts the OTHER row's wording is ABSENT as well as its own being
// present -- either half alone would pass against a message that changed shape
// without becoming true, and against a gate wired to a constant.
func TestCredentialUpdate_MaxWaitOnACollapsedRequest(t *testing.T) {
	const canonicalName = "cur-another-session"

	cases := []struct {
		name string
		// canonicalDelivered stamps the CANONICAL's interactionRef, which is what
		// channelsd does when it really publishes -- the one fact that entitles
		// this follower's message to say a human was asked.
		canonicalDelivered bool
		// canonicalMissing deletes the canonical outright: its session ended and
		// owner-ref GC took its request. Delivery cannot be established, so the
		// weaker wording must win here too.
		canonicalMissing bool
		wantContains     []string
		wantAbsent       []string
	}{
		{
			name:               "canonical WAS delivered: the agent is told a human is already looking at this",
			canonicalDelivered: true,
			wantContains: []string{
				"A human has already been asked to replace this credential",
				"waiting on",
			},
			wantAbsent: []string{"could not be confirmed"},
		},
		{
			name:               "canonical was never delivered: says a shared ask is in flight, delivery unconfirmed",
			canonicalDelivered: false,
			wantContains: []string{
				"Another session already raised a request to replace this same credential",
				"could not be confirmed",
				"waiting on",
			},
			// The whole point: nobody has been asked yet, so the strong claim is
			// false and must not appear.
			wantAbsent: []string{"A human has already been asked"},
		},
		{
			name:             "canonical no longer exists: delivery is unestablishable, so the weaker wording wins",
			canonicalMissing: true,
			wantContains: []string{
				"could not be confirmed",
				"waiting on",
			},
			wantAbsent: []string{"A human has already been asked"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The canonical: another session's request for the same credential,
			// carrying the delivery mark (or not) this row is about.
			canonical := &spiceboxv1alpha1.CredentialUpdateRequest{
				ObjectMeta: metav1.ObjectMeta{Name: canonicalName, Namespace: curNS},
				Status: spiceboxv1alpha1.CredentialUpdateRequestStatus{
					Phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
				},
			}
			if tc.canonicalDelivered {
				canonical.Status.InteractionRef = "req-canonical-delivered"
			}
			objs := []client.Object{curSessionFixture()}
			if !tc.canonicalMissing {
				objs = append(objs, canonical)
			}

			c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
				WithObjects(objs...).
				WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).Build()

			// Positive control: when this row seeds a canonical, its delivery mark
			// really is readable through the very field the tool reads. Without
			// this, the "delivered" row could pass against a fixture whose stamp
			// never landed -- which is indistinguishable from the gate being wired
			// to a constant false.
			if !tc.canonicalMissing {
				var seeded spiceboxv1alpha1.CredentialUpdateRequest
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: curNS, Name: canonicalName}, &seeded), "Get seeded canonical")
				require.Equal(t, tc.canonicalDelivered, seeded.Status.InteractionRef != "",
					"control: the canonical's delivery mark must be exactly what this row set")
			}

			origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
			tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
				Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 100 * time.Millisecond,
			})

			// Simulate the operator collapsing this request onto the canonical:
			// phase Collapsed + collapsedInto, and -- as the collapse path
			// guarantees -- NO InteractionRef of its own. Left that way for the
			// rest of MaxWait, so the tool times out on a follower still waiting.
			go func() {
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) {
					var list spiceboxv1alpha1.CredentialUpdateRequestList
					if err := c.List(context.Background(), &list, client.InNamespace(curNS)); err == nil {
						for i := range list.Items {
							cr := list.Items[i]
							if cr.Name == canonicalName {
								continue // never touch the canonical
							}
							cr.Status.Phase = spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed
							cr.Status.Determination = spiceboxv1alpha1.CredentialUpdateDeterminationCollapsed
							cr.Status.CollapsedInto = &spiceboxv1alpha1.NamespacedRef{Namespace: curNS, Name: canonicalName}
							_ = c.Status().Update(context.Background(), &cr)
							return
						}
					}
					time.Sleep(2 * time.Millisecond)
				}
			}()

			args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
			require.NoError(t, err, "marshal args")

			done := make(chan struct{})
			var res tool.Result
			go func() {
				res, _ = tl.Execute(context.Background(), args, curSessCtx())
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Execute did not return in time -- it is hanging instead of respecting MaxWait")
			}

			assert.True(t, res.IsError, "a follower that timed out without an answer is still an error result")
			// True on BOTH rows: the follower branch must beat the never-delivered
			// branch either way, and the request is Collapsed, not Open.
			assert.NotContains(t, res.Content, "nobody has been asked yet",
				"an ask for this credential IS in flight, so this flat claim is wrong on both rows; content=%q", res.Content)
			assert.NotContains(t, res.Content, "no card was ever delivered",
				"the follower branch must win over the never-delivered branch; content=%q", res.Content)
			assert.NotContains(t, res.Content, "The request is still open",
				"the request is Collapsed, not Open; content=%q", res.Content)

			for _, want := range tc.wantContains {
				assert.Contains(t, res.Content, want, "content=%q", res.Content)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, res.Content, absent,
					"this row's message must not borrow the other row's claim; content=%q", res.Content)
			}
		})
	}
}

// TestCredentialUpdate_MaxWaitAfterUnreadableStatusSaysUnknown pins the third
// give-up outcome. When the poll loop's Get keeps failing for a non-NotFound
// reason -- the exact shape of a missing RBAC rule (403) or an apiserver blip
// -- `fresh` is never populated, so its InteractionRef is empty for a reason
// that has NOTHING to do with whether a card was published. Reporting "no card
// was ever delivered to a human, so nobody has been asked yet" there would be a
// confident false claim manufactured out of a swallowed error. The message must
// instead name the error and say the outcome is unknown.
func TestCredentialUpdate_MaxWaitAfterUnreadableStatusSaysUnknown(t *testing.T) {
	getErr := apierrors.NewForbidden(
		schema.GroupResource{Group: "agentprimitives.authzed.com", Resource: "credentialupdaterequests"},
		"cur-demo", errors.New("credentialupdaterequests is forbidden"))

	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			// Fail ONLY the poll loop's Get of a CredentialUpdateRequest; the
			// AgentSession Get that createRequest does must still succeed, so
			// the tool gets as far as polling.
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*spiceboxv1alpha1.CredentialUpdateRequest); ok {
					return getErr
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 30 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
	require.NoError(t, err, "marshal args")

	done := make(chan struct{})
	var res tool.Result
	go func() {
		res, _ = tl.Execute(context.Background(), args, curSessCtx())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Execute did not return within 2s of a 30ms MaxWait -- a failing Get must not stop the deadline from firing")
	}

	assert.True(t, res.IsError, "giving up after unreadable status must return IsError")
	assert.NotContains(t, res.Content, "nobody has been asked yet",
		"the reads FAILED -- claiming nobody was asked asserts the opposite of what is known")
	assert.Contains(t, res.Content, "UNKNOWN",
		"the message must say the outcome is unknown rather than manufacture a determination")
	assert.Contains(t, res.Content, "forbidden",
		"the swallowed error must be surfaced so an operator can tell a 403 from an apiserver blip")
}

// TestCredentialUpdate_ReattachesToAnyNonTerminalRequest covers every phase a
// runner restart mid-park can leave behind, because findOpenRequest's contract
// is NON-TERMINAL, not "Open" -- and the three differ in what a mistake costs.
//
// The rows are the fall-through set of the reconciler's phase gate plus
// Collapsed, and Collapsed is the expensive one. It is the shape collapsing
// itself creates: this session's ask is riding on another session's card. A
// reattach narrowed to Open finds nothing there, creates a SECOND request, and
// burns the second of this session's two lifetime asks on a duplicate about a
// credential a human is already looking at -- after which the session is out of
// budget and its next genuine ask is refused. Pending is the cheaper sibling:
// undetermined, so a duplicate spends nothing yet, but it still leaves two
// requests where the reconciler expects at most one.
//
// Every row's control is the SAME pair of claims -- the poll settled on the
// pre-existing request, and no second request exists -- so a narrowed filter
// reddens on the phases it dropped rather than on wording.
func TestCredentialUpdate_ReattachesToAnyNonTerminalRequest(t *testing.T) {
	const existingName = "cur-preexisting"

	cases := []struct {
		name  string
		phase string
	}{
		{
			name:  "Open: a live card of this session's own -- the obvious reattach",
			phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseOpen,
		},
		{
			name:  "Collapsed: riding another session's card, so a duplicate would burn the second of two asks",
			phase: spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed,
		},
		{
			name:  "Pending: determined by nobody yet, and still not a reason to raise a second request",
			phase: spiceboxv1alpha1.CredentialUpdateRequestPhasePending,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, spiceboxv1alpha1.IsCredentialUpdateRequestTerminal(tc.phase),
				"control: %q must really be non-terminal, or reattaching to it is not the contract", tc.phase)

			existing := &spiceboxv1alpha1.CredentialUpdateRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name: existingName, Namespace: curNS,
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(), Kind: "AgentSession",
						Name: curSessName, UID: curSessUID,
					}},
				},
				Spec: spiceboxv1alpha1.CredentialUpdateRequestSpec{
					SessionRef: spiceboxv1alpha1.NamespacedRef{Namespace: curNS, Name: curSessName},
					Origin:     "mcpserver/github", ToolName: "github_create_issue", Why: "the original why",
				},
				Status: spiceboxv1alpha1.CredentialUpdateRequestStatus{Phase: tc.phase},
			}
			if tc.phase == spiceboxv1alpha1.CredentialUpdateRequestPhaseCollapsed {
				existing.Status.CollapsedInto = &spiceboxv1alpha1.NamespacedRef{
					Namespace: curNS, Name: "cur-another-session"}
			}

			listed := make(chan struct{})
			var listedOnce sync.Once
			c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
				WithObjects(curSessionFixture(), existing).
				WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
				WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						err := cl.List(ctx, list, opts...)
						if _, ok := list.(*spiceboxv1alpha1.CredentialUpdateRequestList); ok {
							listedOnce.Do(func() { close(listed) })
						}
						return err
					},
				}).Build()

			origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
			tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
				Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 2 * time.Second,
			})

			// Only the PRE-EXISTING request is ever driven to Fulfilled. A tool
			// that created a second one instead polls an object nothing settles
			// and times out -- which is what makes the assertions below about
			// reattachment rather than about the poll loop.
			go func() {
				<-listed
				driveCredentialUpdateRequestPhase(c, curNS, existingName,
					spiceboxv1alpha1.CredentialUpdateRequestPhaseFulfilled, "")
			}()

			args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "401 unauthorized"})
			require.NoError(t, err, "marshal args")
			res, err := tl.Execute(context.Background(), args, curSessCtx())
			require.NoError(t, err, "Execute must not return a Go error")
			assert.False(t, res.IsError,
				"the reattached request reaching Fulfilled must not be an error: %s", res.Content)

			var list spiceboxv1alpha1.CredentialUpdateRequestList
			require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
			require.Len(t, list.Items, 1,
				"reattaching must not create a second CredentialUpdateRequest -- a second ask about the same "+
					"credential spends this session's budget on a duplicate")
			assert.Equal(t, existingName, list.Items[0].Name)
			assert.Equal(t, "the original why", list.Items[0].Spec.Why,
				"the pre-existing request's spec must be left untouched")
		})
	}
}

// TestCredentialUpdate_RequestNameIsServerAssigned — the tool must not pick the
// object's name itself.
//
// A client-side random suffix can collide, and a collision surfaces to the
// agent as an AlreadyExists error on the one ask it just spent: the agent has
// no way to retry it differently, because the name is not something it chose or
// can vary. GenerateName hands uniqueness to the apiserver, which resolves a
// collision itself by retrying with a fresh suffix.
func TestCredentialUpdate_RequestNameIsServerAssigned(t *testing.T) {
	var sentName, sentGenerateName string
	c := fake.NewClientBuilder().WithScheme(credUpdateScheme(t)).
		WithObjects(curSessionFixture()).
		WithStatusSubresource(&spiceboxv1alpha1.CredentialUpdateRequest{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if cr, ok := obj.(*spiceboxv1alpha1.CredentialUpdateRequest); ok {
					sentName, sentGenerateName = cr.Name, cr.GenerateName
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()
	origin := stubOriginTool{name: "github_create_issue", origin: "mcpserver/github"}
	tl := meta.NewCredentialUpdate(meta.CredentialUpdateConfig{
		Client: c, ToolLookup: lookupFor(origin), PollInterval: 5 * time.Millisecond, MaxWait: 20 * time.Millisecond,
	})
	args, err := json.Marshal(map[string]any{"tool": "github_create_issue", "why": "the token stopped working"})
	require.NoError(t, err, "marshal args")

	// Nothing drives the CR to a terminal phase, so Execute times out polling.
	// Expected; this test is about how the object was named.
	_, err = tl.Execute(context.Background(), args, curSessCtx())
	require.NoError(t, err, "Execute must not return a Go error")

	assert.Empty(t, sentName, "the tool must leave the name to the apiserver rather than picking one")
	assert.Equal(t, "cur-"+curSessName+"-", sentGenerateName, "generateName must carry the session-scoped prefix")

	var list spiceboxv1alpha1.CredentialUpdateRequestList
	require.NoError(t, c.List(context.Background(), &list, client.InNamespace(curNS)))
	require.Len(t, list.Items, 1, "exactly one CredentialUpdateRequest must be created")
	assert.NotEmpty(t, list.Items[0].Name,
		"the stored object must still carry a name — the tool polls by it")
}
