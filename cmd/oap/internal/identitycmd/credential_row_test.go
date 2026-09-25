package identitycmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	// Registers the static/oauth/federated credkind.Kinds so
	// credkindregistry.Get(cred.Type) resolves in this package's tests.
	_ "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/imports"
)

func TestCredentialRow_UsesTheKindDisplayName(t *testing.T) {
	row := credentialRow(spiceboxv1alpha1.AgentCredential{
		Name: "gh", Type: "static",
		Static: &spiceboxv1alpha1.StaticCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretKeyRef{Name: "demo-agent-gh", Key: "token"}},
	})
	assert.Equal(t, "Static token", row.TypeLabel)
	assert.Equal(t, "demo-agent-gh", row.SecretName)
	assert.Equal(t, "token", row.SecretKey)
}

func TestCredentialRow_UnknownTypeDegradesVisibly(t *testing.T) {
	row := credentialRow(spiceboxv1alpha1.AgentCredential{Name: "x", Type: "nosuch"})
	assert.Contains(t, row.TypeLabel, "unknown",
		"an unrecognized type must read as unknown, never as blank")
	assert.Empty(t, row.SecretName, "an unrecognized type has no known Secret to name")
}

// TestCredentialRow_OAuthReportsSecretWithNoKey pins the fact that oauth's
// Secret has a fixed multi-key shape: SecretRef.Key stays empty, unlike
// static's single named key. This is what used to be indistinguishable from
// an unrecognized type — both fell into the old switch's default arm and
// printed "(type=oauth)" with no Secret name at all. The registry-driven row
// now names the Secret for oauth too, which the old code never did.
func TestCredentialRow_OAuthReportsSecretWithNoKey(t *testing.T) {
	row := credentialRow(spiceboxv1alpha1.AgentCredential{
		Name: "linear", Type: "oauth",
		OAuth: &spiceboxv1alpha1.OAuthCredentialSource{
			SecretRef: spiceboxv1alpha1.SecretRef{Name: "linear-oauth-secret"}},
	})
	assert.Equal(t, "OAuth credential", row.TypeLabel)
	assert.Equal(t, "linear-oauth-secret", row.SecretName,
		"the old switch's default arm never named oauth's Secret at all")
	assert.Empty(t, row.SecretKey, "oauth's Secret has a fixed multi-key shape, not one named key")
}

// TestCredentialRow_FederatedHasNoSecretRef exercises a minted type: nothing
// is stored, so SecretRef is nil and the row carries no Secret at all — same
// shape the old code produced for federated (it fell into the default arm,
// which also printed no Secret), but now DisplayName replaces the bare
// "type=federated" text.
func TestCredentialRow_FederatedHasNoSecretRef(t *testing.T) {
	row := credentialRow(spiceboxv1alpha1.AgentCredential{
		Name: "idjag", Type: "federated",
		Federated: &spiceboxv1alpha1.FederatedCredentialSource{
			Resource: "https://mcp.example.com", ResourceServerURL: "https://mcp.example.com",
			IdPSecretRef: spiceboxv1alpha1.SecretRef{Name: "user-idp-secret"},
		},
	})
	assert.Equal(t, "Federated (ID-JAG)", row.TypeLabel)
	assert.Empty(t, row.SecretName, "a minted type has no backing Secret to name")
	assert.Empty(t, row.SecretKey)
}

// TestCredentialRow_MalformedStaticDoesNotPanic — a static-typed credential
// with a nil Static block is malformed (ValidateSpec would refuse it), but
// credentialRow must not dereference a nil pointer rendering it; the row
// degrades to a label with no Secret rather than crashing `oap identity show`.
func TestCredentialRow_MalformedStaticDoesNotPanic(t *testing.T) {
	row := credentialRow(spiceboxv1alpha1.AgentCredential{Name: "broken", Type: "static"})
	assert.Equal(t, "Static token", row.TypeLabel)
	assert.Empty(t, row.SecretName)
	assert.Empty(t, row.SecretKey)
}

func TestPrintCredentialRow(t *testing.T) {
	cases := []struct {
		name string
		row  credRow
		want string
	}{
		{
			name: "single-key Secret: name and key both print",
			row:  credRow{Name: "gh", TypeLabel: "Static token", SecretName: "s", SecretKey: "k"},
			want: "  - gh (Static token; secret=s key=k)\n",
		},
		{
			name: "fixed multi-key Secret: name prints, no key",
			row:  credRow{Name: "linear", TypeLabel: "OAuth credential", SecretName: "s"},
			want: "  - linear (OAuth credential; secret=s)\n",
		},
		{
			name: "no backing Secret: neither name nor key print",
			row:  credRow{Name: "idjag", TypeLabel: "Federated (ID-JAG)"},
			want: "  - idjag (Federated (ID-JAG))\n",
		},
		{
			name: "unknown type: label alone still renders, never a blank line",
			row:  credRow{Name: "x", TypeLabel: "unknown (nosuch)"},
			want: "  - x (unknown (nosuch))\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			printCredentialRow(&buf, tc.row)
			assert.Equal(t, tc.want, buf.String())
		})
	}
}
