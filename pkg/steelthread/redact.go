package steelthread

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

// Redaction is one human-named replacement applied to the FINAL bytes of every
// emitted file, before the secret scans run over them.
//
// Explicit and human-supplied, never guessed. The identifiers a real session
// carries that a repo should not — a customer's name, an internal service, a
// partner company — are ordinary words in ordinary positions, and no heuristic
// can tell one from the surrounding prose without either missing most of them
// or redacting the scenario away. So the capture does not try: a person who
// knows what the session was about names the strings, and the capture applies
// exactly those.
//
// # A replacement is the SAME BYTE LENGTH as what it replaces
//
// Not a style rule and not advice: a length-changing replacement is REFUSED.
//
// The substitution is textual and knows nothing about what the text MEANS, but
// the bundle also records values DERIVED from that text — an artifact's size, a
// content length — and those are recomputed at replay from the redacted bytes
// while the recorded number was computed from the original. Measured on a real
// capture: an owner handle appearing three times inside a rendered artifact,
// replaced by a token five bytes shorter, moved the artifact's size from 5291
// to 5276 and the replay diverged. Fifteen bytes; three occurrences, five bytes
// each.
//
// What makes that worth refusing rather than warning about is WHERE it
// surfaces. The divergence is reported against the artifact step, many turns
// from the rule that caused it, with nothing in the message naming a redaction;
// and a capture whose length change happens not to be counted today starts
// diverging the day some new derived count is recorded. So the length is
// checked at the earliest point both strings are in hand — see ParseRedaction —
// and an operator who does not want to count bytes writes the original alone
// and gets a same-length stand-in generated for them.
//
// BYTES, not characters. The recorded counts are byte counts, so a UTF-8
// original is matched by its encoded length: a three-rune name occupying nine
// bytes takes a nine-byte replacement, and a generated one is nine ASCII
// characters. That trades rune count for byte count deliberately, because byte
// count is what the bundle records.
//
// # Choose a replacement that is legal where the original was
//
// A redacted value is still a value the replay will run through whatever
// validated it the first time. Measured on the same capture: replacing a
// lowercase cluster identifier with COMPANY-10 made the replayed toolspec
// refuse the call — its constraint requires a slug of [a-z0-9_-] — and the
// bundle diverged at that step. The same rules with lowercase tokens replayed
// exactly as far as the unredacted capture did.
//
// So match the original's own shape: case and character set. Nothing here can
// check that for you, because the constraint lives in a toolspec, a URL, or an
// upstream API this package never sees — which is the other reason a generated
// token is lowercase alphanumeric, the character set legal in the most
// positions.
type Redaction struct {
	// Old is the literal string to remove. Never recorded in the bundle.
	Old string
	// New is what replaces it — the token a reader will find in the emitted
	// files, and the only half of the pair the bundle records.
	//
	// EMPTY means "generate one": the operator named a string to remove and
	// left the stand-in to the capture. ResolveRedactions fills it in, and
	// nothing downstream may see an empty New, because applying one would
	// delete the original instead of replacing it.
	New string
}

