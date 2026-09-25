package useridentity

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// TestPutToken_ValueChangeStampsRotationFingerprint proves the fix for the
// same-name / different-value replace gap: re-linking a credential with a NEW
// value changes the UserIdentity's rotation-fingerprint annotation, so the
// otherwise byte-identical UserIdentity write becomes a REAL change that fires
// the AgentSession UserIdentity watch (which re-projects the per-session Secret
// and emits a credential invalidation). A same-value re-paste leaves the
// fingerprint — and thus the object — unchanged, so no spurious watch event.
func TestPutToken_ValueChangeStampsRotationFingerprint(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t) // fake client with UserIdentity + Secret scheme
	req := PutTokenRequest{Subject: "user:alice@example.com", CredentialName: "github-pat", Token: "tok-1"}

	require.NoError(t, PutToken(ctx, c, req))
	uiName := NameForSubject(req.Subject)

	var ui1 spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui1))
	fp1 := ui1.Annotations[RotationFingerprintAnnotation]
	require.NotEmpty(t, fp1, "rotation fingerprint must be stamped on first link")

	// Same value again → fingerprint unchanged (nothing to propagate).
	require.NoError(t, PutToken(ctx, c, req))
	var uiSame spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &uiSame))
	assert.Equal(t, fp1, uiSame.Annotations[RotationFingerprintAnnotation],
		"same-value re-paste must not change the rotation fingerprint")

	// Different value, SAME name → fingerprint changes so the watch fires.
	req.Token = "tok-2"
	require.NoError(t, PutToken(ctx, c, req))
	var ui2 spiceboxv1alpha1.UserIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Name: uiName}, &ui2))
	assert.NotEqual(t, fp1, ui2.Annotations[RotationFingerprintAnnotation],
		"replacing the token value (same name) must change the rotation fingerprint")

	// The master Secret carries the new value.
	secName := MasterSecretName(uiName, req.CredentialName)
	var sec corev1.Secret
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: spiceboxv1alpha1.IdentitiesNamespace, Name: secName}, &sec))
	assert.Equal(t, []byte("tok-2"), sec.Data["token"])

	// Exactly one credential entry throughout (upsert, not append).
	require.Len(t, ui2.Spec.Credentials, 1)
}

// TestPutToken_RecordsTheAttestationAcrossRelinks covers the three states a
// re-link can leave the attestation in, in one sequence, because the states
// only mean anything relative to each other.
//
// The rule SetAttestation encodes is that "not attested" and "attested as
// nothing" are different. A re-link the provider could not check must not
// erase what an earlier verified one established — otherwise a single flaky
// probe silently unbinds an identity — while a re-link that DOES verify, to a
// different account, must move the record.
func TestPutToken_RecordsTheAttestationAcrossRelinks(t *testing.T) {
	ctx := context.Background()
	c := newOAuthTestClient(t)
	base := PutTokenRequest{Subject: "user:alice@example.com", CredentialName: "github-pat"}
	secKey := client.ObjectKey{
		Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		Name:      MasterSecretName(NameForSubject(base.Subject), base.CredentialName),
	}
	attestation := func(t *testing.T) (string, string, bool) {
		t.Helper()
		var sec corev1.Secret
		require.NoError(t, c.Get(ctx, secKey, &sec))
		return Attestation(&sec)
	}

	// 1. First link, verified: both halves land on a freshly-created Secret.
	first := base
	first.Token, first.ProviderID, first.SubjectID = "tok-1", "github-pat", "583231"
	require.NoError(t, PutToken(ctx, c, first))
	providerID, subjectID, ok := attestation(t)
	require.True(t, ok, "a verified first link must attest")
	assert.Equal(t, "github-pat", providerID)
	assert.Equal(t, "583231", subjectID)

	// 2. Re-link with no verdict: the earlier attestation stands. This is the
	// update branch, so it also proves the annotation is not clobbered by the
	// Secret rewrite.
	unverified := base
	unverified.Token = "tok-2"
	require.NoError(t, PutToken(ctx, c, unverified))
	providerID, subjectID, ok = attestation(t)
	require.True(t, ok, "an unverifiable re-link must not erase an earlier attestation")
	assert.Equal(t, "github-pat", providerID)
	assert.Equal(t, "583231", subjectID)

	// 3. Re-link verified as a DIFFERENT account: the record moves, because
	// the token now demonstrably belongs to someone else.
	moved := base
	moved.Token, moved.ProviderID, moved.SubjectID = "tok-3", "github-pat", "999001"
	require.NoError(t, PutToken(ctx, c, moved))
	_, subjectID, ok = attestation(t)
	require.True(t, ok)
	assert.Equal(t, "999001", subjectID, "a re-verified credential re-points the attestation")
}
