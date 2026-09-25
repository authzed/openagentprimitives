package oap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
	"github.com/authzed/openagentprimitives/pkg/authz/pinning/registry"
)

// fakeDigest is a syntactically valid sha256 digest (64 lowercase hex
// chars) — go-containerregistry/oras-go's digest.Parse rejects anything
// shorter, unlike the image kind's hand-rolled SplitRef which doesn't
// validate digest shape at all.
const fakeDigest = "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

func TestName(t *testing.T) {
	k := &Kind{}
	assert.Equal(t, "oap", k.Name())
}

func TestParseRefClassifiesStrength(t *testing.T) {
	k := &Kind{}
	cases := []struct {
		name     string
		spec     string
		strength pinning.Strength
		wantErr  bool
	}{
		{name: "digest ref → frozen", spec: "ghcr.io/acme/demo-agent@" + fakeDigest, strength: pinning.StrengthFrozen},
		{name: "tag ref → named", spec: "ghcr.io/acme/demo-agent:1.2.0", strength: pinning.StrengthNamed},
		{name: "latest tag → unpinned", spec: "ghcr.io/acme/demo-agent:latest", strength: pinning.StrengthUnpinned},
		{name: "bare ref (implicit latest) → unpinned", spec: "ghcr.io/acme/demo-agent", strength: pinning.StrengthUnpinned},
		{name: "host with port and tag → named", spec: "registry.example.com:5000/acme/demo-agent:1.2.0", strength: pinning.StrengthNamed},
		{name: "host with port and digest → frozen", spec: "registry.example.com:5000/acme/demo-agent@" + fakeDigest, strength: pinning.StrengthFrozen},
		{name: "host with port bare → unpinned", spec: "registry.example.com:5000/acme/demo-agent", strength: pinning.StrengthUnpinned},
		{name: "empty → error", spec: "", wantErr: true},
		{name: "malformed (no registry/repo separator) → error", spec: "demo-agent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := k.ParseRef(tc.spec)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "oap", ref.Kind)
			assert.Equal(t, tc.spec, ref.Spec)
			assert.Equal(t, tc.strength, ref.Strength)
		})
	}
}

func TestResolve_MalformedRefErrorsBeforeAnyNetworkIO(t *testing.T) {
	k := &Kind{}
	ref := pinning.Ref{Kind: KindName, Spec: "demo-agent"}
	_, err := k.Resolve(context.Background(), ref)
	assert.Error(t, err, "Resolve on a malformed ref must fail at parse time, never attempt to dial a registry")
}

func TestKindIsRegistered(t *testing.T) {
	_, ok := registry.Get("oap")
	assert.True(t, ok, "init() must register the oap kind")
}

func TestFrozenFromDigest(t *testing.T) {
	cases := []struct {
		name        string
		ref         string
		wantVersion string
		wantErr     bool
	}{
		{name: "tag ref: digest recorded, tag recorded as version", ref: "ghcr.io/acme/demo-agent:1.2.0", wantVersion: "1.2.0"},
		{name: "digest ref: digest recorded, no version", ref: "ghcr.io/acme/demo-agent@" + fakeDigest, wantVersion: ""},
		{name: "bare ref: digest recorded, no version", ref: "ghcr.io/acme/demo-agent", wantVersion: ""},
		{name: "malformed ref (no registry/repo separator) → error", ref: "demo-agent", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := FrozenFromDigest(tc.ref, fakeDigest)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, fakeDigest, f.Digest, "the given digest must be recorded verbatim, without re-resolving")
			assert.Equal(t, tc.wantVersion, f.Version)
		})
	}
}
