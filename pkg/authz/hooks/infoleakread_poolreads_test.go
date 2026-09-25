package hooks_test

// A memory search reads N resources in one call: the session's own scope plus
// every resource pool it reached through a slot grant. The single-resource
// declaration cannot describe that — the pools are heterogeneous in type, so no
// ResourceType + field-path pair names them — so the decl carries a plural,
// result-derived alternative, and this file pins what the read hook does with
// it.
//
// Two properties are the reason the shape is plural CALLS rather than one call
// naming N resources:
//
//   - the minter INTERSECTS a request's resources, so one mint naming two pools
//     produces a tag readable only by whoever can view both. Safe, and wrong: a
//     reader of one pool could not be handed that pool's own datum.
//   - the mint carries the datum's bytes, which the platform places in a
//     child's context when the datum is bound into a data slot. One pool's tag
//     carrying the whole result would put every other pool's entries behind
//     this pool's audience. That is the leak this increment exists to prevent.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/authz/provenance"
	"github.com/authzed/openagentprimitives/pkg/authz/scope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// poolReadHook builds the read hook over a decl that reports the given
// resources for any result, with both recorders attached.
func poolReadHook(t *testing.T, mode string, refs []hooks.ToolReadResource, resolveErr error) (
	*hooks.InfoLeakRead, *[]memory.PtTagMintRequest, *[]infoleakagetaint.TaintRecord,
) {
	t.Helper()
	minted := &[]memory.PtTagMintRequest{}
	tainted := &[]infoleakagetaint.TaintRecord{}
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: mode,
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{
				ResultResources: func(string) ([]hooks.ToolReadResource, error) {
					return refs, resolveErr
				},
			}
		},
		Requester: func(_ context.Context, perCall identity.CanonicalUserID) (string, error) {
			return perCall.SubjectRef().String(), nil
		},
		Check: func(context.Context, string, string, string) (bool, error) {
			t.Fatal("the plural path performs no single-resource view check; reaching this means one was wired")
			return false, nil
		},
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			*tainted = append(*tainted, r)
			return nil
		},
		MintPtTag: func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
			*minted = append(*minted, req)
			return "pt_" + req.Resources[0].ID, nil
		},
	})
	return h, minted, tainted
}

func poolReadCall(result string) pipeline.Input {
	return pipeline.Input{
		Point:     pipeline.PostToolCall,
		Requester: identity.CanonicalFromTrusted("dana", "test fixture"),
		Tool: &pipeline.ToolCallInfo{
			Name:   "search_memory",
			UseID:  "toolu_pool",
			Result: result,
		},
	}
}

func twoPools() []hooks.ToolReadResource {
	return []hooks.ToolReadResource{
		{Type: "customer", ID: "alpha", Permission: memory.PermissionViewMemory, Content: `{"entries":[{"id":"a1"},{"id":"a2"}],"count":2}`},
		{Type: "vendor", ID: "beta", Permission: memory.PermissionViewMemory, Content: `{"entries":[{"id":"b1"}],"count":1}`},
	}
}

// TestPoolReads_OneMintPerPool_NeverOneNamingBoth is the shape assertion the
// whole file turns on. A single mint naming both pools would derive the
// INTERSECTION of their audiences — under-disclosure, so nothing downstream
// fails, and alpha's reader silently loses alpha's own datum.
func TestPoolReads_OneMintPerPool_NeverOneNamingBoth(t *testing.T) {
	h, minted, _ := poolReadHook(t, "enforcing", twoPools(), nil)

	dec := h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	require.Len(t, *minted, 2, "two pools in one result are two MINT CALLS, not one call naming two resources")
	for i, got := range *minted {
		require.Len(t, got.Resources, 1, "mint %d names more than one resource; its audience would be an intersection", i)
		assert.Equal(t, "toolu_pool", got.ToolUseID, "every pool's tag links to the call that read it")
		assert.Equal(t, "application/json", got.MIME)
		assert.Empty(t, got.DerivedFrom, "a pool read is a leaf")
	}
	assert.Equal(t, "customer", (*minted)[0].Resources[0].Type)
	assert.Equal(t, "alpha", (*minted)[0].Resources[0].ID)
	assert.Equal(t, "vendor", (*minted)[1].Resources[0].Type)
	assert.Equal(t, "beta", (*minted)[1].Resources[0].ID)
}

