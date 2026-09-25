package auditcmd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/provenance"
)

// auditTestScope is the fixed scope every fixture chain is signed under.
var auditTestScope = memory.Scope{Kind: "session", ID: "ns/sess"}

var auditFixedTime = time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

// testSigner mints a deterministic Signer for publisher from a seed byte,
// returning the signer and its public key for the verifier key set.
func testSigner(t *testing.T, seed byte, publisher string) (*provenance.Signer, ed25519.PublicKey) {
	t.Helper()
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	priv := ed25519.NewKeyFromSeed(s)
	return provenance.NewSigner(priv, publisher), priv.Public().(ed25519.PublicKey)
}

// signChain signs n append-only entries under signer (advancing its chain)
// and returns them. ids are "ta-1".."ta-n".
func signChain(t *testing.T, signer *provenance.Signer, n int) []memory.Entry {
	t.Helper()
	out := make([]memory.Entry, n)
	for i := 0; i < n; i++ {
		e := memory.Entry{
			Scope:     auditTestScope,
			Kind:      "test_audit",
			ID:        "ta-" + string(rune('1'+i)),
			CreatedAt: auditFixedTime,
			Content:   json.RawMessage(`{"i":` + string(rune('0'+i)) + `}`),
		}
		require.NoError(t, signer.Sign(&e), "sign entry %d", i)
		out[i] = e
	}
	return out
}

func hasVerdict(reports []provenance.Report, publisher string, want provenance.Verdict) bool {
	for _, rep := range reports {
		if rep.Publisher != publisher {
			continue
		}
		for _, f := range rep.Findings {
			if f.Verdict == want {
				return true
			}
		}
	}
	return false
}

func reportFor(reports []provenance.Report, publisher string) (provenance.Report, bool) {
	for _, rep := range reports {
		if rep.Publisher == publisher {
			return rep, true
		}
	}
	return provenance.Report{}, false
}

func TestBuildAuditReport(t *testing.T) {
	const pubA = "session:ns/sess"
	const pubB = "system:operator"

	t.Run("all clean: no findings, ok", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		sb, kb := testSigner(t, 8, pubB)
		entries := append(signChain(t, sa, 3), signChain(t, sb, 2)...)
		v := provenance.NewVerifier(provenance.MapKeyLookup{
			{Publisher: pubA, KeyID: sa.KeyID()}: ka,
			{Publisher: pubB, KeyID: sb.KeyID()}: kb,
		})

		reports := buildAuditReport(v, entries, nil)

		assert.Equal(t, 0, hardFindingCount(reports), "clean chains have no hard findings")
		repA, ok := reportFor(reports, pubA)
		require.True(t, ok, "publisher A report present")
		assert.Equal(t, 3, repA.OK, "all 3 of A verify")
		assert.Empty(t, repA.Findings, "no findings for A")
		repB, ok := reportFor(reports, pubB)
		require.True(t, ok, "publisher B report present")
		assert.Equal(t, 2, repB.OK, "both of B verify")
	})

	t.Run("tampered entry: bad-signature, not ok", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		entries := signChain(t, sa, 3)
		entries[1].Content = json.RawMessage(`{"i":99}`) // payload no longer matches Sig
		v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: pubA, KeyID: sa.KeyID()}: ka})

		reports := buildAuditReport(v, entries, nil)

		assert.True(t, hasVerdict(reports, pubA, provenance.VerdictBadSignature), "tampered payload → bad-signature")
		assert.Greater(t, hardFindingCount(reports), 0, "bad-signature is a hard finding")
	})

	t.Run("dropped middle entry: gap", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		full := signChain(t, sa, 3)
		entries := []memory.Entry{full[0], full[2]} // seq 2 missing
		v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: pubA, KeyID: sa.KeyID()}: ka})

		reports := buildAuditReport(v, entries, nil)

		assert.True(t, hasVerdict(reports, pubA, provenance.VerdictGap), "missing seq → gap")
		assert.Greater(t, hardFindingCount(reports), 0, "gap is a hard finding")
	})

	t.Run("unsigned legacy entry: unsigned only, still ok", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		entries := signChain(t, sa, 2)
		// A nil-provenance append-only entry (legacy / unsigned writer).
		entries = append(entries, memory.Entry{
			Scope: auditTestScope, Kind: "test_audit", ID: "ta-legacy",
			CreatedAt: auditFixedTime, Content: json.RawMessage(`{}`),
		})
		v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: pubA, KeyID: sa.KeyID()}: ka})

		reports := buildAuditReport(v, entries, nil)

		assert.Equal(t, 0, hardFindingCount(reports), "unsigned entries are not hard findings")
		assert.True(t, hasVerdict(reports, unsignedPublisher, provenance.VerdictUnsigned),
			"legacy entry bucketed under the unsigned publisher")
		// The signed publisher's report must be clean — unsigned noise is not
		// counted against it.
		repA, ok := reportFor(reports, pubA)
		require.True(t, ok)
		assert.Empty(t, repA.Findings, "signed publisher report carries no unsigned findings")
		assert.Equal(t, 2, repA.OK, "both signed entries verify")
	})

	t.Run("tail anchor beyond stored: tail-truncated", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		entries := signChain(t, sa, 3)
		v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: pubA, KeyID: sa.KeyID()}: ka})
		// Anchor expects seq 5 but only 3 are stored → truncation.
		anchors := map[string]*provenance.ChainHead{pubA: {Seq: 5, LastHash: "whatever"}}

		reports := buildAuditReport(v, entries, anchors)

		assert.True(t, hasVerdict(reports, pubA, provenance.VerdictTailTruncated), "anchor beyond stored → tail-truncated")
		assert.Greater(t, hardFindingCount(reports), 0, "tail-truncated is a hard finding")
	})
}

