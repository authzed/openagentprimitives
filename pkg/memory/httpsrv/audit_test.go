package httpsrv_test

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/httpclient"
	"github.com/authzed/openagentprimitives/pkg/memory/httpsrv"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/pttagcontent"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/stretchr/testify/require"
)

func TestAdministrativeAuditExportVerifiesHiddenChainEntries(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	signer := provenance.NewSigner(key, "system:operator")
	keys := provenance.MapKeyLookup{{Publisher: signer.Publisher(), KeyID: signer.KeyID()}: key.Public().(ed25519.PublicKey)}
	mem := memory.NewLocal(inmem.NewBackend(), memory.WithProvenanceVerifier(provenance.NewWriteVerifier(keys)))
	signed := provenance.NewSigningMemory(mem, signer)
	scope := memory.Scope{Kind: "session", ID: "ns/session"}
	for i, kind := range []string{lifecycle.Kind{}.Name(), pttagcontent.KindName, lifecycle.Kind{}.Name()} {
		_, err := signed.Put(ctx, memory.Entry{Scope: scope, Kind: kind, ID: []string{"lifecycle-first", "ptc-hidden", "lifecycle-last"}[i], CreatedAt: time.Now().UTC(), Content: json.RawMessage(`{"content":"private audit evidence"}`)})
		require.NoError(t, err)
	}
	// The ordinary session read is still filtered, which made the old CLI report
	// a false gap. Administrative export must include the hidden middle link.
	sessionCtx := memory.WithTokenSession(ctx, memory.NamespacedName{Namespace: "ns", Name: "session"})
	filtered, err := mem.Query(sessionCtx, memory.Query{Scope: scope})
	require.NoError(t, err)
	require.Len(t, filtered.Entries, 2)
	require.NotEmpty(t, provenance.NewVerifier(keys).VerifyChain(signer.Publisher(), filtered.Entries, nil).Findings)
	srv := httptest.NewServer(httpsrv.NewAuditHandler(mem, "admin-secret"))
	defer srv.Close()
	result, err := httpclient.New(srv.URL, "admin-secret").QueryAudit(context.Background(), scope)
	require.NoError(t, err)
	require.Len(t, result.Entries, 3)
	report := provenance.NewVerifier(keys).VerifyChain(signer.Publisher(), result.Entries, nil)
	require.Empty(t, report.Findings)
	require.Equal(t, 3, report.OK)
	for _, e := range result.Entries {
		require.Equal(t, scope, e.Scope)
	}
}

func TestAuditExportRejectsNonAdministrativeRequests(t *testing.T) {
	mem := memory.NewLocal(inmem.NewBackend())
	for _, tc := range []struct {
		name, token, method, path, header string
		status                            int
	}{
		{"missing", "admin", "GET", "/audit/ns/session", "", 401},
		{"session credential", "admin", "GET", "/audit/ns/session", "Bearer session", 401},
		{"component credential", "admin", "GET", "/audit/ns/session", "Bearer channelsd", 401},
		{"empty configured credential", "", "GET", "/audit/ns/session", "Bearer admin", 401},
		{"bare token", "admin", "GET", "/audit/ns/session", "admin", 401},
		{"write", "admin", "POST", "/audit/ns/session", "Bearer admin", 405},
		{"filters", "admin", "GET", "/audit/ns/session?kind=lifecycle", "Bearer admin", 400},
		{"extra scope segment", "admin", "GET", "/audit/ns/session/other", "Bearer admin", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", tc.header)
			w := httptest.NewRecorder()
			httpsrv.NewAuditHandler(mem, tc.token).ServeHTTP(w, req)
			require.Equal(t, tc.status, w.Code, w.Body.String())
		})
	}
}
