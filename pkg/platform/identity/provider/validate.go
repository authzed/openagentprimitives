package provider

import (
	"fmt"
	"regexp"
	"strings"
)

// TokenFormatError is returned by ValidateToken when a (trimmed) token does
// not match the provider's declared TokenShape. It carries the provider id
// and a human-readable hint (the TokenShape Description) so every entry point
// — the identityd paste forms, the `oap user-identity put-token` CLI, and the
// builtin setup flows — can render ONE consistent, actionable message instead
// of letting a wrong token sail through and fail opaquely downstream (e.g. a
// 401 the first time the agent actually uses the credential).
type TokenFormatError struct {
	ProviderID string
	Hint       string

	// Matches names the catalog providers whose declared format the rejected
	// value DOES satisfy, in catalog order. Empty when it looks like nothing we
	// know.
	//
	// Naming what a value looks like is what turns "this is wrong" into "you
	// pasted the other one": the two Claude credentials are both sk-ant-
	// prefixed, belong to sibling toolkits, and are not interchangeable, so the
	// expected shape ALONE leaves a reader comparing two near-identical strings.
	Matches []TokenShapeMatch
}

// TokenShapeMatch is one provider whose declared format a rejected value
// matches: the catalog id plus that shape's own human-readable hint.
type TokenShapeMatch struct {
	ProviderID string
	Hint       string
}

func (e *TokenFormatError) Error() string {
	var b strings.Builder
	if e.Hint != "" {
		fmt.Fprintf(&b, "the value does not look like a valid %s token (%s)", e.ProviderID, e.Hint)
	} else {
		fmt.Fprintf(&b, "the value does not look like a valid %s token", e.ProviderID)
	}
	for i, m := range e.Matches {
		if i == 0 {
			b.WriteString("; it matches the declared format of ")
		} else {
			b.WriteString(", ")
		}
		if m.Hint != "" {
			fmt.Fprintf(&b, "%s (%s)", m.ProviderID, m.Hint)
		} else {
			b.WriteString(m.ProviderID)
		}
	}
	return b.String()
}

// ValidateToken checks a pasted/typed token against the provider's declared
// TokenShape, returning nil when it matches OR when the provider declares no
// format. It is the single format gate shared by the paste forms, the
// put-token CLI, and the builtin setup flows, so a credential is validated
// against the SAME declared format wherever it is captured.
//
// Permissiveness is deliberate and load-bearing: a provider with no TokenShape
// (or an empty pattern) is NEVER blocked — never refuse a credential whose
// format we do not know. The token is trimmed before matching (a pasted token
// often carries a trailing newline), but nothing here mutates what the caller
// stores.
//
// A mismatch returns a *TokenFormatError carrying the provider id, hint, and
// the catalog shapes the value DOES match; callers may errors.As it to render a
// tailored message.
func ValidateToken(p Provider, token string) error {
	re, err := compileShape(p)
	if err != nil {
		return err
	}
	if re == nil {
		return nil // no declared format: never blocked
	}
	if !re.MatchString(strings.TrimSpace(token)) {
		return &TokenFormatError{
			ProviderID: p.ID,
			Hint:       p.TokenShape.Description,
			Matches:    matchingShapes(token, p.ID),
		}
	}
	return nil
}

// compileShape returns p's declared format as a regexp, or (nil, nil) when p
// declares none. Shared by the gate and the "what does this look like instead"
// scan so the two cannot disagree about what counts as a declared format.
func compileShape(p Provider) (*regexp.Regexp, error) {
	if p.TokenShape == nil || p.TokenShape.Pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(p.TokenShape.Pattern)
	if err != nil {
		// Embedded providers are compile-validated at load (loader.go fails
		// loudly), so this is only reachable for a caller-constructed Provider
		// with a bad pattern — surface it rather than silently passing the
		// token through unvalidated.
		return nil, fmt.Errorf("provider %q: tokenShape pattern %q does not compile: %w", p.ID, p.TokenShape.Pattern, err)
	}
	return re, nil
}

// matchingShapes returns the embedded providers, other than excludeID, whose
// declared format token satisfies — in catalog order, so the rendering is
// stable.
//
// It deliberately does NOT call ValidateToken: that would recurse, since a
// non-matching candidate builds a TokenFormatError which would scan again.
//
// A candidate whose pattern does not compile is skipped rather than reported.
// Every embedded pattern is compiled at load and a bad one makes loader.go
// panic, so no such candidate can reach here; there is no live error being
// swallowed, only a branch kept total.
func matchingShapes(token, excludeID string) []TokenShapeMatch {
	trimmed := strings.TrimSpace(token)
	var out []TokenShapeMatch
	for _, cand := range All() {
		if cand.ID == excludeID {
			continue
		}
		re, err := compileShape(cand)
		if err != nil || re == nil {
			continue
		}
		if re.MatchString(trimmed) {
			out = append(out, TokenShapeMatch{ProviderID: cand.ID, Hint: cand.TokenShape.Description})
		}
	}
	return out
}

