package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/apimage"
)

// TestTrustedImageRegistryFrom pins the reversal of
// apimage.Image.RegistryRef/DigestRef's composition: fix round 1 for the
// workshop admission webhook derives the SidecarToolbox image-gate's trusted
// registry from this function, applied to cfg.sandboxImage — a genuinely
// wrong answer here is a genuinely wrong trust boundary, not just a cosmetic
// bug.
func TestTrustedImageRegistryFrom(t *testing.T) {
	sandbox := apimage.Image{Name: "spicebox-sandbox"}

	cases := []struct {
		name string
		ref  string
		want string
	}{
		{
			name: "local-dev bare ref (LocalRef): no registry",
			ref:  sandbox.LocalRef(),
			want: "",
		},
		{
			name: "registry-qualified ref (RegistryRef): recovers the registry",
			ref:  sandbox.RegistryRef("ghcr.io/example"),
			want: "ghcr.io/example",
		},
		{
			name: "single-segment registry host: recovers just the host",
			ref:  sandbox.RegistryRef("myregistry.internal:5000"),
			want: "myregistry.internal:5000",
		},
		{
			name: "digest-pinned ref (DigestRef): recovers the registry, ignoring the digest suffix",
			ref:  sandbox.DigestRef("ghcr.io/example", "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
			want: "ghcr.io/example",
		},
		{
			name: "a ref that doesn't even name this image: fails SAFE to empty (local-dev-only)",
			ref:  "totally-unrelated-image:v1",
			want: "",
		},
		{
			name: "empty ref: fails safe to empty",
			ref:  "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, trustedImageRegistryFrom(sandbox, tc.ref))
		})
	}
}
