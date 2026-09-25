package steelthread_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/steelthread"
)

// Every credential planted below is OBVIOUSLY fake, and several are the
// vendor's own published example value (AKIAIOSFODNN7EXAMPLE is AWS's, the JWT
// is the one every JWT tutorial shows). The point is to exercise a SHAPE, and a
// test fixture that looked like a real key would be the exact thing this file
// exists to keep out of a repo.
const (
	fakePEMKey    = "-----BEGIN RSA PRIVATE KEY-----\nRVhBTVBMRQ==\n-----END RSA PRIVATE KEY-----"
	fakeJWT       = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJFWEFNUExFIn0.EXAMPLEsignature"
	fakeGitHubPAT = "ghp_EXAMPLEONLYEXAMPLEONLYEXAMPLEONLY01"
	fakeAWSKeyID  = "AKIAIOSFODNN7EXAMPLE"
	fakeSlackTok  = "xoxb-000000000000-000000000000-EXAMPLEONLYEXAMPLEONLY"
	fakeAnthropic = "sk-ant-api03-EXAMPLEONLYEXAMPLEONLYEXAMPLEONLY"
	fakeGoogleKey = "AIzaEXAMPLEONLYEXAMPLEONLYEXAMPLEONLY12"
	fakeBearer    = "Authorization: Bearer EXAMPLEONLYTOKENVALUE"
	fakeClientKey = "client-key-data: RVhBTVBMRVBSSVZBVEVLRVk="
	// Mixed case and digits in one unbroken run: what credential MATERIAL looks
	// like, as opposed to the credential NAMES and prose the same field also
	// carries. "EXAMPLEONLYPASSWORD" would be neither — one character class and
	// no vendor prefix — and opaqueValue correctly declines it. See TestOpaqueValue.
	fakePassword = "password: EXAMPLEonly0P4ssw0rd"
)

// TestStructuralScan_FiresOnEveryShape is the half of this detector that makes
// it a detector. A pattern table with no test proving each row FIRES is a list
// of regexps that reads as coverage.
//
// Each case plants one credential shape into bundle.json — the surface that
// used to go unscanned, and the one a credential returned by an upstream tool
// actually lands in — and requires a hard structural-secret finding naming both
// the file and the pattern.
func TestStructuralScan_FiresOnEveryShape(t *testing.T) {
	cases := []struct {
		name    string
		planted string
		pattern string
	}{
		{name: "a PEM private key", planted: fakePEMKey, pattern: "pem-private-key"},
		{name: "a JWT", planted: fakeJWT, pattern: "jwt"},
		{name: "a GitHub personal-access token", planted: fakeGitHubPAT, pattern: "github-token"},
		{name: "an AWS access-key id", planted: fakeAWSKeyID, pattern: "aws-access-key-id"},
		{name: "a Slack bot token", planted: fakeSlackTok, pattern: "slack-token"},
		{name: "an Anthropic-style API key", planted: fakeAnthropic, pattern: "openai-anthropic-key"},
		{name: "a Google API key", planted: fakeGoogleKey, pattern: "google-api-key"},
		{name: "an Authorization: Bearer header", planted: fakeBearer, pattern: "authorization-bearer"},
		{name: "a kubeconfig client-key-data field", planted: fakeClientKey, pattern: "kubeconfig-client-key"},
		{name: "a populated password field", planted: fakePassword, pattern: "credential-field"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := cleanCapture(t)
			plant(t, &in, "bundle.json", []byte("\n"+tc.planted+"\n"))

			f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeStructuralSecret)
			assert.Equal(t, steelthread.SeverityHard, f.Severity,
				"a credential shape must STOP the capture, not annotate it")
			assert.Contains(t, f.Message, tc.pattern, "the finding must name which shape matched")
			assert.Contains(t, f.Message, "bundle.json", "the finding must name the file a reader has to open")
			assert.NotContains(t, f.Message, tc.planted,
				"the finding must never reprint the credential it caught")
			assert.True(t, steelthread.HasHardFinding(steelthread.SelfCheck(in)))
		})
	}
}

