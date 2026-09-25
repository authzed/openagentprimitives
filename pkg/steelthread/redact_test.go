package steelthread_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/kinds/triggerdelivery"
	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// TestParseRedaction covers the four malformed rules that are refused, and the
// three forms that are accepted. Each refusal exists because ACCEPTING it
// produces a capture that looks redacted and is not.
func TestParseRedaction(t *testing.T) {
	cases := []struct {
		name    string
		spec    string
		wantOld string
		wantNew string
		wantErr string
	}{
		{
			name: "old=new: parsed", spec: "acme-corp=COMPANY-A",
			wantOld: "acme-corp", wantNew: "COMPANY-A",
		},
		{
			name: "a replacement containing an = is kept whole", spec: "acme-corp=COMPANY=A",
			wantOld: "acme-corp", wantNew: "COMPANY=A",
		},
		{
			// No separator is not a typo of old=new: it is the deliberate
			// request for a generated stand-in, and New stays empty until
			// ResolveRedactions — which is the only place that knows the whole
			// rule set a token has to be unique against.
			name: "a bare original: the generated form, replacement left empty", spec: "acme-corp",
			wantOld: "acme-corp", wantNew: "",
		},
		{
			name: "an empty original: refused, it matches every offset", spec: "=COMPANY-A",
			wantErr: "empty original",
		},
		{
			// An explicit = is a promise a replacement follows. A promise not
			// kept is what a truncated line looks like, so it stays an error
			// even though the bare form above now means "choose one for me".
			name: "an empty replacement after =: refused, name it or drop the =", spec: "acme-corp=",
			wantErr: "empty replacement",
		},
		{
			name: "a replacement containing the original: refused, nothing is removed",
			spec: "acme=acme-corp", wantErr: "still contains it",
		},
		{
			name: "a shorter replacement: refused, it desyncs recorded byte counts",
			spec: "josephplaceholder=testorg", wantErr: "17 byte(s)",
		},
		{
			name:    "a longer replacement: refused for the same reason, both directions",
			spec:    "acme-corp=COMPANY-A-INTERNATIONAL",
			wantErr: "is 23 byte(s) where the value it replaces is 9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := steelthread.ParseRedaction(tc.spec)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantOld, got.Old)
			assert.Equal(t, tc.wantNew, got.New)
		})
	}
}

// TestParseRedactionFile pins the file form: comments and blanks are skipped so
// an operator can keep an annotated, reusable list, and a malformed line is an
// ERROR naming its number rather than a skip. Skipping one silently ships the
// value that line was written to remove.
func TestParseRedactionFile(t *testing.T) {
	t.Run("comments and blank lines are skipped", func(t *testing.T) {
		got, err := steelthread.ParseRedactionFile(
			"# the customers this agent's sessions name\nacme-corp=COMPANY-A\n\n  \nbeta-labs=COMPANY-B\n")
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "COMPANY-A", got[0].New)
		assert.Equal(t, "COMPANY-B", got[1].New)
	})

	t.Run("a bare line is the generated form, not a malformed one", func(t *testing.T) {
		got, err := steelthread.ParseRedactionFile("acme-corp=COMPANY-A\nbeta-labs\n")
		require.NoError(t, err)
		require.Len(t, got, 2)
		assert.Equal(t, "beta-labs", got[1].Old)
		assert.Empty(t, got[1].New, "the stand-in is chosen by ResolveRedactions, not by the parser")
	})

	t.Run("a malformed line names its number", func(t *testing.T) {
		_, err := steelthread.ParseRedactionFile("acme-corp=COMPANY-A\nbeta-labs=\n")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "line 2")
	})
}

// recordsNamingACustomer is a session whose tool result carries both a
// third-party identifier a scan can never recognize and a credential shape one
// always will — the two halves the redaction/scan ordering has to keep apart.
func recordsNamingACustomer(t *testing.T) steelthread.Records {
	t.Helper()
	recs := syntheticRecords(t)
	recs.Turns[2] = toolResult(2, "tu_1",
		`{"owner":"acme-corp","key":"`+fakeAWSKeyID+`"}`, false)
	return recs
}

func captureWith(t *testing.T, recs steelthread.Records, rules ...string) (steelthread.Result, []steelthread.Finding) {
	t.Helper()
	in := captureInput(t)
	for _, spec := range rules {
		r, err := steelthread.ParseRedaction(spec)
		require.NoError(t, err, "the test's own redaction must parse")
		in.Redact = append(in.Redact, r)
	}
	res, findings, err := steelthread.Capture(recs, in)
	require.NoError(t, err)
	return res, findings
}

// bytesOf returns the emitted file's bytes, failing with the full list when it
// is absent.
func bytesOf(t *testing.T, res steelthread.Result, name string) []byte {
	t.Helper()
	var got []string
	for _, f := range res.Emitted {
		if f.Name == name {
			return f.Bytes
		}
		got = append(got, f.Name)
	}
	require.Failf(t, "expected emitted file missing", "%q not among %v", name, got)
	return nil
}

