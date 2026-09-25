//go:build e2e

// This file is the WRITE half of the resource-memory pool scenario; see
// pool_read_test.go's package comment for why both halves are scenarios
// against the real handler rather than bronzethread bundles. The reason is
// unchanged here and, for writes, even more load-bearing: the e2e in-process
// harness wires the runner's `sess.Mem` to a plain *memory.Local
// (test/e2e/inprocess_runner_factory.go), and *memory.Local does not implement
// memory.PoolWriter. record_observation type-asserts that interface out of
// sess.Mem, so inside the harness the assertion fails and the tool refuses
// EVERY call with "this session's memory backend does not support writing into
// a resource pool" — a refusal that has nothing to do with the grant being
// tested. A bundle written against that would pass while proving nothing, or
// fail for the wrong reason.
//
// What IS reachable is the gate itself: httpsrv's destinationFor, over the
// harness's real SpiceDB, holding a real slot grant. That gate binds every
// writer — the runner reaches it through httpclient.Client.PutToPool, and so
// does this test.
package resourcepool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/hooks"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/infoleakagetaint"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	// writeFixtureClass is the SECOND AgentClass in the same fixture. It
	// exists only to declare `dossier/write_memory` in spec.authz.slots:
	// spec.authz.slots admits one entry per resourceType, and slot composition
	// is global, so two classes are the only way one type carries both
	// slot_grant_read and slot_grant_write_memory. See 03-agent.yaml.
	writeFixtureClass = "dossier-pool-write-e2e"

	// writeSlotPermission is the permission pools.ForSession treats specially:
	// ANY slot_grant_* makes a resource a READ pool, and only
	// slot_grant_write_memory makes it a WRITE pool.
	writeSlotPermission = "write_memory"

	// Three dossiers, three different standings for ONE session. A fixture
	// with only "granted" and "ungranted" cannot tell "a write grant
	// authorizes a write" apart from "any grant does" — writableID and
	// readOnlyID are what separate them.
	writableID  = "d-writable"  // held under slot_grant_write_memory
	readOnlyID  = "d-readable"  // held under slot_grant_read ONLY
	ungrantedID = "d-ungranted" // held under nothing, by anyone

	writerSession  = "pool-writer-session"
	noGrantSession = "pool-no-grant-session"

	writerTok  = "bearer-for-the-pool-writer-session"
	noGrantTok = "bearer-for-the-no-grant-session"

	// Every text is unique, so an assertion names the entry it means rather
	// than counting rows.
	textWritten        = "pool-write-observation-that-a-write-grant-let-through"
	textReadOnlyPool   = "pool-write-seed-in-the-pool-this-session-may-only-read"
	textWriterOwn      = "pool-write-seed-in-the-writer-session-own-scope"
	textNoGrantOwn     = "pool-write-seed-in-the-no-grant-session-own-scope"
	textRefusedNoGrant = "pool-write-observation-refused-for-holding-no-grant"
	textRefusedReadOnl = "pool-write-observation-refused-for-holding-only-a-read-grant"
	textRefusedPerInst = "pool-write-observation-refused-on-a-dossier-this-session-does-not-hold"
)

