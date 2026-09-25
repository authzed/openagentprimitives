package artifactdelivery_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/artifactdelivery"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/deliveries"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	// Both kinds register themselves on import; the requirement resolves them
	// through the channel-kind registry, so nothing here is asked by name.
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/agent"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
)

const (
	sessNS  = "default"
	sessNm  = "review-1"
	sessUID = types.UID("uid-review-1")

	// humanFacingKind and sessionFacingKind are the two answers
	// AllowsSessionCounterparty gives, named by the shipped kinds that give
	// them. Both register on import, and both are asked THROUGH the registry —
	// this requirement must never compare a kind name itself.
	humanFacingKind   = "fake"
	sessionFacingKind = "agent"
)

// render builds one ArtifactRender in the given phase, owned by ownerUID.
func render(t *testing.T, name string, phase spiceboxv1alpha1.ArtifactRenderPhase, ownerUID types.UID) *spiceboxv1alpha1.ArtifactRender {
	t.Helper()
	cr := &spiceboxv1alpha1.ArtifactRender{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: sessNS},
		Spec:       spiceboxv1alpha1.ArtifactRenderSpec{Kind: "html"},
		Status: spiceboxv1alpha1.ArtifactRenderStatus{
			Phase: phase, OutputMIME: "text/html", OutputFilename: name + ".html",
		},
	}
	if ownerUID != "" {
		cr.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
			Kind:       "AgentSession", Name: sessNm, UID: ownerUID,
		}}
	}
	return cr
}

// agentSession builds the session CR the requirement reads to learn whether
// this session has a user-facing delivery path. kindName is its input
// binding's channel kind; "" means no binding at all, the kubectl-driven
// shape.
func agentSession(kindName string) *spiceboxv1alpha1.AgentSession {
	cr := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessNm, Namespace: sessNS, UID: sessUID},
	}
	if kindName != "" {
		cr.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{Name: "demo-channel", Kind: kindName}
	}
	return cr
}

// sessionWith builds a SessionContext over a fake client holding objs, with a
// live deliveries store whose delivered set is seeded from delivered.
//
// The session CR it seeds is bound to a HUMAN-facing channel kind, because
// that is the shape this requirement is meant to hold: a session that was
// offered respond_to_user must still be refused for an artifact it never
// attached. sessionOnKind covers the other shapes.
func sessionWith(t *testing.T, delivered []string, objs ...client.Object) *tool.SessionContext {
	t.Helper()
	return sessionOnKind(t, humanFacingKind, delivered, objs...)
}

// sessionOnKind is sessionWith over an explicit input-binding channel kind.
func sessionOnKind(t *testing.T, kindName string, delivered []string, objs ...client.Object) *tool.SessionContext {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(append([]client.Object{agentSession(kindName)}, objs...)...).Build()

	sess := &tool.SessionContext{
		Namespace: sessNS, Name: sessNm, AgentSessionUID: sessUID,
		K8sClient: c,
		State:     state.NewRegistry(state.Deps{}),
	}
	if len(delivered) > 0 {
		store, ok := deliveries.TryFrom(sess)
		require.True(t, ok, "importing the deliveries package registers its state kind")
		// This requirement reads render names only, so the fixture leaves the
		// artifact id empty — it must stay satisfiable for a render whose CR
		// carries no artifact-id label.
		items := make([]deliveries.Item, 0, len(delivered))
		for _, n := range delivered {
			items = append(items, deliveries.Item{RenderName: n})
		}
		require.NoError(t, store.Record(context.Background(), items...))
	}
	return sess
}

// evaluate drives the requirement THROUGH the registry, never by calling Check
// directly. That is the seam under test as much as the verdict is: the key an
// AgentClass declares has to resolve to this kind through completion.Get, the
// same lookup every other kind uses.
func evaluate(t *testing.T, sess *tool.SessionContext) ([]completion.Unmet, error) {
	t.Helper()
	return completion.Evaluate(context.Background(),
		[]string{artifactdelivery.Key}, completion.Input{Session: sess})
}

func TestArtifactDelivered_ReadyButNeverAttachedRefuses(t *testing.T) {
	sess := sessionWith(t, nil, render(t, "ar-review-1-abc", spiceboxv1alpha1.ArtifactRenderPhaseReady, sessUID))

	unmet, err := evaluate(t, sess)
	require.NoError(t, err)
	require.Len(t, unmet, 1, "a Ready render nobody attached is the whole defect")
	assert.Equal(t, artifactdelivery.Key, unmet[0].Key)

	// The message has to be actionable by a model that only sees this string:
	// which artifact, and the exact call that delivers it.
	assert.Contains(t, unmet[0].Missing, "ar-review-1-abc", "must name the handle to attach")
	assert.Contains(t, unmet[0].Missing, "respond_to_user", "must name the call that delivers")
	assert.Contains(t, unmet[0].Missing, "attached", "must name the field that carries it")
}

