// pkg/controllers/agentclass/permission_validation_test.go
//
// Pure-function tests for validatePermissions: no envtest required.
// Exercises the slice-2 mode-aware toolkit-subcommand walk.
package agentclass

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// makeStateMutatingToolkit returns a SpiceboxToolkit with one destructive
// subcommand and (optionally) a per-subcommand Permission.
func makeStateMutatingToolkit(name string, perm *authz.Permission, destructive bool, writes []string) spiceboxv1alpha1.SpiceboxToolkit {
	return spiceboxv1alpha1.SpiceboxToolkit{
		Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
			Name: name,
			Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
				{
					Path: []string{"rm"},
					Effects: spiceboxv1alpha1.ToolkitEffects{
						Destructive: destructive,
						Writes:      writes,
					},
					Permission: perm,
				},
			},
		},
	}
}

// classInMode returns a minimal AgentClass set to the given ToolAuthMode.
func classInMode(mode string) *spiceboxv1alpha1.AgentClass {
	return &spiceboxv1alpha1.AgentClass{
		Spec: spiceboxv1alpha1.AgentClassSpec{
			Authz: &spiceboxv1alpha1.AuthzBlock{
				ToolCalls: &spiceboxv1alpha1.ToolCallsAuthz{Mode: mode},
			},
		},
	}
}

// readwritePerm returns a well-formed readwrite permission for a destructive
// subcommand.
func readwritePerm() *authz.Permission {
	return &authz.Permission{
		StateImpact: authz.Readwrite,
		Check: &authz.PermissionCheck{
			ResourceType:       "git_repo",
			ResourceIDTemplate: "{repo}",
			Permission:         "write",
		},
	}
}

// TestValidatePermissions_ToolkitSubcommandWalk collapses the seven
// individual "TestValidation_*" cases into one table-driven sweep. Each
// case constructs an AgentClass + toolkit shape and asserts the reason
// validatePermissions returns.
func TestValidatePermissions_ToolkitSubcommandWalk(t *testing.T) {
	cases := []struct {
		name       string
		class      *spiceboxv1alpha1.AgentClass
		toolkit    spiceboxv1alpha1.SpiceboxToolkit
		wantReason string
	}{
		{
			name:       "permissive mode + destructive no-perm subcommand → skip walk, no error",
			class:      classInMode("permissive"),
			toolkit:    makeStateMutatingToolkit("git", nil, true, nil),
			wantReason: "",
		},
		{
			name:       "disabled mode + writes-only no-perm subcommand → skip walk, no error",
			class:      classInMode("disabled"),
			toolkit:    makeStateMutatingToolkit("git", nil, false, []string{"index"}),
			wantReason: "",
		},
		{
			name:       "enforcing + destructive no-perm subcommand → ReasonToolPermissionMissing",
			class:      classInMode("enforcing"),
			toolkit:    makeStateMutatingToolkit("git", nil, true, nil),
			wantReason: spiceboxv1alpha1.ReasonToolPermissionMissing,
		},
		{
			name:       "enforcing + writes-only no-perm subcommand → ReasonToolPermissionMissing",
			class:      classInMode("enforcing"),
			toolkit:    makeStateMutatingToolkit("git", nil, false, []string{"index"}),
			wantReason: spiceboxv1alpha1.ReasonToolPermissionMissing,
		},
		{
			name:       "enforcing + stateless perm on destructive → ReasonPermissionSpecInvalid (would bypass authz)",
			class:      classInMode("enforcing"),
			toolkit:    makeStateMutatingToolkit("git", &authz.Permission{StateImpact: authz.Stateless}, true, nil),
			wantReason: spiceboxv1alpha1.ReasonPermissionSpecInvalid,
		},
		{
			name:       "enforcing + well-formed readwrite perm → valid",
			class:      classInMode("enforcing"),
			toolkit:    makeStateMutatingToolkit("git", readwritePerm(), true, nil),
			wantReason: "",
		},
		{
			name:  "enforcing + non-mutating subcommand without perm → valid (perm is optional)",
			class: classInMode("enforcing"),
			toolkit: spiceboxv1alpha1.SpiceboxToolkit{
				Spec: spiceboxv1alpha1.SpiceboxToolkitSpec{
					Name: "git",
					Subcommands: []spiceboxv1alpha1.ToolkitSubcommand{
						{
							Path:    []string{"log"},
							Effects: spiceboxv1alpha1.ToolkitEffects{}, // none
							// Permission intentionally nil — inherits.
						},
					},
				},
			},
			wantReason: "",
		},
		{
			name:       "empty ToolAuthMode + destructive no-perm subcommand → defaults to enforcing",
			class:      &spiceboxv1alpha1.AgentClass{}, // mode unset
			toolkit:    makeStateMutatingToolkit("git", nil, true, nil),
			wantReason: spiceboxv1alpha1.ReasonToolPermissionMissing,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := validatePermissions(tc.class, nil, []spiceboxv1alpha1.SpiceboxToolkit{tc.toolkit}, nil)
			assert.Equal(t, tc.wantReason, reason, "validatePermissions reason; msg=%q", msg)
		})
	}
}