// TestE2E_ResourcePoolWrite_GrantIsTheOnlyRouteIn is the write chain end to
// end: fixture schema → slot-grant tuple → pools.ForSession's Read/Write split
// → httpsrv's destinationFor → the entry landing (or not landing) in the pool.
//
// Each subtest is one claim and each fails on its own:
//
//   - "a write grant is the route in": the positive. The entry must land in
//     the POOL and not in the session's own scope — a silent downgrade to the
//     session scope answers 201 and looks like success, which is the specific
//     failure destinationFor exists to prevent.
//   - "no grant on the destination": the negative that makes the positive mean
//     something, and it checks BOTH places the bytes could have gone.
//   - "a read grant alone does not authorize a write": the one that earns its
//     keep. The pool is proved reachable for READ first, so the refusal is
//     demonstrably about direction and not about reach.
//   - "not mode-sensitive": the independence claim. The information-leakage
//     audience gate, in disabled mode, allows the exact call the door refuses.
func TestE2E_ResourcePoolWrite_GrantIsTheOnlyRouteIn(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-dossier-pool-e2e"),
		DefaultTimeout: 60 * time.Second,
	})
	// The fixture's MCPServer pins one tool and the controller probes the live
	// server before reporting Valid; nothing here ever calls it. See the read
	// scenario for the failure mode this avoids.
	h.MCP.OnTool("get_dossier", func(_ map[string]any) any { return map[string]any{"id": "d"} })
	h.WaitForAgentClassValid(fixtureClass, 60*time.Second)
	h.WaitForAgentClassValid(writeFixtureClass, 60*time.Second)
	// BOTH relations, and the write one is the new half. Waited on explicitly
	// because the guardian composes on its OWN reconcile, which Valid=True does
	// not imply — without this the grant writes below race composition and fail
	// with a SpiceDB FailedPrecondition.
	h.WaitForComposedSchema(60*time.Second, []e2e.SchemaRel{
		{Definition: poolType, Relation: authz.SlotGrantRelationName(slotPermission)},
		{Definition: poolType, Relation: authz.SlotGrantRelationName(writeSlotPermission)},
	})

	ns := "default"
	writablePool := resourceScope(t, poolType, writableID)
	readOnlyPool := resourceScope(t, poolType, readOnlyID)
	ungrantedPool := resourceScope(t, poolType, ungrantedID)
	writerOwn := memory.Scope{Kind: "session", ID: ns + "/" + writerSession}
	noGrantOwn := memory.Scope{Kind: "session", ID: ns + "/" + noGrantSession}

	// writablePool is deliberately NOT seeded: it starts empty, so the readback
	// in the first subtest can only be answered by the write this test made.
	// Every other scope IS seeded, so each "the refused text is absent" below
	// is paired with something that must still be present — otherwise a reader
	// that had stopped working would satisfy every absence and read as a
	// working gate.
	seedObservation(t, h, readOnlyPool, "obs-pool-readonly", textReadOnlyPool)
	seedObservation(t, h, writerOwn, "obs-own-writer", textWriterOwn)
	seedObservation(t, h, noGrantOwn, "obs-own-no-grant", textNoGrantOwn)

	// The two grants this test depends on, both held by the SAME session and
	// both on the SAME resource type. Nothing but the relation name
	// distinguishes them, which is exactly the distinction pools.ForSession
	// makes — so a test that passes here cannot be passing because of a type
	// difference, a token difference, or a session difference.
	grantSlot(t, h, poolType, writableID, writeSlotPermission, ns, writerSession)
	grantSlot(t, h, poolType, readOnlyID, slotPermission, ns, writerSession)
	// noGrantSession is granted nothing, anywhere. Nobody is granted anything
	// on ungrantedPool.

	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: ns, Name: writerSession}, writerTok, "")
	reg.Set(memory.NamespacedName{Namespace: ns, Name: noGrantSession}, noGrantTok, "")
	srv := httptest.NewServer(httpsrv.NewHandler(h.Memory(), reg,
		httpsrv.WithPools(h.SpiceDB.Pools())))
	t.Cleanup(srv.Close)

	t.Run("a write grant is the route in: the observation lands in the pool, not in the session's own scope", func(t *testing.T) {
		// Through httpclient.Client.PutToPool — the SAME call
		// record_observation makes, building the same
		// POST /memory/observation/{ns}/{sess}?pool={type}:{id} request. Going
		// through the production client rather than a hand-built request is
		// what makes this a proof about the path the runner takes.
		entry, err := observation.NewEntry(textWritten, []string{"pool-write-e2e"})
		require.NoError(t, err, "observation.NewEntry")

		stored, err := httpclient.New(srv.URL, writerTok).
			PutToPool(context.Background(), writerOwn, writablePool, entry)
		require.NoError(t, err, "PutToPool into the pool this session holds slot_grant_%s on", writeSlotPermission)

		// The destination is SERVER-decided: putEntry forces e.Scope from
		// whatever destinationFor proved, never from the body. The returned
		// scope is therefore the server's answer to "where did this land",
		// which is the thing worth asserting.
		assert.Equal(t, writablePool, stored.Scope,
			"the stored entry must carry the RESOURCE scope it was addressed to")
		assert.Equal(t, entry.ID, stored.ID,
			"the server must store the id the caller signed, not one of its own")

		// It is readable back OUT of the pool, by the same grant. This is the
		// half a status code cannot prove: a 201 for a write that went nowhere
		// looks identical.
		pool := listOK(t, srv, writerTok, ns, writerSession, writablePool.ID)
		assert.Equal(t, []string{textWritten}, pool.texts,
			"the pool must hold exactly the observation this test wrote; it was seeded with nothing")
		for _, e := range pool.entries {
			assert.Equal(t, writablePool, e.Scope,
				"every entry read back from the pool must carry the pool's scope")
		}

		// And it did NOT also land in the session's own scope. A silent
		// downgrade is the failure destinationFor was built to prevent, and it
		// would answer 201 all the same.
		own := listOK(t, srv, writerTok, ns, writerSession, "")
		require.Contains(t, own.texts, textWriterOwn,
			"the session's own scope must still answer — otherwise the absence below proves nothing")
		assert.NotContains(t, own.texts, textWritten,
			"a pool write must not also appear in the writing session's own scope")
	})

	t.Run("no grant on the destination: refused by name, and nothing is written in either place", func(t *testing.T) {
		// Depends on the subtest above having written textWritten into the
		// pool: that entry is this subtest's "still present" anchor, so the
		// "refused text absent" assertion cannot be satisfied by a reader that
		// stopped returning anything.
		before := listOK(t, srv, writerTok, ns, writerSession, writablePool.ID)
		require.Equal(t, []string{textWritten}, before.texts,
			"precondition: the granted write must already be in the pool")

		entry, err := observation.NewEntry(textRefusedNoGrant, nil)
		require.NoError(t, err, "observation.NewEntry")
		status, body := postObservation(t, srv, noGrantTok, ns, noGrantSession, writablePool.ID, entry)

		assert.Equal(t, http.StatusForbidden, status,
			"a session holding no grant on %s must be refused; body: %s", writablePool.ID, body)
		assert.Contains(t, body, writablePool.ID,
			"the refusal must NAME the destination it refused, so an operator can tell which grant is missing")

		// Half one of "nothing was written": not in the refused session's own
		// scope. This is the silent-downgrade check — a handler that ignored an
		// unprovable ?pool= would have written here and answered 201.
		own := listOK(t, srv, noGrantTok, ns, noGrantSession, "")
		assert.Equal(t, []string{textNoGrantOwn}, own.texts,
			"the refused session's own scope must hold only its seed — a refused pool write must not be downgraded into it")

		// Half two: not in the destination either. Read through the session
		// that legitimately holds it, since nobody else can.
		after := listOK(t, srv, writerTok, ns, writerSession, writablePool.ID)
		assert.Equal(t, []string{textWritten}, after.texts,
			"the destination pool must be byte-for-byte what it was before the refused write")
	})

	t.Run("a read grant alone does not authorize a write: the same session reaches the pool to read and is refused to write", func(t *testing.T) {
		// Prove the pool IS reachable for this session first. pools.ForSession
		// puts ANY slot_grant_* in Read, so a slot_grant_read makes this a read
		// pool and _search folds it in — exactly what the read scenario proves.
		// Establishing that here is what makes the refusal below about
		// DIRECTION rather than about reach.
		reachable := searchOK(t, srv, writerTok, ns, writerSession, memory.SearchRequest{
			Kinds: []string{observation.KindName},
			Limit: 50,
		})
		require.Contains(t, reachable.texts, textReadOnlyPool,
			"precondition: a slot_grant_%s must make %s a READ pool; scopes reached: %v",
			slotPermission, readOnlyPool.ID, reachable.scopes)

		entry, err := observation.NewEntry(textRefusedReadOnl, nil)
		require.NoError(t, err, "observation.NewEntry")
		status, body := postObservation(t, srv, writerTok, ns, writerSession, readOnlyPool.ID, entry)

		assert.Equal(t, http.StatusForbidden, status,
			"a slot_grant_%s is not a write grant; body: %s", slotPermission, body)
		assert.Contains(t, body, readOnlyPool.ID,
			"the refusal must name the destination it refused")

		// Nothing landed: not in the pool (which this session can read), and
		// not downgraded into its own scope.
		after := searchOK(t, srv, writerTok, ns, writerSession, memory.SearchRequest{
			Kinds: []string{observation.KindName},
			Limit: 50,
		})
		assert.Contains(t, after.texts, textReadOnlyPool,
			"the read-only pool must still answer — otherwise the absence below proves nothing")
		assert.NotContains(t, after.texts, textRefusedReadOnl,
			"a refused write must not reach %s", readOnlyPool.ID)
		own := listOK(t, srv, writerTok, ns, writerSession, "")
		assert.NotContains(t, own.texts, textRefusedReadOnl,
			"and it must not be downgraded into the writing session's own scope either")

		// The proof is not relaxed for a READ of the same pool. destinationFor
		// checks p.Write on every method, because a pool this session may only
		// read is already reachable through _search (which this subtest just
		// showed) and one gate that cannot disagree with itself is worth more
		// than a second, looser one for listings. The hint is how a caller is
		// told where to go instead.
		getStatus, getBody := getObservations(t, srv, writerTok, ns, writerSession, readOnlyPool.ID)
		assert.Equal(t, http.StatusForbidden, getStatus,
			"?pool= on a GET is proved against write grants too; body: %s", getBody)
		assert.Contains(t, getBody, "_search",
			"a read refused for want of a WRITE grant must name the route that would serve it")
	})

	t.Run("not mode-sensitive: the information-leakage gate, disabled, allows the very write the door refuses", func(t *testing.T) {
		// The two gates are separate enforcement points. The information-leakage
		// audience gate is a RUNNER-PIPELINE hook (pkg/authz/hooks.InfoLeakAudience
		// at pipeline.PreToolCall); it does not run inside httpsrv and never sees
		// this HTTP request. The claim under test is that switching it off does
		// not open the door — so this subtest asks the hook itself, in both
		// modes, about the exact call the handler is then given.
		//
		// The hook is built with the SAME write declaration the runner wires
		// (Loop.memoryPoolWritesDecl: DestinationArg "resource" for
		// record_observation), so its pool-write leg is genuinely live rather
		// than inert.
		args, err := json.Marshal(map[string]any{
			"resource": ungrantedPool.ID,
			"text":     textRefusedPerInst,
		})
		require.NoError(t, err, "marshal tool args")
		call := pipeline.Input{
			Point: pipeline.PreToolCall,
			Tool: &pipeline.ToolCallInfo{
				Name: meta.RecordObservationToolName,
				Args: args,
			},
		}
		deps := func(mode string) hooks.InfoLeakAudienceDeps {
			return hooks.InfoLeakAudienceDeps{
				Mode: mode,
				LookupWrites: func(name string) *hooks.ToolWritesDecl {
					if name != meta.RecordObservationToolName {
						return nil
					}
					return &hooks.ToolWritesDecl{DestinationArg: "resource"}
				},
				// A taint read this gate cannot answer is one of its refusals,
				// and it is the one reachable without standing up an audience
				// resolver. It is used only to show the leg is WIRED — that the
				// hook's verdict on this call is mode-dependent — so that the
				// allow below is attributable to the mode and not to a gate that
				// was inert all along.
				TaintList: func(context.Context) ([]infoleakagetaint.TaintRecord, error) {
					return nil, errors.New("e2e: taint list deliberately unavailable")
				},
			}
		}

		require.Equal(t, pipeline.Deny,
			hooks.NewInfoLeakAudience(deps("enforcing")).Eval(context.Background(), call).Verdict,
			"precondition: the leakage gate's pool-write leg must be live for this call — "+
				"otherwise 'disabled allows it' says nothing")
		require.Equal(t, pipeline.Allow,
			hooks.NewInfoLeakAudience(deps("disabled")).Eval(context.Background(), call).Verdict,
			"disabled mode must allow the call — that is what makes the door the only thing left")

		// And the door refuses it anyway. Note the writer session, which holds
		// a write grant on ANOTHER dossier: a grant is per-instance, so holding
		// one pool is not holding the next.
		entry, err := observation.NewEntry(textRefusedPerInst, nil)
		require.NoError(t, err, "observation.NewEntry")
		status, body := postObservation(t, srv, writerTok, ns, writerSession, ungrantedPool.ID, entry)
		assert.Equal(t, http.StatusForbidden, status,
			"the slot gate reads no mode and refuses %s regardless; body: %s", ungrantedPool.ID, body)
		assert.Contains(t, body, ungrantedPool.ID,
			"the refusal must name the destination it refused")

		// ungrantedPool is readable by nobody, so the reachable half of
		// "nothing was written" is the session scope — which is also the half
		// that matters, since a downgrade is the only way a refused write
		// becomes a stored one.
		own := listOK(t, srv, writerTok, ns, writerSession, "")
		assert.NotContains(t, own.texts, textRefusedPerInst,
			"a refused write must not be downgraded into the writing session's own scope")
	})
}

