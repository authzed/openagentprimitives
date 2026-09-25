package auditcmd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/auditkey"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
	"github.com/authzed/openagentprimitives/pkg/memory/publisherkeys"
)

// witnessEntry signs an audit_key record attesting pub for sessionUID, as the
// operator does when it mints a session's signing key.
func witnessEntry(t *testing.T, signer *provenance.Signer, sessionUID string, pub ed25519.PublicKey) memory.Entry {
	t.Helper()
	content, err := json.Marshal(auditkey.Content{
		SessionUID: sessionUID,
		KeyID:      provenance.KeyID(pub),
		PubKey:     base64.StdEncoding.EncodeToString(pub),
	})
	require.NoError(t, err)
	e := memory.Entry{
		Scope:     auditTestScope,
		Kind:      auditkey.KindName,
		ID:        auditkey.IDPrefix + provenance.KeyID(pub),
		CreatedAt: auditFixedTime,
		Content:   content,
	}
	require.NoError(t, signer.Sign(&e))
	return e
}

// TestWitnessedSessionKeys_TrustsAKeyTheOperatorAttested is the trust chain that
// keeps a retried session verifiable.
//
// A session's audit key lives in a Secret owned by its AgentSession, and its
// public half is anchored on that CR's status. Delete the CR and both are gone —
// but the append-only records the key signed are PERMANENT and stay in a scope
// keyed by namespace/name, which the next session of that name inherits. Without
// a durable witness, every entry the deleted instance signed reads as
// "unknown-key" and `oap audit verify` fails on the survivor.
//
// The witness is an operator-signed record in the scope itself, and the
// operator's key is in the publisher-keys ConfigMap, which outlives any session.
func TestWitnessedSessionKeys_TrustsAKeyTheOperatorAttested(t *testing.T) {
	opSigner, opPub := testSigner(t, 9, "system:operator")
	_, retiredPub := testSigner(t, 1, provenance.SessionPublisher("ns", "sess"))

	reg := publisherkeys.New()
	require.NoError(t, reg.Add("system:operator", provenance.KeyID(opPub), opPub),
		"the operator's own key comes from the durable ConfigMap")

	sessionPublisher := provenance.SessionPublisher("ns", "sess")
	entries := []memory.Entry{witnessEntry(t, opSigner, "uid-attempt-1", retiredPub)}

	addWitnessedSessionKeys(reg, sessionPublisher, entries, io.Discard)

	got, ok := reg.PublisherKey(sessionPublisher, provenance.KeyID(retiredPub))
	require.True(t, ok, "a retired session key the operator witnessed stays trusted")
	assert.Equal(t, retiredPub, got)
}

// TestWitnessedSessionKeys_RefusesAnUntrustedWitness keeps the door closed: the
// witness only means anything because a key already in the trusted set signed
// it. A record signed by an unregistered publisher — anything the verifier
// cannot vouch for — registers nothing.
func TestWitnessedSessionKeys_RefusesAnUntrustedWitness(t *testing.T) {
	impostor, _ := testSigner(t, 7, "system:operator") // never registered
	_, forgedPub := testSigner(t, 2, provenance.SessionPublisher("ns", "sess"))

	reg := publisherkeys.New()
	sessionPublisher := provenance.SessionPublisher("ns", "sess")

	addWitnessedSessionKeys(reg, sessionPublisher,
		[]memory.Entry{witnessEntry(t, impostor, "uid-attempt-1", forgedPub)}, io.Discard)

	_, ok := reg.PublisherKey(sessionPublisher, provenance.KeyID(forgedPub))
	assert.False(t, ok, "an unattestable witness must not install a trusted key")
}

// TestWitnessedSessionKeys_RefusesASelfAttestedWitness holds the invariant the
// audit_key Kind states outright: a session may never author its own key
// binding, because a self-attested key proves nothing. The witness means
// something only because a COMPONENT — whose key lives in the durable
// publisher-keys ConfigMap and outlives every session — put its name to it.
//
// In the cluster that invariant is a write-door rule (auditkey's WriteAuthority
// is ComponentWritten, so the facade refuses a session-token write of the
// Kind). `oap audit verify` is an OFFLINE reader of a store it is being asked
// to distrust, which is the whole point of running it, so it cannot inherit
// that door — it has to say the same thing itself.
//
// The registry the loop consults is also the registry it writes into, so
// without this the door is open exactly once: the first witness must be
// component-attested, and each one after it may be signed by a key an earlier
// witness in the same loop just installed. Refusing the session's own publisher
// is what closes that, because the session publisher is the ONLY one this
// function ever registers a key for.
func TestWitnessedSessionKeys_RefusesASelfAttestedWitness(t *testing.T) {
	sessionPublisher := provenance.SessionPublisher("ns", "sess")
	liveSigner, livePub := testSigner(t, 3, sessionPublisher)
	_, mintedPub := testSigner(t, 4, sessionPublisher)

	reg := publisherkeys.New()
	require.NoError(t, reg.Add(sessionPublisher, provenance.KeyID(livePub), livePub),
		"the live session key is anchored on the AgentSession's own status, so the verifier holds it")

	// Signed by a key the registry ALREADY trusts, so nothing but the publisher
	// rule stands between this record and a new trusted key.
	var warnings bytes.Buffer
	addWitnessedSessionKeys(reg, sessionPublisher,
		[]memory.Entry{witnessEntry(t, liveSigner, "uid-attempt-2", mintedPub)}, &warnings)

	_, ok := reg.PublisherKey(sessionPublisher, provenance.KeyID(mintedPub))
	assert.False(t, ok, "a session attesting its own next key must install nothing")
	assert.Contains(t, warnings.String(), "self-attested",
		"and must say so: a record dropped from the trust set without a word leaves the "+
			"reader with unknown-key findings and nothing explaining them")
}

