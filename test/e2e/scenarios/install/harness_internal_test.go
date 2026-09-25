//go:build e2e

package install_test

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/goleak"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestHarness_BootStop_NoGoroutineLeak verifies the harness's cleanup
// chain unwinds every goroutine Start spawned: controller-runtime
// manager, channelsd outbound relay, channel-listener starter, NATS
// connection drain, MCP stub server, and any in-flight runner
// goroutines from the InProcessRunnerFactory. Catches the kind of
// leak that produces flaky shared-resource interactions across tests
// (e.g. a stale runner goroutine that observes the NEXT test's NATS
// connection because the previous test's never exited).
//
// IgnoreCurrent snapshots the goroutine set BEFORE Start so anything
// the test runtime, dockertest container manager (started by a prior
// testspicedb.Endpoint call in another test in the same `go test`
// run), or shared gRPC connections leave running is excluded — those
// belong to infrastructure outside the harness's lifecycle and live
// until process exit by design. The remaining check verifies Start's
// NEW goroutines all unwound.
//
// If this fires for a goroutine that's actually a harness-owned leak
// (not infrastructure), FIX the harness — don't paper over with an
// IgnoreAnyFunction entry. The ignore list below is reserved for
// genuinely external leaks documented per-line.
func TestHarness_BootStop_NoGoroutineLeak(t *testing.T) {
	// Ordering: t.Cleanup callbacks run LIFO. The leak check is
	// registered FIRST (before Start), so it runs LAST — after every
	// cleanup Start registered has executed. That's the order we
	// want: assert "no leaked goroutines" only after teardown is
	// complete.
	t.Cleanup(func() {
		goleak.VerifyNone(t,
			// IgnoreCurrent snapshots the goroutine set at the moment
			// VerifyNone is called. By the time this cleanup fires,
			// Start's own cleanups have already run; anything still
			// live falls into one of two buckets:
			//   1. Goroutines that pre-existed Start (test runtime,
			//      dockertest container manager started by an earlier
			//      test's testspicedb.Endpoint call). IgnoreCurrent
			//      catches these by virtue of being "current" at this
			//      observation point.
			//   2. Genuine harness leaks. We do NOT want IgnoreCurrent
			//      to mask those, but it can only mask goroutines that
			//      are running NOW — by definition, a harness goroutine
			//      that should have exited but didn't is one IgnoreCurrent
			//      wouldn't have ignored at test start, and is therefore
			//      visible here as a leak.
			// Net effect: IgnoreCurrent excludes the persistent
			// infrastructure set, and any harness goroutine that
			// should have exited but didn't surfaces below.
			goleak.IgnoreCurrent(),
			// Shared SpiceDB container connection: the *spicedb.Client
			// the harness opens in Start gets t.Cleanup-closed, but
			// the gRPC stack keeps a balancer/resolver watcher around
			// briefly after Close returns. These belong to the
			// shared-container model (testspicedb.Endpoint), not the
			// per-test harness lifecycle.
			goleak.IgnoreAnyFunction("google.golang.org/grpc.(*ccBalancerWrapper).watcher"),
			goleak.IgnoreAnyFunction("google.golang.org/grpc.(*Server).handleRawConn"),
			goleak.IgnoreAnyFunction("google.golang.org/grpc/internal/grpcsync.(*CallbackSerializer).run"),
			// dockertest's container manager (started by the first
			// testspicedb.Endpoint() call in the test process) keeps
			// a connection pool to the docker daemon for the lifetime
			// of the run.
			goleak.IgnoreAnyFunction("github.com/ory/dockertest/v3.(*Pool).Retry"),
		)
	})

	h := e2e.Start(t, e2e.Options{})
	_ = h
	// Start's t.Cleanup registrations handle teardown; the leak
	// check registered above runs after them all.
}

// TestHarness_DiagnosticDump_StructureCheck verifies the diagnostic
// dump rendered on Expect* timeouts carries the section headers tests
// rely on. The dump's actual content is exercised every time an
// ExpectAgentReply times out in another test — this self-test just
// pins the section structure so a future refactor of dumpState that
// renames a header surfaces here, not as a confusing failure-mode
// dump in some other test's timeout output.
func TestHarness_DiagnosticDump_StructureCheck(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
	})

	// Pre-seed the MCPStub with the centerdot allowlisted tools so
	// the AgentClass reaches Valid=True (same shape as the smoke test
	// in harness_smoketest_test.go).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Drive one turn end-to-end so the dump has a non-empty Channel
	// outbound list and a non-empty LLM-requests-served list to
	// render. Without this, the structural assertions would still
	// pass on header names, but the test would miss a regression
	// where the loop runs but the per-entry render goes empty.
	//
	// .Repeating mirrors TestConversation_PingPong: after
	// respond_to_user the runner does one more LLM turn carrying the
	// tool_result, and an unmatched Send would fail the test inside
	// ScriptedLLM. Repeating the rule keeps the second turn matched
	// — it'll re-emit respond_to_user, the outbound relay has
	// nothing new to deliver (same envelope content), and the loop
	// converges via the runner's end-of-turn detection.
	h.LLM.OnUserMessage("hello").Reply(e2e.RespondToUser("hi back")).Repeating()
	h.SendUserMessage("hello")
	h.ExpectAgentReply(e2e.Contains("hi back"))

	dump := h.DumpState()

	// Use Regexp for the outbound section: the header and the message
	// text appear in the same dump but on different lines, and a (?s)
	// regex makes the "header THEN the message somewhere later" intent
	// explicit. The other two are plain substring checks.
	assert.Regexp(t,
		regexp.MustCompile(`(?s)Channel outbound events.*hi back`),
		dump,
		"dump must include the Channel outbound events header followed by the captured outbound text")
	assert.Contains(t, dump, "LLM requests served",
		"dump must include the LLM requests section header")
	assert.Contains(t, dump, "AgentSession",
		"dump must include the AgentSessions section header")
}