// TestPoolReads_EachTagCarriesOnlyItsOwnPoolsEntries is the leak this
// increment prevents. Content lands in pt_tag_content, which the platform
// places into a child's context when the datum is bound into a data slot, so a
// tag for alpha whose content held beta's entries would disclose beta to
// alpha's audience.
func TestPoolReads_EachTagCarriesOnlyItsOwnPoolsEntries(t *testing.T) {
	h, minted, _ := poolReadHook(t, "enforcing", twoPools(), nil)

	h.Eval(context.Background(), poolReadCall(`{"entries":[{"id":"a1"},{"id":"a2"},{"id":"b1"}],"count":3}`))

	require.Len(t, *minted, 2)
	assert.Contains(t, (*minted)[0].Content, "a1")
	assert.NotContains(t, (*minted)[0].Content, "b1", "alpha's tag must not carry beta's entry")
	assert.Contains(t, (*minted)[1].Content, "b1")
	assert.NotContains(t, (*minted)[1].Content, "a1", "beta's tag must not carry alpha's entry")
}

// TestPoolReads_TheRefsPermissionIsPassedThrough: the hook mints under the
// permission the declaration named for THAT resource and derives none of its
// own. The audience of a pool is whoever holds view_memory on it; substituting
// the hook's own idea of a permission would expand the wrong subject set, and
// nothing downstream would notice.
func TestPoolReads_TheRefsPermissionIsPassedThrough(t *testing.T) {
	refs := []hooks.ToolReadResource{
		{Type: "customer", ID: "alpha", Permission: "view_memory", Content: `{"entries":[]}`},
		{Type: "project", ID: "gamma", Permission: "view_notes", Content: `{"entries":[]}`},
	}
	h, minted, _ := poolReadHook(t, "enforcing", refs, nil)

	h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	require.Len(t, *minted, 2)
	assert.Equal(t, "view_memory", (*minted)[0].Resources[0].Permission)
	assert.Equal(t, "view_notes", (*minted)[1].Resources[0].Permission,
		"per-resource, not one permission the hook picked for the call")
}

// TestPoolReads_EveryPoolIsTainted: the tag is not a substitute for the taint.
// Taint is what feeds the respond-time floor, so a pool with a tag and no taint
// leaves the PreResponse gate blind to it — the tag only refines a payload that
// is FULLY tag-covered, and every partial payload falls back to the floor.
func TestPoolReads_EveryPoolIsTainted(t *testing.T) {
	h, _, tainted := poolReadHook(t, "enforcing", twoPools(), nil)

	h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	require.Len(t, *tainted, 2, "one taint record per pool")
	assert.Equal(t, "customer", (*tainted)[0].ResourceType)
	assert.Equal(t, "alpha", (*tainted)[0].ResourceID)
	assert.Equal(t, memory.PermissionViewMemory, (*tainted)[0].Permission,
		"the floor must record the audience permission, not the tool's own")
	assert.Equal(t, "search_memory", (*tainted)[0].ToolName)
	assert.Equal(t, "toolu_pool", (*tainted)[0].ToolUseID)
	assert.Equal(t, "vendor", (*tainted)[1].ResourceType)
	assert.Equal(t, "beta", (*tainted)[1].ResourceID)
}

// TestPoolReads_ASessionOnlyResultTagsNothing: a search that matched only this
// session's own entries touched no pool, so there is nothing to attribute. The
// session's own reads are already covered by the session-wide floor; minting a
// tag here would claim a per-datum audience for data whose audience IS the
// session.
func TestPoolReads_ASessionOnlyResultTagsNothing(t *testing.T) {
	h, minted, tainted := poolReadHook(t, "enforcing", nil, nil)

	dec := h.Eval(context.Background(), poolReadCall(`{"entries":[{"id":"s1"}],"count":1}`))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
	assert.Empty(t, *minted, "no pool in the result ⇒ no tag")
	assert.Empty(t, *tainted, "and no pool taint either")
}

