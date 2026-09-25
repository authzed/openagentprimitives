package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/authzdecision"
)

func TestApplyProvenance(t *testing.T) {
	cases := []struct {
		name  string
		rec   ProvenanceRecord
		ok    bool
		check func(t *testing.T, d authzdecision.Decision)
	}{
		{
			name: "missing entry: decision unchanged",
			ok:   false,
			check: func(t *testing.T, d authzdecision.Decision) {
				assert.Empty(t, d.PinKind)
				assert.Empty(t, d.PinName)
				assert.Empty(t, d.PinStrength)
				assert.Empty(t, d.PinDigest)
				assert.Empty(t, d.PinVersion)
				assert.False(t, d.PinDrifted)
				assert.Empty(t, d.PinBypassReason)
			},
		},
		{
			name: "mcp frozen no drift: fields populated, PinDrifted false",
			ok:   true,
			rec: ProvenanceRecord{
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     "mcp",
					Strength: "frozen",
					Digest:   "sha256:deadbeef",
					Version:  "v1.2.3",
				},
				Name:         "my-mcp-server",
				DriftSummary: "",
				BypassReason: "",
			},
			check: func(t *testing.T, d authzdecision.Decision) {
				assert.Equal(t, "mcp", d.PinKind)
				assert.Equal(t, "my-mcp-server", d.PinName)
				assert.Equal(t, "frozen", d.PinStrength)
				assert.Equal(t, "sha256:deadbeef", d.PinDigest)
				assert.Equal(t, "v1.2.3", d.PinVersion)
				assert.False(t, d.PinDrifted)
				assert.Empty(t, d.PinBypassReason)
			},
		},
		{
			name: "mcp drifted: PinDrifted true",
			ok:   true,
			rec: ProvenanceRecord{
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     "mcp",
					Strength: "named",
					Digest:   "sha256:newlive",
				},
				Name:         "drifted-server",
				DriftSummary: "MCPServer/drifted-server drifted (sha256:old -> sha256:newlive)",
				BypassReason: "",
			},
			check: func(t *testing.T, d authzdecision.Decision) {
				assert.Equal(t, "mcp", d.PinKind)
				assert.Equal(t, "drifted-server", d.PinName)
				assert.True(t, d.PinDrifted, "PinDrifted must be true when DriftSummary non-empty")
				assert.Empty(t, d.PinBypassReason)
			},
		},
		{
			name: "bypass reason propagated",
			ok:   true,
			rec: ProvenanceRecord{
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     "mcp",
					Strength: "unpinned",
				},
				Name:         "exempted-server",
				DriftSummary: "",
				BypassReason: "dev toolchain exempted from pinning requirement",
			},
			check: func(t *testing.T, d authzdecision.Decision) {
				assert.False(t, d.PinDrifted)
				assert.Equal(t, "dev toolchain exempted from pinning requirement", d.PinBypassReason)
			},
		},
		{
			name: "image pin: kind=image populated",
			ok:   true,
			rec: ProvenanceRecord{
				Pin: spiceboxv1alpha1.PinRecord{
					Kind:     "image",
					Strength: "frozen",
					Digest:   "sha256:imgdigest",
					Version:  "latest",
				},
				Name: "my-sidecar",
			},
			check: func(t *testing.T, d authzdecision.Decision) {
				assert.Equal(t, "image", d.PinKind)
				assert.Equal(t, "my-sidecar", d.PinName)
				assert.Equal(t, "frozen", d.PinStrength)
				assert.Equal(t, "sha256:imgdigest", d.PinDigest)
				assert.Equal(t, "latest", d.PinVersion)
				assert.False(t, d.PinDrifted)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := authzdecision.Decision{
				Outcome: "allowed",
				Subject: "alice@example.com",
			}
			applyProvenance(&d, tc.rec, tc.ok)
			// Core fields must not be touched.
			assert.Equal(t, "allowed", d.Outcome)
			assert.Equal(t, "alice@example.com", d.Subject)
			tc.check(t, d)
		})
	}
}
