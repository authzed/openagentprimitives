package browsertoolbox

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

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

func sampleCR() *spiceboxv1alpha1.BrowserToolbox {
	return &spiceboxv1alpha1.BrowserToolbox{
		ObjectMeta: metav1.ObjectMeta{Name: "btb1", Namespace: "ns1"},
		Spec: spiceboxv1alpha1.BrowserToolboxSpec{
			Access: spiceboxv1alpha1.BrowserAccess{
				Mode: "allowlist",
				Sites: []spiceboxv1alpha1.BrowserSite{
					{Host: "example.com"},
					{Host: "exact.example.com", Exact: true},
				},
			},
			Interaction: "readonly",
			State:       "ephemeral",
		},
	}
}

// findDiag returns the first diagnostic at path, so tests can assert on a
// specific finding without coupling to the full diagnostic list (and its
// order) produced for the rest of the fixture.
func findDiag(diags []contract.Diagnostic, path string) (contract.Diagnostic, bool) {
	for _, d := range diags {
		if d.Path == path {
			return d, true
		}
	}
	return contract.Diagnostic{}, false
}

func TestKind_BasicMetadata(t *testing.T) {
	k := New()
	assert.Equal(t, "BrowserToolbox", k.Name())
	gvr := k.GVR()
	assert.Equal(t, "agentprimitives.authzed.com", gvr.Group)
	assert.Equal(t, "browsertoolboxes", gvr.Resource)
	_, ok := k.NewObject().(*spiceboxv1alpha1.BrowserToolbox)
	assert.True(t, ok, "NewObject wrong type")
	_, ok = k.NewList().(*spiceboxv1alpha1.BrowserToolboxList)
	assert.True(t, ok, "NewList wrong type")
}

func TestKind_DecodeYAML(t *testing.T) {
	cases := []struct {
		name    string
		doc     string
		wantNil bool
		check   func(t *testing.T, obj client.Object)
	}{
		{
			name: "SidecarToolbox doc is not claimed",
			doc: `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: SidecarToolbox
metadata: { name: x }
spec: {}
`,
			wantNil: true,
		},
		{
			name: "BrowserToolbox doc decodes to the typed object",
			doc: `apiVersion: agentprimitives.authzed.com/v1alpha1
kind: BrowserToolbox
metadata: { name: x }
spec:
  access:
    mode: allowlist
    sites:
      - host: example.com
  interaction: readonly
`,
			wantNil: false,
			check: func(t *testing.T, obj client.Object) {
				cr, ok := obj.(*spiceboxv1alpha1.BrowserToolbox)
				require.True(t, ok, "expected *BrowserToolbox, got %T", obj)
				assert.Equal(t, "x", cr.Name)
				assert.Equal(t, "allowlist", cr.Spec.Access.Mode)
				assert.Equal(t, "readonly", cr.Spec.Interaction)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj, err := New().DecodeYAML([]byte(tc.doc))
			require.NoError(t, err)
			if tc.wantNil {
				assert.Nil(t, obj)
				return
			}
			require.NotNil(t, obj)
			tc.check(t, obj)
		})
	}
}

func TestKind_MatchFlat(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{
			name: "access.mode present, no source/server: matches",
			data: `access:
  mode: allowlist
  sites:
    - host: example.com
interaction: readonly
`,
			want: true,
		},
		{
			name: "sidecar-shaped doc: does not match",
			data: `name: redditro
version: "1"
source: { image: ghcr.io/x:v1 }
sandbox: { class: sidecar-sandbox-default }
transport: { port: 8080 }
upstreamAuth: { provider: reddit-app }
tools: []
`,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, New().MatchFlat([]byte(tc.data)))
		})
	}
}

