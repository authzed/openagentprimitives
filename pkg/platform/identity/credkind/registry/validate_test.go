package registry_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind"
	// Registers every credkind in THIS test binary. A test binary is its own
	// process; another package's blank import does not leak in, and every test
	// below asserts the registry is non-empty so a dropped import fails loudly
	// instead of looping over zero kinds.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
)

// credentialWith builds a credential declaring declaredType and carrying a
// populated union block for each name in blocks.
//
// Built through unstructured rather than by naming Go fields, because each
// type's block is the AgentCredential field whose json tag IS the type name —
// the same identity B4's SecretRefPath relies on. That is what lets this
// table enumerate every (declared, foreign) PAIR from the registry instead of
// transcribing 4×4 literals that a fifth kind would silently not extend.
func credentialWith(t *testing.T, declaredType string, blocks ...string) spiceboxv1alpha1.AgentCredential {
	t.Helper()
	obj := map[string]any{"name": "c", "type": declaredType}
	for _, b := range blocks {
		require.NoError(t, unstructured.SetNestedMap(obj, map[string]any{}, b),
			"planting the %q block", b)
	}
	var cred spiceboxv1alpha1.AgentCredential
	require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj, &cred))
	return cred
}

// blockOwners returns the registered kinds that own a union block on
// AgentCredential.
//
// Ownership is DERIVED FROM THE CRD TYPE — a field whose json tag is the
// registry type name — and each owner's HasBlock is then REQUIRED to agree.
// An earlier version filtered on HasBlock instead, which was self-defeating in
// the exact direction that matters: a kind answering false always would simply
// drop out of the sweep, taking its three foreign-block rows with it, and the
// table would narrow from twelve pairs to nine and stay green. Deriving the
// set independently is what turns that into a failure.
func blockOwners(t *testing.T) []credkind.Kind {
	t.Helper()
	credType := reflect.TypeOf(spiceboxv1alpha1.AgentCredential{})
	var owners []credkind.Kind
	for _, k := range registry.All() {
		if !hasJSONField(credType, k.Type()) {
			continue // no union block of its own (federated's store is not its own)
		}
		require.Truef(t, k.HasBlock(credentialWith(t, "unused", k.Type())),
			"AgentCredential carries a %q block but that kind's HasBlock does not report it; "+
				"every cross-kind rule reads HasBlock, so this kind's block would be invisible to all of them",
			k.Type())
		owners = append(owners, k)
	}
	return owners
}

// hasJSONField reports whether t has a struct field whose json tag's first
// component is name.
func hasJSONField(t reflect.Type, name string) bool {
	if t.Kind() != reflect.Struct {
		return false
	}
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if tag == name {
			return true
		}
	}
	return false
}

// TestValidateExclusive_RejectsEveryForeignBlockOnEveryType is the exhaustive
// pair sweep the old per-kind checks only approximated.
//
// Exclusivity used to be written INSIDE each kind as a list of its siblings,
// which is O(n^2) coupling: adding githubApp as a fourth type required editing
// static, oauth and federated to reject it. None of the three was edited, so
// all three accepted a credential declaring their own type while carrying a
// githubApp block. This table would have failed on exactly those three pairs.
//
// Both halves are derived from the registry, so a fifth kind is swept in every
// direction the day it registers — which is the whole point of moving the rule
// off the individual kinds.
func TestValidateExclusive_RejectsEveryForeignBlockOnEveryType(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds, "the registry must be populated or this sweep is vacuous")
	owners := blockOwners(t)
	require.NotEmpty(t, owners, "no kind owns a plantable union block; the sweep would assert nothing")

	var pairs int
	for _, declared := range kinds {
		for _, foreign := range owners {
			if foreign.Type() == declared.Type() {
				continue
			}
			pairs++
			t.Run(fmt.Sprintf("type=%s carrying a %s block: rejected", declared.Type(), foreign.Type()), func(t *testing.T) {
				cred := credentialWith(t, declared.Type(), declared.Type(), foreign.Type())
				err := registry.ValidateExclusive(cred)
				require.Errorf(t, err, "type=%s must reject a stray %s block", declared.Type(), foreign.Type())
				assert.Contains(t, err.Error(), foreign.Type(),
					"the message must name the offending block, or an operator cannot find it")
				assert.Contains(t, err.Error(), "credentials[c]",
					"the message is surfaced verbatim on a condition; it must say which credential")
			})
		}
	}
	// One row per (declared kind) x (foreign block owner). Four kinds, three
	// of which own a block, gives twelve today. Asserted against the derived
	// counts rather than a literal, so a fifth kind raises the floor by itself
	// instead of leaving the sweep quietly under-covering.
	require.Equal(t, len(kinds)*len(owners)-len(owners), pairs,
		"every (declared, foreign) pair must be swept")
}

