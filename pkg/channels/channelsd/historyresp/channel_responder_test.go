package historyresp

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	testingclock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelfeatures"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// fakeMemberChecker is an injectable MemberChecker for tests. It records
// every call so a test can assert whether it was reached at all.
type fakeMemberChecker struct {
	allowed bool
	err     error
	calls   []string // subjectRef the check was called with
}

func (f *fakeMemberChecker) LookupSubjectIncludes(_ context.Context, subjectRef string, _ identity.CanonicalUserID) (bool, error) {
	f.calls = append(f.calls, subjectRef)
	return f.allowed, f.err
}

// kindStub implements channelkinds.Kind inertly for every method the
// channel-history tests don't exercise. Embedded by fakeHistoryKind (which also
// implements ChannelHistoryReader) and nonHistoryKind (which deliberately does
// not). It stays free of any reader method because a value-embedded struct
// promotes ALL of its methods — pointer-receiver ones included, once you take
// the outer type's address — which would leak ChannelHistoryReader onto
// nonHistoryKind and silently void that row.
type kindStub struct{}

func (kindStub) Name() string                                                       { return "fakehistory" }
func (kindStub) DefaultSessionScope() string                                        { return "auto" }
func (kindStub) Capabilities() []string                                             { return []string{"text"} }
func (kindStub) NewListener(_ channelkinds.Deps) channelkinds.Listener              { return nil }
func (kindStub) NewSender(_ channelkinds.Deps) channelkinds.Sender                  { return nil }
func (kindStub) SubChannelSender(_ string, _ channelkinds.Deps) channelkinds.Sender { return nil }
func (kindStub) NewStreamDeltaSink(_ channelkinds.Deps) channelkinds.StreamDeltaSink {
	return nil
}
func (kindStub) SupportsMonitoring() bool    { return false }
func (kindStub) SupportsLiveViewOffer() bool { return false }

func (kindStub) NewMonitoringSender(_ channelkinds.Deps) channelkinds.MonitoringSender {
	return nil
}
func (kindStub) SupportedRoles() []string                                { return spiceboxv1alpha1.AllChannelRoles() }
func (kindStub) ValidateSpec(_ *spiceboxv1alpha1.Channel) error          { return nil }
func (kindStub) PublicSecretKeys(_ *spiceboxv1alpha1.Channel) []string   { return nil }
func (kindStub) RequiredSecretKeys(_ *spiceboxv1alpha1.Channel) []string { return nil }
func (kindStub) FeatureSupport() map[channelfeatures.Feature]channelkinds.FeatureRequirement {
	return nil
}
func (kindStub) RenderMention(externalID string) string { return externalID }
func (kindStub) SupportedMentionLookups() []channelkinds.MentionLookupKind {
	return nil
}
func (kindStub) LookupUser(
	_ context.Context, _ channelkinds.LookupDeps, _ channelkinds.MentionLookupKind, _ string,
) (string, string, error) {
	return "", "", channelkinds.ErrMentionUnsupported
}
func (kindStub) MentionToolDescription() string { return "" }
func (kindStub) UserAttributable() bool         { return true }
func (kindStub) DeliversToHuman() bool          { return true }
func (kindStub) AllowsSyntheticIdentity() bool  { return false }
func (kindStub) RelayedByChannelsd() bool       { return true }
func (kindStub) SpawnsSessionOnInbound() bool   { return true }
func (kindStub) WebAuthenticator(channelkinds.WebAuthDeps) channelkinds.WebAuthenticator {
	return nil
}
func (kindStub) WebhookReceiver(channelkinds.Deps) channelkinds.WebhookReceiver { return nil }
func (kindStub) Wizard() channelkinds.Wizard                                    { return nil }

// fakeHistoryKind implements channelkinds.Kind (via the embedded kindStub)
// plus channelkinds.ChannelHistoryReader, so the gating-matrix test can
// inject it via ChannelResponder.Resolve without touching the global kind
// registry or SpiceDB.
type fakeHistoryKind struct {
	kindStub

	bounds      channelkinds.ChannelHistoryBounds
	viewRef     string
	viewRefOK   bool
	page        channelkinds.HistoryPage
	readErr     error
	readReached bool
	gotOpts     channelkinds.ChannelHistoryOpts
}

