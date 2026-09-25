package runner

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
)

// shrinkInboundAttachmentWait shrinks the fence's budget and poll interval for
// the duration of one test, so the expiry case is provable in milliseconds
// instead of the production five minutes. NOT parallel-safe: both are
// package-level vars, which is exactly why they are vars.
func shrinkInboundAttachmentWait(t *testing.T, budget, poll time.Duration) {
	t.Helper()
	oldBudget, oldPoll := inboundAttachmentWaitBudget, inboundAttachmentPoll
	inboundAttachmentWaitBudget, inboundAttachmentPoll = budget, poll
	t.Cleanup(func() {
		inboundAttachmentWaitBudget, inboundAttachmentPoll = oldBudget, oldPoll
	})
}

// newFenceLoop builds a minimal *Loop with Memory wired and the session
// annotated with count attachments (count <= 0 leaves the annotation off).
func newFenceLoop(t *testing.T, count int) *Loop {
	t.Helper()
	ann := map[string]string{}
	if count > 0 {
		ann[spiceboxv1alpha1.AnnotationInboundAttachmentCount] = strconv.Itoa(count)
	}
	return &Loop{
		Memory: LocalMemoryAdapter(
			memory.NewLocal(inmem.NewBackend()),
			memory.NamespacedName{Namespace: "default", Name: "s1"},
		),
		SessionKey:   memory.NamespacedName{Namespace: "default", Name: "s1"},
		AgentSession: &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default", Annotations: ann}},
	}
}

// TestAwaitInboundAttachmentTurn covers the fence's whole decision surface:
// when it must not wait at all, when it waits and is released, and when it
// gives up. The window it closes is real — channelsd writes the attachment
// turn only after a per-file network round trip, while the runner is already
// running — so "returns promptly" and "waits until the turn lands" are
// different facts and each needs its own case.
func TestAwaitInboundAttachmentTurn(t *testing.T) {
	cases := []struct {
		name string
		// annotated is the attachment count stamped at session creation;
		// 0 means the annotation is absent (the no-attachments message).
		annotated int
		// preexisting turns seeded before the wait begins.
		preexisting []memory.Turn
		// appendAfter, when non-nil, is appended from another goroutine
		// shortly after the wait starts — the real ordering.
		appendAfter *memory.Turn
		// wantReleasedByTurn asserts the fence let go because the turn was
		// there (or because there was nothing to wait for), not because the
		// budget ran out.
		wantReleasedByTurn bool
	}{
		{
			name:               "no annotation (message carried no files): returns immediately, never waits",
			annotated:          0,
			wantReleasedByTurn: true, // vacuously — nothing to wait for
		},
		{
			name:               "annotated, turn already durable: returns immediately",
			annotated:          1,
			preexisting:        []memory.Turn{drainTextTurn(1, "inbox", "[deck.pdf was attached.]")},
			wantReleasedByTurn: true,
		},
		{
			name:               "annotated, turn lands mid-wait: blocks until it does, then returns",
			annotated:          1,
			appendAfter:        ptrTurn(drainTextTurn(1, "inbox", "[deck.pdf was attached.]")),
			wantReleasedByTurn: true,
		},
		{
			name:      "annotated, turn present but already consumed: a drained turn is proof the write landed, not a reason to wait",
			annotated: 1,
			preexisting: []memory.Turn{
				drainTextTurn(1, "inbox", "[deck.pdf was attached.]"),
				drainTextTurn(1, "inbox_done", "inbox entry consumed"),
			},
			wantReleasedByTurn: true,
		},
		{
			name:               "annotated, writer never wrote: gives up at the budget rather than hanging the session",
			annotated:          2,
			wantReleasedByTurn: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkInboundAttachmentWait(t, 300*time.Millisecond, 5*time.Millisecond)
			l := newFenceLoop(t, tc.annotated)
			ctx := memory.WithSystemApproval(context.Background(), "test")
			for _, tn := range tc.preexisting {
				require.NoError(t, l.Memory.Append(ctx, tn), "seed turn")
			}
			if tc.appendAfter != nil {
				tn := *tc.appendAfter
				go func() {
					time.Sleep(40 * time.Millisecond)
					assert.NoError(t, l.Memory.Append(ctx, tn), "late append")
				}()
			}

			start := time.Now()
			l.awaitInboundAttachmentTurn(ctx)
			elapsed := time.Since(start)

			if tc.wantReleasedByTurn {
				assert.Less(t, elapsed, inboundAttachmentWaitBudget,
					"must be released by the turn's arrival, not by the budget expiring")
			} else {
				assert.GreaterOrEqual(t, elapsed, inboundAttachmentWaitBudget,
					"must hold the dispatch for the whole budget before giving up")
			}
			if tc.appendAfter != nil {
				assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond,
					"must actually have blocked until the late write landed")
			}
		})
	}
}

// TestAwaitInboundAttachmentTurn_NoSessionCRIsANoOp pins the kubectl-driven /
// test-wired shape: no AgentSession pointer means no annotation to read and
// therefore nothing to wait for. Without this the fence would burn its whole
// budget on every session that never had a channel in front of it.
func TestAwaitInboundAttachmentTurn_NoSessionCRIsANoOp(t *testing.T) {
	shrinkInboundAttachmentWait(t, 300*time.Millisecond, 5*time.Millisecond)
	l := newFenceLoop(t, 0)
	l.AgentSession = nil

	start := time.Now()
	l.awaitInboundAttachmentTurn(memory.WithSystemApproval(context.Background(), "test"))
	assert.Less(t, time.Since(start), 100*time.Millisecond, "no session CR ⇒ no wait")
}

// TestInboundAttachmentCount pins the annotation's read side. A value nothing
// deliberately wrote must read as absent: the fence holds the user's first
// reply, so a garbage annotation must not be able to hold it for the budget.
func TestInboundAttachmentCount(t *testing.T) {
	cases := []struct {
		name        string
		ann         map[string]string
		wantCount   int
		wantPresent bool
	}{
		{name: "absent: no fence", ann: nil, wantPresent: false},
		{name: "one attachment: fenced, count 1", ann: map[string]string{spiceboxv1alpha1.AnnotationInboundAttachmentCount: "1"}, wantCount: 1, wantPresent: true},
		{name: "ten attachments: fenced, count 10", ann: map[string]string{spiceboxv1alpha1.AnnotationInboundAttachmentCount: "10"}, wantCount: 10, wantPresent: true},
		{name: "non-numeric: reads as absent, never fences", ann: map[string]string{spiceboxv1alpha1.AnnotationInboundAttachmentCount: "lots"}, wantPresent: false},
		{name: "zero: reads as absent, never fences", ann: map[string]string{spiceboxv1alpha1.AnnotationInboundAttachmentCount: "0"}, wantPresent: false},
		{name: "negative: reads as absent, never fences", ann: map[string]string{spiceboxv1alpha1.AnnotationInboundAttachmentCount: "-3"}, wantPresent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			count, present := spiceboxv1alpha1.InboundAttachmentCount(tc.ann)
			assert.Equal(t, tc.wantPresent, present)
			assert.Equal(t, tc.wantCount, count)
		})
	}
}

func ptrTurn(t memory.Turn) *memory.Turn { return &t }
