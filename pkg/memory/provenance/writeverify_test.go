package provenance_test

import (
	"context"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// fakeKeys is a PublisherKeyLookup over a (publisher,keyID)→key map.
type fakeKeys map[[2]string]ed25519.PublicKey

func (k fakeKeys) PublisherKey(publisher, keyID string) (ed25519.PublicKey, bool) {
	pub, ok := k[[2]string{publisher, keyID}]
	return pub, ok
}

// signedAudit signs e with signer and returns the attested entry.
func signedAudit(t *testing.T, signer *provenance.Signer, e memory.Entry) memory.Entry {
	t.Helper()
	require.NoError(t, signer.Sign(&e), "sign audit entry")
	return e
}

// sessionToken returns a context marked as arriving on the per-session bearer
// token issued for ns/name — what httpsrv.ServeHTTP attaches for every runner
// request. Used instead of a hand-built caller string because a session token's
// caller is ALWAYS empty in production (the AgentSession reconciler registers it
// with an empty callerID), so a case that supplies one tests a caller class the
// HTTP path cannot produce.
func sessionToken(ns, name string) context.Context {
	return memory.WithTokenSession(context.Background(), memory.NamespacedName{Namespace: ns, Name: name})
}

func TestWriteVerifier_VerifyEntry(t *testing.T) {
	const publisher = "session:ns/sess"
	priv := testKey(42)
	signer := provenance.NewSigner(priv, publisher)
	keyID := signer.KeyID()

	// The FOREIGN publisher's key is registered too. That is the point of the
	// binding cases below: their entries are cryptographically impeccable, and
	// the only thing wrong with them is who presented them.
	const foreignPublisher = "session:ns/other"
	foreignPriv := testKey(43)
	foreignSigner := provenance.NewSigner(foreignPriv, foreignPublisher)

	keys := fakeKeys{
		{publisher, keyID}:                          pub(priv),
		{foreignPublisher, foreignSigner.KeyID()}:   pub(foreignPriv),
		{"system:channelsd", foreignSigner.KeyID()}: pub(foreignPriv),
	}

	// A baseline signed, valid entry the cases derive from.
	base := signedAudit(t, signer, auditEntry("ta-1", `{"outcome":"allowed"}`))
	foreign := signedAudit(t, foreignSigner, auditEntry("ta-2", `{"outcome":"allowed"}`))

	cases := []struct {
		name    string
		ctx     context.Context
		caller  string
		entry   func() memory.Entry
		wantErr error // nil means VerifyEntry must succeed
	}{
		{
			name:   "system token, caller matches publisher: accepted",
			ctx:    context.Background(),
			caller: publisher,
			entry:  func() memory.Entry { return base },
		},
		{
			name:   "in-process write, no caller and no token: accepted",
			ctx:    context.Background(),
			caller: "",
			entry:  func() memory.Entry { return base },
		},
		{
			name:    "missing provenance: ErrProvenanceRequired",
			ctx:     context.Background(),
			caller:  publisher,
			entry:   func() memory.Entry { e := base; e.Provenance = nil; return e },
			wantErr: memory.ErrProvenanceRequired,
		},
		{
			name:    "system token, caller != publisher: ErrBadProvenance",
			ctx:     context.Background(),
			caller:  "system:channelsd",
			entry:   func() memory.Entry { return base },
			wantErr: memory.ErrBadProvenance,
		},
		{
			name:  "session token authoring its own publisher: accepted",
			ctx:   sessionToken("ns", "sess"),
			entry: func() memory.Entry { return base },
		},
		{
			name:    "session token authoring another session's registered publisher: ErrBadProvenance",
			ctx:     sessionToken("ns", "sess"),
			entry:   func() memory.Entry { return foreign },
			wantErr: memory.ErrBadProvenance,
		},
		{
			name: "session token authoring a component publisher it can sign for: ErrBadProvenance",
			ctx:  sessionToken("ns", "sess"),
			entry: func() memory.Entry {
				return signedAudit(t, provenance.NewSigner(foreignPriv, "system:channelsd"), auditEntry("ta-3", `{"outcome":"allowed"}`))
			},
			wantErr: memory.ErrBadProvenance,
		},
		{
			name:    "token session present but unnamed (wiring bug): ErrBadProvenance, no fall-through",
			ctx:     sessionToken("", ""),
			entry:   func() memory.Entry { return base },
			wantErr: memory.ErrBadProvenance,
		},
		{
			name:   "unknown key: ErrBadProvenance",
			ctx:    context.Background(),
			caller: publisher,
			entry: func() memory.Entry {
				e := base
				p := *e.Provenance
				p.KeyID = "feedface" // not in the lookup
				e.Provenance = &p
				return e
			},
			wantErr: memory.ErrBadProvenance,
		},
		{
			name:   "tampered content after signing: ErrBadProvenance",
			ctx:    context.Background(),
			caller: publisher,
			entry: func() memory.Entry {
				e := base
				e.Content = []byte(`{"outcome":"denied"}`) // digest no longer matches Sig
				return e
			},
			wantErr: memory.ErrBadProvenance,
		},
	}

	wv := provenance.NewWriteVerifier(keys)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := wv.VerifyEntry(tc.ctx, tc.caller, tc.entry())
			if tc.wantErr == nil {
				assert.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestWriteVerifier_ImplementsProvenanceVerifier(t *testing.T) {
	var _ memory.ProvenanceVerifier = (*provenance.WriteVerifier)(nil)
}
