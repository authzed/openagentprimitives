package mcpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/retry"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// toolOriginRevokeKind is the registered revocation.Invalidator kind the
// MCPServer-delete revoke targets — the same kind the settings publisher uses.
const toolOriginRevokeKind = "tool-origin"

// RevokeEmitter is the slice of revocation.Publisher the delete path needs.
// Narrowed to one method so the retry behaviour can be tested without a bus.
type RevokeEmitter interface {
	Emit(ctx context.Context, kind, key, scope string) error
}

// DeleteRevokeBackoff is the retry schedule for the delete-time tool-origin
// revoke: five attempts over roughly three seconds.
//
// Unlike every other revocation trigger in the operator, this one has NO
// durable anchor to re-derive from. The MCPServer is gone by the time the
// event fires, so there is no object whose status could record "this origin
// still owes a revoke", and no reconcile can ever rediscover it — the informer
// delete notification is the only signal that will ever exist. Retrying in
// process is therefore the entire recovery story, which is why it exists at
// all and why exhausting it is logged as the capability leak it is.
var DeleteRevokeBackoff = wait.Backoff{
	Steps:    5,
	Duration: 200 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

// MCPServerRevokeKeyScope returns the revocation key and scope for an MCPServer
// CR deletion event. The key matches MCPTool.Origin() = "mcpserver/<name>".
// MCPServer is namespace-scoped so the scope is the CR's namespace.
// Exported so unit tests can assert the exact key/scope values directly.
func MCPServerRevokeKeyScope(cr *spiceboxv1alpha1.MCPServer) (key, scope string) {
	return "mcpserver/" + cr.Name, cr.Namespace
}

// MCPServerFromDelete extracts the MCPServer from a cache delete notification,
// handling both the direct object and the DeletedFinalStateUnknown tombstone.
// Returns nil if the object is neither.
// Exported so unit tests can exercise the helper independently of the informer wiring.
func MCPServerFromDelete(obj any) *spiceboxv1alpha1.MCPServer {
	if cr, ok := obj.(*spiceboxv1alpha1.MCPServer); ok {
		return cr
	}
	if tomb, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		if cr, ok := tomb.Obj.(*spiceboxv1alpha1.MCPServer); ok {
			return cr
		}
	}
	return nil
}

// EmitDeleteRevoke publishes the tool-origin revoke for a deleted MCPServer,
// retrying on the given backoff. It blocks for the length of the backoff, so
// the informer handler runs it in a goroutine.
//
// Every exit that does NOT publish says so, and says what it costs: a live
// session holding tools from this origin keeps them until the session ends,
// because the runner applies the allowlist once at pod boot and the revoke is
// the only mid-session invalidation path.
func EmitDeleteRevoke(ctx context.Context, emitter RevokeEmitter, logger logr.Logger, obj any, backoff wait.Backoff) {
	cr := MCPServerFromDelete(obj)
	if cr == nil {
		logger.Info("mcpserver delete event carried neither an MCPServer nor a tombstone holding one; NO tool-origin revoke was emitted, so live sessions keep this origin's tools until they end",
			"objType", fmt.Sprintf("%T", obj))
		return
	}
	key, scope := MCPServerRevokeKeyScope(cr)
	// Every publish failure is retriable: the bus is either up or it is not,
	// and there is no error class here that a later attempt cannot clear.
	err := retry.OnError(backoff, func(error) bool { return true }, func() error {
		return emitter.Emit(ctx, toolOriginRevokeKind, key, scope)
	})
	if err != nil {
		logger.Info("emit tool-origin revoke on delete FAILED after every retry; this origin can never be revoked again — no object survives the delete for a reconcile to re-derive it from — so live sessions keep its tools until they end",
			"server", cr.Namespace+"/"+cr.Name, "key", key, "scope", scope, "err", err.Error())
		return
	}
	logger.Info("tool-origin revoke published on delete",
		"server", cr.Namespace+"/"+cr.Name, "key", key, "scope", scope)
}
