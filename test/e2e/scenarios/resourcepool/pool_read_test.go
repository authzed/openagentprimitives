//go:build e2e

// Package resourcepool_test proves the resource-memory READ path end to end:
// a session reaches a resource's memory pool because it holds a SLOT GRANT on
// that resource, and for no other reason.
//
// Why a scenario and not a bronzethread bundle. The e2e harness wires the
// runner DIRECTLY to the in-process memory facade
// (test/e2e/inprocess_runner_factory.go's `loop.Mem = sysApprovedMem{...}`);
// the harness wires no httpsrv for a runner at all — the only one under
// test/e2e is the handler this file mounts below, driven by an http.Client and
// never by a runner. Both halves of the mechanism
// under test live ONLY in pkg/memory/httpsrv — mintPoolApprovals turns a
// session's slot grants into per-pool approvals, and handleSearch folds those
// same pools into the scope list — so an in-process search_memory never spans
// a pool. Worse, sysApprovedMem wraps every call in memory.WithSystemApproval,
// which opens the per-scope door for ANY scope named, including a pool the
// session holds no grant on. A bundle written against that would pass with
// the grant-and-expansion path completely broken: the "green test that never
// reaches the code" shape. So this test builds the handler the way
// internal/cmd/operator/main.go builds one — httpsrv.NewHandler with
// WithPools(<client>.Pools()) — over the harness's REAL SpiceDB and the
// harness's REAL memory facade, and drives it with a bearer token, exactly as
// a runner does.
//
// Nothing here mints a system approval and nothing here touches
// sysApprovedMem. The only thing that can open a pool scope is the grant.
//
// Routing the harness's own runner through httpsrv is the right long-term
// fix and is deliberately NOT attempted here: it changes the memory path for
// every existing memory scenario at once.
package resourcepool_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	spicedbv1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	_ "github.com/authzed/openagentprimitives/pkg/memory/kinds/all" // registers the observation Kind this scenario seeds
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/observation"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	fixtureClass = "dossier-pool-e2e"

	// poolType is the fixture resource type (agent-dossier-pool-e2e's
	// MCPServer fragment). slotPermission is what the AgentClass slots, and
	// is deliberately NOT memory.PermissionViewMemory — see the fixture.
	poolType       = "dossier"
	slotPermission = "read"

	// grantedPoolID is the dossier the granted session holds a slot on;
	// unreachedPoolID is one NOBODY holds a slot on. Both pools are seeded
	// with an entry that matches the same search, so "absent" is always
	// distinguishable from "the search found nothing".
	grantedPoolID   = "d-reachable"
	unreachedPoolID = "d-unreachable"

	grantedSession   = "pool-granted-session"
	ungrantedSession = "pool-ungranted-session"

	grantedTok   = "bearer-for-the-granted-session"
	ungrantedTok = "bearer-for-the-ungranted-session"

	// Every seeded observation's text is unique, so an assertion names the
	// entry it means rather than a count.
	textGrantedPool   = "dossier-pool-note-reachable-through-the-slot-grant"
	textUnreachedPool = "dossier-pool-note-no-session-holds-a-slot-on-this-one"
	textGrantedOwn    = "session-scope-note-belonging-to-the-granted-session"
	textUngrantedOwn  = "session-scope-note-belonging-to-the-ungranted-session"
)

