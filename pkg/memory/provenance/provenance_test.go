package provenance_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/inmem"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// --- test fixtures -------------------------------------------------------

// testKey returns a deterministic Ed25519 private key seeded by a single
// repeated byte, so tests reference stable keys and key IDs.
func testKey(seed byte) ed25519.PrivateKey {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return ed25519.NewKeyFromSeed(s)
}

func pub(priv ed25519.PrivateKey) ed25519.PublicKey {
	return priv.Public().(ed25519.PublicKey)
}

// auditKind is a registered append-only Kind used by the chain/seed
// tests; mutKind is its mutable sibling for the pass-through test.
type auditKind struct{}

func (auditKind) Name() string                { return "test_audit" }
func (auditKind) IDPrefix() string            { return "ta-" }
func (auditKind) Retention() memory.Retention { return memory.Retention{AppendOnly: true} }
func (auditKind) ContentSchema() reflect.Type { return nil }
func (auditKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (auditKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (auditKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }

type mutKind struct{}

func (mutKind) Name() string                { return "test_mutable" }
func (mutKind) IDPrefix() string            { return "tm-" }
func (mutKind) Retention() memory.Retention { return memory.Retention{} }
func (mutKind) ContentSchema() reflect.Type { return nil }
func (mutKind) IndexedFields() []string     { return nil }

// WriteAuthority: a stand-in for an ordinary agent-authored kind; the
// per-kind write door is exercised against the real registry in
// pkg/memory/httpsrv and pkg/memory/kinds/all.
func (mutKind) WriteAuthority() memory.WriteAuthority        { return memory.SessionWritten }
func (mutKind) NewScopeHooks(memory.Scope) memory.ScopeHooks { return noopHooks{} }

type noopHooks struct{}

func (noopHooks) OnSignal(context.Context, memory.Signal) error { return nil }

// registerKinds installs the test Kinds and tears them down after the
// test. The registry is process-global, so tests touching it must not
// run in parallel with each other.
func registerKinds(t *testing.T) {
	t.Helper()
	memory.ResetRegistryForTest()
	memory.RegisterKind(auditKind{})
	memory.RegisterKind(mutKind{})
	t.Cleanup(memory.ResetRegistryForTest)
}

var fixedTime = time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

func auditEntry(id string, content string) memory.Entry {
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/sess"},
		Kind:      "test_audit",
		ID:        id,
		CreatedAt: fixedTime,
		Tags:      []string{"b", "a"},
		Content:   json.RawMessage(content),
	}
}

// --- tests ---------------------------------------------------------------

func TestSessionPublisher(t *testing.T) {
	assert.Equal(t, "session:default/sess-1",
		provenance.SessionPublisher("default", "sess-1"),
		"publisher string is session:<ns>/<name>")
}

func TestDecodePubKey(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err, "generate key")
	validB64 := base64.StdEncoding.EncodeToString(pub)

	cases := []struct {
		name    string
		b64     string
		wantErr string // substring; "" means expect success
	}{
		{name: "valid 32-byte key round-trips", b64: validB64},
		{name: "invalid base64 returns not-valid-base64 error", b64: "not!base64!", wantErr: "not valid base64"},
		{name: "wrong-size key returns byte-count error", b64: base64.StdEncoding.EncodeToString([]byte("too short")), wantErr: "want 32"},
		{name: "empty string is wrong size", b64: "", wantErr: "is 0 bytes, want 32"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := provenance.DecodePubKey(tc.b64)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Nil(t, got, "no key on error")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, ed25519.PublicKey(pub).Equal(got), "decoded key equals original")
		})
	}
}

