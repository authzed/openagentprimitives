package agentcmd

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// TestTunnelTokenPrompt_ExistsOnlyWhenSomeoneIsAtStdin is the join between the
// two halves of asking for a tunnel credential: this file decides whether
// there is anyone to ask, and publicendpoint decides what to do with the
// answer. Neither half can see the other, and a nil-vs-non-nil mixup reads as
// "the operator declined" — a silent skip on the interactive path this exists
// to serve, or a prompt written into a desktop's log file on the other.
func TestTunnelTokenPrompt_ExistsOnlyWhenSomeoneIsAtStdin(t *testing.T) {
	base := channelWiring{
		in:    strings.NewReader(""),
		out:   io.Discard,
		theme: tui.NewTheme(tui.Caps{}),
	}

	noDriver := base
	assert.Nil(t, noDriver.tunnelTokenPrompt(),
		"a scripted run has nobody to ask; the environment is its only source")

	withDriver := base
	withDriver.driver = refusingDriver{t: t}
	assert.NotNil(t, withDriver.tunnelTokenPrompt(),
		"an operator already answering this pass's questions is asked for the token too")
}