// TestE2E_ResourcePoolRead_GrantFoldsThePoolIntoASearch is the whole chain in
// one scenario: fixture schema → slot grant tuple → pools.ForSession →
// mintPoolApprovals → handleSearch's scope expansion → CompositeSearcher's
// per-scope door → the seeded entry.
//
// Each subtest is one claim, and each can fail on its own:
//
//   - "the fixture type composed": the schema half. Reddens if the fragment
//     stopped composing, if the slot stopped being injected, or if the
//     composer ever started owning view_memory (which would silently rewrite
//     the pool's audience).
//   - "a slot grant folds the pool in": the positive. Reddens if the grant is
//     not read back, if the pool is not minted an approval, or if the scope
//     list is not widened — Task 4a's mutation testing showed each of those
//     is separately reachable.
//   - "no grant, no pool": the negative that makes the positive mean
//     something. Its OWN session-scope entry must still come back, so a
//     searcher that broke outright cannot masquerade as a working gate. It
//     also asks for the granted pool BY NAME and is still refused.
//   - "the body cannot name a pool": the reach comes from the GRANT. A
//     granted caller naming the UNREACHED pool in its request body must not
//     get it, and must not lose the pool it did earn.
func TestE2E_ResourcePoolRead_GrantFoldsThePoolIntoASearch(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-dossier-pool-e2e"),
		DefaultTimeout: 60 * time.Second,
	})
	// The fixture's MCPServer pins one tool, and the MCPServer controller
	// probes the live server before reporting Valid. A tool the stub does not
	// offer fails the class as AgentClassMCPServerInvalid/AllowlistDrift — an
	// error several layers from "the scenario forgot to register it". Nothing
	// here ever calls it; it exists so the class validates.
	h.MCP.OnTool("get_dossier", func(_ map[string]any) any { return map[string]any{"id": "d"} })
	h.WaitForAgentClassValid(fixtureClass, 60*time.Second)
	// The slot relation is composed on the guardian's OWN reconcile, which
	// AgentClass Valid=True does not imply. Without this the grant write below
	// can race the composition and fail with a SpiceDB FailedPrecondition.
	h.WaitForComposedSchema(60*time.Second, []e2e.SchemaRel{
		{Definition: poolType, Relation: authz.SlotGrantRelationName(slotPermission)},
	})

	ns := "default"
	grantedPool := resourceScope(t, poolType, grantedPoolID)
	unreachedPool := resourceScope(t, poolType, unreachedPoolID)
	grantedOwn := memory.Scope{Kind: "session", ID: ns + "/" + grantedSession}
	ungrantedOwn := memory.Scope{Kind: "session", ID: ns + "/" + ungrantedSession}

	// Seed BEFORE granting, so nothing about ordering can make the positive
	// pass for the wrong reason.
	seedObservation(t, h, grantedPool, "obs-pool-reachable", textGrantedPool)
	seedObservation(t, h, unreachedPool, "obs-pool-unreachable", textUnreachedPool)
	seedObservation(t, h, grantedOwn, "obs-own-granted", textGrantedOwn)
	seedObservation(t, h, ungrantedOwn, "obs-own-ungranted", textUngrantedOwn)

	// The ONE grant in this test. Written on the composed
	// `slot_grant_read: agentsession with expiration` relation, so the
	// expiry is mandatory — a grant SpiceDB would accept without one is
	// exactly the indefinite authority the schema refuses to express.
	grantSlot(t, h, poolType, grantedPoolID, slotPermission, ns, grantedSession)

	// The handler, built the way internal/cmd/operator/main.go builds it. The
	// registry is this test's own: two per-session bearers, each with an EMPTY
	// callerID, which is what the AgentSession reconciler registers in
	// production (a runner's write has no user creator to record).
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: ns, Name: grantedSession}, grantedTok, "")
	reg.Set(memory.NamespacedName{Namespace: ns, Name: ungrantedSession}, ungrantedTok, "")
	srv := httptest.NewServer(httpsrv.NewHandler(h.Memory(), reg,
		httpsrv.WithPools(h.SpiceDB.Pools())))
	t.Cleanup(srv.Close)

	t.Run("the fixture type composed: dossier carries a slottable read AND an unslotted view_memory", func(t *testing.T) {
		resp, err := h.SpiceDB.ReadSchema(context.Background(), &spicedbv1.ReadSchemaRequest{})
		require.NoError(t, err, "ReadSchema")
		schema := resp.SchemaText

		require.Contains(t, schema, "definition "+poolType,
			"the MCPServer fragment's resource type must be in the composed schema; got:\n%s", schema)

		// The composer OWNS the slot permission line: whatever the fragment
		// wrote is replaced with slot_grant_<perm>->interact + owner.
		assert.Contains(t, schema,
			"permission "+slotPermission+" = "+authz.SlotGrantRelationName(slotPermission)+"->interact + owner",
			"ComposeSlots must own the slotted permission; got:\n%s", schema)

		// And it must NOT own the audience permission. If a future change ever
		// let a slot be composed onto view_memory, this line would become the
		// slot expression and every entry already in every dossier pool would
		// silently change readership. isSlottableMemoryPermission is what
		// refuses it; this is the observable consequence of that refusal.
		assert.Contains(t, schema, "permission "+memory.PermissionViewMemory+" = audience + owner_ref",
			"the pool's audience permission must survive composition verbatim; got:\n%s", schema)
		assert.NotContains(t, schema,
			"permission "+memory.PermissionViewMemory+" = "+authz.SlotGrantRelationName(memory.PermissionViewMemory),
			"view_memory must never be composed as a slot; got:\n%s", schema)
	})

	t.Run("a slot grant folds the resource pool into the session's search", func(t *testing.T) {
		got := searchOK(t, srv, grantedTok, ns, grantedSession, memory.SearchRequest{
			Kinds: []string{observation.KindName},
			Limit: 50,
		})

		// The headline. Nothing in the request named this scope; the only
		// route to it is the slot grant.
		assert.Contains(t, got.texts, textGrantedPool,
			"the granted session must reach the pool it holds a slot_grant_%s on; scopes reached: %v",
			slotPermission, got.scopes)
		assert.Contains(t, got.scopes, grantedPool.Kind+":"+grantedPool.ID,
			"the hit must carry the RESOURCE scope, not a session scope")

		// Its own session scope still answers — so a positive can never be a
		// searcher that returns everything it is handed.
		assert.Contains(t, got.texts, textGrantedOwn,
			"the session's own scope must still be searched")

		// And a grant is per-instance: holding one dossier does not hold another.
		assert.NotContains(t, got.texts, textUnreachedPool,
			"a slot grant on %s:%s must not reach %s:%s", poolType, grantedPoolID, poolType, unreachedPoolID)
	})

	t.Run("no slot grant, no pool: the same request reaches only the session's own scope", func(t *testing.T) {
		got := searchOK(t, srv, ungrantedTok, ns, ungrantedSession, memory.SearchRequest{
			Kinds: []string{observation.KindName},
			Limit: 50,
		})

		// Non-empty first: without this, a searcher that had stopped working
		// would satisfy every NotContains below and read as a working gate.
		require.Contains(t, got.texts, textUngrantedOwn,
			"the ungranted session's OWN scope must still answer — otherwise the absences below prove nothing")

		assert.NotContains(t, got.texts, textGrantedPool,
			"a session holding no slot grant must not reach %s:%s; scopes reached: %v",
			poolType, grantedPoolID, got.scopes)
		assert.NotContains(t, got.texts, textUnreachedPool,
			"a session holding no slot grant must not reach %s:%s", poolType, unreachedPoolID)
		assert.NotContains(t, got.texts, textGrantedOwn,
			"and it must not reach another session's scope either")

		// The same session ASKING for the pool by name, in the one wire field
		// that carries scopes. A body is not a grant: handleSearch forces the
		// scope list from the URL plus the session's own read pools, so this
		// request is byte-for-byte as powerless as the one above.
		//
		// Kept separate from the granted caller's version below because the
		// directions fail differently: there, an honored body would have to
		// get past the per-scope door; here, the caller holds no pool approval
		// at all, so an honored body would either leak the entry or turn an
		// ordinary search into a 403 — and searchOK requires the 200.
		asked := searchOK(t, srv, ungrantedTok, ns, ungrantedSession, memory.SearchRequest{
			Scopes: []memory.Scope{grantedPool},
			Kinds:  []string{observation.KindName},
			Limit:  50,
		})
		assert.NotContains(t, asked.texts, textGrantedPool,
			"naming %s in the body must not reach it without a grant; scopes reached: %v",
			grantedPool.ID, asked.scopes)
		assert.Contains(t, asked.texts, textUngrantedOwn,
			"and naming a scope it may not have must not cost it the scope it may")
	})

	t.Run("the pool arrives through the grant, not the request: a named scope in the body is not honored", func(t *testing.T) {
		// The granted caller ASKS for the unreached pool by name, in the one
		// field of the wire format that carries scopes. handleSearch forces
		// the scope list from the URL plus the session's own read pools, so
		// the body cannot add one.
		//
		// Two ways this could redden, and both are the finding: the entry
		// comes back (the body was honored and the per-scope door did not
		// stop it), or the whole request fails with ErrMissingApproval (the
		// body was honored and the door refused the search outright). Either
		// says a caller can steer which scopes a search spans.
		got := searchOK(t, srv, grantedTok, ns, grantedSession, memory.SearchRequest{
			Scopes: []memory.Scope{unreachedPool},
			Kinds:  []string{observation.KindName},
			Limit:  50,
		})

		assert.NotContains(t, got.texts, textUnreachedPool,
			"naming %s in the request body must not reach it; scopes reached: %v",
			unreachedPool.ID, got.scopes)
		// Unchanged from the plain request: the body neither added a scope nor
		// replaced the ones the grant earned.
		assert.Contains(t, got.texts, textGrantedPool,
			"the granted pool must still be reached; scopes reached: %v", got.scopes)
		assert.Contains(t, got.texts, textGrantedOwn,
			"and the session's own scope must still be reached")
	})
}

