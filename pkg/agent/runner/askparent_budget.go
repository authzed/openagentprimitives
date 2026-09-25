package runner

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// AskParentBudgetGuard wraps the status write ask_parent performs with the
// delegation's own exchange budget, so a question the SubagentRequest
// controller is going to refuse comes back to the child as a legible tool
// result in the same turn — instead of parking it on an answer that is never
// coming, and instead of the delegation being torn down around it.
//
// It ENFORCES nothing, and must not be read as if it did. The budget is
// resolved from spec.mode exactly once, by that controller, which counts it
// down on status.exchangesRemaining and refuses the exchange itself whatever a
// runner does here — this session can patch its own status.parentExchange, so a
// check running inside it could never be the ceiling. What this buys is the
// difference between a refusal the child can act on and a silent park.
//
// It reads a COUNT the controller wrote, never spec.mode: the mode is an
// attack-surface declaration and interpreting it a second time, here, would put
// one decision in two places that can disagree.
//
// Fail-CLOSED on a read failure, and both directions cost something, so the
// choice is deliberate: refusing costs the child a question it might have been
// allowed and it continues with what it has, while admitting one it should not
// have parks it until the parent-reply bound ends the delegation and discards
// everything it has already done.
func AskParentBudgetGuard(
	r client.Reader,
	sess *spiceboxv1alpha1.AgentSession,
	record func(ctx context.Context, question string) (int64, error),
) func(ctx context.Context, question string) (int64, error) {
	return func(ctx context.Context, question string) (int64, error) {
		if sess == nil || sess.Spec.Parent == nil {
			// Nobody delegated to this session, so there is no budget to spend
			// and none to read. Checked here as well as inside
			// OwningSubagentRequest so the reader below is never touched for a
			// session that has no delegation to look one up for.
			return record(ctx, question)
		}
		if r == nil {
			return 0, fmt.Errorf("%w: nothing is wired to read the delegation's remaining budget, so this question is refused rather than left unanswered",
				meta.ErrExchangeBudgetSpent)
		}
		sr, err := spiceboxv1alpha1.OwningSubagentRequest(ctx, r, sess)
		if err != nil {
			return 0, fmt.Errorf("%w: the delegation's remaining budget could not be read, so this question is refused rather than left unanswered: %w",
				meta.ErrExchangeBudgetSpent, err)
		}
		if sr != nil {
			if rem := sr.Status.ExchangesRemaining; rem != nil && *rem <= 0 {
				return 0, meta.ErrExchangeBudgetSpent
			}
		}
		return record(ctx, question)
	}
}
