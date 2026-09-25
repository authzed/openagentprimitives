package meta_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/triggerstatus"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
)

// ---------------------------------------------------------------------
// A registered kind that reports trigger status, standing in for github.
//
// A fake rather than the real kind on purpose: these tests are about the
// TOOLS — that they dispatch through the registry, hand the surface exactly
// what the model supplied and nothing more, and report every failure — and a
// real provider client in the middle would let a github-side bug read as a
// tool-side pass, or the reverse.
// ---------------------------------------------------------------------

// recordedConclusion is what the fake surface was asked to publish.
type recordedConclusion struct {
	channelkinds.TriggerConclusion
	// Channel and binding as the tool resolved them, so a test can assert the
	// surface was addressed for the session's own trigger.
	ChannelName string
	BindingKey  string
}

type triggerFake struct {
	*stubKind
	name string

	// claim is what Claim reports; claimErr overrides it.
	claim    channelkinds.TriggerClaim
	claimErr error
	// concludeErr makes Conclude fail.
	concludeErr error
	// surfaceErr makes TriggerSurface itself refuse.
	surfaceErr error

	claims      int
	concluded   []recordedConclusion
	lastBaseURL string
}

func (k *triggerFake) Name() string { return k.name }

// Compile-time, and NOT decoration. registry.TriggerStatusReporterFor resolves
// a kind by TYPE ASSERTION, so a fake that stops satisfying this interface does
// not fail to build — it silently stops being a reporter, and every test using
// it fails with "channel kind %q reports no trigger status in this build", which
// names the kind rather than the missing method. That is exactly what happened
// when the interface gained TriggerProviderStateIn.
var _ channelkinds.TriggerStatusReporter = (*triggerFake)(nil)

func (k *triggerFake) TriggerSurfaceKind() string { return "a fixture trigger's status board" }

// TriggerProviderStateIn: this fixture kind writes no provider identifiers into
// its own text, so there is nothing to read back. The zero value is the complete
// answer here, not a stub — the method exists because the seam is what a capture
// asks to seed a stand-in with, and a kind that mints nothing seeds nothing.
func (k *triggerFake) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}

func (k *triggerFake) TriggerSurface(
	ch *spiceboxv1alpha1.Channel,
	b *spiceboxv1alpha1.ChannelBinding,
	secrets channelkinds.WebhookSecrets,
	opts channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	if k.surfaceErr != nil {
		return nil, k.surfaceErr
	}
	// Pinned here because it is the tool's job to hand the kind the Channel's
	// OWN credentials: a surface built from an empty Secret would fail much
	// later, against the provider, with a message nobody could trace back.
	if string(secrets.Data["fixture-key"]) != "present" {
		return nil, errors.New("fixture: the Channel's credentials Secret did not reach the kind")
	}
	k.lastBaseURL = opts.ProviderAPIBaseURL
	return &triggerFakeSurface{kind: k, channelName: ch.Name, bindingKey: b.Key}, nil
}

type triggerFakeSurface struct {
	kind        *triggerFake
	channelName string
	bindingKey  string
}

func (s *triggerFakeSurface) Surface() string { return "the fixture status board for " + s.bindingKey }

func (s *triggerFakeSurface) Claim(context.Context) (channelkinds.TriggerClaim, error) {
	s.kind.claims++
	if s.kind.claimErr != nil {
		return channelkinds.TriggerClaim{}, s.kind.claimErr
	}
	return s.kind.claim, nil
}

func (s *triggerFakeSurface) Conclude(_ context.Context, c channelkinds.TriggerConclusion) error {
	if s.kind.concludeErr != nil {
		return s.kind.concludeErr
	}
	s.kind.concluded = append(s.kind.concluded, recordedConclusion{
		TriggerConclusion: c, ChannelName: s.channelName, BindingKey: s.bindingKey,
	})
	return nil
}

// registerTriggerFake puts the fake kind in the process-wide registry for the
// rest of the run. The registry panics on duplicate registration and has no
// Unregister, so each caller must pass a distinct name.
func registerTriggerFake(t *testing.T, name string) *triggerFake {
	t.Helper()
	k := &triggerFake{stubKind: &stubKind{}, name: name}
	registry.Register(k)
	return k
}

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