func TestKind_ValidateFile(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, diags []contract.Diagnostic)
	}{
		{
			name: "valid allowlist with two sites: no diagnostics",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
    - host: other.example.org
interaction: act
state: ephemeral
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			name: "bad mode: sideways errors naming access.mode",
			body: `
access:
  mode: sideways
  sites:
    - host: example.com
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.mode")
				require.True(t, ok, "expected a diagnostic at path access.mode, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "sideways")
			},
		},
		{
			name: "bad host: https://aa.com errors via CanonicalHost",
			body: `
access:
  mode: allowlist
  sites:
    - host: https://aa.com
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.sites[0].host")
				require.True(t, ok, "expected a diagnostic at path access.sites[0].host, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "scheme")
			},
		},
		{
			name: "duplicate host entries error",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
    - host: example.com
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.sites[1].host")
				require.True(t, ok, "expected a diagnostic at path access.sites[1].host, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "duplicate")
			},
		},
		{
			name: "allowlist with zero sites: warning",
			body: `
access:
  mode: allowlist
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.sites")
				require.True(t, ok, "expected a diagnostic at path access.sites, got %+v", diags)
				assert.Equal(t, "warning", d.Severity)
				assert.Equal(t, "allowlist with no sites denies all browsing", d.Message)
			},
		},
		{
			name: "bad interaction: write errors",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
interaction: write
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "interaction")
				require.True(t, ok, "expected a diagnostic at path interaction, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "write")
			},
		},
		{
			// Mirrors the first CEL rule on BrowserToolbox.Spec: a blocklist
			// (general-access) browser cannot carry an identity.
			name: "blocklist + identity: errors",
			body: `
access:
  mode: blocklist
  sites:
    - host: example.com
  identity: joey-airlines
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.identity")
				require.True(t, ok, "expected a diagnostic at path access.identity, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "blocklist")
			},
		},
		{
			// Mirrors the first CEL rule on BrowserToolbox.Spec: a blocklist
			// (general-access) browser cannot carry identitySites either.
			name: "blocklist + identitySites: errors",
			body: `
access:
  mode: blocklist
  sites:
    - host: example.com
  identitySites: true
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.identitySites")
				require.True(t, ok, "expected a diagnostic at path access.identitySites, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "blocklist")
			},
		},
		{
			// Mirrors the second CEL rule on BrowserToolbox.Spec: identitySites
			// requires identity.
			name: "identitySites without identity: errors",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
  identitySites: true
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.identitySites")
				require.True(t, ok, "expected a diagnostic at path access.identitySites, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "identity")
			},
		},
		{
			name: "allowlist + identity + identitySites: clean",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
  identity: joey-airlines
  identitySites: true
interaction: act
state: ephemeral
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			// Mirrors the third CEL rule on BrowserToolbox.Spec (C8): an
			// unattended site requires allowlist mode — a blocklist (general
			// access) browser cannot stand up a session-start grant with no
			// per-site allowlist at all.
			name: "unattended site under blocklist: errors",
			body: `
access:
  mode: blocklist
  sites:
    - host: aa.com
      unattended: true
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.sites[0].unattended")
				require.True(t, ok, "expected a diagnostic at path access.sites[0].unattended, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "allowlist")
			},
		},
		{
			// Mirrors the third CEL rule on BrowserToolbox.Spec (C8): an
			// unattended site requires access.identity -- the unattended grant is
			// written for a bound identity's browsing session, and there is no
			// standing approver to ask if the identity binding is missing.
			name: "unattended site under allowlist without identity: errors",
			body: `
access:
  mode: allowlist
  sites:
    - host: aa.com
      unattended: true
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "access.sites[0].unattended")
				require.True(t, ok, "expected a diagnostic at path access.sites[0].unattended, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "identity")
			},
		},
		{
			name: "unattended site under allowlist with identity: clean",
			body: `
access:
  mode: allowlist
  sites:
    - host: aa.com
      unattended: true
  identity: joey-airlines
interaction: act
state: ephemeral
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			// C9 behavior (c): mirrors the field-scoped CEL rule on
			// BrowserToolbox.Spec — an EXPLICIT state=persistent requires identity.
			name: "explicit persistent without identity: errors at state",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
state: persistent
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "state")
				require.True(t, ok, "expected a diagnostic at path state, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "identity")
			},
		},
		{
			// C9: explicit persistent WITH an identity is legitimate.
			name: "explicit persistent with identity: clean",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
  identity: joey-airlines
state: persistent
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			// C9 behavior (d): an explicit ephemeral is honored even with an
			// identity bound — no persistence rule applies to it.
			name: "explicit ephemeral with identity: clean",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
  identity: joey-airlines
state: ephemeral
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			// C9 behaviors (a)/(b): an UNSET state is left to the AgentSession
			// snapshot to default; authoring-time validation accepts it in both the
			// identity-bound and identity-less shapes.
			name: "unset state with identity: clean (snapshot defaults it)",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
  identity: joey-airlines
interaction: act
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				assert.Empty(t, diags)
			},
		},
		{
			name: "unset state without identity: clean (snapshot defaults it)",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				// An allowlist with sites and no identity has no errors; the only
				// finding would be the zero-sites warning, which does not apply here.
				_, hasStateDiag := findDiag(diags, "state")
				assert.False(t, hasStateDiag, "an unset state must not be flagged, got %+v", diags)
			},
		},
		{
			name: "invalid state value: errors at state",
			body: `
access:
  mode: allowlist
  sites:
    - host: example.com
state: sticky
`,
			check: func(t *testing.T, diags []contract.Diagnostic) {
				d, ok := findDiag(diags, "state")
				require.True(t, ok, "expected a diagnostic at path state, got %+v", diags)
				assert.Equal(t, "error", d.Severity)
				assert.Contains(t, d.Message, "sticky")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTemp(t, tc.body)
			diags, err := New().ValidateFile(path)
			require.NoError(t, err)
			tc.check(t, diags)
		})
	}
}

