package pincmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"

	// The pin verbs dispatch over the pinning-kind registry; oap registers every
	// kind from main, so the --kind filter assertions below need the same set.
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/cli"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/image"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/oap"
	_ "github.com/authzed/openagentprimitives/pkg/authz/pinning/kinds/skill"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// ---------- truncateDigest ----------

func TestTruncateDigest(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "full sha256 digest truncated to 19 chars (sha256: prefix + 12 hex)",
			input: "sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			want:  "sha256:abcdef012345", // 7 + 12 = 19 chars
		},
		{
			name:  "exactly 19 chars unchanged",
			input: "sha256:abcdef012345", // 7 + 12 = 19 chars
			want:  "sha256:abcdef012345",
		},
		{
			name:  "short digest unchanged",
			input: "sha256:abc",
			want:  "sha256:abc",
		},
		{
			name:  "non-sha256 long digest truncated at 19",
			input: "ghcr.io/foo@sha256:abcdef0123456789abcdef",
			want:  "ghcr.io/foo@sha256:",
		},
		{
			name:  "empty string unchanged",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateDigest(tc.input)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------- pinDriftReason ----------

func TestPinDriftReason(t *testing.T) {
	cases := []struct {
		name  string
		conds []metav1.Condition
		want  string
	}{
		{
			name:  "no conditions → dash",
			conds: nil,
			want:  "-",
		},
		{
			name: "PinMatch condition",
			conds: []metav1.Condition{
				{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionTrue, Reason: spiceboxv1alpha1.ReasonPinMatch},
			},
			want: spiceboxv1alpha1.ReasonPinMatch,
		},
		{
			name: "PinDrifted condition",
			conds: []metav1.Condition{
				{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionFalse, Reason: spiceboxv1alpha1.ReasonPinDrifted},
			},
			want: spiceboxv1alpha1.ReasonPinDrifted,
		},
		{
			name: "unrelated condition → dash",
			conds: []metav1.Condition{
				{Type: "Valid", Status: metav1.ConditionTrue, Reason: "Ready"},
			},
			want: "-",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pinDriftReason(tc.conds)
			assert.Equal(t, tc.want, got)
		})
	}
}

// ---------- pinStatusRow ----------