// resourceScope builds the fixture's pool scope, failing the test rather than
// returning an error: a scope this test cannot address is a broken fixture,
// not a case to handle.
func resourceScope(t *testing.T, objType, objID string) memory.Scope {
	t.Helper()
	sc, err := memory.ResourceScope(objType, objID)
	require.NoError(t, err, "memory.ResourceScope(%q, %q)", objType, objID)
	return sc
}

// seedObservation writes one observation entry straight into scope through the
// harness's memory facade.
//
// Deliberately NOT through the HTTP handler: the write path is not what this
// scenario is about, and going through a bearer would mean giving one of them
// write reach on a pool, which is the other half of the design (write pools
// need slot_grant_write_memory) and would muddy what the read assertions
// prove. It carries no approval of any kind — an append-only Kind's Put is
// gated by provenance verification, not by a WriteMemory approval, and the
// harness's facade has no verifier wired (it logs and allows), so nothing
// here can be mistaken for opening the door the read assertions test.
func seedObservation(t *testing.T, h *e2e.Harness, scope memory.Scope, id, text string) {
	t.Helper()
	content, err := json.Marshal(observation.Content{Text: text})
	require.NoError(t, err, "marshal observation content")
	_, err = h.Memory().Put(context.Background(), memory.Entry{
		Scope:     scope,
		Kind:      observation.KindName,
		ID:        id,
		CreatedAt: time.Now().UTC(),
		Content:   content,
	})
	require.NoError(t, err, "seed observation %q into %s:%s", id, scope.Kind, scope.ID)
}

