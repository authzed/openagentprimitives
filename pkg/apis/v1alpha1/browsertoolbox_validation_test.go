//go:build integration

// pkg/apis/v1alpha1/browsertoolbox_validation_test.go
//
// Integration test for the BrowserToolbox CEL validation added in Phase C Task
// C9: an EXPLICIT state=persistent requires access.identity, while an UNSET state
// is accepted in every shape (it is defaulted in code at the AgentSession
// snapshot, not at admission — C-R5). Runs against a real envtest apiserver (via
// pkg/controllers/testenv) so the assertion exercises the generated CRD's
// XValidation rule and the enum-but-not-defaulted State field directly, not a
// Go-level pre-check that could drift from what is installed.
package v1alpha1_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/testenv"
)

// newBrowserToolbox builds an allowlist BrowserToolbox over one host with the
// given identity (empty = none) and state (empty = unset), for exercising the
// persistent⇒identity CEL rule and the no-default State field in isolation.
func newBrowserToolbox(name, identity, state string) *v1alpha1.BrowserToolbox {
	access := v1alpha1.BrowserAccess{
		Mode:  "allowlist",
		Sites: []v1alpha1.BrowserSite{{Host: "example.com"}},
	}
	if identity != "" {
		access.Identity = identity
	}
	return &v1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.BrowserToolboxSpec{
			Access:      access,
			Interaction: "act",
			State:       state,
		},
	}
}

func TestBrowserToolboxStateCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	// (c) EXPLICIT persistent without identity: rejected by the CEL rule.
	t.Run("explicit persistent without identity: rejected", func(t *testing.T) {
		tb := newBrowserToolbox("btb-persist-no-id", "", v1alpha1.BrowserStatePersistent)
		err := env.Client.Create(ctx, tb)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
		assert.Contains(t, err.Error(), "a persistent browser requires access.identity")
	})

	// EXPLICIT persistent WITH identity: accepted.
	t.Run("explicit persistent with identity: accepted", func(t *testing.T) {
		tb := newBrowserToolbox("btb-persist-id", "joey-airlines", v1alpha1.BrowserStatePersistent)
		require.NoError(t, env.Client.Create(ctx, tb),
			"a persistent toolbox with a bound identity must be accepted")
	})

	// (d) EXPLICIT ephemeral WITH identity: honored as-is (no persistence rule).
	t.Run("explicit ephemeral with identity: accepted", func(t *testing.T) {
		tb := newBrowserToolbox("btb-ephemeral-id", "joey-airlines", v1alpha1.BrowserStateEphemeral)
		require.NoError(t, env.Client.Create(ctx, tb),
			"an explicit ephemeral toolbox must be accepted even with an identity")
	})

	// (e) UNSET state is accepted — enum-but-not-defaulted means an omitted field
	// does not trip the enum, and the snapshot defaults it in code.
	t.Run("unset state with identity: accepted", func(t *testing.T) {
		tb := newBrowserToolbox("btb-unset-id", "joey-airlines", "")
		require.NoError(t, env.Client.Create(ctx, tb),
			"an unset state with an identity must be accepted (snapshot defaults it to persistent)")
		// And the stored value stays empty — NOT defaulted at admission — so the
		// snapshot can still tell "unset" from an explicit value.
		var got v1alpha1.BrowserToolbox
		require.NoError(t, env.Client.Get(ctx, client.ObjectKeyFromObject(tb), &got))
		assert.Empty(t, got.Spec.State, "state must stay unset at the API (no admission default)")
	})

	t.Run("unset state without identity: accepted", func(t *testing.T) {
		tb := newBrowserToolbox("btb-unset-no-id", "", "")
		require.NoError(t, env.Client.Create(ctx, tb),
			"an unset state with no identity must be accepted (snapshot defaults it to ephemeral)")
	})

	t.Run("invalid state value: rejected by enum", func(t *testing.T) {
		tb := newBrowserToolbox("btb-bad-state", "joey-airlines", "sticky")
		err := env.Client.Create(ctx, tb)
		require.Error(t, err)
		assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
	})
}

// TestBrowserToolboxSitesLessUnattendedCEL guards the unattended-site CEL
// rule's `!has(self.access.sites) || ...` fix: CEL's list access on an ABSENT
// optional `sites` field errors rather than reading empty, so
// `self.access.sites.exists(...)` alone throws on every sites-less
// BrowserToolbox, and the error is absorbed only when the right `||` operand
// happens to be true -- which a sites-less blocklist (the common
// general-access shape) can never satisfy. Before the guard, every one of
// these creates was rejected with the unattended-site rule's own message,
// even though no site (let alone an unattended one) was ever declared.
// Deliberately uses the typed client with Sites left nil/omitted (not
// newBrowserToolbox, which always sets one) so the submitted JSON has no
// `sites` key at all.
func TestBrowserToolboxSitesLessUnattendedCEL(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	t.Run("sites-less blocklist: accepted", func(t *testing.T) {
		tb := &v1alpha1.BrowserToolbox{
			ObjectMeta: metav1.ObjectMeta{Name: "btb-no-sites-blocklist", Namespace: "default"},
			Spec: v1alpha1.BrowserToolboxSpec{
				Access:      v1alpha1.BrowserAccess{Mode: "blocklist"},
				Interaction: "act",
			},
		}
		require.NoError(t, env.Client.Create(ctx, tb),
			"a sites-less blocklist BrowserToolbox must be accepted (no unattended site exists to check)")
	})

	t.Run("sites-less allowlist: accepted", func(t *testing.T) {
		tb := &v1alpha1.BrowserToolbox{
			ObjectMeta: metav1.ObjectMeta{Name: "btb-no-sites-allowlist", Namespace: "default"},
			Spec: v1alpha1.BrowserToolboxSpec{
				Access:      v1alpha1.BrowserAccess{Mode: "allowlist"},
				Interaction: "act",
			},
		}
		require.NoError(t, env.Client.Create(ctx, tb),
			"a sites-less allowlist BrowserToolbox must be accepted")
	})
}

// TestBrowserToolboxEmptyIdentityRejected guards the hardening fix on
// BrowserAccess.Identity's new MinLength=1 marker: an EXPLICIT identity: ""
// must be rejected, not treated by CEL's has() as "no identity bound".
// Go's typed client cannot submit this case -- `omitempty` on Identity drops
// the field entirely whenever its value is the empty string, which is
// indistinguishable at the wire from never setting it -- so this goes through
// unstructured.Unstructured to put a literal "" in the JSON the apiserver
// actually validates.
func TestBrowserToolboxEmptyIdentityRejected(t *testing.T) {
	env := testenv.Start(t)
	ctx := context.Background()

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(v1alpha1.SchemeGroupVersion.WithKind("BrowserToolbox"))
	obj.SetName("btb-empty-identity")
	obj.SetNamespace("default")
	require.NoError(t, unstructured.SetNestedField(obj.Object, "allowlist", "spec", "access", "mode"))
	require.NoError(t, unstructured.SetNestedField(obj.Object, "", "spec", "access", "identity"))
	require.NoError(t, unstructured.SetNestedSlice(obj.Object, []interface{}{
		map[string]interface{}{"host": "example.com"},
	}, "spec", "access", "sites"))
	require.NoError(t, unstructured.SetNestedField(obj.Object, "act", "spec", "interaction"))

	err := env.Client.Create(ctx, obj)
	require.Error(t, err, "an explicit empty identity must be rejected at admission")
	assert.True(t, apierrors.IsInvalid(err), "expected an Invalid API error, got: %v", err)
}
