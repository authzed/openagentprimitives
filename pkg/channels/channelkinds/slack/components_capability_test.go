package slack

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

func TestKindCapabilitiesIncludesComponents(t *testing.T) {
	caps := (&Kind{}).Capabilities()
	assert.Contains(t, caps, "components", "slack advertises the components capability so respond_to_user exposes the blocks field")
}

func TestComponentFormattingInstructionsCoverTheContract(t *testing.T) {
	got := (&Kind{}).ComponentFormattingInstructions()
	require.NotEmpty(t, got, "a components-capable kind must supply authoring instructions")
	lower := strings.ToLower(got)
	// Names the supported layout blocks.
	for _, want := range []string{"header", "section", "divider", "context", "rich_text"} {
		assert.Contains(t, lower, want, "instructions should name the %q block", want)
	}
	// States the key prohibitions and the text-fallback requirement.
	assert.Contains(t, lower, "interactive", "instructions should say interactive elements are unsupported")
	assert.Contains(t, lower, "image", "instructions should say images are not yet supported")
	assert.Contains(t, lower, "text", "instructions should tell the model to also supply a text summary/fallback")
}

// The slack Kind must satisfy the neutral component seam used by respond_to_user.
func TestKindSatisfiesComponentValidator(t *testing.T) {
	var _ channelkinds.ComponentValidator = (*Kind)(nil)
	var _ channelkinds.ComponentFormatter = (*Kind)(nil)

	k := &Kind{}
	require.NoError(t, k.ValidateComponents(json.RawMessage(`[{"type":"divider"}]`)))
	require.Error(t, k.ValidateComponents(json.RawMessage(`[{"type":"actions","elements":[]}]`)))
}

func TestComponentsPlainText(t *testing.T) {
	k := &Kind{}
	got := k.ComponentsPlainText(json.RawMessage(`[{"type":"header","text":{"type":"plain_text","text":"Results"}},{"type":"section","text":{"type":"mrkdwn","text":"all good"}}]`))
	assert.Contains(t, got, "Results")
	assert.Contains(t, got, "all good")
	assert.Equal(t, "", k.ComponentsPlainText(json.RawMessage(`not json`)), "unparseable input yields empty projection")
}
