//go:build e2e

// This file proves the shipped reviewbot demo's own binding shape end to end:
// a verified pull_request webhook binds github_pull_request/write_memory at
// SESSION MINT, with no human and no approval in the loop, and the bound
// grant is what the sibling pr_identity_write_test.go's gate (relwrites.Run
// over relwrites.NewSlotBoundChecker) and pool_write_test.go's write door
// (pools.ForSession + httpsrv's destinationFor) then consult.
//
// pr_identity_write_test.go seeds its slot grant directly
// (grantSlot, in pool_read_test.go) — it is about the GATE, not about how a
// grant comes to exist. This file is about the missing half: a trigger-time
// slot-binding path that a shipped demo needs. The grant itself now comes
// from pipeline.TriggerSlotRequestsFor + (*pipeline.Pipeline).BindTriggerSlots,
// driven directly against the harness's real SpiceDB and a crafted
// pull_request payload — never a hand-written tuple standing in for what the
// pipeline would have written.
//
// The fixture's THIRD AgentClass (05-trigger-class.yaml,
// pr-identity-trigger-e2e) is what makes github_pull_request carry
// slot_grant_write_memory at all: it declares the same
// (resourceType, permission, fillFrom) shape as the shipped demo's own slot
// (examples/reviewbot/manifests/agentclass.yaml), and slot composition is
// global, so this fixture's grant relation and the shipped demo's are the
// SAME composed relation.
package resourcepool_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/relwrites"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/channels/channelsd/pipeline"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/pools"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// triggerFixtureClass is 05-trigger-class.yaml's own AgentClass — a
	// sibling of prIdentityFixtureClass (03-class.yaml), not a variant of it:
	// spec.authz.slots admits one entry per resourceType, so the only way
	// github_pull_request can carry both slot_grant_pin (the sibling's own
	// scenario) and slot_grant_write_memory (this file's) is two classes.
	triggerFixtureClass = "pr-identity-trigger-e2e"

	// triggerSlotPermission is write_memory, deliberately: it is the ONLY
	// slot_grant_* relation pools.ForSession puts in the WRITE half of a
	// session's pools, and it is the permission the shipped reviewbot demo's
	// own slot declares.
	triggerSlotPermission = "write_memory"

	// Two pull requests, so a session bound to one never satisfies a check
	// against the other — the exact shape subtest 4 depends on.
	triggerPR1 = "PR_kwDOtriggerbindingone"
	triggerPR2 = "PR_kwDOtriggerbindingtwo"

	triggerRepoID     = "R_kgDOtriggerbindingrepo"
	triggerAuthorAcct = "U_kgDOtriggerbindingauthor"

	triggerSession1 = "pr-identity-trigger-session-1"
	triggerSession2 = "pr-identity-trigger-session-2"

	triggerBearer1 = "bearer-for-pr-identity-trigger-session-1"
	triggerBearer2 = "bearer-for-pr-identity-trigger-session-2"

	triggerObservationText = "trigger-slot-binding-observation-written-through-the-bound-grant"

	// The interact proof's own session and accounts — independent of the
	// write-path subtests above: it proves the OTHER half of trigger-time
	// authorization (who may speak to the session a pull request opened),
	// not the pool-write gate.
	triggerInteractSession    = "pr-identity-trigger-interact-session"
	triggerInteractAuthorAcct = "U_kgDOtriggerbindinginteractauthor"
)

// triggerGithubChannel builds the minimal input Channel BindTriggerSlots
// needs: a Spec.Kind the chregistry can resolve to the github Kind (whose
// WebhookReceiver implements TriggerSlotProvider). BindTriggerSlots reads
// only ev.Channel.Spec.Kind — see its own doc comment — so nothing else on
// the struct matters here.
func triggerGithubChannel() *spiceboxv1alpha1.Channel {
	return &spiceboxv1alpha1.Channel{
		Spec: spiceboxv1alpha1.ChannelSpec{Kind: "github"},
	}
}

