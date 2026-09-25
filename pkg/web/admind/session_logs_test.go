package admind_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/web/admind"
)

// sessionLogsBody is the JSON shape handleSessionLogs returns.
type sessionLogsBody struct {
	Entries   []logEntryBody `json:"entries"`
	Truncated bool           `json:"truncated"`
}

type logEntryBody struct {
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Index         int    `json:"index"`
	Role          string `json:"role"`
	Content       string `json:"content"`
	Actor         string `json:"actor"`
	ActorInferred bool   `json:"actorInferred"`
	Blocks        []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
		IsError bool            `json:"isError"`
	} `json:"blocks"`
}

// putTurn seeds one transcript turn through the same facade the handler reads.
func putTurn(t *testing.T, a *admind.Admind, scopeID string, idx int, role string, at time.Time, blocks ...memory.ContentBlock) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Content []memory.ContentBlock `json:"content"`
	}{Content: blocks})
	require.NoError(t, err)
	_, err = a.Memory().Put(context.Background(), memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: scopeID},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   raw,
	})
	require.NoError(t, err)
}

// putTurnWithAuthor mirrors putTurn but also stamps the turn's per-turn
// author, exercising the same field turn.EntryToTurn decodes into
// memory.Turn.Author.
func putTurnWithAuthor(t *testing.T, a *admind.Admind, scopeID string, idx int, role string, at time.Time, author identity.Subject, blocks ...memory.ContentBlock) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Content []memory.ContentBlock `json:"content"`
		Author  identity.Subject      `json:"author,omitempty"`
	}{Content: blocks, Author: author})
	require.NoError(t, err)
	_, err = a.Memory().Put(context.Background(), memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: scopeID},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   raw,
	})
	require.NoError(t, err)
}

func TestAdmindSessionLogs_OrderedDecodedTranscript(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	// Seed out of chronological order to prove the handler orders by CreatedAt.
	putTurn(t, a, "default/s1", 1, "assistant", base.Add(time.Second),
		memory.ContentBlock{Type: "text", Text: "on it"},
		memory.ContentBlock{Type: "tool_use", ToolUse: &memory.ToolUseBlock{Name: "bash", Input: []byte(`{"cmd":"ls"}`)}})
	putTurn(t, a, "default/s1", 0, "user", base,
		memory.ContentBlock{Type: "text", Text: "hello there"})
	h := a.Handler()

	w := do(t, h, http.MethodGet, "/admin/v1/sessions/default/s1/logs", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionLogsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Entries, 2, "full transcript returns every turn")
	assert.False(t, body.Truncated)

	// Chronological: the user turn (index 0) precedes the assistant turn.
	assert.Equal(t, 0, body.Entries[0].Index)
	assert.Equal(t, "user", body.Entries[0].Role)
	assert.Equal(t, "turn", body.Entries[0].Kind)
	assert.Contains(t, body.Entries[0].Content, "hello there")

	assert.Equal(t, 1, body.Entries[1].Index)
	assert.Equal(t, "assistant", body.Entries[1].Role)
	assert.Contains(t, body.Entries[1].Content, "on it", "assistant text decoded")
	assert.Contains(t, body.Entries[1].Content, "bash", "tool_use block summarized into content")
}

func TestAdmindSessionLogs_StructuredBlocksAndActor(t *testing.T) {
	// Seed the session CR so its started-by annotation supplies the actor.
	sess := &spiceboxv1alpha1.AgentSession{}
	sess.Namespace, sess.Name = "default", "s2"
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	// A user request turn, then an assistant turn carrying text + a tool_use
	// with structured input + a tool_result with structured JSON output.
	putTurn(t, a, "default/s2", 0, "user", base,
		memory.ContentBlock{Type: "text", Text: "run ls"})
	putTurn(t, a, "default/s2", 1, "assistant", base.Add(time.Second),
		memory.ContentBlock{Type: "text", Text: "on it"},
		memory.ContentBlock{Type: "tool_use", ToolUse: &memory.ToolUseBlock{
			Name: "bash", Input: []byte(`{"cmd":"ls"}`)}},
		memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
			Content: `{"files":["a","b"]}`}})

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/s2/logs", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionLogsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Entries, 2)

	// Actor: user turn attributed to the session started-by (legacy, author-less
	// turn, so flagged inferred); assistant turn not.
	user, asst := body.Entries[0], body.Entries[1]
	assert.Equal(t, "user:alice", user.Actor, "user turn carries the started-by actor")
	assert.True(t, user.ActorInferred, "author-less turn falls back to started-by, so it's inferred")
	assert.Empty(t, asst.Actor, "assistant turn is the agent, not a requester")
	assert.False(t, asst.ActorInferred)

	// The flat summary is still present (backward-compat).
	assert.Contains(t, asst.Content, "on it")
	assert.Contains(t, asst.Content, "bash")

	// Structured blocks: 3 typed blocks with structured input/content.
	require.Len(t, asst.Blocks, 3, "text + tool_use + tool_result")
	assert.Equal(t, "text", asst.Blocks[0].Type)
	assert.Equal(t, "on it", asst.Blocks[0].Text)

	assert.Equal(t, "tool_use", asst.Blocks[1].Type)
	assert.Equal(t, "bash", asst.Blocks[1].Name)
	assert.JSONEq(t, `{"cmd":"ls"}`, string(asst.Blocks[1].Input), "input is raw JSON, not stringified")

	assert.Equal(t, "tool_result", asst.Blocks[2].Type)
	assert.False(t, asst.Blocks[2].IsError)
	assert.JSONEq(t, `{"files":["a","b"]}`, string(asst.Blocks[2].Content), "JSON output stays structured")
}