// TestValidateExclusive_AcceptsOnlyItsOwnBlock is the non-vacuity control for
// the sweep above: a rule that rejected everything would pass every row there.
func TestValidateExclusive_AcceptsOnlyItsOwnBlock(t *testing.T) {
	kinds := registry.All()
	require.NotEmpty(t, kinds)

	for _, k := range kinds {
		t.Run(k.Type(), func(t *testing.T) {
			assert.NoError(t, registry.ValidateExclusive(credentialWith(t, k.Type(), k.Type())),
				"a credential carrying only its own block must be accepted")
			assert.NoError(t, registry.ValidateExclusive(credentialWith(t, k.Type())),
				"a missing own block is ValidateSpec's finding, not an exclusivity violation")
		})
	}
}

// TestValidateCredential_AsksAllFourQuestions pins the single entry point the
// AgentIdentity and UserIdentity reconcilers now share, one row per way it can
// refuse plus the accepting row.
func TestValidateCredential_AsksAllFourQuestions(t *testing.T) {
	require.NotEmpty(t, registry.All())

	staticCred := func() spiceboxv1alpha1.AgentCredential {
		return spiceboxv1alpha1.AgentCredential{
			Name: "c", Type: "static",
			Static: &spiceboxv1alpha1.StaticCredentialSource{
				SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "s", Key: "k"},
			},
		}
	}

	cases := []struct {
		name    string
		cred    spiceboxv1alpha1.AgentCredential
		scope   credkind.Scope
		wantErr string
	}{
		{
			name:    "unregistered type: refused naming the credential",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "not-a-kind"},
			scope:   credkind.ScopeAgentIdentity,
			wantErr: "credentials[c]",
		},
		{
			name:    "empty type: refused, never defaulted",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c"},
			scope:   credkind.ScopeAgentIdentity,
			wantErr: "credentials[c]",
		},
		{
			name: "federated on an AgentIdentity: refused by scope",
			cred: spiceboxv1alpha1.AgentCredential{Name: "c", Type: "federated",
				Federated: &spiceboxv1alpha1.FederatedCredentialSource{Resource: "r"}},
			scope:   credkind.ScopeAgentIdentity,
			wantErr: "is not valid here",
		},
		{
			name:    "own block malformed: ValidateSpec's message passes through",
			cred:    spiceboxv1alpha1.AgentCredential{Name: "c", Type: "static"},
			scope:   credkind.ScopeAgentIdentity,
			wantErr: "secretRef must set both name and key",
		},
		{
			name: "stray githubApp block on a static credential: refused",
			cred: func() spiceboxv1alpha1.AgentCredential {
				c := staticCred()
				c.GitHubApp = &spiceboxv1alpha1.GitHubAppCredentialSource{}
				return c
			}(),
			scope:   credkind.ScopeAgentIdentity,
			wantErr: "must not set the githubApp block",
		},
		{
			name:  "well-formed static on an AgentIdentity: accepted",
			cred:  staticCred(),
			scope: credkind.ScopeAgentIdentity,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := registry.ValidateCredential(tc.cred, tc.scope)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
