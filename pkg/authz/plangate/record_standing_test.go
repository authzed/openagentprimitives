package plangate

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/permsurface"
)

// PhaseAuthorityRecord projects the phase's slot requests into SlotRefs; each
// one must carry the standing that gave (or will give) its approval its
// authority, or the log cannot tell a vouch from a delegation after the fact.
func TestPhaseAuthorityRecord_carriesStandingIntoSlotRefs(t *testing.T) {
	p := Plan{Phases: []Phase{{
		Permissions: []permsurface.Handle{handle(t, "push", "git_repo")},
		Slots: []Slot{
			{Type: "git_repo", ID: "acme/app"},
			{Type: "crm_company", ID: "4210"},
		},
	}}}
	standing := map[string]string{
		"git_repo":    spiceboxv1alpha1.StandingSessionOnly,
		"crm_company": spiceboxv1alpha1.StandingRequired,
	}

	rec := PhaseAuthorityRecord(p, 0, standing)

	byType := map[string]string{}
	for _, ref := range rec.SlotRefs {
		byType[ref.Type] = ref.Standing
	}
	assert.Equal(t, spiceboxv1alpha1.StandingSessionOnly, byType["git_repo"])
	assert.Equal(t, spiceboxv1alpha1.StandingRequired, byType["crm_company"])
}

// A type absent from the standing map must record an EMPTY Standing, not a
// guessed one — the reader (the card, `ap audit verify`) is what applies the
// "absent means session-only" default, and baking a guess into the record
// would make replaying an old log (recorded before the type was resolved)
// indistinguishable from one that genuinely never resolved it.
func TestPhaseAuthorityRecord_unmappedTypeRecordsEmptyStanding(t *testing.T) {
	p := Plan{Phases: []Phase{{
		Permissions: []permsurface.Handle{handle(t, "push", "git_repo")},
		Slots:       []Slot{{Type: "git_repo", ID: "acme/app"}},
	}}}

	rec := PhaseAuthorityRecord(p, 0, nil)

	assert.Len(t, rec.SlotRefs, 1)
	assert.Equal(t, "", rec.SlotRefs[0].Standing)
}
