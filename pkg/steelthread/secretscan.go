package steelthread

import (
	"bytes"
	"fmt"
	"math"
	"regexp"
	"strings"
)

// The structural credential scan: what it is FOR, and why it is not the same
// check as the known-value one next to it.
//
// checkSecrets compares the emitted bytes against the live values the capture
// READ — the Secrets the gathered manifests referenced. That check is exact and
// it is the right one for what it covers, but it can only ever find a
// credential somebody registered. The credential that actually reaches a repo
// is the one nobody registered: a kubeconfig a tool printed into its stdout, a
// token pasted into a message, an API key echoed by an upstream server. None of
// those is in any Secret this capture reads, so the known-value scan looks
// straight past them and reports clean.
//
// This scan finds them by SHAPE instead. It needs no registration and no prior
// knowledge, and — unlike the known-value scan — an empty result from it
// honestly means "nothing matching a credential shape is in these bytes"
// rather than "nobody supplied anything to look for". THAT is why it is
// ungated, and the distinction is not a detail: LiveSecrets carries a gate
// (CodeSecretCheckSkipped) precisely because an empty input there is
// indistinguishable from an omission. Gating this one on anything would
// reintroduce the fail-open that gate exists to close.
//
// # Why some patterns warn and one family is excluded outright
//
// A detector that cries wolf gets turned off, and a detector that is turned off
// catches nothing at all. Two deliberate concessions keep this one worth
// leaving on:
//
//   - certificate-authority-data and client-certificate-data are NOT scanned.
//     Both are PUBLIC certificates — the CA bundle a client verifies the API
//     server against, and the client's own certificate, whose secret half is
//     the separate client-key-data field. Making either a finding would refuse
//     every capture of a session that so much as printed a kubeconfig, for
//     material that is public by construction.
//   - High-entropy base64 WARNS and never refuses. A legitimate payload — an
//     embedded image, a diff, a compressed blob, a webhook body — has exactly
//     the statistics of a key, and there is no way to tell them apart from the
//     bytes alone. A warning a human reads costs a moment; a hard finding that
//     refuses a valid capture costs the capture.
//
// # Placeholder values are not credentials
//
// RewriteFixture substitutes live credential material with values that are
// shape-valid on purpose (an AgentIdentity whose stored value fails its
// provider's declared token shape never goes Valid, and the replayed bundle is
// skipped) — so an emitted fixture legitimately contains things like
// "ghx_-fixtureplaceholder" and "unused-by-the-test-provider". Those spell one
// of the markers below exactly so they read as fake to a person AND to a
// scanner; see fixtureMarker. A match containing one is this package's own
// substitution and is not reported. A real credential colliding with these
// literals is not a thing that happens.
//
// # Never print the matched value
//
// Findings are printed to a terminal and land in whatever captured that
// terminal. A check that exists to stop a credential reaching a repo must not
// be the thing that copies one into a log, so every message names the pattern,
// the file and the position — never the bytes. Same rule CodeSecretLeak holds.

// placeholderMarkers are the literals RewriteFixture writes in place of live
// credential material. A structural match containing one is a substitution this
// package made, not a leak.
//
// Derived from the constants the rewrite actually uses rather than transcribed,
// so a new placeholder shape cannot drift away from the scan that has to
// recognize it.
var placeholderMarkers = []string{
	fixtureMarker,
	genericPlaceholderPrefix,
	unusedByTestProviderValue,
	unusedByFakeChannelValue,
}

// structuralPattern is one credential SHAPE the scan recognizes.
type structuralPattern struct {
	// Name is what the finding calls the shape. Stable, because a reader greps
	// for it.
	Name string
	// Re matches the credential. Written to match inside JSON-escaped text as
	// well as raw YAML, because bundle.json carries whole tool results as
	// escaped strings and a pattern anchored to real line structure would see
	// none of them.
	Re *regexp.Regexp
	// Why completes "…, which is <why>" in the finding message.
	Why string
	// Value, when non-nil, decides whether the text of Re's FIRST capture group
	// is credential MATERIAL. A match whose value fails it is not reported.
	//
	// Only one pattern needs this, and the asymmetry is the point — see
	// credential-field's own comment. Every other row keys on a value shape
	// that has no innocent reading, so the match IS the evidence and a second
	// opinion about it would only weaken the row.
	//
	// A pattern that sets this MUST carry a capture group;
	// TestStructuralPatterns_ValueGuardedRowsCaptureTheirValue pins it, and
	// realHits REPORTS a match whose group is missing rather than dropping it,
	// so a regexp edit that loses the group fails loudly instead of switching
	// the row off.
	Value func(value []byte) bool
}

