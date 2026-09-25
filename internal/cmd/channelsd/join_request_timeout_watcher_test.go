package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
)

// fakeNATSPub records every Publish call for assertion.
type fakeNATSPub struct {
	subjects []string
	payloads [][]byte
}

func (f *fakeNATSPub) Publish(subj string, data []byte) error {
	f.subjects = append(f.subjects, subj)
	f.payloads = append(f.payloads, append([]byte(nil), data...))
	return nil
}

// joinTimeoutFixture seeds a fake k8s client with one AgentSession holding a
// single PendingRequester, and (optionally) one AgentClass. Returns the wired
// watcher + the fake NATS recorder so callers can assert on publishes.
type joinTimeoutFixture struct {
	cli     client.Client
	watcher *joinRequestTimeoutWatcher
	natsRec *fakeNATSPub
	sessKey client.ObjectKey
}

// newJoinTimeoutFixture builds a fake k8s client containing one AgentSession
// (with the given pendingRequester + class name) and one AgentClass (with the
// given approvalTimeout, nil for default). className may be empty to test the
// no-class fallback path; approvalTimeout may be nil for the class-without-
// timeout path.
func newJoinTimeoutFixture(
	t *testing.T,
	pr spiceboxv1alpha1.PendingRequester,
	className string,
	approvalTimeout *metav1.Duration,
	nowFn func() time.Time,
) *joinTimeoutFixture {
	t.Helper()
	scheme := makeScheme()

	sessKey := client.ObjectKey{Namespace: "default", Name: "sess-1"}
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessKey.Name, Namespace: sessKey.Namespace},
		Spec:       spiceboxv1alpha1.AgentSessionSpec{Class: className},
	}

	objs := []client.Object{sess}
	if className != "" {
		cls := &spiceboxv1alpha1.AgentClass{
			ObjectMeta: metav1.ObjectMeta{Name: className, Namespace: sessKey.Namespace},
			Spec: spiceboxv1alpha1.AgentClassSpec{
				Authz: &spiceboxv1alpha1.AuthzBlock{ApprovalTimeout: approvalTimeout},
			},
		}
		objs = append(objs, cls)
	}

	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	// Seed the pendingRequester + matching condition via the status subresource.
	var seeded spiceboxv1alpha1.AgentSession
	require.NoError(t, cli.Get(context.Background(), sessKey, &seeded), "get session for seed")
	seeded.Status.PendingRequesters = []spiceboxv1alpha1.PendingRequester{pr}
	seeded.Status.Conditions = []metav1.Condition{{
		Type:               spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending,
		Status:             metav1.ConditionTrue,
		Reason:             "RequesterPending",
		LastTransitionTime: metav1.Now(),
	}}
	require.NoError(t, cli.Status().Update(context.Background(), &seeded), "seed pending requester")

	natsRec := &fakeNATSPub{}
	pipe := pipeline.NewPipeline(cli, nil, nil, natsRec, func(string) []string { return nil })
	w := newJoinRequestTimeoutWatcher(cli, pipe)
	if nowFn != nil {
		w.now = nowFn
	}
	return &joinTimeoutFixture{cli: cli, watcher: w, natsRec: natsRec, sessKey: sessKey}
}

// testRequester returns a baseline PendingRequester so each row only overrides
// the RequestedAt timestamp.
func testRequester(requestedAt time.Time) spiceboxv1alpha1.PendingRequester {
	return spiceboxv1alpha1.PendingRequester{
		Kind:        "slack",
		TeamScope:   "T1",
		ExternalID:  "U123",
		RequestRef:  "ref-1",
		RequestedAt: metav1.NewTime(requestedAt),
		MessageText: "please let me in",
	}
}

