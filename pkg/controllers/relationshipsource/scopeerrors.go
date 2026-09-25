// pkg/controllers/relationshipsource/scopeerrors.go
//
// Turning a Pass's non-fatal per-scope failures into something an operator
// can SEE — a count, a bounded sample, and the PartialFailure condition —
// without breaking the one invariant this controller cannot afford to lose:
// a reconcile that observed nothing new must not write status.
//
// relsync.Pass is right to treat a single scope's failure as non-fatal, and
// Ready keeps saying exactly what it said before. What was missing is that
// NOTHING downstream could tell a partially-failed pass from a clean one: a
// GitHub source whose every per-repository team fetch answered 403 reported
// Ready=True, Synced, scopesProcessed: 156, and the 403s reached only the
// operator log.
//
// Two hazards shape everything below, and they pull in opposite directions.
//
//  1. STATUS MUST NOT GROW WITH THE FAILURE. The pass that most wants to be
//     reported is the one where an entire arm of the sync failed, which is
//     also the one with the most errors. 156 failures must not become 156
//     status entries, so the sample is capped and each message is bounded.
//
//  2. STATUS MUST NOT CHURN. The controller's self-watch has no predicate,
//     so any field that differs on two identical passes makes every
//     reconcile a status write that re-enqueues itself and re-runs the
//     upstream Pass forever — see applySyncResult's own doc and
//     FinishedAt's treatment there. Error text is the worst possible
//     carrier for that hazard, because it is written by someone else: the
//     GitHub kind's rate-limit error carries a REMAINING BACKOFF that
//     rateLimitErrorFor computes with time.Until when it CONSTRUCTS the
//     error, so every pass builds a different one from the same upstream
//     reset timestamp and Error() then renders it ("retry after 59m12.4s").
//     A source that naively persisted that text would rewrite status on
//     every pass and hammer the API it was just told to back off from.
//     Sorting and scrubbing are necessary here but not sufficient;
//     preserveStableSamples is the part that actually closes it.
package relationshipsource

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/relsync"
)

// maxScopeErrorSamples bounds how many scope errors land on status. Small on
// purpose: the sample exists to say WHAT KIND of failure this is, and an
// operator chasing the full list has the controller log (every scope error is
// logged, every pass) and `oap directory` behind it. See hazard 1 above.
const maxScopeErrorSamples = 5

// crdScopeMaxLength and crdMessageMaxLength MIRROR the
// +kubebuilder:validation:MaxLength markers on RelationshipSourceScopeError's
// two fields. Kubernetes measures maxLength in RUNES, and a status write that
// exceeds one is rejected WHOLE — not trimmed. That rejection is not a lost
// field, it is `Status().Update` returning an error, Reconcile returning it,
// and controller-runtime retrying with backoff, each retry re-running
// relsync.Pass against the upstream: the exact runaway this file exists to
// prevent, reached from the other end. Every value below must stay at or under
// the mirrored bound, and TestScopeErrorSamplesFitTheCRDBounds asserts it.
const (
	crdScopeMaxLength   = 256
	crdMessageMaxLength = 512
)

// maxScopeErrorMessageLen and maxScopeErrorScopeLen bound what we EMIT, in
// runes, INCLUDING truncateRunes's own marker. Both are strictly under the
// mirrored CRD bound above, so truncation is always our choice at a readable
// boundary and never the API server's rejection of the whole write.
//
// The message limit is far tighter than its field allows on purpose (an
// upstream answering with an HTML error page must not put its page in a CR);
// the scope limit sits just under its field's, because a scope id is an
// upstream identifier we do not get to shorten meaningfully — the headroom is
// there so the two can never meet exactly.
const (
	maxScopeErrorMessageLen = 256
	maxScopeErrorScopeLen   = crdScopeMaxLength - 1
)

// redactedQuery replaces a URL's query string wherever one appears in error
// text. See scrubScopeErrorMessage.
const redactedQuery = "<redacted>"

// urlWithQueryPattern matches an http(s) URL that carries a query string OR a
// fragment, up to the first "?" or "#", so the replacement can keep the path
// and drop the rest.
//
// The fragment half is here because it is the one credential-shaped exploit a
// query-only pattern misses: the OAuth implicit flow returns its token in the
// fragment (`…/callback#access_token=…`), so an error quoting a redirect target
// would carry a live token.
//
// This pattern requires a LOWERCASE scheme and that is fine, because it is not
// the only pass: an uppercase one still gets its query redacted by
// queryLikePattern below. An earlier version of this comment argued uppercase
// away as unreachable — `url.URL.String()` lowercases a scheme — and the
// argument was sound about strings a KIND builds and irrelevant to the ones an
// operator types. Nothing here rests on that reasoning any more;
// TestReconcile_HostileTenantInputReachesNeitherSink drives each shape through
// a real Reconcile instead.
var urlWithQueryPattern = regexp.MustCompile(`https?://[^\s?#]*[?#][^\s]*`)

