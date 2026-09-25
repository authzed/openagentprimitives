package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/plansteps"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// artifact-delivered resolves the session's binding kind through the
	// channel-kind registry, so the kind this fixture names has to be in it.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

const gateSessUID = types.UID("uid-gate-1")

// gateSession builds a channel-attached-shaped SessionContext carrying both
// observation sources the two registered requirement kinds read: a client
// holding the session's ArtifactRenders, and the state registry backing the
// deliveries and plans stores.
type gateSession struct {
	sess      *tool.SessionContext
	submitted *tool.AgentResult
}

func newGateSession(t *testing.T, renders ...*spiceboxv1alpha1.ArtifactRender) *gateSession {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	// The session CR is seeded with a HUMAN-facing input binding on purpose:
	// artifact-delivered is answered by a respond_to_user call, so it holds
	// only a session that was offered one, and every assertion below is about
	// a session that WAS.
	b := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "gate-1", Namespace: "default", UID: gateSessUID},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			InputChannel: &spiceboxv1alpha1.ChannelBinding{Name: "demo-channel", Kind: "fake"},
		},
	})
	for _, r := range renders {
		b = b.WithObjects(r)
	}

	g := &gateSession{}
	g.sess = &tool.SessionContext{
		Namespace: "default", Name: "gate-1", AgentSessionUID: gateSessUID,
		K8sClient:    b.Build(),
		State:        state.NewRegistry(state.Deps{}),
		SubmitResult: func(r tool.AgentResult) { g.submitted = &r },
	}
	return g
}

func readyRender(name string) *spiceboxv1alpha1.ArtifactRender {
	return &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: "gate-1", UID: gateSessUID,
			}},
		},
		Spec: spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: spiceboxv1alpha1.ArtifactRenderPhaseReady, OutputMIME: "text/html",
		},
	}
}

// bothKinds is the pair every seam assertion below declares. Naming both is the
// point: a break in the shared completion.Get lookup has to redden every kind,
// not whichever one a single-kind test happened to pick.
var bothKinds = []string{artifactdelivery.Key, plansteps.Key}

// TestCompletionGate_BothRegisteredKindsRefuseThroughOneLookup is the seam
// test. One AgentClass declares both keys; each resolves through
// completion.Get inside Evaluate and contributes its own line to one refusal.
// If the shared lookup stops resolving, BOTH halves of this fail — which is the
// evidence that the checker dispatches rather than branches.
func TestCompletionGate_BothRegisteredKindsRefuseThroughOneLookup(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))
	store, ok := plans.TryFrom(g.sess)
	require.True(t, ok)
	_, err := store.Update(context.Background(), "main", plans.ParentRef{}, plans.Content{
		Items: []plans.Item{{ID: "s6", Label: "conclude the check run", Status: plans.StatusPending}},
	})
	require.NoError(t, err)

	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: bothKinds,
		RecordBypass: func(context.Context, completion.Bypass) error { return nil },
	})

	res, execErr := tl.Execute(context.Background(), json.RawMessage(`{"summary":"done"}`), g.sess)
	require.NoError(t, execErr, "a refusal is a tool result the model can act on, not a Go error")
	assert.True(t, res.IsError, "an unmet requirement must refuse")
	assert.False(t, res.Terminal, "a refused completion must not end the session")
	assert.Nil(t, g.submitted, "a refused completion must not submit a result")

	assert.Contains(t, res.Content, artifactdelivery.Key, "the artifact requirement resolved and reported")
	assert.Contains(t, res.Content, "ar-gate-1-report", "and named what it found missing")
	assert.Contains(t, res.Content, plansteps.Key, "the plan requirement resolved and reported")
	assert.Contains(t, res.Content, "conclude the check run", "and named what it found missing")
	assert.Contains(t, res.Content, "bypass_reason", "the refusal must name the escape it offers")
}

func TestCompletionGate_SatisfiedRequirementsComplete(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))
	dl, ok := deliveries.TryFrom(g.sess)
	require.True(t, ok)
	require.NoError(t, dl.Record(context.Background(), deliveries.Item{RenderName: "ar-gate-1-report"}))

	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: bothKinds,
		RecordBypass: func(context.Context, completion.Bypass) error { return nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"summary":"done"}`), g.sess)
	require.NoError(t, err)
	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal, "satisfying every requirement must let completion through")
	require.NotNil(t, g.submitted)
	assert.Equal(t, "done", g.submitted.Summary)
}

func TestCompletionGate_UndeclaredRequirementNeverFires(t *testing.T) {
	// The same session that fails the seam test above: a Ready render nobody
	// attached. A class that declared nothing must still complete — registering
	// a requirement makes it available, never on.
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{})
	res, err := tl.Execute(context.Background(), json.RawMessage(`{"summary":"done"}`), g.sess)
	require.NoError(t, err)
	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal, "no class opts into a requirement by accident")
	assert.NotNil(t, g.submitted)
}

func TestCompletionGate_BypassWithReasonSucceedsAndIsRecorded(t *testing.T) {
	g := newGateSession(t, readyRender("ar-gate-1-report"))

	var got *completion.Bypass
	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(_ context.Context, b completion.Bypass) error { got = &b; return nil },
	})

	args := json.RawMessage(`{"summary":"done","bypass_reason":"the render failed to attach; a degraded review is still a review"}`)
	res, err := tl.Execute(context.Background(), args, g.sess)
	require.NoError(t, err)
	assert.False(t, res.IsError, "content: %s", res.Content)
	assert.True(t, res.Terminal, "a reasoned bypass must let the round finish")
	assert.NotNil(t, g.submitted)

	require.NotNil(t, got, "the bypass must be recorded, not merely allowed")
	assert.Equal(t, "the render failed to attach; a degraded review is still a review", got.Reason)
	require.Len(t, got.Unmet, 1, "the record must carry WHAT was skipped, not only that something was")
	assert.Equal(t, artifactdelivery.Key, got.Unmet[0].Key)
	assert.NotEmpty(t, got.Unmet[0].Title, "the human-facing record needs a title a reader understands")
}

func TestCompletionGate_EmptyBypassReasonIsRefused(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{name: `empty string: refused, nothing recorded`, args: `{"summary":"done","bypass_reason":""}`},
		{name: `whitespace only: refused, nothing recorded`, args: `{"summary":"done","bypass_reason":"   "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateSession(t, readyRender("ar-gate-1-report"))
			recorded := 0
			tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
				Requirements: []string{artifactdelivery.Key},
				RecordBypass: func(context.Context, completion.Bypass) error { recorded++; return nil },
			})

			res, err := tl.Execute(context.Background(), json.RawMessage(tc.args), g.sess)
			require.NoError(t, err)
			assert.True(t, res.IsError, "a bypass that states nothing is an off switch")
			assert.False(t, res.Terminal)
			assert.Nil(t, g.submitted)
			assert.Zero(t, recorded, "nothing may be recorded for a bypass that was refused")
			assert.Contains(t, res.Content, "bypass_reason", "the refusal must say which field was empty")
		})
	}
}