func TestEntryDigest_KnownAnswer(t *testing.T) {
	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/sess"},
		Kind:      "test_audit",
		ID:        "ta-1",
		CreatedAt: fixedTime,
		Tags:      []string{"b", "a"},
		Content:   json.RawMessage(`{"outcome":"allowed"}`),
		Provenance: &memory.Provenance{
			Publisher: "session:ns/sess",
			KeyID:     "deadbeef",
			Seq:       1,
			PrevHash:  "",
			Sig:       []byte("ignored-by-digest"),
		},
	}

	d1 := provenance.EntryDigest(e)
	assert.Len(t, d1, 64, "hex SHA-256 is 64 chars")

	// Pinned known answer: regenerated by running this test once with a
	// t.Logf of d1, then hardcoded here. Determinism is asserted across
	// -count=2. Any change to the canonical serialization breaks this.
	const wantDigest = "cfe522e3d2d43ac7901ac9767ba09a9d4265e06bbea95939ac68d7511f59ebc4"
	assert.Equal(t, wantDigest, d1, "EntryDigest is a stable known answer")

	// Tag order does not affect the digest (tags are sorted).
	reordered := e
	reordered.Tags = []string{"a", "b"}
	assert.Equal(t, d1, provenance.EntryDigest(reordered), "tag reorder yields same digest")

	// Sig is not part of the digest.
	mutatedSig := e
	mutatedSig.Provenance = &memory.Provenance{
		Publisher: e.Provenance.Publisher, KeyID: e.Provenance.KeyID,
		Seq: e.Provenance.Seq, PrevHash: e.Provenance.PrevHash,
		Sig: []byte("a completely different signature"),
	}
	assert.Equal(t, d1, provenance.EntryDigest(mutatedSig), "Sig change yields same digest")

	// Content change flips the digest.
	mutatedContent := e
	mutatedContent.Content = json.RawMessage(`{"outcome":"denied"}`)
	assert.NotEqual(t, d1, provenance.EntryDigest(mutatedContent), "content change yields different digest")
}

// TestEntryDigest_ContentEncodingInvariant is the regression guard for
// JSONB round-trip safety: semantically-equivalent content encodings
// (reordered keys, extra whitespace) must produce the SAME digest, so a
// signature survives a postgres JSONB normalization round-trip.
func TestEntryDigest_ContentEncodingInvariant(t *testing.T) {
	base := func(content string) memory.Entry {
		return memory.Entry{
			Scope:     memory.Scope{Kind: "session", ID: "ns/sess"},
			Kind:      "test_audit",
			ID:        "ta-1",
			CreatedAt: fixedTime,
			Content:   json.RawMessage(content),
		}
	}

	sorted := provenance.EntryDigest(base(`{"a":1,"b":2}`))
	reordered := provenance.EntryDigest(base(`{"b":2,"a":1}`))
	whitespace := provenance.EntryDigest(base(`{"a": 1, "b": 2}`))

	assert.Equal(t, sorted, reordered, "reordered keys yield the same digest")
	assert.Equal(t, sorted, whitespace, "added whitespace yields the same digest")
}

// TestEntryDigest_MalformedContentStaysDistinguishing guards the chain against
// a digest collapse: content that is not valid JSON must still produce a
// digest that depends on the entry's identity. Carrying such content through
// canonically as raw bytes made json.Marshal of the canonical struct fail, and
// the dropped error hashed a nil body — so every malformed entry digested to
// sha256(""), one signature verified arbitrarily many of them, and PrevHash
// could not tell a forked chain from a legitimate one.
func TestEntryDigest_MalformedContentStaysDistinguishing(t *testing.T) {
	entry := func(scopeID, kind, id string, seq uint64, prev string, content string) memory.Entry {
		return memory.Entry{
			Scope:     memory.Scope{Kind: "session", ID: scopeID},
			Kind:      kind,
			ID:        id,
			CreatedAt: fixedTime,
			Content:   json.RawMessage(content),
			Provenance: &memory.Provenance{
				Publisher: "session:" + scopeID, KeyID: "deadbeef", Seq: seq, PrevHash: prev,
			},
		}
	}

	const malformed = `{not json`
	a := provenance.EntryDigest(entry("ns/sess-a", "transcript", "e-a", 1, "", malformed))
	b := provenance.EntryDigest(entry("ns/sess-b", "audit", "e-b", 99, "beef", malformed))

	emptySum := sha256.Sum256(nil)
	assert.NotEqual(t, hex.EncodeToString(emptySum[:]), a,
		"a malformed-content digest must not collapse onto sha256(\"\")")
	assert.NotEqual(t, a, b,
		"two malformed entries differing in scope/kind/id/seq/prevHash must digest differently")

	// The fallback is still deterministic, or re-verification would fail.
	assert.Equal(t, a, provenance.EntryDigest(entry("ns/sess-a", "transcript", "e-a", 1, "", malformed)),
		"the malformed-content fallback must be stable across calls")

	// Malformed content is part of the digest, not discarded.
	assert.NotEqual(t, a, provenance.EntryDigest(entry("ns/sess-a", "transcript", "e-a", 1, "", `{also not json`)),
		"different malformed content must digest differently")
}