// TestWitnessedSessionKeys_ASelfAttestedWitnessCannotBootstrapAnother is the
// chain the guard above exists to stop, stated end to end: a first witness
// signed under the session's own publisher mints a key, and a second witness
// signed by THAT key mints another. Neither may land — one refused witness that
// still widened the trust set would make every later one attestable.
func TestWitnessedSessionKeys_ASelfAttestedWitnessCannotBootstrapAnother(t *testing.T) {
	sessionPublisher := provenance.SessionPublisher("ns", "sess")
	liveSigner, livePub := testSigner(t, 5, sessionPublisher)
	mintedSigner, mintedPub := testSigner(t, 6, sessionPublisher)
	_, secondPub := testSigner(t, 8, sessionPublisher)

	reg := publisherkeys.New()
	require.NoError(t, reg.Add(sessionPublisher, provenance.KeyID(livePub), livePub))

	addWitnessedSessionKeys(reg, sessionPublisher, []memory.Entry{
		witnessEntry(t, liveSigner, "uid-attempt-2", mintedPub),
		witnessEntry(t, mintedSigner, "uid-attempt-3", secondPub),
	}, io.Discard)

	_, first := reg.PublisherKey(sessionPublisher, provenance.KeyID(mintedPub))
	assert.False(t, first, "the self-attested witness installs nothing")
	_, second := reg.PublisherKey(sessionPublisher, provenance.KeyID(secondPub))
	assert.False(t, second, "so the witness that depended on it has nothing to be attested by")
}

// TestBuildAuditReport_ChainSurvivesAKeyRotation is invariant #4 stated
// directly: after a delete-and-redeliver the scope holds one continuous chain
// whose first entries are signed by the deleted instance's key and whose tail is
// signed by the live one. With both keys trusted the whole chain verifies clean
// — no unknown-key, no gap, no fork — and `oap audit verify` exits 0.
func TestBuildAuditReport_ChainSurvivesAKeyRotation(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "test")
	publisher := provenance.SessionPublisher("ns", "sess")
	retiredSigner, retiredPub := testSigner(t, 1, publisher)
	liveSigner, livePub := testSigner(t, 2, publisher)

	m := memory.NewLocal(inmem.NewBackend())
	sign := func(signer *provenance.Signer, id string) memory.Entry {
		t.Helper()
		e := memory.Entry{
			Scope: auditTestScope, Kind: lifecycle.KindName, ID: "lifecycle-" + id,
			CreatedAt: auditFixedTime, Content: json.RawMessage(`{}`),
		}
		require.NoError(t, signer.Sign(&e))
		stored, err := m.Put(ctx, e)
		require.NoError(t, err)
		return stored
	}

	// The failed attempt's entries, then the retry continuing the SAME chain:
	// a fresh Signer seeds itself from the tail already in the scope, exactly as
	// SigningMemory does on a session's first append.
	entries := []memory.Entry{sign(retiredSigner, "a1"), sign(retiredSigner, "a2")}
	require.NoError(t, liveSigner.SeedFromMemory(ctx, m, auditTestScope))
	entries = append(entries, sign(liveSigner, "b1"), sign(liveSigner, "b2"))

	keys := provenance.MapKeyLookup{
		{Publisher: publisher, KeyID: provenance.KeyID(retiredPub)}: retiredPub,
		{Publisher: publisher, KeyID: provenance.KeyID(livePub)}:    livePub,
	}
	reports := buildAuditReport(provenance.NewVerifier(keys), entries, nil)

	require.Len(t, reports, 1)
	assert.Empty(t, reports[0].Findings,
		"a chain that continues across a key rotation verifies clean under both keys")
	assert.Equal(t, 4, reports[0].OK)
	assert.Zero(t, hardFindingCount(reports), "no hard finding, so the command exits 0")
}