const (
	tsNamespace = "default"
	tsChannel   = "reviewbot-trigger"
	tsSecret    = "reviewbot-trigger-creds"
	tsKey       = "pr:demo-org/platform#42"
)

func tsScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return s
}

// tsClient is a cluster holding the input Channel and its credentials Secret —
// the two objects the tools resolve at call time rather than capture at
// assembly, so a Secret rotated mid-session is the one that gets used.
func tsClient(t *testing.T, kindName string, objs ...client.Object) client.Client {
	t.Helper()
	all := []client.Object{
		&spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: tsChannel, Namespace: tsNamespace},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           kindName,
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: tsSecret},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: tsSecret, Namespace: tsNamespace},
			Data:       map[string][]byte{"fixture-key": []byte("present")},
		},
	}
	all = append(all, objs...)
	return fakeclient.NewClientBuilder().WithScheme(tsScheme(t)).WithObjects(all...).Build()
}

func tsConfig(kindName string) meta.TriggerStatusConfig {
	return meta.TriggerStatusConfig{
		KindName:    kindName,
		SurfaceKind: "a fixture trigger's status board",
		ChannelName: tsChannel,
		Binding: &spiceboxv1alpha1.ChannelBinding{
			Name: tsChannel, Kind: kindName, Key: tsKey,
		},
	}
}

func tsSession(c client.Client) *tool.SessionContext {
	return &tool.SessionContext{Namespace: tsNamespace, Name: "sess-1", K8sClient: c}
}

// ---------------------------------------------------------------------
// claim_trigger_status
// ---------------------------------------------------------------------

// TestClaimTriggerStatus_TakesNoArgumentsAtAll is the design claim, asserted
// against the schema the model actually reads: there is nothing here for a
// model to get wrong, because there is nothing here for it to supply.
func TestClaimTriggerStatus_TakesNoArgumentsAtAll(t *testing.T) {
	tl := meta.NewClaimTriggerStatus(tsConfig("faketrigger-schema"))
	require.NotNil(t, tl)

	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))
	assert.Empty(t, schema.Properties, "claiming needs no argument; every identifier comes from the trigger")
	assert.Empty(t, schema.Required)
	assert.Contains(t, tl.Description(), "a fixture trigger's status board",
		"the description names what the KIND says it reports on, not a hardcoded surface")
}

// TestConcludeTriggerStatus_SchemaEnumIsTheSeamsClosedSet: the enum is what a
// model chooses from and ParseTriggerOutcome is what accepts the answer. A
// hand-typed enum drifts from the parser silently, and the failure lands as a
// refused call at the end of a completed review.
func TestConcludeTriggerStatus_SchemaEnumIsTheSeamsClosedSet(t *testing.T) {
	tl := meta.NewConcludeTriggerStatus(tsConfig("faketrigger-enum"))
	var schema struct {
		Properties struct {
			Outcome struct {
				Enum []string `json:"enum"`
			} `json:"outcome"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))

	want := make([]string, 0, len(channelkinds.TriggerOutcomes()))
	for _, o := range channelkinds.TriggerOutcomes() {
		want = append(want, string(o))
	}
	assert.Equal(t, want, schema.Properties.Outcome.Enum)

	// The model-text mode's description is a stable contract composed mode
	// must not perturb, and this phrase is the first thing a shared-prefix
	// refactor of Description would lose.
	assert.Contains(t, tl.Description(), "what you write here is what a person reads",
		"the default (model-text) description must keep its original wording byte-for-byte")
}

// TestClaimTriggerStatus_ReportsTheClaim: the happy path, and the proof that
// the tool reached the registered kind rather than doing anything itself.
func TestClaimTriggerStatus_ReportsTheClaim(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-claim")
	k.claim = channelkinds.TriggerClaim{Ref: "fixture status 4242"}

	res, err := meta.NewClaimTriggerStatus(tsConfig(k.name)).
		Execute(context.Background(), json.RawMessage(`{}`), tsSession(tsClient(t, k.name)))
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, 1, k.claims)
	assert.Contains(t, res.Content, "fixture status 4242")
}

// TestClaimTriggerStatus_ReportsAnAlreadyAnsweredTrigger. This is the fact an
// agent previously reconstructed with a hand-written list call, and it decides
// whether the round should do any work at all — so the result has to say it
// unmistakably, not merely include it.
func TestClaimTriggerStatus_ReportsAnAlreadyAnsweredTrigger(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-already")
	k.claim = channelkinds.TriggerClaim{
		Ref:       "fixture status 4242",
		Concluded: true,
		Outcome:   channelkinds.TriggerOutcomeClean,
	}

	res, err := meta.NewClaimTriggerStatus(tsConfig(k.name)).
		Execute(context.Background(), json.RawMessage(`{}`), tsSession(tsClient(t, k.name)))
	require.NoError(t, err)
	assert.False(t, res.IsError, "a repeat delivery is a normal state, not a failure")

	var out struct {
		AlreadyConcluded bool   `json:"already_concluded"`
		Outcome          string `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content), &out), "result: %s", res.Content)
	assert.True(t, out.AlreadyConcluded)
	assert.Equal(t, "clean", out.Outcome)
}

