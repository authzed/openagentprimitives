package runner

import (
	"context"
	"sync"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestAppToolSessionContext_NoRaceWithRunEntrySetup stress-tests
// appToolSessionContext (the NATS-goroutine reader used by HandleAppToolCall)
// against a concurrent writer that mimics Run's entry-setup block: reassigning
// l.SessionContext and mutating its fields in place (loop.go's
// `sess := l.SessionContext ... l.SessionContext = sess` sequence). A
// deterministic race is impossible to construct, but 1000 concurrent
// iterations reliably surfaces an unguarded read/write under `go test -race`.
//
// This test is not meaningful without -race: without the race detector it
// will pass whether or not sessionCtxMu exists, since the shallow copy still
// produces a structurally valid (if possibly torn) SessionContext.
func TestAppToolSessionContext_NoRaceWithRunEntrySetup(t *testing.T) {
	l := &Loop{
		SessionContext: &tool.SessionContext{Namespace: "default", Name: "s"},
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "s"},
	}

	var wg sync.WaitGroup

	// writer: simulate Run's entry-setup reassigning + mutating under the lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			l.sessionCtxMu.Lock()
			sess := l.SessionContext
			if sess == nil {
				sess = &tool.SessionContext{}
			}
			sess.SubmitResult = func(tool.AgentResult) {}
			l.SessionContext = sess
			l.sessionCtxMu.Unlock()
		}
	}()

	// reader: the handler path (appToolSessionContext), on what would be the
	// NATS-subscription goroutine in production.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			_ = l.appToolSessionContext()
		}
	}()

	wg.Wait()
}

// TestMarkPlanStoppedBestEffort_NoRaceWithRunEntrySetup stress-tests
// MarkPlanStoppedBestEffort — invoked from internal/cmd/runner's SIGTERM/SIGINT
// stop-handler goroutine, independent of the loop goroutine running Run — against
// a concurrent writer mimicking Run's entry-setup block (the same
// `sess := l.SessionContext ... l.SessionContext = sess` sequence under
// sessionCtxMu.Lock as the sibling test above). The SessionContext carries no
// State, so plans.TryFrom fails and MarkPlanStoppedBestEffort returns right
// after its guarded pointer read — enough to exercise the read/write race
// without needing a real plans.Store.
//
// Not meaningful without -race: strip the sessionCtxMu.RLock/RUnlock in
// MarkPlanStoppedBestEffort (or in seqForEmit) and this test still passes
// under a plain `go test`, but fails under `go test -race`.
func TestMarkPlanStoppedBestEffort_NoRaceWithRunEntrySetup(t *testing.T) {
	l := &Loop{
		SessionContext: &tool.SessionContext{Namespace: "default", Name: "s"},
		SessionKey:     memory.NamespacedName{Namespace: "default", Name: "s"},
	}

	var wg sync.WaitGroup

	// writer: simulate Run's entry-setup reassigning + mutating under the lock.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			l.sessionCtxMu.Lock()
			sess := l.SessionContext
			if sess == nil {
				sess = &tool.SessionContext{}
			}
			sess.SubmitResult = func(tool.AgentResult) {}
			l.SessionContext = sess
			l.sessionCtxMu.Unlock()
		}
	}()

	// reader: the SIGTERM/SIGINT stop-handler path (MarkPlanStoppedBestEffort,
	// which internally also calls seqForEmit on the paused=true branch — not
	// reached here since the planless SessionContext returns early, but the
	// pointer read itself is the thing under test).
	wg.Add(1)
	go func() {
		defer wg.Done()
		ctx := context.Background()
		for i := 0; i < 1000; i++ {
			l.MarkPlanStoppedBestEffort(ctx)
		}
	}()

	wg.Wait()
}
