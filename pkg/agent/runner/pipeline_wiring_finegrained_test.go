package runner

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	tool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func classGranting(caps map[string]string) *spiceboxv1alpha1.AgentClass {
	ac := &spiceboxv1alpha1.AgentClass{
		ObjectMeta: metav1.ObjectMeta{Name: "demo-agent", Namespace: "ns"},
	}
	if len(caps) > 0 {
		ac.Spec.Capabilities = map[string]apiextensionsv1.JSON{}
		for k, v := range caps {
			ac.Spec.Capabilities[k] = apiextensionsv1.JSON{Raw: []byte(v)}
		}
	}
	return ac
}

func loopWithLookup(class *spiceboxv1alpha1.AgentClass) *Loop {
	return &Loop{
		AgentClass: class,
		SpiceDBLookupSubjects: func(_ context.Context, resource, permission string) ([]string, error) {
			return []string{resource + "#" + permission}, nil
		},
	}
}

// TestFineGrainedIsNilWithoutTheCapability is the default-off regression at
// the wiring layer.
//
// Every existing AgentClass omits the key, so this is the path every agent on
// every cluster takes. Returning non-nil here would switch them all onto
// per-datum tracking — a LookupSubjects per tool call, a durable record per
// datum, and a disclosure gate answering from a different set — with nothing
// in any diff to say so.
func TestFineGrainedIsNilWithoutTheCapability(t *testing.T) {
	assert.Nil(t, loopWithLookup(classGranting(nil)).fineGrainedLeakageDeps(),
		"a class that did not ask for per-datum provenance must get the coarse path, unchanged")

	assert.Nil(t, loopWithLookup(classGranting(map[string]string{"artifacts": `{}`})).fineGrainedLeakageDeps(),
		"granting some OTHER capability must not enable this one")
}

func TestFineGrainedIsWiredWhenGranted(t *testing.T) {
	deps := loopWithLookup(classGranting(map[string]string{
		"fine_grained_info_leakage": `{}`,
	})).fineGrainedLeakageDeps()

	require.NotNil(t, deps)
	require.NotNil(t, deps.Enabled)
	assert.True(t, deps.Enabled(context.Background()))
	require.NotNil(t, deps.TagReaders)
}

// TestTagReadersAsksForTheRESOLVEDPermission pins that a tag's audience comes
// from `reader` on the pt_tag object, not from its direct_reader tuples.
//
// `reader` resolves the intersection across the whole derivation tree inside
// SpiceDB, which is what keeps the arrow semantics the single source of truth
// and makes the reader set LIVE: revoking access to a source narrows every tag
// derived from it at the next check, with no invalidation pass in Go. Reading
// direct_reader instead would see only the top tag's own readers and miss the
// intersection entirely — answering wider than the lattice does.
func TestTagReadersAsksForTheRESOLVEDPermission(t *testing.T) {
	var gotResource, gotPermission string
	l := &Loop{
		AgentClass: classGranting(map[string]string{"fine_grained_info_leakage": `{}`}),
		SpiceDBLookupSubjects: func(_ context.Context, resource, permission string) ([]string, error) {
			gotResource, gotPermission = resource, permission
			return []string{"user:tim"}, nil
		},
	}
	deps := l.fineGrainedLeakageDeps()
	require.NotNil(t, deps)

	readers, err := deps.TagReaders(context.Background(), "ptt-1")
	require.NoError(t, err)
	assert.Equal(t, []string{"user:tim"}, readers)
	assert.Equal(t, "pt_tag:ptt-1", gotResource)
	assert.Equal(t, "reader", gotPermission,
		"the audience is the RESOLVED permission; direct_reader would miss the intersection over derived_from")
}