// ---------------------------------------------------------------------
// conclude_trigger_status
// ---------------------------------------------------------------------

// TestConcludeTriggerStatus_PassesOnlyJudgement is the seam's central property
// stated at the tool boundary: the conclusion that reaches the kind carries the
// agent's verdict and prose, and the trigger's identifiers come from the
// binding — a model could not have supplied them if it wanted to.
func TestConcludeTriggerStatus_PassesOnlyJudgement(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-conclude")

	res, err := meta.NewConcludeTriggerStatus(tsConfig(k.name)).Execute(context.Background(),
		json.RawMessage(`{"outcome":"problems_found","summary":"Two findings in the auth path.","details_url":"https://example.test/t/1"}`),
		tsSession(tsClient(t, k.name)))
	require.NoError(t, err)
	assert.False(t, res.IsError, "result: %s", res.Content)

	require.Len(t, k.concluded, 1)
	got := k.concluded[0]
	assert.Equal(t, channelkinds.TriggerOutcomeProblemsFound, got.Outcome)
	assert.Equal(t, "Two findings in the auth path.", got.Summary)
	assert.Equal(t, "https://example.test/t/1", got.DetailsURL)
	assert.Equal(t, tsChannel, got.ChannelName, "the surface was built for the session's own input Channel")
	assert.Equal(t, tsKey, got.BindingKey, "the trigger is the binding's, never an argument's")
}

// TestConcludeTriggerStatus_RefusesAnOutcomeOutsideTheClosedSet. The outcome
// is the one thing the model does supply, so it is validated here rather than
// left for a provider to reject after the session has moved on.
func TestConcludeTriggerStatus_RefusesAnOutcomeOutsideTheClosedSet(t *testing.T) {
	cases := []struct {
		name string
		args string
	}{
		{name: "a provider's own vocabulary", args: `{"outcome":"action_required","summary":"x"}`},
		{name: "an omitted outcome", args: `{"summary":"x"}`},
		{name: "an empty summary", args: `{"outcome":"clean","summary":"  "}`},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := registerTriggerFake(t, "faketrigger-badargs-"+string(rune('a'+i)))
			res, err := meta.NewConcludeTriggerStatus(tsConfig(k.name)).
				Execute(context.Background(), json.RawMessage(tc.args), tsSession(tsClient(t, k.name)))
			require.NoError(t, err, "a refused argument is a tool_result the model can correct, not a runner error")
			assert.True(t, res.IsError)
			assert.Empty(t, k.concluded, "nothing may reach the provider on a refused call")
		})
	}
}