// structuralPatterns is the table. Every entry here is HARD: each names a
// credential family whose shape has no innocent reading. The soft cases —
// entropy, and the public certificate fields — are handled outside it, for the
// reasons the file comment gives.
var structuralPatterns = []structuralPattern{
	{
		Name: "pem-private-key",
		Re:   regexp.MustCompile(`BEGIN (?:[A-Z0-9]{2,10} )*PRIVATE KEY`),
		Why:  "the header of a PEM-encoded private key",
	},
	{
		Name: "jwt",
		Re:   regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*`),
		Why:  "a JSON Web Token: two base64url-encoded JSON segments and a signature",
	},
	{
		Name: "github-token",
		Re:   regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
		Why:  "a GitHub personal-access, OAuth, app or refresh token",
	},
	{
		Name: "aws-access-key-id",
		Re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),
		Why:  "an AWS access-key id (long-lived or STS)",
	},
	{
		Name: "slack-token",
		Re:   regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}`),
		Why:  "a Slack bot, user, app, refresh or legacy token",
	},
	{
		// sk- and sk-ant- both, in one entry: the prefixes nest, so two
		// patterns would report one key twice.
		//
		// The 20-character floor is what keeps this from matching prose. A real
		// key of either family runs to 48 characters or more, while "sk-" turns
		// up in ordinary text; without a floor the pattern fires on a sentence.
		Name: "openai-anthropic-key",
		Re:   regexp.MustCompile(`\bsk-(?:ant-)?[A-Za-z0-9][A-Za-z0-9_-]{19,}`),
		Why:  "an OpenAI- or Anthropic-style API key",
	},
	{
		Name: "google-api-key",
		Re:   regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
		Why:  "a Google API key",
	},
	{
		// The optional \\?" runs throughout this table's key:value patterns so
		// one expression matches `token: x` in a YAML file and `\"token\": \"x\"`
		// inside a JSON-escaped tool result. [ \t] rather than \s deliberately:
		// \s would let the value be found on a later line than its key.
		Name: "authorization-bearer",
		Re: regexp.MustCompile(
			`(?i)authorization\\?"?[ \t]*[:=][ \t]*\\?"?bearer[ \t]+[A-Za-z0-9._~+/=-]{12,}`),
		Why: "an Authorization header carrying a bearer token",
	},
	{
		// Any non-empty value is a finding here, with no length floor: the field
		// holds the base64 of a client PRIVATE key and appears nowhere else in
		// any format. Its public siblings — certificate-authority-data and
		// client-certificate-data — are deliberately absent from this table.
		Name: "kubeconfig-client-key",
		Re:   regexp.MustCompile(`client-key-data\\?"?[ \t]*:[ \t]*\\?"?[^\s"\\,}]`),
		Why:  "a kubeconfig client-key-data field with a value, which is a client private key",
	},
	{
		// The ONE row in this table keyed on a FIELD NAME rather than on a
		// value shape, and the only one that therefore needs a second opinion
		// about what it matched.
		//
		// That asymmetry is structural, not a defect in the regexp. Every other
		// row above recognizes a value that spells out what it is — a PEM
		// header, a vendor prefix, two base64url JSON segments — so the match
		// IS the evidence. "token" and "password" are ordinary words, and the
		// places they appear as a KEY are not all credential stores: a system
		// prompt discussing GitHub PATs, a toolBundle's credentialRemap mapping
		// one logical credential NAME to another, an example in documentation, a
		// tool printing its own help. A captured transcript is full of all four.
		//
		// So the key selects a candidate and Value decides. A length floor alone
		// cannot: `git-token: github-token` clears twelve characters and is two
		// English words naming an identity, while a sixteen-character opaque
		// token is the thing this row exists to catch. See opaqueValue for the
		// rule, and for what it deliberately lets through.
		//
		// The leading character class rather than \b: \b treats the hyphen in
		// "bot-token:" as a boundary, which is correct — that IS a credential
		// field — but it also matches inside "mytoken:", which is a different
		// word. Excluding only the alphanumerics keeps the first and drops the
		// second.
		//
		// The 12-character floor is kept as a cheap prefilter: a value shorter
		// than opaqueSegmentMin cannot contain a segment that long, so the
		// regexp declines it before Value is ever called.
		Name: "credential-field",
		Re: regexp.MustCompile(
			`(?im)(?:^|[^a-z0-9_])(?:token|password)\\?"?[ \t]*:[ \t]*\\?"?([^\s"\\,}]{12,})`),
		Why:   "a token or password field carrying an opaque value",
		Value: opaqueValue,
	},
}

