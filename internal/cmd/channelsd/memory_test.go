package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // registers every memory Kind, including envelope_fact
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

const testSysToken = "sys-test-token"

// testSignerKey returns a throwaway Ed25519 private key for constructing a
// memoryClient in tests. The test memory server has no verify-on-write, so
// the signature need not resolve to a registered key.
func testSignerKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return priv
}

// newTestMemoryServer stands up the v2 memory HTTP API over an in-memory
// Backend with the system channelsd token registered, and returns the
// memory.Memory facade (for seeding turns directly) alongside the client
// under test.
func newTestMemoryServer(t *testing.T) (*httptest.Server, memory.Memory, *memoryClient) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.SetChannelsdToken(testSysToken)
	srv := httptest.NewServer(httpsrv.NewHandler(mem, reg))
	t.Cleanup(srv.Close)
	cli := newMemoryClient(srv.URL, testSysToken, testSignerKey(t))
	return srv, mem, cli
}

// seedTurns appends the given turns into the session scope through a
// turn.Appender — the same Kind-parameterized path the client reads.
func seedTurns(t *testing.T, mem memory.Memory, ns, name string, turns ...memory.Turn) {
	t.Helper()
	app := turn.NewAppender(mem, memory.Scope{Kind: "session", ID: ns + "/" + name})
	ctx := memory.WithSystemApproval(t.Context(), "test")
	for _, tn := range turns {
		require.NoError(t, app.Append(ctx, tn), "seed Append")
	}
}

// TestReadAllWithTurns verifies that ReadAll returns the correct
// []pipeline.MemTurn (Index, Role, Content preserved) when the server has
// turns stored.
func TestReadAllWithTurns(t *testing.T) {
	_, mem, cli := newTestMemoryServer(t)
	ctx := t.Context()

	seedTurns(t, mem, "default", "sess-1",
		memory.Turn{
			Index:     0,
			Role:      "user",
			Content:   []memory.ContentBlock{{Type: "text", Text: "hello"}},
			CreatedAt: time.Unix(1, 0).UTC(),
		},
		memory.Turn{
			Index:     1,
			Role:      "assistant",
			Content:   []memory.ContentBlock{{Type: "text", Text: "world"}},
			CreatedAt: time.Unix(2, 0).UTC(),
		},
	)

	got, err := cli.ReadAll(ctx, "default", "sess-1")
	require.NoError(t, err, "ReadAll")
	require.Len(t, got, 2, "got %+v", got)

	cases := []struct {
		wantIndex int
		wantRole  string
		wantText  string
	}{
		{0, "user", "hello"},
		{1, "assistant", "world"},
	}
	for i, tc := range cases {
		assert.Equal(t, tc.wantIndex, got[i].Index, "turn[%d].Index", i)
		assert.Equal(t, tc.wantRole, got[i].Role, "turn[%d].Role", i)
		if assert.NotEmpty(t, got[i].Content, "turn[%d].Content empty", i) {
			assert.Equal(t, tc.wantText, got[i].Content[0].Text, "turn[%d].Content[0].Text", i)
		}
	}
}

// TestReadAllEmptySession verifies that ReadAll on a session with no turns
// returns (nil or empty slice, nil error) — the caller's for-range is a no-op.
func TestReadAllEmptySession(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	ctx := t.Context()

	got, err := cli.ReadAll(ctx, "default", "no-such-session")
	require.NoError(t, err, "ReadAll on empty session")
	assert.Empty(t, got, "empty session should yield zero turns")
}

// TestReadAll5xxError verifies that ReadAll wraps a non-2xx response in an
// error containing "memory read".
func TestReadAll5xxError(t *testing.T) {
	// Spin up a server that unconditionally returns 500.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	cli := newMemoryClient(srv.URL, testSysToken, testSignerKey(t))

	_, err := cli.ReadAll(t.Context(), "default", "sess-err")
	require.Error(t, err, "expected non-nil error on 500 response")
	assert.Contains(t, err.Error(), "memory read",
		"error should mention 'memory read'")
}

