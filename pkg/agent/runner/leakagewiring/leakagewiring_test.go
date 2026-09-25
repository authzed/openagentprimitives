package leakagewiring

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	slackkind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack"
)

func newLookupTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// TestLookupMCPToolResourceMapping_LLMFacingName codifies that the
// info-leakage hook receives the prefixed, normalized LLM-facing name
// (`linear_get_issue`) and resolves it back to the MCPServer's
// bare-named ToolResourceMapping entry (`tool: get_issue`). Without
// this translation, the toolResourceMap declaration is dead and every
// MCP read tool trips ErrUnmappedTool in enforcing mode.
func TestLookupMCPToolResourceMapping_LLMFacingName(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "linear",
			Version: "v1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
			Tools:   []spiceboxv1alpha1.MCPServerTool{{Name: "get_issue"}},
			ToolResourceMap: []spiceboxv1alpha1.ToolResourceMapping{
				{Tool: "get_issue", Reads: &spiceboxv1alpha1.ToolReads{
					ResourceType: "linear_issue",
					IDArg:        "issueId",
					Permission:   "view",
				}},
			},
		},
	}
	c := newLookupTestClient(t, srv)
	refs := []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear"}}

	t.Run("LLM-facing prefixed name resolves to the bare entry", func(t *testing.T) {
		got := LookupMCPToolResourceMapping(context.Background(), c, "ns", refs, "linear_get_issue")
		require.NotNil(t, got, "must find the entry from the LLM-facing name")
		assert.Equal(t, "get_issue", got.Tool)
		require.NotNil(t, got.Reads)
		assert.Equal(t, "linear_issue", got.Reads.ResourceType)
	})

	t.Run("bare tool name without prefix does NOT match", func(t *testing.T) {
		got := LookupMCPToolResourceMapping(context.Background(), c, "ns", refs, "get_issue")
		assert.Nil(t, got)
	})

	t.Run("wrong prefix does NOT match the wrong MCPServer", func(t *testing.T) {
		got := LookupMCPToolResourceMapping(context.Background(), c, "ns", refs, "github_get_issue")
		assert.Nil(t, got)
	})

	t.Run("unknown tool under correct prefix returns nil", func(t *testing.T) {
		got := LookupMCPToolResourceMapping(context.Background(), c, "ns", refs, "linear_unknown")
		assert.Nil(t, got)
	})
}

// TestLookupMCPToolResourceMapping_PrefixVariesFromCRName verifies the
// LLM prefix is taken from AgentClassMCPServerRef.Name (not the
// MCPServer CR's metadata.name). An operator can rename the prefix
// per AgentClass without renaming the MCPServer CR.
func TestLookupMCPToolResourceMapping_PrefixVariesFromCRName(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "linear-readonly-any", Namespace: "ns"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "linear-readonly-any",
			Version: "v1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
			ToolResourceMap: []spiceboxv1alpha1.ToolResourceMapping{
				{Tool: "get_issue", Reads: &spiceboxv1alpha1.ToolReads{
					ResourceType: "linear_issue", IDArg: "issueId", Permission: "view",
				}},
			},
		},
	}
	c := newLookupTestClient(t, srv)
	refs := []spiceboxv1alpha1.AgentClassMCPServerRef{{Name: "linear", Ref: "linear-readonly-any"}}

	got := LookupMCPToolResourceMapping(context.Background(), c, "ns", refs, "linear_get_issue")
	require.NotNil(t, got)
	assert.Equal(t, "get_issue", got.Tool)
}

// TestRequesterSubjectRef codifies the runner-side contract: the leakage
// hook's RequesterCanonicalID must return a SpiceDB subject ref
// ("user:<canonical>"), NOT a bare canonical. The downstream SpiceDB
// CheckPermission call splits the value by ":" and rejects bare
// canonicals with "must be of the form type:id".
func TestRequesterSubjectRef(t *testing.T) {
	cases := []struct {
		name      string
		canonical string
		want      string
	}{
		{
			name:      "base64 email canonical: prefixed",
			canonical: "am9leUBhdXRoemVkLmNvbQ",
			want:      "user:am9leUBhdXRoemVkLmNvbQ",
		},
		{
			name:      "kind:team:externalID canonical: prefixed",
			canonical: "c2xhY2s6VDAxNjpVMDE3WEpKUUQ3QQ",
			want:      "user:c2xhY2s6VDAxNjpVMDE3WEpKUUQ3QQ",
		},
		{
			name:      "empty canonical: empty ref (kubectl-driven session)",
			canonical: "",
			want:      "",
		},
		{
			// A session whose inbound carried no human acts as its input
			// Channel's declared subject, which is already a fully-qualified
			// reference. Prefixing it again yields "user:service:<id>", which
			// SplitObject accepts and SpiceDB then rejects — an object_id with
			// a colon in it — so the leakage gate fails on a malformed request
			// instead of on an authorization answer.
			name:      "service subject: already qualified, passed through unprefixed",
			canonical: "service:demo-reviewbot-github",
			want:      "service:demo-reviewbot-github",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RequesterSubjectRef(tc.canonical))
		})
	}
}