func TestPinStatusRow(t *testing.T) {
	now := time.Now().UTC()
	metaNow := metav1.NewTime(now)

	cases := []struct {
		name       string
		kind       string
		objName    string
		pin        *spiceboxv1alpha1.PinRecord
		conds      []metav1.Condition
		wantCols   []string // substrings expected in the row output
		wantAbsent []string // substrings that must NOT appear
	}{
		{
			name:    "nil pin renders all dashes",
			kind:    "MCPServer",
			objName: "my-server",
			pin:     nil,
			conds:   nil,
			// The table pads cells with spaces; check the individual dash fields.
			wantCols: []string{"MCPServer", "my-server", "  -  "},
		},
		{
			name:    "MCPServer with full pin + PinMatch",
			kind:    "MCPServer",
			objName: "gh",
			pin: &spiceboxv1alpha1.PinRecord{
				Kind:       "mcp",
				Strength:   "unpinned",
				Digest:     "sha256:abcdef0123456789ff",
				Version:    "v1.2.3",
				ObservedAt: &metaNow,
			},
			conds: []metav1.Condition{
				{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionTrue, Reason: spiceboxv1alpha1.ReasonPinMatch},
			},
			// truncateDigest("sha256:abcdef0123456789ff") → "sha256:abcdef012345" (7+12=19)
			wantCols: []string{"MCPServer", "gh", "unpinned", "sha256:abcdef012345", "v1.2.3", "PinMatch"},
		},
		{
			name:    "SidecarToolbox with PinDrifted",
			kind:    "SidecarToolbox",
			objName: "my-tb",
			pin: &spiceboxv1alpha1.PinRecord{
				Kind:       "image",
				Strength:   "named",
				Digest:     "sha256:000111222333444555",
				Version:    "latest",
				ObservedAt: &metaNow,
			},
			conds: []metav1.Condition{
				{Type: spiceboxv1alpha1.PinDriftCondition, Status: metav1.ConditionFalse, Reason: spiceboxv1alpha1.ReasonPinDrifted},
			},
			// truncateDigest("sha256:000111222333444555") → "sha256:000111222333" (7+12=19)
			wantCols: []string{"SidecarToolbox", "my-tb", "named", "sha256:000111222333", "latest", "PinDrifted"},
		},
		{
			name:     "Skill with nil pin",
			kind:     "Skill",
			objName:  "my-skill",
			pin:      nil,
			conds:    nil,
			wantCols: []string{"Skill", "my-skill"},
		},
		{
			name:    "SpiceboxToolkit with frozen pin",
			kind:    "SpiceboxToolkit",
			objName: "mytoolkit",
			pin: &spiceboxv1alpha1.PinRecord{
				Kind:       "cli",
				Strength:   "frozen",
				Digest:     "sha256:deadbeef0123456789abc",
				Version:    "",
				ObservedAt: &metaNow,
			},
			conds: nil,
			// truncateDigest("sha256:deadbeef0123456789abc") → "sha256:deadbeef0123" (7+12=19)
			wantCols: []string{"SpiceboxToolkit", "mytoolkit", "frozen", "sha256:deadbeef0123"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			table := tui.NewTable(tui.NewTheme(tui.Caps{}), "KIND", "NAME", "STRENGTH",
				"DIGEST", "VERSION", "DRIFT", "MODE", "OBSERVED")
			pinStatusRow(table, tc.kind, tc.objName, tc.pin, tc.conds, "-")
			out := table.Render()
			for _, want := range tc.wantCols {
				assert.Contains(t, out, want, "output must contain %q", want)
			}
			for _, absent := range tc.wantAbsent {
				assert.NotContains(t, out, absent, "output must not contain %q", absent)
			}
		})
	}
}

// ---------- printToolNameDiff ----------

func TestPrintToolNameDiff(t *testing.T) {
	cases := []struct {
		name     string
		baseline []string
		live     []probe.Tool
		wantOut  string
	}{
		{
			name:     "no diff → empty output",
			baseline: []string{"read", "write"},
			live:     []probe.Tool{{Name: "read"}, {Name: "write"}},
			wantOut:  "",
		},
		{
			name:     "tool added",
			baseline: []string{"read"},
			live:     []probe.Tool{{Name: "read"}, {Name: "write"}},
			wantOut:  "+ write",
		},
		{
			name:     "tool removed",
			baseline: []string{"read", "delete"},
			live:     []probe.Tool{{Name: "read"}},
			wantOut:  "- delete",
		},
		{
			name:     "added and removed sorted",
			baseline: []string{"b_tool", "c_tool"},
			live:     []probe.Tool{{Name: "a_tool"}, {Name: "b_tool"}},
			wantOut:  "+ a_tool\n  - c_tool",
		},
		{
			name:     "empty baseline, live tools appear as added",
			baseline: nil,
			live:     []probe.Tool{{Name: "alpha"}, {Name: "beta"}},
			wantOut:  "+ alpha",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printToolNameDiff(&buf, tc.baseline, tc.live)
			out := buf.String()
			if tc.wantOut == "" {
				assert.Empty(t, out)
			} else {
				assert.Contains(t, out, tc.wantOut)
			}
		})
	}
}

// ---------- pin_update: setMCPServerRefreeze / setSidecarToolboxRefreeze ----------

func TestSetMCPServerRefreeze(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Name:    "gh",
				Version: "1",
				Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
			},
		},
	).Build()

	require.NoError(t, setMCPServerRefreeze(ctx, c, "default", "gh", "sha256:abcdef01234"))

	var got spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh"}, &got))
	assert.Equal(t, "sha256:abcdef01234", got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])
}

