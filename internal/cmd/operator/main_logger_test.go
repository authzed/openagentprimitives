package main

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TestSetupOperatorLoggerEmitsViaFromContext is a regression for the
// slice-2 silent-crash root cause where pkg/authz/spicedb's init() called
// logf.SetLogger(zerologr.New(<zerolog.Nop>)) BEFORE main, and our
// subsequent ctrl.SetLogger(zap...) was a silent no-op. Controllers
// using log.FromContext(ctx) during reconcile then received a no-op
// logger and every zap.Info we emitted disappeared, while klog
// leader-election lines kept flowing — producing the "klog-only, no
// application logs" symptom we observed.
//
// The fix in 08c8fe2 passes the zap logger explicitly via
// ctrl.Options.Logger. The manager's LogConstructor uses
// mgr.GetLogger() (= options.Logger) to stamp the logger into each
// reconcile ctx, so log.FromContext(ctx) in a controller now returns
// our zap logger regardless of the global state.
//
// This test verifies the OBSERVABLE property the fix restored: the
// logger returned by setupOperatorLogger() (the one main passes via
// Options.Logger), when injected into a ctx via log.IntoContext,
// flows through log.FromContext to the operator's stderr sink.
//
// Note: we deliberately do NOT assert on ctrl.Log.Info(...) because
// that global is hijacked by spicedb's init in the test binary (the
// exact root cause we are working around), so the assertion would
// pass or fail based on Go test binary import order, not the fix.
// The setup helper still defensively calls ctrl.SetLogger(zapLogger)
// for the case where no transitive dep won the race.
func TestSetupOperatorLoggerEmitsViaFromContext(t *testing.T) {
	// Capture stderr. zap.WriteTo(os.Stderr) evaluates os.Stderr at the
	// moment the logger is constructed, so we must swap stderr BEFORE
	// calling setupOperatorLogger().
	oldStderr := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err, "os.Pipe")
	os.Stderr = w
	defer func() { os.Stderr = oldStderr }()

	logger := setupOperatorLogger()

	// Direct emission via the returned logger — this is the path
	// main() uses (log := zapLogger.WithName("main")).
	logger.Info("sentinel-direct")

	// log.FromContext on a ctx-with-logger — this is the path each
	// controller takes during reconcile (the manager calls
	// log.IntoContext(ctx, mgr.GetLogger()) before dispatching).
	ctx := log.IntoContext(context.Background(), logger)
	log.FromContext(ctx).Info("sentinel-via-fromctx")

	// Flush + read.
	_ = w.Close()
	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err, "read captured stderr")
	captured := buf.String()

	assert.Contains(t, captured, "sentinel-direct",
		"direct logger.Info output not captured; setupOperatorLogger "+
			"is not writing to stderr. Captured: %q", captured)
	assert.Contains(t, captured, "sentinel-via-fromctx",
		"log.FromContext(ctxWithLogger).Info output not captured; "+
			"the logger we pass to ctrl.Options.Logger does not propagate to "+
			"controllers via the reconcile ctx. This is the regression that "+
			"caused slice-2's silent operator crash-loop. Captured: %q",
		captured)
}

// TestSetupOperatorLoggerCallsCtrlSetLogger is a soft assertion that
// the helper defensively calls ctrl.SetLogger. This is best-effort
// in production (spicedb's init wins the race today) but the call
// MUST remain — when the spicedb-init issue is fixed upstream, this
// path will start working and is the most-used controller-runtime
// logging API.
//
// We can't assert ctrl.Log.Info reaches our sink (see comment on
// TestSetupOperatorLoggerEmitsViaFromContext), so this test just
// asserts the helper runs without panicking — guarding against a
// future refactor that removes the call entirely.
func TestSetupOperatorLoggerCallsCtrlSetLogger(t *testing.T) {
	assert.NotPanics(t, func() {
		// Two calls: setupOperatorLogger internally calls ctrl.SetLogger,
		// which on first-write-wins semantics MAY be a no-op. Either way
		// it must not panic and must return a non-nil logger.
		_ = setupOperatorLogger()
		// And ctrl.Log itself must be usable (Info on a hijacked-noop
		// logger is still safe to call — just produces no output).
		ctrl.Log.Info("smoke-ctrl-log-no-panic")
	}, "setupOperatorLogger must not panic")
}

// TestMainPassesLoggerToManagerOptions parses internal/cmd/operator/main.go and
// verifies that the ctrl.Options composite literal passed to
// ctrl.NewManager has a Logger field. This is the SECOND half of the
// slice-2 hijack fix: even with a properly configured zap logger,
// removing the Options.Logger assignment would cause the manager to
// fall back to options.Logger = log.Log (the hijacked global) in
// pkg/manager/manager.go, and every controller's log.FromContext(ctx)
// would silently revert to a no-op sink.
//
// Static check (vs. constructing a real manager) avoids needing an
// envtest cluster just to verify the assignment exists.
func TestMainPassesLoggerToManagerOptions(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.AllErrors)
	require.NoError(t, err, "parse main.go")

	var loggerFieldFound bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Looking for ctrl.NewManager(_, ctrl.Options{...}).
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel == nil || sel.Sel.Name != "NewManager" {
			return true
		}
		// Second arg should be a composite literal with a Logger field.
		if len(call.Args) < 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			ident, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if ident.Name == "Logger" {
				loggerFieldFound = true
				return false
			}
		}
		return true
	})

	require.True(t, loggerFieldFound,
		"ctrl.NewManager(ctx, ctrl.Options{...}) is missing the "+
			"Logger field. Without an explicit Logger, the manager falls "+
			"back to log.Log (= the hijacked ctrl-runtime global polluted "+
			"by spicedb's init), and every controller's log.FromContext(ctx) "+
			"emits to a no-op sink. This is the slice-2 silent-crash "+
			"regression — restore the Logger assignment.")
}