// TestCapture_ARedactionCannotSuppressAStructuralFinding is the ordering
// property, and it is the reason there is no --no-secret-scan flag: nothing
// needs to forbid suppressing a finding, because the order makes it impossible.
//
// Redactions run over the final bytes and the scan runs over the result. A rule
// that genuinely removed a credential leaves nothing to find; a rule that did
// not — including one whose replacement is itself credential-shaped — leaves
// the scan looking at the same bytes that would reach the repo.
func TestCapture_ARedactionCannotSuppressAStructuralFinding(t *testing.T) {
	recs := recordsNamingACustomer(t)

	t.Run("an unrelated redaction leaves the finding standing", func(t *testing.T) {
		res, findings := captureWith(t, recs, "acme-corp=COMPANY-A")

		f := findByCode(t, findings, steelthread.CodeStructuralSecret)
		assert.Contains(t, f.Message, "aws-access-key-id")
		assert.NotContains(t, string(bytesOf(t, res, "bundle.json")), "acme-corp",
			"the redaction that DID apply must still have applied")
	})

	t.Run("a redaction that CREATES a credential shape is caught", func(t *testing.T) {
		// The direction-proving case, and it is built with some care. A rule
		// that merely rewrote an EXISTING credential would fire under either
		// ordering, since the original is credential-shaped too — so here the
		// transcript carries no credential and the rule assembles one.
		//
		// Neither half is credential-shaped on its own: "KEYPREFIXXXX7EXAMPLE"
		// matches nothing, and the replacement token "AKIAIOSFODNN" is four
		// characters short of the AWS pattern's sixteen. Only their PRODUCT is,
		// and the product exists nowhere but in the redacted output — not even
		// in the bundle's own redaction record, which carries the token alone.
		// A finding here is therefore possible only if the scan read the rule's
		// output.
		//
		// Both halves are twelve bytes. A length-changing rule is refused, so
		// the original carries the padding the replacement's length demands.
		clean := syntheticRecords(t)
		clean.Turns[2] = toolResult(2, "tu_1", `{"owner":"KEYPREFIXXXX7EXAMPLE"}`, false)

		_, findings := captureWith(t, clean, "KEYPREFIXXXX=AKIAIOSFODNN")
		f := findByCode(t, findings, steelthread.CodeStructuralSecret)
		assert.Contains(t, f.Message, "aws-access-key-id")
	})

	t.Run("removing the credential genuinely clears it", func(t *testing.T) {
		// The generated form, and it is the natural one here: a credential is
		// exactly the case where nobody wants to count the original's bytes to
		// invent a same-length stand-in for it.
		res, findings := captureWith(t, recs, fakeAWSKeyID)
		for _, f := range findings {
			assert.NotEqual(t, steelthread.CodeStructuralSecret, f.Code,
				"a credential that is gone must not still be reported: %s", f.Message)
		}
		for _, f := range res.Emitted {
			assert.NotContains(t, string(f.Bytes), fakeAWSKeyID,
				"%s still carries the value the redaction removed", f.Name)
		}
	})
}

// TestCapture_RedactionsAreRecordedWithoutTheirOriginals pins what the bundle
// says about its own redactions. A record naming the ORIGINAL would carry the
// very string the operator asked to have removed into the repo the redaction
// existed to keep it out of.
func TestCapture_RedactionsAreRecordedWithoutTheirOriginals(t *testing.T) {
	res, _ := captureWith(t, recordsNamingACustomer(t),
		"acme-corp=COMPANY-A", fakeAWSKeyID+"=REDACTED-AWS-KEY-000", "absent-co=COMPANY-Z")

	require.NotNil(t, res.Bundle.Capture)
	require.Len(t, res.Bundle.Capture.Redactions, 3)

	assert.Equal(t, "COMPANY-A", res.Bundle.Capture.Redactions[0].Replacement)
	assert.Positive(t, res.Bundle.Capture.Redactions[0].Count)
	assert.Equal(t, "REDACTED-AWS-KEY-000", res.Bundle.Capture.Redactions[1].Replacement)
	assert.Positive(t, res.Bundle.Capture.Redactions[1].Count)

	// A rule that matched NOTHING is recorded with a zero count rather than
	// dropped: a typo'd rule and a rule with nothing to do are otherwise
	// indistinguishable, and the first is the one an operator needs to hear
	// about.
	assert.Equal(t, "COMPANY-Z", res.Bundle.Capture.Redactions[2].Replacement)
	assert.Equal(t, 0, res.Bundle.Capture.Redactions[2].Count)

	for _, f := range res.Emitted {
		assert.NotContains(t, string(f.Bytes), "acme-corp",
			"%s carries an original the bundle promises is absent", f.Name)
		assert.NotContains(t, string(f.Bytes), "absent-co",
			"%s echoes a rule's original back through the redaction record", f.Name)
	}
}