// TestBuildAuditReport_AnchoredPublisherWithZeroEntries pins the maximal-tampering
// case: deleting EVERY append-only entry of a publisher must not be quieter than
// deleting one of them. The anchor on status.auditChainHeads is what makes the
// missing tail detectable, so a publisher that has an anchor and no surviving
// entries has to reach VerifyChain — otherwise the wipe produces no report row,
// no finding, and `oap audit verify` exits 0 on the worst input it can be given.
func TestBuildAuditReport_AnchoredPublisherWithZeroEntries(t *testing.T) {
	const pubA = "session:ns/sess"
	const pubB = "system:operator"

	t.Run("whole scope wiped: tail-truncated reported, not a clean bill of health", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		v := provenance.NewVerifier(provenance.MapKeyLookup{{Publisher: pubA, KeyID: sa.KeyID()}: ka})
		// The session ran and anchored a 3-entry chain; every entry is gone.
		anchors := map[string]*provenance.ChainHead{pubA: {Seq: 3, LastHash: "deadbeef"}}

		reports := buildAuditReport(v, nil, anchors)

		repA, ok := reportFor(reports, pubA)
		require.True(t, ok, "an anchored publisher must be reported even with zero surviving entries")
		assert.Equal(t, 0, repA.OK, "nothing verified — there is nothing left")
		assert.True(t, hasVerdict(reports, pubA, provenance.VerdictTailTruncated),
			"anchor at seq 3 with no stored entries → tail-truncated")
		assert.Greater(t, hardFindingCount(reports), 0, "a wiped chain must fail verification")
	})

	t.Run("one publisher wiped, the other intact: wiped row present and failing", func(t *testing.T) {
		sa, ka := testSigner(t, 7, pubA)
		sb, kb := testSigner(t, 8, pubB)
		entries := signChain(t, sa, 3) // pubB's entries deleted; pubA's untouched
		v := provenance.NewVerifier(provenance.MapKeyLookup{
			{Publisher: pubA, KeyID: sa.KeyID()}: ka,
			{Publisher: pubB, KeyID: sb.KeyID()}: kb,
		})
		anchors := map[string]*provenance.ChainHead{
			pubA: {Seq: 3, LastHash: provenance.EntryDigest(entries[2])},
			pubB: {Seq: 2, LastHash: "cafef00d"},
		}

		reports := buildAuditReport(v, entries, anchors)

		repA, ok := reportFor(reports, pubA)
		require.True(t, ok, "the intact publisher is still reported")
		assert.Empty(t, repA.Findings, "the intact chain matches its anchor")
		_, ok = reportFor(reports, pubB)
		require.True(t, ok, "the wiped publisher must not silently vanish from the report")
		assert.True(t, hasVerdict(reports, pubB, provenance.VerdictTailTruncated),
			"the wiped publisher's anchor has no chain behind it → tail-truncated")
		assert.Greater(t, hardFindingCount(reports), 0, "a wiped chain must fail verification")
	})
}

