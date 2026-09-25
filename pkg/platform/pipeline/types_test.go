package pipeline_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

func TestVerdict_ZeroValueIsAllow(t *testing.T) {
	// Allow must be the zero value so an empty Decision{} means "allow, no effects".
	var d pipeline.Decision
	assert.Equal(t, pipeline.Allow, d.Verdict, "zero Decision must be Allow")
	assert.Nil(t, d.Approval)
	assert.Empty(t, d.Notices)
}

func TestInput_CarriesPerPointPayloads(t *testing.T) {
	in := pipeline.Input{
		Point:     pipeline.PreToolCall,
		Session:   pipeline.SessionRef{Namespace: "default", Name: "s1", Class: "ac"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Tool:      &pipeline.ToolCallInfo{Name: "get_issue", Args: []byte(`{"id":"L-1"}`)},
	}
	assert.Equal(t, "get_issue", in.Tool.Name)
	assert.Nil(t, in.Turn)
	assert.Nil(t, in.Response)
	assert.Nil(t, in.Metaagent)
}

// TestInput_CarriesMetaagentInfo verifies the control-plane payload carries only
// generic primitives (no domain types) and round-trips on Input.Metaagent.
func TestInput_CarriesMetaagentInfo(t *testing.T) {
	in := pipeline.Input{
		Point:     pipeline.MetaagentReceived,
		Session:   pipeline.SessionRef{Namespace: "default", Name: "s1"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Metaagent: &pipeline.MetaagentInfo{
			Kind:      "cold_start",
			Trigger:   "session_start",
			Requester: "user:alice",
			Text:      "summarize L-140 and do not read ENG",
			AutoApply: true,
			InboxIdx:  0,
		},
	}
	require.NotNil(t, in.Metaagent)
	assert.Equal(t, "cold_start", in.Metaagent.Kind)
	assert.Equal(t, "session_start", in.Metaagent.Trigger)
	assert.Equal(t, "user:alice", in.Metaagent.Requester)
	assert.Equal(t, "summarize L-140 and do not read ENG", in.Metaagent.Text)
	assert.True(t, in.Metaagent.AutoApply)
	assert.Equal(t, 0, in.Metaagent.InboxIdx)
	assert.Nil(t, in.Turn)
	assert.Nil(t, in.Tool)
}

func TestInput_Fork_RoundTrips(t *testing.T) {
	in := pipeline.Input{
		Point:     pipeline.SessionFork,
		Session:   pipeline.SessionRef{Namespace: "ns", Name: "child", Class: "writer"},
		Requester: identity.CanonicalFromTrusted("user:alice", "test fixture"),
		Fork: &pipeline.ForkInfo{
			ParentRef: "ns/parent",
			ChildRef:  "ns/child",
			Forker:    "user:alice",
			CutTurn:   3,
		},
	}
	require.NotNil(t, in.Fork)
	assert.Equal(t, pipeline.SessionFork, in.Point)
	assert.Equal(t, "ns/parent", in.Fork.ParentRef)
	assert.Equal(t, "ns/child", in.Fork.ChildRef)
	assert.Equal(t, "user:alice", in.Fork.Forker)
	assert.Equal(t, 3, in.Fork.CutTurn)
}