// TestTriggerStatusTools_ReportEveryFailurePath: each of these once had a
// plausible silent form — a nil client, a Channel that has been deleted, a kind
// that refuses to build a surface, a provider call that fails. None of them may
// end a round quietly, because the whole point of this surface is that somebody
// is waiting on it.
func TestTriggerStatusTools_ReportEveryFailurePath(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext)
		wantErr string
	}{
		{
			name: "no kubernetes client on the session",
			setup: func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext) {
				return tsConfig(k.name), &tool.SessionContext{Namespace: tsNamespace, Name: "sess-1"}
			},
			wantErr: "Kubernetes client",
		},
		{
			name: "the input Channel is gone",
			setup: func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext) {
				c := fakeclient.NewClientBuilder().WithScheme(tsScheme(t)).Build()
				return tsConfig(k.name), tsSession(c)
			},
			wantErr: tsChannel,
		},
		{
			name: "the kind is not linked into this build",
			setup: func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext) {
				cfg := tsConfig(k.name)
				cfg.KindName = "nosuchkind"
				return cfg, tsSession(tsClient(t, k.name))
			},
			wantErr: "nosuchkind",
		},
		{
			name: "the kind refuses to build a surface",
			setup: func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext) {
				k.surfaceErr = errors.New("credentials Secret has no installation-id")
				return tsConfig(k.name), tsSession(tsClient(t, k.name))
			},
			wantErr: "installation-id",
		},
		{
			name: "the provider call fails",
			setup: func(t *testing.T, k *triggerFake) (meta.TriggerStatusConfig, *tool.SessionContext) {
				k.claimErr = errors.New("status 503")
				k.concludeErr = errors.New("status 503")
				return tsConfig(k.name), tsSession(tsClient(t, k.name))
			},
			wantErr: "503",
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := registerTriggerFake(t, "faketrigger-fail-"+string(rune('a'+i)))
			cfg, sess := tc.setup(t, k)

			claimRes, err := meta.NewClaimTriggerStatus(cfg).Execute(context.Background(), json.RawMessage(`{}`), sess)
			require.NoError(t, err)
			assert.True(t, claimRes.IsError, "claim result: %s", claimRes.Content)
			assert.Contains(t, claimRes.Content, tc.wantErr)

			concludeRes, err := meta.NewConcludeTriggerStatus(cfg).Execute(context.Background(),
				json.RawMessage(`{"outcome":"clean","summary":"nothing blocking"}`), sess)
			require.NoError(t, err)
			assert.True(t, concludeRes.IsError, "conclude result: %s", concludeRes.Content)
			assert.Contains(t, concludeRes.Content, tc.wantErr)
		})
	}
}

// TestTriggerStatusTools_DispatchThroughTheSharedLookup: both tools reach their
// kind by resolving the binding's kind NAME through the channel-kind registry.
// A kind registered under a name nothing else knows could not be reached any
// other way, so a green result here is proof the dispatch happened rather than
// a hardcoded path being taken.
func TestTriggerStatusTools_DispatchThroughTheSharedLookup(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-dispatch")
	c := tsClient(t, k.name)
	cfg := tsConfig(k.name)
	cfg.ProviderAPIBaseURL = "https://stand-in.test"

	_, err := meta.NewClaimTriggerStatus(cfg).Execute(context.Background(), json.RawMessage(`{}`), tsSession(c))
	require.NoError(t, err)
	_, err = meta.NewConcludeTriggerStatus(cfg).Execute(context.Background(),
		json.RawMessage(`{"outcome":"clean","summary":"nothing blocking"}`), tsSession(c))
	require.NoError(t, err)

	assert.Equal(t, 1, k.claims)
	require.Len(t, k.concluded, 1)
	assert.Equal(t, "https://stand-in.test", k.lastBaseURL,
		"the provider override must reach the kind, or nothing below the seam is testable")
}

// ---------------------------------------------------------------------
// what the completion gate reads
// ---------------------------------------------------------------------

// tsStatefulSession is tsSession plus the per-session state registry a real
// runner builds, so the trigger-status store these tools write is present.
func tsStatefulSession(c client.Client) *tool.SessionContext {
	s := tsSession(c)
	s.State = state.NewRegistry(state.Deps{})
	return s
}