// validateAuthFailureConfig enforces the structural rules for an
// authFailure: block: every StderrPatterns entry must compile as a Go
// regexp, and every HTTPStatuses/ExitCodes entry must be a plausible
// process/HTTP status value. Errors name both the provider id and the
// offending value so a broken embedded provider fails loudly at load
// (loader.go) instead of silently shipping a corroboration rule that can
// never match.
func validateAuthFailureConfig(providerID string, af *AuthFailure) error {
	for _, status := range af.HTTPStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("provider %q: authFailure httpStatuses entry %d out of range (must be 100-599)", providerID, status)
		}
	}
	for _, code := range af.ExitCodes {
		// 0 means the process SUCCEEDED, so declaring it INVERTS the signal
		// rather than widening it: every clean call at that origin would
		// corroborate "credential rejected", decisively so for a provider with no
		// verify: probe. No legitimate declaration does this, so rejecting it
		// outright has no false positives.
		//
		// Exit code 1 is deliberately NOT rejected — see AuthFailure.ExitCodes.
		if code == 0 {
			return fmt.Errorf("provider %q: authFailure exitCodes may not declare 0: 0 means the process SUCCEEDED, so declaring it would make every successful call at that origin corroborate a credential failure", providerID)
		}
		// A POSIX exit status is a byte. Anything outside 1-255 can never match
		// an observed code, so it is a corroboration rule that silently never
		// fires — exactly what this function exists to catch at load.
		if code < 0 || code > 255 {
			return fmt.Errorf("provider %q: authFailure exitCodes entry %d out of range (a process exit status is 0-255, so this could never match)", providerID, code)
		}
	}
	for _, pat := range af.StderrPatterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("provider %q: authFailure stderrPatterns pattern %q does not compile: %w", providerID, pat, err)
		}
		// A pattern matching the empty string matches EVERY stderr, including no
		// stderr at all, so it corroborates every failed tool call regardless of
		// output. No legitimate auth-failure pattern does this.
		//
		// It is the ONLY semantic rule here, deliberately. The real hazard is a
		// pattern the AGENT can provoke (see AuthFailure.StderrPatterns), and
		// anchoring does not fix it: Go's ^/$ bind the whole text without (?m), so
		// requiring full anchors would reject every pattern that works against
		// real multi-line stderr and push authors to `(?s).*x.*`, which passes
		// while matching strictly more. Provokability depends on CLI behavior this
		// catalog cannot inspect, so it stays a human-review property.
		if re.MatchString("") {
			return fmt.Errorf("provider %q: authFailure stderrPatterns pattern %q matches the empty string, so it would corroborate every failed call regardless of output; require text only the provider's auth layer emits", providerID, pat)
		}
	}
	return nil
}

// validateAgentShapedCorroborationHasProbe refuses the one combination in this
// catalog that is dangerous rather than merely weak: an AGENT-SHAPED
// corroboration signal — authFailure.stderrPatterns or authFailure.exitCodes —
// declared on a provider with NO verify: probe.
//
// WHY THE COMBINATION. Both fields are agent-shapable: a sandbox tool's argv is
// a freeform []string the model fills in, CLIs echo those arguments back into
// their own stderr unquoted, and most exit 1 on ANY error. That is tolerable
// while a probe exists, because corroboration is then one input alongside a live
// verdict from the provider. Without a probe it is the SOLE evidence opening a
// credential-entry form (Determine's unverified tier is reached exactly when the
// re-probe was indeterminate) — a phishing primitive the agent triggers on
// demand.
//
// SCOPE: both agent-shaped fields, httpStatuses deliberately NOT. Covering only
// one of stderrPatterns/exitCodes would imply the other is safe; it is not.
// httpStatuses is a different signal class — only the provider's HTTP auth layer
// emits a 401 — and refusing it here would silently disable the ONLY path to a
// credential card for MCP origins, whose flow reports unsupported and so can
// never carry a verify: probe.
//
// KNOWN LIMIT. `p.Verify != nil` is unsound for a provider that also declares a
// builtin:. verify.go short-circuits to the registered flow, so a declarative
// verify: block there is never read and an INERT one satisfies this rule without
// adding any probe. This package cannot see the builtin registry, so the error
// text puts the obligation on the reviewer.
//
// KNOWN LIMIT. "One input alongside a live verdict" assumes the probe returns a
// verdict. credupdate.Determine routes VerifyIndeterminate, VerifyUnsupported
// and VerifyForbidden alike into the corroboration-alone branch, so a probe that
// times out, 5xx's, or is refused hands the decision back to the agent-shaped
// signal — the same hazard, conditioned on a probe failure the agent need not
// cause. A provider WITH a probe would pass this guard today if it declared
// either field. Not closeable here: the probe outcome lives in credupdate.
func validateAgentShapedCorroborationHasProbe(p Provider) error {
	if p.AuthFailure == nil {
		return nil
	}
	// Named in declaration order so the error tells the author exactly which
	// field(s) tripped it, rather than making them re-derive it from a rule
	// that spans two.
	var declared []string
	if len(p.AuthFailure.StderrPatterns) > 0 {
		declared = append(declared, "stderrPatterns")
	}
	if len(p.AuthFailure.ExitCodes) > 0 {
		declared = append(declared, "exitCodes")
	}
	if len(declared) == 0 || p.Verify != nil {
		return nil
	}
	return fmt.Errorf(
		"provider %q: declares authFailure.%s but no verify: probe. "+
			"Without a probe a match is not one signal among several — it is the sole evidence "+
			"that opens a credential-entry form asking a human to re-enter a working credential, so a "+
			"signal the agent can provoke through its own argv is decisive rather than contributing. "+
			"Both fields are argv-shaped: a CLI echoes agent-chosen arguments into its own stderr, and "+
			"most CLIs exit 1 on any error including a bad flag the model chose. "+
			"Relaxing this takes evidence, not a sign-off: either add a verify: probe, or establish by "+
			"execution that this CLI's auth failure cannot be forged from agent-chosen argv and cite that "+
			"run alongside captured matching and non-matching samples. Note that anchoring is not the fix "+
			"— a leading newline in argv forges a (?m)^-anchored program preamble. If this provider "+
			"declares a builtin:, a declarative verify: block is a no-op (the registered flow decides), "+
			"so confirm the flow actually probes rather than reporting unsupported",
		p.ID, strings.Join(declared, " and "))
}
