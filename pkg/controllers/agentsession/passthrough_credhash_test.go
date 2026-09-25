package agentsession

import (
	"context"
	"errors"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/revocation"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredHashes_StableAndPerKey(t *testing.T) {
	h := credHashes(map[string]string{"github": "tok-1", "linear": "tok-2"})
	assert.Len(t, h, 2)
	assert.Equal(t, h["github"], credHashes(map[string]string{"github": "tok-1"})["github"],
		"same value hashes stably")
	assert.NotEqual(t, h["github"], h["linear"], "different values hash differently")
}

func TestDiffCredHashes(t *testing.T) {
	cases := []struct {
		name        string
		prev, next  map[string]string
		wantChanged []string
		wantRemoved bool
	}{
		{name: "no change", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "1"}, wantChanged: nil, wantRemoved: false},
		{name: "replaced", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "2"}, wantChanged: []string{"a"}, wantRemoved: false},
		{name: "removed", prev: map[string]string{"a": "1", "b": "2"}, next: map[string]string{"a": "1"}, wantChanged: []string{"b"}, wantRemoved: true},
		{name: "added only (no emit)", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "1", "b": "2"}, wantChanged: nil, wantRemoved: false},
		{name: "replaced + removed", prev: map[string]string{"a": "1", "b": "2"}, next: map[string]string{"a": "9"}, wantChanged: []string{"a", "b"}, wantRemoved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			changed, removed := diffCredHashes(tc.prev, tc.next)
			sort.Strings(changed)
			want := append([]string(nil), tc.wantChanged...)
			sort.Strings(want)
			assert.Equal(t, want, changed)
			assert.Equal(t, tc.wantRemoved, removed)
		})
	}
}

// fakeBus records Publish calls and can be made to fail. envelopes captures
// every published envelope (in order) so callers — notably the envtest-level
// proof in passthrough_invalidation_envtest_test.go — can decode the payload
// and assert on Kind/Key/Scope, not just the call count.
type fakeBus struct {
	published int
	fail      bool
	envelopes []channelevents.Envelope
}

func (f *fakeBus) Publish(ctx context.Context, env channelevents.Envelope) error {
	f.published++
	f.envelopes = append(f.envelopes, env)
	if f.fail {
		return errors.New("bus down")
	}
	return nil
}

func newReconcilerWithBus(f *fakeBus) *Reconciler {
	return &Reconciler{RevokePublisher: revocation.NewPublisher(f)}
}

func sessWithHashes(prev map[string]string) *spiceboxv1alpha1.AgentSession {
	s := &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-session", Namespace: "sess-ns"},
	}
	s.Status.PassthroughCredHashes = prev
	return s
}

func TestApplyPassthroughCredInvalidation(t *testing.T) {
	cases := []struct {
		name          string
		prev, next    map[string]string
		busFail       bool
		wantPublished int
		wantErr       bool
		wantStatus    map[string]string // expected sess.Status.PassthroughCredHashes after
	}{
		{name: "prime (nil prev): record, no emit", prev: nil, next: map[string]string{"a": "1"}, wantPublished: 0, wantStatus: map[string]string{"a": "1"}},
		{name: "no change: record, no emit", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "1"}, wantPublished: 0, wantStatus: map[string]string{"a": "1"}},
		{name: "added only: record, no emit", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "1", "b": "2"}, wantPublished: 0, wantStatus: map[string]string{"a": "1", "b": "2"}},
		{name: "replaced: emit once, advance status", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "2"}, wantPublished: 1, wantStatus: map[string]string{"a": "2"}},
		{name: "removed: emit once, advance status", prev: map[string]string{"a": "1", "b": "2"}, next: map[string]string{"a": "1"}, wantPublished: 1, wantStatus: map[string]string{"a": "1"}},
		{name: "replaced + bus fail: best-effort, advance status, no err", prev: map[string]string{"a": "1"}, next: map[string]string{"a": "2"}, busFail: true, wantPublished: 1, wantErr: false, wantStatus: map[string]string{"a": "2"}},
		{name: "removed + bus fail: fail-closed, keep prev, err", prev: map[string]string{"a": "1", "b": "2"}, next: map[string]string{"a": "1"}, busFail: true, wantPublished: 1, wantErr: true, wantStatus: map[string]string{"a": "1", "b": "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeBus{fail: tc.busFail}
			r := newReconcilerWithBus(f)
			s := sessWithHashes(tc.prev)
			err := r.applyPassthroughCredInvalidation(context.Background(), s, tc.next)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantPublished, f.published, "publish count")
			assert.Equal(t, tc.wantStatus, s.Status.PassthroughCredHashes, "status hashes after")
		})
	}
}
