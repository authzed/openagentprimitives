package image

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

func TestParseRefClassifiesStrength(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		strength pinning.Strength
		wantErr  bool
	}{
		{name: "digest ref → frozen", spec: "ghcr.io/org/tool@sha256:abc123", strength: pinning.StrengthFrozen},
		{name: "tag ref → named", spec: "ghcr.io/org/tool:v1", strength: pinning.StrengthNamed},
		{name: "latest tag → unpinned", spec: "ghcr.io/org/tool:latest", strength: pinning.StrengthUnpinned},
		{name: "bare ref → unpinned", spec: "ghcr.io/org/tool", strength: pinning.StrengthUnpinned},
		{name: "docker-library bare → unpinned", spec: "tool", strength: pinning.StrengthUnpinned},
		{name: "host with port and tag → named", spec: "registry.example.com:5000/org/tool:v1", strength: pinning.StrengthNamed},
		{name: "host with port bare → unpinned", spec: "registry.example.com:5000/org/tool", strength: pinning.StrengthUnpinned},
		{name: "host with port and digest → frozen", spec: "registry.example.com:5000/org/tool@sha256:abc", strength: pinning.StrengthFrozen},
		{name: "empty → error", spec: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "image", ref.Kind)
			assert.Equal(t, tc.spec, ref.Spec)
			assert.Equal(t, tc.strength, ref.Strength)
		})
	}
}

func TestResolveReportsSyntacticIdentity(t *testing.T) {
	k := &Kind{}
	t.Run("frozen ref: digest set, version is tag portion of spec", func(t *testing.T) {
		ref, err := k.ParseRef("ghcr.io/org/tool@sha256:abc123")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", f.Digest)
		// For digest-only refs there's no tag
		assert.Empty(t, f.Version)
	})
	t.Run("named ref: version is tag, no digest", func(t *testing.T) {
		ref, err := k.ParseRef("ghcr.io/org/tool:v1")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Equal(t, "v1", f.Version)
	})
	t.Run("unpinned bare ref: empty frozen", func(t *testing.T) {
		ref, err := k.ParseRef("ghcr.io/org/tool")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Empty(t, f.Version)
	})
	t.Run("latest tag: empty frozen (unpinned)", func(t *testing.T) {
		ref, err := k.ParseRef("ghcr.io/org/tool:latest")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Empty(t, f.Version)
	})
}

func TestKindIsRegistered(t *testing.T) {
	_, ok := registry.Get("image")
	assert.True(t, ok, "init() must register the image kind")
}

func TestVerifyDetectsDriftNotEnrichment(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		baseline pinning.Frozen
		drifted  bool
	}{
		{
			name:     "frozen unchanged: no drift",
			spec:     "ghcr.io/org/tool@sha256:abc123",
			baseline: pinning.Frozen{Digest: "sha256:abc123"},
			drifted:  false,
		},
		{
			name:     "frozen rewritten to different digest: drift",
			spec:     "ghcr.io/org/tool@sha256:abc123",
			baseline: pinning.Frozen{Digest: "sha256:OLD"},
			drifted:  true,
		},
		{
			name:     "named tag unchanged: no drift",
			spec:     "ghcr.io/org/tool:v1",
			baseline: pinning.Frozen{Version: "v1"},
			drifted:  false,
		},
		{
			name:     "named tag with fetch-enriched baseline digest: no drift (enrichment not drift)",
			spec:     "ghcr.io/org/tool:v1",
			baseline: pinning.Frozen{Digest: "sha256:abc123", Version: "v1"},
			drifted:  false,
		},
		{
			name:     "named tag changed: drift",
			spec:     "ghcr.io/org/tool:v2",
			baseline: pinning.Frozen{Digest: "sha256:abc123", Version: "v1"},
			drifted:  true,
		},
		{
			name:     "unpinned with fetch-enriched baseline: no drift",
			spec:     "ghcr.io/org/tool",
			baseline: pinning.Frozen{Digest: "sha256:abc123", Version: ""},
			drifted:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			require.NoError(t, err)
			rep, err := k.Verify(context.Background(), ref, tc.baseline)
			require.NoError(t, err)
			assert.Equal(t, tc.drifted, rep.Drifted)
			if tc.drifted {
				assert.NotEmpty(t, rep.Summary)
			}
		})
	}
}
