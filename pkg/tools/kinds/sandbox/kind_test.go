package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/contract"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "spec.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func sampleToolspec() *spiceboxv1alpha1.SpiceboxToolspec {
	return &spiceboxv1alpha1.SpiceboxToolspec{
		ObjectMeta: metav1.ObjectMeta{Name: "ts1"},
		Spec: spiceboxv1alpha1.SpiceboxToolspecSpec{
			Toolkit:          spiceboxv1alpha1.ToolspecToolkitRef{Name: "tk", Revision: "r1"},
			AllowSubcommands: []string{"say"},
			Deny: spiceboxv1alpha1.ToolspecDeny{
				Effects: spiceboxv1alpha1.ToolspecDenyEffects{Destructive: true},
			},
			Constraints: []spiceboxv1alpha1.ToolspecConstraint{
				{CEL: "true", Message: "always"},
			},
		},
		Status: spiceboxv1alpha1.SpiceboxToolspecStatus{
			ResolvedToolkit: "tk@r1",
			Conditions: []metav1.Condition{
				{Type: spiceboxv1alpha1.SpiceboxToolspecConditionValid, Status: metav1.ConditionTrue, Reason: "Compiled"},
			},
		},
	}
}

func TestKind_BasicMetadata(t *testing.T) {
	k := New()
	assert.Equal(t, "SpiceboxToolspec", k.Name())
	gvr := k.GVR()
	assert.Equal(t, "agentprimitives.authzed.com", gvr.Group)
	assert.Equal(t, "spiceboxtoolspecs", gvr.Resource)
	_, ok := k.NewObject().(*spiceboxv1alpha1.SpiceboxToolspec)
	assert.True(t, ok, "NewObject wrong type")
	_, ok = k.NewList().(*spiceboxv1alpha1.SpiceboxToolspecList)
	assert.True(t, ok, "NewList wrong type")
}

func TestKind_DecodeYAML_Match(t *testing.T) {
	const doc = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SpiceboxToolspec
metadata: {name: x}
spec:
  toolkit: {name: tk, revision: "r"}
  allowSubcommands: []
`
	obj, err := New().DecodeYAML([]byte(doc))
	require.NoError(t, err)
	cr, ok := obj.(*spiceboxv1alpha1.SpiceboxToolspec)
	require.True(t, ok, "type assertion failed")
	assert.Equal(t, "x", cr.Name)
	assert.Equal(t, "tk", cr.Spec.Toolkit.Name)
}

func TestKind_DecodeYAML_OtherKind(t *testing.T) {
	const doc = `
apiVersion: agentprimitives.authzed.com/v1alpha1
kind: MCPServer
metadata: {name: x}
`
	obj, err := New().DecodeYAML([]byte(doc))
	require.NoError(t, err)
	assert.Nil(t, obj)
}

func TestKind_Row(t *testing.T) {
	row := New().Row(sampleToolspec())
	assert.Equal(t, "SpiceboxToolspec", row.Kind)
	assert.Equal(t, "ts1", row.Name)
	assert.Contains(t, row.Status, "Valid=True")
	assert.Contains(t, row.Summary, "tk@r1")
}

func TestKind_Detail_HasAllSections(t *testing.T) {
	d := New().Detail(sampleToolspec())
	body := ""
	for _, s := range d.Sections {
		body += s.Title + "\n" + s.Body + "\n"
	}
	for _, want := range []string{
		"Toolkit",
		"tk@r1",
		"AllowSubcommands",
		"- say",
		"Deny",
		"destructive: true",
		"Constraints",
		"true",
		"Conditions",
		"Compiled",
	} {
		assert.Contains(t, body, want, "detail missing %q", want)
	}
}

func TestKind_ValidateFile(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantDiag bool
		check    func(t *testing.T, diags []contract.Diagnostic)
	}{
		{
			name: "valid spec: zero diagnostics",
			body: `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: [say]
constraints:
  - cel: 'call.subcommand == "say"'
    message: ok
`,
			wantDiag: false,
		},
		{
			name: "malformed CEL: 1 error diagnostic pointing at constraints[0]",
			body: `
name: s
version: "1"
toolkit: {name: t, revision: "r"}
allowSubcommands: []
constraints:
  - cel: 'call.subcommand =='
`,
			wantDiag: true,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
				assert.Contains(t, diags[0].Path, "constraints[0]")
			},
		},
		{
			name:     "structural error: empty name",
			body:     `name: ""`,
			wantDiag: true,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				require.Len(t, diags, 1)
				assert.Equal(t, "error", diags[0].Severity)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.body)
			diags, err := New().ValidateFile(path)
			require.NoError(t, err)
			if !tc.wantDiag {
				assert.Empty(t, diags)
				return
			}
			tc.check(t, diags)
		})
	}
}
