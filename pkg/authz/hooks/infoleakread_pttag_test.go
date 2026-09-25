package hooks_test

// Per-datum provenance is minted HERE, at PostToolCall, from the resources a
// call accessed — the same inputs the session-wide taint record already uses.
// The design names this boundary explicitly and rules out the alternative: a
// hook reacting to the entry-appended signal would write a memory entry in
// response to a memory entry, which deadlocks on SigningMemory's
// non-reentrant per-scope mutex.
//
// The two records compose rather than compete. Taint is the FLOOR — the answer
// whenever a payload is not fully tag-covered — and tags are what let a flow be
// judged on the data actually in it.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// readHookMinting builds the hook with both recorders attached.
func readHookMinting(t *testing.T, mintErr error) (*hooks.InfoLeakRead, *[]memory.PtTagMintRequest, *[]infoleakagetaint.TaintRecord) {
	t.Helper()
	minted := &[]memory.PtTagMintRequest{}
	tainted := &[]infoleakagetaint.TaintRecord{}
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(context.Context, string, string, string) (bool, error) { return true, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			*tainted = append(*tainted, r)
			return nil
		},
		MintPtTag: func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
			*minted = append(*minted, req)
			if mintErr != nil {
				return "", mintErr
			}
			return "pt_1", nil
		},
	})
	return h, minted, tainted
}

func readCall() pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("alice", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name:   "get_issue",
			UseID:  "toolu_9",
			Args:   json.RawMessage(`{"args":{"id":"ENG-1"}}`),
			Result: `{}`,
		},
	}
}

// TestAReadMintsATagForTheResourceItTouched is the wiring, and it asserts the
// CONTENT of the request rather than merely that one was made: the minter
// expands each resource under the permission the read was authorized with, so
// a request naming the wrong permission derives the wrong audience.
func TestAReadMintsATagForTheResourceItTouched(t *testing.T) {
	h, minted, tainted := readHookMinting(t, nil)

	dec := h.Eval(context.Background(), readCall())

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, *minted, 1, "a permitted read must mint a tag for the datum it carried into context")
	got := (*minted)[0]
	assert.Equal(t, "toolu_9", got.ToolUseID,
		"the tag links to the tool_use block, matching how taint records the same read")
	require.Len(t, got.Resources, 1)
	assert.Equal(t, "issue", got.Resources[0].Type)
	assert.Equal(t, "ENG-1", got.Resources[0].ID)
	assert.Equal(t, "view", got.Resources[0].Permission,
		"the permission the read was authorized under decides which subject set the minter expands")

	// Both records, from one read. The tag does not replace the taint.
	require.Len(t, *tainted, 1, "taint remains the floor; tags are the resolution, not a substitute")
}

// TestTheRequestNeverNamesAnAudience is the security property of the whole
// route, asserted at the caller.
//
// A tag's direct_reader GRANTS disclosure. The runner holds a session
// credential, so if it could state an audience it could name one its source
// never authorized. The wire type has no field for it; this pins that the
// caller also derives nothing audience-shaped to smuggle in — the resources it
// sends are what it TOUCHED, and nothing more.
func TestTheRequestNeverNamesAnAudience(t *testing.T) {
	h, minted, _ := readHookMinting(t, nil)
	h.Eval(context.Background(), readCall())

	require.Len(t, *minted, 1)
	got := (*minted)[0]
	assert.Empty(t, got.DerivedFrom, "a leaf mint carries no derivation edges")
	assert.False(t, got.UntrustedOrigin, "integrity is declared where a datum enters, not inferred from a read")
	// Everything the caller sends is a claim about what it read.
	for _, r := range got.Resources {
		assert.NotEmpty(t, r.Type)
		assert.NotEmpty(t, r.Permission)
	}
}