// ParseRedaction reads one `old=new` rule, or a bare `old` whose stand-in the
// capture generates.
//
// Split on the FIRST `=`, so a replacement token may contain one and an
// original may not. That asymmetry is the right way round: the original is a
// name the operator is removing (a company, a hostname, a person), and the
// replacement is a token they invent, which is where a `=` would plausibly turn
// up.
//
// A spec with NO `=` at all is the generated form. The separator is a promise
// that a replacement follows, so `old=` — a promise not kept, which is what a
// truncated line or an interrupted edit looks like — stays an error, while
// `old` alone is the deliberate request for a stand-in.
//
// Four rules are refused rather than accepted-and-ignored, each because
// accepting it produces a capture that looks redacted and is not:
//
//   - An EMPTY original matches at every byte offset in every file, so the
//     replacement would be interleaved through the whole bundle.
//   - An EMPTY replacement after an explicit `=` deletes the original silently.
//     Deletion is not obviously wrong, but a token is strictly better: the
//     reader of a redacted transcript needs to see that something stood there,
//     and a bundle with a hole reads as a session that never mentioned
//     anything. Naming the stand-in is also what makes Capture.Redactions worth
//     recording.
//   - A replacement CONTAINING the original puts back what the rule removed,
//     leaving the operator believing a value is gone when every occurrence is
//     still there, spelled slightly differently.
//   - A replacement of a DIFFERENT BYTE LENGTH desynchronises every recorded
//     value derived from a byte count. See Redaction's own doc for the
//     measurement; the error names the exact length required, because the
//     operator is one edit away from a rule that works.
func ParseRedaction(spec string) (Redaction, error) {
	old, replacement, found := strings.Cut(spec, "=")
	if old == "" {
		return Redaction{}, fmt.Errorf("steelthread: redaction %q has an empty original; "+
			"an empty string matches at every offset of every emitted file", spec)
	}
	if !found {
		// The generated form. Everything ParseRedaction checks about a supplied
		// replacement, ResolveRedactions checks about a generated one — at the
		// point it knows the whole rule set, which is what uniqueness needs.
		return Redaction{Old: old}, nil
	}
	if replacement == "" {
		return Redaction{}, fmt.Errorf("steelthread: redaction %q has an empty replacement; "+
			"name the stand-in token (old=COMPANY-A), or write the original alone (%q) to have a "+
			"same-length one generated. Deleting the value would leave a reader of the bundle unable to "+
			"see that anything stood there", spec, old)
	}
	if strings.Contains(replacement, old) {
		return Redaction{}, fmt.Errorf("steelthread: redaction %q replaces a value with a string that still "+
			"contains it, so every occurrence would survive the substitution", spec)
	}
	if len(replacement) != len(old) {
		return Redaction{}, lengthMismatchError(old, replacement)
	}
	return Redaction{Old: old, New: replacement}, nil
}

// lengthMismatchError is the one message both the parser and ResolveRedactions
// raise, so a programmatic caller that skipped the parser hears exactly what an
// operator does.
func lengthMismatchError(old, replacement string) error {
	return standInLengthError(redactionStandIn.what, old, replacement)
}

// standInLengthError is the length refusal in the form both flags raise it.
//
// One message rather than two, for the reason the generator is one engine: the
// rule is identical, the measurement behind it is identical, and two messages
// would drift into describing two rules. Only the noun changes, so only the noun
// is a parameter.
func standInLengthError(what, old, replacement string) error {
	return fmt.Errorf("steelthread: the %s replacing with %q is %d byte(s) where the value it "+
		"replaces is %d; a length change silently desynchronises every recorded value derived from a byte "+
		"count (an artifact's size, a content length), and the replay then diverges at that value rather "+
		"than at the %s. Give a replacement of exactly %d byte(s), or drop the =%s and write the "+
		"original alone to have a same-length stand-in generated",
		what, replacement, len(replacement), len(old), what, len(old), replacement)
}

// ParseRedactionFile reads one rule per line, in either form.
//
// Blank lines and `#` comments are skipped so an operator can keep an annotated
// list of the identifiers a given agent's sessions carry, and reuse it across
// captures — which is the point of the file form over repeating flags.
//
// A malformed line is an ERROR naming its line number, never a skip. The whole
// file exists to remove things; silently ignoring one line means shipping the
// value that line was written to remove.
func ParseRedactionFile(content string) ([]Redaction, error) {
	var out []Redaction
	sc := bufio.NewScanner(strings.NewReader(content))
	// A redaction file is a short list of names; the default 64KiB token limit
	// is ample, but a line longer than it would otherwise come back as a scan
	// error handled below rather than as a silent truncation.
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		r, err := ParseRedaction(text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("steelthread: read redactions: %w", err)
	}
	return out, nil
}

// standInStem describes one generated-token LAYOUT: the word a token leads with
// and the hash domain its digest is drawn from.
//
// Two flags generate same-length stand-ins — --redact over arbitrary prose and
// --elide-skill over a repo authority — and they share one engine rather than
// owning a generator each. The engine is the part that is hard to get right
// (determinism, collision-freedom, the widen-before-truncate ordering); the
// layout is the only thing that genuinely differs between them, so the layout is
// all a caller supplies.
//
// The DOMAIN differs per layout so two layouts can never derive the same digest
// from the same original. Nothing today would break if they did — the stem words
// already differ — but a token meaning "a company stood here" and a token
// meaning "a repo stood here" should be different strings by construction.
type standInStem struct {
	// word is what the token leads with, as far as the length allows, so a
	// reader of a committed fixture can tell at a glance that the value was
	// removed rather than observed.
	//
	// Lowercase, because the token has to be legal where the original was and
	// lowercase alphanumeric is the character set the most positions accept — a
	// DNS label, a slug, a URL path segment, a Kubernetes object name. An
	// upper-case stand-in is what made a replayed toolspec refuse its own call
	// once already.
	word string
	// domain separates this layout's digests from every other layout's.
	domain string
	// what names the rule set in the exhaustion error, so an operator reads
	// about the flag they typed rather than about a package internal.
	what string
	// accept, when non-nil, is an extra admissibility test every candidate must
	// pass. It exists for a layout whose token is not merely text but a value
	// some parser reads back — see elisionStandIn.
	accept func(string) bool
}