// TestAppendThenReadAll verifies that an Append at an explicit Index lands
// at the same transcript position when read back through the v2 routes.
func TestAppendThenReadAll(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	ctx := t.Context()

	turn := pipeline.MemTurn{
		Index:   7,
		Role:    "user",
		Content: []pipeline.MemContent{{Type: "text", Text: "inherited message"}},
	}
	require.NoError(t, cli.Append(ctx, "default", "sess-append", turn), "Append")

	got, err := cli.ReadAll(ctx, "default", "sess-append")
	require.NoError(t, err, "ReadAll")
	require.Len(t, got, 1, "got %+v", got)
	assert.Equal(t, 7, got[0].Index, "forwarded index")
	assert.Equal(t, "user", got[0].Role, "forwarded role")
	if assert.NotEmpty(t, got[0].Content, "Content empty") {
		assert.Equal(t, "inherited message", got[0].Content[0].Text, "Content[0].Text")
	}
}

// TestAppendAutoAssignsIndex verifies the Index==0 active-session reply
// path: Append auto-assigns max(existing index)+1 and writes the inbound
// message under the distinct "inbox" role so it never collides with the
// runner's "user"/"assistant" writes at the same (Index, Role). The
// runner drains "inbox" turns into real "user" turns (loop.drainInbox).
func TestAppendAutoAssignsIndex(t *testing.T) {
	_, mem, cli := newTestMemoryServer(t)
	ctx := t.Context()

	// Runner's prior writes: a user turn at Index 0 and an assistant reply.
	seedTurns(t, mem, "default", "sess-auto",
		memory.Turn{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "first"}}},
		memory.Turn{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "reply"}}},
	)

	// Channelsd's inbound message arrives with Index 0. It must land at
	// index 2 under the "inbox" role — NOT "user".
	require.NoError(t, cli.Append(ctx, "default", "sess-auto", pipeline.MemTurn{
		Role:    "user",
		Content: []pipeline.MemContent{{Type: "text", Text: "second"}},
	}), "Append auto-assign")

	got, err := cli.ReadAll(ctx, "default", "sess-auto")
	require.NoError(t, err, "ReadAll")
	require.Len(t, got, 3, "got %+v", got)
	assert.Equal(t, 2, got[2].Index, "auto-assigned index")
	assert.Equal(t, "inbox", got[2].Role,
		"active-reply path writes under the distinct inbox role, never user")
}

// TestAppendActiveReplyUsesInboxRole verifies the active-session reply
// path (t.Index == 0) writes the inbound message under the "inbox" role
// regardless of the caller-supplied Role — a distinct role is what keeps
// channelsd's write from colliding with a runner "user"/"assistant"
// turn at a shared (Index, Role) key.
func TestAppendActiveReplyUsesInboxRole(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	ctx := t.Context()

	// No prior turns: the inbound message lands at index 0, role "inbox".
	require.NoError(t, cli.Append(ctx, "default", "sess-inbox", pipeline.MemTurn{
		Role:    "user", // caller-supplied role is overridden on this path
		Content: []pipeline.MemContent{{Type: "text", Text: "hello there"}},
	}), "Append active-reply")

	got, err := cli.ReadAll(ctx, "default", "sess-inbox")
	require.NoError(t, err, "ReadAll")
	require.Len(t, got, 1, "got %+v", got)
	assert.Equal(t, 0, got[0].Index, "inbound message at index 0")
	assert.Equal(t, "inbox", got[0].Role,
		"active-reply path writes under inbox role, not the caller-supplied user")
}

