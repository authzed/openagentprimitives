package meta_test

// Error-path coverage for show_attachment and set_view_params — the two meta
// tools whose Execute had no test at all.
//
// The property both share, and the reason these are worth testing over their
// happy paths, is the split between a BAD ARGUMENT (an IsError result the model
// reads and can correct) and a WIRING BUG (the tool was offered without the
// collaborator that backs it). Collapsing the two is how a tool reports success
// while doing nothing, which the set_view_params source calls "the worst of the
// available failures".

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
)

const demoHandle = "mem://demo-session/attachment-1"

// pinRecorder captures the handle Execute forwarded and returns a canned
// outcome, so a test can assert both the delegation and the failure mapping.
type pinRecorder struct {
	calls []string
	err   error
}

func (p *pinRecorder) pin(_ context.Context, handle string) error {
	p.calls = append(p.calls, handle)
	return p.err
}

func TestShowAttachment_Execute_ArgumentErrors(t *testing.T) {
	cases := []struct {
		name    string
		raw     json.RawMessage
		wantMsg string
	}{
		{
			name:    "malformed JSON: IsError result, tool not run",
			raw:     json.RawMessage(`{not json`),
			wantMsg: "show_attachment",
		},
		{
			name:    "handle absent: IsError result naming the required field",
			raw:     json.RawMessage(`{}`),
			wantMsg: "`handle` is required",
		},
		{
			name:    "handle empty: IsError result naming the required field",
			raw:     json.RawMessage(`{"handle":""}`),
			wantMsg: "`handle` is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pinRecorder{}
			tl := meta.NewShowAttachment(meta.ShowAttachmentConfig{Pin: rec.pin})

			res, err := tl.Execute(context.Background(), tc.raw, nil)
			require.NoError(t, err, "bad input is a tool-result error, never a turn-fatal Go error")
			assert.True(t, res.IsError, "the model must see this as a failed call")
			assert.True(t, res.Trusted, "a tool's own diagnostic is trusted text, not model-influenced content")
			assert.Contains(t, res.Content, tc.wantMsg)
			assert.Empty(t, rec.calls, "a rejected call must not reach the pin function")
		})
	}
}

// TestShowAttachment_Execute_NilPinIsTurnFatal pins the wiring-bug branch. A
// nil Pin means the runner offered a tool it cannot back; that returns a Go
// error (fatal to the turn) rather than an IsError result, precisely so the
// model does not try to work around it.
func TestShowAttachment_Execute_NilPinIsTurnFatal(t *testing.T) {
	tl := meta.NewShowAttachment(meta.ShowAttachmentConfig{}) // no Pin wired

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"handle":"`+demoHandle+`"}`), nil)
	require.Error(t, err, "a tool offered without its collaborator is a wiring bug, not bad input")
	assert.Contains(t, err.Error(), "no pin function wired",
		"the operator must be able to locate the wiring bug from the message alone")
	assert.False(t, res.IsError,
		"the failure travels as a Go error; it must not ALSO be dressed as a model-correctable result")
}

func TestShowAttachment_Execute_PinFailureIsModelCorrectable(t *testing.T) {
	rec := &pinRecorder{err: errors.New("handle not found in this session")}
	tl := meta.NewShowAttachment(meta.ShowAttachmentConfig{Pin: rec.pin})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"handle":"`+demoHandle+`"}`), nil)
	require.NoError(t, err, "a rejected handle is bad input, not a turn-fatal failure")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, demoHandle, "the message must name the handle that failed")
	assert.Contains(t, res.Content, "handle not found in this session",
		"the underlying reason must surface, not be swallowed into a generic message")
	assert.Equal(t, []string{demoHandle}, rec.calls, "the handle must reach the pin function verbatim")
}

func TestShowAttachment_Execute_Success(t *testing.T) {
	rec := &pinRecorder{}
	tl := meta.NewShowAttachment(meta.ShowAttachmentConfig{Pin: rec.pin})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"handle":"`+demoHandle+`"}`), nil)
	require.NoError(t, err)
	assert.False(t, res.IsError)
	assert.Equal(t, []string{demoHandle}, rec.calls)
	// The file arrives on the NEXT message, not in this result. Saying so is
	// the whole point of the success text — without it the model looks for
	// content that is not there and calls the tool again.
	assert.Contains(t, res.Content, "next message")
	assert.Contains(t, res.Content, "NOT in this result")
}

// TestSetViewParams_Execute_NilViewReportsFailure covers the inert-tool guard:
// with no Runtime attached the tool must report failure rather than confirm a
// write it never performed.
func TestSetViewParams_Execute_NilViewReportsFailure(t *testing.T) {
	tl := meta.NewSetViewParams(meta.SetViewParamsConfig{}) // no View wired

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"params":{"window.from":"-7d"}}`), nil)
	require.NoError(t, err, "the guard reports through the tool result, not a turn-fatal error")
	assert.True(t, res.IsError, "an inert tool that reported success would be the worst available failure")
	assert.True(t, res.Trusted)
	assert.Contains(t, res.Content, "not available for this session")
	assert.NotContains(t, res.Content, "updated", "it must not read as a successful write")
}

func TestSetViewParams_Execute_MalformedArgs(t *testing.T) {
	tl := meta.NewSetViewParams(meta.SetViewParamsConfig{})

	res, err := tl.Execute(context.Background(), json.RawMessage(`{"params": "not an object"}`), nil)
	require.NoError(t, err, "bad input is a tool-result error, never a turn-fatal Go error")
	assert.True(t, res.IsError)
	assert.Contains(t, res.Content, "set_view_params", "the message must name the tool that rejected the call")
}

// TestMetaTools_ParseArgsRejectsMalformedJSON sweeps every meta tool's Execute
// with syntactically invalid JSON. None may panic, and none may report success
// — the shared tool.ParseArgs contract, which is easy to bypass by unmarshalling
// directly in a new tool.
func TestMetaTools_ParseArgsRejectsMalformedJSON(t *testing.T) {
	// Tools whose Execute needs live collaborators before it ever reaches arg
	// parsing are covered by their own tests; this sweep takes the ones that
	// parse first.
	tools := []tool.Tool{
		meta.NewShowAttachment(meta.ShowAttachmentConfig{}),
		meta.NewSetViewParams(meta.SetViewParamsConfig{}),
		meta.NewSetThreadTitle(meta.SetThreadTitleConfig{}),
		meta.NewUpdateStatus(meta.UpdateStatusConfig{}),
		meta.NewQueryMemory(),
		meta.NewSearchMemory(),
		meta.NewQueryKnowledge(),
		meta.NewLoadSkill(nil),
		// nil reader is safe here: malformed JSON is rejected in ParseArgs
		// before Execute ever reaches t.reader.
		meta.NewGetPreferences(nil),
	}
	for _, tl := range tools {
		t.Run(tl.Name()+": malformed JSON is rejected, never a success", func(t *testing.T) {
			res, err := tl.Execute(context.Background(), json.RawMessage(`{"`), nil)
			if err != nil {
				return // a turn-fatal wiring error is an acceptable outcome here
			}
			assert.True(t, res.IsError, "unparseable args must never produce a success result")
		})
	}
}
