package bento_test

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/bento" // self-register via init()
)

func TestKind_RegisteredAsBento(t *testing.T) {
	k, ok := registry.Get("bento")
	require.True(t, ok, `registry.Get("bento") returned ok=false`)
	_, isBento := k.(*bento.Kind)
	assert.Truef(t, isBento, `registered kind for "bento" is %T, want *bento.Kind`, k)
}

func TestKind_NotUserAttributable(t *testing.T) {
	assert.False(t, (bento.Kind{}).UserAttributable(),
		"bento has no per-user attribution")
}

func TestKind_SupportedMentionLookups_Empty(t *testing.T) {
	assert.Empty(t, (bento.Kind{}).SupportedMentionLookups(),
		"lookup_user_for_mention should not be wired for bento")
}

func TestKind_RelayedByChannelsd(t *testing.T) {
	assert.True(t, (bento.Kind{}).RelayedByChannelsd(),
		"bento kind is hosted by channelsd")
}

func TestKind_SpawnsSessionOnInbound(t *testing.T) {
	assert.True(t, (bento.Kind{}).SpawnsSessionOnInbound(),
		"bento is a durable scheduler-driven channel; a tick spawns a new session")
}

// TestValidateSpec_RoleOutputRejected names the outcome INDEPENDENTLY of
// SupportedRoles, and that independence is the whole point: it is the only
// thing here that catches the list being wrongly WIDENED.
//
// TestValidateSpecEnforcesSupportedRoles below is derived from the list, so it
// follows a widening — every role becomes an "accepted" case and it stays
// green. Widen bento to AllChannelRoles() without this test and nothing in the
// repo reddens, while `{kind: bento, role: output}` starts passing both the
// lint and reconcile: a cron trigger with no sender (NewSender is a no-op and
// the reply goes out on the sibling output Channel), admitted as an output
// Channel that can never deliver anything.
//
// github's own suite has the equivalent case. Keep both kinds' — a narrow
// SupportedRoles needs one non-derived assertion or the narrowing is
// unguarded.
func TestValidateSpec_RoleOutputRejected(t *testing.T) {
	err := (bento.Kind{}).ValidateSpec(&spiceboxv1alpha1.Channel{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-cron"},
		Spec: spiceboxv1alpha1.ChannelSpec{
			Kind: "bento", Role: spiceboxv1alpha1.ChannelRoleOutput,
			Bento: &spiceboxv1alpha1.BentoChannelConfig{
				Generate: &spiceboxv1alpha1.BentoGenerateConfig{},
			},
		},
	})
	require.Error(t, err, "bento is input-only: it has no sender to deliver an outbound message with")
	assert.Contains(t, err.Error(), "must declare role=input")
}

// TestValidateSpecEnforcesSupportedRoles is the anti-drift assertion for the
// declaration/enforcement pair: SupportedRoles is what `oap agent lint` reads
// off a bundle's requires.channels, ValidateSpec is what reconcile enforces,
// and the Kind interface requires the second to derive from the first. A
// ValidateSpec that stopped consulting the list — or a list narrowed without
// it — would let the lint report a role the cluster then accepts, or the
// reverse.
//
// It lives here rather than in a registry-wide sweep because only a caller
// that knows this kind can build a WELL-FORMED bento Channel. A sweep can
// supply a kind and a role and nothing else, and this kind rejects such a
// Channel for the missing spec.bento.generate regardless of role, so a sweep's
// assert.Error would hold even with the role gate deleted.
//
// This kind had no ValidateSpec test at all before, so the role rule and the
// spec.bento.generate rule are both covered here for the first time.
func TestValidateSpecEnforcesSupportedRoles(t *testing.T) {
	served := (bento.Kind{}).SupportedRoles()
	require.NotEmpty(t, served)

	wellFormed := func(role string) *spiceboxv1alpha1.Channel {
		return &spiceboxv1alpha1.Channel{
			ObjectMeta: metav1.ObjectMeta{Name: "demo-agent-cron"},
			Spec: spiceboxv1alpha1.ChannelSpec{
				Kind: "bento", Role: role,
				Bento: &spiceboxv1alpha1.BentoChannelConfig{
					Generate: &spiceboxv1alpha1.BentoGenerateConfig{},
				},
			},
		}
	}

	for _, role := range spiceboxv1alpha1.AllChannelRoles() {
		if slices.Contains(served, role) {
			t.Run(role+": declared served, so a well-formed Channel is accepted", func(t *testing.T) {
				assert.NoError(t, (bento.Kind{}).ValidateSpec(wellFormed(role)))
			})
			continue
		}
		t.Run(role+": not declared served, so rejected on an otherwise well-formed Channel", func(t *testing.T) {
			err := (bento.Kind{}).ValidateSpec(wellFormed(role))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "must declare role=input",
				"and the message names what this kind does serve, read from SupportedRoles")
		})
	}

	t.Run("a served role still needs spec.bento.generate", func(t *testing.T) {
		ch := wellFormed(served[0])
		ch.Spec.Bento = nil
		err := (bento.Kind{}).ValidateSpec(ch)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec.bento.generate is required",
			"the role gate must not swallow the spec rule behind it")
	})
}

var _ channelkinds.Kind = (*bento.Kind)(nil)