func (k *fakeHistoryKind) ChannelHistoryBounds() channelkinds.ChannelHistoryBounds { return k.bounds }

func (k *fakeHistoryKind) ReadChannelHistory(
	_ context.Context, _ channelkinds.Deps, _ *spiceboxv1alpha1.ChannelBinding, opts channelkinds.ChannelHistoryOpts,
) (channelkinds.HistoryPage, error) {
	k.readReached = true
	k.gotOpts = opts
	return k.page, k.readErr
}

func (k *fakeHistoryKind) ChannelViewSubjectRef(_ *spiceboxv1alpha1.ChannelBinding) (string, bool) {
	return k.viewRef, k.viewRefOK
}

// nonHistoryKind implements channelkinds.Kind (via kindStub) but NOT
// ChannelHistoryReader — used for the "kind lacks reader" row.
type nonHistoryKind struct{ kindStub }

// resolveTo builds a ChannelResponder.Resolve func that always returns the
// given kind (and a throwaway Secret), regardless of the Channel passed in.
func resolveTo(k channelkinds.Kind) func(context.Context, client.Client, *spiceboxv1alpha1.Channel) (*corev1.Secret, channelkinds.Kind, error) {
	return func(context.Context, client.Client, *spiceboxv1alpha1.Channel) (*corev1.Secret, channelkinds.Kind, error) {
		return &corev1.Secret{}, k, nil
	}
}

func enabledHistorySpec() *spiceboxv1alpha1.ChannelHistorySpec {
	return &spiceboxv1alpha1.ChannelHistorySpec{Enabled: true}
}

func leakageEnforcingClass(ns, name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				InformationLeakage: &spiceboxv1alpha1.InformationLeakagePolicy{Mode: "enforcing"},
			},
		},
	}
}

func leakageDisabledClass(ns, name string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       spiceboxv1alpha1.AgentClassSpec{},
	}
}

