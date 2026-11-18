package triggeranswer_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	chregistry "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/triggeranswer"
)

const (
	taNS          = "default"
	taChannelName = "demo-reviewbot-gh"
	taSecretName  = "demo-reviewbot-gh-creds"
	taBindingKey  = "pr:demo-org/platform#42"
	// taKindName owns a trigger status surface; the registered `fake` kind,
	// which does not, is the control.
	taKindName = "faketrigger-answer"
)

// answerKind is a registered channel kind with a status surface, standing in
// for github, recording what was done to its surface.
type answerKind struct {
	fakekind.Kind

	mu          sync.Mutex
	standing    *channelkinds.TriggerClaim
	claimErr    error
	concludeErr error
	noSurface   bool
	claims      int
	published   []channelkinds.TriggerConclusion
}

var _ channelkinds.TriggerStatusReporter = (*answerKind)(nil)

func (*answerKind) Name() string               { return taKindName }
func (*answerKind) TriggerSurfaceKind() string { return "a pull request's fixture check run" }
func (*answerKind) TriggerProviderStateIn(string) channelkinds.TriggerProviderState {
	return channelkinds.TriggerProviderState{}
}

func (k *answerKind) TriggerSurface(
	ch *spiceboxv1alpha1.Channel, b *spiceboxv1alpha1.ChannelBinding,
	secrets channelkinds.WebhookSecrets, opts channelkinds.TriggerStatusOptions,
) (channelkinds.TriggerSurface, error) {
	if string(secrets.Data["fixture-key"]) != "present" {
		return nil, errors.New("fixture: the Channel's credentials Secret did not reach the kind")
	}
	if ch.Name != taChannelName || b.Key != taBindingKey {
		return nil, errors.New("fixture: addressed for something other than this session's own trigger")
	}
	if opts.ProviderAPIBaseURL != "https://provider.test" {
		return nil, errors.New("fixture: the provider override did not reach the kind")
	}
	if k.noSurface {
		return nil, nil // "this binding carries no trigger"
	}
	return &answerSurface{kind: k}, nil
}

type answerSurface struct{ kind *answerKind }

func (*answerSurface) Surface() string { return "the fixture check run on demo-org/platform#42" }

func (s *answerSurface) Claim(context.Context) (channelkinds.TriggerClaim, error) {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	s.kind.claims++
	if s.kind.claimErr != nil {
		return channelkinds.TriggerClaim{}, s.kind.claimErr
	}
	if s.kind.standing != nil {
		return *s.kind.standing, nil
	}
	return channelkinds.TriggerClaim{Ref: "fixture run 1"}, nil
}

func (s *answerSurface) Conclude(_ context.Context, c channelkinds.TriggerConclusion) error {
	s.kind.mu.Lock()
	defer s.kind.mu.Unlock()
	if s.kind.concludeErr != nil {
		return s.kind.concludeErr
	}
	s.kind.published = append(s.kind.published, c)
	return nil
}

// registered is the process-wide registration: the registry panics on a
// duplicate name and has no Unregister, so one instance is shared and reset
// per case.
var registered = func() *answerKind {
	k := &answerKind{}
	chregistry.Register(k)
	return k
}()

func reset(t *testing.T) *answerKind {
	t.Helper()
	registered.mu.Lock()
	defer registered.mu.Unlock()
	registered.standing, registered.claimErr, registered.concludeErr = nil, nil, nil
	registered.noSurface = false
	registered.claims, registered.published = 0, nil
	return registered
}

func binding(kind string) *spiceboxv1alpha1.ChannelBinding {
	return &spiceboxv1alpha1.ChannelBinding{Name: taChannelName, Kind: kind, Key: taBindingKey}
}

func cluster(t *testing.T, withChannel, withSecret bool) client.Client {
	t.Helper()
	sch := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(sch))
	require.NoError(t, spiceboxv1alpha1.AddToScheme(sch))
	var objs []client.Object
	if withChannel {
		objs = append(objs, &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: taChannelName, Namespace: taNS},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind:           taKindName,
				Role:           spiceboxv1alpha1.ChannelRoleInput,
				CredentialsRef: spiceboxv1alpha1.ChannelCredentialsRef{SecretName: taSecretName},
			},
		})
	}
	if withSecret {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: taSecretName, Namespace: taNS},
			Data:       map[string][]byte{"fixture-key": []byte("present")},
		})
	}
	return fake.NewClientBuilder().WithScheme(sch).WithObjects(objs...).Build()
}