func TestSetSidecarToolboxRefreeze(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&spiceboxv1alpha1.SidecarToolbox{
			ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		},
	).Build()

	require.NoError(t, setSidecarToolboxRefreeze(ctx, c, "default", "my-tb", "sha256:deadbeef00001111"))

	var got spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-tb"}, &got))
	assert.Equal(t, "sha256:deadbeef00001111", got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])
}

// TestPinUpdateImageRequiresTo verifies that `oap pin update image <name>`
// without --to returns an error. Validation runs before Bundle(), so no
// kubeconfig is needed.
func TestPinUpdateImageRequiresTo(t *testing.T) {
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"image", "my-tb"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to")
}

// TestPinUpdateSkillRejected verifies that `oap pin update skill <name>` is
// rejected with an explanatory error.
func TestPinUpdateSkillRejected(t *testing.T) {
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"skill", "my-skill"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "skill")
	assert.Contains(t, err.Error(), "declared in spec")
}

// TestPinUpdateCLIRejected verifies that `oap pin update cli <name>` is
// rejected with an explanatory error.
func TestPinUpdateCLIRejected(t *testing.T) {
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"cli", "my-toolkit"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declared in spec")
}

// TestPinUpdateMCPWithTo verifies that `oap pin update mcp <name> --to <hash>`
// passes --to validation (which runs before Bundle()) and that the annotation
// helper writes the correct value. The test drives newPinUpdateCmd for the
// validation path, and the setMCPServerRefreeze helper for the write path.
func TestPinUpdateMCPWithTo(t *testing.T) {
	validHash := "sha256:" + strings.Repeat("a", 64)

	// Validate flag path: a well-formed hash must not be rejected by the regex
	// gate. Bundle() will fail without a kubeconfig, but the error must not
	// mention --to.
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"mcp", "gh", "--to", validHash})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	if err != nil {
		assert.NotContains(t, err.Error(), `--to must be`, "valid hash must not trigger --to validation error")
	}

	// Helper-level coverage: the annotation is written with the exact value.
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(
		&spiceboxv1alpha1.MCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
			Spec: spiceboxv1alpha1.MCPServerSpec{
				Name:    "gh",
				Version: "1",
				Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
			},
		},
	).Build()

	require.NoError(t, setMCPServerRefreeze(ctx, c, "default", "gh", validHash))
	var got spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh"}, &got))
	assert.Equal(t, validHash, got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze],
		"annotation must be set to the requested identity")
}

