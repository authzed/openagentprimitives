package uiview_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/web/uicomponents"
	"github.com/authzed/openagentprimitives/pkg/web/uiview"
)

func TestDeclarationFromSpecRoundTripsSlotsAndActions(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{
		ObjectMeta: metav1.ObjectMeta{Namespace: "demo-ns", Name: "demo-ui"},
		Spec: spiceboxv1alpha1.AgentUISpec{
			Actions: []spiceboxv1alpha1.AgentUIAction{{Name: "advance", Tool: "crm_advance_stage", Inputs: []string{"why"}}},
			Slots: []spiceboxv1alpha1.AgentUISlot{
				{Name: "panel", AgentWritable: true, Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:text","props":{"text":"t0"}}`)}},
			},
		},
	}
	d, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	assert.Nil(t, d.Slots, "Normalize always compiles the legacy shape into View and clears Slots")
	require.NotNil(t, d.View)

	hooks := uicomponents.Hooks(d)
	require.Len(t, hooks, 1)
	assert.Equal(t, "panel", hooks[0].Name)
	n := *d.View
	for _, i := range hooks[0].Path {
		n = n.Children[i]
	}
	require.Len(t, n.Children, 1, "the slot's default becomes the hook's sole child")
	assert.Equal(t, "ap:text", n.Children[0].Component)

	require.Len(t, d.Actions, 1)
	assert.Equal(t, []string{"why"}, d.Actions[0].Inputs)
}

func TestDeclarationFromSpecRejectsAnUnknownWireFieldInASlotDefault(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{Spec: spiceboxv1alpha1.AgentUISpec{
		Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "panel",
			Default: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:text","childs":[]}`)}}}}}
	_, err := uiview.DeclarationFromSpec(ui)
	assert.Error(t, err, "a field-copying conversion would accept and silently drop this")
}

// TestDeclarationFromSpecOmitsAbsentDefaultRatherThanNull pins that a slot
// with no Default compiles to a hook with no children, rather than one
// wrapping an empty node — the shim's CompileSlots leaves hook.Children nil
// when the slot never had a default to seed it with.
func TestDeclarationFromSpecOmitsAbsentDefaultRatherThanNull(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{Spec: spiceboxv1alpha1.AgentUISpec{
		Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "panel", AgentWritable: true}},
	}}
	d, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	require.NotNil(t, d.View)

	hooks := uicomponents.Hooks(d)
	require.Len(t, hooks, 1)
	n := *d.View
	for _, i := range hooks[0].Path {
		n = n.Children[i]
	}
	assert.Empty(t, n.Children)
}

func TestDeclarationFromSpecCarriesSpecView(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{Spec: spiceboxv1alpha1.AgentUISpec{
		View: &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack","children":[{"component":"oap:generative","props":{"name":"brief","allowedComponents":["*"]}}]}`)},
	}}
	d, err := uiview.DeclarationFromSpec(ui)
	require.NoError(t, err)
	require.NotNil(t, d.View)
	assert.Nil(t, d.Slots)
	assert.Equal(t, "brief", uicomponents.Hooks(d)[0].Name)
}

func TestDeclarationFromSpecRefusesViewAndSlotsTogether(t *testing.T) {
	ui := &spiceboxv1alpha1.AgentUI{Spec: spiceboxv1alpha1.AgentUISpec{
		View:  &apiextensionsv1.JSON{Raw: []byte(`{"component":"ap:stack"}`)},
		Slots: []spiceboxv1alpha1.AgentUISlot{{Name: "x"}},
	}}
	_, err := uiview.DeclarationFromSpec(ui)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either view or slots")
}