// concludedOutcome reads back what the session recorded about its trigger.
func concludedOutcome(t *testing.T, sess *tool.SessionContext) (channelkinds.TriggerOutcome, bool) {
	t.Helper()
	store, ok := triggerstatus.TryFrom(sess)
	require.True(t, ok, "the runner's state registry carries the trigger-status store")
	return store.Concluded()
}

// TestTriggerStatusTools_RecordTheAnswerTheCompletionGateReads. The gate cannot
// ask the provider: TriggerSurface has no read-only query — Claim OPENS a claim
// when none exists — so a completion check that asked would put a pull request
// into "in progress" as a side effect of checking. These two tools are
// therefore the only writers of the fact the gate reads, and both ways a
// trigger comes to carry an answer have to reach it.
func TestTriggerStatusTools_RecordTheAnswerTheCompletionGateReads(t *testing.T) {
	t.Run("concluding records the verdict that was published", func(t *testing.T) {
		k := registerTriggerFake(t, "faketrigger-record-conclude")
		sess := tsStatefulSession(tsClient(t, k.name))

		res, err := meta.NewConcludeTriggerStatus(tsConfig(k.name)).Execute(context.Background(),
			json.RawMessage(`{"outcome":"problems_found","summary":"Two findings in the auth path."}`), sess)
		require.NoError(t, err)
		require.False(t, res.IsError, "result: %s", res.Content)

		outcome, done := concludedOutcome(t, sess)
		assert.True(t, done)
		assert.Equal(t, channelkinds.TriggerOutcomeProblemsFound, outcome,
			"the PARSED outcome, matching what was published, not the raw argument")
	})

	t.Run("a claim on an already-answered trigger counts as answered", func(t *testing.T) {
		// The redelivery path. This tool tells the model to say nothing further
		// and end the round; without this the gate would then refuse to let it,
		// for an answer the pull request already carries.
		k := registerTriggerFake(t, "faketrigger-record-claim")
		k.claim = channelkinds.TriggerClaim{
			Ref: "run 7", Concluded: true, Outcome: channelkinds.TriggerOutcomeClean,
		}
		sess := tsStatefulSession(tsClient(t, k.name))

		res, err := meta.NewClaimTriggerStatus(tsConfig(k.name)).
			Execute(context.Background(), json.RawMessage(`{}`), sess)
		require.NoError(t, err)
		require.False(t, res.IsError, "result: %s", res.Content)

		outcome, done := concludedOutcome(t, sess)
		assert.True(t, done)
		assert.Equal(t, channelkinds.TriggerOutcomeClean, outcome)
	})

	t.Run("an open claim records nothing: the work is still owed", func(t *testing.T) {
		k := registerTriggerFake(t, "faketrigger-record-open")
		k.claim = channelkinds.TriggerClaim{Ref: "run 8"}
		sess := tsStatefulSession(tsClient(t, k.name))

		_, err := meta.NewClaimTriggerStatus(tsConfig(k.name)).
			Execute(context.Background(), json.RawMessage(`{}`), sess)
		require.NoError(t, err)

		_, done := concludedOutcome(t, sess)
		assert.False(t, done, "claiming is starting the work, not answering for it")
	})

	t.Run("a conclusion the provider refused records nothing", func(t *testing.T) {
		k := registerTriggerFake(t, "faketrigger-record-failed")
		k.concludeErr = errors.New("status 503")
		sess := tsStatefulSession(tsClient(t, k.name))

		res, err := meta.NewConcludeTriggerStatus(tsConfig(k.name)).Execute(context.Background(),
			json.RawMessage(`{"outcome":"clean","summary":"nothing blocking"}`), sess)
		require.NoError(t, err)
		require.True(t, res.IsError)

		_, done := concludedOutcome(t, sess)
		assert.False(t, done,
			"recording an answer that never landed would tell the gate a pull request was answered when it was not")
	})
}

// ---------------------------------------------------------------------
// composed mode
// ---------------------------------------------------------------------

// tsComposedConfig is tsConfig with composed-only publication switched on —
// the shape a class that publishes to a publicly readable surface opts into.
func tsComposedConfig(kindName string) meta.TriggerStatusConfig {
	cfg := tsConfig(kindName)
	cfg.ComposedTextOnly = true
	return cfg
}