// TestEntryDigest_CreatedAtMicrosecondInvariant guards the timestamp seam:
// postgres TIMESTAMPTZ stores microsecond precision, so EntryDigest
// truncates CreatedAt to microseconds. Sub-microsecond differences must
// NOT change the digest (the postgres round-trip would otherwise break
// re-verification), but a whole-microsecond difference still must.
func TestEntryDigest_CreatedAtMicrosecondInvariant(t *testing.T) {
	base := time.Date(2026, 6, 12, 1, 2, 3, 0, time.UTC)
	e := memory.Entry{Scope: memory.Scope{Kind: "session", ID: "ns/s"}, Kind: "turn", ID: "turn-1-user", CreatedAt: base.Add(456 * time.Nanosecond), Content: json.RawMessage(`{}`)}
	withNanos := provenance.EntryDigest(e)
	e.CreatedAt = base // same microsecond, zero nanos
	assert.Equal(t, withNanos, provenance.EntryDigest(e), "sub-microsecond CreatedAt differences must not change the digest (postgres stores µs)")
	e.CreatedAt = base.Add(time.Microsecond) // a different microsecond
	assert.NotEqual(t, withNanos, provenance.EntryDigest(e), "microsecond-level CreatedAt differences MUST change the digest")
}

func TestSignerChainsAndVerifierAccepts(t *testing.T) {
	priv := testKey(7)
	signer := provenance.NewSigner(priv, "session:ns/sess")
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	entries := make([]memory.Entry, 3)
	for i := range entries {
		e := memory.Entry{
			Scope: scope, Kind: "test_audit",
			ID:        "ta-" + string(rune('1'+i)),
			CreatedAt: fixedTime,
			Content:   json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		}
		require.NoError(t, signer.Sign(&e), "sign entry %d", i)
		entries[i] = e
	}

	assert.Equal(t, uint64(1), entries[0].Provenance.Seq)
	assert.Equal(t, uint64(2), entries[1].Provenance.Seq)
	assert.Equal(t, uint64(3), entries[2].Provenance.Seq)
	assert.Equal(t, "", entries[0].Provenance.PrevHash, "first entry has empty PrevHash")
	assert.Equal(t, provenance.EntryDigest(entries[0]), entries[1].Provenance.PrevHash,
		"entry1 PrevHash links to entry0 digest")
	assert.Equal(t, provenance.EntryDigest(entries[1]), entries[2].Provenance.PrevHash,
		"entry2 PrevHash links to entry1 digest")

	v := provenance.NewVerifier(provenance.MapKeyLookup{
		{Publisher: "session:ns/sess", KeyID: signer.KeyID()}: pub(priv),
	})
	rep := v.VerifyChain("session:ns/sess", entries, nil)
	assert.Equal(t, 3, rep.OK, "all three entries verify")
	assert.Empty(t, rep.Findings, "no findings on a clean chain")
}