// TestPinUpdateToValidation verifies that malformed --to values are rejected
// before Bundle(), so no kubeconfig is required.
func TestPinUpdateToValidation(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		to      string
		wantErr string
	}{
		{
			name:    "mcp: bad --to (too short)",
			kind:    "mcp",
			to:      "sha256:abc",
			wantErr: `--to must be "sha256:" + 64 hex chars`,
		},
		{
			name:    "mcp: bad --to (uppercase hex rejected)",
			kind:    "mcp",
			to:      "sha256:" + strings.Repeat("A", 64),
			wantErr: `--to must be "sha256:" + 64 hex chars`,
		},
		{
			name:    "mcp: bad --to (missing sha256: prefix)",
			kind:    "mcp",
			to:      strings.Repeat("a", 64),
			wantErr: `--to must be "sha256:" + 64 hex chars`,
		},
		{
			name:    "image: bad --to (too short)",
			kind:    "image",
			to:      "sha256:deadbeef",
			wantErr: `--to must be "sha256:" + 64 hex chars`,
		},
		{
			name:    "image: bad --to (uppercase hex rejected)",
			kind:    "image",
			to:      "sha256:" + strings.Repeat("F", 64),
			wantErr: `--to must be "sha256:" + 64 hex chars`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &apcmd.Globals{Namespace: "default"}
			cmd := newPinUpdateCmd(g)
			cmd.SetArgs([]string{tc.kind, "my-resource", "--to", tc.to})
			var outBuf bytes.Buffer
			cmd.SetOut(&outBuf)

			err := cmd.Execute()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// ---------- pin update --current (single resource via runPinUpdateOne) ----------

// validDriftHash is a well-formed sha256 digest used across --current tests.
const validDriftHash = "sha256:cccc0000111122223333444455556666aaaabbbbccccdddd0000111122223333"

// buildFakePinClient builds a fake client seeded with the given objects,
// with SidecarToolbox and MCPServer registered as status subresources.
func buildFakePinClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(kube.Scheme).
		WithObjects(objs...).
		WithStatusSubresource(
			&spiceboxv1alpha1.SidecarToolbox{},
			&spiceboxv1alpha1.MCPServer{},
		).Build()
}

// TestPinUpdateOne_Image_Current_WithDriftKey verifies that runPinUpdateOne with
// kind=image and useCurrent=true reads Details["observedDriftDigest"] and sets
// the refreeze annotation to that value, and that the output mentions the digest.
func TestPinUpdateOne_Image_Current_WithDriftKey(t *testing.T) {
	ctx := context.Background()
	sidecar := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:     "image",
				Strength: "named",
				Digest:   "sha256:" + strings.Repeat("a", 64),
				Details:  map[string]string{detailKeyObservedDrift: validDriftHash},
			},
		},
	}
	c := buildFakePinClient(t, sidecar)
	b := &kube.Bundle{Controller: c, Namespace: "default"}

	var out bytes.Buffer
	err := runPinUpdateOne(ctx, &out, b, "image", "my-tb", "", true)
	require.NoError(t, err)

	var got spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-tb"}, &got))
	assert.Equal(t, validDriftHash, got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze],
		"refreeze annotation must be set to the observed drift digest")
	assert.Contains(t, out.String(), validDriftHash, "output must mention the accepted digest")
}

// TestPinUpdateOne_Image_Current_NoDriftKey verifies that runPinUpdateOne with
// kind=image and useCurrent=true returns the "no observed drift recorded" error
// when the Details key is absent.
func TestPinUpdateOne_Image_Current_NoDriftKey(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		sidecar *spiceboxv1alpha1.SidecarToolbox
	}{
		{
			name: "nil pin → no drift key",
			sidecar: &spiceboxv1alpha1.SidecarToolbox{
				ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
			},
		},
		{
			name: "pin without observedDriftDigest → no drift key",
			sidecar: &spiceboxv1alpha1.SidecarToolbox{
				ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
				Status: spiceboxv1alpha1.SidecarToolboxStatus{
					Pin: &spiceboxv1alpha1.PinRecord{
						Kind:    "image",
						Digest:  "sha256:" + strings.Repeat("a", 64),
						Details: map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
					},
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildFakePinClient(t, tc.sidecar)
			b := &kube.Bundle{Controller: c, Namespace: "default"}

			var out bytes.Buffer
			err := runPinUpdateOne(ctx, &out, b, "image", "my-tb", "", true)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no observed drift recorded",
				"error must explain why there is nothing to accept")
		})
	}
}

// TestPinUpdateOne_MCP_Current_WithDriftKey verifies that runPinUpdateOne with
// kind=mcp and useCurrent=true reads Details["observedDriftDigest"] and sets
// the refreeze annotation without probing the server.
func TestPinUpdateOne_MCP_Current_WithDriftKey(t *testing.T) {
	ctx := context.Background()
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "gh",
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "mcp",
				Digest:  "sha256:" + strings.Repeat("a", 64),
				Details: map[string]string{detailKeyObservedDrift: validDriftHash},
			},
		},
	}
	c := buildFakePinClient(t, srv)
	b := &kube.Bundle{Controller: c, Namespace: "default"}

	var out bytes.Buffer
	err := runPinUpdateOne(ctx, &out, b, "mcp", "gh", "", true)
	require.NoError(t, err)

	var got spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh"}, &got))
	assert.Equal(t, validDriftHash, got.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze],
		"refreeze annotation must be set to the observed drift digest")
	// Server URL is not reachable in tests; success proves no probe was attempted.
}