func TestAdmindSessionLogs_PerTurnAuthorWinsOverStartedBy(t *testing.T) {
	// Seed the session CR so its started-by annotation is available as the
	// legacy fallback for author-less turns.
	sess := &spiceboxv1alpha1.AgentSession{}
	sess.Namespace, sess.Name = "default", "s4"
	sess.Annotations = map[string]string{
		spiceboxv1alpha1.AnnotationStartedByCanonicalID: "user:alice",
	}
	k8s := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(sess).Build()
	a := newTestAdmind(t, k8s)
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)

	// Index 0: a legacy author-less user turn (written before per-turn
	// authorship) — must fall back to the session's started-by, flagged.
	putTurn(t, a, "default/s4", 0, "user", base,
		memory.ContentBlock{Type: "text", Text: "legacy request"})
	// Index 1: a joiner's turn carrying its own per-turn author — the real
	// author must win over the session started-by (multiplayer-correct).
	putTurnWithAuthor(t, a, "default/s4", 1, "user", base.Add(time.Second),
		identity.Subject("user:bob"),
		memory.ContentBlock{Type: "text", Text: "bob's request"})

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/s4/logs", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionLogsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Entries, 2)

	byIndex := map[int]logEntryBody{}
	for _, e := range body.Entries {
		byIndex[e.Index] = e
	}

	legacy, bob := byIndex[0], byIndex[1]
	assert.Equal(t, "user:alice", legacy.Actor, "legacy author-less turn falls back to started-by")
	assert.True(t, legacy.ActorInferred, "legacy fallback is flagged inferred")

	assert.Equal(t, "user:bob", bob.Actor, "real per-turn author wins over started-by")
	assert.False(t, bob.ActorInferred, "a genuine per-turn author is not inferred")
}

func TestAdmindSessionLogs_ToolResultNonJSONIsQuotedString(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	base := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	putTurn(t, a, "default/s3", 0, "assistant", base,
		memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
			Content: "plain text output", IsError: true}})

	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/s3/logs", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code)

	var body sessionLogsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Entries, 1)
	require.Len(t, body.Entries[0].Blocks, 1)
	b := body.Entries[0].Blocks[0]
	assert.Equal(t, "tool_result", b.Type)
	assert.True(t, b.IsError)
	// Non-JSON output is emitted as a valid JSON string so the UI can render it.
	assert.JSONEq(t, `"plain text output"`, string(b.Content))
}

func TestAdmindSessionLogs_UnknownSessionIsEmpty200(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/ghost/logs", "test-token", "user:YWRtaW4", "")
	require.Equal(t, http.StatusOK, w.Code, "an untracked session is 200, never 404")

	var body sessionLogsBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Entries)
	assert.False(t, body.Truncated)
}

func TestAdmindSessionLogs_RequiresViewSessions(t *testing.T) {
	a := newTestAdmind(t, fake.NewClientBuilder().WithScheme(newScheme(t)).Build())
	// "user:bm9wZQ" is not in the stub's admin allow-map → view_sessions denied.
	w := do(t, a.Handler(), http.MethodGet, "/admin/v1/sessions/default/s1/logs", "test-token", "user:bm9wZQ", "")
	assert.Equal(t, http.StatusForbidden, w.Code, "session logs are gated on view_sessions")
}
