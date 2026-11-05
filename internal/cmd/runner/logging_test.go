package main

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// log.FromContext is how respond_to_user, the dispatch loop and authz slot
// promotion log. Before loggingContext existed those lines went nowhere.
func TestLoggingContext_FromContextReachesSlogDefault(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logf.FromContext(loggingContext()).Info("fallback notice publish failed", "session", "ns/demo-agent")

	assert.Contains(t, buf.String(), "fallback notice publish failed")
	assert.Contains(t, buf.String(), "session=ns/demo-agent")
}