func TestKind_LintFile_DelegatesToValidateFile(t *testing.T) {
	path := writeTemp(t, `
access:
  mode: sideways
`)
	validateDiags, err := New().ValidateFile(path)
	require.NoError(t, err)
	lintDiags, err := New().LintFile(path)
	require.NoError(t, err)
	assert.Equal(t, validateDiags, lintDiags)
}

func TestKind_Row(t *testing.T) {
	row := New().Row(sampleCR())
	assert.Equal(t, "BrowserToolbox", row.Kind)
	assert.Equal(t, "btb1", row.Name)
	assert.Equal(t, "ns1", row.Namespace)
	assert.Equal(t, "Mode=allowlist Interaction=readonly", row.Status)
	assert.Contains(t, row.Summary, "2")
}

func TestKind_Detail_ShowsIdentityWhenSet(t *testing.T) {
	cr := sampleCR()
	cr.Spec.Access.Mode = "allowlist"
	cr.Spec.Access.Identity = "joey-airlines"
	cr.Spec.Access.IdentitySites = true

	d := New().Detail(cr)
	var access string
	for _, s := range d.Sections {
		if s.Title == "Access" {
			access = s.Body
		}
	}
	assert.Contains(t, access, "joey-airlines")
	assert.Contains(t, access, "identitySites")
}

func TestKind_Detail_OmitsIdentityWhenUnset(t *testing.T) {
	d := New().Detail(sampleCR())
	var access string
	for _, s := range d.Sections {
		if s.Title == "Access" {
			access = s.Body
		}
	}
	assert.NotContains(t, access, "identity")
}

func TestKind_Detail_HasAllSections(t *testing.T) {
	d := New().Detail(sampleCR())
	assert.Equal(t, "BrowserToolbox", d.Kind)
	assert.Equal(t, "btb1", d.Name)
	titles := make([]string, 0, len(d.Sections))
	body := ""
	for _, s := range d.Sections {
		titles = append(titles, s.Title)
		body += s.Title + "\n" + s.Body + "\n"
	}
	assert.Contains(t, titles, "Access")
	assert.Contains(t, titles, "Interaction")
	assert.Contains(t, titles, "State")
	for _, want := range []string{
		"allowlist",
		"example.com",
		"exact.example.com",
		"(exact)",
		"readonly",
		"ephemeral",
	} {
		assert.Contains(t, body, want, "detail missing %q", want)
	}
}
