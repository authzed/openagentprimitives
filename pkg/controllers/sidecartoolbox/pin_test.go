// Package sidecartoolbox — pin_test.go: unit tests for the pure pin helpers.
// These tests cover digestFromProbePod, computeImagePin, and the controller's
// pinCheck phase wiring (via reconcile with SkipProbe injection seam). No live
// API server or probe pod is needed.
package sidecartoolbox

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// ---------------------------------------------------------------------------
// digestFromProbePod
// ---------------------------------------------------------------------------

func TestDigestFromProbePod_HappyPath(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "probe", ImageID: "ghcr.io/example/echo-mcp@sha256:abcdef1234567890"},
			},
		},
	}
	got := digestFromProbePod(pod)
	assert.Equal(t, "sha256:abcdef1234567890", got)
}

func TestDigestFromProbePod_DockerPullablePrefix(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "probe", ImageID: "docker-pullable://ghcr.io/example/echo-mcp@sha256:deadbeef"},
			},
		},
	}
	got := digestFromProbePod(pod)
	assert.Equal(t, "sha256:deadbeef", got)
}

func TestDigestFromProbePod_NoMatchingContainer(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", ImageID: "ghcr.io/example/echo-mcp@sha256:abcdef"},
			},
		},
	}
	got := digestFromProbePod(pod)
	assert.Equal(t, "", got)
}

func TestDigestFromProbePod_NoDigestInImageID(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "probe", ImageID: "ghcr.io/example/echo-mcp:v1"},
			},
		},
	}
	got := digestFromProbePod(pod)
	assert.Equal(t, "", got)
}

// ---------------------------------------------------------------------------
// computeImagePin
// ---------------------------------------------------------------------------

const (
	digest1 = "sha256:aaaa0000111122223333444455556666aaaabbbbccccdddd0000111122223333"
	digest2 = "sha256:bbbb0000111122223333444455556666aaaabbbbccccdddd0000111122223333"
)

func fixedTime(sec int64) *metav1.Time {
	t := metav1.NewTime(time.Unix(sec, 0))
	return &t
}

func TestComputeImagePin_TOFUFirstObservation(t *testing.T) {
	// Named ref, no prior baseline, new digest resolved → TOFU record written.
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", digest1, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, "image", rec.Kind)
	assert.Equal(t, "named", rec.Strength)
	assert.Equal(t, digest1, rec.Digest)
	assert.Equal(t, "v1", rec.Version)
	assert.NotNil(t, rec.ObservedAt)
	assert.Equal(t, "ghcr.io/example/echo-mcp:v1", rec.Details["declaredRef"])
}

func TestComputeImagePin_UnchangedPreservesObservedAt(t *testing.T) {
	// Same digest as baseline: ObservedAt must be preserved, not refreshed.
	original := fixedTime(1000)
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     digest1,
		Version:    "v1",
		ObservedAt: original,
	}
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", digest1, prev)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, digest1, rec.Digest)
	// ObservedAt must be the same pointer value (preserved).
	assert.Equal(t, original, rec.ObservedAt, "ObservedAt must be preserved when digest is unchanged")
}

func TestComputeImagePin_DigestChange_Drifted(t *testing.T) {
	// Prior baseline digest1, now resolves to digest2 → drift detected.
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:     "image",
		Strength: "named",
		Digest:   digest1,
		Version:  "v1",
	}
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", digest2, prev)
	require.NoError(t, err)
	assert.Nil(t, rec, "no record update on drift")
	assert.True(t, drifted)
	assert.Contains(t, summary, digest1, "summary must mention old digest")
	assert.Contains(t, summary, digest2, "summary must mention new digest")
	assert.Contains(t, summary, "pin the source image by digest", "summary must contain accept-hint")
}

func TestComputeImagePin_FrozenDeclaredMatch(t *testing.T) {
	// Declared ref is by-digest, resolved matches → strength frozen, no drift.
	frozenRef := "ghcr.io/example/echo-mcp@" + digest1
	rec, drifted, summary, err := computeImagePin(frozenRef, digest1, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, "frozen", rec.Strength)
	assert.Equal(t, digest1, rec.Digest)
	assert.Empty(t, rec.Version, "frozen refs have no tag component")
}

func TestComputeImagePin_FrozenDeclaredMismatch_Drifted(t *testing.T) {
	// Declared ref is by-digest but probe resolves a different digest → drift.
	frozenRef := "ghcr.io/example/echo-mcp@" + digest1
	rec, drifted, summary, err := computeImagePin(frozenRef, digest2, nil)
	require.NoError(t, err)
	assert.Nil(t, rec)
	assert.True(t, drifted)
	assert.Contains(t, summary, digest1)
	assert.Contains(t, summary, digest2)
	assert.Contains(t, summary, "pin the source image by digest", "accept-hint required")
}

