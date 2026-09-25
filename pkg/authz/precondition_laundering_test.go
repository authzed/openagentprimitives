package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/precondition"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/envelopefact"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/factcontent"
)

// The design's co-derivation invariant (spec §3): a fact and the subject it is
// about leave one signed payload together, so a fact ABOUT one instance can
// never be made to answer a precondition ABOUT a different instance. These are
// the two instances that name the boundary.
const (
	launderRepo = "demo-org/demo-repo"
	// PR #5's head commit lives on the base repo — NOT a fork. This is the fact
	// an attacker would try to launder onto #6.
	pr5HeadOID = "05aa05aa05aa"
	// PR #6's head commit lives on a fork — reviewing it means checking out
	// untrusted code, which the gate is written to stop.
	pr6HeadOID = "06bb06bb06bb"

	// The fork gate, verbatim from the spec's §10 scenario. head_is_fork=false
	// yields `!false` == true (Satisfied, clear to act); true yields Refused; an
	// unrecorded fact yields Undetermined. All three are exercised below.
	forkGate = `!facts.envelope.head_is_fork`
)

// recordPRDelivery records ONE signed-delivery observation the way channelsd's
// TriggerFacts does: head_is_fork co-derived onto BOTH the head commit and the
// PR, from a single payload, through the REAL envelopefact.Record. The two
// subjects and the fact leave Record inseparable — which is the whole point, so
// this must go through the real accessor and a real in-memory store, not a fake
// that answers "here are the facts", or the read path this test exists to probe
// is never exercised.
func recordPRDelivery(t *testing.T, mem memory.Memory, memScope memory.Scope, headOID, prNumber string, isFork bool) {
	t.Helper()
	require.NoError(t, envelopefact.Record(systemCtx(), mem, memScope, factcontent.Observation{
		Subjects: []factcontent.Subject{
			{ResourceType: "git_commit", ResourceID: headOID},
			{ResourceType: "github_pr", ResourceID: launderRepo + "#" + prNumber},
		},
		Facts:  map[string]any{"head_is_fork": isFork},
		Source: factcontent.Source{ChannelKind: "github", Event: "pull_request"},
	}))
}

// checkoutCheck models `git checkout <sha>`: the head commit OID is the raw
// resource id, keyed under git_commit. fetchCheck models `git fetch origin
// pull/<n>/head`: the PR is the raw resource id, keyed under github_pr. Neither
// declares transforms — ExplainPreconditionDenial looks facts up by the RAW id
// regardless (RULING P-2), and these are already the raw provider identifiers.
var (
	checkoutCheck = authz.PermissionCheck{ResourceType: "git_commit", Permission: "checkout", ResourceIDTemplate: "{sha}"}
	fetchCheck    = authz.PermissionCheck{ResourceType: "github_pr", Permission: "fetch", ResourceIDTemplate: "{repo}#{number}"}
)

func checkoutArgs(headOID string) map[string]any { return map[string]any{"sha": headOID} }
func fetchArgs(prNumber string) map[string]any {
	return map[string]any{"repo": launderRepo, "number": prNumber}
}

