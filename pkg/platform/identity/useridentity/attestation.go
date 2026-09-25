// attestation.go records WHICH provider-side account a stored credential
// authenticated as, so the UserIdentity reconciler can bind that account to
// this catalog's human without performing network I/O of its own.
//
// The value is an OBSERVATION made at link time, when the credential was live-
// verified. It is metadata about the credential rather than credential material,
// so it lives in annotations and not in Data.
//
// It is a pure function of the credential: no timestamp, no counter, no
// generation. Re-applying it is byte-identical, which is what keeps a re-link
// from churning field ownership on a Secret the operator also writes.

package useridentity

import corev1 "k8s.io/api/core/v1"

// AttestedSubjectAnnotation holds the provider's STABLE id for the account the
// credential authenticated as (for GitHub, the numeric user id as a string —
// never the login, which is mutable and reclaimable).
const AttestedSubjectAnnotation = "useridentity.agentprimitives.authzed.com/attested-subject"

// AttestedProviderAnnotation holds the provider id the attestation came from, so
// a reader never has to infer which namespace the subject belongs to.
const AttestedProviderAnnotation = "useridentity.agentprimitives.authzed.com/attested-provider"

// SetAttestation records that sec's credential authenticated as subjectID at
// providerID.
//
// An empty providerID or subjectID writes nothing and clears nothing: absent and
// "attested as nothing" are different states, and a verification that could not
// extract an id must not erase an earlier one that could.
func SetAttestation(sec *corev1.Secret, providerID, subjectID string) {
	if sec == nil || providerID == "" || subjectID == "" {
		return
	}
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Annotations[AttestedProviderAnnotation] = providerID
	sec.Annotations[AttestedSubjectAnnotation] = subjectID
}

// Attestation reads back what SetAttestation wrote. ok is false unless both
// halves are present, so a partially-written pair is treated as absent rather
// than as a claim about an unknown provider.
func Attestation(sec *corev1.Secret) (providerID, subjectID string, ok bool) {
	if sec == nil {
		return "", "", false
	}
	providerID = sec.Annotations[AttestedProviderAnnotation]
	subjectID = sec.Annotations[AttestedSubjectAnnotation]
	if providerID == "" || subjectID == "" {
		return "", "", false
	}
	return providerID, subjectID, true
}
