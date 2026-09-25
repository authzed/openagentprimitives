package agentui

// White-box tests for validateDeclaration, in package agentui rather than
// agentui_test. validateDeclaration's marshal-failure arm (a malformed
// slot.Default.Raw) cannot be exercised end-to-end through Reconcile: a
// malformed *apiextensionsv1.JSON anywhere in spec also breaks
// client.MergeFrom's own JSON-marshal-and-diff step when writeStatusIfChanged
// computes the status patch — client.MergeFrom(prior).Data(obj) marshals the
// WHOLE object, spec included, to compute the diff, not merely status. That
// makes Reconcile itself return a hard Go error before any status is
// persisted, which is a real (if narrow) property of using client.MergeFrom
// for status-only patches on an object with an arbitrary-JSON field
// elsewhere — worth a maintainer's attention some day, but out of this
// task's scope and not what this test is checking. validateDeclaration is
// therefore tested directly as a pure function, sidestepping the fake
// client, the patch mechanism, and this second-order issue entirely.

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestValidateDeclarationRejectsAMalformedDefaultThatFailsToMarshal(t *testing.T) {
	// slot.Default.Raw is user-supplied and is not re-validated as JSON
	// before validateDeclaration re-marshals it inside a wireDeclaration:
	// json.RawMessage.MarshalJSON returns the bytes verbatim, and the outer
	// json.Marshal call rejects them if they are not valid JSON. That
	// failure must surface as a diagnosable *ValidationError, not a panic
	// and not a silently-accepted default.
	spec := spiceboxv1alpha1.AgentUISpec{
		Slots: []spiceboxv1alpha1.AgentUISlot{{
			Name:    "root",
			Default: &apiextensionsv1.JSON{Raw: []byte("{not json")},
		}},
	}

	_, verr := validateDeclaration(spec, nil)

	require.NotNil(t, verr, "a malformed Default must be rejected, not silently accepted")
	assert.Contains(t, verr.Reason, "marshal wire declaration")
}
