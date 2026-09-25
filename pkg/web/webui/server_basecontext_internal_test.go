package webui

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// capturingSink records the messages logged to it, so the test can assert a
// logger flowed through a context without depending on process-global logger
// state (which a transitive spicedb init() poisons with a Nop sink — the very
// bug newHTTPServer's BaseContext works around).
type capturingSink struct{ msgs []string }

func (c *capturingSink) Init(logr.RuntimeInfo)               {}
func (c *capturingSink) Enabled(int) bool                    { return true }
func (c *capturingSink) Info(_ int, msg string, _ ...any)    { c.msgs = append(c.msgs, msg) }
func (c *capturingSink) Error(_ error, msg string, _ ...any) { c.msgs = append(c.msgs, msg) }
func (c *capturingSink) WithValues(_ ...any) logr.LogSink    { return c }
func (c *capturingSink) WithName(_ string) logr.LogSink      { return c }

// TestNewHTTPServerBaseContextCarriesLogger pins the no-silent-errors fix: the
// server's BaseContext must carry the ctx (and its logger) into request
// contexts, so handler log.FromContext returns webd's injected logger rather
// than the spicedb-poisoned global delegating logger. net/http guarantees
// every request context derives from BaseContext, so verifying BaseContext
// yields the injected logger proves handlers will see it.
func TestNewHTTPServerBaseContextCarriesLogger(t *testing.T) {
	sink := &capturingSink{}
	logger := logr.New(sink)
	ctx := log.IntoContext(context.Background(), logger)

	srv := newHTTPServer(ctx, ":0", http.NewServeMux())
	require.NotNil(t, srv.BaseContext, "BaseContext must be set so request contexts carry the logger")

	got := srv.BaseContext(nil)
	log.FromContext(got).Info("surfaced")

	require.Len(t, sink.msgs, 1, "log.FromContext on the base context must reach the injected logger, not a discard sink")
	assert.Equal(t, "surfaced", sink.msgs[0])
}