// TestStructuralScan_SilentOnACleanCapture is the negative control, and it is
// what stops the table above from passing on a detector that fires on
// everything. cleanCapture carries a transcript, a bundle and two manifests and
// not one credential.
func TestStructuralScan_SilentOnACleanCapture(t *testing.T) {
	for _, f := range steelthread.SelfCheck(cleanCapture(t)) {
		assert.NotEqual(t, steelthread.CodeStructuralSecret, f.Code,
			"the structural scan fired on a capture with no credential in it: %s", f.Message)
		assert.NotEqual(t, steelthread.CodeHighEntropyBlob, f.Code,
			"the entropy warning fired on ordinary bundle content: %s", f.Message)
	}
}

// TestStructuralScan_PublicCertificateFieldsAreNotFindings pins the exclusion
// that keeps this detector usable.
//
// certificate-authority-data and client-certificate-data are PUBLIC
// certificates — the CA a client verifies the API server against, and the
// client's own certificate whose secret half is the separate client-key-data
// field. Refusing a capture for either would refuse every session that so much
// as printed a kubeconfig, for material that is public by construction; a
// detector that refuses valid work gets switched off, and then it catches
// nothing at all.
func TestStructuralScan_PublicCertificateFieldsAreNotFindings(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "bundle.json", []byte(
		"\ncertificate-authority-data: RVhBTVBMRUNB\nclient-certificate-data: RVhBTVBMRUNFUlQ=\n"))

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeStructuralSecret, f.Code,
			"a public certificate must not refuse a capture: %s", f.Message)
	}
}

// TestStructuralScan_FixturePlaceholdersAreNotFindings pins the other
// exclusion, and this one is not hypothetical: RewriteFixture writes
// SHAPE-VALID placeholder credentials on purpose, because an AgentIdentity
// whose stored value fails its provider's declared token shape never goes Valid
// and the replayed bundle is skipped. The values below are what a real capture
// of a GitHub-credentialled agent emits today.
func TestStructuralScan_FixturePlaceholdersAreNotFindings(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "03-agent.yaml", []byte(
		"stringData:\n  token: ghx_-fixtureplaceholder\n  api-key: unused-by-the-test-provider\n"+
			"  bot-token: fixture-placeholder-bot-token\n"))

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeStructuralSecret, f.Code,
			"this package's own placeholder substitution must not read as a leak: %s", f.Message)
	}
}

// TestStructuralScan_HighEntropyWarnsAndNeverRefuses pins the one severity
// judgement in the detector. An embedded image, a compressed blob, a diff and a
// webhook body all have the statistics of key material, and nothing in the
// bytes tells them apart — so a human is asked to look rather than the capture
// deciding for them.
func TestStructuralScan_HighEntropyWarnsAndNeverRefuses(t *testing.T) {
	// 64 base64 characters with no repeating structure: entropy near the
	// ceiling for its length, which is what an encoded key looks like and what
	// an encoded PNG looks like too.
	blob := "aG7pQzX2mVwL9tRcJ4yNbF8kSdH3uZeA1oPiT6rWgYxCvKlB0jMnEqUsD5fXhIaO"
	require.Len(t, blob, 64)

	in := cleanCapture(t)
	plant(t, &in, "bundle.json", []byte("\n"+blob+"\n"))

	got := steelthread.SelfCheck(in)
	f := findByCode(t, got, steelthread.CodeHighEntropyBlob)
	assert.Equal(t, steelthread.SeverityWarn, f.Severity)
	assert.Contains(t, f.Message, "bundle.json")
	assert.NotContains(t, f.Message, blob, "the warning must not reprint what it found")
	assert.False(t, steelthread.HasHardFinding(got),
		"a high-entropy blob is a warning; refusing a valid capture on a guess is how a detector gets disabled")
}

// TestStructuralScan_LongIdentifiersDoNotWarn pins the entropy FLOOR, which is
// the half that decides whether the warning is worth having.
//
// A captured transcript is full of long tool and metric names, and every
// character of one is in the base64 alphabet — so a floor set too low turns
// every capture into a page of warnings, which is the same as no warning at
// all. Measured against a real 528KB capture: of 126 base64-alphabet runs of 40
// characters or more, the highest entropy was 4.27 bits per character. Random
// base64 at that length sits above 5. The floor at 4.5 has headroom on both
// sides, and the names below are what the measured runs look like.
func TestStructuralScan_LongIdentifiersDoNotWarn(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "bundle.json", []byte(
		"\nspicedb_audit_log_stream_version_quick_debug_handler\n"+
			"tigercache_materialize_watched_permissions_collector\n"))

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeHighEntropyBlob, f.Code,
			"an ordinary long identifier must not warn; a detector that warns on every capture is off: %s",
			f.Message)
	}
}

