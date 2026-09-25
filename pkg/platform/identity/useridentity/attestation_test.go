package useridentity_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func TestSetAttestation_RecordsProviderAndSubject(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred"}}

	useridentity.SetAttestation(sec, "github-pat", "583231")

	assert.Equal(t, "github-pat", sec.Annotations[useridentity.AttestedProviderAnnotation])
	assert.Equal(t, "583231", sec.Annotations[useridentity.AttestedSubjectAnnotation])
}

// A pure function of its inputs: re-applying the same attestation must be a
// byte-identical no-op, or every re-link churns field ownership on a Secret the
// operator also writes.
func TestSetAttestation_IsIdempotent(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred"}}

	useridentity.SetAttestation(sec, "github-pat", "583231")
	first := map[string]string{}
	for k, v := range sec.Annotations {
		first[k] = v
	}
	useridentity.SetAttestation(sec, "github-pat", "583231")

	assert.Equal(t, first, sec.Annotations, "a repeated attestation changes nothing")
}

// An empty id must not write an empty annotation: absent and "attested as
// nothing" are different states, and Task 4 skips on absence.
func TestSetAttestation_EmptyIDWritesNothing(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred"}}

	useridentity.SetAttestation(sec, "github-pat", "")

	assert.NotContains(t, sec.Annotations, useridentity.AttestedSubjectAnnotation)
	assert.NotContains(t, sec.Annotations, useridentity.AttestedProviderAnnotation)
}

// A previously-attested credential replaced by one for a DIFFERENT account must
// not keep the stale claim. Overwriting is correct; leaving both is not.
func TestSetAttestation_OverwritesAPriorAttestation(t *testing.T) {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred"}}

	useridentity.SetAttestation(sec, "github-pat", "583231")
	useridentity.SetAttestation(sec, "github-pat", "999999")

	assert.Equal(t, "999999", sec.Annotations[useridentity.AttestedSubjectAnnotation])
}

// TestSetAttestation_OverwritesAPriorAttestation_PersistsThroughStore is the
// round-trip counterpart to the in-memory test above: it proves the overwrite
// survives setup.Store's real Get → mutate → MergeFrom path against a fake
// client, not just two calls against the same in-memory struct. A re-link for
// a DIFFERENT account must replace the persisted annotation, not accumulate
// alongside it or leave the stale one in place — that is where the moving
// parts (a fetched Secret, a patch computed from a DeepCopy taken before the
// mutation) could plausibly get it wrong even though the pure function can't.
func TestSetAttestation_OverwritesAPriorAttestation_PersistsThroughStore(t *testing.T) {
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	require.NoError(t, corev1.AddToScheme(s))
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&spiceboxv1alpha1.AgentIdentity{}).
		Build()
	ctx := context.Background()

	req := setup.StoreRequest{
		Namespace:    "default",
		IdentityName: "my-bot",
		Requirement: authkind.CredentialRequirement{
			SuggestedName: "gh-pat",
			ProviderID:    "github-pat",
		},
		Value:     builtins.StoreValue{Bearer: "ghp_first"},
		SubjectID: "583231",
	}
	require.NoError(t, setup.Store(ctx, c, req))

	// Re-link: same credential name, a different account's token.
	req.Value = builtins.StoreValue{Bearer: "ghp_second"}
	req.SubjectID = "999999"
	require.NoError(t, setup.Store(ctx, c, req))

	var ai spiceboxv1alpha1.AgentIdentity
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "my-bot"}, &ai))
	require.Len(t, ai.Spec.Credentials, 1, "re-link must not duplicate the credential")
	require.NotNil(t, ai.Spec.Credentials[0].Static)

	var sec corev1.Secret
	require.NoError(t, c.Get(ctx,
		client.ObjectKey{Namespace: "default", Name: ai.Spec.Credentials[0].Static.SecretRef.Name}, &sec))
	assert.Equal(t, "999999", sec.Annotations[useridentity.AttestedSubjectAnnotation],
		"the persisted Secret must carry the new subject, not the stale one")
	assert.Equal(t, "github-pat", sec.Annotations[useridentity.AttestedProviderAnnotation])
}

// TestAttestation_RequiresBothHalves covers Attestation's read side: ok is
// true only when both annotations are present, so a partially-written pair —
// a hand-edited Secret, or a future bug that writes one half — reads back as
// absent rather than as a claim about an unknown provider.
func TestAttestation_RequiresBothHalves(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		wantOK      bool
	}{
		{
			name: "both present: reads back",
			annotations: map[string]string{
				useridentity.AttestedProviderAnnotation: "github-pat",
				useridentity.AttestedSubjectAnnotation:  "583231",
			},
			wantOK: true,
		},
		{
			name: "subject missing: reads back absent",
			annotations: map[string]string{
				useridentity.AttestedProviderAnnotation: "github-pat",
			},
			wantOK: false,
		},
		{
			name:        "neither present: reads back absent",
			annotations: map[string]string{},
			wantOK:      false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cred", Annotations: tc.annotations}}

			providerID, subjectID, ok := useridentity.Attestation(sec)

			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, "github-pat", providerID)
				assert.Equal(t, "583231", subjectID)
			} else {
				assert.Empty(t, providerID)
				assert.Empty(t, subjectID)
			}
		})
	}
}
