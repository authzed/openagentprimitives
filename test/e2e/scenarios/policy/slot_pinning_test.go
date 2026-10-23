//go:build e2e

package policy_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestSlotPinning proves the single-occupancy slot-pinning mechanism end to end
// against the harness's REAL SpiceDB — the layer a scripted LLM session cannot
// reach. It drives authz.GrantSlots / authz.BindApproved and the
// SlotPinner.EnsurePin exactly as the runner wires them (the SpiceDB-backed
// *spicedb.RelationWriter, via h.SpiceDB.Relations), so the pin tuple, the
// refusal of a second instance, and the atomic A→B move with its grant
// revocation are asserted at the tuple level rather than inferred from a reply.
//
// This is the deterministic companion to the slot-pin-move-on-amendment bronze
// bundle (which proves the move rendering on a real published card through a
// full LLM-driven plan gate). Here the writer is called directly, below the
// approver-standing gate, so the code_repo fixture's standing is irrelevant and
// left session-only;
// what is under test is GrantSlots' occupancy/rebind handling and the
// SlotPinner's atomic preconditions.
//
// No real names: code_repo / repo-alpha / repo-bravo are fictional.
func TestSlotPinning(t *testing.T) {
	manifests, err := os.ReadFile(filepath.Join(e2e.TestdataDir("slot_pinning"), "manifests.yaml"))
	require.NoError(t, err, "read manifests")

	h := e2e.Start(t, e2e.Options{
		ExtraManifests: []string{string(manifests)},
		DefaultTimeout: 30 * time.Second,
	})

	// The MCPServer's one tool must be probeable or the AgentClass never reaches
	// Valid and the guardian never composes the schema this test writes against.
	h.MCP.OnTool("fetch", func(map[string]any) any {
		return map[string]any{"ok": true}
	})

	// The AgentClass declaring code_repo as a slot is what makes the guardian
	// compose `relation slot_pin: agentsession` (single-occupancy) and
	// `relation slot_grant_read: agentsession` (the read grant) onto code_repo.
	h.WaitForAgentClassValid("ac-slot-pinning", 30*time.Second)
	h.WaitForComposedSchema(30*time.Second, []e2e.SchemaRel{
		{Definition: "code_repo", Relation: "slot_pin"},
		{Definition: "code_repo", Relation: "slot_grant_read"},
	})

	const (
		repoA = "repo-alpha"
		repoB = "repo-bravo"
	)
	// Far-future expiry: the schema requires one on every slot grant, and these
	// assertions read fully-consistently moments later, so nothing must lapse.
	expiry := authz.SlotGrantExpiry(time.Now(), time.Hour)

	// A single-occupancy, rebind=approval binding for one instance of code_repo.
	bindingFor := func(id, rebind string) authz.SlotBinding {
		return authz.SlotBinding{
			ResourceType: "code_repo",
			ResourceID:   authz.TrustedObjectID(id),
			Permission:   "read",
			Occupancy:    authz.SlotOccupancySingle,
			Rebind:       rebind,
		}
	}

	// ── bind A, refuse B, approve the move to B ──────────────────────────────
	t.Run("binds the first instance, refuses a second, and an approved amendment moves the pin", func(t *testing.T) {
		writer := h.SpiceDB.Relations()
		_, ok := writer.(authz.SlotPinner)
		require.True(t, ok, "the SpiceDB RelationWriter must implement authz.SlotPinner (GrantSlots refuses single-occupancy otherwise)")

		sess := authz.SessionRef{Namespace: "default", Name: "pin-move"}
		subj := identity.Subject("agentsession:" + sess.String())
		ctx := memory.WithSystemApproval(context.Background(), "test")

		// Bind A: a first-fill. The pin lands and the read grant is written.
		require.NoError(t, authz.GrantSlots(ctx, writer, sess, []authz.SlotBinding{bindingFor(repoA, "approval")}, expiry),
			"binding the first instance of a single-occupancy slot must succeed")
		h.AssertSpiceDB("code_repo:"+repoA, "slot_pin", subj, true)
		h.AssertSpiceDB("code_repo:"+repoA, "slot_grant_read", subj, true)

		// Attempt B WITHOUT an approved move: a different instance of a filled
		// single-occupancy slot is refused, and the refusal names the pinned
		// instance and the route out (the text the model is handed).
		err := authz.GrantSlots(ctx, writer, sess, []authz.SlotBinding{bindingFor(repoB, "approval")}, expiry)
		require.Error(t, err, "a second distinct instance of a single-occupancy slot must be refused")
		require.True(t, errors.Is(err, authz.ErrSlotPinned), "want ErrSlotPinned, got %v", err)
		assert.Contains(t, err.Error(), "code_repo:"+repoA, "the refusal must name the pinned instance")
		assert.Contains(t, err.Error(), "propose an updated plan", "rebind=approval routes to a plan amendment")
		// Nothing moved: B is neither pinned nor granted.
		h.AssertSpiceDB("code_repo:"+repoB, "slot_pin", subj, false)
		h.AssertSpiceDB("code_repo:"+repoB, "slot_grant_read", subj, false)

		// Approve the move: BindApproved with PriorID=A repoints the pin A→B and
		// revokes A's grants in the same request — the human-approved amendment.
		mem := memory.NewLocal(inmem.NewBackend())
		scope := memory.Scope{Kind: "session", ID: sess.String()}
		move := bindingFor(repoB, "approval")
		move.PriorID = authz.TrustedObjectID(repoA)
		require.NoError(t, authz.BindApproved(ctx, mem, scope, sess, writer,
			[]authz.SlotBinding{move}, authz.PreconditionsWaived, expiry, logr.Discard(), time.Now),
			"an approved move of a single-occupancy slot must succeed")

		// The pin is now on B, A's is gone; B's read grant is written and A's is
		// revoked — the grant permission is false for A, true for B.
		h.AssertSpiceDB("code_repo:"+repoB, "slot_pin", subj, true)
		h.AssertSpiceDB("code_repo:"+repoA, "slot_pin", subj, false)
		h.AssertSpiceDB("code_repo:"+repoB, "slot_grant_read", subj, true)
		h.AssertSpiceDB("code_repo:"+repoA, "slot_grant_read", subj, false)
	})

	// ── rebind: never — the refusal routes to a NEW SESSION, nothing moves ───
	t.Run("rebind never refuses a second instance and names a new session, moving nothing", func(t *testing.T) {
		writer := h.SpiceDB.Relations()
		sess := authz.SessionRef{Namespace: "default", Name: "pin-never"}
		subj := identity.Subject("agentsession:" + sess.String())
		ctx := memory.WithSystemApproval(context.Background(), "test")

		require.NoError(t, authz.GrantSlots(ctx, writer, sess, []authz.SlotBinding{bindingFor(repoA, authz.SlotRebindNever)}, expiry),
			"binding the first instance must succeed")
		h.AssertSpiceDB("code_repo:"+repoA, "slot_pin", subj, true)

		err := authz.GrantSlots(ctx, writer, sess, []authz.SlotBinding{bindingFor(repoB, authz.SlotRebindNever)}, expiry)
		require.Error(t, err, "rebind=never must refuse a second instance")
		require.True(t, errors.Is(err, authz.ErrSlotPinned), "want ErrSlotPinned, got %v", err)
		assert.Contains(t, err.Error(), "code_repo:"+repoA, "the refusal must name the pinned instance")
		assert.Contains(t, err.Error(), "start a new session", "rebind=never routes to a new session, not an amendment")
		assert.NotContains(t, err.Error(), "an approved plan moves the pin", "rebind=never must not offer the move route")

		// Nothing moved: A is still pinned, B is not.
		h.AssertSpiceDB("code_repo:"+repoA, "slot_pin", subj, true)
		h.AssertSpiceDB("code_repo:"+repoB, "slot_pin", subj, false)
	})

	// ── concurrent first bind: exactly one winner, atomically ────────────────
	//
	// Two goroutines race EnsurePin for DIFFERENT instances through the real
	// writer. SpiceDB evaluates each write's MUST_NOT_MATCH precondition and the
	// write atomically, so exactly one reaches the server first and wins the
	// slot; the other is refused by the server (not by a read-then-write race in
	// this process) and reads back the winner's id. The same-instance
	// fall-through EnsurePin also exercises (held=true, pinnedID==resourceID) is
	// the stated proxy for "same instance after expiry": a fake cannot model the
	// TTL, but the pin branch is identical.
	t.Run("two concurrent first binds for different instances leave exactly one pin", func(t *testing.T) {
		writer := h.SpiceDB.Relations()
		pinner, ok := writer.(authz.SlotPinner)
		require.True(t, ok, "the SpiceDB RelationWriter must implement authz.SlotPinner")

		sess := authz.SessionRef{Namespace: "default", Name: "pin-concurrent"}
		subj := identity.Subject("agentsession:" + sess.String())
		ctx := context.Background()

		type result struct {
			id       string
			held     bool
			pinnedID string
			err      error
		}
		results := make([]result, 2)
		ids := []string{repoA, repoB}
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start // line both calls up as closely as the scheduler allows
				held, pinnedID, err := pinner.EnsurePin(ctx, "code_repo", ids[i], sess)
				results[i] = result{id: ids[i], held: held, pinnedID: pinnedID, err: err}
			}(i)
		}
		close(start)
		wg.Wait()

		for _, r := range results {
			require.NoError(t, r.err, "EnsurePin for %s must not error (a refusal is held=true, not an error)", r.id)
		}

		var winners, losers []result
		for _, r := range results {
			if r.held {
				losers = append(losers, r)
			} else {
				winners = append(winners, r)
			}
		}
		require.Len(t, winners, 1, "exactly one first bind wins the slot")
		require.Len(t, losers, 1, "the other is refused by the server, not errored")
		assert.Equal(t, winners[0].id, losers[0].pinnedID,
			"the loser must read back the WINNER's instance as the one holding the pin")

		// Exactly one pin exists in SpiceDB: the winner's, not the loser's.
		h.AssertSpiceDB("code_repo:"+winners[0].id, "slot_pin", subj, true)
		h.AssertSpiceDB("code_repo:"+losers[0].id, "slot_pin", subj, false)
	})
}