// opaqueValue reports whether v — the value found in a token:/password: field —
// is credential MATERIAL rather than a credential's NAME.
//
// # The rule
//
// v is split on the separators that join words into identifiers and paths, and
// is credential material when any one of the resulting segments is at least
// opaqueSegmentMin characters long AND draws on at least two of the three
// character classes {lower-case, upper-case, digit}.
//
// Both halves are load-bearing, and each was chosen against a case the other
// gets wrong:
//
//   - Requiring the run to be UNBROKEN is what clears `github-token`,
//     `your-github-personal-access-token` and `/var/run/secrets/token`. Their
//     segments are dictionary words; none reaches twelve characters. It is safe
//     for base64 and base64url because a random twelve-character stretch of
//     either alphabet is overwhelmingly unlikely to contain a separator (`-`,
//     `_` and `/` each appear about once in 64 characters), so an encoded
//     credential still presents a long unbroken segment.
//   - Requiring TWO character classes is what clears `446655440000` (a UUID's
//     last group), `27T14` (a timestamp's) and `verylongpathsegment`. A single
//     class is a number, a word, or an all-caps identifier. Opaque credential
//     material — hex, base32, base62, base64 — mixes.
//
// Entropy is NOT used here, and that is a measured conclusion rather than an
// omission. At these lengths entropy is bounded by log2(len): a twelve-character
// value can carry at most 3.58 bits/char, and `github-token` already measures
// 3.42. There is no floor that separates a short random token from a short
// hyphenated name, so the discriminator has to be composition. (The separate
// high-entropy WARNING still runs, on runs of 40 characters or more, where the
// estimate is meaningful.)
//
// # What this deliberately lets through
//
// An all-lower-case passphrase of any length ("correcthorsebattery") is one
// class and is not reported. No value-shape rule can catch it without also
// catching every identifier and every long word in prose, and this row is HARD
// — it refuses the capture — so the false-positive cost is a refused capture an
// operator cannot override. The entropy warning is the backstop.
func opaqueValue(v []byte) bool {
	for _, seg := range bytes.FieldsFunc(v, isIdentifierSeparator) {
		if len(seg) >= opaqueSegmentMin && charClasses(seg) >= 2 {
			return true
		}
	}
	return false
}

// opaqueSegmentMin is the shortest unbroken run opaqueValue will call
// credential material.
//
// Twelve, matching the regexp's own floor: the shortest credential worth
// catching in a token field, and longer than the dictionary words that make up
// the identifiers this rule has to clear ("github", "personal", "credentials").
const opaqueSegmentMin = 12

// isIdentifierSeparator reports whether r joins words in an identifier, a path,
// a timestamp or a host name. Split on these, a name decomposes into words and a
// credential does not.
//
// `+` and `=` are deliberately absent: both are base64 alphabet characters, and
// splitting on them would break an encoded credential into pieces.
func isIdentifierSeparator(r rune) bool {
	switch r {
	case '-', '_', '.', '/', ':':
		return true
	default:
		return false
	}
}