// grantSlot writes the one relationship this scenario's positive case depends
// on: <type>:<id>#slot_grant_<perm>@agentsession:<ns>/<name>.
//
// Written directly rather than through the approval flow because the flow is
// not under test here — what is under test is what the memory read path does
// with a grant that exists. The relation name comes from
// authz.SlotGrantRelationName, the same helper the composer emits and
// pools.ForSession reads by prefix, so this tuple cannot be spelled
// differently than the code that consumes it.
//
// The expiry is mandatory: the composed relation is declared
// `with expiration`, and SpiceDB refuses the write without one.
func grantSlot(t *testing.T, h *e2e.Harness, objType, objID, permission, ns, sessName string) {
	t.Helper()
	_, err := h.SpiceDB.Writer(e2e.E2EHarnessSource).WriteRelationships(context.Background(),
		&spicedbv1.WriteRelationshipsRequest{
			Updates: []*spicedbv1.RelationshipUpdate{{
				Operation: spicedbv1.RelationshipUpdate_OPERATION_TOUCH,
				Relationship: &spicedbv1.Relationship{
					Resource: &spicedbv1.ObjectReference{ObjectType: objType, ObjectId: objID},
					Relation: authz.SlotGrantRelationName(permission),
					Subject: &spicedbv1.SubjectReference{
						Object: &spicedbv1.ObjectReference{ObjectType: "agentsession", ObjectId: ns + "/" + sessName},
					},
					OptionalExpiresAt: timestamppb.New(
						authz.SlotGrantExpiry(time.Now(), time.Hour)),
				},
			}},
		})
	require.NoError(t, err, "write slot grant %s:%s#%s@agentsession:%s/%s",
		objType, objID, authz.SlotGrantRelationName(permission), ns, sessName)
}

// searchHits is a search answer reduced to what the assertions read: the
// observation TEXTS that came back, and the scopes they came from.
type searchHits struct {
	texts  []string
	scopes []string
}

// searchOK POSTs req to /memory/_search/{ns}/{name} as token and requires a
// 200, returning the hits.
//
// Requiring 200 is part of the contract each subtest asserts: a refusal is a
// different outcome from an empty result, and collapsing the two would let a
// broken door (every search 403) read as a working one (nothing leaked).
func searchOK(t *testing.T, srv *httptest.Server, token, ns, sess string, req memory.SearchRequest) searchHits {
	t.Helper()
	body, err := json.Marshal(req)
	require.NoError(t, err, "marshal SearchRequest")

	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/memory/_search/"+ns+"/"+sess, bytes.NewReader(body))
	require.NoError(t, err, "build search request")
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(httpReq)
	require.NoError(t, err, "POST _search for %s/%s", ns, sess)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read search response")
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"search for %s/%s must succeed; body: %s", ns, sess, strings.TrimSpace(string(raw)))

	var res memory.MergedSearchResult
	require.NoError(t, json.Unmarshal(raw, &res), "decode MergedSearchResult from %s", raw)

	out := searchHits{}
	for _, se := range res.Entries {
		var c observation.Content
		require.NoError(t, json.Unmarshal(se.Entry.Content, &c),
			"decode observation content of %s", se.Entry.ID)
		out.texts = append(out.texts, c.Text)
		out.scopes = append(out.scopes, fmt.Sprintf("%s:%s", se.Entry.Scope.Kind, se.Entry.Scope.ID))
	}
	return out
}
