package channelkinds

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// componentKind embeds the Kind interface (nil — its methods are never called)
// and adds the optional component seam, so the lookup helpers can be exercised
// without constructing a full Kind or importing a concrete channel package.
type componentKind struct {
	Kind
}

func (componentKind) ComponentFormattingInstructions() string { return "author blocks like so" }

func (componentKind) ValidateComponents(raw json.RawMessage) error {
	if string(raw) == "bad" {
		return errors.New("bad components")
	}
	return nil
}

func (componentKind) ComponentsPlainText(raw json.RawMessage) string {
	return "flattened:" + string(raw)
}

// plainKind (a bare struct{ Kind }, declared in identity_helpers_test.go) stands
// in for a kind implementing neither component interface.

func TestComponentFormattingInstructionsFor(t *testing.T) {
	assert.Equal(t, "", ComponentFormattingInstructionsFor(nil), "nil kind has no instructions")
	assert.Equal(t, "", ComponentFormattingInstructionsFor(plainKind{}), "non-component kind has no instructions")
	assert.Equal(t, "author blocks like so", ComponentFormattingInstructionsFor(componentKind{}), "component kind returns its instructions")
}

func TestComponentValidatorFor(t *testing.T) {
	assert.Nil(t, ComponentValidatorFor(nil), "nil kind has no validator")
	assert.Nil(t, ComponentValidatorFor(plainKind{}), "non-component kind has no validator")

	v := ComponentValidatorFor(componentKind{})
	require.NotNil(t, v, "component kind exposes a validator")
	assert.NoError(t, v.ValidateComponents(json.RawMessage(`ok`)))
	assert.Error(t, v.ValidateComponents(json.RawMessage(`bad`)))
	assert.Equal(t, "flattened:ok", v.ComponentsPlainText(json.RawMessage(`ok`)), "validator projects components to text for egress measurement")
}
