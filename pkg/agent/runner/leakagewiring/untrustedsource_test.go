package leakagewiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// trustedFixture builds an MCPServer whose one tool carries the given raw
// SEP-1913 returnMetadata. A nil raw means the server declared nothing.
func untrustedFixture(t *testing.T, raw []byte) (spiceboxv1alpha1.MCPServer, []spiceboxv1alpha1.AgentClassMCPServerRef) {
	t.Helper()
	tool := spiceboxv1alpha1.MCPServerTool{Name: "fetch_page"}
	if raw != nil {
		tool.Trust.ReturnMetadata = &apiextv1.JSON{Raw: raw}
	}
	return spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "web",
			Version: "v1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
			Tools:   []spiceboxv1alpha1.MCPServerTool{tool},
		},
	}, []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "web", Ref: "web"}}
}

// TestUntrustedSourceReadsTheServersOwnDeclaration.
//
// This is the trifecta's leg A, and it comes from a signal that already
// existed: the MCP validator reads the same `returnMetadata.source` for
// `deny.trust.sourceUntrustedPublic`. The two are independent on purpose —
// deny.trust is an opt-in refusal a spec chooses, while the tag's mark is a
// fact about the datum. A tag left unmarked because nobody chose to deny
// would launder untrusted data into a handoff never allowed to carry it.
func TestUntrustedSourceReadsTheServersOwnDeclaration(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{
			name: "source: untrustedPublic marks the datum",
			raw:  []byte(`{"source":["untrustedPublic"]}`),
			want: true,
		},
		{
			name: "a different source does not",
			raw:  []byte(`{"source":["trustedInternal"]}`),
			want: false,
		},
		{
			name: "no returnMetadata at all is NOT untrusted — absence is the norm, and marking every unannotated read would make the axis meaningless",
			raw:  nil,
			want: false,
		},
		{
			name: "returnMetadata present but carrying no source",
			raw:  []byte(`{"other":["x"]}`),
			want: false,
		},
		{
			// The server controls its own tools/list response, so a blob that
			// is valid JSON but not an OBJECT may be corrupt rather than
			// merely odd. Reading it as a clean source is the one answer that
			// cannot be justified; marking it can only make a later gate fire
			// more readily.
			//
			// Syntactically invalid JSON is deliberately not a case here: the
			// field is Schemaless with PreserveUnknownFields, so the apiserver
			// rejects it at admission and it cannot reach a stored CR. (The
			// fake client refuses it too, for the same marshalling reason.)
			// ContainsEnumFieldChecked still fails closed on it.
			name: "a valid-JSON non-object FAILS CLOSED to untrusted",
			raw:  []byte(`"untrustedPublic"`),
			want: true,
		},
		{
			name: "an array where an object was expected likewise fails closed",
			raw:  []byte(`["untrustedPublic"]`),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, refs := untrustedFixture(t, tc.raw)
			c := newLookupTestClient(t, &srv)
			got := LookupMCPToolUntrustedSource(context.Background(), c, "ns", refs, "web_fetch_page")
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestUntrustedSourceUsesTheLLMFacingName — the same prefix translation the
// resource-mapping lookup needs. Keyed by the bare name, every declaration
// would be invisible and leg A would read clean for a tool that declared
// itself untrusted.
func TestUntrustedSourceUsesTheLLMFacingName(t *testing.T) {
	srv, refs := untrustedFixture(t, []byte(`{"source":["untrustedPublic"]}`))
	c := newLookupTestClient(t, &srv)

	assert.True(t, LookupMCPToolUntrustedSource(context.Background(), c, "ns", refs, "web_fetch_page"),
		"the prefixed name the hook is actually handed")
	assert.False(t, LookupMCPToolUntrustedSource(context.Background(), c, "ns", refs, "fetch_page"),
		"a bare name must not match")
	assert.False(t, LookupMCPToolUntrustedSource(context.Background(), c, "ns", refs, "web_other"),
		"an unknown tool is not untrusted")
}

// TestSidecarToolboxUntrustedSource is the SidecarToolbox sibling: the same
// SEP-1913 returnMetadata.source read off a toolbox tool, same fail-closed
// posture (unparseable → untrusted; absent → not).
func TestSidecarToolboxUntrustedSource(t *testing.T) {
	build := func(raw []byte) (*spiceboxv1alpha1.SidecarToolbox, []spiceboxv1alpha1.AgentClassSidecarToolboxRef) {
		tool := spiceboxv1alpha1.MCPServerTool{Name: "read_public_note"}
		if raw != nil {
			tool.Trust.ReturnMetadata = &apiextv1.JSON{Raw: raw}
		}
		return &spiceboxv1alpha1.SidecarToolbox{
			ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "ns"},
			Spec:       spiceboxv1alpha1.SidecarToolboxSpec{Name: "records", Version: "1", Tools: []spiceboxv1alpha1.MCPServerTool{tool}},
		}, []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "pde", Ref: "records"}}
	}
	for _, tc := range []struct {
		name string
		raw  []byte
		want bool
	}{
		{"untrustedPublic marks the datum", []byte(`{"source":["untrustedPublic"]}`), true},
		{"a different source does not", []byte(`{"source":["trustedInternal"]}`), false},
		{"no returnMetadata is not untrusted", nil, false},
		// A valid-JSON non-object fails closed to untrusted (syntactically
		// invalid JSON can't reach a stored CR — see the MCP sibling test).
		{"valid-JSON non-object fails closed to untrusted", []byte(`"untrustedPublic"`), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tb, refs := build(tc.raw)
			c := newLookupTestClient(t, tb)
			got := LookupSidecarToolboxToolUntrustedSource(context.Background(), c, "ns", refs, "pde_read_public_note")
			assert.Equal(t, tc.want, got)
		})
	}
}
