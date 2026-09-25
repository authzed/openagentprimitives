package mcpserver_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	toolscache "k8s.io/client-go/tools/cache"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/controllers/mcpserver"
)

func TestMCPServerRevokeKeyScope(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "github-mcp",
			Namespace: "default",
		},
	}
	key, scope := mcpserver.MCPServerRevokeKeyScope(cr)
	assert.Equal(t, "mcpserver/github-mcp", key, "key must match MCPTool.Origin()")
	assert.Equal(t, "default", scope, "scope must be the CR namespace")
}

func TestMCPServerFromDelete_DirectObject(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "my-server", Namespace: "ns1"},
	}
	got := mcpserver.MCPServerFromDelete(cr)
	require.NotNil(t, got)
	assert.Equal(t, "my-server", got.Name)
}

func TestMCPServerFromDelete_Tombstone(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "my-server", Namespace: "ns1"},
	}
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "ns1/my-server",
		Obj: cr,
	}
	got := mcpserver.MCPServerFromDelete(tomb)
	require.NotNil(t, got)
	assert.Equal(t, "my-server", got.Name)
}

func TestMCPServerFromDelete_Garbage(t *testing.T) {
	got := mcpserver.MCPServerFromDelete("not-a-cr")
	assert.Nil(t, got)
}

func TestMCPServerFromDelete_TombstoneWithGarbage(t *testing.T) {
	tomb := toolscache.DeletedFinalStateUnknown{
		Key: "ns1/my-server",
		Obj: "not-a-cr",
	}
	got := mcpserver.MCPServerFromDelete(tomb)
	assert.Nil(t, got)
}

// flakyEmitter fails its first failures calls, then succeeds, recording every
// (kind, key, scope) it was asked to publish — including the failed attempts.
type flakyEmitter struct {
	mu       sync.Mutex
	failures int
	calls    [][3]string
}

func (f *flakyEmitter) Emit(_ context.Context, kind, key, scope string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{kind, key, scope})
	if f.failures > 0 {
		f.failures--
		return errors.New("nats: connection closed")
	}
	return nil
}

func (f *flakyEmitter) snapshot() [][3]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][3]string(nil), f.calls...)
}

// fastBackoff keeps the retry tests sub-millisecond while preserving the
// attempt count that matters.
var fastBackoff = wait.Backoff{Steps: 5, Duration: time.Microsecond, Factor: 1.0}

func TestEmitDeleteRevoke_RetriesUntilThePublishSucceeds(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: "default"},
	}
	em := &flakyEmitter{failures: 3}
	mcpserver.EmitDeleteRevoke(context.Background(), em, logr.Discard(), cr, fastBackoff)

	calls := em.snapshot()
	require.Len(t, calls, 4, "the delete revoke has no durable anchor, so a transient bus failure must be retried in process")
	assert.Equal(t, [3]string{"tool-origin", "mcpserver/github-mcp", "default"}, calls[len(calls)-1],
		"every attempt must carry the same key and scope MCPTool.Origin() uses")
}

func TestEmitDeleteRevoke_GivesUpAfterTheBackoffIsExhausted(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "github-mcp", Namespace: "default"},
	}
	em := &flakyEmitter{failures: 100}
	mcpserver.EmitDeleteRevoke(context.Background(), em, logr.Discard(), cr, fastBackoff)

	assert.Len(t, em.snapshot(), fastBackoff.Steps,
		"retries must be bounded; an unbounded loop would outlive the origin it is revoking")
}

func TestEmitDeleteRevoke_UnrecognizedDeleteObjectEmitsNothing(t *testing.T) {
	em := &flakyEmitter{}
	mcpserver.EmitDeleteRevoke(context.Background(), em, logr.Discard(), "not-a-cr", fastBackoff)
	assert.Empty(t, em.snapshot(), "nothing to revoke when the event carries no MCPServer")
}