// TestEffectiveToolAuthMode covers the helper directly: empty / invalid
// strings default to enforcing.
func TestEffectiveToolAuthMode(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "enforcing"},
		{"bogus", "enforcing"},
		{"enforcing", "enforcing"},
		{"permissive", "permissive"},
		{"disabled", "disabled"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			assert.Equal(t, c.want, effectiveToolAuthMode(classInMode(c.in)),
				"effectiveToolAuthMode(%q)", c.in)
		})
	}
}

func TestValidateSiteURL_AcceptsHTTPS(t *testing.T) {
	err := validateSiteURL("https://linear.app")
	require.NoError(t, err)
}

func TestValidateSiteURL_AcceptsHTTP(t *testing.T) {
	err := validateSiteURL("http://example.invalid")
	require.NoError(t, err)
}

func TestValidateSiteURL_AcceptsEmpty(t *testing.T) {
	err := validateSiteURL("")
	require.NoError(t, err, "empty SiteURL is allowed (field is +optional)")
}

func TestValidateSiteURL_RejectsBadScheme(t *testing.T) {
	cases := []string{
		"file:///etc/passwd",
		"data:text/html,<script>",
		"javascript:alert(1)",
		"ftp://example.invalid",
		"ws://example.invalid",
		"://no-scheme",
		"not a url",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			err := validateSiteURL(in)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "siteURL")
		})
	}
}

// TestValidatePermissions_MCPUIAppTools_ReusesToolPermissionGuarantee locks
// in Task 3's core claim: opting an MCPServer into mcpUiAppTools does NOT
// bypass the existing per-tool Permission requirement. An app-visible tool
// (Visibility: ["app"]) with no Permission block must still fail exactly
// like any other unpermissioned tool — validatePermissions needs no new
// app-tool-specific check, because the existing walk over srv.Spec.Tools
// already covers every tool regardless of Visibility.
func TestValidatePermissions_MCPUIAppTools_ReusesToolPermissionGuarantee(t *testing.T) {
	srv := spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "widget-server"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			MCPUIAppTools: &spiceboxv1alpha1.MCPUIAppToolsSpec{Enabled: true},
			Tools: []spiceboxv1alpha1.MCPServerTool{
				{
					Name:       "list_pages",
					Visibility: []string{"app"},
					// Permission intentionally nil.
				},
			},
		},
	}

	reason, msg := validatePermissions(&spiceboxv1alpha1.AgentClass{}, []spiceboxv1alpha1.MCPServer{srv}, nil, nil)
	assert.Equal(t, spiceboxv1alpha1.ReasonToolPermissionMissing, reason, "msg=%q", msg)
	assert.NotEmpty(t, msg)
}

// TestIsStateMutating covers the helper used by validateToolkitSubcommands.
func TestIsStateMutating(t *testing.T) {
	cases := []struct {
		name    string
		effects spiceboxv1alpha1.ToolkitEffects
		want    bool
	}{
		{"destructive=true → state-mutating", spiceboxv1alpha1.ToolkitEffects{Destructive: true}, true},
		{"writes non-empty → state-mutating", spiceboxv1alpha1.ToolkitEffects{Writes: []string{"x"}}, true},
		{"empty effects → not state-mutating", spiceboxv1alpha1.ToolkitEffects{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isStateMutating(tc.effects))
		})
	}
}
