package channelcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
)

// TestHandedOverTextFitsTheTTYBodyColumn is a CLIENT-side claim about a KIND's
// text, and so lives here rather than in the kind: the budget it holds that
// text to is this package's chrome, which channelkinds knows nothing about.
//
// What it measures is a BUDGET, not a render. slack's manual route hands its
// app manifest back as the error that ends the run, and an error out of
// Execute is printed verbatim — cmd/oap/main.go's `fmt.Fprintln(os.Stderr,
// err)` — so on THIS route no chrome, note or wrap is involved. The budget
// still binds, because a kind does not choose which surface its text lands on:
// the same Resolve error becomes a chrome-rendered note the moment the run has
// a card left to draw, and a wrapped YAML line continues at column 0 inside an
// indented block, which the service it is pasted into rejects — with nothing
// about the wrapped render telling the operator it happened. Holding the text
// under the tightest column it could meet keeps that unreachable.
//
// Driven through the REGISTERED kind, so what is measured is the text an
// operator actually gets rather than a fixture resembling it.
func TestHandedOverTextFitsTheTTYBodyColumn(t *testing.T) {
	kind, ok := registry.Get("slack")
	require.True(t, ok, "the slack kind must be registered in the oap binary")

	th := tui.NewTheme(tui.Caps{TTY: true, Color: false, Width: 80})
	body := tui.NewChrome(wizardTitle("slack"), []tui.Step{{ID: "x", Label: "X"}}, th).BodyWidth()
	require.Positive(t, body, "the chrome must report a body column to hold anything to")

	// The manual route: no cluster to reach, no working directory to write
	// into, and the whole deliverable comes back as the error that ends the
	// run.
	_, err := kind.Wizard().Resolve(context.Background(),
		channelkinds.WizardInput{Namespace: "default"},
		map[string]string{
			"agentclass":   "demo-agent",
			"slackapp":     "false",
			"capabilities": "",
		})
	require.Error(t, err, "the manual route hands its text over as the error that ends the run")

	widest, cols := longestLine(err.Error())
	t.Logf("widest handed-over line = %d columns (%q); TTY body column at 80 = %d", cols, widest, body)
	assert.Less(t, cols, body,
		"every line handed to the operator must fit the body column, or the chrome breaks it: %q", widest)
}

// longestLine reports the widest line in s, and how many columns wide it is.
func longestLine(s string) (string, int) {
	var widest string
	best := 0
	for _, line := range strings.Split(s, "\n") {
		if n := len([]rune(line)); n > best {
			best, widest = n, line
		}
	}
	return widest, best
}