// pullRequestOpenedPayload is the narrow slice of a real GitHub pull_request
// webhook body the github Kind's TriggerSlotInstances reads (pull_request.node_id).
func pullRequestOpenedPayload(number int, nodeID string) []byte {
	return []byte(`{"action":"opened","number":` + strconv.Itoa(number) + `,"repository":{"full_name":"acme/widgets"},` +
		`"pull_request":{"node_id":"` + nodeID + `"}}`)
}

// TestE2E_TriggerSlotBinding_ReviewbotDemoBindsAtDeliveryAndWritesThroughTheGate
// drives the whole chain the shipped reviewbot demo depends on: a verified
// pull_request delivery -> pipeline.TriggerSlotRequestsFor +
// (*pipeline.Pipeline).BindTriggerSlots -> a real slot_grant_write_memory
// tuple -> the relwrites slot-bound gate -> the resource pool's write door ->
// the trigger-owner edge's interact grant.
//
// Each subtest is one claim and each can fail on its own:
//
//   - subtest 1: the grant itself. The pipeline binds the PR the webhook
//     named, with no hand-seeded tuple standing in for it.
//   - subtest 2: the SAME gate pr_identity_write_test.go proves (relwrites.Run
//     over the real slot-bound checker) is satisfied by a TRIGGER-BOUND grant,
//     not only by one seeded directly.
//   - subtest 3: the grant OPENS a resource memory pool for writing, through
//     the production write door (pools.ForSession + httpsrv's destinationFor),
//     not merely a readable relation.
//   - subtest 4: a grant is per-instance. A session bound to PR2 must not
//     reach PR1's pool, refused by name.
//   - subtest 5: the OTHER half of trigger-time authorization — the PR
//     author's own linked platform identity may interact with the session
//     their pull request opened, and a stranger may not. This is a
//     SCHEMA-COMPOSITION proof, not a second end-to-end drive: the owner
//     tuple is seeded directly, in the exact shape
//     (*Reconciler).touchTriggerOwner writes
//     (pkg/controllers/agentsession/owner_resolve.go) — github_user:<id>#user
//     — never through that reconciler itself, which this in-process harness
//     does not run.
func TestE2E_TriggerSlotBinding_ReviewbotDemoBindsAtDeliveryAndWritesThroughTheGate(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-pr-identity-e2e"),
		DefaultTimeout: 60 * time.Second,
	})
	// Same reason pr_identity_write_test.go stamps this: the in-process
	// harness never wires the SpiceboxToolspec controller, so the toolspec
	// coverage check inside AgentClass validation is stamped directly.
	stampToolspecValid(t, h.K8s, prIdentityToolspec)
	h.WaitForAgentClassValid(prIdentityFixtureClass, 60*time.Second)
	h.WaitForAgentClassValid(triggerFixtureClass, 60*time.Second)
	h.WaitForComposedSchema(60*time.Second, []e2e.SchemaRel{
		{Definition: prType, Relation: authz.SlotGrantRelationName(triggerSlotPermission)},
		{Definition: prType, Relation: "author"},
		{Definition: prType, Relation: "repo"},
		{Definition: "github_repo", Relation: "admin"},
	})

	ctx := context.Background()
	ns := "default"

	var ac spiceboxv1alpha1.AgentClass
	require.NoError(t, h.K8s.Get(ctx, client.ObjectKey{Namespace: ns, Name: triggerFixtureClass}, &ac),
		"get the fixture's REAL, reconciled AgentClass — status.resolvedSlots must come from the controller, not a hand-built literal")

	// reqs is deliberately NOT require.Len'd here: Step 6's mutation (remove
	// the slot, or just its fillFrom, from 05-trigger-class.yaml) makes this
	// empty, and an abort at this shared setup point would stop every subtest
	// below from ever running at all — hiding the very reddening Step 6 exists
	// to observe. Each subtest that depends on a bound instance asserts its
	// OWN precondition instead, so a fixture mutation reddens the subtests
	// that actually depend on it (1-3, and 4's own precondition) while leaving
	// subtest 5 — which binds nothing and depends on neither reqs nor the
	// slot — demonstrably unaffected.
	reqs := pipeline.TriggerSlotRequestsFor(ctx, &ac)

	p := &pipeline.Pipeline{Authz: h.SpiceDB}
	ch := triggerGithubChannel()
	expiresAt := time.Now().Add(time.Hour)

	blocks := blocksFromToolspec(t, h, prIdentityToolspec)
	require.Len(t, blocks, 2, "the shared fixture toolspec must carry exactly the author and repo writesRelationships blocks")
	w := &relwrites.SpiceDBWriter{Client: h.SpiceDB.Writer(relwrites.Source)}

	t.Run("a verified pull_request delivery binds the slot with no hand-seeded tuple", func(t *testing.T) {
		require.Len(t, reqs, 1, "the fixture class's slot must be trigger-eligible: fillFrom names trigger explicitly")
		assert.Equal(t, prType, reqs[0].ResourceType)
		assert.Equal(t, triggerSlotPermission, reqs[0].Permission)
		assert.Nil(t, reqs[0].Expr, "no triggerInstance declared: the github Kind's own TriggerSlotProvider supplies the instance")

		sess := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: triggerSession1}}
		ev := channelkinds.InboundEvent{
			Channel:       ch,
			DeliveryEvent: "pull_request",
			RawDelivery:   pullRequestOpenedPayload(1, triggerPR1),
		}
		p.BindTriggerSlots(ctx, sess, ev, reqs, expiresAt)

		assert.True(t,
			checkRelation(t, h, prType, triggerPR1, authz.SlotGrantRelationName(triggerSlotPermission), "agentsession", ns+"/"+triggerSession1),
			"the pipeline must have written %s:%s#%s@agentsession:%s/%s",
			prType, triggerPR1, authz.SlotGrantRelationName(triggerSlotPermission), ns, triggerSession1)
	})

	t.Run("the trigger-bound grant satisfies the SAME slot-bound gate a seeded grant satisfies", func(t *testing.T) {
		checker := relwrites.NewSlotBoundChecker(h.SpiceDB, ns, triggerSession1)
		vars := prViewResultVars(ns, triggerSession1, triggerPR1, triggerAuthorAcct, false, triggerRepoID)

		written, err := relwrites.Run(ctx, w, blocks, vars, checker, prIdentityLogf(t))
		require.NoError(t, err, "a trigger-bound grant must pass the gate exactly as a seeded one does")
		assert.Len(t, written, 2, "both the author and repo tuples must write")
	})

	t.Run("the grant opens the pull request's memory pool for WRITING, through the production write door", func(t *testing.T) {
		got, err := pools.ForSession(ctx, h.SpiceDB.Pools(), ns, triggerSession1)
		require.NoError(t, err)
		prScope := resourceScope(t, prType, triggerPR1)
		assert.Contains(t, got.Write, prScope,
			"a slot_grant_write_memory tuple the pipeline wrote must open a WRITE pool; write pools: %v", got.Write)

		reg := tokens.NewRegistry()
		reg.Set(memory.NamespacedName{Namespace: ns, Name: triggerSession1}, triggerBearer1, "")
		srv := httptest.NewServer(httpsrv.NewHandler(h.Memory(), reg, httpsrv.WithPools(h.SpiceDB.Pools())))
		t.Cleanup(srv.Close)

		entry, err := observation.NewEntry(triggerObservationText, []string{"trigger-slot-binding-e2e"})
		require.NoError(t, err, "observation.NewEntry")
		stored, err := httpclient.New(srv.URL, triggerBearer1).
			PutToPool(ctx, memory.Scope{Kind: "session", ID: ns + "/" + triggerSession1}, prScope, entry)
		require.NoError(t, err, "a session holding the trigger-bound write grant must be allowed to write into the pool")
		assert.Equal(t, prScope, stored.Scope, "the stored entry must carry the resource scope it was addressed to")
	})

	t.Run("a second delivery's PR does not open the first PR's pool: refused by name", func(t *testing.T) {
		sess2 := &spiceboxv1alpha1.AgentSession{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: triggerSession2}}
		ev2 := channelkinds.InboundEvent{
			Channel:       ch,
			DeliveryEvent: "pull_request",
			RawDelivery:   pullRequestOpenedPayload(2, triggerPR2),
		}
		p.BindTriggerSlots(ctx, sess2, ev2, reqs, expiresAt)
		require.True(t,
			checkRelation(t, h, prType, triggerPR2, authz.SlotGrantRelationName(triggerSlotPermission), "agentsession", ns+"/"+triggerSession2),
			"precondition: session 2 must hold its OWN grant, on PR2, or the refusal below proves nothing")

		reg := tokens.NewRegistry()
		reg.Set(memory.NamespacedName{Namespace: ns, Name: triggerSession2}, triggerBearer2, "")
		srv := httptest.NewServer(httpsrv.NewHandler(h.Memory(), reg, httpsrv.WithPools(h.SpiceDB.Pools())))
		t.Cleanup(srv.Close)

		pr1Scope := resourceScope(t, prType, triggerPR1)
		entry, err := observation.NewEntry("trigger-slot-binding-refused-cross-pr-write", nil)
		require.NoError(t, err, "observation.NewEntry")
		status, body := postObservation(t, srv, triggerBearer2, ns, triggerSession2, pr1Scope.ID, entry)

		assert.Equal(t, http.StatusForbidden, status,
			"a session bound only to PR2 must be refused writing into PR1's pool; body: %s", body)
		assert.Contains(t, body, pr1Scope.ID,
			"the refusal must NAME the destination it refused, so an operator can tell which grant is missing")
	})

	t.Run("the composed schema admits the trigger-owner edge to interact: PR author yes, stranger no", func(t *testing.T) {
		aliceID := e2e.CanonicalForFakeEmail("trigger-owner-alice@example.test")
		malloryID := e2e.CanonicalForFakeEmail("trigger-owner-mallory@example.test")

		// Seeded here in the shape (*Reconciler).touchTriggerOwner writes
		// (pkg/controllers/agentsession/owner_resolve.go) —
		// github_user:<id>#user — not driven through that reconciler: this
		// in-process harness does not run it. What is under test is whether
		// the COMPOSED SCHEMA resolves that shape to interact at all
		// (github.Kind.SessionRelationLinks widening owner/participant/denied
		// to admit github_user#user), not the reconciler's own annotation
		// parsing or write path.
		require.NoError(t, h.SpiceDB.TouchOwner(ctx, ns, triggerInteractSession, "github_user:"+triggerInteractAuthorAcct+"#user"),
			"touch agentsession#owner@github_user:%s#user", triggerInteractAuthorAcct)
		require.NoError(t, h.SpiceDB.TouchAttestedIdentity(ctx, "github_user", triggerInteractAuthorAcct, aliceID),
			"touch github_user:%s#user@user:%s", triggerInteractAuthorAcct, aliceID)

		allowed, err := h.SpiceDB.CheckInteract(ctx, ns, triggerInteractSession, aliceID, true)
		require.NoError(t, err, "CheckInteract for the PR author's own linked identity")
		assert.True(t, allowed,
			"the PR author's linked platform identity must interact with the session their pull request opened")

		denied, err := h.SpiceDB.CheckInteract(ctx, ns, triggerInteractSession, malloryID, true)
		require.NoError(t, err, "CheckInteract for a stranger")
		assert.False(t, denied, "a stranger with no attested edge to the PR author's account must not interact")
	})
}
