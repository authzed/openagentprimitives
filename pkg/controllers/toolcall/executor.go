package toolcall

import (
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// executorFor returns an exec transport bound to this session's sandbox.
//
// Fail-closed at both steps: a session that has not yet recorded a handle, or
// one whose backend is not linked into this binary, gets an error rather than a
// guess. Running a tool in the wrong sandbox — or in a default one that happens
// to exist — is worse than not running it.
func (r *Reconciler) executorFor(sess *spiceboxv1alpha1.SpiceboxSession) (exec.Executor, error) {
	h := sess.Status.Sandbox
	if h == nil {
		return nil, fmt.Errorf("session %s/%s has no sandbox handle yet", sess.Namespace, sess.Name)
	}
	rt, ok := r.Runtimes.For(h.Kind)
	if !ok {
		return nil, fmt.Errorf("no runtime for sandbox kind %q (session %s/%s)", h.Kind, sess.Namespace, sess.Name)
	}
	return rt.Executor(sandboxkinds.HandleFromStatus(h))
}

// fileTransfererFor reports whether this session's backend can move files
// natively, and if so returns the transferer and the session's handle.
//
// A backend without one is not an error: tar-over-exec is the mechanism every
// backend shipping today uses, and remains the fallback. The type assertion is
// the whole selection mechanism — there is deliberately no capability flag to
// contradict it.
func (r *Reconciler) fileTransfererFor(sess *spiceboxv1alpha1.SpiceboxSession) (sandboxkinds.FileTransferer, sandboxkinds.Handle, bool) {
	h := sess.Status.Sandbox
	if h == nil {
		return nil, sandboxkinds.Handle{}, false
	}
	rt, ok := r.Runtimes.For(h.Kind)
	if !ok {
		return nil, sandboxkinds.Handle{}, false
	}
	ft, ok := rt.(sandboxkinds.FileTransferer)
	if !ok {
		return nil, sandboxkinds.Handle{}, false
	}
	return ft, sandboxkinds.HandleFromStatus(h), true
}
