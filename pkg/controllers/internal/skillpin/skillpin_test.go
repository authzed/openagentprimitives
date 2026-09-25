package skillpin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/skills/canonical"
)

func TestDeclared(t *testing.T) {
	then := metav1.Now()
	cases := []struct {
		name          string
		kind          string
		strength      string
		digest        string
		version       string
		prev          *v1.PinRecord
		wantPreserved bool
	}{
		{
			name: "nil prev: stamps new ObservedAt",
			kind: "cli", strength: "named", digest: "", version: ">=1.0.0",
		},
		{
			name: "identity unchanged: preserves ObservedAt",
			kind: "cli", strength: "named", digest: "", version: ">=1.0.0",
			prev:          &v1.PinRecord{Kind: "cli", Strength: "named", Digest: "", Version: ">=1.0.0", ObservedAt: &then},
			wantPreserved: true,
		},
		{
			name: "strength changed: restamps",
			kind: "cli", strength: "frozen", digest: "sha256:abc", version: "",
			prev:          &v1.PinRecord{Kind: "cli", Strength: "named", Digest: "", Version: ">=1.0.0", ObservedAt: &then},
			wantPreserved: false,
		},
		{
			name: "version changed: restamps",
			kind: "cli", strength: "named", digest: "", version: ">=2.0.0",
			prev:          &v1.PinRecord{Kind: "cli", Strength: "named", Digest: "", Version: ">=1.0.0", ObservedAt: &then},
			wantPreserved: false,
		},
		{
			name: "digest changed: restamps",
			kind: "cli", strength: "frozen", digest: "sha256:newdigest",
			prev:          &v1.PinRecord{Kind: "cli", Strength: "frozen", Digest: "sha256:olddigest", ObservedAt: &then},
			wantPreserved: false,
		},
		{
			name: "image kind frozen: stamps correctly",
			kind: "image", strength: "frozen", digest: "sha256:abc123", version: "v1",
		},
		{
			name: "image kind frozen unchanged: preserves",
			kind: "image", strength: "frozen", digest: "sha256:abc123", version: "v1",
			prev:          &v1.PinRecord{Kind: "image", Strength: "frozen", Digest: "sha256:abc123", Version: "v1", ObservedAt: &then},
			wantPreserved: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Declared(tc.kind, tc.strength, tc.digest, tc.version, tc.prev)
			require.NotNil(t, rec)
			assert.Equal(t, tc.kind, rec.Kind)
			assert.Equal(t, tc.strength, rec.Strength)
			assert.Equal(t, tc.digest, rec.Digest)
			assert.Equal(t, tc.version, rec.Version)
			require.NotNil(t, rec.ObservedAt, "ObservedAt must always be set")
			if tc.wantPreserved {
				require.NotNil(t, tc.prev)
				assert.Equal(t, tc.prev.ObservedAt, rec.ObservedAt, "ObservedAt should be preserved")
			} else if tc.prev != nil {
				assert.NotEqual(t, tc.prev.ObservedAt, rec.ObservedAt, "ObservedAt should be restamped")
			}
		})
	}
}

func mustParse(t *testing.T, s string) canonical.Name {
	t.Helper()
	n, err := canonical.Parse(s)
	require.NoError(t, err)
	return n
}

func TestBaseline(t *testing.T) {
	then := metav1.Now()
	cases := []struct {
		name        string
		spec        string
		prev        *v1.PinRecord
		wantDigest  string
		wantVersion string
		// wantPreserved: ObservedAt (and Details) carried from prev
		wantPreserved bool
	}{
		{name: "fresh frozen: digest+version are the sha, new ObservedAt",
			spec: "github.com/org/repo//skills/x@deadbee", wantDigest: "deadbee", wantVersion: "deadbee"},
		{name: "frozen unchanged: preserved",
			spec:       "github.com/org/repo//skills/x@deadbee",
			prev:       &v1.PinRecord{Kind: "skill", Strength: "frozen", Digest: "deadbee", Version: "deadbee", ObservedAt: &then},
			wantDigest: "deadbee", wantVersion: "deadbee", wantPreserved: true},
		{name: "frozen sha rewritten: restamped",
			spec:       "github.com/org/repo//skills/x@0123456",
			prev:       &v1.PinRecord{Kind: "skill", Strength: "frozen", Digest: "deadbee", Version: "deadbee", ObservedAt: &then},
			wantDigest: "0123456", wantVersion: "0123456"},
		{name: "named with fetch-enriched prev digest: enrichment carried, preserved",
			spec:       "github.com/org/repo//skills/x@v1.2.0",
			prev:       &v1.PinRecord{Kind: "skill", Strength: "named", Digest: "deadbee", Version: "v1.2.0", ObservedAt: &then},
			wantDigest: "deadbee", wantVersion: "v1.2.0", wantPreserved: true},
		{name: "named version bump: enrichment dropped, restamped",
			spec:       "github.com/org/repo//skills/x@v2.0.0",
			prev:       &v1.PinRecord{Kind: "skill", Strength: "named", Digest: "deadbee", Version: "v1.2.0", ObservedAt: &then},
			wantDigest: "", wantVersion: "v2.0.0"},
		{name: "unpinned with enriched prev: enrichment carried, preserved",
			spec:       "github.com/org/repo//skills/x",
			prev:       &v1.PinRecord{Kind: "skill", Strength: "unpinned", Digest: "deadbee", Version: "", ObservedAt: &then},
			wantDigest: "deadbee", wantVersion: "", wantPreserved: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := Baseline(mustParse(t, tc.spec), tc.prev)
			require.NotNil(t, rec)
			assert.Equal(t, "skill", rec.Kind)
			assert.Equal(t, tc.wantDigest, rec.Digest)
			assert.Equal(t, tc.wantVersion, rec.Version)
			require.NotNil(t, rec.ObservedAt)
			if tc.wantPreserved {
				assert.Equal(t, tc.prev.ObservedAt, rec.ObservedAt)
			} else if tc.prev != nil {
				assert.NotEqual(t, tc.prev.ObservedAt, rec.ObservedAt)
			}
		})
	}
}