// TestPinUpdateImage_BareErrors verifies that `oap pin update image <name>`
// without --to or --current returns an error mentioning both flags.
func TestPinUpdateImage_BareErrors(t *testing.T) {
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"image", "my-tb"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to", "error must mention --to")
	assert.Contains(t, err.Error(), "--current", "error must mention --current")
}

// TestPinUpdateToAndCurrentMutuallyExclusive verifies that --to and --current
// together produce a usage error.
func TestPinUpdateToAndCurrentMutuallyExclusive(t *testing.T) {
	validHash := "sha256:" + strings.Repeat("a", 64)
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"image", "my-tb", "--to", validHash, "--current"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--to and --current are mutually exclusive")
}

// ---------- pin update --all ----------

// TestPinUpdateAll_AcceptsMultipleDrifted verifies that --all --current sets
// the refreeze annotation on every drifted MCPServer and SidecarToolbox,
// prints a summary table, and prints the accepted count.
func TestPinUpdateAll_AcceptsMultipleDrifted(t *testing.T) {
	ctx := context.Background()

	driftedMCP := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "gh",
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "mcp",
				Digest:  "sha256:" + strings.Repeat("a", 64),
				Details: map[string]string{"observedDriftDigest": validDriftHash},
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}
	driftedImage := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "image",
				Digest:  "sha256:" + strings.Repeat("b", 64),
				Details: map[string]string{"observedDriftDigest": validDriftHash},
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}

	c := buildFakePinClient(t, driftedMCP, driftedImage)

	var outBuf bytes.Buffer
	err := runPinUpdateAll(ctx, newCmdWithOut(&outBuf), tui.NewTheme(tui.Caps{}), &kube.Bundle{Controller: c, Namespace: "default"}, "")
	require.NoError(t, err)

	out := outBuf.String()
	assert.Contains(t, out, "gh", "summary must name the MCPServer")
	assert.Contains(t, out, "my-tb", "summary must name the SidecarToolbox")
	assert.Contains(t, out, "accepted 2 of 2 drifted", "count line must reflect both resources")

	// Annotations must be set on both resources.
	var gotMCP spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh"}, &gotMCP))
	assert.Equal(t, validDriftHash, gotMCP.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])

	var gotSidecar spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-tb"}, &gotSidecar))
	assert.Equal(t, validDriftHash, gotSidecar.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])
}

// TestPinUpdateAll_KindFilterImage verifies that --all --kind image only
// touches SidecarToolboxes and leaves MCPServers unchanged.
func TestPinUpdateAll_KindFilterImage(t *testing.T) {
	ctx := context.Background()

	driftedMCP := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: "default"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "gh",
			Version: "1",
			Server:  spiceboxv1alpha1.MCPServerServer{URL: "https://example.com/mcp"},
		},
		Status: spiceboxv1alpha1.MCPServerStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "mcp",
				Digest:  "sha256:" + strings.Repeat("a", 64),
				Details: map[string]string{"observedDriftDigest": validDriftHash},
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}
	driftedImage := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "image",
				Digest:  "sha256:" + strings.Repeat("b", 64),
				Details: map[string]string{"observedDriftDigest": validDriftHash},
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}

	c := buildFakePinClient(t, driftedMCP, driftedImage)

	var outBuf bytes.Buffer
	err := runPinUpdateAll(ctx, newCmdWithOut(&outBuf), tui.NewTheme(tui.Caps{}), &kube.Bundle{Controller: c, Namespace: "default"}, "image")
	require.NoError(t, err)

	// MCPServer must NOT have a refreeze annotation.
	var gotMCP spiceboxv1alpha1.MCPServer
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "gh"}, &gotMCP))
	_, hasMCPAnnotation := gotMCP.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze]
	assert.False(t, hasMCPAnnotation, "--kind image must not touch MCPServers")

	// SidecarToolbox must have the annotation.
	var gotSidecar spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-tb"}, &gotSidecar))
	assert.Equal(t, validDriftHash, gotSidecar.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])
}

