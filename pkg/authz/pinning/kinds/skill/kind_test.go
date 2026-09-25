package skill

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
		{name: "full sha → frozen", spec: "github.com/org/repo//skills/x@0123456789abcdef0123456789abcdef01234567", strength: pinning.StrengthFrozen},
		{name: "short sha → frozen", spec: "github.com/org/repo//skills/x@deadbee", strength: pinning.StrengthFrozen},
		{name: "tag → named", spec: "github.com/org/repo//skills/x@v1.2.0", strength: pinning.StrengthNamed},
		{name: "no ref → unpinned", spec: "github.com/org/repo//skills/x", strength: pinning.StrengthUnpinned},
		{name: "local skill, no ref → unpinned", spec: "local//my-skill", strength: pinning.StrengthUnpinned},
		{name: "malformed (no //) → error", spec: "github.com/org/repo/skills/x", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "skill", ref.Kind)
			assert.Equal(t, tc.spec, ref.Spec)
			assert.Equal(t, tc.strength, ref.Strength)
		})
	}
}

func TestResolveReportsSyntacticIdentity(t *testing.T) {
	k := &Kind{}
	t.Run("frozen ref: digest and version are the sha", func(t *testing.T) {
		ref, err := k.ParseRef("github.com/org/repo//skills/x@deadbee")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Equal(t, "deadbee", f.Digest)
		assert.Equal(t, "deadbee", f.Version)
	})
	t.Run("named ref: version only, no digest", func(t *testing.T) {
		ref, err := k.ParseRef("github.com/org/repo//skills/x@v1.2.0")
		require.NoError(t, err)
		f, err := k.Resolve(context.Background(), ref)
		require.NoError(t, err)
		assert.Empty(t, f.Digest)
		assert.Equal(t, "v1.2.0", f.Version)
	})
}

func TestKindIsRegistered(t *testing.T) {
	_, ok := registry.Get("skill")
	assert.True(t, ok, "init() must register the skill kind")
}

func TestVerifyDetectsRefRewritesNotFetchEnrichment(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		baseline pinning.Frozen
		drifted  bool
	}{
		{name: "frozen unchanged: no drift", spec: "github.com/org/repo//skills/x@deadbee",
			baseline: pinning.Frozen{Digest: "deadbee", Version: "deadbee"}, drifted: false},
		{name: "frozen rewritten to a different sha: drift", spec: "github.com/org/repo//skills/x@deadbee",
			baseline: pinning.Frozen{Digest: "0123456", Version: "0123456"}, drifted: true},
		{name: "named unchanged with fetch-enriched baseline digest: no drift", spec: "github.com/org/repo//skills/x@v1.2.0",
			baseline: pinning.Frozen{Digest: "deadbee", Version: "v1.2.0"}, drifted: false},
		{name: "named version bump: drift", spec: "github.com/org/repo//skills/x@v2.0.0",
			baseline: pinning.Frozen{Digest: "deadbee", Version: "v1.2.0"}, drifted: true},
		{name: "unpinned with fetch-enriched baseline: no drift", spec: "github.com/org/repo//skills/x",
			baseline: pinning.Frozen{Digest: "deadbee", Version: ""}, drifted: false},
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
