package spiceboxtoolkit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func TestCliPin(t *testing.T) {
	then := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	cases := []struct {
		name          string
		target        spiceboxv1alpha1.ToolkitTarget
		prev          *spiceboxv1alpha1.PinRecord
		wantStrength  string
		wantDigest    string
		wantVersion   string
		wantPreserved bool
	}{
		{
			name:         "VersionRange only → named pin",
			target:       spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool", VersionRange: ">=1.0.0 <2.0.0"},
			wantStrength: "named",
			wantDigest:   "",
			wantVersion:  ">=1.0.0 <2.0.0",
		},
		{
			name:         "PinnedBinaryHash set → frozen pin",
			target:       spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool", PinnedBinaryHash: "sha256:deadbeef"},
			wantStrength: "frozen",
			wantDigest:   "sha256:deadbeef",
			wantVersion:  "",
		},
		{
			name:         "neither set → unpinned",
			target:       spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool"},
			wantStrength: "unpinned",
			wantDigest:   "",
			wantVersion:  "",
		},
		{
			name: "both PinnedBinaryHash and VersionRange set → frozen wins",
			target: spiceboxv1alpha1.ToolkitTarget{
				Binary:           "/usr/bin/tool",
				PinnedBinaryHash: "sha256:deadbeef",
				VersionRange:     ">=1.0.0",
			},
			wantStrength: "frozen",
			wantDigest:   "sha256:deadbeef",
			wantVersion:  ">=1.0.0",
		},
		{
			name:   "re-reconcile with unchanged VersionRange → ObservedAt preserved",
			target: spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool", VersionRange: ">=1.0.0"},
			prev: &spiceboxv1alpha1.PinRecord{
				Kind:       "cli",
				Strength:   "named",
				Version:    ">=1.0.0",
				ObservedAt: &then,
			},
			wantStrength:  "named",
			wantVersion:   ">=1.0.0",
			wantPreserved: true,
		},
		{
			name:   "re-reconcile with unchanged PinnedBinaryHash → ObservedAt preserved",
			target: spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool", PinnedBinaryHash: "sha256:abc123"},
			prev: &spiceboxv1alpha1.PinRecord{
				Kind:       "cli",
				Strength:   "frozen",
				Digest:     "sha256:abc123",
				ObservedAt: &then,
			},
			wantStrength:  "frozen",
			wantDigest:    "sha256:abc123",
			wantPreserved: true,
		},
		{
			name:   "VersionRange changed → ObservedAt restamped",
			target: spiceboxv1alpha1.ToolkitTarget{Binary: "/usr/bin/tool", VersionRange: ">=2.0.0"},
			prev: &spiceboxv1alpha1.PinRecord{
				Kind:       "cli",
				Strength:   "named",
				Version:    ">=1.0.0",
				ObservedAt: &then,
			},
			wantStrength:  "named",
			wantVersion:   ">=2.0.0",
			wantPreserved: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := cliPin(tc.target, tc.prev)
			require.NotNil(t, rec)
			assert.Equal(t, "cli", rec.Kind)
			assert.Equal(t, tc.wantStrength, rec.Strength)
			assert.Equal(t, tc.wantDigest, rec.Digest)
			assert.Equal(t, tc.wantVersion, rec.Version)
			require.NotNil(t, rec.ObservedAt, "ObservedAt must always be set")
			if tc.wantPreserved {
				require.NotNil(t, tc.prev)
				assert.Equal(t, tc.prev.ObservedAt, rec.ObservedAt, "ObservedAt should be preserved for unchanged identity")
			} else if tc.prev != nil {
				assert.NotEqual(t, tc.prev.ObservedAt, rec.ObservedAt, "ObservedAt should be restamped when identity changes")
			}
		})
	}
}
