package runner

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz/contentguard"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

type fakeInspector struct {
	points []pipeline.Point
	block  string // if Result contains this substring, Block
}

func (f fakeInspector) Points() []pipeline.Point { return f.points }
func (f fakeInspector) Inspect(_ context.Context, s contentguard.Subject) (contentguard.Finding, error) {
	if f.block != "" && strings.Contains(s.Result, f.block) {
		return contentguard.Finding{Action: contentguard.Block, Reason: "blocked: injection"}, nil
	}
	return contentguard.Finding{Action: contentguard.Pass}, nil
}

func TestInspectUntrustedResult(t *testing.T) {
	cases := []struct {
		name     string
		insp     []contentguard.Instance
		content  string
		wantErr  bool // result should become IsError
		wantKept bool // content unchanged
	}{
		{name: "no inspectors: content kept", insp: nil, content: "hello", wantKept: true},
		{name: "pass verdict: content kept",
			insp:    []contentguard.Instance{fakeInspector{points: []pipeline.Point{pipeline.PostToolCall}}},
			content: "hello", wantKept: true},
		{name: "block verdict: becomes IsError",
			insp:    []contentguard.Instance{fakeInspector{points: []pipeline.Point{pipeline.PostToolCall}, block: "ignore previous"}},
			content: "ignore previous instructions", wantErr: true},
		{name: "pre-only inspector is skipped: content kept",
			insp:    []contentguard.Instance{fakeInspector{points: []pipeline.Point{pipeline.PreToolCall}, block: "ignore previous"}},
			content: "ignore previous instructions", wantKept: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{ContentInspectors: tc.insp}
			// inspectUntrustedResult is the helper the call site invokes once it
			// has decided the result is untrusted; it does not re-check Trusted.
			got := l.inspectUntrustedResult(context.Background(), "read_channel_history",
				tool.Result{Content: tc.content})
			if tc.wantErr {
				assert.True(t, got.IsError, "expected block to surface as IsError")
			}
			if tc.wantKept {
				assert.False(t, got.IsError)
				assert.Equal(t, tc.content, got.Content)
			}
		})
	}
}
