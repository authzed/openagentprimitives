package toolcall

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/exec"
	"github.com/authzed/openagentprimitives/pkg/tools/sandboxkinds"
)

// plainRuntime implements only sandboxkinds.Runtime — the shape of every
// backend shipping today (pod, and agent-sandbox once it lands). It must
// fall through to tar-over-exec.
type plainRuntime struct{}

func (plainRuntime) Ensure(ctx context.Context, req sandboxkinds.EnsureRequest) (sandboxkinds.Handle, error) {
	return sandboxkinds.Handle{}, nil
}

func (plainRuntime) Status(ctx context.Context, h sandboxkinds.Handle) (sandboxkinds.Status, error) {
	return sandboxkinds.Status{}, nil
}

func (plainRuntime) Teardown(ctx context.Context, h sandboxkinds.Handle) error {
	return nil
}

func (plainRuntime) Executor(h sandboxkinds.Handle) (exec.Executor, error) {
	return nil, nil
}

func (plainRuntime) Watches() []sandboxkinds.Watch {
	return nil
}

// transferRuntime additionally implements sandboxkinds.FileTransferer — a
// backend with a native file API that can skip tar-over-exec.
type transferRuntime struct {
	plainRuntime
}

func (transferRuntime) PutFile(ctx context.Context, h sandboxkinds.Handle, path string, data []byte) error {
	return nil
}

func (transferRuntime) GetFile(ctx context.Context, h sandboxkinds.Handle, path string) ([]byte, error) {
	return nil, nil
}

// A backend without a native file API must fall through to tar-over-exec —
// that is every backend shipping today, so the fallback is the common path.
func TestFileTransfererFor_AbsentWhenBackendHasNoNativeAPI(t *testing.T) {
	r := &Reconciler{Runtimes: sandboxkinds.Runtimes{"demo": plainRuntime{}}}
	sess := &spiceboxv1alpha1.SpiceboxSession{}
	sess.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{Kind: "demo", Ref: "r"}

	_, _, ok := r.fileTransfererFor(sess)
	assert.False(t, ok, "a runtime that does not implement FileTransferer must fall back to tar")
}

// A backend that implements it is used, and gets the session's handle.
func TestFileTransfererFor_PresentAndCarriesTheHandle(t *testing.T) {
	r := &Reconciler{Runtimes: sandboxkinds.Runtimes{"demo": transferRuntime{}}}
	sess := &spiceboxv1alpha1.SpiceboxSession{}
	sess.Status.Sandbox = &spiceboxv1alpha1.SandboxHandle{Kind: "demo", Ref: "demo-ref"}

	ft, h, ok := r.fileTransfererFor(sess)
	require.True(t, ok)
	require.NotNil(t, ft)
	assert.Equal(t, "demo-ref", h.Ref, "the transferer must be given this session's handle")
}

// A session with no sandbox handle yet has nothing to select a transferer
// for — falls back to tar the same as a kind with no runtime.
func TestFileTransfererFor_AbsentWhenNoSandboxHandleYet(t *testing.T) {
	r := &Reconciler{Runtimes: sandboxkinds.Runtimes{"demo": transferRuntime{}}}
	sess := &spiceboxv1alpha1.SpiceboxSession{}

	_, _, ok := r.fileTransfererFor(sess)
	assert.False(t, ok, "no sandbox handle yet must fall back to tar, not guess a backend")
}
