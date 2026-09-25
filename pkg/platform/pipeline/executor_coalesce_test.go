package pipeline_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// A turn's tool calls are dispatched CONCURRENTLY (loop.go fans out one
// goroutine per tool_use). When several of them need the same unapproved plan
// phase, each independently reached the gate, found it unapproved, and raised
// its own approval card.
//
// Observed twice in one live session: two `card_built` records in the SAME
// second before any approval, and later two `amendment_requested` the same way.
// The user clicked four times for two decisions.
//
// The defect is not cosmetic. Approval volume scaled with the MODEL'S
// PARALLELISM rather than with what was being decided — the exact property the
// tier system exists to control. A five-step phase would have produced five
// identical cards.
func TestExecutor_coalescesConcurrentAsksWithTheSameKey(t *testing.T) {
	host := &fakeHost{approveResult: true}
	// Block inside AwaitDecision until both callers are inside Run, so the
	// second genuinely races the first rather than arriving after it resolved.
	release := make(chan struct{})
	host.awaitHook = func() { <-release }

	reg := pipeline.NewRegistry()
	reg.Register(askingHook{key: "plan:abc#0"}, 10)
	ex := pipeline.NewExecutor(reg)

	var wg sync.WaitGroup
	outcomes := make([]pipeline.Outcome, 2)
	for i := range outcomes {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			o, _ := ex.Run(context.Background(), pipeline.PreToolCall,
				pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "t", UseID: "u"}}, host)
			outcomes[i] = o
		}()
	}
	// Give both goroutines time to reach the ask, then let the decision resolve.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Len(t, host.publishedAsks(), 1,
		"two concurrent callers needing the SAME decision must produce ONE card")
	assert.Equal(t, outcomes[0].Verdict, outcomes[1].Verdict,
		"both callers must observe the same decision; a coalesced waiter that "+
			"guessed its own outcome would be worse than a duplicate card")
}

// Different decisions must NOT be merged. Two calls needing different additions
// are genuinely two things to decide, and folding them into one card would make
// a single click grant authority the approver never separately considered.
func TestExecutor_doesNotCoalesceDifferentKeys(t *testing.T) {
	host := &fakeHost{approveResult: true}
	reg := pipeline.NewRegistry()
	reg.Register(askingHook{key: "plan:abc#0"}, 10)
	reg2 := pipeline.NewRegistry()
	reg2.Register(askingHook{key: "plan:abc#1"}, 10)

	ex := pipeline.NewExecutor(reg)
	ex2 := pipeline.NewExecutor(reg2)
	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "t", UseID: "u"}}
	_, _ = ex.Run(context.Background(), pipeline.PreToolCall, in, host)
	_, _ = ex2.Run(context.Background(), pipeline.PreToolCall, in, host)

	assert.Len(t, host.publishedAsks(), 2, "different keys are different decisions")
}

// No key means no coalescing — today's behaviour, preserved. An ask that does
// not opt in must never be merged with anything.
func TestExecutor_withoutAKeyEveryAskIsItsOwnDecision(t *testing.T) {
	host := &fakeHost{approveResult: true}
	reg := pipeline.NewRegistry()
	reg.Register(askingHook{key: ""}, 10)
	ex := pipeline.NewExecutor(reg)

	in := pipeline.Input{Tool: &pipeline.ToolCallInfo{Name: "t", UseID: "u"}}
	_, _ = ex.Run(context.Background(), pipeline.PreToolCall, in, host)
	_, _ = ex.Run(context.Background(), pipeline.PreToolCall, in, host)

	assert.Len(t, host.publishedAsks(), 2, "an ask with no coalesce key stands alone")
}

// askingHook always asks for approval, with a settable coalesce key.
type askingHook struct{ key string }

func (askingHook) Name() string             { return "asking" }
func (askingHook) Points() []pipeline.Point { return []pipeline.Point{pipeline.PreToolCall} }
func (h askingHook) Eval(context.Context, pipeline.Input) pipeline.Decision {
	return pipeline.Decision{
		Approval: &pipeline.ApprovalAsk{Kind: "plan_phase", Summary: "s", CoalesceKey: h.key},
	}
}