// TestCapture_RedactionsReachEverySurface pins that a rule is applied to all
// four files a capture writes, not just the manifests. A capture that scrubbed
// a customer's name out of the fixture and left it in the transcript has
// removed nothing.
func TestCapture_RedactionsReachEverySurface(t *testing.T) {
	recs := recordsNamingACustomer(t)
	recs.Trigger = &triggerdelivery.Content{
		Kind: "demoforge", Event: "pull_request", ChannelKey: "pr:acme-corp/widgets#4",
		Body: []byte(`{"repo":"acme-corp/widgets"}`),
	}
	in := captureInput(t)
	in.Fixture.TriggerChannel = "demo-hooks"
	r, err := steelthread.ParseRedaction("acme-corp=COMPANY-A")
	require.NoError(t, err)
	in.Redact = []steelthread.Redaction{r}

	res, _, err := steelthread.Capture(recs, in)
	require.NoError(t, err)

	require.NotEmpty(t, res.Emitted)
	for _, f := range res.Emitted {
		assert.NotContains(t, string(f.Bytes), "acme-corp", "%s was not redacted", f.Name)
	}
	assert.Contains(t, string(bytesOf(t, res, "payloads/delivery.json")), "COMPANY-A",
		"the trigger payload is a surface like any other; it is re-signed at replay, so rewriting it is safe")
}

// TestCapture_RedactionTokensDoNotRaiseTheEntropyWarning pins an exclusion
// that was MEASURED rather than anticipated.
//
// A real capture emitted zero entropy warnings and then, redacted with 55
// COMPANY-nn tokens, reported thirty-two runs. Substituting an upper-case token
// into an otherwise lowercase identifier is what did it: the mixed case and
// digits lift the run over the floor. Every one of those was a name the
// operator chose, so the warning said nothing true — and a warning that fires
// on every redacted capture is the noise the floor exists to avoid.
//
// The exclusion is entropy-only. The hard patterns still read the redacted
// bytes, which is what the ordering test above relies on.
func TestCapture_RedactionTokensDoNotRaiseTheEntropyWarning(t *testing.T) {
	recs := syntheticRecords(t)
	// A flat lowercase identifier, long enough to be scanned and quiet on its
	// own at 4.07 bits per character; substituting the token lifts it to 4.86,
	// across the 4.5 floor. Both numbers are what makes this test discriminate
	// rather than pass either way — a milder pair does not cross at all.
	recs.Turns[2] = toolResult(2, "tu_1",
		`{"node":"crdb_metrics_customerplaceholder_uswest2_zoneb_shard"}`, false)

	_, plain := captureWith(t, recs)
	for _, f := range plain {
		require.NotEqual(t, steelthread.CodeHighEntropyBlob, f.Code,
			"the unredacted control must be quiet, or this test proves nothing: %s", f.Message)
	}

	// Nineteen bytes for nineteen: a length-changing rule is refused, so the
	// mixed case and digits that lift the run over the floor have to be packed
	// into the original's own width.
	_, redacted := captureWith(t, recs, "customerplaceholder=COMPANY-01-XYZ-4567")
	for _, f := range redacted {
		assert.NotEqual(t, steelthread.CodeHighEntropyBlob, f.Code,
			"a run whose statistics the capture itself changed is not unknown bytes: %s", f.Message)
	}
}

// TestWriteResult_WritesExactlyTheBytesThatWereScanned is the structural half
// of the fix. While WriteResult re-derived its own bytes from Result.Bundle,
// the scans could look at one thing and the repo receive another — which is
// how three of the four surfaces shipped unscanned.
func TestWriteResult_WritesExactlyTheBytesThatWereScanned(t *testing.T) {
	res, _ := captureWith(t, recordsNamingACustomer(t), "acme-corp=COMPANY-A")

	dir := t.TempDir()
	require.NoError(t, steelthread.WriteResult(dir, res))

	var onDisk int
	require.NoError(t, filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		onDisk++
		return nil
	}))
	assert.Equal(t, len(res.Emitted), onDisk,
		"every emitted file and nothing else must land on disk")

	for _, f := range res.Emitted {
		got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f.Name)))
		require.NoError(t, err, "emitted file %s was not written", f.Name)
		assert.Equal(t, string(f.Bytes), string(got),
			"%s on disk differs from the bytes the secret scans read", f.Name)
	}
}

// TestWriteResult_RefusesAResultItDidNotGetFromCapture pins the fail-closed
// precondition. Emitted is the only thing WriteResult writes, so an empty one
// would create the directory, report success and leave nothing in it — and with
// Overwrite it would first DELETE a real capture to put nothing in its place.
func TestWriteResult_RefusesAResultItDidNotGetFromCapture(t *testing.T) {
	dir := t.TempDir()
	err := steelthread.WriteResult(dir, steelthread.Result{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bundle.json")

	entries, readErr := os.ReadDir(dir)
	require.NoError(t, readErr)
	assert.Empty(t, entries, "a refused write must leave the directory as it found it")
}