// poolPath builds the keyed-route URL, naming poolID as the destination when
// it is non-empty. It is the same shape httpclient.Client.PutToPool builds, so
// a refusal this test observes is a refusal a runner would observe.
func poolPath(srv *httptest.Server, ns, sess, poolID string) string {
	u := fmt.Sprintf("%s/memory/%s/%s/%s", srv.URL, observation.KindName, ns, sess)
	if poolID != "" {
		u += "?pool=" + poolID
	}
	return u
}

// postObservation POSTs e to the keyed route and returns the wire status and
// body, WITHOUT requiring either.
//
// Deliberately not a require-200 helper like searchOK: every refusal this file
// asserts is a status, and a helper that failed the test on a non-2xx could
// not express them. The trade is that a caller must assert the status itself,
// which each one does.
func postObservation(t *testing.T, srv *httptest.Server, token, ns, sess, poolID string, e memory.Entry) (int, string) {
	t.Helper()
	body, err := json.Marshal(e)
	require.NoError(t, err, "marshal Entry")

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		poolPath(srv, ns, sess, poolID), bytes.NewReader(body))
	require.NoError(t, err, "build put request")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	require.NoError(t, err, "POST %s/%s pool=%q", ns, sess, poolID)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read put response")
	return resp.StatusCode, strings.TrimSpace(string(raw))
}

