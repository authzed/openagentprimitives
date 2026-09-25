package registry_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry/crdenumtest"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/github"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

// TestChannelKindEnumMatchesRegistry pins the shipped CRD's spec.kind enum
// against the kinds actually registered in a binary that imports them all.
// The two are separate hand-maintained facts today: a kind can register
// itself, satisfy every existing test, and still be rejected by the API
// server because nothing added it to the enum — and a value can linger in the
// enum after its package is gone. Set EQUALITY, not containment, is what
// catches the second case.
func TestChannelKindEnumMatchesRegistry(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t) // walks manifests.Install
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	registered := make([]string, 0, len(registry.All()))
	for _, k := range registry.All() {
		registered = append(registered, k.Name())
	}
	require.NotEmpty(t, registered, "no channel kinds registered — a blank import is missing from this test package")

	assert.ElementsMatch(t, registered, enum,
		"Channel.spec.kind's CRD enum and the channel-kind registry disagree; "+
			"update the +kubebuilder:validation:Enum marker and run `mage gen:api` then `mage manifests`")
}

// TestChannelKindEnumIsNotStale asserts the retired "builtin" spelling —
// renamed to "browser" — is gone from the shipped enum. Redundant with
// TestChannelKindEnumMatchesRegistry by construction (the registry has no
// "builtin" kind, so set equality already forbids it in the enum); kept
// anyway because its failure line names the specific regression rather than
// a generic set mismatch.
func TestChannelKindEnumIsNotStale(t *testing.T) {
	enum := crdenumtest.ChannelKindEnum(t)
	require.NotEmpty(t, enum, "spec.kind enum not found in the embedded install bundle")

	assert.NotContains(t, enum, "builtin",
		`"builtin" was renamed to "browser"; it must not linger in the shipped CRD enum`)
}

// TestChannelRoleEnumMatchesAllChannelRoles pins v1alpha1.AllChannelRoles() —
// the Go list every role-enumerating caller reads (each kind's
// SupportedRoles, oap/channelplan's "is this a real role" check) — against
// spec.role's enum in the shipped CRD.
//
// The two are separate hand-maintained facts, the same shape as
// TestChannelKindEnumMatchesRegistry above. AllChannelRoles is built from the
// ChannelRole* constants, so renaming a value's spelling follows
// automatically; ADDING a role does not. A new role reaching the
// +kubebuilder:validation:Enum marker and the constants but not this function
// would be accepted by the apiserver and rejected by `oap agent lint` as "not
// one of input|output|both|monitoring", and no test would have said so.
//
// Set EQUALITY, not containment, so a value retired from the enum but left in
// the Go list is caught too.
func TestChannelRoleEnumMatchesAllChannelRoles(t *testing.T) {
	enum := crdenumtest.ChannelRoleEnum(t)
	require.NotEmpty(t, enum, "spec.role enum not found in the embedded install bundle")

	assert.ElementsMatch(t, spiceboxv1alpha1.AllChannelRoles(), enum,
		"Channel.spec.role's CRD enum and v1alpha1.AllChannelRoles() disagree; "+
			"update the +kubebuilder:validation:Enum marker and the ChannelRole constants together, "+
			"then run `mage gen:api` and `mage manifests`")
}
