package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/externaltoken"
)

// fakeTokenGranter records every write/delete applyGrantDiff issues so a test
// can assert the exact diff (and, for the idempotency case, the ABSENCE of any
// write). listErr, when set, is returned from ListAuthorizedTokens.
type fakeTokenGranter struct {
	current []externaltoken.AuthorizedTokenGrant
	listErr error

	touchedCaveated []string // "credID:valueHash"
	touchedIdentity []string // credID
	deleted         []string // credID
}

func (f *fakeTokenGranter) TouchAuthorizedToken(_ context.Context, _, _, credID, authorizedValueHash string) error {
	f.touchedCaveated = append(f.touchedCaveated, credID+":"+authorizedValueHash)
	return nil
}

func (f *fakeTokenGranter) TouchAuthorizedTokenIdentity(_ context.Context, _, _, credID string) error {
	f.touchedIdentity = append(f.touchedIdentity, credID)
	return nil
}

func (f *fakeTokenGranter) DeleteAuthorizedToken(_ context.Context, _, _, credID string) error {
	f.deleted = append(f.deleted, credID)
	return nil
}

func (f *fakeTokenGranter) ListAuthorizedTokens(_ context.Context, _, _ string) ([]externaltoken.AuthorizedTokenGrant, error) {
	return f.current, f.listErr
}

func TestReconcileCredentialGrants_Diff(t *testing.T) {
	fake := &fakeTokenGranter{
		current: []externaltoken.AuthorizedTokenGrant{
			{CredID: "stale", AuthorizedValueHash: "x"},       // should be deleted
			{CredID: "keep", AuthorizedValueHash: "old-hash"}, // should be TOUCHed (hash changed)
		},
	}
	desired := []desiredGrant{
		{CredID: "keep", ValueHash: "new-hash", IdentityOnly: false}, // hash changed → touch
		{CredID: "new", ValueHash: "h", IdentityOnly: false},         // missing → touch
		{CredID: "fed", ValueHash: "", IdentityOnly: true},           // identity-only → touch identity
	}
	err := applyGrantDiff(context.Background(), fake, "ns1", "sess", desired)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"keep:new-hash", "new:h"}, fake.touchedCaveated)
	assert.Equal(t, []string{"fed"}, fake.touchedIdentity)
	assert.Equal(t, []string{"stale"}, fake.deleted)
}

func TestReconcileCredentialGrants_Idempotent(t *testing.T) {
	fake := &fakeTokenGranter{current: []externaltoken.AuthorizedTokenGrant{{CredID: "keep", AuthorizedValueHash: "h"}}}
	err := applyGrantDiff(context.Background(), fake, "ns1", "sess", []desiredGrant{{CredID: "keep", ValueHash: "h"}})
	require.NoError(t, err)
	assert.Empty(t, fake.touchedCaveated, "unchanged grant must NOT be re-touched")
	assert.Empty(t, fake.touchedIdentity, "unchanged grant must NOT be re-touched")
	assert.Empty(t, fake.deleted)
}

// A federated (identity-only) grant already present must NOT be re-touched: an
// existing grant with an empty AuthorizedValueHash matches a desired federated
// grant, so the diff is a no-op.
func TestReconcileCredentialGrants_IdempotentFederated(t *testing.T) {
	fake := &fakeTokenGranter{current: []externaltoken.AuthorizedTokenGrant{{CredID: "fed", AuthorizedValueHash: ""}}}
	err := applyGrantDiff(context.Background(), fake, "ns1", "sess", []desiredGrant{{CredID: "fed", IdentityOnly: true}})
	require.NoError(t, err)
	assert.Empty(t, fake.touchedIdentity, "unchanged identity-only grant must NOT be re-touched")
	assert.Empty(t, fake.touchedCaveated)
	assert.Empty(t, fake.deleted)
}

// A ListAuthorizedTokens failure is fatal: the diff cannot be computed against
// unknown current state, so applyGrantDiff must surface the error rather than
// treating "no current grants" as authoritative and deleting/re-touching blind.
func TestReconcileCredentialGrants_ListErrorIsFatal(t *testing.T) {
	fake := &fakeTokenGranter{listErr: errors.New("spicedb unreachable")}
	err := applyGrantDiff(context.Background(), fake, "ns1", "sess", []desiredGrant{{CredID: "keep", ValueHash: "h"}})
	require.Error(t, err)
	assert.Empty(t, fake.touchedCaveated)
	assert.Empty(t, fake.deleted)
}