// signClean returns a freshly signed, valid 3-entry chain plus the
// verifier that trusts it.
func signClean(t *testing.T) ([]memory.Entry, *provenance.Verifier, *provenance.Signer) {
	t.Helper()
	priv := testKey(9)
	signer := provenance.NewSigner(priv, "session:ns/sess")
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}
	entries := make([]memory.Entry, 3)
	for i := range entries {
		e := memory.Entry{
			Scope: scope, Kind: "test_audit",
			ID:        "ta-" + string(rune('1'+i)),
			CreatedAt: fixedTime,
			Content:   json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		}
		require.NoError(t, signer.Sign(&e))
		entries[i] = e
	}
	v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: "session:ns/sess", KeyID: signer.KeyID()}: pub(priv)})
	return entries, v, signer
}

func hasVerdict(findings []provenance.Finding, want provenance.Verdict) bool {
	for _, f := range findings {
		if f.Verdict == want {
			return true
		}
	}
	return false
}

func TestVerifierVerdicts(t *testing.T) {
	cases := []struct {
		name string
		// mutate derives the entries/anchor under test from a clean chain.
		mutate func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead)
		want   provenance.Verdict
	}{
		{
			name: "tampered content: bad-signature",
			mutate: func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead) {
				out := append([]memory.Entry(nil), clean...)
				out[1].Content = json.RawMessage(`{"i":99}`) // payload no longer matches Sig
				return out, nil
			},
			want: provenance.VerdictBadSignature,
		},
		{
			name: "dropped middle entry: gap",
			mutate: func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead) {
				return []memory.Entry{clean[0], clean[2]}, nil // seq 2 missing
			},
			want: provenance.VerdictGap,
		},
		{
			name: "unregistered signing key: unknown-key",
			mutate: func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead) {
				// Re-sign with a key the verifier doesn't trust.
				other := provenance.NewSigner(testKey(123), "session:ns/sess")
				e := memory.Entry{
					Scope: clean[0].Scope, Kind: "test_audit", ID: "ta-x",
					CreatedAt: fixedTime, Content: json.RawMessage(`{"i":0}`),
				}
				require.NoError(t, other.Sign(&e))
				return []memory.Entry{e}, nil
			},
			want: provenance.VerdictUnknownKey,
		},
		{
			name: "anchor expects higher seq: tail-truncated",
			mutate: func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead) {
				return clean, &provenance.ChainHead{Seq: 5, LastHash: "whatever"}
			},
			want: provenance.VerdictTailTruncated,
		},
		{
			name: "nil-provenance append-only entry: unsigned",
			mutate: func(t *testing.T, clean []memory.Entry) ([]memory.Entry, *provenance.ChainHead) {
				unsigned := memory.Entry{
					Scope: clean[0].Scope, Kind: "test_audit", ID: "ta-z",
					CreatedAt: fixedTime, Content: json.RawMessage(`{}`),
				}
				return append(append([]memory.Entry(nil), clean...), unsigned), nil
			},
			want: provenance.VerdictUnsigned,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clean, v, _ := signClean(t)
			entries, anchor := tc.mutate(t, clean)
			rep := v.VerifyChain("session:ns/sess", entries, anchor)
			assert.Truef(t, hasVerdict(rep.Findings, tc.want),
				"expected verdict %q in findings, got %+v", tc.want, rep.Findings)
		})
	}
}

// TestVerifierMalformedKeyNoPanic guards Fix 2: a trusted-map entry
// whose public key is the wrong size must yield an unknown-key verdict,
// not panic in ed25519.Verify (which requires exactly 32 bytes).
func TestVerifierMalformedKeyNoPanic(t *testing.T) {
	clean, _, signer := signClean(t)

	// Trust the signer's KeyID, but map it to a 5-byte (malformed) key.
	v := provenance.NewVerifier(provenance.MapKeyLookup{
		{Publisher: "session:ns/sess", KeyID: signer.KeyID()}: ed25519.PublicKey{1, 2, 3, 4, 5},
	})

	var rep provenance.Report
	require.NotPanics(t, func() {
		rep = v.VerifyChain("session:ns/sess", clean, nil)
	}, "malformed key must not panic ed25519.Verify")
	assert.True(t, hasVerdict(rep.Findings, provenance.VerdictUnknownKey),
		"malformed key yields unknown-key verdict, got %+v", rep.Findings)
}