// TestPinUpdateAll_PositionalArgError verifies that --all with positional args
// returns a usage error.
func TestPinUpdateAll_PositionalArgError(t *testing.T) {
	g := &apcmd.Globals{Namespace: "default"}
	cmd := newPinUpdateCmd(g)
	cmd.SetArgs([]string{"--all", "--current", "image", "my-tb"})
	var outBuf bytes.Buffer
	cmd.SetOut(&outBuf)

	err := cmd.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--all accepts no positional arguments")
}

// TestPinUpdateAll_NothingDrifted verifies that --all with no drifted resources
// prints the "nothing drifted" message.
func TestPinUpdateAll_NothingDrifted(t *testing.T) {
	ctx := context.Background()
	c := buildFakePinClient(t)

	var outBuf bytes.Buffer
	err := runPinUpdateAll(ctx, newCmdWithOut(&outBuf), tui.NewTheme(tui.Caps{}), &kube.Bundle{Controller: c, Namespace: "default"}, "")
	require.NoError(t, err)
	assert.Contains(t, outBuf.String(), "nothing drifted")
}

// TestPinUpdateAll_PartialFailureExitsNonZero verifies that --all returns a
// non-zero exit when at least one drifted resource could not be accepted.  The
// summary table must still be printed before the error so callers can see which
// rows succeeded and which were skipped.
func TestPinUpdateAll_PartialFailureExitsNonZero(t *testing.T) {
	ctx := context.Background()

	// One image resource with a valid drift digest (will succeed).
	driftedImage := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tb", Namespace: "default"},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:    "image",
				Digest:  "sha256:" + strings.Repeat("b", 64),
				Details: map[string]string{detailKeyObservedDrift: validDriftHash},
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}
	// One image resource whose drift digest is absent (will be skipped).
	noDigestImage := &spiceboxv1alpha1.SidecarToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "no-digest-tb", Namespace: "default"},
		Status: spiceboxv1alpha1.SidecarToolboxStatus{
			Pin: &spiceboxv1alpha1.PinRecord{
				Kind:   "image",
				Digest: "sha256:" + strings.Repeat("c", 64),
				// No observedDriftDigest key → will be skipped.
			},
			Conditions: []metav1.Condition{
				{
					Type:   spiceboxv1alpha1.PinDriftCondition,
					Status: metav1.ConditionFalse,
					Reason: spiceboxv1alpha1.ReasonPinDrifted,
				},
			},
		},
	}

	c := buildFakePinClient(t, driftedImage, noDigestImage)

	var outBuf bytes.Buffer
	err := runPinUpdateAll(ctx, newCmdWithOut(&outBuf), tui.NewTheme(tui.Caps{}), &kube.Bundle{Controller: c, Namespace: "default"}, "image")

	// Must return a non-zero error.
	require.Error(t, err, "--all with a skipped row must exit non-zero")
	assert.Contains(t, err.Error(), "accepted 1 of 2", "error must report the counts")
	assert.Contains(t, err.Error(), "skipped/failed", "error must mention skipped/failed")

	// The summary table must have been printed to out before the error.
	out := outBuf.String()
	assert.Contains(t, out, "my-tb", "table must include the accepted row")
	assert.Contains(t, out, "no-digest-tb", "table must include the skipped row")
	assert.Contains(t, out, "SKIPPED", "table must label the skipped row")

	// The accepted resource must still have its annotation set.
	var gotSidecar spiceboxv1alpha1.SidecarToolbox
	require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "my-tb"}, &gotSidecar))
	assert.Equal(t, validDriftHash, gotSidecar.Annotations[spiceboxv1alpha1.AnnotationPinRefreeze])
}