// queryLikePattern catches a query or fragment that is NOT hanging off a
// well-formed http(s) URL — anything from a "?" or "#" through the next
// whitespace, required to contain a "=" so it is a parameter list rather than
// prose punctuation.
//
// It exists because scheme-anchoring the pattern above left the most direct
// route wide open, and a test found it rather than a reading did.
// spec.baseURL is tenant-writable and is NOT required to be a well-formed
// absolute URL; credhost.Check's "names no host" branch quotes whatever was
// written, in full; and `/Groups?access_token=…` matches nothing
// scheme-anchored. That string reached the Ready condition AND the monitoring
// Summary unredacted — the anchored pattern was protecting the shape a kind
// builds for itself while missing the shape an operator types.
//
// Requiring the "=" is what keeps this from eating ordinary prose ("what
// now?"), and it costs only a bare valueless query, which carries no
// parameter to leak.
//
// TWO consequences of being this broad, both deliberate, both ASSERTED in
// TestReconcile_HostileTenantInputReachesNeitherSink rather than argued here:
//
//   - It redacts harmless diagnostic queries too (`?status=403&reason=…`).
//     Accepted: this text reaches a chat channel, and a message that reads
//     worse is a smaller cost than a credential that travels further.
//   - It does NOT redact a parameter separated from its "?" by a literal
//     space (`?  token=…`). Loosening it to "any key=value anywhere" would
//     strip the substance out of nearly every message this controller
//     produces, and the shape requires an operator to type a space into
//     their own destination, breaking their own sync.
var queryLikePattern = regexp.MustCompile(`[?#][^\s]*=[^\s]*`)

// urlUserinfoPattern matches the "user:password@" userinfo segment of an
// http(s) URL — the OTHER place in a URL a credential can ride.
var urlUserinfoPattern = regexp.MustCompile(`(https?://)[^\s/@]*@`)

// queryTrailingPunctuation is the set of characters trimmed back off the end
// of a redacted query string. A query match runs to the next whitespace, so
// in `GET https://host/p?a=b: unexpected status 403` it swallows the sentence
// punctuation too; putting it back keeps the message readable without
// teaching the pattern to parse prose.
const queryTrailingPunctuation = `.,;:)]}"'>`

// scrubScopeErrorMessage prepares one upstream error string to leave this
// process — whether it leaves as status, as console output, or as a chat
// message. Every such path goes through it:
//
//   - the sampled scope errors here;
//   - the three Ready=False condition messages in controller.go built from an
//     error (credential resolution, a fatal Pass, a failed enumeration);
//   - the two Ready=False messages built from spec.kind (unregistered,
//     claimed), which is free text an operator typed: MinLength=1, no pattern,
//     no enum;
//   - the three MonitoringEvent.Summary strings this controller composes
//     ITSELF (scopeFailedSummary, publishCredResolveFailed, publishKindClaimed).
//
// The list is spelled out because an earlier version of this comment claimed
// "every path" while covering only the first two groups, and the omission was
// the half that reaches furthest. A Summary composed here goes straight to
// every role=monitoring Channel without passing through a condition, so
// scrubbing the condition did nothing for it: credhost.Check quotes the full
// raw destination when it refuses a credential, and a spec.baseURL carrying a
// token in its query string was scrubbed into status and broadcast verbatim to
// chat on the same reconcile.
//
// Naming all of them matters because none of them stop at the CR. Status is
// readable by anything with `get relationshipsources`; the admin console's
// read-only view_config path renders it; and, since RelationshipSource joined
// pkg/controllers/monitoring's Targets, the watcher ALSO copies a condition's
// Message into a MonitoringEvent.Summary of its own. A condition message is now
// chat traffic, and it was not before. Adding a path without this call is the
// regression to watch for.
//
// The text is quoted verbatim by the kinds (`github: GET %s: unexpected status
// %d`) and by credhost, so it routinely carries a URL — or something an
// operator typed INTO spec.baseURL that was meant to be one. Both are where a
// credential rides: in a query parameter (`?access_token=…`, `?sig=…`), in a
// fragment (the OAuth implicit flow's `#access_token=…`), or in userinfo
// (`https://x:token@host`). All are removed STRUCTURALLY rather than by looking
// for token-shaped substrings: the point is that no query or fragment reaches
// status, not that the ones we thought of don't.
//
// Two passes over the query/fragment case, not one, and the second is the
// important one: an http(s)-anchored pattern only ever protected the URL shape
// a KIND builds for itself, and the shortest route to this function is a
// tenant-writable spec.baseURL that is not a well-formed URL at all. See
// queryLikePattern.
//
// Whitespace is collapsed and the result bounded for the same reason the
// sample count is: an upstream that answers with an HTML error page must not
// be able to put its page into a CR.
func scrubScopeErrorMessage(msg string) string {
	msg = urlUserinfoPattern.ReplaceAllString(msg, "${1}"+redactedQuery+"@")
	// Anchored first so the path before the "?" survives, then unanchored for
	// everything that was never a well-formed URL. The second pass cannot
	// re-match the first's output: "?<redacted>" contains no "=".
	msg = redactFromSeparator(urlWithQueryPattern, msg)
	msg = redactFromSeparator(queryLikePattern, msg)
	msg = strings.Join(strings.Fields(msg), " ")
	return truncateRunes(msg, maxScopeErrorMessageLen)
}