// TestVerifierRejectsSeqZero guards Fix 3: a validly-signed entry at
// Seq 0 violates the 1-based chain contract and is a fork finding.
func TestVerifierRejectsSeqZero(t *testing.T) {
	priv := testKey(21)
	signer := provenance.NewSigner(priv, "session:ns/sess")

	// Sign normally (Seq becomes 1), then force Seq 0 and re-sign so the
	// signature still verifies — isolating the seq-0 rule from a bad-sig.
	e := memory.Entry{
		Scope: memory.Scope{Kind: "session", ID: "ns/sess"}, Kind: "test_audit",
		ID: "ta-1", CreatedAt: fixedTime, Content: json.RawMessage(`{"i":0}`),
	}
	require.NoError(t, signer.Sign(&e))
	e.Provenance.Seq = 0
	raw, err := hex.DecodeString(provenance.EntryDigest(e))
	require.NoError(t, err)
	e.Provenance.Sig = ed25519.Sign(priv, raw)

	v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: "session:ns/sess", KeyID: signer.KeyID()}: pub(priv)})
	rep := v.VerifyChain("session:ns/sess", []memory.Entry{e}, nil)
	assert.True(t, hasVerdict(rep.Findings, provenance.VerdictFork),
		"seq 0 yields a fork verdict, got %+v", rep.Findings)
}

func TestSeedFromMemoryResumesChain(t *testing.T) {
	registerKinds(t)

	mem := memory.NewLocal(inmem.NewBackend())
	priv := testKey(11)
	publisher := "session:ns/sess"
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	signing := provenance.NewSigningMemory(mem, provenance.NewSigner(priv, publisher))
	stored := make([]memory.Entry, 2)
	for i := range stored {
		out, err := signing.Put(memory.WithSystemApproval(context.Background(), "test"), auditEntry("ta-"+string(rune('1'+i)), `{"i":`+string(rune('0'+i))+`}`))
		require.NoError(t, err, "put entry %d", i)
		stored[i] = out
	}
	require.Equal(t, uint64(1), stored[0].Provenance.Seq)
	require.Equal(t, uint64(2), stored[1].Provenance.Seq)

	// Fresh Signer, same key + publisher, resumes from persisted state.
	resumed := provenance.NewSigner(priv, publisher)
	require.NoError(t, resumed.SeedFromMemory(memory.WithSystemApproval(context.Background(), "test"), mem, scope))

	third := auditEntry("ta-3", `{"i":2}`)
	require.NoError(t, resumed.Sign(&third))
	assert.Equal(t, uint64(3), third.Provenance.Seq, "resumed chain continues at seq 3")
	assert.Equal(t, provenance.EntryDigest(stored[1]), third.Provenance.PrevHash,
		"third entry links to the second's digest")
}

func TestSigningMemoryPassesThroughMutableKinds(t *testing.T) {
	registerKinds(t)

	mem := memory.NewLocal(inmem.NewBackend())
	signing := provenance.NewSigningMemory(mem, provenance.NewSigner(testKey(13), "session:ns/sess"))

	e := memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: "ns/sess"},
		Kind:      "test_mutable",
		ID:        "tm-1",
		CreatedAt: fixedTime,
		Content:   json.RawMessage(`{"k":"v"}`),
	}
	out, err := signing.Put(memory.WithSystemApproval(context.Background(), "test"), e)
	require.NoError(t, err)
	assert.Nil(t, out.Provenance, "mutable-kind entry stays unsigned")
}