func TestArtifactDelivered_DeliveredRenderSatisfies(t *testing.T) {
	sess := sessionWith(t,
		[]string{"ar-review-1-abc"},
		render(t, "ar-review-1-abc", spiceboxv1alpha1.ArtifactRenderPhaseReady, sessUID))

	unmet, err := evaluate(t, sess)
	require.NoError(t, err)
	assert.Empty(t, unmet, "an artifact respond_to_user attached is delivered and must not block completion")
}

func TestArtifactDelivered_IgnoresNotReadyAndOtherSessions(t *testing.T) {
	cases := []struct {
		name string
		cr   *spiceboxv1alpha1.ArtifactRender
	}{
		{
			// A render still working or failed is not an undelivered result,
			// and refusing on it would trap the agent on something it cannot fix.
			name: "pending render: not a delivery obligation",
			cr:   render(t, "ar-review-1-pending", spiceboxv1alpha1.ArtifactRenderPhasePending, sessUID),
		},
		{
			// Renders are namespace-scoped and namespaces host many sessions;
			// ownership is the only thing that says whose a render is.
			name: "another session's Ready render: not ours to deliver",
			cr:   render(t, "ar-other-xyz", spiceboxv1alpha1.ArtifactRenderPhaseReady, types.UID("uid-someone-else")),
		},
		{
			name: "unowned Ready render: not ours to deliver",
			cr:   render(t, "ar-orphan", spiceboxv1alpha1.ArtifactRenderPhaseReady, ""),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unmet, err := evaluate(t, sessionWith(t, nil, tc.cr))
			require.NoError(t, err)
			assert.Empty(t, unmet)
		})
	}
}

// TestArtifactDelivered_HeldOnlyWhereAUserCanBeDeliveredTo pins the predicate
// from both sides at once, which is the only way it stays honest. The
// requirement names ONE act — a respond_to_user call carrying the handle — so
// it must be enforced exactly where that call exists and waived exactly where
// it does not.
//
// The first row is the guard against this becoming a blanket exemption: a
// session that HAS a user-facing delivery path is still refused for a Ready
// render it never attached, which is the defect the requirement was written
// for. The rest are sessions the same runner never offers respond_to_user to,
// where refusing could only ever be answered by a bypass — an off switch
// dressed as a requirement.
func TestArtifactDelivered_HeldOnlyWhereAUserCanBeDeliveredTo(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		wantUnmet bool
	}{
		{
			name: "human-facing binding, artifact never attached: still refused",
			kind: humanFacingKind, wantUnmet: true,
		},
		{
			// A delegated child. respond_to_user is withheld from it because a
			// reply would land in the parent's transcript uninspected, so the
			// only call that answers this requirement is one it does not have.
			// Its artifacts reach the parent through return_result instead, and
			// the parent is the session with a user.
			name: "binding reaching another agent: waived, the answering call was never offered",
			kind: sessionFacingKind, wantUnmet: false,
		},
		{
			// No binding at all: the channel_interaction capability contributes
			// nothing, so there is no respond_to_user here either. This is a
			// session SHAPE, not a delegation — which is why the predicate keys
			// on the missing tool and not on being a child.
			name: "no channel binding at all: waived for the same reason",
			kind: "", wantUnmet: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := sessionOnKind(t, tc.kind, nil,
				render(t, "ar-review-1-abc", spiceboxv1alpha1.ArtifactRenderPhaseReady, sessUID))

			unmet, err := evaluate(t, sess)

			require.NoError(t, err)
			if tc.wantUnmet {
				require.Len(t, unmet, 1)
				assert.Contains(t, unmet[0].Missing, "respond_to_user",
					"a session that was offered the call must be told to make it")
				return
			}
			assert.Empty(t, unmet,
				"a session with no user-facing delivery path must not be asked to bypass on every finish")
		})
	}
}

// TestArtifactDelivered_UnregisteredKindFailsClosed keeps the waiver from
// widening into a failure mode. An unresolvable kind is a wiring bug, and
// reading "could not be established" as "was never offered a way to answer"
// would turn every such bug into a silent exemption.
func TestArtifactDelivered_UnregisteredKindFailsClosed(t *testing.T) {
	sess := sessionOnKind(t, "no-such-kind-registered", nil,
		render(t, "ar-review-1-abc", spiceboxv1alpha1.ArtifactRenderPhaseReady, sessUID))

	_, err := evaluate(t, sess)

	require.Error(t, err, "an unanswerable question must refuse, not exempt")
	assert.Contains(t, err.Error(), "no-such-kind-registered", "the refusal must name the kind it could not resolve")
}

func TestArtifactDelivered_NoClientFailsClosed(t *testing.T) {
	sess := &tool.SessionContext{
		Namespace: sessNS, Name: sessNm, AgentSessionUID: sessUID,
		State: state.NewRegistry(state.Deps{}),
	}
	_, err := evaluate(t, sess)
	require.Error(t, err,
		"a session that cannot list its renders must refuse, not report satisfaction it cannot establish")
}