// TestDestinationAudienceIsWired pins that the tool-call checkpoint has a
// resolver, and that an unknown tool answers UNRESOLVED rather than allowing.
//
// The destination is derived from the authorization the call already performs
// — the Check whose resource id authz.ResolveResourceID pulls from the same
// args — so there is no second declaration to keep in sync with it. What the
// resolver cannot establish it reports as unresolved, and the caller applies
// the leakage Mode; it never reports an empty audience, which would be
// vacuously safe.
func TestDestinationAudienceIsWired(t *testing.T) {
	deps := loopWithLookup(classGranting(map[string]string{
		"fine_grained_info_leakage": `{}`,
	})).fineGrainedLeakageDeps()

	require.NotNil(t, deps)
	require.NotNil(t, deps.DestinationAudience)

	audience, resolved, err := deps.DestinationAudience(context.Background(), "no_such_tool", nil)
	require.NoError(t, err)
	assert.False(t, resolved,
		"a tool this loop does not know has an UNKNOWN destination, and unknown must not read as an empty audience")
	assert.Empty(t, audience)
}

// fakeDestTool is a minimal egress tool carrying a readwrite Check whose
// resource id is a CEL expr over the INNER args (string(args.board_id)) — the
// shape a SidecarToolbox/MCPServer post_to_board declares.
type fakeDestTool struct {
	name string
	perm authz.Permission
}

func (f fakeDestTool) Name() string                                  { return f.name }
func (f fakeDestTool) Kind() tool.Kind                               { return "mcp" }
func (f fakeDestTool) Description() string                           { return "" }
func (f fakeDestTool) InputSchema() json.RawMessage                  { return nil }
func (f fakeDestTool) Permission() authz.Permission                  { return f.perm }
func (f fakeDestTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f fakeDestTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{}, nil
}

// TestDestinationAudience_unwrapsMCPEnvelopeArgs pins the fix for the live
// per-datum-egress demo failure: post_to_board was blocked with "audience could
// not be determined" because destinationAudience resolved the destination id
// from the OUTER MCP envelope {operation_id, _reason, args:{board_id}}, where
// `args.board_id` (the Check's resourceIDExpr) is nested one level down and
// evaluates to nothing. The tool's own authz Check unwraps that envelope
// (loop_dispatch.go → unwrapToolArgs); the fine-grained destination resolver
// must unwrap it the SAME way, or an egress tool's audience is never resolvable
// and every post fails closed.
func TestDestinationAudience_unwrapsMCPEnvelopeArgs(t *testing.T) {
	l := &Loop{
		AgentClass: classGranting(map[string]string{"fine_grained_info_leakage": `{}`}),
		SpiceDBLookupSubjects: func(_ context.Context, resource, permission string) ([]string, error) {
			// Audience for pde_board:board-team#view.
			return []string{"user:you", "user:teammate"}, nil
		},
		Tools: []tool.Tool{fakeDestTool{
			name: "pde_post_to_board",
			perm: authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:   "pde_board",
					Permission:     "post",
					ResourceIDExpr: "string(args.board_id)",
				},
			},
		}},
		LookupToolMapping: func(name string) *spiceboxv1alpha1.ToolResourceMapping {
			if name != "pde_post_to_board" {
				return nil
			}
			return &spiceboxv1alpha1.ToolResourceMapping{
				Tool:  "post_to_board",
				Reads: &spiceboxv1alpha1.ToolReads{ResourceType: "pde_board", IDArg: "board_id", Permission: "view"},
			}
		},
	}

	// The exact envelope shape the LLM sends an MCP tool: board_id nested under "args".
	args := map[string]any{
		"operation_id": "op-1",
		"_reason":      "posting",
		"args":         map[string]any{"board_id": "board-team", "text": "secret"},
	}
	audience, resolved, err := l.destinationAudience(context.Background(), "pde_post_to_board", args)
	require.NoError(t, err)
	assert.True(t, resolved,
		"destinationAudience must UNWRAP the MCP envelope so the Check's string(args.board_id) resolves the board id")
	assert.Equal(t, []string{"user:you", "user:teammate"}, audience)
}