func TestCompletionGate_UnrecordableBypassIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		record func(context.Context, completion.Bypass) error
		want   string
	}{
		{
			name:   "no recorder wired: refused, because an unseen override is not an override",
			record: nil,
			want:   "cannot record",
		},
		{
			name:   "recorder fails: refused, because a bypass that was not recorded did not happen",
			record: func(context.Context, completion.Bypass) error { return errors.New("nats down") },
			want:   "nats down",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGateSession(t, readyRender("ar-gate-1-report"))
			tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
				Requirements: []string{artifactdelivery.Key},
				RecordBypass: tc.record,
			})

			args := json.RawMessage(`{"summary":"done","bypass_reason":"finishing anyway"}`)
			res, err := tl.Execute(context.Background(), args, g.sess)
			require.NoError(t, err)
			assert.True(t, res.IsError)
			assert.False(t, res.Terminal)
			assert.Nil(t, g.submitted)
			assert.Contains(t, res.Content, tc.want)
		})
	}
}

func TestCompletionGate_UnknownRequirementRefusesAndStaysBypassable(t *testing.T) {
	g := newGateSession(t)

	var got *completion.Bypass
	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: []string{"no-such-requirement"},
		RecordBypass: func(_ context.Context, b completion.Bypass) error { got = &b; return nil },
	})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"summary":"done"}`), g.sess)
	require.NoError(t, err)
	assert.True(t, res.IsError, "a guarantee this build cannot check must not be assumed satisfied")
	assert.Contains(t, res.Content, "no-such-requirement")
	assert.Contains(t, res.Content, "bypass_reason",
		"a misconfigured class must degrade the round, not wedge the session")

	// And the offered escape actually works, so the wedge really is avoidable.
	args := json.RawMessage(`{"summary":"done","bypass_reason":"the requirement is misconfigured; work is otherwise finished"}`)
	res, err = tl.Execute(context.Background(), args, g.sess)
	require.NoError(t, err)
	assert.True(t, res.Terminal, "content: %s", res.Content)
	assert.NotNil(t, got, "even a configuration-fault bypass is reported to the user")
}

func TestCompletionGate_BypassReasonWithNothingUnmetRecordsNothing(t *testing.T) {
	// A model repeating its previous call after fixing the gap still carries
	// the reason. That must complete, and must NOT tell the user an override
	// happened when none did.
	g := newGateSession(t)
	recorded := 0
	tl := meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
		RecordBypass: func(context.Context, completion.Bypass) error { recorded++; return nil },
	})

	args := json.RawMessage(`{"summary":"done","bypass_reason":"leftover from my previous attempt"}`)
	res, err := tl.Execute(context.Background(), args, g.sess)
	require.NoError(t, err)
	assert.True(t, res.Terminal, "content: %s", res.Content)
	assert.Zero(t, recorded, "there was nothing to bypass, so no override may be reported")
}

// TestCompletionGate_BypassReasonOnlyOfferedWhenSomethingCanBeBypassed guards
// the schema: a field the model can see is a field it will eventually try, and
// an escape from a gate the class never declared is an invitation to nothing.
func TestCompletionGate_BypassReasonOnlyOfferedWhenSomethingCanBeBypassed(t *testing.T) {
	props := func(t *testing.T, tl tool.Tool) map[string]any {
		t.Helper()
		var schema struct {
			Properties map[string]any `json:"properties"`
		}
		require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema), "schema must be valid JSON")
		return schema.Properties
	}

	ungated := props(t, meta.NewAgentWorkComplete(meta.CompletionConfig{}))
	assert.NotContains(t, ungated, "bypass_reason", "a class with no requirements is offered no bypass")
	assert.Contains(t, ungated, "summary", "the ungated schema is otherwise unchanged")

	gated := props(t, meta.NewAgentWorkComplete(meta.CompletionConfig{
		Requirements: []string{artifactdelivery.Key},
	}))
	assert.Contains(t, gated, "bypass_reason", "a class with requirements must be told how to override one")
}
