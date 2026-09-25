// pkg/controllers/agentsession/artifact_org_visibility_test.go
//
// The opt-in org-wide artifact audience is LEVEL-TRIGGERED from the class:
// every reconcile re-levels the agentsession#artifact_org_viewer wildcard
// tuple to what spec.authz.session.artifactVisibility says right now. The
// cases that earn their keep here are the ones a happy-path test would miss:
//
//   - a TERMINAL session must still level. Completed sessions are precisely
//     the ones whose artifacts people share after the fact, and the terminal
//     pod reap short-circuits the reconcile long before the owner writes —
//     the same shape that once cost this controller the memory-token
//     re-registration (memorytoken_test.go). The sync must sit above that
//     short-circuit, and must not break the reap it sits above.
//   - flipping OFF must level to disabled (revocation), including when the
//     whole AgentClass is gone: no class means no opt-in evidence.
//   - a syncer error must fail the reconcile. A TOUCH that did not land is a
//     grant the user believes exists; a DELETE that did not land is a
//     revocation still live. Neither may be silently deferred.
package agentsession

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"k8s.io/apimachinery/pkg/types"
)

// fakeOrgViewerSyncer records every level call; err, when set, is returned
// from each call.
type fakeOrgViewerSyncer struct {
	calls []orgViewerLevel
	err   error
}

type orgViewerLevel struct {
	ns, name string
	enabled  bool
}

func (f *fakeOrgViewerSyncer) SyncArtifactOrgViewer(_ context.Context, ns, name string, enabled bool) error {
	f.calls = append(f.calls, orgViewerLevel{ns: ns, name: name, enabled: enabled})
	return f.err
}

// classWithArtifactVisibility creates the fixture session's AgentClass
// ("cls" in "default", per newReapFixture) carrying the given visibility
// ("" = field absent).
func classWithArtifactVisibility(t *testing.T, c client.Client, visibility string) {
	t.Helper()
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cls"},
	}
	if visibility != "" {
		ac.Spec.Authz = &spiceboxv1alpha1.AuthzBlock{
			Session: &spiceboxv1alpha1.SessionAuthz{ArtifactVisibility: visibility},
		}
	}
	require.NoError(t, c.Create(memory.WithSystemApproval(context.Background(), "test"), ac), "seed AgentClass")
}

func TestReconcile_TerminalSession_LevelsArtifactOrgViewerFromClass(t *testing.T) {
	cases := []struct {
		name        string
		visibility  string // "" = field absent; "absent-class" = no AgentClass at all
		wantEnabled bool
	}{
		{name: "class opted into organization: levels to enabled", visibility: spiceboxv1alpha1.ArtifactVisibilityOrganization, wantEnabled: true},
		{name: "class says session: levels to disabled", visibility: spiceboxv1alpha1.ArtifactVisibilitySession, wantEnabled: false},
		{name: "class silent on visibility: levels to disabled", visibility: "", wantEnabled: false},
		{name: "class deleted after the session finished: levels to disabled (revocation)", visibility: "absent-class", wantEnabled: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -time.Hour)
			if tc.visibility != "absent-class" {
				classWithArtifactVisibility(t, f.c, tc.visibility)
			}
			syncer := &fakeOrgViewerSyncer{}
			f.r.OrgViewerSyncer = syncer

			f.reconcile(t)

			require.Len(t, syncer.calls, 1, "a terminal session's reconcile must level the org-viewer tuple exactly once")
			assert.Equal(t, orgViewerLevel{ns: "default", name: "s1", enabled: tc.wantEnabled}, syncer.calls[0])
			assert.True(t, f.sandboxGone(t), "the sync must sit above the terminal reap without breaking it")
		})
	}
}

// A live session levels through the same single call site, before the class
// Valid gate — visibility is a plain declaration, not something validity
// vouches for, and a session parked on a not-yet-Valid class must still track
// flips.
func TestReconcile_LiveSession_LevelsArtifactOrgViewer(t *testing.T) {
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhasePending, time.Minute, -time.Hour)
	classWithArtifactVisibility(t, f.c, spiceboxv1alpha1.ArtifactVisibilityOrganization)
	syncer := &fakeOrgViewerSyncer{}
	f.r.OrgViewerSyncer = syncer

	f.reconcile(t)

	require.NotEmpty(t, syncer.calls, "a live session's reconcile must level the org-viewer tuple")
	assert.Equal(t, orgViewerLevel{ns: "default", name: "s1", enabled: true}, syncer.calls[0])
}

func TestReconcile_OrgViewerSyncerError_FailsTheReconcile(t *testing.T) {
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -time.Hour)
	classWithArtifactVisibility(t, f.c, spiceboxv1alpha1.ArtifactVisibilityOrganization)
	syncer := &fakeOrgViewerSyncer{err: assert.AnError}
	f.r.OrgViewerSyncer = syncer

	_, err := f.r.Reconcile(memory.WithSystemApproval(context.Background(), "test"),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "s1"}})
	require.ErrorIs(t, err, assert.AnError,
		"a level that did not land is either a missing grant or a live revocation — the reconcile must retry, not shrug")
}

// A nil syncer is the SpiceDB-disabled wiring; the sync must be a no-op, not a
// panic. Every other untagged reconcile test runs with the field unset, so
// this is asserted implicitly there too — this case just names the contract.
func TestReconcile_NilOrgViewerSyncer_IsNoOp(t *testing.T) {
	f := newReapFixture(t, spiceboxv1alpha1.AgentSessionPhaseSucceeded, time.Minute, -time.Hour)
	classWithArtifactVisibility(t, f.c, spiceboxv1alpha1.ArtifactVisibilityOrganization)
	f.reconcile(t) // must not panic and must not error
}
