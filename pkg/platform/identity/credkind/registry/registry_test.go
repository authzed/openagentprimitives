package registry

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/authkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// fakeKind is a minimal credkind.Kind. Every method is present so that a
// change to the interface breaks this file at compile time rather than
// silently leaving a registry test that no longer exercises the contract.
type fakeKind struct{ typ string }

var _ credkind.Kind = fakeKind{}

func (f fakeKind) Type() string      { return f.typ }
func (fakeKind) Minted() bool        { return false }
func (fakeKind) NeedsRefresh() bool  { return false }
func (fakeKind) DisplayName() string { return "Fake" }

func (fakeKind) ValidOn() []credkind.Scope { return []credkind.Scope{credkind.ScopeAgentIdentity} }

func (fakeKind) ValidateSpec(spiceboxv1alpha1.AgentCredential) error { return nil }

func (fakeKind) SecretRef(spiceboxv1alpha1.AgentCredential) *credkind.SecretRef { return nil }

func (fakeKind) SecretRefPath() []string { return nil }

// HasBlock: a test kind owns no AgentCredential union block.
func (fakeKind) HasBlock(spiceboxv1alpha1.AgentCredential) bool { return false }

func (fakeKind) BuildCredential(string, string, string) (spiceboxv1alpha1.AgentCredential, error) {
	return spiceboxv1alpha1.AgentCredential{}, nil
}

func (fakeKind) Resolve(context.Context, credkind.Deps, spiceboxv1alpha1.CredentialSource) (authkind.ResolvedCredential, time.Time, error) {
	return authkind.ResolvedCredential{}, time.Time{}, nil
}

func (fakeKind) ReadStoredValue(context.Context, client.Reader, string, spiceboxv1alpha1.AgentCredential) (sensitive.SensitiveValue, error) {
	return sensitive.SensitiveValue{}, nil
}

func (fakeKind) Projectable() bool { return false }

func (fakeKind) PublicSecretKeys(spiceboxv1alpha1.AgentCredential) []string   { return nil }
func (fakeKind) RequiredSecretKeys(spiceboxv1alpha1.AgentCredential) []string { return nil }

// snapshotSelf saves this test binary's current registry contents and
// restores exactly that snapshot on cleanup, so a mid-test Reset() (below,
// to get a controlled empty/single-fake-kind registry) does not leave the
// registry empty for whatever test runs next in this binary — the same bug
// class as three Plan 1 guards going vacuous (AGENTS.md, "Test conventions").
//
// This inlines registrytest.Snapshot's logic rather than importing it:
// registrytest imports this package (registry), and this file is compiled
// as part of package registry itself, not an external _test package, so
// importing it here would be a cycle.
func snapshotSelf(t *testing.T) {
	t.Helper()
	saved := All()
	t.Cleanup(func() {
		Reset()
		for _, k := range saved {
			Register(k)
		}
	})
}

func TestGet_UnknownAndEmptyKeysFailClosed(t *testing.T) {
	snapshotSelf(t)
	Reset()

	_, err := Get("")
	require.Error(t, err, "empty key must error, never silently fall back")
	assert.Contains(t, err.Error(), "empty credential type")

	_, err = Get("nosuchtype")
	require.Error(t, err, "unknown key must error")
	assert.Contains(t, err.Error(), "nosuchtype")
}

func TestRegisterThenGet_ReturnsTheKind(t *testing.T) {
	snapshotSelf(t)
	Reset()

	Register(fakeKind{typ: "demo"})

	k, err := Get("demo")
	require.NoError(t, err, "a registered kind must resolve")
	assert.Equal(t, "demo", k.Type())
	assert.Equal(t, []string{"demo"}, Keys())
}

func TestGet_ErrorNamesTheRegisteredAlternatives(t *testing.T) {
	snapshotSelf(t)
	Reset()
	Register(fakeKind{typ: "demo"})

	_, err := Get("typo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demo",
		"the error must list what IS registered so a typo is diagnosable without source-diving")
}