// TestStructuralScan_ReadsEverySurface is the widening itself, stated as a
// test. Four files are written and all four are scanned; before this, three of
// them were not, and bundle.json — where a credential a TOOL returned lives —
// was among the three.
func TestStructuralScan_ReadsEverySurface(t *testing.T) {
	surfaces := []string{"bundle.json", "trace.golden", "03-agent.yaml", "payloads/delivery.json"}

	for _, name := range surfaces {
		t.Run(name, func(t *testing.T) {
			in := cleanCapture(t)
			// trace.golden and the payload are not part of cleanCapture's
			// fixture set, so they are appended as the surfaces they are.
			if name == "trace.golden" || name == "payloads/delivery.json" {
				in.Emitted = append(in.Emitted, steelthread.EmittedFile{Name: name, Bytes: []byte("seed\n")})
			}
			plant(t, &in, name, []byte("\n"+fakeAWSKeyID+"\n"))

			f := findByCode(t, steelthread.SelfCheck(in), steelthread.CodeStructuralSecret)
			assert.Contains(t, f.Message, name)
		})
	}
}

// TestStructuralScan_OneFindingPerFileAndPattern pins the reporting shape. A
// leaked kubeconfig matches on many lines, and a hundred findings saying one
// thing about one file is a report nobody reads — so the count and the first
// few positions carry it instead.
func TestStructuralScan_OneFindingPerFileAndPattern(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "bundle.json", []byte(strings.Repeat("\n"+fakeAWSKeyID+"\n", 4)))

	var n int
	for _, f := range steelthread.SelfCheck(in) {
		if f.Code == steelthread.CodeStructuralSecret {
			n++
			assert.Contains(t, f.Message, "4 times", "the finding must say how many hits it stands for")
			assert.Contains(t, f.Message, "line ", "the finding must say where to look")
		}
	}
	assert.Equal(t, 1, n, "four hits of one pattern in one file are one finding")
}

// TestOpaqueValue is the value-shape rule that decides whether a
// token:/password: FIELD carries credential material or a credential's NAME.
//
// The field-name row is the one pattern in the table keyed on a key rather than
// on a value, so it is the one that can fire on something that is not a
// credential at all — and it did, on a live AgentClass whose toolBundle mapped
// one logical credential name to another. Both directions are pinned here: the
// row must stay silent on names and prose, and must still refuse opaque
// material with no vendor prefix, which no other row in the table catches.
func TestOpaqueValue(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		// Not credentials. Each is a real shape that reaches a captured file.
		{name: "a credentialRemap target: two words naming an identity", value: "github-token", want: false},
		{name: "a longer hyphenated identity name", value: "your-github-personal-access-token", want: false},
		{name: "an env-var name in prose", value: "GITHUB_TOKEN", want: false},
		{name: "a file path a tool printed", value: "/var/run/secrets/service-token", want: false},
		{name: "a UUID, whose last group is twelve digits", value: "550e8400-e29b-41d4-a716-446655440000", want: false},
		{name: "an RFC3339 timestamp", value: "2026-08-27T14:34:00Z", want: false},
		{name: "a session name: words plus a short hex suffix", value: "codebot-fbb69604", want: false},
		{name: "a single long lower-case word", value: "correcthorsebatterystaple", want: false},
		{name: "a dotted host name", value: "api.internal.example.com", want: false},
		{name: "a versioned identifier", value: "my-agent-v2-config", want: false},

		// Credentials. Every one of these is opaque material with no vendor
		// prefix, so no other row in the table would report it.
		{name: "a 32-character lower-case hex digest", value: "3f8a9b2c1d4e5f6a7b8c9d0e1f2a3b4c", want: true},
		{name: "a 16-character hex secret, under every entropy floor", value: "3f8a9b2c1d4e5f6a", want: true},
		{name: "a mixed-case base62 API key", value: "AbC123dEf456GhI789", want: true},
		{name: "a base64 blob", value: "RVhBTVBMRVNFQ1JFVFZBTFVFMDAx", want: true},
		{name: "a password mixing case and digits", value: "S3cur3EXAMPLEP4ssw0rd", want: true},
		{name: "an opaque segment inside an otherwise wordlike value", value: "acct-3f8a9b2c1d4e5f6a", want: true},
		{name: "a base64url token whose separators do not break it up", value: "EXAMPLEonly0Zm9vYmFy-QUJD123def", want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, steelthread.OpaqueValueForTest([]byte(tc.value)))
		})
	}
}