// TestAReadMintsALeafAndRecordsItOnTheLedger: with a ledger present, a read
// still mints a LEAF over its resource (never derived), and records the tag id
// on the ledger for the emitter. This is the anti-laundering property — a read
// output's provenance is the RESOURCE it read, not any tag the model may have
// padded into the args.
func TestAReadMintsALeafAndRecordsItOnTheLedger(t *testing.T) {
	h, minted, _ := readHookMinting(t, nil)

	ctx, led := provenance.WithTagLedger(context.Background())
	h.Eval(ctx, readCall())

	require.Len(t, *minted, 1)
	got := (*minted)[0]
	assert.Empty(t, got.DerivedFrom, "a read output is a leaf, never derived from args")
	require.Len(t, got.Resources, 1)
	assert.Equal(t, "issue", got.Resources[0].Type)

	id, ok := led.MintedFor("toolu_9")
	assert.True(t, ok, "the minted tag id is recorded for the emitter")
	assert.Equal(t, "pt_1", id)
}

// TestAFailedMintDegradesToOverBlockingRatherThanDenying.
//
// Without a tag the datum falls back to the session-wide taint set, which can
// only blanket-block or blanket-approve — over-blocking, which is the
// degradation the design requires ("a stripped tag must degrade to
// over-blocking, never to unchecked"). Denying an already-executed read because
// its provenance could not be recorded would trade a safe degradation for a
// broken tool call.
func TestAFailedMintDegradesToOverBlockingRatherThanDenying(t *testing.T) {
	h, minted, tainted := readHookMinting(t, errors.New("mint route unreachable"))

	dec := h.Eval(context.Background(), readCall())

	assert.Equal(t, pipeline.Allow, dec.Verdict,
		"a provenance-recording failure must not deny a read that already happened")
	require.Len(t, *minted, 1, "it was attempted")
	require.Len(t, *tainted, 1,
		"and the taint floor still recorded it — which is what makes the fallback over-block rather than go unchecked")
}

// TestNoMinterMeansNoMintAndNoFailure: every deployment that has not enabled
// the mint route leaves this nil, and must behave exactly as before.
func TestNoMinterMeansNoMintAndNoFailure(t *testing.T) {
	var tainted []infoleakagetaint.TaintRecord
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{ResourceType: "issue", Permission: "view", IDArg: "id"}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(context.Context, string, string, string) (bool, error) { return true, nil },
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			tainted = append(tainted, r)
			return nil
		},
		// MintPtTag deliberately nil.
	})

	dec := h.Eval(context.Background(), readCall())

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Len(t, tainted, 1, "the pre-existing taint behaviour is untouched")
}

// TestAnErroredCallWithNoResolvableResourceMintsNothing: a call that really
// failed names no resource, so there is nothing whose provenance to record and
// minting one would tag a datum that never entered context.
//
// The condition is "no resolvable resource", NOT "isError". That flag is
// written by the UPSTREAM, so keying on it let a server return a full issue
// body under isError:true and have it reach the model untagged — no pt-tag, and
// therefore nothing for the per-datum egress gate to consult. The sibling below
// pins the other half.
func TestAnErroredCallWithNoResolvableResourceMintsNothing(t *testing.T) {
	h, minted, _ := readHookMinting(t, nil)

	in := readCall()
	in.Tool.IsError = true
	in.Tool.Args = json.RawMessage(`{"args":{}}`) // the id the declaration names is absent
	in.Tool.Result = "connection refused"
	h.Eval(context.Background(), in)

	assert.Empty(t, *minted, "a failed read carried nothing into context to tag")
}

// The other half: content delivered UNDER an error flag did enter context, so
// it is tagged like any other read. Otherwise the flag an upstream sets is the
// way to skip provenance entirely.
func TestAnErroredCallCarryingContentStillMints(t *testing.T) {
	h, minted, _ := readHookMinting(t, nil)

	in := readCall()
	in.Tool.IsError = true
	in.Tool.Result = `{"title":"the confidential one"}`
	h.Eval(context.Background(), in)

	assert.Len(t, *minted, 1,
		"a resolvable resource entered context; the error flag does not unmake that")
}
