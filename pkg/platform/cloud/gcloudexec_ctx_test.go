package cloud

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExternalToolCtx_IgnoresParentDeadline is the regression for the "signal:
// killed at 33% of a node-pool update" bug: a gcloud long-running operation runs
// under the install's --timeout context, and when that deadline fired mid-update
// exec.CommandContext SIGKILLed gcloud. externalToolCtx must NOT propagate a bare
// timeout to the child.
func TestExternalToolCtx_IgnoresParentDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	t.Cleanup(cancel)

	toolCtx, stop := externalToolCtx(parent)
	t.Cleanup(stop)

	// Let the parent deadline elapse.
	<-parent.Done()
	require.ErrorIs(t, parent.Err(), context.DeadlineExceeded, "parent must have hit its deadline")

	// Give the AfterFunc a moment to (wrongly) fire if the semantics regress.
	time.Sleep(20 * time.Millisecond)
	assert.NoError(t, toolCtx.Err(), "a parent timeout must NOT cancel the external-tool context")
}

// TestExternalToolCtx_PropagatesCancel proves the other half: an explicit parent
// cancellation (stands in for SIGINT via the install's signal context) DOES
// terminate the child, so Ctrl-C still aborts a stuck tool.
func TestExternalToolCtx_PropagatesCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	toolCtx, stop := externalToolCtx(parent)
	t.Cleanup(stop)

	assert.NoError(t, toolCtx.Err(), "child is live until the parent is canceled")
	cancel()

	select {
	case <-toolCtx.Done():
		assert.ErrorIs(t, toolCtx.Err(), context.Canceled, "explicit parent cancel must propagate")
	case <-time.After(time.Second):
		t.Fatal("external-tool context was not canceled when the parent was canceled")
	}
}

// TestExternalToolCtx_AppliesOwnTimeout proves the child still has a finite
// (generous) deadline of its own, so a genuinely wedged tool eventually fails
// instead of hanging forever.
func TestExternalToolCtx_AppliesOwnTimeout(t *testing.T) {
	old := defaultExternalToolTimeout
	t.Cleanup(func() { defaultExternalToolTimeout = old })
	defaultExternalToolTimeout = 20 * time.Millisecond

	toolCtx, stop := externalToolCtx(context.Background())
	t.Cleanup(stop)

	select {
	case <-toolCtx.Done():
		assert.ErrorIs(t, toolCtx.Err(), context.DeadlineExceeded, "the external-tool timeout must fire on its own")
	case <-time.After(time.Second):
		t.Fatal("external-tool context never hit its own timeout")
	}
}

// TestExternalToolCtx_HonorsContextTimeout proves --external-tool-timeout takes
// effect: a value carried via WithExternalToolTimeout overrides the default and
// survives the WithoutCancel detach (values are preserved).
func TestExternalToolCtx_HonorsContextTimeout(t *testing.T) {
	parent := WithExternalToolTimeout(context.Background(), 20*time.Millisecond)
	toolCtx, stop := externalToolCtx(parent)
	t.Cleanup(stop)

	select {
	case <-toolCtx.Done():
		assert.ErrorIs(t, toolCtx.Err(), context.DeadlineExceeded, "the context-carried timeout must bound the tool")
	case <-time.After(time.Second):
		t.Fatal("external-tool context did not honor WithExternalToolTimeout")
	}
}

// TestExternalToolCtx_StopReleases ensures the returned stop() cancels the child
// (and unregisters the propagation hook) so a completed gcloud call leaks nothing.
func TestExternalToolCtx_StopReleases(t *testing.T) {
	parent := context.Background()
	toolCtx, stop := externalToolCtx(parent)
	stop()
	assert.ErrorIs(t, toolCtx.Err(), context.Canceled, "stop() must cancel the child context")
}
