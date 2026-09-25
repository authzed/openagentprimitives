package toolcall

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func ownedToolCall(ns, ownerSession, bundle string) *spiceboxv1alpha1.ToolCall {
	return &spiceboxv1alpha1.ToolCall{
		ObjectMeta: metav1.ObjectMeta{
			Name: "tc-1", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: spiceboxv1alpha1.SchemeBuilder.GroupVersion.String(),
				Kind:       "AgentSession", Name: ownerSession, UID: "uid-1",
			}},
		},
		Spec: spiceboxv1alpha1.ToolCallSpec{Session: bundle, Tool: "git"},
	}
}

func bundleFor(ns, bundle, parentSession string) *spiceboxv1alpha1.SpiceboxSession {
	s := &spiceboxv1alpha1.SpiceboxSession{
		ObjectMeta: metav1.ObjectMeta{Name: bundle, Namespace: ns},
	}
	if parentSession != "" {
		s.Labels = map[string]string{agentSessionLabel: parentSession}
	}
	return s
}

// spec.session and the ownerReference are BOTH written by the creator, and
// nothing required them to agree. The controller then resolves the sandbox to
// exec into, and the AgentSession whose use_token grant it consults, from
// spec.session — so a ToolCall owned by one session but naming another's
// bundle ran in that other session's sandbox, authorized by that other
// session's own grant.
//
// This is the controller-side half of the binding the admission webhook also
// enforces. It exists separately so a webhook that is down, unregistered, or
// bypassed is not a bypass of the rule.
func TestValidateSessionBinding_RefusesABundleBelongingToAnotherSession(t *testing.T) {
	tc := ownedToolCall("ns1", "attacker", "victim-bundle")
	bundle := bundleFor("ns1", "victim-bundle", "victim")

	err := validateSessionBinding(tc, bundle)
	require.Error(t, err, "a ToolCall owned by one session may not name another session's bundle")
	assert.Contains(t, err.Error(), "victim")
	assert.Contains(t, err.Error(), "attacker")
}

// The ordinary case: the bundle's parent label names the same session the
// ToolCall is owned by.
func TestValidateSessionBinding_AcceptsTheSessionsOwnBundle(t *testing.T) {
	tc := ownedToolCall("ns1", "sess", "sess-bundle")
	bundle := bundleFor("ns1", "sess-bundle", "sess")

	require.NoError(t, validateSessionBinding(tc, bundle))
}

// A bundle with no parent label cannot be shown to belong to the owning
// session, so it must be refused rather than assumed. The label is written by
// the operator, so its absence is a platform bug — and reading "unknown" as
// "fine" is how the original defect worked.
func TestValidateSessionBinding_FailsClosedOnAnUnlabelledBundle(t *testing.T) {
	tc := ownedToolCall("ns1", "sess", "sess-bundle")
	bundle := bundleFor("ns1", "sess-bundle", "")

	err := validateSessionBinding(tc, bundle)
	require.Error(t, err, "an unlabelled bundle proves nothing and must not be treated as belonging to the caller")
}

// A ToolCall with no AgentSession ownerReference never reaches here in
// production — admission refuses it — but the check must not read a missing
// owner as a match against a missing label.
func TestValidateSessionBinding_FailsClosedWithNoOwnerReference(t *testing.T) {
	tc := ownedToolCall("ns1", "sess", "sess-bundle")
	tc.OwnerReferences = nil
	bundle := bundleFor("ns1", "sess-bundle", "")

	require.Error(t, validateSessionBinding(tc, bundle),
		"absent owner and absent label must not cancel out into an allow")
}
