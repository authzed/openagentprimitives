package image

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSplitRef(t *testing.T) {
	cases := []struct {
		name       string
		ref        string
		wantRepo   string
		wantTag    string
		wantDigest string
	}{
		{
			name:     "tag ref: ghcr.io/org/tool:v1",
			ref:      "ghcr.io/org/tool:v1",
			wantRepo: "ghcr.io/org/tool",
			wantTag:  "v1",
		},
		{
			name:       "digest ref: ghcr.io/org/tool@sha256:abc",
			ref:        "ghcr.io/org/tool@sha256:abc",
			wantRepo:   "ghcr.io/org/tool",
			wantDigest: "sha256:abc",
		},
		{
			name:     "host with port and tag: registry.example.com:5000/org/tool:v1",
			ref:      "registry.example.com:5000/org/tool:v1",
			wantRepo: "registry.example.com:5000/org/tool",
			wantTag:  "v1",
		},
		{
			name:     "host with port, bare: registry.example.com:5000/org/tool",
			ref:      "registry.example.com:5000/org/tool",
			wantRepo: "registry.example.com:5000/org/tool",
		},
		{
			name:     "bare tool (docker-library style): tool",
			ref:      "tool",
			wantRepo: "tool",
		},
		{
			name:     "bare ref without tag: ghcr.io/org/tool",
			ref:      "ghcr.io/org/tool",
			wantRepo: "ghcr.io/org/tool",
		},
		{
			name:       "host with port and digest: registry.example.com:5000/org/tool@sha256:abc",
			ref:        "registry.example.com:5000/org/tool@sha256:abc",
			wantRepo:   "registry.example.com:5000/org/tool",
			wantDigest: "sha256:abc",
		},
		{
			name:     "latest tag: ghcr.io/org/tool:latest",
			ref:      "ghcr.io/org/tool:latest",
			wantRepo: "ghcr.io/org/tool",
			wantTag:  "latest",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, tag, digest := SplitRef(tc.ref)
			assert.Equal(t, tc.wantRepo, repo, "repo")
			assert.Equal(t, tc.wantTag, tag, "tag")
			assert.Equal(t, tc.wantDigest, digest, "digest")
		})
	}
}

func TestWithDigest(t *testing.T) {
	t.Run("drops tag and adds digest", func(t *testing.T) {
		result := WithDigest("ghcr.io/org/tool:v1", "sha256:deadbeef")
		assert.Equal(t, "ghcr.io/org/tool@sha256:deadbeef", result)
	})
	t.Run("bare ref gets digest", func(t *testing.T) {
		result := WithDigest("ghcr.io/org/tool", "sha256:deadbeef")
		assert.Equal(t, "ghcr.io/org/tool@sha256:deadbeef", result)
	})
	t.Run("round-trips through SplitRef", func(t *testing.T) {
		orig := "ghcr.io/org/tool:v1"
		digest := "sha256:abc123"
		pinned := WithDigest(orig, digest)
		repo, tag, d := SplitRef(pinned)
		assert.Equal(t, "ghcr.io/org/tool", repo)
		assert.Equal(t, "", tag)
		assert.Equal(t, digest, d)
	})
	t.Run("host with port: drops tag and adds digest", func(t *testing.T) {
		result := WithDigest("registry.example.com:5000/org/tool:v1", "sha256:deadbeef")
		assert.Equal(t, "registry.example.com:5000/org/tool@sha256:deadbeef", result)
	})
}

func TestDigestFromImageID(t *testing.T) {
	cases := []struct {
		name       string
		imageID    string
		wantDigest string
	}{
		{
			name:       "docker-pullable prefix",
			imageID:    "docker-pullable://ghcr.io/org/tool@sha256:abc123",
			wantDigest: "sha256:abc123",
		},
		{
			name:       "bare repo@digest",
			imageID:    "ghcr.io/org/tool@sha256:abc123",
			wantDigest: "sha256:abc123",
		},
		{
			name:       "bare sha256 digest",
			imageID:    "sha256:abc123",
			wantDigest: "sha256:abc123",
		},
		{
			name:       "no digest present",
			imageID:    "ghcr.io/org/tool:v1",
			wantDigest: "",
		},
		{
			name:       "empty string",
			imageID:    "",
			wantDigest: "",
		},
		{
			name:       "docker-pullable with host port",
			imageID:    "docker-pullable://registry.example.com:5000/org/tool@sha256:deadbeef",
			wantDigest: "sha256:deadbeef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantDigest, DigestFromImageID(tc.imageID))
		})
	}
}
