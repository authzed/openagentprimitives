package triggerconcluded_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/completion/kinds/triggerconcluded"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

const (
	tcNamespace = "default"
	tcSession   = "review-1"
	tcChannel   = "demo-reviewbot-gh"
	// tcReportingKind is a kind that DOES report trigger status; `fake`, which
	// registers itself on import, is the control that does not.
	tcReportingKind = "faketrigger-completion"
)

// reportingKind is a registered channel kind that owns a trigger status
// surface. It embeds the fake kind so only the two methods under test are
// spelled out here, and it never builds a live surface: this requirement must
// answer from the session's own record, never by calling a provider, so a
// TriggerSurface that panics would be as valid a fixture as this one.
type reportingKind struct{ fake.Kind }

func (*reportingKind) Name() string { return tcReportingKind }

// Compile-time, and NOT decoration: the registry resolves a reporter by TYPE
// ASSERTION, so a fake that stops satisfying this interface still builds and
// silently stops being one. See the same guard in pkg/agent/tool/meta.
var _ channelkinds.TriggerStatusReporter = (*reportingKind)(nil)

func (*reportingKind) TriggerSurfaceKind() string { return "a pull request's fixture check run" }

// TriggerProviderStateIn: this fixture kind writes no provider identifiers into
// its own text, so there is nothing to read back. The zero value is the complete
// answer here, not a stub — the method exists because the seam is what a capture
// asks to seed a stand-in with, and a kind that mints nothing seeds nothing.
func (*reportingKind) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}
func (*reportingKind) TriggerSurface(
	*spiceboxv1alpha1.Channel, *spiceboxv1alpha1.ChannelBinding,
	channelkinds.WebhookSecrets, channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	return nil, nil
}

func init() { chregistry.Register(&reportingKind{}) }

// sessionWith builds a runner SessionContext over a fake cluster holding an
// AgentSession whose input binding names kindName ("" ⇒ no input binding at
// all, the kubectl-driven shape). concluded records an answer on the session's
// trigger-status store first.
func sessionWith(t *testing.T, kindName string, concluded channelkinds.TriggerOutcome) *tool.SessionContext {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme), "AddToScheme")

	cr := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: tcSession, Namespace: tcNamespace},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: "demo-reviewbot"},
	}
	if kindName != "" {
		cr.Spec.InputChannel = &spiceboxv1alpha1.ChannelBinding{
			Name: tcChannel, Kind: kindName, Key: "pr:demo-org/platform#42",
			NATSSubjectPrefix: "ap.session." + tcNamespace + "." + tcSession,
		}
	}
	c := fakeclient.NewClientBuilder().WithScheme(scheme).
		WithObjects([]client.Object{cr}...).Build()

	sess := &tool.SessionContext{
		Namespace: tcNamespace, Name: tcSession,
		K8sClient: c,
		State:     state.NewRegistry(state.Deps{}),
	}
	if concluded != "" {
		store, ok := triggerstatus.TryFrom(sess)
		require.True(t, ok, "importing the triggerstatus package registers its state kind")
		require.NoError(t, store.RecordConcluded(context.Background(), concluded))
	}
	return sess
}

// evaluate drives the requirement THROUGH the registry, never by calling Check
// directly: the key an AgentClass declares has to resolve to this kind through
// completion.Get, the same lookup every other kind uses.
func evaluate(t *testing.T, sess *tool.SessionContext, keys ...string) ([]completion.Unmet, error) {
	t.Helper()
	return completion.Evaluate(context.Background(), keys, completion.Input{Session: sess})
}