// TestHandle_GatingMatrix is the security-critical table: every combination
// of opt-in, kind capability, and info-leakage authz that determines whether
// a read_channel_history call is allowed or withheld.
func TestHandle_GatingMatrix(t *testing.T) {
	const ns = "default"

	type tc struct {
		name string
		// buildChannel customizes the Channel CR (opt-in, agentClass).
		buildChannel func(ch *spiceboxv1alpha1.Channel)
		// class is the AgentClass seeded alongside (nil = none seeded).
		class *spiceboxv1alpha1.AgentClass
		// kind is what Resolve returns; nil = *fakeHistoryKind default.
		kind channelkinds.Kind
		// outputDiffers sets sess.Spec.OutputChannel to a DIFFERENT channel
		// than the input binding. Default (false) leaves OutputChannel nil,
		// which ChannelBinding.SameChannelAs treats as "same as input".
		outputDiffers bool
		// startedByCanonical seeds the started-by annotation ("" = omit).
		startedByCanonical string
		// authzAllowed / authzErr configure the fakeMemberChecker.
		authzAllowed bool
		authzErr     error
		nilAuthz     bool

		wantWithheld     bool
		wantReadReached  bool
		wantAuthzReached bool
	}

	cases := []tc{
		{
			name: "opt-in absent -> Error, ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = nil
			},
			wantWithheld:    true,
			wantReadReached: false,
		},
		{
			name: "kind lacks ChannelHistoryReader -> Error, ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
			},
			kind:            &nonHistoryKind{},
			wantWithheld:    true,
			wantReadReached: false,
		},
		{
			name: "leakage on, member=true -> allowed, ReadChannelHistory reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:              leakageEnforcingClass(ns, "cls"),
			startedByCanonical: "user:alice",
			authzAllowed:       true,
			wantWithheld:       false,
			wantReadReached:    true,
			wantAuthzReached:   true,
		},
		{
			name: "leakage on, member=false -> Error (withheld), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:              leakageEnforcingClass(ns, "cls"),
			startedByCanonical: "user:bob",
			authzAllowed:       false,
			wantWithheld:       true,
			wantReadReached:    false,
			wantAuthzReached:   true,
		},
		{
			name: "leakage on, SpiceDB error -> Error (fail closed), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:              leakageEnforcingClass(ns, "cls"),
			startedByCanonical: "user:carol",
			authzErr:           errSpiceDBUnavailable,
			wantWithheld:       true,
			wantReadReached:    false,
			wantAuthzReached:   true,
		},
		{
			name: "leakage on, no session initiator -> Error (withheld), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:            leakageEnforcingClass(ns, "cls"),
			wantWithheld:     true,
			wantReadReached:  false,
			wantAuthzReached: false,
		},
		{
			name: "leakage on, nil Authz -> Error (withheld), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:              leakageEnforcingClass(ns, "cls"),
			startedByCanonical: "user:dave",
			nilAuthz:           true,
			wantWithheld:       true,
			wantReadReached:    false,
		},
		{
			name: "agent class Get error (missing) -> Error (fail closed), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "does-not-exist"
			},
			wantWithheld:    true,
			wantReadReached: false,
		},
		{
			name: "leakage off, output==input -> allowed, ReadChannelHistory reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:           leakageDisabledClass(ns, "cls"),
			wantWithheld:    false,
			wantReadReached: true,
		},
		{
			name: "leakage off, output!=input -> Error (withheld), ReadChannelHistory not reached",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				ch.Spec.AgentClass = "cls"
			},
			class:           leakageDisabledClass(ns, "cls"),
			outputDiffers:   true,
			wantWithheld:    true,
			wantReadReached: false,
		},
		{
			name: "no agentClass set on Channel -> leakage treated off; output==input -> allowed",
			buildChannel: func(ch *spiceboxv1alpha1.Channel) {
				ch.Spec.ChannelHistory = enabledHistorySpec()
				// ch.Spec.AgentClass left empty
			},
			wantWithheld:    false,
			wantReadReached: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ch := &spiceboxv1alpha1.Channel{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "ch1"},
			}
			c.buildChannel(ch)

			input := &spiceboxv1alpha1.ChannelBinding{Name: "ch1", Kind: "fakehistory", External: map[string]string{"channel_id": "C1"}}
			// nil OutputChannel means "same as input" per ChannelBinding.SameChannelAs;
			// only set it explicitly when the case wants a DIFFERENT output channel.
			var output *spiceboxv1alpha1.ChannelBinding
			if c.outputDiffers {
				output = &spiceboxv1alpha1.ChannelBinding{Name: "ch2", Kind: "fakehistory", External: map[string]string{"channel_id": "C2"}}
			}

			sess := &spiceboxv1alpha1.AgentSession{
				ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "sess1"},
				Spec: spiceboxv1alpha1.AgentSessionSpec{
					InputChannel:  input,
					OutputChannel: output,
				},
			}
			if c.startedByCanonical != "" {
				sess.Annotations = map[string]string{spiceboxv1alpha1.AnnotationStartedByCanonicalID: c.startedByCanonical}
			}

			objs := []client.Object{sess, ch}
			if c.class != nil {
				objs = append(objs, c.class)
			}
			cli := fake.NewClientBuilder().WithScheme(buildScheme(t)).WithObjects(objs...).Build()

			var kind channelkinds.Kind
			var fhk *fakeHistoryKind
			if c.kind != nil {
				kind = c.kind
			} else {
				fhk = &fakeHistoryKind{
					bounds:    channelkinds.ChannelHistoryBounds{MaxLookback: 30 * 24 * time.Hour, MaxMessages: 200, SupportsDateRange: true},
					viewRef:   "slack_channel:C1#view",
					viewRefOK: true,
					page: channelkinds.HistoryPage{
						Messages: []channelkinds.HistoryMessage{{AuthorDisplayName: "Alice", Text: "hi", TS: "1.0"}},
					},
				}
				kind = fhk
			}

			checker := &fakeMemberChecker{allowed: c.authzAllowed, err: c.authzErr}
			r := &ChannelResponder{
				K8s:     cli,
				Clock:   testingclock.NewFakeClock(time.Unix(0, 0)),
				Resolve: resolveTo(kind),
			}
			if !c.nilAuthz {
				r.Authz = checker
			}

			resp := r.handle(context.Background(), ns, "sess1", channelevents.ChannelHistoryRequest{})

			if c.wantWithheld {
				assert.NotEmpty(t, resp.Error, "expected withheld/error response")
			} else {
				assert.Empty(t, resp.Error, "expected allowed response, got error: %s", resp.Error)
				require.Len(t, resp.Messages, 1)
				assert.Equal(t, "Alice", resp.Messages[0].AuthorDisplayName)
			}

			if fhk != nil {
				assert.Equal(t, c.wantReadReached, fhk.readReached, "ReadChannelHistory reached mismatch")
			}
			assert.Equal(t, c.wantAuthzReached, len(checker.calls) > 0, "Authz.LookupSubjectIncludes reached mismatch")
		})
	}
}