// TestParseAnchors covers the status.AuditChainHeads "seq:hash" decode,
// including malformed entries that must be skipped rather than abort.
func TestParseAnchors(t *testing.T) {
	var errOut bytes.Buffer
	got, _ := parseAnchors(map[string]string{
		"session:ns/sess": "3:deadbeef",
		"system:operator": "12:cafef00d",
		"bad-no-colon":    "nope",
		"bad-seq":         "x:hash",
	}, &errOut)
	require.Len(t, got, 2, "only well-formed anchors are kept")
	assert.Equal(t, &provenance.ChainHead{Seq: 3, LastHash: "deadbeef"}, got["session:ns/sess"])
	assert.Equal(t, &provenance.ChainHead{Seq: 12, LastHash: "cafef00d"}, got["system:operator"])
	assert.Nil(t, got["bad-no-colon"], "missing colon → skipped")
	assert.Nil(t, got["bad-seq"], "non-numeric seq → skipped")
}

// TestParseAnchors_MalformedHeadIsSurfacedAndFails pins the half that matters
// operationally: a chain head that cannot be decoded removes tail-truncation
// detection for that publisher entirely (VerifyChain's whole truncation block
// is gated on a non-nil anchor), so it must be BOTH warned about and fatal.
// Skipping it quietly means `oap audit verify` prints a clean report and exits
// 0 for a session whose tail nothing checked.
func TestParseAnchors_MalformedHeadIsSurfacedAndFails(t *testing.T) {
	cases := []struct {
		name string
		head string
	}{
		{name: "no colon: warned, publisher reported malformed", head: "nope"},
		{name: "non-numeric seq: warned, publisher reported malformed", head: "x:hash"},
		{name: "empty seq: warned, publisher reported malformed", head: ":hash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var errOut bytes.Buffer
			anchors, malformed := parseAnchors(map[string]string{"system:operator": tc.head}, &errOut)

			assert.Empty(t, anchors, "a malformed head yields no anchor")
			assert.Equal(t, []string{"system:operator"}, malformed, "the publisher is reported as malformed")
			assert.Contains(t, errOut.String(), "system:operator", "the operator is warned which publisher is affected")
			assert.Error(t, auditExitError(nil, malformed), "an unverifiable tail must not exit 0")
		})
	}
}

// TestAuditExitError covers the exit-status decision in isolation: hard
// findings and malformed anchors each fail on their own, a clean run does not.
func TestAuditExitError(t *testing.T) {
	hard := []provenance.Report{{Publisher: "p", Findings: []provenance.Finding{{Verdict: provenance.VerdictGap}}}}
	unsignedOnly := []provenance.Report{{Publisher: "unsigned", Findings: []provenance.Finding{{Verdict: provenance.VerdictUnsigned}}}}

	assert.ErrorIs(t, auditExitError(hard, nil), errAuditFailed, "hard finding → fail")
	assert.ErrorIs(t, auditExitError(nil, []string{"p"}), errAuditAnchorMalformed, "malformed anchor alone → fail")
	assert.NoError(t, auditExitError(unsignedOnly, nil), "unsigned entries alone stay a warning")
	assert.NoError(t, auditExitError(nil, nil), "clean run → exit 0")
}