// TestJoinTimeoutWatcher_FiringAcrossTimeoutSources collapses the deadline
// behaviors into one table: default-timeout fires after 15m, class-timeout
// fires once exceeded, class-timeout suppresses while inside the window.
func TestJoinTimeoutWatcher_FiringAcrossTimeoutSources(t *testing.T) {
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	classTimeout := &metav1.Duration{Duration: 2 * time.Minute}

	cases := []struct {
		name      string
		className string // empty ⇒ default 15m
		timeout   *metav1.Duration
		age       time.Duration
		wantFired bool
	}{
		{
			name:      "no class, 16m old vs default 15m: expires (deny/timeout published, condition False)",
			age:       16 * time.Minute,
			wantFired: true,
		},
		{
			name:      "class timeout 2m, 3m old: expires (exceeded class window)",
			className: "ac-fast",
			timeout:   classTimeout,
			age:       3 * time.Minute,
			wantFired: true,
		},
		{
			name:      "class timeout 2m, 90s old: does NOT expire (inside class window)",
			className: "ac-fast",
			timeout:   classTimeout,
			age:       90 * time.Second,
			wantFired: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newJoinTimeoutFixture(t,
				testRequester(now.Add(-tc.age)),
				tc.className, tc.timeout,
				func() time.Time { return now },
			)

			require.NoError(t, fx.watcher.tick(context.Background()), "tick")

			var got spiceboxv1alpha1.AgentSession
			require.NoError(t, fx.cli.Get(context.Background(), fx.sessKey, &got), "Get session")

			if !tc.wantFired {
				assert.Len(t, got.Status.PendingRequesters, 1,
					"pending requester must NOT clear inside timeout window")
				assert.Empty(t, fx.natsRec.subjects, "expected no publishes inside timeout window")
				return
			}

			assert.Empty(t, got.Status.PendingRequesters,
				"PendingRequesters must be cleared on timeout")

			var cond *metav1.Condition
			for i := range got.Status.Conditions {
				c := &got.Status.Conditions[i]
				if c.Type == spiceboxv1alpha1.AgentSessionConditionPermissionRequestPending {
					cond = c
					break
				}
			}
			if assert.NotNil(t, cond, "PermissionRequestPending condition should be set") {
				assert.Equal(t, metav1.ConditionFalse, cond.Status, "condition must flip False")
			}

			pl := findPermissionAppliedPayload(t, fx.natsRec)
			assert.Equal(t, "ref-1", pl.RequestRef)
			assert.Equal(t, "permission_request", pl.Category, "expired join must carry the permission_request category")
			assert.Equal(t, channelevents.OutcomeExpired, pl.Outcome, "an expired join is not a success")
			assert.Nil(t, pl.DecidedBy, "no one decided a timeout")
		})
	}
}

// TestJoinTimeoutWatcher_SkipsSessionsWithoutPending verifies AgentSessions
// with no PendingRequesters are a no-op: no publishes, no patches.
func TestJoinTimeoutWatcher_SkipsSessionsWithoutPending(t *testing.T) {
	scheme := makeScheme()
	sess := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "sess-empty", Namespace: "default"},
	}
	cli := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sess).
		WithStatusSubresource(&spiceboxv1alpha1.AgentSession{}).
		Build()

	natsRec := &fakeNATSPub{}
	pipe := pipeline.NewPipeline(cli, nil, nil, natsRec, func(string) []string { return nil })
	w := newJoinRequestTimeoutWatcher(cli, pipe)
	w.now = func() time.Time { return time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC) }

	require.NoError(t, w.tick(context.Background()), "tick")
	assert.Empty(t, natsRec.subjects, "expected zero publishes")
}

// findPermissionAppliedPayload locates the first OUT KindInteractionApplied
// envelope recorded by rec and returns its decoded payload. TimeoutPermissionRequest
// (pkg/channels/channelsd/pipeline/decision.go's publishPermissionTimeoutApplied) publishes
// the generic interaction_applied kind, not the legacy permission_decision_applied.
func findPermissionAppliedPayload(t *testing.T, rec *fakeNATSPub) channelevents.InteractionAppliedPayload {
	t.Helper()
	suffix := ".out." + string(channelevents.KindInteractionApplied)
	for i, s := range rec.subjects {
		if !strings.HasSuffix(s, suffix) {
			continue
		}
		var env channelevents.Envelope
		require.NoError(t, json.Unmarshal(rec.payloads[i], &env), "unmarshal envelope")
		var pl channelevents.InteractionAppliedPayload
		require.NoError(t, json.Unmarshal(env.Payload, &pl), "unmarshal applied payload")
		return pl
	}
	t.Fatalf("no OUT interaction_applied envelope; subjects=%v", rec.subjects)
	return channelevents.InteractionAppliedPayload{}
}
