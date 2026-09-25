package cli

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

func TestParseRefClassifiesAssertion(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		strength pinning.Strength
		wantErr  bool
	}{
		{name: "@sha256:… → frozen", spec: "gh@sha256:abc123", strength: pinning.StrengthFrozen},
		{name: "@version-range → named", spec: "gh@>=1.2.0", strength: pinning.StrengthNamed},
		{name: "@v1.2.0 → named", spec: "gh@v1.2.0", strength: pinning.StrengthNamed},
		{name: "bare name → unpinned", spec: "gh", strength: pinning.StrengthUnpinned},
		{name: "bare toolkit name no @ → unpinned", spec: "my-toolkit", strength: pinning.StrengthUnpinned},
		{name: "empty → error", spec: "", wantErr: true},
		{name: "empty suffix after @ → error", spec: "gh@", wantErr: true},
		{name: "empty name → error", spec: "@sha256:abc", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "cli", ref.Kind)
			assert.Equal(t, tc.spec, ref.Spec)
			assert.Equal(t, tc.strength, ref.Strength)
		})
	}
}

func TestResolveAndVerifyAreSyntactic(t *testing.T) {
	k := &Kind{}

	t.Run("frozen ref: digest is the sha256 hash", func(t *testing.T) {
		ref, err := k.ParseRef("gh@sha256:abc123")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Equal(t, "sha256:abc123", f.Digest)
		assert.Empty(t, f.Version)
	})

	t.Run("named ref: version is the suffix, no digest", func(t *testing.T) {
		ref, err := k.ParseRef("gh@>=1.2.0")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Equal(t, ">=1.2.0", f.Version)
	})

	t.Run("unpinned ref: empty frozen", func(t *testing.T) {
		ref, err := k.ParseRef("gh")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Empty(t, f.Version)
	})
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
			spec:     "gh@sha256:abc123",
			baseline: pinning.Frozen{Digest: "sha256:abc123"},
			drifted:  false,
		},
		{
			name:     "frozen rewritten to different hash: drift",
			spec:     "gh@sha256:abc123",
			baseline: pinning.Frozen{Digest: "sha256:OLD"},
			drifted:  true,
		},
		{
			name:     "named unchanged: no drift",
			spec:     "gh@>=1.2.0",
			baseline: pinning.Frozen{Version: ">=1.2.0"},
			drifted:  false,
		},
		{
			name:     "named with fetch-enriched baseline digest: no drift (enrichment not drift)",
			spec:     "gh@>=1.2.0",
			baseline: pinning.Frozen{Digest: "sha256:abc123", Version: ">=1.2.0"},
			drifted:  false,
		},
		{
			name:     "named range changed: drift",
			spec:     "gh@>=2.0.0",
			baseline: pinning.Frozen{Version: ">=1.2.0"},
			drifted:  true,
		},
		{
			name:     "unpinned with fetch-enriched baseline: no drift",
			spec:     "gh",
			baseline: pinning.Frozen{Digest: "sha256:abc123"},
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

func TestKindIsRegistered(t *testing.T) {
	_, ok := registry.Get("cli")
	assert.True(t, ok, "init() must register the cli kind")
}

func TestStrengthFor(t *testing.T) {
	cases := []struct {
		name             string
		pinnedBinaryHash string
		versionRange     string
		want             pinning.Strength
	}{
		{
			name:             "hash set → frozen",
			pinnedBinaryHash: "sha256:deadbeef",
			want:             pinning.StrengthFrozen,
		},
		{
			name:         "version range only → named",
			versionRange: ">=1.0.0",
			want:         pinning.StrengthNamed,
		},
		{
			name: "neither set → unpinned",
			want: pinning.StrengthUnpinned,
		},
		{
			name:             "both set → frozen wins",
			pinnedBinaryHash: "sha256:abc123",
			versionRange:     ">=1.0.0",
			want:             pinning.StrengthFrozen,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StrengthFor(tc.pinnedBinaryHash, tc.versionRange)
			assert.Equal(t, tc.want, got)
		})
	}
}
