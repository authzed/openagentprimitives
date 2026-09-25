package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/extract/extractordclient"
)

// TestNewAttachmentExtractor_EmptyEndpointIsATrueNilInterface turns the
// typed-nil property this file's wiring depends on from a comment into an
// assertion: an unset endpoint must yield an httpsrv.AttachmentExtractor for
// which `== nil` is true (a genuine nil interface), not a
// {*extractordclient.Client, nil} tuple that `== nil` would report false
// for. That distinction is the entire point of newAttachmentExtractor
// existing as its own function rather than three edits away from
// `var c *extractordclient.Client; ...; WithAttachmentExtractor(c)` — the
// exact shape AGENTS.md's "Nil interfaces" section documents as a real
// production outage.
func TestNewAttachmentExtractor_EmptyEndpointIsATrueNilInterface(t *testing.T) {
	got := newAttachmentExtractor("")
	// Deliberately NOT assert.Nil: testify's isNil is reflection-based and
	// reports true for ANY nilable-kind value whose underlying pointer is
	// nil — including a typed-nil *extractordclient.Client wrapped in a
	// non-nil httpsrv.AttachmentExtractor interface, which is exactly the
	// bug this test exists to catch (see AGENTS.md's "Nil interfaces"
	// section). assert.Nil would pass on that buggy value just as happily as
	// on a genuine nil interface, defeating the test. A raw `== nil`
	// comparison uses Go's actual interface-equality semantics instead,
	// which correctly distinguish the two. Do not "clean this up" back to
	// assert.Nil.
	require.True(t, got == nil,
		"an unset endpoint must yield a TRUE nil interface; assert.Nil would pass here even for a "+
			"typed-nil pointer wrapped in a non-nil interface, which is the bug this guards")
}

// TestNewAttachmentExtractor_NonEmptyEndpointReturnsAClient proves the other
// half: a configured endpoint returns a real, usable client — never nil,
// and specifically an *extractordclient.Client (the type
// pkg/platform/extract/extractordclient's own suite tests against a real HTTP
// server).
func TestNewAttachmentExtractor_NonEmptyEndpointReturnsAClient(t *testing.T) {
	got := newAttachmentExtractor("http://agentprimitives-extractord.agentprimitives-system.svc:8080")
	require.NotNil(t, got)
	_, ok := got.(*extractordclient.Client)
	assert.True(t, ok, "a configured endpoint must return *extractordclient.Client")
}

// TestExtractordEndpointFlag_BoundToEnvVar proves the flag → env key
// binding end to end through the real cobra command (not by introspecting
// the inline map literal in newCommand's clikit.EnvOverridePreRunE call,
// which isn't exposed as testable data): setting EXTRACTORD_ENDPOINT and
// running PreRunE must set the --extractord-endpoint flag's value. A typo'd
// or renamed env key here would silently degrade every attachment to
// "temporary failure" forever in a real deployment (the operator would just
// never see EXTRACTORD_ENDPOINT) with no compile-time or flag-parse error to
// catch it.
func TestExtractordEndpointFlag_BoundToEnvVar(t *testing.T) {
	t.Setenv("EXTRACTORD_ENDPOINT", "http://extractord-test.example:8080")

	cmd := newCommand()
	require.NotNil(t, cmd.PreRunE, "newCommand must wire an env-override PreRunE")
	require.NoError(t, cmd.PreRunE(cmd, nil))

	f := cmd.Flags().Lookup("extractord-endpoint")
	require.NotNil(t, f, "--extractord-endpoint must be a registered flag")
	assert.Equal(t, "http://extractord-test.example:8080", f.Value.String(),
		"EXTRACTORD_ENDPOINT must bind to --extractord-endpoint")
}
