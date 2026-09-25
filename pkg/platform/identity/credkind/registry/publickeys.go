package registry

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// PublicSecretKeysFor answers "which data keys of this credential's backing
// Secret are PUBLIC identifiers rather than secret material" through the
// registry — SecretNameFor's neighbour, dispatching the same way for the same
// reason.
//
// It exists because a consumer holding a Secret's bytes cannot tell the two
// apart. "This value came out of a Secret" is provenance; it is not
// sensitivity, and a credential bundle routinely mixes both. Treating the whole
// Secret as sensitive is the safe default and the wrong ANSWER: it makes a
// public identifier that is inherent to a payload — a GitHub App installation
// id, which GitHub itself puts in the webhook body — unrepresentable, forever,
// for every user of that credential type. The type that defines the Secret's
// shape is the only place that knows, so the question is asked there.
//
// # Fail closed
//
// Every outcome that is not an explicit declaration is "all of it is secret",
// and a caller MUST treat them identically:
//
//   - An UNREGISTERED type returns an error — the same wiring bug SecretNameFor
//     reports, and the same remedy (a missing blank import of the type's kind
//     package). A caller decides whether to skip the one credential or fail the
//     operation, but must not read the empty slice as "nothing is secret".
//   - A registered type that declares nothing returns (nil, nil). This is the
//     answer for three of the four types shipped today and the answer any new
//     type gets for free.
//
// The asymmetry with SecretNameFor is deliberate: there, an empty name is a
// real answer ("this type stores nothing"). Here an empty list and a failure to
// ask look the same to a consumer, so both must be safe.
func PublicSecretKeysFor(cred spiceboxv1alpha1.AgentCredential) ([]string, error) {
	k, err := Get(cred.Type)
	if err != nil {
		return nil, err
	}
	return k.PublicSecretKeys(cred), nil
}
