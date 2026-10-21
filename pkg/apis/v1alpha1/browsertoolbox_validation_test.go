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