// redactionStandIn is --redact's layout: a bare word plus digest, which is what
// a replaced company name, hostname or handle should look like.
var redactionStandIn = standInStem{
	word:   "redacted",
	domain: "steelthread-redaction",
	what:   "redaction",
}

// generateSaltAttempts bounds the re-hashing at one digest width before the
// generator widens the digest instead. Two same-length originals colliding is
// already improbable; needing 64 tries at one width means the width itself is
// too narrow, and widening is what actually fixes that.
const generateSaltAttempts = 64

// ResolveRedactions fills in a stand-in for every rule that did not name one,
// and validates the ones that did.
//
// Idempotent: a fully-supplied rule set comes back unchanged, so a caller that
// resolves early in order to report what it will do can hand the result to
// Capture, which resolves again for the callers that did not.
//
// # The four properties a generated token holds at once
//
// SAME BYTE LENGTH, which is the whole reason this exists — see Redaction.
//
// DETERMINISTIC: the token is a function of the original, the rule set, and
// nothing else. No clock, no randomness, no map iteration. Re-capturing the
// same session with the same rules produces byte-identical files, because a
// bundle whose stand-ins churned on every capture would make every re-capture a
// diff nobody can read.
//
// COLLISION-FREE across the rule set, by construction rather than by
// probability. A digest prefix short enough to fit a short original is short
// enough to collide, and a collision would silently merge two identities into
// one token — the reader of the fixture would see one company where the session
// named two. So a candidate already taken is rejected and the generator tries
// again, and when it runs out of tries it says so rather than reusing a token.
//
// LEGIBLE, as far as the length allows. The token leads with as much of
// "redacted" as fits and spends the rest on the digest, so a twelve-byte
// original yields "redacted" plus four hex characters and a reader is in no
// doubt. Below four bytes there is no room for both, and the digest wins: a
// token nobody recognises as a placeholder is a cosmetic problem, and two
// identities sharing a token is a correctness one.
//
// # What a candidate is checked against
//
// Every replacement already taken — supplied or generated — and every rule's
// ORIGINAL. The second is not fussiness: applyRedactions runs the rules in
// order over the same text, so a later rule whose original appears inside an
// earlier rule's stand-in would rewrite part of that stand-in. Refusing such a
// candidate keeps the emitted token the token the bundle records.
//
// Two SUPPLIED replacements are allowed to be equal. That is the operator
// deliberately mapping two spellings of one name onto one stand-in, and it is
// their call to make; only generated tokens are held to uniqueness, because
// only there is the collision an accident nobody chose.
func ResolveRedactions(rules []Redaction) ([]Redaction, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	out := make([]Redaction, len(rules))
	copy(out, rules)

	olds := make([]string, 0, len(out))
	for _, r := range out {
		if r.Old == "" {
			return nil, fmt.Errorf("steelthread: a redaction has an empty original; " +
				"an empty string matches at every offset of every emitted file")
		}
		olds = append(olds, r.Old)
	}

	// Supplied replacements are claimed FIRST, so a generated token can never
	// take one of them. Order matters here and nowhere else: claiming before
	// generating is what makes the outcome independent of where in the list the
	// generated rules happen to sit.
	taken := make(map[string]bool, len(out))
	for _, r := range out {
		if r.New == "" {
			continue
		}
		if strings.Contains(r.New, r.Old) {
			return nil, fmt.Errorf("steelthread: the redaction replacing with %q still contains the value it "+
				"replaces, so every occurrence would survive the substitution", r.New)
		}
		if len(r.New) != len(r.Old) {
			return nil, lengthMismatchError(r.Old, r.New)
		}
		taken[r.New] = true
	}

	for i, r := range out {
		if r.New != "" {
			continue
		}
		tok, err := generateReplacement(r.Old, taken, olds)
		if err != nil {
			return nil, err
		}
		taken[tok] = true
		out[i].New = tok
	}
	return out, nil
}