// charClasses counts how many of {lower-case, upper-case, digit} appear in seg.
func charClasses(seg []byte) int {
	var lower, upper, digit bool
	for _, c := range seg {
		switch {
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	n := 0
	for _, present := range [...]bool{lower, upper, digit} {
		if present {
			n++
		}
	}
	return n
}

// entropyMinRun and entropyMinBits bound the high-entropy warning.
//
// Both are measured against a real capture rather than guessed. Over a 528KB
// bundle of live tool output, every base64-alphabet run of 40 characters or
// more topped out at 4.27 bits per character — they are long identifiers, not
// keys. Random base64 at that length sits above 5. The floor at 4.5 leaves
// headroom on both sides.
//
// 40 characters is the shortest run worth reporting: below it the entropy
// estimate is noisy, and no credential family worth catching is shorter without
// also carrying a prefix one of the patterns above already matches.
const (
	entropyMinRun  = 40
	entropyMinBits = 4.5
)

var base64Run = regexp.MustCompile(fmt.Sprintf(`[A-Za-z0-9+/=_-]{%d,}`, entropyMinRun))

// scanStructural reports every emitted file carrying something SHAPED like a
// credential.
//
// One finding per (file, pattern) rather than per occurrence: a leaked
// kubeconfig matches the same pattern on many lines, and a hundred findings
// saying the same thing about one file is a report nobody reads. The count and
// the first few positions are what a reader needs to find it.
func scanStructural(files []EmittedFile, redactionTokens []string) []Finding {
	var out []Finding
	for _, f := range files {
		for _, p := range structuralPatterns {
			hits := realHits(f.Bytes, p)
			if len(hits) == 0 {
				continue
			}
			out = append(out, Finding{
				Severity: SeverityHard,
				Code:     CodeStructuralSecret,
				Message: fmt.Sprintf("emitted file %s matches the %s pattern %s at %s. These files are "+
					"written into a repo, so the capture refuses rather than committing what may be a live "+
					"credential. Remove it at the source, or — if it is not a credential — say so in the "+
					"report; there is deliberately no flag that suppresses this check. (The matched value is "+
					"NOT reprinted here.)",
					f.Name, p.Name, plural(len(hits), "once", "%d times"), positions(f.Bytes, hits)),
			})
		}
		if fs := entropyFinding(f, redactionTokens); fs != nil {
			out = append(out, *fs)
		}
	}
	return out
}

// realHits returns the match offsets that are neither this package's own
// placeholder substitutions (see placeholderMarkers) nor, for a value-guarded
// pattern, a match whose captured value is not credential material (see
// structuralPattern.Value).
//
// Fails CLOSED on a missing capture group: a guarded pattern whose regexp no
// longer captures anything reports every match rather than none. Losing the
// group would otherwise silently switch the row off, which is the failure mode
// this whole file exists to avoid.
func realHits(b []byte, p structuralPattern) [][]int {
	var out [][]int
	for _, m := range p.Re.FindAllSubmatchIndex(b, -1) {
		if isPlaceholder(b[m[0]:m[1]]) {
			continue
		}
		if p.Value != nil && len(m) >= 4 && m[2] >= 0 && !p.Value(b[m[2]:m[3]]) {
			continue
		}
		out = append(out, []int{m[0], m[1]})
	}
	return out
}

func isPlaceholder(match []byte) bool {
	return containsAny(match, placeholderMarkers)
}

func containsAny(b []byte, needles []string) bool {
	s := string(b)
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// entropyFinding warns about base64-alphabet runs whose character distribution
// looks like key material. Warn, never hard — see the file comment.
//
// A run containing one of the capture's own redaction TOKENS is skipped, and
// that exclusion was measured rather than anticipated. Substituting COMPANY-10
// into an identifier like "<vendor>_<customer>_us-east-1" mixes upper case and
// digits into an otherwise lowercase run and lifts it over the floor: a real
// capture went from zero entropy warnings to thirty-two runs purely because it
// was redacted. Every one of those was a name the operator chose, so the
// warning said nothing true, and a warning that fires on every redacted capture
// is the noise this floor exists to avoid.
//
// It applies to the ENTROPY half only, never to the structural patterns, and
// that asymmetry is the point. Entropy is a statistical guess about unknown
// bytes, and a run whose statistics this capture itself changed is not unknown.
// A credential-shaped replacement token is a different question, and the hard
// patterns still answer it — a rule cannot be used to launder a credential past
// the scan.
func entropyFinding(f EmittedFile, redactionTokens []string) *Finding {
	var hits [][]int
	for _, m := range base64Run.FindAllIndex(f.Bytes, -1) {
		run := f.Bytes[m[0]:m[1]]
		if isPlaceholder(run) || containsAny(run, redactionTokens) || shannonBits(run) < entropyMinBits {
			continue
		}
		hits = append(hits, m)
	}
	if len(hits) == 0 {
		return nil
	}
	return &Finding{
		Severity: SeverityWarn,
		Code:     CodeHighEntropyBlob,
		Message: fmt.Sprintf("emitted file %s carries %s of at least %d base64-alphabet characters whose "+
			"character distribution (%.1f bits or more per character) is what key material looks like, at %s. "+
			"A warning rather than a refusal because an embedded image, a compressed blob or a diff has the "+
			"same statistics as a key and nothing in the bytes tells them apart — read it and decide. (The "+
			"value itself is NOT reprinted here.)",
			f.Name, plural(len(hits), "one run", "%d runs"), entropyMinRun, entropyMinBits,
			positions(f.Bytes, hits)),
	}
}

// shannonBits is the Shannon entropy of b in bits per byte.
func shannonBits(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range b {
		freq[c]++
	}
	n := float64(len(b))
	var bits float64
	for _, count := range freq {
		if count == 0 {
			continue
		}
		p := float64(count) / n
		bits -= p * math.Log2(p)
	}
	return bits
}

// positionsShown bounds how many match sites a finding names. Enough to find
// the thing; not so many that the message becomes the leak's own transcript.
const positionsShown = 5

// positions renders match sites as "line L (offset N)", which is what a reader
// needs for both shapes these files take: a YAML manifest where the line is the
// useful coordinate, and bundle.json, where a whole tool result is one escaped
// string on one line and only the offset locates anything.
func positions(b []byte, hits [][]int) string {
	var sb strings.Builder
	for i, m := range hits {
		if i == positionsShown {
			fmt.Fprintf(&sb, " and %d more", len(hits)-positionsShown)
			break
		}
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "line %d (offset %d)", 1+bytes.Count(b[:m[0]], []byte("\n")), m[0])
	}
	return sb.String()
}

// plural renders a count as one of two forms, so a message reads "once" rather
// than "1 times".
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}
