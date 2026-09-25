package main

// The pt-tag mint route spent its whole existence DEFINED AND UNREACHABLE:
// httpsrv had the handler and WithPtTagMinter, httpclient had MintPtTag,
// pttagmint had the Minter — and nothing anywhere called the option, so every
// request got 405 and no pt-tag could exist. Every unit test in each of those
// packages passed the entire time.
//
// So the assertion worth having is not "the minter works" (its own package
// covers that) but "a handler built the way the operator builds one actually
// SERVES the route".

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/tokens"
)

// stubPtTagSpiceDB satisfies the two SpiceDB capabilities minting needs. It is
// never exercised here — this file asks whether the route is WIRED, not what it
// computes — so both methods refuse loudly if a future change starts depending
// on them.
type stubPtTagSpiceDB struct{ t *testing.T }

func (s stubPtTagSpiceDB) LookupSubjects(context.Context, string) ([]string, error) {
	s.t.Helper()
	s.t.Fatal("LookupSubjects called: this test asserts wiring, not derivation")
	return nil, nil
}

func (s stubPtTagSpiceDB) WriteRelationships(context.Context, *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	s.t.Helper()
	s.t.Fatal("WriteRelationships called: this test asserts wiring, not derivation")
	return nil, nil
}

type stubMemWriter struct{}

func (stubMemWriter) Put(_ context.Context, e memory.Entry) (memory.Entry, error) { return e, nil }

// TestTheOperatorBuildsAMinterWithBothSpiceDBRolesFromOneClient.
//
// Subjects and Rels must be the SAME client: deriving a reader set from one
// datastore and writing the tuples that enforce it to another would leave a tag
// whose audience and whose authorization describe different things.
func TestTheOperatorBuildsAMinterWithBothSpiceDBRolesFromOneClient(t *testing.T) {
	c := stubPtTagSpiceDB{t: t}
	m := newPtTagMinter(c, stubMemWriter{})

	require.NotNil(t, m)
	assert.NotNil(t, m.Subjects, "the subject lookup that derives the reader set")
	assert.NotNil(t, m.Rels, "the relationship writer that makes it enforceable")
	assert.NotNil(t, m.Mem, "the component memory credential that records it")
	assert.Equal(t, m.Subjects, m.Rels,
		"both roles must come from one client, or a tag's audience and its tuples can disagree")
}

// newMintProbeHandler builds a handler over an in-memory store with one
// per-session token, wired with the given minter (nil = route left off).
//
// The token registry is load-bearing, not scaffolding. Without it the handler
// rejects at 401 BEFORE route dispatch, and since 401 is not 405 the "wired"
// case passes while proving nothing — which is what the first version of this
// test did.
func newMintProbeHandler(t *testing.T, m httpsrv.PtTagMinter) (http.Handler, string) {
	t.Helper()
	mem := memory.NewLocal(inmem.NewBackend())
	reg := tokens.NewRegistry()
	reg.Set(memory.NamespacedName{Namespace: "demo", Name: "demo-session"}, "tok-1", "")
	if m == nil {
		return httpsrv.NewHandler(mem, reg), "tok-1"
	}
	return httpsrv.NewHandler(mem, reg, httpsrv.WithPtTagMinter(m)), "tok-1"
}

// TestTheMintRouteIsSERVEDWhenWiredThatWay is the pin that would have caught
// the original defect.
//
// 405 is the handler's "not enabled" answer, chosen so a deployment that never
// turned per-datum provenance on stays distinguishable from a real refusal.
// That is exactly what every request got while the option had no caller.
// Anything other than 405 means the route is reachable — which is all this
// asserts, deliberately: what it computes belongs to pttagmint's own tests.
func TestTheMintRouteIsSERVEDWhenWiredThatWay(t *testing.T) {
	minter := newPtTagMinter(stubPtTagSpiceDB{t: t}, stubMemWriter{})

	enabled, token := newMintProbeHandler(t, minter)
	disabled, _ := newMintProbeHandler(t, nil)

	for _, tc := range []struct {
		name    string
		h       http.Handler
		want405 bool
	}{
		{"wired the way the operator wires it: the route answers", enabled, false},
		{"not wired at all: 405, the honest not-offered answer", disabled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A body the handler can decode, so a rejection is the ROUTE's
			// answer and not a parse failure that would look the same here.
			req := httptest.NewRequest(http.MethodPost,
				"/memory/_pttag_mint/demo/demo-session", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, req)

			require.NotEqual(t, http.StatusUnauthorized, rec.Code,
				"the probe must authenticate, or 401 masks the answer this test is asking for")

			if tc.want405 {
				assert.Equal(t, http.StatusMethodNotAllowed, rec.Code,
					"an unwired route must say NOT OFFERED, distinguishably from a refusal")
				return
			}
			assert.NotEqual(t, http.StatusMethodNotAllowed, rec.Code,
				"a wired route must not answer 405 — that is the code that meant 'nobody called the option'")
		})
	}
}
