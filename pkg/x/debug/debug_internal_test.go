package debug

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

func TestRefSessionKeyUsesStoreKey(t *testing.T) {
	// A store whose base carries a bucket segment must still yield
	// (ns, session) from the KEY, not from the raw ref path.
	dir := t.TempDir()
	s, err := blobstore.Open(context.Background(), "file://"+dir+"?create_dir=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	ref, err := s.Put(context.Background(), "team-ns/sess-1/call/stdout", strings.NewReader("x"))
	require.NoError(t, err)

	ns, sess, ok := refSessionKey(s, string(ref))
	require.True(t, ok)
	assert.Equal(t, "team-ns", ns)
	assert.Equal(t, "sess-1", sess)

	_, _, ok = refSessionKey(s, "gs://foreign-bucket/ns/sess/x")
	assert.False(t, ok, "foreign ref must not parse")
}

// TestRefSessionKeyRecoversInboundAssetKey proves refSessionKey correctly
// recovers (ns, session) from a key laid out the way
// pkg/memory/httpsrv's /inbound-asset route builds it:
// path.Join(ns, sess, "inbound-asset", assetID, filename). fetch_artifact —
// the only documented read path for a stored attachment (see
// pkg/agent/tool/sandbox/artifact.go's HTTPArtifactClient, which hits
// /debug/artifact) — authorizes a per-session memory token against exactly
// this recovered (ns, session) pair, so a wrong layout here means every
// attachment Ref/TextRef is an unreadable dead handle. A prior
// "inbound-asset/<ns>/<sess>/…" layout parsed as ns="inbound-asset",
// sess="<ns>" and could never authorize — this test is the regression guard
// for that class of bug, not just today's specific layout.
func TestRefSessionKeyRecoversInboundAssetKey(t *testing.T) {
	dir := t.TempDir()
	s, err := blobstore.Open(context.Background(), "file://"+dir+"?create_dir=true")
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	key := path.Join("team-ns", "sess-1", "inbound-asset", "asset-uuid-1", "report.pdf")
	ref, err := s.Put(context.Background(), key, strings.NewReader("pdf bytes"))
	require.NoError(t, err)

	ns, sess, ok := refSessionKey(s, string(ref))
	require.True(t, ok, "refSessionKey must recover ns/session from an inbound-asset key")
	assert.Equal(t, "team-ns", ns)
	assert.Equal(t, "sess-1", sess)
}