func TestComputeImagePin_ParseRefFailure(t *testing.T) {
	// Empty ref → ParseRef errors → PinVerifyFailed surface path.
	rec, drifted, summary, err := computeImagePin("", digest1, nil)
	assert.Error(t, err, "empty ref must return error")
	assert.Nil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
}

func TestComputeImagePin_LatestTagIsUnpinned(t *testing.T) {
	// :latest → classified unpinned; TOFU still records digest.
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:latest", digest1, nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, "unpinned", rec.Strength)
	assert.Equal(t, digest1, rec.Digest)
	assert.Equal(t, "latest", rec.Version)
}

func TestComputeImagePin_NoResolvedDigest_SkipsRecord(t *testing.T) {
	// Named ref, no resolved digest (probe didn't populate it) → nil record.
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", "", nil)
	require.NoError(t, err)
	assert.Nil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
}

func TestComputeImagePin_FrozenNoResolvedDigest_RecordsDeclared(t *testing.T) {
	// Frozen ref, no resolved digest → records the declared digest (syntactic).
	frozenRef := "ghcr.io/example/echo-mcp@" + digest1
	rec, drifted, summary, err := computeImagePin(frozenRef, "", nil)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, "frozen", rec.Strength)
	assert.Equal(t, digest1, rec.Digest)
}

func TestComputeImagePin_TagBump_RestampsBaseline(t *testing.T) {
	// When the operator changes the declared ref (:v1 → :v2), the new tag's
	// digest must restamp the baseline — not be reported as drift. ObservedAt
	// must also be advanced (not preserved from the v1 baseline).
	originalTime := fixedTime(1000)
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:       "image",
		Strength:   "named",
		Digest:     digest1,
		Version:    "v1",
		ObservedAt: originalTime,
		Details:    map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	// New declared ref (:v2) resolves to digest2.
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v2", digest2, prev)
	require.NoError(t, err)
	require.NotNil(t, rec, "tag bump must produce a new baseline record")
	assert.False(t, drifted, "tag bump is not drift")
	assert.Empty(t, summary)
	assert.Equal(t, digest2, rec.Digest, "restamped baseline must carry the new digest")
	assert.Equal(t, "v2", rec.Version, "restamped baseline must carry the new tag")
	assert.Equal(t, "ghcr.io/example/echo-mcp:v2", rec.Details["declaredRef"])
	// ObservedAt must be advanced (a new stamp), not the stale originalTime.
	require.NotNil(t, rec.ObservedAt)
	assert.NotEqual(t, originalTime, rec.ObservedAt, "ObservedAt must be advanced on tag bump")
}

func TestComputeImagePin_SameDeclaredRef_DigestMoved_IsDrift(t *testing.T) {
	// Same declared ref (:v1) but digest changed → drift, not a restamp.
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:    "image",
		Digest:  digest1,
		Version: "v1",
		Details: map[string]string{"declaredRef": "ghcr.io/example/echo-mcp:v1"},
	}
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", digest2, prev)
	require.NoError(t, err)
	assert.Nil(t, rec, "drift path must not update baseline")
	assert.True(t, drifted, "same ref + different digest = drift")
	assert.Contains(t, summary, digest1)
	assert.Contains(t, summary, digest2)
}

func TestComputeImagePin_OldRecord_NoDeclaredRef_TagBump_Restamps(t *testing.T) {
	// Older records that predate the declaredRef detail: fall back to comparing
	// Version (tag). If the tag changed, treat as fresh TOFU — restamp.
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:    "image",
		Digest:  digest1,
		Version: "v1",
		// No Details["declaredRef"] — simulates a record written before this field existed.
	}
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v2", digest2, prev)
	require.NoError(t, err)
	require.NotNil(t, rec, "version bump on old record must restamp baseline")
	assert.False(t, drifted)
	assert.Empty(t, summary)
	assert.Equal(t, digest2, rec.Digest)
	assert.Equal(t, "v2", rec.Version)
}

func TestComputeImagePin_OldRecord_NoDeclaredRef_SameTag_DigestMoved_IsDrift(t *testing.T) {
	// Older record without declaredRef detail, same tag, different digest → drift.
	prev := &spiceboxv1alpha1.PinRecord{
		Kind:    "image",
		Digest:  digest1,
		Version: "v1",
	}
	rec, drifted, summary, err := computeImagePin("ghcr.io/example/echo-mcp:v1", digest2, prev)
	require.NoError(t, err)
	assert.Nil(t, rec)
	assert.True(t, drifted, "old record same tag + different digest = drift")
	assert.Contains(t, summary, digest1)
	assert.Contains(t, summary, digest2)
}