// redactFromSeparator replaces everything after the first "?" or "#" in each
// match of pat, keeping what came before it.
//
// The separator is kept as it was found, so a reader can still tell a query
// from a fragment. Trailing sentence punctuation is put back because a match
// runs to the next whitespace and therefore swallows it — in `GET
// https://host/p?a=b: unexpected status 403` the ":" belongs to the sentence,
// not the query, and restoring it keeps the message readable without teaching
// either pattern to parse prose.
func redactFromSeparator(pat *regexp.Regexp, msg string) string {
	return pat.ReplaceAllStringFunc(msg, func(m string) string {
		i := strings.IndexAny(m, "?#")
		base, sep, rest := m[:i], m[i:i+1], m[i+1:]
		trailing := ""
		for len(rest) > 0 && strings.ContainsRune(queryTrailingPunctuation, rune(rest[len(rest)-1])) {
			trailing = string(rest[len(rest)-1]) + trailing
			rest = rest[:len(rest)-1]
		}
		return base + sep + redactedQuery + trailing
	})
}

// truncateRunes clips s so the RESULT is at most max runes — the marker it
// appends counts toward the budget, so a caller can hand it a CRD maxLength
// and get back something the API server will accept.
//
// That the marker counts is the whole contract, and it is easy to get wrong:
// clipping to max and THEN appending emits max+1, which for a bound the caller
// took from a maxLength marker is one rune over, and one rune over is the API
// server rejecting the entire status write.
//
// Rune-wise, not byte-wise, for the same family of reason: a byte cut lands
// mid-codepoint and the invalid UTF-8 costs the whole write too.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 0 {
		return ""
	}
	return string(r[:max-1]) + "…"
}

// buildScopeErrorSamples renders errs into the bounded, deterministically
// ordered sample that lands on status.
//
// Sorted by (scope, message) and only THEN truncated, so which errors get
// sampled is a property of the failure set rather than of the order
// relsync.Pass happened to append them in — a map-ordered enumeration, a
// concurrent fetch, or a future reordering inside Pass must not be able to
// rotate the sample and rewrite status.
func buildScopeErrorSamples(errs []relsync.ScopeError) []v1.RelationshipSourceScopeError {
	if len(errs) == 0 {
		return nil
	}
	out := make([]v1.RelationshipSourceScopeError, 0, len(errs))
	for _, se := range errs {
		msg := ""
		if se.Err != nil {
			msg = scrubScopeErrorMessage(se.Err.Error())
		}
		out = append(out, v1.RelationshipSourceScopeError{
			Scope:   truncateRunes(string(se.Scope), maxScopeErrorScopeLen),
			Message: msg,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].Message < out[j].Message
	})
	if len(out) > maxScopeErrorSamples {
		out = out[:maxScopeErrorSamples]
	}
	return out
}

