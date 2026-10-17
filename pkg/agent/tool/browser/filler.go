package browser

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/directcred"
	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

// filledKinds records which credential field kinds a Fill substituted, so the
// type tool can decide whether a field needs blanking afterward (Phase C Task
// C5). It carries only booleans — never the values.
type filledKinds struct {
	username bool
	password bool
	otp      bool
}

// any reports whether Fill substituted at least one credential value.
func (f filledKinds) any() bool { return f.username || f.password || f.otp }

// CredentialFetcher resolves one catalog entry's credential, by its opaque
// catalog ID, for IMMEDIATE USE. The production implementation (built in
// internal/cmd/runner) resolves through directcred against the identity's
// backing Secret; tests and the e2e harness supply a fake that returns a stub
// and counts calls, so no test path touches a real vault. The returned
// Credential must never be logged or persisted — see directcred.Credential.
type CredentialFetcher func(ctx context.Context, catalogID string) (directcred.Credential, error)

// CredentialFiller resolves {{login.<host>.username|password|otp}} placeholders
// in a to-be-typed string, FAIL-CLOSED per the three §3.5 rules, substituting
// values that never reach the model, the transcript, or any log (it runs strictly
// inside the type tool's Execute, after the loop has already recorded the raw
// placeholder args). An unresolvable placeholder returns a stable §3.4 error —
// never a silent empty, and never the raw secret in the error text.
type CredentialFiller interface {
	// Fill substitutes every login placeholder in text against pageHost, the
	// host the browser is actually on. inputIsPassword reports whether the DOM
	// element the text will be typed into is a type=password input (rule 3). It
	// returns the substituted text and which kinds it filled.
	Fill(ctx context.Context, pageHost string, inputIsPassword bool, text string) (string, filledKinds, error)
}

// placeholderRe matches one credential placeholder: {{login.<host>.<field>}}.
// The host group is any run of non-brace characters (hosts never contain braces);
// the field is anchored to the closed set, so the greedy host group backtracks
// to the last "." before a valid field. CanonicalHost rejects a host the regexp
// over-accepts (spaces, empty labels, a scheme), so the grammar stays permissive
// and the three rules do the real validation.
var placeholderRe = regexp.MustCompile(`\{\{login\.([^{}]+)\.(username|password|otp)\}\}`)

// malformedLoginMarker is the substring that, if it survives in the NON-matched
// gaps of the input, means the model wrote something login-placeholder-SHAPED
// that the strict grammar did not accept (a missing or misspelled field). Typing
// it literally could leak a half-formed credential reference, so Fill fails
// closed on it rather than passing it through.
const malformedLoginMarker = "{{login"

// credFiller is the default CredentialFiller. It owns the catalog and the Fetch
// seam, and reaches the §5.3 hold through its Session so a host's credential is
// Fetched at most once and later placeholders (including .otp's live code)
// resolve from the hold.
type credFiller struct {
	sess    *Session
	catalog []spiceboxv1alpha1.BrowserLoginRef
	fetch   CredentialFetcher
}

var _ CredentialFiller = (*credFiller)(nil)

// newCredFiller builds the default filler. fetch may be nil (a toolbox with no
// bound identity, or a test that never resolves a placeholder) — in that case
// any placeholder that reaches the Fetch step fails closed rather than panicking.
func newCredFiller(sess *Session, catalog []spiceboxv1alpha1.BrowserLoginRef, fetch CredentialFetcher) *credFiller {
	return &credFiller{sess: sess, catalog: catalog, fetch: fetch}
}

// Fill implements CredentialFiller. See the interface doc for the contract.
func (f *credFiller) Fill(ctx context.Context, pageHost string, inputIsPassword bool, text string) (string, filledKinds, error) {
	var kinds filledKinds
	matches := placeholderRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		if strings.Contains(text, malformedLoginMarker) {
			return "", filledKinds{}, fmt.Errorf("malformed login placeholder; use {{login.<host>.username|password|otp}}")
		}
		return text, kinds, nil
	}

	var out strings.Builder
	var gaps strings.Builder // the ORIGINAL text between matches, for the malformed-leftover check
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		host := text[m[2]:m[3]]
		field := text[m[4]:m[5]]

		value, kind, err := f.resolve(ctx, pageHost, inputIsPassword, host, field)
		if err != nil {
			// Fail the WHOLE fill closed: never a partially-substituted string,
			// never the raw placeholder typed alongside a real secret.
			return "", filledKinds{}, err
		}

		gap := text[last:start]
		out.WriteString(gap)
		out.WriteString(value)
		gaps.WriteString(gap)
		last = end

		switch kind {
		case "username":
			kinds.username = true
		case "password":
			kinds.password = true
		case "otp":
			kinds.otp = true
		}
	}
	tail := text[last:]
	out.WriteString(tail)
	gaps.WriteString(tail)

	// A login-shaped leftover in the non-matched gaps is a malformed placeholder
	// (checked over the ORIGINAL gap text, so a substituted value that happens to
	// contain the marker can never trip it). Fail closed.
	if strings.Contains(gaps.String(), malformedLoginMarker) {
		return "", filledKinds{}, fmt.Errorf("malformed login placeholder; use {{login.<host>.username|password|otp}}")
	}
	return out.String(), kinds, nil
}