type fakeAudienceLookup struct {
	subjects []string
	calls    []string
}

func (f *fakeAudienceLookup) LookupSubjects(_ context.Context, subjectRef string) ([]string, error) {
	f.calls = append(f.calls, subjectRef)
	return f.subjects, nil
}

// TestWireChannelKindAudienceResolvers_Slack codifies the runner's
// startup contract: after wiring, the registered *slack.Kind's
// ResolveAudience no longer reports "not wired" and instead delegates to
// the supplied lookup.
func TestWireChannelKindAudienceResolvers_Slack(t *testing.T) {
	k, ok := registry.Get(slackkind.KindName)
	require.True(t, ok, "slack kind must be registered")
	sk, ok := k.(*slackkind.Kind)
	require.True(t, ok, "registered kind must be *slack.Kind")

	sess := channelkinds.SessionInfo{
		Channel: &spiceboxv1alpha1.ChannelBinding{
			External: map[string]string{"channel_id": "C123"},
		},
	}

	fake := &fakeAudienceLookup{subjects: []string{"user:alice", "user:bob"}}
	WireChannelKindAudienceResolvers(fake)

	subs, err := sk.ResolveAudience(context.Background(), sess)
	require.NoError(t, err, "post-wiring resolve must succeed")
	assert.Equal(t, []string{"user:alice", "user:bob"}, subs)
	require.Len(t, fake.calls, 1, "fake must have been called exactly once")
	assert.Equal(t, "slack_channel:C123#view", fake.calls[0])
}

// TestLookupSidecarToolboxToolResourceMapping mirrors the MCPServer lookup for a
// SidecarToolbox: the LLM-facing prefixed name resolves back to the toolbox's
// bare-named ToolResourceMapping, so a declared sidecar tool participates in the
// info-leakage gate instead of being treated as undeclared.
func TestLookupSidecarToolboxToolResourceMapping(t *testing.T) {
	tb := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "ns"},
		Spec: spiceboxv1alpha1.SidecarToolboxSpec{
			Name:    "records",
			Version: "1",
			Tools:   []spiceboxv1alpha1.MCPServerTool{{Name: "read_record"}},
			ToolResourceMap: []spiceboxv1alpha1.ToolResourceMapping{
				{Tool: "read_record", Reads: &spiceboxv1alpha1.ToolReads{
					ResourceType: "pde_record", IDArg: "id", Permission: "view",
				}},
			},
		},
	}
	c := newLookupTestClient(t, tb)
	refs := []spiceboxv1alpha1.AgentClassSidecarToolboxRef{{Name: "pde", Ref: "records"}}

	t.Run("LLM-facing prefixed name resolves to the bare entry", func(t *testing.T) {
		got := LookupSidecarToolboxToolResourceMapping(context.Background(), c, "ns", refs, "pde_read_record")
		require.NotNil(t, got, "must find the entry from the LLM-facing name")
		assert.Equal(t, "read_record", got.Tool)
		require.NotNil(t, got.Reads)
		assert.Equal(t, "pde_record", got.Reads.ResourceType)
	})
	t.Run("bare name without prefix does NOT match", func(t *testing.T) {
		assert.Nil(t, LookupSidecarToolboxToolResourceMapping(context.Background(), c, "ns", refs, "read_record"))
	})
	t.Run("unknown tool under correct prefix returns nil", func(t *testing.T) {
		assert.Nil(t, LookupSidecarToolboxToolResourceMapping(context.Background(), c, "ns", refs, "pde_unknown"))
	})
}
