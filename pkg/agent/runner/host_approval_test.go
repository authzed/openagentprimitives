package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// standingRepoRecord is a phase that reads and pushes one named repository,
// the shape plangate.PhaseAuthorityRecord freezes for a standing plan.
func standingRepoRecord() plangateaudit.Content {
	return plangateaudit.Content{
		PhaseIndex: new(int32(0)), PhaseKey: "key-ship", MaxCount: 1,
		Ceiling:  []string{"perm:push:github_repo", "perm:read:github_repo"},
		Slots:    []string{"github_repo"},
		SlotRefs: []plangateaudit.SlotRef{{Type: "github_repo", ID: "acme/app", Standing: spiceboxv1alpha1.StandingRequired}},
	}
}

// standingBindHost is a runner whose class marks github_repo standing
// `required` (read + push), with the approver holding exactly held.
func standingBindHost(t *testing.T, held ...plangateaudit.SlotRef) (*runnerHost, *memory.Local, *movingBinder) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	h := planGateHost(t, &channelevents.Envelope{})
	h.l.Mem = mem
	binder := &movingBinder{pinned: map[string]string{}}
	h.l.SlotBinder = binder
	h.l.PlanGateSlotPermissionSets = map[string][]string{"github_repo": {"read", "push"}}
	h.l.PlanGateSlotStanding = map[string]string{"github_repo": spiceboxv1alpha1.StandingRequired}
	installInstanceStanding(t, h, held...)
	return h, mem, binder
}

// withoutExpiry drops the wall-clock expiry so two binds are comparable.
func withoutExpiry(rels []authz.Relation) []authz.Relation {
	out := make([]authz.Relation, 0, len(rels))
	for _, r := range rels {
		r.ExpiresAt = time.Time{}
		out = append(out, r)
	}
	return out
}

var standingApprover = channelevents.ExternalIdentity{Kind: identity.KindIdP, ExternalID: "owner@example.com", Email: "owner@example.com"}

// The shared golden: a standing approval binds EXACTLY what a human's click
// binds for the same record and approver — the same grants, the same session
// scope — because both run one delegation-and-bind path.
func TestBindApprovedPhase_writesWhatTheClickPathWrites(t *testing.T) {
	repo := plangateaudit.SlotRef{Type: "github_repo", ID: "acme/app"}

	click, clickMem, clickBinder := standingBindHost(t, repo)
	ask := planPhaseAsk()
	ask.CoalesceKey = "plan:" + planPhaseDigest
	ask.Payload["covered"] = []plangateaudit.Content{standingRepoRecord()}
	recs := driveDecisionAs(t, click, clickMem, ask, true, standingApprover)
	approved := onlyEventOf(t, recs, plangateaudit.EventPhaseApproved)
	require.NotEmpty(t, clickBinder.wrote, "the click bound the repository")

	standing, standingMem, standingBinder := standingBindHost(t, repo)
	canonical, err := standingApprover.Principal().Canonical()
	require.NoError(t, err)
	rec := standingRepoRecord()
	rec.Event, rec.PlanDigest, rec.Provenance = plangateaudit.EventPhaseApproved, planPhaseDigest, "standing_authorization"
	require.NoError(t, standing.bindApprovedPhase(context.Background(), rec, identity.RawSubject("user:"+canonical.String())))

	assert.Equal(t, withoutExpiry(clickBinder.wrote), withoutExpiry(standingBinder.wrote), "the same grants")
	assert.Equal(t, scopedIDs(t, clickMem, "github_repo"), scopedIDs(t, standingMem, "github_repo"), "the same session scope")
	assert.Equal(t, approved.SlotRefs, rec.SlotRefs, "the click granted every ref the standing record names")
	for _, perm := range []string{"read", "push"} {
		assert.True(t, standingBinder.wroteGrant("github_repo", "acme/app", perm), "granted %s", perm)
	}
}

// Defense in depth: the runner re-asks SpiceDB rather than trusting the
// server's answer, and a standing approval binds all of the phase or fails —
// it never narrows to a subset the way a click does.
func TestBindApprovedPhase_failsRatherThanBindingLess(t *testing.T) {
	approver := identity.RawSubject("user:approver")
	cases := []struct {
		name    string
		arrange func(h *runnerHost)
	}{
		{name: "approver holds nothing on the repository: error, nothing written", arrange: func(h *runnerHost) {
			installInstanceStanding(t, h)
		}},
		{name: "approver lost push: error, nothing written", arrange: func(h *runnerHost) {
			h.l.SpiceDBHasOnResource = func(_ context.Context, _, _, perm string, _ identity.CanonicalUserID) (bool, error) {
				return perm == "read", nil
			}
		}},
		{name: "standing lookup errors: error, nothing written", arrange: func(h *runnerHost) {
			h.l.SpiceDBHasOnResource = func(context.Context, string, string, string, identity.CanonicalUserID) (bool, error) {
				return false, errors.New("spicedb unavailable")
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, mem, binder := standingBindHost(t, plangateaudit.SlotRef{Type: "github_repo", ID: "acme/app"})
			tc.arrange(h)
			rec := standingRepoRecord()
			rec.Event, rec.PlanDigest = plangateaudit.EventPhaseApproved, planPhaseDigest
			require.Error(t, h.bindApprovedPhase(context.Background(), rec, approver))
			assert.Empty(t, binder.wrote)
			assert.Empty(t, scopedIDs(t, mem, "github_repo"))
		})
	}
}

// A grant write that fails is a bind error the gate turns into a denial.
func TestBindApprovedPhase_aFailedGrantWriteIsAnError(t *testing.T) {
	h, _, _ := standingBindHost(t, plangateaudit.SlotRef{Type: "github_repo", ID: "acme/app"})
	h.l.SlotBinder = &moveTrackingBinder{pinnedTo: "acme/other"}
	rec := standingRepoRecord()
	rec.Event, rec.PlanDigest = plangateaudit.EventPhaseApproved, planPhaseDigest
	assert.Error(t, h.bindApprovedPhase(context.Background(), rec, identity.RawSubject("user:approver")),
		"a slot pinned elsewhere refuses the grant, and the standing approval must not pass as bound")
}