func TestAnswerCouldNotFinish(t *testing.T) {
	const summary = "stopped; reason `BudgetExceeded`"
	cases := []struct {
		name                    string
		binding                 *spiceboxv1alpha1.ChannelBinding
		withChannel, withSecret bool
		arrange                 func(k *answerKind)
		wantResult              triggeranswer.Result
		wantStanding            channelkinds.TriggerOutcome
		wantErr                 string
		wantClaims              int
		wantPublished           bool
	}{
		{
			name:    "open trigger: concluded could_not_finish with the caller's summary",
			binding: binding(taKindName), withChannel: true, withSecret: true,
			wantResult: triggeranswer.Answered, wantClaims: 1, wantPublished: true,
		},
		{
			name:    "trigger already answered: verdict kept, nothing published",
			binding: binding(taKindName), withChannel: true, withSecret: true,
			arrange: func(k *answerKind) {
				k.standing = &channelkinds.TriggerClaim{Concluded: true, Outcome: channelkinds.TriggerOutcomeProblemsFound}
			},
			wantResult: triggeranswer.AlreadyAnswered, wantStanding: channelkinds.TriggerOutcomeProblemsFound, wantClaims: 1,
		},
		{
			name:    "claim fails: verdict unknown, so nothing published and an error returned",
			binding: binding(taKindName), withChannel: true, withSecret: true,
			arrange:    func(k *answerKind) { k.claimErr = errors.New("provider down") },
			wantErr:    "left alone",
			wantClaims: 1,
		},
		{
			name:    "conclude fails: error returned",
			binding: binding(taKindName), withChannel: true, withSecret: true,
			arrange:    func(k *answerKind) { k.concludeErr = errors.New("provider down") },
			wantErr:    "conclude the fixture check run",
			wantClaims: 1,
		},
		{
			name:    "no input binding: not owed, provider never touched",
			binding: nil, withChannel: true, withSecret: true,
			wantResult: triggeranswer.NotOwed,
		},
		{
			name:    "kind with no trigger surface: not owed, provider never touched",
			binding: binding("fake"), withChannel: true, withSecret: true,
			wantResult: triggeranswer.NotOwed,
		},
		{
			name:    "kind says this binding carries no trigger: not owed, provider never touched",
			binding: binding(taKindName), withChannel: true, withSecret: true,
			arrange:    func(k *answerKind) { k.noSurface = true },
			wantResult: triggeranswer.NotOwed,
		},
		{
			name:    "input Channel missing: error, provider never touched",
			binding: binding(taKindName), withSecret: true,
			wantErr: "read the input Channel",
		},
		{
			name:    "credentials Secret missing: error, provider never touched",
			binding: binding(taKindName), withChannel: true,
			wantErr: "read the credentials Secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := reset(t)
			if tc.arrange != nil {
				tc.arrange(k)
			}
			rep, err := triggeranswer.Trigger{
				Client:             cluster(t, tc.withChannel, tc.withSecret),
				Namespace:          taNS,
				Binding:            tc.binding,
				ProviderAPIBaseURL: "https://provider.test",
			}.AnswerCouldNotFinish(context.Background(), summary)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.wantResult, rep.Result)
				assert.Equal(t, tc.wantStanding, rep.Standing)
			}
			claims, published := k.claims, k.published
			assert.Equal(t, tc.wantClaims, claims, "Claim is asked exactly when a surface was reached")
			if tc.wantPublished {
				require.Len(t, published, 1)
				assert.Equal(t, channelkinds.TriggerOutcomeCouldNotFinish, published[0].Outcome)
				assert.Equal(t, summary, published[0].Summary)
			} else {
				assert.Empty(t, published, "nothing may be written to the trigger in this case")
			}
		})
	}
}

func TestSummaries_CarryTheReasonTokenAndSayDifferentThings(t *testing.T) {
	const reason = "BudgetExceeded"
	start, stopped := triggeranswer.CouldNotStartSummary(reason), triggeranswer.StoppedSummary(reason)
	assert.Contains(t, start, "`"+reason+"`")
	assert.Contains(t, stopped, "`"+reason+"`")
	assert.Contains(t, start, "could not start")
	assert.Contains(t, stopped, "stopped before it finished")
	assert.NotContains(t, stopped, "could not start")
}

func TestAnswerCouldNotFinish_NoClientIsAnErrorNotAPanic(t *testing.T) {
	reset(t)
	_, err := triggeranswer.Trigger{Namespace: taNS, Binding: binding(taKindName)}.
		AnswerCouldNotFinish(context.Background(), "s")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no Kubernetes client")
}

func TestLog(t *testing.T) {
	cases := []struct {
		name    string
		rep     triggeranswer.Report
		err     error
		wantMsg string // "" means nothing is logged
	}{
		{name: "error: logged as still in progress", err: errors.New("boom"), wantMsg: "keeps showing the work as in progress"},
		{name: "already answered: logged as kept", rep: triggeranswer.Report{Result: triggeranswer.AlreadyAnswered, Standing: channelkinds.TriggerOutcomeClean}, wantMsg: "did not overwrite it"},
		{name: "answered: logged", rep: triggeranswer.Report{Result: triggeranswer.Answered}, wantMsg: "answered on behalf of a failed session"},
		{name: "not owed: silent", rep: triggeranswer.Report{Result: triggeranswer.NotOwed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var lines []string
			logger := funcr.New(func(prefix, args string) { lines = append(lines, args) }, funcr.Options{})
			triggeranswer.Log(logger, "ns/sess", "BudgetExceeded", tc.rep, tc.err)
			if tc.wantMsg == "" {
				assert.Empty(t, lines)
				return
			}
			require.Len(t, lines, 1)
			assert.Contains(t, lines[0], tc.wantMsg)
			assert.Contains(t, lines[0], "ns/sess")
			assert.Contains(t, lines[0], "BudgetExceeded")
		})
	}
}
