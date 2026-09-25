package meta_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"

	_ "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack" // register the slack kind so its dialect is assertable
)

// textFmtKind is a registered channel kind whose formatting instructions are
// unlike any real kind's, so a schema that contains them can only have got
// them by asking the kind.
type textFmtKind struct {
	*stubKind
	name  string
	instr string
}

func (k textFmtKind) Name() string                       { return k.name }
func (k textFmtKind) Capabilities() []string             { return []string{"text", "markdown"} }
func (k textFmtKind) TextFormattingInstructions() string { return k.instr }

// registerTextFmtKind puts a stub kind in the process-wide registry for the
// duration of one test. The registry panics on duplicate registration and has
// no Unregister, so each caller gets a distinct name.
func registerTextFmtKind(t *testing.T, name, instr string) {
	t.Helper()
	registry.Register(textFmtKind{stubKind: &stubKind{}, name: name, instr: instr})
}

// respondTextDescription builds respond_to_user for the named channel kind and
// returns the `text` property's description from the tool's input schema —
// which is prompt text the model reads verbatim.
func respondTextDescription(t *testing.T, kindName string, caps []string) string {
	t.Helper()
	tl := meta.New(meta.RespondConfig{
		Capabilities: caps,
		ChannelKind:  kindName,
		NATSPublish:  func(_ context.Context, _ string, _ []byte) error { return nil },
	})
	require.NotNil(t, tl, "meta.New must return a tool")

	var schema struct {
		Properties struct {
			Text struct {
				Description string `json:"description"`
			} `json:"text"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(tl.InputSchema(), &schema), "decode respond_to_user schema")
	return schema.Properties.Text.Description
}

// TestRespondTextDescriptionComesFromTheKind is the RED test for
// pkg/agent/tool/meta/respond.go's `switch cfg.ChannelKind { case "slack": … }`.
//
// pkg/agent/runner/prompt.go tells the model that respond_to_user's formatting
// rules are "channel-specific … and authoritative". Before the fix the tool
// hardcoded exactly one channel's dialect and gave every other kind a generic
// CommonMark line, so that promise was only kept for Slack. Registering a kind
// whose dialect differs from both arms of the old switch shows the leak: the
// kind's own instructions never reach the schema.
func TestRespondTextDescriptionComesFromTheKind(t *testing.T) {
	const instr = "Use ROT13 markup only; ((emphasis)) and [[code]] are the sole spans."
	registerTextFmtKind(t, "textfmt-owned", instr)

	desc := respondTextDescription(t, "textfmt-owned", []string{"text", "markdown"})

	assert.Contains(t, desc, instr,
		"respond_to_user's text description must come from the bound kind's "+
			"TextFormattingInstructions, not from a switch in pkg/agent/tool/meta")
	assert.NotContains(t, desc, "Markdown is supported.",
		"the generic CommonMark line must not be used for a kind that answered for itself")
}

// TestRespondTextDescriptionKeepsSlackMrkdwn guards the user-visible half: the
// Slack rules must survive the move onto the kind unchanged. Losing them would
// make the model emit CommonMark that Slack renders with literal asterisks and
// literal [label](url) — worse than the switch this replaces.
func TestRespondTextDescriptionKeepsSlackMrkdwn(t *testing.T) {
	desc := respondTextDescription(t, "slack", []string{"text", "markdown"})

	assert.Contains(t, desc, "Slack mrkdwn", "slack replies must still be told to use mrkdwn")
	assert.Contains(t, desc, "*bold* (single asterisk)")
	assert.Contains(t, desc, "<https://url|label> for links")
	assert.Contains(t, desc, "NOT supported: headings")
}

// TestRespondTextDescriptionWithoutMarkdownCapability keeps the capability
// gate where it belongs: "can this surface render markdown at all" is answered
// by the binding's capabilities, not by the kind's dialect, so a kind that
// declares formatting instructions still gets the plain-text line when the
// binding has no markdown capability.
func TestRespondTextDescriptionWithoutMarkdownCapability(t *testing.T) {
	const instr = "Use ROT13 markup only."
	registerTextFmtKind(t, "textfmt-plain", instr)

	desc := respondTextDescription(t, "textfmt-plain", []string{"text"})

	assert.Contains(t, desc, "Plain text only")
	assert.NotContains(t, desc, instr,
		"a channel with no markdown capability must not be handed markup rules")
}

// TestRespondTextDescriptionUnregisteredKindFallsBack pins the documented
// degrade: an unregistered kind name yields the generic instructions rather
// than an empty description or a panic.
func TestRespondTextDescriptionUnregisteredKindFallsBack(t *testing.T) {
	desc := respondTextDescription(t, "no-such-kind-registered", []string{"text", "markdown"})
	assert.Contains(t, desc, "Markdown", "unregistered kinds keep the generic CommonMark line")
}
