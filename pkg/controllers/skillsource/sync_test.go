package skillsource

import (
	"testing"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/conditions"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credresolve"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTokenFromSecret(t *testing.T) {
	ref := types.NamespacedName{Namespace: "ns", Name: "pat"}

	cases := []struct {
		name      string
		data      map[string][]byte
		key       string
		wantToken string
		wantErr   bool
	}{
		{
			name:      "value present: returned verbatim",
			data:      map[string][]byte{"token": []byte("ghp_x")},
			key:       "token",
			wantToken: "ghp_x",
		},
		{
			name:    "key absent: ErrSecretKeyMissing, no anonymous fallback",
			data:    map[string][]byte{"other": []byte("ghp_x")},
			key:     "token",
			wantErr: true,
		},
		{
			name:    "key present but empty: ErrSecretKeyMissing, no anonymous fallback",
			data:    map[string][]byte{"token": []byte("")},
			key:     "token",
			wantErr: true,
		},
		{
			name:    "no data at all: ErrSecretKeyMissing",
			data:    nil,
			key:     "token",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := &corev1.Secret{Data: tc.data}
			got, err := TokenFromSecret(sec, ref, tc.key)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, credresolve.ErrSecretKeyMissing)
				assert.Empty(t, got, "an empty token means 'clone anonymously' downstream — never return one on error")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantToken, got)
		})
	}
}

// genObj is a minimal conditions.Generationer for RecordSync.
type genObj int64

func (g genObj) GetGeneration() int64 { return int64(g) }

func TestRecordSync(t *testing.T) {
	// synced is the status a completed pass over one skill at SHA "sha1" leaves.
	synced := func() *v1.SkillSourceStatus {
		st := &v1.SkillSourceStatus{}
		require.True(t, RecordSync(genObj(1), st, SyncResult{
			ResolvedSHA: "sha1", DiscoveredSkills: 1, Materialized: true,
		}), "seeding pass must report a move")
		return st
	}

	cases := []struct {
		name      string
		res       SyncResult
		wantMoved bool
	}{
		{
			name:      "identical pass that wrote nothing: no move, no re-stamp",
			res:       SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1},
			wantMoved: false,
		},
		{
			name:      "identical derived fields but the pass wrote: move, re-stamp",
			res:       SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1, Materialized: true},
			wantMoved: true,
		},
		{
			name:      "resolved SHA advanced: move",
			res:       SyncResult{ResolvedSHA: "sha2", DiscoveredSkills: 1},
			wantMoved: true,
		},
		{
			name:      "skill count changed: move",
			res:       SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 2},
			wantMoved: true,
		},
		{
			name:      "a discovery problem appeared: move",
			res:       SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1, Problems: []string{"skipping x"}},
			wantMoved: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := synced()
			before := st.DeepCopy()

			assert.Equal(t, tc.wantMoved, RecordSync(genObj(1), st, tc.res))
			if tc.wantMoved {
				return
			}
			// A no-move pass must leave the status byte-identical — including
			// lastSyncTime, the one volatile field — so the caller can skip its
			// Status().Update and the self-watch stays quiet.
			assert.Equal(t, before, st)
		})
	}
}

func TestRecordSyncStampsLastSyncTimeOnFirstPass(t *testing.T) {
	st := &v1.SkillSourceStatus{}
	require.True(t, RecordSync(genObj(1), st, SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1}))

	assert.Equal(t, "sha1", st.ResolvedSHA)
	assert.Equal(t, int32(1), st.DiscoveredSkills)
	assert.Equal(t, int64(1), st.ObservedGeneration)
	assert.NotNil(t, st.LastSyncTime, "the first pass always moves, so lastSyncTime is stamped")

	ready := conditions.Find(st.Conditions, v1.SkillSourceConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, metav1.ConditionTrue, ready.Status)
	assert.Equal(t, v1.ReasonSkillSourceSynced, ready.Reason)
}

func TestRecordSyncReadyReflectsWhatWasDiscovered(t *testing.T) {
	// Ready is the one signal an operator reads without asking for detail — it
	// is the READY column of `oap skill source list` and the headline of
	// `kubectl get skillsource`. A pass that materialized NO skill has produced
	// nothing for any AgentClass to opt into, so reporting it as Synced makes a
	// source that yielded nothing look identical to one that yielded everything,
	// and leaves the parked AgentClass as the only thing that looks wrong.
	cases := []struct {
		name       string
		res        SyncResult
		wantStatus metav1.ConditionStatus
		wantReason string
		wantMsg    string
	}{
		{
			name:       "one skill, no problems: Ready=True/Synced",
			res:        SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1.ReasonSkillSourceSynced,
		},
		{
			name:       "one skill, one rejected: Ready=True/Synced, message points at status",
			res:        SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 1, Problems: []string{"skipping x"}},
			wantStatus: metav1.ConditionTrue,
			wantReason: v1.ReasonSkillSourceSynced,
			wantMsg:    "status.discoveryProblems",
		},
		{
			name:       "no skill and nothing rejected: Ready=False/NoSkillsDiscovered",
			res:        SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 0},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSkillSourceNoSkillsDiscovered,
			wantMsg:    "no SKILL.md",
		},
		{
			name:       "no skill because every candidate was rejected: Ready=False/NoSkillsDiscovered",
			res:        SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 0, Problems: []string{"skipping x"}},
			wantStatus: metav1.ConditionFalse,
			wantReason: v1.ReasonSkillSourceNoSkillsDiscovered,
			wantMsg:    "status.discoveryProblems",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &v1.SkillSourceStatus{}
			require.True(t, RecordSync(genObj(1), st, tc.res), "the first pass always moves")

			ready := conditions.Find(st.Conditions, v1.SkillSourceConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, tc.wantStatus, ready.Status)
			assert.Equal(t, tc.wantReason, ready.Reason)
			if tc.wantMsg != "" {
				assert.Contains(t, ready.Message, tc.wantMsg)
			}
			assert.Len(t, st.DiscoveryProblems, len(tc.res.Problems),
				"every discovery problem stays on status regardless of the Ready outcome")
		})
	}
}

func TestRecordSyncEmptyPassStaysQuiescent(t *testing.T) {
	// The zero-skill outcome is recomputed on every pass like every other
	// derived field, so a repeat of the same empty pass must leave the status
	// byte-identical. Both controllers watch their own object with no
	// predicate, so a re-stamp here re-enqueues the reconcile — and its git
	// fetch — forever.
	st := &v1.SkillSourceStatus{}
	require.True(t, RecordSync(genObj(1), st, SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 0}))
	before := st.DeepCopy()

	assert.False(t, RecordSync(genObj(1), st, SyncResult{ResolvedSHA: "sha1", DiscoveredSkills: 0}),
		"an identical empty pass must not report a move")
	assert.Equal(t, before, st)
}