// TestPoolReads_AnUnreadableResultFailsClosed: a result the declaration cannot
// attribute is not a result with no pools in it. Allowing it would hand the
// model entries whose provenance nothing recorded.
func TestPoolReads_AnUnreadableResultFailsClosed(t *testing.T) {
	boom := errors.New("parsing result: unexpected end of JSON input")

	h, minted, _ := poolReadHook(t, "enforcing", nil, boom)
	dec := h.Eval(context.Background(), poolReadCall(`{"entries":`))
	assert.Equal(t, pipeline.Deny, dec.Verdict, "enforcing: an unattributable result is withheld")
	assert.Empty(t, *minted)

	// Logging mode changes nothing by contract: audit it and allow.
	h2, _, _ := poolReadHook(t, "logging", nil, boom)
	dec2 := h2.Eval(context.Background(), poolReadCall(`{"entries":`))
	assert.Equal(t, pipeline.Allow, dec2.Verdict)
	require.NotEmpty(t, dec2.Audit, "logging mode must still say the attribution failed")
}

// TestPoolReads_AFailedCallNamingNoPoolIsNothingToGate mirrors the
// single-resource path's errorResultUnresolvable: a call that FAILED and whose
// result cannot be attributed is a genuine failure, not a broken declaration.
// The skip keys on the fact (nothing resolved), never on the isError flag
// alone — which the upstream writes.
func TestPoolReads_AFailedCallNamingNoPoolIsNothingToGate(t *testing.T) {
	h, minted, _ := poolReadHook(t, "enforcing", nil, errors.New("parsing result: invalid character"))
	in := poolReadCall("search_memory: no search providers configured")
	in.Tool.IsError = true

	dec := h.Eval(context.Background(), in)

	assert.Equal(t, pipeline.Allow, dec.Verdict, "a failed call with nothing to attribute is not a denial")
	assert.Empty(t, *minted)
}

// TestPoolReads_AnUnattributableScopeIsRefusedEvenWhenTheUpstreamSaysError is
// the counterpart, and it is the one that keeps the exemption above honest.
//
// The isError exemption exists for ONE thing: a failed call whose result is not
// the tool's envelope at all, so there is nothing to attribute. It must not
// stretch to cover a result that PARSED fine and named a scope the declaration
// refuses — because isError is written by the UPSTREAM, and a gate whose
// off-switch is a flag the other side sets is not a gate. That is the exact
// failure the comment block above Eval's errored computation records as a past
// production bug; the sentinel is what keeps a second instance from growing
// underneath it.
func TestPoolReads_AnUnattributableScopeIsRefusedEvenWhenTheUpstreamSaysError(t *testing.T) {
	refusal := fmt.Errorf("entry scope %q/%q is neither this session's own nor a resource pool: %w",
		"session", "ns/other-session", hooks.ErrUnattributableResource)

	h, minted, tainted := poolReadHook(t, "enforcing", nil, refusal)
	in := poolReadCall(`{"entries":[{"id":"x"}],"count":1}`)
	in.Tool.IsError = true

	dec := h.Eval(context.Background(), in)

	assert.Equal(t, pipeline.Deny, dec.Verdict,
		"an upstream isError must not excuse a scope the declaration refused")
	assert.Contains(t, dec.Reason, "could not be attributed")
	assert.Empty(t, *minted)
	assert.Empty(t, *tainted)

	// Logging mode: allowed, and the record still written.
	h2, _, _ := poolReadHook(t, "logging", nil, refusal)
	in2 := poolReadCall(`{"entries":[{"id":"x"}],"count":1}`)
	in2.Tool.IsError = true
	dec2 := h2.Eval(context.Background(), in2)
	assert.Equal(t, pipeline.Allow, dec2.Verdict)
	require.Len(t, dec2.Audit, 1)
	assert.Equal(t, "unattributable_result", dec2.Audit[0].Kind)
}

// TestPoolReads_NothingIsRecordedOnTheTagLedger documents a deliberate
// decision, not an omission.
//
// The ledger carries ONE tag id per tool_use, and the emitter wraps the WHOLE
// result in that id's pt-untrusted envelope. No single pool's tag governs the
// whole result — each one's stored content is only its own slice — so recording
// any of them would claim an id over bytes it does not govern. The operator's
// content binding would refuse that claim and drop the payload to the coarse
// floor, so the outcome is the same; the difference is that the envelope does
// not assert something false on the way there.
func TestPoolReads_NothingIsRecordedOnTheTagLedger(t *testing.T) {
	h, minted, _ := poolReadHook(t, "enforcing", twoPools(), nil)
	ctx, led := provenance.WithTagLedger(context.Background())

	h.Eval(ctx, poolReadCall(`{"entries":[]}`))

	require.Len(t, *minted, 2, "the tags are still minted; they govern the datum wherever it is bound")
	_, ok := led.MintedFor("toolu_pool")
	assert.False(t, ok, "no single pool tag covers the whole result, so none is claimed over it")
}

