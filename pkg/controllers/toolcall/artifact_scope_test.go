package toolcall

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	blobstore "github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
)

// refAt stores a byte under key and returns the ref the store minted for it,
// which is how a real ref is constructed — Put's return value, not a string
// the test assembles.
func refAt(t *testing.T, s *blobstore.Store, key string) string {
	t.Helper()
	ref, err := s.Put(context.Background(), key, strings.NewReader("x"))
	require.NoError(t, err)
	return string(ref)
}

// The artifact store's authorization model is "a token may read refs under its
// own <ns>/<session> key prefix" — pkg/x/debug enforces exactly that on the
// direct read path. The toolcall hydrate path fetched spec.inputArtifacts with
// the OPERATOR's unrestricted store and never compared the ref's prefix to the
// call's own session, so it was the way around that model.
//
// It needs no credentials and no grant: the attacker names their OWN bundle in
// spec.session (so the session-binding check passes and checkTokenUse never
// runs), points an input artifact at a sibling's stdout, and harvests the file
// back out under a prefix their own token may read.
func TestValidateInputArtifactRefs_RefusesAnotherSessionsArtifact(t *testing.T) {
	store := blobstore.NewMem()

	err := validateInputArtifactRefs(store, "ns1", "attacker-bundle", []string{
		refAt(t, store, "ns1/victim-bundle/uid-9/stdout"),
	})
	require.Error(t, err, "a ref under another session's key prefix must be refused")
	assert.Contains(t, err.Error(), "victim-bundle")
}

// The ordinary case: a call hydrating an artifact its own session produced.
func TestValidateInputArtifactRefs_AcceptsTheCallersOwnArtifact(t *testing.T) {
	store := blobstore.NewMem()

	require.NoError(t, validateInputArtifactRefs(store, "ns1", "sess-bundle", []string{
		refAt(t, store, "ns1/sess-bundle/uid-1/out/report.txt"),
	}))
}

// A ref in another NAMESPACE is refused even when the session segment matches:
// namespace is part of the identity, and the store's key space spans them.
func TestValidateInputArtifactRefs_RefusesACrossNamespaceRef(t *testing.T) {
	store := blobstore.NewMem()

	require.Error(t, validateInputArtifactRefs(store, "ns1", "sess-bundle", []string{
		refAt(t, store, "ns2/sess-bundle/uid-1/stdout"),
	}))
}

// A ref this store cannot parse into a scope proves nothing about who owns it,
// so it is refused rather than passed through. Treating an unparseable ref as
// acceptable is the same "unknown reads as fine" shape the binding check
// already fails closed on.
func TestValidateInputArtifactRefs_FailsClosedOnAnUnparseableRef(t *testing.T) {
	store := blobstore.NewMem()

	require.Error(t, validateInputArtifactRefs(store, "ns1", "sess-bundle", []string{
		"gs://some-other-bucket/whatever",
	}))
}

// Every ref is checked, not just the first — otherwise a call could pass by
// leading with a legitimate one.
func TestValidateInputArtifactRefs_ChecksEveryRef(t *testing.T) {
	store := blobstore.NewMem()

	err := validateInputArtifactRefs(store, "ns1", "sess-bundle", []string{
		refAt(t, store, "ns1/sess-bundle/uid-1/stdout"),
		refAt(t, store, "ns1/victim-bundle/uid-9/stdout"),
	})
	require.Error(t, err, "a legitimate first ref must not vouch for the rest")
}

// No input artifacts is the common case and must stay free.
func TestValidateInputArtifactRefs_AllowsNoArtifacts(t *testing.T) {
	require.NoError(t, validateInputArtifactRefs(blobstore.NewMem(), "ns1", "sess-bundle", nil))
}

// A nil store means the operator was built without one; there is then no way
// to establish a ref's scope, so hydration must not proceed.
func TestValidateInputArtifactRefs_FailsClosedWithNoStore(t *testing.T) {
	var store artifactstore.Store
	require.Error(t, validateInputArtifactRefs(store, "ns1", "sess-bundle", []string{"anything"}))
}