// resolve enforces the three §3.5 rules for one placeholder and returns its
// value and kind, all fail-closed. Rules 1-3 are checked BEFORE any Fetch, so a
// placeholder that violates one never reaches the vault.
func (f *credFiller) resolve(ctx context.Context, pageHost string, inputIsPassword bool, rawHost, field string) (string, string, error) {
	host, err := browserpolicy.CanonicalHost(rawHost)
	if err != nil {
		return "", "", fmt.Errorf("login placeholder host %q is not a valid hostname", rawHost)
	}

	// Rule 1: the placeholder host must be the host the page is actually on
	// (equality or a subdomain). A placeholder for aa.example typed on a lookalike
	// — or on no page at all — matches nothing and resolves to an error, never the
	// secret.
	if pageHost == "" || !browserpolicy.Match(browserpolicy.Site{Host: host}, pageHost) {
		return "", "", fmt.Errorf("login placeholder for %q does not match the current page host; open the site with navigate first", host)
	}

	// Rule 2: the host must have a catalog entry in THIS toolbox's identity.
	entry, ok := f.catalogEntryFor(host)
	if !ok {
		return "", "", fmt.Errorf("no stored login for host %q in this browser toolbox", host)
	}

	// Rule 3: a password may only land in a type=password input.
	if field == "password" && !inputIsPassword {
		return "", "", fmt.Errorf("a password can only be typed into a password input field")
	}

	cred, err := f.credentialFor(ctx, host, entry.ID)
	if err != nil {
		// The directcred sources (first-party) do not put credential values in
		// their errors; wrapping is safe and keeps the no-silent-errors contract.
		return "", "", fmt.Errorf("resolving the stored login for host %q: %w", host, err)
	}

	switch field {
	case "username":
		return cred.Username, "username", nil
	case "password":
		return cred.Password, "password", nil
	case "otp":
		if cred.OTP == nil {
			return "", "", fmt.Errorf("the stored login for host %q has no one-time code configured", host)
		}
		code, oerr := cred.OTP()
		if oerr != nil {
			// Deliberately NOT wrapped: a TOTP/HOTP generation error is about the
			// seed or clock, but we refuse to risk echoing anything value-adjacent.
			return "", "", fmt.Errorf("could not generate the one-time code for host %q", host)
		}
		return code, "otp", nil
	default:
		// Unreachable: the grammar's field group is the closed set above.
		return "", "", fmt.Errorf("unknown login field %q", field)
	}
}

// catalogEntryFor returns the catalog entry whose hosts cover host (via the same
// suffix-by-label match the egress policy uses), and whether one was found.
func (f *credFiller) catalogEntryFor(host string) (spiceboxv1alpha1.BrowserLoginRef, bool) {
	for _, e := range f.catalog {
		for _, h := range e.Hosts {
			ch, err := browserpolicy.CanonicalHost(h.Host)
			if err != nil {
				continue
			}
			if browserpolicy.Match(browserpolicy.Site{Host: ch, Exact: h.Exact}, host) {
				return e, true
			}
		}
	}
	return spiceboxv1alpha1.BrowserLoginRef{}, false
}

// credentialFor returns host's credential from the §5.3 hold, Fetching it (once)
// and placing it in the hold on a miss. The held Credential carries the live OTP
// closure, so repeated .otp placeholders produce fresh codes without re-Fetching.
func (f *credFiller) credentialFor(ctx context.Context, host, catalogID string) (directcred.Credential, error) {
	if cred, ok := f.sess.heldCredential(host); ok {
		return cred, nil
	}
	if f.fetch == nil {
		return directcred.Credential{}, fmt.Errorf("no credential source is configured for this browser toolbox")
	}
	cred, err := f.fetch(ctx, catalogID)
	if err != nil {
		return directcred.Credential{}, err
	}
	f.sess.putHeldCredential(host, cred)
	return cred, nil
}