// newCmdWithOut returns a minimal cobra.Command whose output is wired to w.
// Used by tests that call runPinUpdateAll directly.
func newCmdWithOut(w *bytes.Buffer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(w)
	return cmd
}

// ---------- pin status --kind filter ----------

// TestPinStatusKindFilter_MCPOnly verifies that --kind mcp shows only MCPServer rows.
func TestPinStatusKindFilter_MCPOnly(t *testing.T) {
	filter, err := kindFilterName("mcp")
	require.NoError(t, err)
	assert.Equal(t, "mcp", filter)

	assert.True(t, kindMatches(filter, "mcp"), "mcp must match mcp filter")
	assert.False(t, kindMatches(filter, "image"), "image must not match mcp filter")
	assert.False(t, kindMatches(filter, "skill"), "skill must not match mcp filter")
	assert.False(t, kindMatches(filter, "cli"), "cli must not match mcp filter")
}

// TestPinStatusKindFilter_ImageOnly verifies that --kind image shows only SidecarToolbox rows.
func TestPinStatusKindFilter_ImageOnly(t *testing.T) {
	filter, err := kindFilterName("image")
	require.NoError(t, err)
	assert.Equal(t, "image", filter)

	assert.True(t, kindMatches(filter, "image"), "image must match image filter")
	assert.False(t, kindMatches(filter, "mcp"), "mcp must not match image filter")
}

// TestPinStatusKindFilter_SkillCoversSkillAndClusterSkill verifies that
// --kind skill enables rows for both Skill and ClusterSkill (both carry kind="skill").
func TestPinStatusKindFilter_SkillCoversSkillAndClusterSkill(t *testing.T) {
	filter, err := kindFilterName("skill")
	require.NoError(t, err)
	assert.Equal(t, "skill", filter)

	assert.True(t, kindMatches(filter, "skill"), "skill row must match skill filter")
	assert.False(t, kindMatches(filter, "mcp"), "mcp must not match skill filter")
}

// TestPinStatusKindFilter_Empty verifies that an empty filter matches all kinds.
func TestPinStatusKindFilter_Empty(t *testing.T) {
	filter, err := kindFilterName("")
	require.NoError(t, err)
	assert.Equal(t, "", filter)

	assert.True(t, kindMatches(filter, "mcp"), "empty filter must match mcp")
	assert.True(t, kindMatches(filter, "image"), "empty filter must match image")
	assert.True(t, kindMatches(filter, "skill"), "empty filter must match skill")
	assert.True(t, kindMatches(filter, "cli"), "empty filter must match cli")
}

// TestPinStatusKindFilter_UnknownReturnsError verifies that an unregistered kind
// name produces an error listing valid kinds.
func TestPinStatusKindFilter_UnknownReturnsError(t *testing.T) {
	_, err := kindFilterName("bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown --kind")
	assert.Contains(t, err.Error(), "bogus")
	// Error must list at least one valid kind so the user knows what's accepted.
	assert.Contains(t, err.Error(), "mcp")
}

// TestPinStatusKindFilter_CaseInsensitive verifies that "MCP" and "IMAGE" are
// treated the same as their lowercase equivalents.
func TestPinStatusKindFilter_CaseInsensitive(t *testing.T) {
	for _, input := range []string{"MCP", "mcp", "Mcp"} {
		filter, err := kindFilterName(input)
		require.NoError(t, err, "kind %q should be valid", input)
		assert.Equal(t, "mcp", filter)
	}
}

// ---------- effective-mode column ----------