// TestConcludeTriggerStatus_ComposedMode_OffersOnlyTheOutcome: in composed
// mode the model is never ASKED for text — the schema is the prompt, and a
// summary field left in it would invite the exact bytes this mode exists to
// keep off the surface.
func TestConcludeTriggerStatus_ComposedMode_OffersOnlyTheOutcome(t *testing.T) {
	tl := meta.NewConcludeTriggerStatus(tsComposedConfig("faketrigger-composed-schema"))
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema))
	assert.Equal(t, []string{"outcome"}, schema.Required)
	require.Len(t, schema.Properties, 1)
	assert.Contains(t, schema.Properties, "outcome")

	// The enum is still the seam's closed set, derived, never retyped.
	var outcome struct {
		Enum []string `json:"enum"`
	}
	require.NoError(t, json.Unmarshal(schema.Properties["outcome"], &outcome))
	want := make([]string, 0, len(channelkinds.TriggerOutcomes()))
	for _, o := range channelkinds.TriggerOutcomes() {
		want = append(want, string(o))
	}
	assert.Equal(t, want, outcome.Enum)

	assert.Contains(t, tl.Description(), "composed",
		"the description must tell the model its text does not reach the surface")
}

// TestConcludeTriggerStatus_ComposedMode_PublishesNoModelAuthoredByte is the
// design's central property at the tool boundary: a chatty or prompt-injected
// model supplies a summary and its own link anyway, and NONE of it reaches
// the surface — the summary is composed here, and the details link is the
// framework's own (empty in this fixture, which delivered nothing linkable).
func TestConcludeTriggerStatus_ComposedMode_PublishesNoModelAuthoredByte(t *testing.T) {
	k := registerTriggerFake(t, "faketrigger-composed-noleak")

	res, err := meta.NewConcludeTriggerStatus(tsComposedConfig(k.name)).Execute(context.Background(),
		json.RawMessage(`{"outcome":"problems_found","summary":"SQLi in login.go line 40","details_url":"https://attacker.example/x?leak=finding"}`),
		tsSession(tsClient(t, k.name)))
	require.NoError(t, err)
	assert.False(t, res.IsError, "result: %s", res.Content)

	require.Len(t, k.concluded, 1)
	got := k.concluded[0]
	assert.Equal(t, channelkinds.TriggerOutcomeProblemsFound, got.Outcome)
	assert.NotEmpty(t, got.Summary, "the kind refuses an empty summary; composed mode must satisfy it")
	assert.NotContains(t, got.Summary, "SQLi", "no model-authored byte may reach the surface")
	assert.NotContains(t, got.Summary, "login.go")
	assert.Empty(t, got.DetailsURL,
		"nothing was delivered, so there is nothing to link — and the model's URL must never be it")
}

// TestConcludeTriggerStatus_ComposedMode_ComposesASummaryForEveryOutcome
// walks the seam's own closed set: every outcome must publish, each with its
// own text — a shared text would tell a maintainer nothing the conclusion
// icon does not.
func TestConcludeTriggerStatus_ComposedMode_ComposesASummaryForEveryOutcome(t *testing.T) {
	seen := map[string]bool{}
	for i, o := range channelkinds.TriggerOutcomes() {
		t.Run(string(o)+": publishes a distinct composed summary", func(t *testing.T) {
			k := registerTriggerFake(t, fmt.Sprintf("faketrigger-composed-each-%d", i))
			res, err := meta.NewConcludeTriggerStatus(tsComposedConfig(k.name)).Execute(context.Background(),
				json.RawMessage(`{"outcome":"`+string(o)+`"}`), tsSession(tsClient(t, k.name)))
			require.NoError(t, err)
			assert.False(t, res.IsError, "result: %s", res.Content)
			require.Len(t, k.concluded, 1)
			s := k.concluded[0].Summary
			assert.NotEmpty(t, s)
			assert.False(t, seen[s], "outcome %s reuses another outcome's composed text", o)
			seen[s] = true
		})
	}
}