// getObservations GETs the keyed route and returns the wire status and body,
// without requiring either — the read counterpart of postObservation, used
// where a refusal is the expected answer.
func getObservations(t *testing.T, srv *httptest.Server, token, ns, sess, poolID string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
		poolPath(srv, ns, sess, poolID), nil)
	require.NoError(t, err, "build list request")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := srv.Client().Do(req)
	require.NoError(t, err, "GET %s/%s pool=%q", ns, sess, poolID)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read list response")
	return resp.StatusCode, strings.TrimSpace(string(raw))
}

// listHits is a listing reduced to what the assertions read.
type listHits struct {
	texts   []string
	entries []memory.Entry
}

// listOK GETs the keyed route and REQUIRES a 200, returning the entries.
//
// Requiring the 200 is part of the contract: "the pool is unchanged" and "the
// pool cannot be read at all" are different outcomes, and collapsing them
// would let a door that refuses everything read as one that refuses the right
// thing.
func listOK(t *testing.T, srv *httptest.Server, token, ns, sess, poolID string) listHits {
	t.Helper()
	status, raw := getObservations(t, srv, token, ns, sess, poolID)
	require.Equal(t, http.StatusOK, status,
		"list for %s/%s pool=%q must succeed; body: %s", ns, sess, poolID, raw)

	var res memory.QueryResult
	require.NoError(t, json.Unmarshal([]byte(raw), &res), "decode QueryResult from %s", raw)

	out := listHits{entries: res.Entries}
	for _, e := range res.Entries {
		var c observation.Content
		require.NoError(t, json.Unmarshal(e.Content, &c), "decode observation content of %s", e.ID)
		out.texts = append(out.texts, c.Text)
	}
	return out
}
