package credupdate

import (
	"regexp"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
)

// defaultHTTPStatuses is what an authFailure: block means when it declares
// HTTPStatuses but leaves it empty. It is 401 ONLY.
//
// An LLM cannot forge a 401 — that requires the provider's own auth layer to
// reject a credential it validated — but it CAN provoke a 403 on demand by
// requesting a resource it knows it isn't entitled to. Manufactured 403
// "corroboration" would let a prompt-injected agent talk a human into
// re-entering a WORKING credential, the phishing vector this gate exists to
// close. 403 counts only when a provider opts in explicitly via httpStatuses.
//
// A function, not a package-level slice: a shared backing array could be widened
// in place by any same-package append or index assignment with no compiler
// signal. A fresh literal per call leaves nothing to mutate.
func defaultHTTPStatuses() []int {
	return []int{401}
}

// Observation is what the platform independently saw a tool call do, reduced
// to the three shapes an AuthFailure block can recognize plus the one fact
// that positively refutes all of them (OriginAuthenticated). It is
// deliberately NOT a ToolCall or an artifact reference — IsAuthShaped must
// stay testable without constructing either.
type Observation struct {
	// HTTPStatus is the response status from an MCP-origin tool call. Zero
	// means "no HTTP status was observed" (a CLI toolkit call, or a call that
	// never got a response at all) — it is never itself a value to match.
	HTTPStatus int

	// ExitCode is a CLI toolkit call's process exit code. Nil means "no exit
	// code was observed" (an MCP-origin call). A non-nil zero IS a real
	// observation (the process exited 0), so this must stay a pointer rather
	// than collapsing "not observed" and "exited 0" onto the same zero value.
	ExitCode *int32

	// Stderr is a CLI toolkit call's captured stderr text. Empty means
	// "nothing was captured" — never itself a value to match.
	Stderr string

	// OriginAuthenticated reports that the origin's own auth layer accepted this
	// call's credential and ran the tool (MCP 200 with a JSON-RPC result, or a
	// CLI process that exited 0). False means "not observed", never
	// "authentication failed".
	//
	// No authFailure: block can declare it; it OVERRIDES every shape above. A
	// call the provider authenticated is not an authentication failure however
	// its output reads — load-bearing because the agent chooses the arguments,
	// and bad arguments produce tool-level errors on demand that must retract a
	// stale observation rather than prop it up.
	//
	// See tool.Result.OriginAuthenticated for the producing end and
	// authfail.Recorder.ObserveFailure for the retraction it drives.
	OriginAuthenticated bool
}

// IsAuthShaped reports whether obs looks like the specific authentication
// failure af describes for this provider. This is where the corroboration gate
// lives: the reconciler calls it only after a live re-probe came back
// indeterminate, and true here promotes an agent's unverified claim into a card
// a human sees (TierUnverified in Determine). Permissiveness here is a phishing
// vector — see defaultHTTPStatuses — so every branch defaults to "did not
// match" and the function returns false unless SOME declared signal matches.
//
// PURE (no I/O, no clock, no k8s) so the whole matching matrix is testable
// without envtest.
func IsAuthShaped(obs Observation, af *provider.AuthFailure) bool {
	// nil means "no corroboration available for this provider" — no declared
	// signal to check against, so there is nothing to corroborate with. Never a
	// default-yes.
	if af == nil {
		return false
	}

	// A call the origin AUTHENTICATED is not an authentication failure, whatever
	// the tool then said. Checked first because every shape below is fallible in
	// this direction: `exitCodes: [1]` or a loose stderr pattern would otherwise
	// match a process that ran fine on arguments the agent chose. Positive
	// evidence beats a pattern match.
	if obs.OriginAuthenticated {
		return false
	}

	if obs.HTTPStatus != 0 && matchesHTTPStatus(obs.HTTPStatus, af.HTTPStatuses) {
		return true
	}

	if obs.ExitCode != nil && containsInt(af.ExitCodes, int(*obs.ExitCode)) {
		return true
	}

	if obs.Stderr != "" && matchesAnyPattern(obs.Stderr, af.StderrPatterns) {
		return true
	}

	return false
}

// matchesHTTPStatus applies the documented default: an authFailure: block
// that declares no httpStatuses at all means 401 ONLY, never every status a
// provider might one day 4xx with.
func matchesHTTPStatus(status int, declared []int) bool {
	if len(declared) == 0 {
		return containsInt(defaultHTTPStatuses(), status)
	}
	return containsInt(declared, status)
}

func containsInt(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// matchesAnyPattern reports whether stderr matches any of patterns.
//
// Patterns are compiled per call, not cached: every StderrPatterns entry is
// already compile-validated at catalog load (provider.validateAuthFailureConfig),
// the lists are tiny, and this runs at most once per credential-update
// determination — a human-latency-bound path, not a hot loop.
//
// An uncompilable pattern (only reachable from a caller-built AuthFailure that
// skipped load-time validation) is SKIPPED, not panicked on: it contributes no
// match, keeping the fail-closed contract.
func matchesAnyPattern(stderr string, patterns []string) bool {
	for _, pat := range patterns {
		re, err := regexp.Compile(pat)
		if err != nil {
			continue
		}
		if re.MatchString(stderr) {
			return true
		}
	}
	return false
}