// TestTriggerConcluded_UnansweredTriggerRefusesCompletion is the observed
// defect: the review was delivered, the check run was never concluded, and the
// pull request sat showing the work as still in progress.
func TestTriggerConcluded_UnansweredTriggerRefusesCompletion(t *testing.T) {
	unmet, err := evaluate(t, sessionWith(t, tcReportingKind, ""), triggerconcluded.Key)
	require.NoError(t, err)
	require.Len(t, unmet, 1, "an unanswered trigger is the whole defect")
	assert.Equal(t, triggerconcluded.Key, unmet[0].Key)

	// The message has to be actionable by a model that only sees this string:
	// which call answers the trigger, and what it needs.
	assert.Contains(t, unmet[0].Missing, "conclude_trigger_status", "must name the call that answers")
	assert.Contains(t, unmet[0].Missing, "outcome", "must name the judgement it has to supply")
	for _, o := range channelkinds.TriggerOutcomes() {
		assert.Contains(t, unmet[0].Missing, string(o),
			"the legal outcomes are derived from the seam's own closed set, never retyped")
	}
}

func TestTriggerConcluded_AnsweredTriggerSatisfies(t *testing.T) {
	unmet, err := evaluate(t,
		sessionWith(t, tcReportingKind, channelkinds.TriggerOutcomeProblemsFound),
		triggerconcluded.Key)
	require.NoError(t, err)
	assert.Empty(t, unmet, "a trigger that carries an answer must not block completion")
}

// TestTriggerConcluded_InertWhereThereIsNothingToAnswer: registration makes a
// requirement AVAILABLE, and a declared one must still stay silent for every
// session whose trigger has no status surface. Getting this wrong wedges every
// slack-only agent whose class happens to declare the key.
func TestTriggerConcluded_InertWhereThereIsNothingToAnswer(t *testing.T) {
	cases := []struct {
		name string
		kind string
	}{
		{
			// Every conversational kind lands here, and the trigger-status
			// tools were never offered to these sessions either.
			name: "input kind with no status surface: nothing to answer",
			kind: fake.Kind{}.Name(),
		},
		{
			name: "kubectl-driven session: no channel started it at all",
			kind: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			unmet, err := evaluate(t, sessionWith(t, tc.kind, ""), triggerconcluded.Key)
			require.NoError(t, err)
			assert.Empty(t, unmet)
		})
	}
}

// TestTriggerConcluded_UndeclaredClassIsUnaffected: only the keys an AgentClass
// declared are consulted, so registering this kind must not tighten a class
// that never opted in.
func TestTriggerConcluded_UndeclaredClassIsUnaffected(t *testing.T) {
	unmet, err := evaluate(t, sessionWith(t, tcReportingKind, ""))
	require.NoError(t, err)
	assert.Empty(t, unmet, "a class declaring no requirements has nothing checked")
}

// TestTriggerConcluded_FailsClosedWhenItCannotAnswer: a requirement that cannot
// be EVALUATED must refuse rather than report a satisfaction it did not
// establish. Evaluate turns each into a refusal the agent can still bypass.
func TestTriggerConcluded_FailsClosedWhenItCannotAnswer(t *testing.T) {
	cases := []struct {
		name string
		sess func(t *testing.T) *tool.SessionContext
	}{
		{
			name: "no Kubernetes client: the input binding cannot be read",
			sess: func(t *testing.T) *tool.SessionContext {
				t.Helper()
				return &tool.SessionContext{
					Namespace: tcNamespace, Name: tcSession,
					State: state.NewRegistry(state.Deps{}),
				}
			},
		},
		{
			name: "AgentSession gone: which trigger this session answers is unknowable",
			sess: func(t *testing.T) *tool.SessionContext {
				t.Helper()
				scheme := runtime.NewScheme()
				require.NoError(t, spiceboxv1alpha1.AddToScheme(scheme))
				return &tool.SessionContext{
					Namespace: tcNamespace, Name: tcSession,
					K8sClient: fakeclient.NewClientBuilder().WithScheme(scheme).Build(),
					State:     state.NewRegistry(state.Deps{}),
				}
			},
		},
		{
			name: "no trigger-status state: whether it was answered is unknowable",
			sess: func(t *testing.T) *tool.SessionContext {
				t.Helper()
				s := sessionWith(t, tcReportingKind, "")
				s.State = nil
				return s
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evaluate(t, tc.sess(t), triggerconcluded.Key)
			require.Error(t, err)
		})
	}
}