// generateReplacement returns --redact's stand-in for old.
//
// A thin naming of the shared engine, kept so the redaction path reads in its
// own vocabulary at its call site.
func generateReplacement(old string, taken map[string]bool, olds []string) (string, error) {
	return generateStandIn(old, taken, olds, redactionStandIn)
}

// generateStandIn returns a stand-in for old under one layout: legible where it
// fits, unique against taken and olds always, admissible to the layout's own
// parser always, and exactly len(old) bytes.
//
// Two nested dimensions, widened in the order that costs legibility last. The
// digest starts at the narrowest width that still leaves room for the whole stem
// word and grows only when every salt at the current width is rejected — so the
// common case keeps the word intact, and a genuinely crowded rule set trades
// word characters for digest characters one at a time.
func generateStandIn(old string, taken map[string]bool, olds []string, s standInStem) (string, error) {
	n := len(old)
	base := n - len(s.word)
	if base < 1 {
		// At least one digest character, always: without it every original of a
		// given length would produce the same prefix of the word, which is the
		// collision this exists to prevent.
		base = 1
	}
	for width := base; width <= n; width++ {
		for salt := 0; salt < generateSaltAttempts; salt++ {
			tok := buildToken(old, width, salt, s)
			// containsAny is the secret scan's own helper, reused rather than
			// re-spelled: the question is identical and there is no second
			// answer to keep in step.
			if taken[tok] || containsAny([]byte(tok), olds) {
				continue
			}
			// Checked LAST, because it is the only test that rejects on the
			// token's own SHAPE rather than on what else is in the rule set: a
			// layout whose word ends mid-separator can produce a candidate its
			// own parser refuses, and widening is what recovers from that.
			if s.accept != nil && !s.accept(tok) {
				continue
			}
			return tok, nil
		}
	}
	return "", fmt.Errorf("steelthread: could not generate a %d-byte stand-in that is unique across the "+
		"%s set and free of every original in it; at this length there are too few distinct tokens. "+
		"Name the replacement explicitly (old=new, exactly %d byte(s))", n, s.what, n)
}

// buildToken lays out one candidate: as much of the stem word as the requested
// digest width leaves, then the digest.
func buildToken(old string, width, salt int, s standInStem) string {
	n := len(old)
	if width > n {
		width = n
	}
	wordLen := n - width
	if wordLen > len(s.word) {
		wordLen = len(s.word)
		width = n - wordLen
	}
	return s.word[:wordLen] + digestChars(old, salt, width, s.domain)
}

// digestChars returns want lowercase hex characters derived from old, salt and
// the layout's hash domain.
//
// Blocked rather than a single hash, so an original longer than one digest still
// gets a full-length token instead of running off the end of it.
func digestChars(old string, salt, want int, domain string) string {
	if want <= 0 {
		return ""
	}
	var b strings.Builder
	for block := 0; b.Len() < want; block++ {
		sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%s", domain, salt, block, old))
		b.WriteString(hex.EncodeToString(sum[:]))
	}
	return b.String()[:want]
}

// applyRedactions rewrites b under every rule in order and reports how many
// occurrences each one replaced.
//
// In ORDER, and each rule sees the previous rule's output. Two rules whose
// originals overlap therefore compose predictably — the earlier one wins the
// shared text — rather than depending on a sort this function invented. The
// returned counts are per-rule and parallel to rules, including zeros.
//
// Every rule must already be resolved; an unresolved one would delete its
// original rather than replace it. emit refuses before reaching here.
func applyRedactions(b []byte, rules []Redaction) ([]byte, []int) {
	counts := make([]int, len(rules))
	if len(rules) == 0 {
		return b, counts
	}
	s := string(b)
	for i, r := range rules {
		counts[i] = strings.Count(s, r.Old)
		if counts[i] == 0 {
			continue
		}
		s = strings.ReplaceAll(s, r.Old, r.New)
	}
	return []byte(s), counts
}

// redactionRecord turns per-rule hit counts into what the bundle records:
// replacement tokens and counts, never originals. See bt.Capture.Redactions.
func redactionRecord(rules []Redaction, counts []int) []bt.Redaction {
	if len(rules) == 0 {
		return nil
	}
	out := make([]bt.Redaction, len(rules))
	for i, r := range rules {
		out[i] = bt.Redaction{Replacement: r.New, Count: counts[i]}
	}
	return out
}
