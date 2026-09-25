package permsurface

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
)

func TestResolve_findsLiveHandle(t *testing.T) {
	cs := []Candidate{permCandidate("gh_pr_view", "readonly", "read", "github_repo")}
	h, err := NewPermHandle("read", "github_repo")
	require.NoError(t, err)

	d, ok := Resolve(h, cs)
	require.True(t, ok)
	assert.Equal(t, authz.Readonly, d.StateImpact)
}

// A ceiling outliving the tool that justified it (revocation, toolkit removed)
// must resolve to inert — never to allow.
func TestResolve_missingHandleIsInert(t *testing.T) {
	h, err := NewPermHandle("write", "tracker_issue")
	require.NoError(t, err)

	d, ok := Resolve(h, []Candidate{permCandidate("gh_pr_view", "readonly", "read", "github_repo")})
	assert.False(t, ok, "a handle absent from the envelope must be inert")
	assert.True(t, d.Handle.IsZero())
}

func TestDigest_stableAcrossCandidateOrder(t *testing.T) {
	a := Enumerate([]Candidate{
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
		permCandidate("tracker_update", "readwrite", "write", "tracker_issue"),
	})
	b := Enumerate([]Candidate{
		permCandidate("tracker_update", "readwrite", "write", "tracker_issue"),
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
	})
	assert.Equal(t, Digest(a), Digest(b))
}

// Via is display-only. A cosmetic tool rename must NOT move the digest, or
// every rename would raise a spurious drift alarm on an approved plan.
func TestDigest_ignoresProvenanceOnlyChanges(t *testing.T) {
	before := Enumerate([]Candidate{permCandidate("gh_pr_view", "readonly", "read", "github_repo")})
	after := Enumerate([]Candidate{permCandidate("gh_view_pr", "readonly", "read", "github_repo")})
	assert.Equal(t, Digest(before), Digest(after))
}

// The digest MUST move when what is enforceable changes.
func TestDigest_changesWhenStateImpactEscalates(t *testing.T) {
	before := Enumerate([]Candidate{permCandidate("gh_pr_view", "readonly", "read", "github_repo")})
	after := Enumerate([]Candidate{permCandidate("gh_pr_view", "external", "read", "github_repo")})
	assert.NotEqual(t, Digest(before), Digest(after))
}

func TestDigest_changesWhenHandleAdded(t *testing.T) {
	before := Enumerate([]Candidate{permCandidate("gh_pr_view", "readonly", "read", "github_repo")})
	after := Enumerate([]Candidate{
		permCandidate("gh_pr_view", "readonly", "read", "github_repo"),
		permCandidate("tracker_update", "readwrite", "write", "tracker_issue"),
	})
	assert.NotEqual(t, Digest(before), Digest(after))
}

func TestDigest_emptySurfaceIsStable(t *testing.T) {
	assert.Equal(t, Digest(nil), Digest([]Descriptor{}))
}
