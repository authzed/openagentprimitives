package browser

// actionkey.go derives the stable, specific key identifying one web_action
// SpiceDB resource — the id a per-action grant (web_action#act) is written on.
// It is the Phase E consequence-acting-gate's E4 task: the key a mutate
// approval is scoped TO, so the same logical action always reuses the same
// grant and a different action never collides with one.
import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// maxKebabNameLen bounds the readable name segment of the key so a long
// accessible name doesn't produce an unwieldy ObjectID. Truncation never
// affects specificity: the hash suffix is computed over the FULL (untruncated)
// kebab name, so two long names that agree on their first maxKebabNameLen
// characters but diverge after it still yield distinct keys.
const maxKebabNameLen = 40

// nonKebabRun matches any run of characters that isn't a lowercase letter or
// digit, so it collapses whitespace, punctuation, and anything else into a
// single "-" rather than leaving the key dependent on exactly which
// non-alphanumeric separator the source text happened to use.
var nonKebabRun = regexp.MustCompile(`[^a-z0-9]+`)

// ActionKey derives the stable, specific key for one acting call from its
// ActionFacts (E3). It is a PURE function of the six stable facts the laya
// spike's normalized_key establishes as sufficient — registrable domain, op,
// role, normalized name, action path, page path — and nothing else: no
// wall-clock, no randomness, no volatile DOM noise (hashed CSS/JS-framework
// classes, csrf/session tokens, query parameters, whitespace/case drift).
// E3's ActionFacts has already dropped all of that: FormAction is URL-path-only
// (no query string survives), Text is already trimmed/collapsed/lowercased,
// and Container/hashed classes never reach this function at all. So the same
// logical action — however the DOM happens to render it on a given load —
// always normalizes to the same ActionFacts and therefore the same key; a
// different action (different name, different page, different site, different
// target path) always normalizes to a different one.
//
// Per ruling E-R1 the key is READABLE-BUT-SPECIFIC, not an opaque hash: this
// key becomes the web_action ObjectID an operator reads on a revalidation
// approval and the record E9's audit trail writes, so a human scanning it
// should recognize the host, the operation, and roughly what was clicked.
// The format is:
//
//	<domain>/<op>/<kebab-name>-<8hex>
//
// - domain  is f.Host with a "www." prefix stripped (the spike's
//
//	urlparse(...).netloc.removeprefix("www.")) — NOT full host canonicalization;
//	that already happened upstream (browserpolicy.CanonicalHost, via
//	Session.CurrentHost) before ActionFacts was built.
//
// - op      is f.Op verbatim (click | type | press | select | submit).
// - kebab-name is f.Text lowercased, with runs of non-alphanumeric characters
//     collapsed to a single "-", and capped at maxKebabNameLen for legibility.
// - 8hex    is the first 8 hex characters (4 bytes) of
//     sha256(domain|op|role|kebab-name|action-path|page-path) — the FULL
//     six-fact tuple, including role, the untruncated action path, and the
//     page path, none of which appear in the readable prefix. This is what
//     gives the key its specificity: two actions that share a domain, op, and
//     name (e.g. the same "confirm seat" label on two different trips, or the
//     same action replayed on a different page of the same flow) still hash to
//     different suffixes because action-path/page-path differ.
//
// The raw key returned here is NOT a legal SpiceDB ObjectID as-is — it
// contains "/" and likely "." (the domain). Turning it into one is the
// caller's job at the grant-write/check site, via
// browserschema.WebActionValueTransforms() (= ["spicedb_escape"]), mirroring
// exactly how Phase C derives web_host's ObjectID from a canonical host.
// ActionKey never escapes; it only produces the raw, human-legible key.
func ActionKey(f ActionFacts) string {
	domain := strings.TrimPrefix(f.Host, "www.")
	name := kebabName(f.Text)
	actionPath := f.FormAction
	pagePath := urlPath(f.PageURL)

	hashInput := strings.Join([]string{domain, f.Op, f.Role, name, actionPath, pagePath}, "|")
	sum := sha256.Sum256([]byte(hashInput))
	suffix := hex.EncodeToString(sum[:])[:8]

	return fmt.Sprintf("%s/%s/%s-%s", domain, f.Op, truncateKebab(name), suffix)
}

// kebabName lowercases text and collapses every run of non-alphanumeric
// characters to a single "-", trimming any leading/trailing "-". Defensive
// lowercasing matches the spike's name = "...".lower().split(), even though
// E3's ActionFacts.Text is already lowercased by the time it reaches here —
// ActionKey stays correct as a pure function of its input regardless of
// caller discipline. An empty result (no alphanumeric content at all, e.g. an
// icon-only control with no accessible name) falls back to the literal
// "action" rather than leaving a bare hyphen or empty segment in the key.
func kebabName(text string) string {
	s := nonKebabRun.ReplaceAllString(strings.ToLower(text), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "action"
	}
	return s
}

// truncateKebab bounds a kebab name to maxKebabNameLen for the READABLE
// prefix only; the hash suffix (computed by the caller over the untruncated
// name) is what keeps two names differing only past the cutoff distinct.
func truncateKebab(s string) string {
	if len(s) <= maxKebabNameLen {
		return s
	}
	return strings.Trim(s[:maxKebabNameLen], "-")
}