// TestOutboundContentExcludesRoutingArgAndUnescapesMarkup is the regression for
// the live-only failure that kept per-datum egress on the coarse floor even
// after the mint and verify paths worked: the audience gate scanned
// string(in.Tool.Args) — the wire JSON — where the pt-untrusted markup is
// enveloped and JSON-ESCAPED, so PtRegions found zero regions and every real
// (enveloped) egress tool fell to coarse. outboundContent unwraps the envelope,
// drops the routing arg the Check consumes (board_id), and hands back exactly
// the content the model tagged — which PtRegions can then cover.
func TestOutboundContentExcludesRoutingArgAndUnescapesMarkup(t *testing.T) {
	l := &Loop{
		Tools: []tool.Tool{fakeDestTool{
			name: "pde_post_to_board",
			perm: authz.Permission{
				StateImpact: authz.Readwrite,
				Check: &authz.PermissionCheck{
					ResourceType:   "pde_board",
					Permission:     "post",
					ResourceIDExpr: "string(args.board_id)",
				},
			},
		}},
	}
	// The exact wire form the runner receives: the MCP envelope, with the
	// content field's markup JSON-escaped.
	rawArgs := []byte(`{"operation_id":"op-1","_reason":"posting","args":{"board_id":"board-team","text":"<pt-untrusted nonce=\"n1\" id=\"ptt-1\">\n{\"amount\":\"1890.00\"}\n</pt-untrusted nonce=\"n1\">"}}`)

	got := l.outboundContent(context.Background(), "pde_post_to_board", rawArgs)

	assert.NotContains(t, got, "board-team",
		"the routing arg the Check consumes (board_id) must be excluded from the scanned content")
	assert.Contains(t, got, "<pt-untrusted",
		"the content arg's markup must survive UNESCAPED so PtRegions can find the region")

	regions, covered := toolenvelope.PtRegions(got)
	assert.True(t, covered, "with only the content arg left, the payload is fully region-covered")
	require.Len(t, regions, 1)
	assert.Equal(t, "ptt-1", regions[0].ID)
}

// TestOutboundContentDropsToFloorOnAnUntaggedContentArg keeps the security half:
// an untagged string in a NON-routing arg must break coverage, so untagged data
// out never becomes a silent per-datum pass.
func TestOutboundContentDropsToFloorOnAnUntaggedContentArg(t *testing.T) {
	l := &Loop{
		Tools: []tool.Tool{fakeDestTool{
			name: "pde_post_to_board",
			perm: authz.Permission{
				StateImpact: authz.Readwrite,
				Check:       &authz.PermissionCheck{ResourceType: "pde_board", Permission: "post", ResourceIDExpr: "string(args.board_id)"},
			},
		}},
	}
	// board_id is routing (excluded); note carries UNTAGGED data.
	rawArgs := []byte(`{"operation_id":"op-1","_reason":"x","args":{"board_id":"board-team","note":"leaked secret with no tag"}}`)

	got := l.outboundContent(context.Background(), "pde_post_to_board", rawArgs)
	_, covered := toolenvelope.PtRegions(got)
	assert.False(t, covered,
		"an untagged non-routing arg must leave the payload uncovered → the coarse floor, never a silent pass")
}

func TestCheckReferencedArgNames(t *testing.T) {
	cases := []struct {
		name  string
		check *authz.PermissionCheck
		want  []string
	}{
		{name: "CEL resourceIDExpr", check: &authz.PermissionCheck{ResourceIDExpr: "string(args.board_id)"}, want: []string{"board_id"}},
		{name: "template resourceID", check: &authz.PermissionCheck{ResourceIDTemplate: "{project}/{issue}"}, want: []string{"project", "issue"}},
		{name: "nil check", check: nil, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := checkReferencedArgNames(tc.check)
			for _, w := range tc.want {
				assert.True(t, got[w], "expected %q to be recognized as a Check-referenced (routing) arg", w)
			}
			assert.Len(t, got, len(tc.want))
		})
	}
}

// TestFineGrainedIsNilWithoutASubjectLookup keeps the deps from being
// half-built. TagReaders closes over SpiceDBLookupSubjects, so wiring the
// struct without it would produce a hook that panics on the first tagged
// payload rather than one that quietly runs coarse.
func TestFineGrainedIsNilWithoutASubjectLookup(t *testing.T) {
	l := &Loop{AgentClass: classGranting(map[string]string{"fine_grained_info_leakage": `{}`})}
	assert.Nil(t, l.fineGrainedLeakageDeps(),
		"without a subject lookup there is no audience to derive, so the coarse path must keep the job")
}