// errSpiceDBUnavailable is a stand-in SpiceDB failure for the fail-closed row.
var errSpiceDBUnavailable = context.DeadlineExceeded

func TestClampOpts_DefaultsAndClamps(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	r := &ChannelResponder{Clock: testingclock.NewFakeClock(now)}

	t.Run("no request bounds -> default 7d window, kind limit", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{ChannelHistory: enabledHistorySpec()}}
		bounds := channelkinds.ChannelHistoryBounds{MaxLookback: 30 * 24 * time.Hour, MaxMessages: 500}
		opts := r.clampOpts(ch, bounds, channelevents.ChannelHistoryRequest{})
		assert.Equal(t, now.Add(-channelHistoryDefaultWindow), opts.NotBefore)
		assert.Equal(t, now, opts.NotAfter)
		assert.Equal(t, 500, opts.Limit)
	})

	t.Run("CR ceiling tighter than kind bounds wins", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{ChannelHistory: &spiceboxv1alpha1.ChannelHistorySpec{
			Enabled:     true,
			MaxLookback: &metav1.Duration{Duration: 24 * time.Hour},
			MaxMessages: int32Ptr(10),
		}}}
		bounds := channelkinds.ChannelHistoryBounds{MaxLookback: 30 * 24 * time.Hour, MaxMessages: 500}
		opts := r.clampOpts(ch, bounds, channelevents.ChannelHistoryRequest{LookbackDays: 20, Limit: 100})
		assert.Equal(t, now.Add(-24*time.Hour), opts.NotBefore, "CR MaxLookback caps the request's LookbackDays")
		assert.Equal(t, 10, opts.Limit, "CR MaxMessages caps the request's Limit")
	})

	t.Run("date range ignored when kind does not support it", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{ChannelHistory: enabledHistorySpec()}}
		bounds := channelkinds.ChannelHistoryBounds{MaxLookback: 30 * 24 * time.Hour, MaxMessages: 500, SupportsDateRange: false}
		opts := r.clampOpts(ch, bounds, channelevents.ChannelHistoryRequest{Since: "2020-01-01T00:00:00Z"})
		assert.Equal(t, now.Add(-channelHistoryDefaultWindow), opts.NotBefore, "Since ignored: kind doesn't support date range")
	})

	t.Run("date range honored when kind supports it", func(t *testing.T) {
		ch := &spiceboxv1alpha1.Channel{Spec: spiceboxv1alpha1.ChannelSpec{ChannelHistory: enabledHistorySpec()}}
		bounds := channelkinds.ChannelHistoryBounds{MaxLookback: 30 * 24 * time.Hour, MaxMessages: 500, SupportsDateRange: true}
		since := now.Add(-2 * time.Hour)
		opts := r.clampOpts(ch, bounds, channelevents.ChannelHistoryRequest{Since: since.Format(time.RFC3339)})
		assert.Equal(t, since, opts.NotBefore)
	})
}

func int32Ptr(v int32) *int32 { return &v }