// TestPrecondition_Laundering_AFactAboutPR5CannotClearPR6 is THE laundering test
// the spec names (§10): the one that fails if a §3 co-derivation regression ever
// lets a fact about one instance answer a precondition about another.
//
// It works at the fact/precondition layer, over ExplainPreconditionDenial — the
// dispatch-time recomputation that decides WHY a denied call was refused. That
// function reads the gated instance's facts BY SUBJECT (candidateFacts →
// factcontent.ForSubject) and returns:
//
//   - nil          when every gate is Satisfied (the fact cleared the slot);
//   - {Undetermined} when no fact about the instance has been recorded;
//   - {Refused}    when the instance's own fact decided against it.
//
// So a nil return is "the gate opened", and a non-nil return is "the call is
// DENIED, here is why". Asserting on that — never on a reply — is what makes this
// a test about the boundary rather than about a transcript.
//
// The mutation this catches: drop the resourceID half of the lookup key in
// factcontent.ForSubject (the query FieldEquals AND the Go-side c.ResourceID
// filter) so any fact of the same TYPE matches. #5's not-a-fork fact then
// answers #6's git_commit lookup, #6's checkout goes Satisfied, the denial goes
// nil, and Part 1's require.NotNil fails — the leak becomes reachable and the
// test says so.
func TestPrecondition_Laundering_AFactAboutPR5CannotClearPR6(t *testing.T) {
	slots := []authz.BoundEntitySpec{
		gatedSlot("git_commit", "checkout", mustCompile(t, forkGate)),
		gatedSlot("github_pr", "fetch", mustCompile(t, forkGate)),
	}
	// ExplainPreconditionDenial mints its own system approval for the fact read,
	// exactly as the runner's PreToolCall ctx does; a bare Background is the
	// realistic call shape.
	explain := func(mem memory.Memory, sc memory.Scope, check authz.PermissionCheck, args map[string]any) *authz.PreconditionDenial {
		return authz.ExplainPreconditionDenial(context.Background(), mem, sc, slots, check, args)
	}

	t.Run("pr5's not-a-fork fact clears pr5 but leaves pr6 undetermined — both pr6 acts DENY", func(t *testing.T) {
		mem := memory.NewLocal(inmem.NewBackend())
		memScope := memory.Scope{Kind: "session", ID: "ns/" + t.Name()}
		// Only #5 has been observed, and #5's head is NOT a fork.
		recordPRDelivery(t, mem, memScope, pr5HeadOID, "5", false)

		// Control — the gate DOES open for the instance the fact is actually
		// about. A nil denial here proves head_is_fork=false was found under
		// #5's own subjects and Satisfied the gate. Without it, the #6 denials
		// below could pass merely because the gate is closed for everyone, which
		// is a broken gate, not an enforced boundary.
		assert.Nil(t, explain(mem, memScope, checkoutCheck, checkoutArgs(pr5HeadOID)),
			"#5's own head commit carries head_is_fork=false, so its checkout gate is Satisfied and adds no denial")
		assert.Nil(t, explain(mem, memScope, fetchCheck, fetchArgs("5")),
			"#5's own PR carries head_is_fork=false, so its fetch gate is Satisfied and adds no denial")

		// The laundering itself. #6's head is a fork, and NOTHING has been
		// observed about #6. #5's not-a-fork fact is keyed to #5's subjects, so
		// #6's lookup finds nothing — Undetermined, never Satisfied — and the
		// call is refused. A nil here would BE the §3 regression.
		checkoutDenial := explain(mem, memScope, checkoutCheck, checkoutArgs(pr6HeadOID))
		require.NotNil(t, checkoutDenial,
			"#5's fact must not clear #6's checkout — a nil denial here is the laundering the gate exists to prevent")
		assert.Equal(t, precondition.Undetermined, checkoutDenial.Verdict,
			"nothing is observed about #6, so its gate is Undetermined — #5's fact did not leak in as a Satisfied")
		assert.False(t, checkoutDenial.Unevaluatable,
			"the store read fine; this is a genuine Undetermined verdict, not an unread gate")

		fetchDenial := explain(mem, memScope, fetchCheck, fetchArgs("6"))
		require.NotNil(t, fetchDenial,
			"#5's fact must not clear #6's fetch either — the boundary holds on both subjects of the co-derived observation")
		assert.Equal(t, precondition.Undetermined, fetchDenial.Verdict,
			"PR #6 has no recorded fact, so its fetch gate is Undetermined, not Satisfied by PR #5's")
		assert.False(t, fetchDenial.Unevaluatable, "the store read fine here too")
	})

	t.Run("pr6 is decided by its OWN fork fact, unchanged by pr5's not-a-fork fact", func(t *testing.T) {
		// A store where BOTH deliveries have arrived: #5 not-a-fork, #6 a fork.
		both := memory.NewLocal(inmem.NewBackend())
		bothScope := memory.Scope{Kind: "session", ID: "ns/" + t.Name() + "/both"}
		recordPRDelivery(t, both, bothScope, pr5HeadOID, "5", false)
		recordPRDelivery(t, both, bothScope, pr6HeadOID, "6", true)

		d := explain(both, bothScope, checkoutCheck, checkoutArgs(pr6HeadOID))
		require.NotNil(t, d, "#6's own fork fact must refuse its checkout")
		assert.Equal(t, precondition.Refused, d.Verdict,
			"#6's head lives on a fork; its OWN head_is_fork=true fact decides Refused")

		// Independence, asserted directly: recompute #6 in a store that holds
		// ONLY #6's fork fact — no #5 fact anywhere — and require the identical
		// verdict. If #5's not-a-fork fact had softened #6's answer, these two
		// verdicts would differ. They must not.
		only6 := memory.NewLocal(inmem.NewBackend())
		only6Scope := memory.Scope{Kind: "session", ID: "ns/" + t.Name() + "/only6"}
		recordPRDelivery(t, only6, only6Scope, pr6HeadOID, "6", true)

		dOnly6 := explain(only6, only6Scope, checkoutCheck, checkoutArgs(pr6HeadOID))
		require.NotNil(t, dOnly6, "#6's fork fact refuses its checkout with or without #5 in the store")
		assert.Equal(t, d.Verdict, dOnly6.Verdict,
			"#6's verdict is Refused whether or not #5's not-a-fork fact is present — the two subjects are independent")

		// And the boundary cuts both ways: #6's fork fact must not close #5's
		// gate. In the both-recorded store, #5's own checkout is still Satisfied.
		assert.Nil(t, explain(both, bothScope, checkoutCheck, checkoutArgs(pr5HeadOID)),
			"#6's fork fact must not leak onto #5; #5's own head is not a fork and its gate stays Satisfied")
	})
}