// TestAppendInheritanceCopyKeepsRole verifies the inheritance-copy path
// (t.Index > 0) is unchanged: it forwards the turn verbatim, keeping its
// original Role (so a copied "user"/"assistant" transcript lands at the
// same position in the new session).
func TestAppendInheritanceCopyKeepsRole(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)
	ctx := t.Context()

	require.NoError(t, cli.Append(ctx, "default", "sess-copy", pipeline.MemTurn{
		Index:   3,
		Role:    "user",
		Content: []pipeline.MemContent{{Type: "text", Text: "inherited"}},
	}), "Append inheritance-copy")

	got, err := cli.ReadAll(ctx, "default", "sess-copy")
	require.NoError(t, err, "ReadAll")
	require.Len(t, got, 1, "got %+v", got)
	assert.Equal(t, 3, got[0].Index, "inheritance-copy keeps the source Index")
	assert.Equal(t, "user", got[0].Role,
		"inheritance-copy path keeps the source Role unchanged")
}

// TestRecordEnvelopeFactsWritesOnePerSubjectSigned resolves the blocker-class
// question this task carried: envelope_fact is append-only, and the operator
// facade rejects an unsigned append-only write. RecordEnvelopeFacts MUST
// route through the same provenance-SIGNING client RecordTriggerDelivery
// uses (m.signed, not the plain m.client) — this test reads the entries back
// off the server's own backing Local facade and asserts each carries a
// non-nil, populated Provenance. Had RecordEnvelopeFacts used the unsigned
// client, Provenance would be nil here (provenance.SigningMemory is the only
// thing that attaches it) — this server has no verify-on-write configured
// (see newTestMemoryServer), so an unsigned write would fail silently at a
// REAL operator (ErrProvenanceRequired) while passing here undetected; the
// Provenance assertion is what makes that failure visible in this harness.
func TestRecordEnvelopeFactsWritesOnePerSubjectSigned(t *testing.T) {
	_, mem, cli := newTestMemoryServer(t)
	ctx := t.Context()

	facts := []channelkinds.TriggerFact{{
		Subjects: []factcontent.Subject{
			{ResourceType: "git_commit", ResourceID: "deadbeef"},
			{ResourceType: "github_pr", ResourceID: "acme/widgets#7"},
		},
		Facts: map[string]any{"head_is_fork": true},
	}}
	require.NoError(t, cli.RecordEnvelopeFacts(ctx, "default", "sess-facts", "github", "pull_request", facts),
		"RecordEnvelopeFacts")

	res, err := mem.Query(memory.WithSystemApproval(ctx, "test"), memory.Query{
		Scope: memory.Scope{Kind: "session", ID: "default/sess-facts"},
		Kinds: []string{envelopefact.KindName},
	})
	require.NoError(t, err, "Query envelope_fact entries")
	require.Len(t, res.Entries, 2, "one entry per subject in the single fact")

	for _, e := range res.Entries {
		if assert.NotNil(t, e.Provenance, "entry %s must carry signed provenance — an unsigned write means the "+
			"caller used the plain client instead of the signing facade", e.ID) {
			assert.Equal(t, "system:channelsd", e.Provenance.Publisher, "entry %s Provenance.Publisher", e.ID)
			assert.NotEmpty(t, e.Provenance.KeyID, "entry %s Provenance.KeyID", e.ID)
			assert.NotEmpty(t, e.Provenance.Sig, "entry %s Provenance.Sig", e.ID)
		}
	}
}

// TestMemoryClientPreferencesReturnsLiveNonNilClient asserts the
// Deps.Preferences wiring seam: memoryClient.Preferences() must return a
// genuinely non-nil channelkinds.PreferencesClient backed by the same
// *httpclient.Client the facade already holds — not a typed-nil pointer
// wrapped into the interface (AGENTS.md's nil-interface rule), since a
// typed-nil there would make the slack listener's "nil = degrade to
// couldn't-load" check lie and panic on first call instead.
func TestMemoryClientPreferencesReturnsLiveNonNilClient(t *testing.T) {
	_, _, cli := newTestMemoryServer(t)

	prefs := cli.Preferences()
	require.NotNil(t, prefs, "Preferences() must return a genuinely non-nil interface")
	assert.Same(t, cli.client, prefs, "Preferences() must be backed by the same concrete httpclient.Client the facade holds")
}