func TestEffectiveModeFor(t *testing.T) {
	eff := &spiceboxv1alpha1.EffectivePinning{
		Cluster: &spiceboxv1alpha1.PinningPolicy{
			Rules: []spiceboxv1alpha1.PinningRule{
				{Kind: "mcp", MinStrength: "frozen", Mode: spiceboxv1alpha1.PinModeBlock},
				{Kind: "image", MinStrength: "frozen"}, // empty mode = approve default
				{Kind: "skill", Mode: spiceboxv1alpha1.PinModeWarn},
			},
			Bypass: []spiceboxv1alpha1.PinningBypass{
				{Kind: "mcp", Name: "trusted-server", Reason: "vendor-reviewed"},
			},
		},
	}

	cases := []struct {
		name       string
		kind, item string
		want       string
	}{
		{name: "block rule renders block", kind: "mcp", item: "gh", want: "block"},
		{name: "empty rule mode renders the approve default", kind: "image", item: "toolbox", want: "approve"},
		{name: "warn rule renders warn", kind: "skill", item: "github.com/org/repo//skills/x", want: "warn"},
		{name: "bypassed item renders bypassed", kind: "mcp", item: "trusted-server", want: "bypassed"},
		{name: "kind with no rule renders dash", kind: "cli", item: "gh-cli", want: "-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, effectiveModeFor(eff, tc.kind, tc.item))
		})
	}

	t.Run("nil effective pinning renders dash", func(t *testing.T) {
		assert.Equal(t, "-", effectiveModeFor(nil, "mcp", "gh"))
	})
}

func TestFetchEffectivePinning(t *testing.T) {
	t.Run("no settings anywhere yields nil (MODE column all dashes)", func(t *testing.T) {
		c := fake.NewClientBuilder().WithScheme(kube.Scheme).Build()
		eff, err := fetchEffectivePinning(context.Background(), c, "default")
		require.NoError(t, err)
		assert.Nil(t, eff)
	})

	t.Run("cluster pinning rules surface, per kind", func(t *testing.T) {
		cas := &spiceboxv1alpha1.ClusterAgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterAgentSettingsName},
			Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{
				Pinning: &spiceboxv1alpha1.PinningPolicy{
					Rules: []spiceboxv1alpha1.PinningRule{
						{Kind: "mcp", Mode: spiceboxv1alpha1.PinModeWarn},
						{Kind: "skill", MinStrength: "frozen", Mode: spiceboxv1alpha1.PinModeBlock},
					},
				},
			}},
		}
		c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(cas).Build()
		eff, err := fetchEffectivePinning(context.Background(), c, "default")
		require.NoError(t, err)
		require.NotNil(t, eff)
		require.NotNil(t, eff.Cluster)
		assert.Equal(t, "warn", effectiveModeFor(eff, "mcp", "anything"))
		assert.Equal(t, "block", effectiveModeFor(eff, "skill", "github.com/org/repo//skills/x@v1"))
	})

	t.Run("namespace tier participates", func(t *testing.T) {
		as := &spiceboxv1alpha1.AgentSettings{
			ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.AgentSettingsName, Namespace: "team-a"},
			Spec: spiceboxv1alpha1.SettingsSpec{Limits: &spiceboxv1alpha1.SettingsLimits{
				Pinning: &spiceboxv1alpha1.PinningPolicy{
					Rules: []spiceboxv1alpha1.PinningRule{{Kind: "image", Mode: spiceboxv1alpha1.PinModeBlock}},
				},
			}},
		}
		c := fake.NewClientBuilder().WithScheme(kube.Scheme).WithObjects(as).Build()
		eff, err := fetchEffectivePinning(context.Background(), c, "team-a")
		require.NoError(t, err)
		require.NotNil(t, eff)
		assert.Equal(t, "block", effectiveModeFor(eff, "image", "any-toolbox"))
		// A different namespace sees no AgentSettings.
		effOther, err := fetchEffectivePinning(context.Background(), c, "team-b")
		require.NoError(t, err)
		assert.Nil(t, effOther)
	})
}
