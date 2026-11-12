package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// lockedBuffer is a bytes.Buffer safe for the concurrent writes a process-wide
// log destination can receive.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// installedOutput backs installedHandler. controller-runtime's SetLogger is
// first-write-wins and cannot be undone, so the handler installLogging is
// tested with must be the same one on every run in this process (-count=2
// included); only its buffer is reset.
var (
	installedOutput  = &lockedBuffer{}
	installedHandler = slog.NewTextHandler(installedOutput, nil)
)

// These tests cover the two helpers main() and run() call. They cannot prove
// that main() calls installLogging or that run() builds its root context with
// newRootContext — neither is reachable from a unit test.

// Not parallel: installLogging mutates slog's default logger, the stdlib log
// package's output, and controller-runtime's global logger.
func TestInstallLogging(t *testing.T) {
	prevWriter, prevFlags := log.Writer(), log.Flags()

	t.Run("slog default and controller-runtime's global logger reach the handler", func(t *testing.T) {
		prevDefault := slog.Default()
		t.Cleanup(func() {
			slog.SetDefault(prevDefault)
			// SetDefault with a non-default handler also redirects the stdlib
			// log package; restoring the previous default does not undo that.
			log.SetOutput(prevWriter)
			log.SetFlags(prevFlags)
		})
		installedOutput.Reset()

		installLogging(installedHandler)

		slog.Info("via slog default")
		logf.Log.Info("fallback notice publish failed", "session", "ns/demo-agent")
		logf.Log.Error(errors.New("bus closed"), "publish errored")

		out := installedOutput.String()
		assert.Contains(t, out, "via slog default")
		assert.Contains(t, out, "fallback notice publish failed")
		assert.Contains(t, out, "session=ns/demo-agent")
		assert.Contains(t, out, `level=ERROR msg="publish errored" err="bus closed"`)
	})

	assert.Same(t, prevWriter, log.Writer(), "stdlib log output restored after cleanup")
	assert.Equal(t, prevFlags, log.Flags(), "stdlib log flags restored after cleanup")
}

// log.FromContext is how respond_to_user, the dispatch loop and authz slot
// promotion log.
func TestNewRootContext_FromContextReachesHandler(t *testing.T) {
	var buf bytes.Buffer
	ctx, cancel := newRootContext(slog.NewTextHandler(&buf, nil))

	logf.FromContext(ctx).Info("fallback notice publish failed", "session", "ns/demo-agent")

	assert.Contains(t, buf.String(), "fallback notice publish failed")
	assert.Contains(t, buf.String(), "session=ns/demo-agent")

	assert.NoError(t, ctx.Err())
	cancel()
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