// TestStructuralScan_CredentialNamesAndProseAreNotFindings is the same claim as
// TestOpaqueValue made end to end, through SelfCheck, over the bytes a capture
// really emits.
//
// The first fixture is the exact shape that refused a live capture: an
// AgentClass whose toolBundle remaps a logical credential name onto an identity
// name twelve characters long. The second is prose of the kind a system prompt
// carries — a class that instructs an agent about GitHub personal-access tokens
// discusses them at length, which is precisely why a field-name pattern with no
// opinion about its value is unusable.
func TestStructuralScan_CredentialNamesAndProseAreNotFindings(t *testing.T) {
	in := cleanCapture(t)
	plant(t, &in, "03-agent.yaml", []byte(
		"    credentialRemap:\n      git-token: github-token\n"))
	plant(t, &in, "bundle.json", []byte(
		"Store the value under a key named token: your-github-personal-access-token, "+
			"and never echo the password: never-echo-a-password back to the user.\n"))

	for _, f := range steelthread.SelfCheck(in) {
		assert.NotEqual(t, steelthread.CodeStructuralSecret, f.Code,
			"a credential NAME is not a credential: %s", f.Message)
	}
}

// TestStructuralScan_OpaqueTokenFieldStillRefuses is the other direction, and it
// is why the row was value-gated rather than demoted to a warning.
//
// The planted value is what the field-name row UNIQUELY catches: opaque material
// with no vendor prefix, so no other row recognizes it, and shorter than the
// entropy warning's 40-character floor, so nothing else would even mention it.
// Lower-case hex additionally tops out at 4.0 bits per character, permanently
// below the 4.5 floor — length would not save it either.
func TestStructuralScan_OpaqueTokenFieldStillRefuses(t *testing.T) {
	const planted = "3f8a9b2c1d4e5f6a7b8c9d0e1f2a3b4c"
	in := cleanCapture(t)
	plant(t, &in, "bundle.json", []byte("\n  token: "+planted+"\n"))

	got := steelthread.SelfCheck(in)
	f := findByCode(t, got, steelthread.CodeStructuralSecret)
	assert.Equal(t, steelthread.SeverityHard, f.Severity)
	assert.Contains(t, f.Message, "credential-field")
	assert.NotContains(t, f.Message, planted, "the finding must never reprint the credential it caught")
	assert.True(t, steelthread.HasHardFinding(got))
}

// TestStructuralPatterns_ValueGuardedRowsCaptureTheirValue pins the invariant
// realHits depends on: a row that gates on its captured value must actually
// capture one.
//
// Asserted over the whole table rather than over the single row that has a guard
// today, so a second guarded row added later is covered by a test that already
// exists. realHits fails CLOSED when the group is missing — it reports every
// match rather than none — so losing the group is loud rather than silent; this
// test is what makes it loud at build time instead of at the next capture.
func TestStructuralPatterns_ValueGuardedRowsCaptureTheirValue(t *testing.T) {
	var guarded int
	for _, p := range steelthread.StructuralPatternsForTest() {
		if !p.ValueGated {
			continue
		}
		guarded++
		assert.GreaterOrEqualf(t, p.NumSubexp, 1,
			"pattern %q gates on its captured value but its regexp captures nothing", p.Name)
	}
	require.NotZero(t, guarded,
		"no pattern is value-gated; either the guard was removed or this test asserts nothing")
}