// TestPoolReads_TheScopeHookIgnoresAPluralDecl pins the ruling that scope
// narrowing stays a single-resource concern: a plural decl names no
// ResourceType, so there is nothing for ResourceDisallowed to match on, and the
// hook must pass rather than invent one.
func TestPoolReads_TheScopeHookIgnoresAPluralDecl(t *testing.T) {
	h := hooks.NewScope(hooks.ScopeDeps{
		Enabled: true,
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{
				ResultResources: func(string) ([]hooks.ToolReadResource, error) { return twoPools(), nil },
			}
		},
		GetScope: func(context.Context) (scope.Scope, bool, error) { return scope.Scope{}, false, nil },
	})

	dec := h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	assert.Equal(t, pipeline.Allow, dec.Verdict)
}

// TestPoolReads_APluralDeclIsNotOptedOutByBypassRequesterCheck closes a trap
// rather than a bug.
//
// The plural path deliberately performs no requester view check, so the next
// author writing a plural declaration has every reason to also set
// BypassRequesterCheck — and with the branches in the other order that decl
// matched the "NoTaint equivalent" early return and opted out of tagging AND
// tainting, silently, with the pools right there in the result. Plurality wins:
// a declaration that names resources is tracked, whatever else it says.
func TestPoolReads_APluralDeclIsNotOptedOutByBypassRequesterCheck(t *testing.T) {
	minted := &[]memory.PtTagMintRequest{}
	tainted := &[]infoleakagetaint.TaintRecord{}
	h := hooks.NewInfoLeakRead(hooks.InfoLeakReadDeps{
		Mode: "enforcing",
		LookupReads: func(string) *hooks.ToolReadsDecl {
			return &hooks.ToolReadsDecl{
				BypassRequesterCheck: true,
				ResultResources: func(string) ([]hooks.ToolReadResource, error) {
					return twoPools(), nil
				},
			}
		},
		AppendTaint: func(_ context.Context, r infoleakagetaint.TaintRecord) error {
			*tainted = append(*tainted, r)
			return nil
		},
		MintPtTag: func(_ context.Context, req memory.PtTagMintRequest) (string, error) {
			*minted = append(*minted, req)
			return "pt_" + req.Resources[0].ID, nil
		},
	})

	h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	assert.Len(t, *minted, 2, "a plural decl still tags, whatever it says about the requester check")
	assert.Len(t, *tainted, 2, "and still taints")
}

// TestPoolReads_TheAuditNamesThePoolsAsAString: the plural path records no view
// verdict, and the audit is what makes that absence VISIBLE rather than
// implied. An audit saying only "a read happened, unchecked" would not support
// that claim — which pools, under which permission, is the whole content.
//
// As a STRING because the sink keeps only string-valued fields: runnerHost.
// writeAudit copies `tool`, `requester`, `leakedTo`, `audience` and then every
// remaining field whose value is a string, so the structured `resources` slice
// is dropped on the floor exactly as it already is for read_permitted. A field
// that does not survive the sink is not an audit record.
func TestPoolReads_TheAuditNamesThePoolsAsAString(t *testing.T) {
	refs := []hooks.ToolReadResource{
		{Type: "customer", ID: "alpha", Permission: memory.PermissionViewMemory, Content: `{"entries":[]}`},
		{Type: "project", ID: "gamma", Permission: "view_notes", Content: `{"entries":[]}`},
	}
	h, _, _ := poolReadHook(t, "enforcing", refs, nil)

	dec := h.Eval(context.Background(), poolReadCall(`{"entries":[]}`))

	require.Len(t, dec.Audit, 1)
	rec := dec.Audit[0]
	assert.Equal(t, "read_unchecked", rec.Kind)
	assert.Equal(t, "result_resources", rec.Fields["reason"])

	refsField, ok := rec.Fields["resourceRefs"].(string)
	require.True(t, ok, "must be a string or the sink drops it")
	assert.Equal(t, "customer:alpha#view_memory, project:gamma#view_notes", refsField,
		"each pool with the permission ITS audience was expanded under; one flattened "+
			"permission field would be a lie the moment two pools differ")
}
