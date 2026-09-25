//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/label"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
	"github.com/authzed/openagentprimitives/test/e2e"
)

// The two scopes the search spans. Neither needs an AgentSession CR: SpiceDB
// object ids are strings, and what this test is about is the authorization
// lattice, not the K8s objects a live session would also have.
const (
	searchAuthzReadableSession = "mem-search-readable"
	searchAuthzHiddenSession   = "mem-search-hidden"

	searchAuthzReadableEntryID = "label-reader-may-see-this"
	searchAuthzHiddenEntryID   = "label-reader-may-not-see-this"
)

// TestHarnessSearcherFiltersByAuthorization is the test that makes the e2e
// harness's searcher worth having.
//
// e2e.Start wires memory.Local with a CompositeSearcher, and that searcher
// post-filters its ranking through a SpiceDB Authorizer. Wiring the searcher
// WITHOUT the authorizer is the tempting shortcut — it is one fewer option, and
// every scenario in the suite goes on passing — so this is the test that
// refuses it: with the authorizer dropped, a caller with standing on ONE of two
// scopes gets entries from BOTH and the assertion below fails by name. Any
// later scenario about which memory a session may reach would otherwise pass
// with the whole filter deleted, which is this repo's most expensive recurring
// failure.
//
// It drives the facade rather than an agent on purpose, and the reason is
// worth stating because it is the same fact the next scenario author needs:
// the per-entry filter is CALLER-gated (CompositeSearcher consults the
// Authorizer only when memory.CallerFrom(ctx) is set), and an AGENT's search
// never carries a caller on either path — the AgentSession reconciler registers
// every per-session memory token with an empty callerID
// (pkg/controllers/agentsession/controller.go's Tokens.Set), precisely so a
// runner's writes never mint a memory_entry#creator tuple for a subject that is
// not a user. So this filter is what gates a USER-attributed read (webd, `oap
// memory search`); what gates an agent's search is the per-scope approval door
// inside CompositeSearcher.Search. Driving a bundle here would exercise neither
// claim.
func TestHarnessSearcherFiltersByAuthorization(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: "testdata/agent-centerdot-companies"})

	// The MCP stub must serve the fixture's tools before validity is awaited,
	// or the MCPServer flips Valid=False/AllowlistDrift and the class follows
	// it down. Waiting on the class is how this test gets the composed SpiceDB
	// schema — WaitForAgentClassValid waits on WaitForAuthzSchema — which
	// memory_entry#read resolves through.
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	ns := "default"
	readableScope := memory.Scope{Kind: "session", ID: ns + "/" + searchAuthzReadableSession}
	hiddenScope := memory.Scope{Kind: "session", ID: ns + "/" + searchAuthzHiddenSession}

	// Three distinct identities, because a two-identity fixture cannot tell
	// "read follows the session" from "read follows the creator": the SCRIBE
	// authors both entries, so if creator conferred read the hidden entry would
	// survive the filter and this test would say so.
	reader := e2e.CanonicalForFakeEmail("reader@fixture.invalid")
	bystander := e2e.CanonicalForFakeEmail("bystander@fixture.invalid")
	scribe := e2e.CanonicalForFakeEmail("scribe@fixture.invalid")

	relCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// memory_entry#read resolves as session->read_transcript, so standing on
	// the SESSION is the whole difference between the two entries. The hidden
	// session gets a participant too — an empty session would make "filtered
	// out" indistinguishable from "nobody can read it because nothing is
	// wired".
	require.NoError(t, h.SpiceDB.TouchInteractParticipantUser(relCtx, ns, searchAuthzReadableSession, reader),
		"grant the reader standing on the readable session")
	require.NoError(t, h.SpiceDB.TouchInteractParticipantUser(relCtx, ns, searchAuthzHiddenSession, bystander),
		"grant someone ELSE standing on the hidden session")

	readable := memory.Entry{
		Scope: readableScope,
		Kind:  label.KindName,
		ID:    searchAuthzReadableEntryID,
		Content: json.RawMessage(
			`{"resourceType":"crm_company","resourceId":"acme","label":"the note the reader may see"}`),
	}
	hidden := memory.Entry{
		Scope: hiddenScope,
		Kind:  label.KindName,
		ID:    searchAuthzHiddenEntryID,
		Content: json.RawMessage(
			`{"resourceType":"crm_company","resourceId":"zenith","label":"the note the reader may not see"}`),
	}

	// A system approval stands in for the operator httpsrv's per-request
	// capability mint — the same shortcut sysApprovedMem takes for every
	// in-process component in this harness. It opens the per-SCOPE door inside
	// CompositeSearcher.Search for both scopes, which is exactly what makes
	// this a test of the per-ENTRY filter and not of the door.
	sysCtx := memory.WithSystemApproval(relCtx, "e2e-search-authorizer-test")

	mem := h.Memory()
	_, err := mem.Put(sysCtx, readable)
	require.NoError(t, err, "put the readable entry")
	_, err = mem.Put(sysCtx, hidden)
	require.NoError(t, err, "put the hidden entry")

	// The harness facade is wired with a searcher but NOT with
	// memory.WithAuthorizer, so Local.Put writes no memory_entry tuples of its
	// own. Write them the way production writes them — through the very
	// Authorizer the operator hands the facade — rather than by hand through
	// e2e.WriteRel, which cannot write these relations at all (spicedbauthorizer
	// claims memory_entry#session and #creator as its relsource, and an
	// unclaimed writer is refused).
	authorizer := spicedbauthorizer.New(h.SpiceDB)
	scribeCtx := memory.WithCaller(sysCtx, scribe.String())
	require.NoError(t, authorizer.AuthorizePut(scribeCtx, readable), "authorize-put the readable entry")
	require.NoError(t, authorizer.AuthorizePut(scribeCtx, hidden), "authorize-put the hidden entry")

	req := memory.SearchRequest{
		Scopes: []memory.Scope{readableScope, hiddenScope},
		Kinds:  []string{label.KindName},
		Limit:  20,
	}

	// Control FIRST: with no caller the filter does not run, so both entries
	// must come back. Without this, a searcher that silently returned nothing
	// would satisfy every claim below.
	unfiltered, err := mem.Search(sysCtx, req)
	require.NoError(t, err, "search with no caller")
	assert.ElementsMatch(t,
		[]string{searchAuthzReadableEntryID, searchAuthzHiddenEntryID},
		searchAuthzEntryIDs(unfiltered),
		"both entries are in the index and both scopes are searchable, so a callerless search sees both")

	// Now the same search, attributed to a user with standing on ONE scope.
	// Polled because AuthorizeQuery checks at MinimizeLatency consistency, so
	// a relationship written moments ago can be a quantization interval away
	// from visible. The loop converges on the ALLOW half; the DENY half is
	// asserted against whatever that same read returned, so a stale snapshot
	// can never be what makes the denial look right.
	callerCtx := memory.WithCaller(sysCtx, reader.String())
	var got []string
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, serr := mem.Search(callerCtx, req)
		require.NoError(t, serr, "search as the reader")
		got = searchAuthzEntryIDs(res)
		if len(got) == 1 && got[0] == searchAuthzReadableEntryID {
			break
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	assert.Contains(t, got, searchAuthzReadableEntryID,
		"the entry whose session the caller participates in must survive the authorizer's post-filter")
	assert.NotContains(t, got, searchAuthzHiddenEntryID,
		"the entry whose session the caller has NO standing on must be filtered out; "+
			"seeing it means the harness's CompositeSearcher has no authorizer (test/e2e/harness.go) "+
			"and every scope handed to a search is readable")
}

func searchAuthzEntryIDs(res memory.MergedSearchResult) []string {
	out := make([]string, 0, len(res.Entries))
	for _, se := range res.Entries {
		out = append(out, se.Entry.ID)
	}
	return out
}