// preserveStableSamples is the churn guard, and the reason sorting and
// scrubbing alone are not enough.
//
// The sample is an OBSERVATION whose defining inputs are "how many scopes
// failed" and "which ones we sampled". Its message text is a RENDERING of
// that same failure, and a rendering is free to differ between two passes
// that observed the identical thing — the GitHub kind's rate-limit error
// carries a backoff that rateLimitErrorFor computes with time.Until at
// CONSTRUCTION, so two consecutive throttled passes build two different
// durations from one upstream reset timestamp and Error() renders each
// (`retry after 59m12.4s`). Persisting those bytes would move status on every
// pass, and the self-watch has no predicate, so the source would re-enqueue
// and re-poll an upstream that had just asked it to wait.
//
// So: when the failure count and the sampled scope identities are unchanged,
// the STORED sample is kept and the freshly-rendered one discarded. Anything
// an operator would call news — a different number of failures, a different
// set of scopes failing, the failures clearing — changes the key and the
// sample refreshes with it.
//
// TWO trades, both named rather than hidden, and both bounded by the same
// fact: what survives here was TRUE of some pass and is still true of THIS
// one, because the key that preserved it includes the sampled scope ids. The
// sample never names a scope that is not currently failing.
//
//  1. A scope that keeps failing under the same id while its error text
//     genuinely changes (403 becoming 500) keeps showing the older text until
//     the count or the sampled set moves.
//
//  2. A SAME-SIZE rotation below the cap does not move status: with ten
//     failures and five sampled, the five unsampled ones can change identity
//     entirely and the key is unchanged, because the count and the sampled
//     ids both held. An operator sees a truthful sample of a failure set
//     whose membership has partly turned over without the CR saying so. The
//     count still says ten, which is the number that drives the alert.
//
// Both are accepted for the same reason: the operator log carries every scope
// error on every pass (Reconcile logs them unconditionally), and a stale or
// partial sample is a far smaller fault than an unbounded reconcile loop
// against an upstream that is already failing.
func preserveStableSamples(prior *v1.RelationshipSourcePassStats, count int32, fresh []v1.RelationshipSourceScopeError) []v1.RelationshipSourceScopeError {
	if prior == nil || prior.ScopeErrors != count || len(prior.ScopeErrorSamples) != len(fresh) {
		return fresh
	}
	for i := range fresh {
		if prior.ScopeErrorSamples[i].Scope != fresh[i].Scope {
			return fresh
		}
	}
	return prior.ScopeErrorSamples
}

// enumerationFailedMessage is the Ready=False message for a pass that
// enumerated nothing and processed nothing.
//
// It is not simply the first error quoted, because this branch is where a
// RATE-LIMITED enumeration lands — relsync folds the kind's error into
// ScopeErrors and reports EnumComplete=false, Processed=0 — and a rate-limit
// error carries a backoff its kind computes fresh on every pass. Quoting that
// text moves requeue's own DeepEqual on every reconcile; the self-watch has no
// predicate, so the write re-enqueues immediately and the source re-polls an
// upstream that had just asked it to wait, at whatever rate the reconcile loop
// can manage rather than at the failure cadence. That is the same runaway
// preserveStableSamples closes for the sample, arriving through the failure
// path instead.
//
// A rate limit is therefore reported STRUCTURALLY — recognized with the
// existing RetryAfter interface via errors.As, the same way retryAfterFrom
// recognizes it, never by matching message text — and its own rendering is
// dropped. Everything else is quoted (scrubbed), because everything else on
// this path is deterministic text: a revoked token, a missing scope, a DNS
// failure.
func enumerationFailedMessage(errs []relsync.ScopeError) string {
	if len(errs) == 0 {
		return "enumeration produced no scopes and nothing was synced this pass"
	}
	if _, rateLimited := retryAfterFrom(errs); rateLimited {
		return "enumeration was rate limited upstream and nothing was synced this pass"
	}
	return "enumeration failed and nothing was synced this pass: " +
		scrubScopeErrorMessage(errs[0].Err.Error())
}

// partialFailureMessage is the PartialFailure condition's message.
//
// Built from the COUNT and the (already stability-gated) samples, never from
// freshly-rendered error text: a condition message is part of status and is
// compared by the same DeepEqual, so a message quoting a live error would
// reintroduce the churn preserveStableSamples just closed.
func partialFailureMessage(count int32, samples []v1.RelationshipSourceScopeError) string {
	if count == 0 {
		return ""
	}
	msg := fmt.Sprintf("%d scope(s) failed this pass; the pass was otherwise applied", count)
	if len(samples) > 0 {
		first := samples[0]
		where := "source-level"
		if first.Scope != "" {
			where = "scope " + first.Scope
		}
		msg += fmt.Sprintf(". First (%s): %s", where, first.Message)
	}
	if int(count) > len(samples) {
		msg += fmt.Sprintf(" (%d of %d errors sampled on status.sync.lastPass.scopeErrorSamples)",
			len(samples), count)
	}
	return msg
}
