package builtins

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// VerifyStatus is the outcome of live token verification. Every switch over it
// carries a default: arm, so a value added here cannot fall open at a consumer
// that has not been taught what it means.
type VerifyStatus string

const (
	// VerifyValid: the provider accepted the token.
	VerifyValid VerifyStatus = "valid"
	// VerifyRejected: the provider's auth layer definitively rejected the token
	// (401). 403 is VerifyForbidden — that refuses the request, not the credential.
	VerifyRejected VerifyStatus = "rejected"
	// VerifyForbidden: the provider authenticated the credential and then refused
	// this particular check (403). Evidence neither way — a live token that is
	// SSO-restricted, org-blocked, or missing a scope answers exactly as a revoked
	// one does. Distinct from Indeterminate because consumers act on it
	// differently: an unattended verdict engine must not act on it, a human at a
	// credential-entry form is told and asked.
	VerifyForbidden VerifyStatus = "forbidden"
	// VerifyIndeterminate: verification was attempted but could not conclude
	// (network error, timeout, rate limit, 5xx, unexpected status, flow error).
	// A soft-warn state. Never blocks.
	VerifyIndeterminate VerifyStatus = "indeterminate"
	// VerifyUnsupported: no live verification is available and none was attempted
	// (no provider, no verifier configured, a builtin with no way to ping).
	// Consumers proceed quietly; it is NOT a warning-worthy state.
	VerifyUnsupported VerifyStatus = "unsupported"
)

// VerifyResult is what a live verification reports back to the entry
// point that is about to store a credential.
type VerifyResult struct {
	Status VerifyStatus
	// Detail is a human-readable outcome. Never empty for
	// Rejected/Forbidden/Indeterminate/Unsupported — no-silent-errors.
	Detail string
	// Subject is the provider-side identity the token authenticated as, when the
	// probe can extract one (e.g. the GitHub login).
	Subject string
	// SubjectID is the provider's STABLE identifier for that identity, when the
	// provider declares a SubjectIDField. Distinct from Subject because the
	// readable name and the durable key are usually different fields — see
	// provider.VerifyConfig.SubjectIDField. Empty when undeclared or unusable.
	SubjectID string
	// ProviderID is the catalog id (provider.Provider.ID) of the provider that
	// ran this check. VerifyCredential stamps it, so a caller holding only the
	// result still knows which id-space SubjectID belongs to.
	//
	// The two travel together because neither is meaningful alone: a stable id
	// names nothing without the namespace it is stable within, and a caller
	// that re-derived the provider from the credential name could name a
	// DIFFERENT provider than the one that actually verified. Empty when no
	// provider resolved.
	ProviderID string
	// Metadata is an extension hook for richer checks (scopes, expiry).
	// Nothing consumes it yet.
	Metadata map[string]string
}

// VerifyRequest is what a Flow.Verify call receives.
type VerifyRequest struct {
	Provider *provider.Provider
	Value    StoreValue // the credential about to be stored
}

// verifyTimeout bounds every live probe so token entry never hangs on a
// slow provider. Timeouts map to VerifyIndeterminate.
const verifyTimeout = 5 * time.Second

// verifyHTTPClient is the client factory for live probes: the SSRF-guarded
// safehttp.Client(). Tests driving a loopback stub must override via
// SetVerifyHTTPClient — safehttp blocks loopback dialing.
var verifyHTTPClient = safehttp.Client

// SetVerifyHTTPClient overrides the probe HTTP-client factory; nil restores the
// default. Test seam.
func SetVerifyHTTPClient(fn func() *http.Client) {
	if fn == nil {
		verifyHTTPClient = safehttp.Client
		return
	}
	verifyHTTPClient = fn
}

// VerifyHTTPBearer runs a provider's declarative bearer-token probe. It never
// returns an error: every failure mode maps onto a VerifyStatus with the
// underlying cause in Detail.
func VerifyHTTPBearer(ctx context.Context, prov *provider.Provider, token string) VerifyResult {
	if prov == nil || prov.Verify == nil || prov.Verify.Endpoint == "" {
		return VerifyResult{Status: VerifyUnsupported, Detail: "no verification probe configured for this provider"}
	}
	label := prov.Title
	if label == "" {
		label = prov.ID
	}

	method := prov.Verify.Method
	if method == "" {
		method = http.MethodGet
	}
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, prov.Verify.Endpoint, nil)
	if err != nil {
		return VerifyResult{Status: VerifyIndeterminate, Detail: "building verification request failed: " + err.Error()}
	}
	scheme := prov.Verify.AuthScheme
	if scheme == "" {
		scheme = "Bearer"
	}
	req.Header.Set("Authorization", scheme+" "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := verifyHTTPClient().Do(req)
	if err != nil {
		return VerifyResult{Status: VerifyIndeterminate, Detail: fmt.Sprintf("could not reach %s: %v", prov.Verify.Endpoint, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	success := prov.Verify.SuccessStatuses
	if len(success) == 0 {
		success = []int{http.StatusOK}
	}
	for _, s := range success {
		if resp.StatusCode == s {
			res := VerifyResult{Status: VerifyValid, Detail: label + " accepted the token"}
			// Decode body once, preserving the literal digit string of JSON numbers
			// via UseNumber() — crucial for stable IDs, where a float64 loss of
			// precision in the decoder silently produces a wrong-but-plausible value.
			var m map[string]any
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			if dec.Decode(&m) == nil {
				// SubjectField expects a string for human display (e.g. "login").
				if f := prov.Verify.SubjectField; f != "" {
					if v, ok := m[f].(string); ok && v != "" {
						res.Subject = v
						res.Detail = "authenticated as " + v
					}
				}
				// SubjectIDField accepts a string or a JSON number for the stable key.
				if f := prov.Verify.SubjectIDField; f != "" {
					res.SubjectID = stableIDString(m[f])
				}
			}
			return res
		}
	}

	hint := bodyMessage(body)
	// GitHub and others answer a rate-limited request with 403, which says nothing
	// about the token. Never reject on a throttle.
	if strings.Contains(strings.ToLower(hint), "rate limit") {
		return VerifyResult{Status: VerifyIndeterminate, Detail: fmt.Sprintf("%s is rate limiting verification (%s); could not verify", label, resp.Status)}
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		// 401 is the one status that speaks about the TOKEN rather than about
		// what the token was asking for, which is why it alone is definitive
		// here — and why the corroboration gate accepts only it (see
		// AuthFailure.HTTPStatuses in pkg/platform/identity/provider/types.go).
		detail := fmt.Sprintf("%s rejected the token: %s", label, resp.Status)
		if hint != "" {
			detail += " — " + hint
		}
		return VerifyResult{Status: VerifyRejected, Detail: detail}

	case http.StatusForbidden:
		// A 403 says the REQUEST was refused, not that the credential died: a
		// live token that is SSO-restricted, org-blocked, or missing a scope
		// answers exactly as a revoked one does. Calling it a rejection would
		// declare a WORKING credential expired and send a human off to replace it
		// (credupdate.Determine promotes a rejection straight to TierVerified, no
		// corroboration required). Calling it Valid would SUPPRESS a legitimate
		// card, since some providers do answer 403 for a revoked token. So it
		// asserts neither — and it is not folded into Indeterminate, because
		// consumers must tell it apart from a timeout and act differently.
		detail := fmt.Sprintf("%s accepted the credential but refused this check", label)
		if hint != "" {
			detail += " — " + hint
		}
		return VerifyResult{Status: VerifyForbidden, Detail: detail}
	}
	return VerifyResult{Status: VerifyIndeterminate, Detail: fmt.Sprintf("unexpected response from %s: %s", prov.Verify.Endpoint, resp.Status)}
}

// ForbiddenNotice puts the VerifyForbidden verdict into words for a human.
//
// It is the single source of that copy — CLI, setup wizard, and both browser
// forms render this one sentence — so no surface can word a refused check as a
// dead credential and send somebody off to replace one that works. The framing
// is stated here rather than trusted from res.Detail, which the flow words
// however it likes and which is appended as the provider's own reason.
func ForbiddenNotice(res VerifyResult) string {
	s := "the credential authenticated but was refused for this check, so it could not be confirmed either way"
	if res.Detail != "" {
		s += " (" + res.Detail + ")"
	}
	return s
}

// UnrecognizedNotice words a VerifyStatus this build has no branch for.
//
// Reaching it means a verdict was added to VerifyStatus without teaching a
// consumer, so it is worded as an unknown rather than as a pass: every caller
// pairs it with a refusal or an explicit human confirmation. It omits the status
// value — an internal identifier the reader can do nothing with — but every
// caller logs or returns the raw detail, so the cause is never lost.
func UnrecognizedNotice(res VerifyResult) string {
	s := "the credential could not be checked and the result was not understood"
	if res.Detail != "" {
		s += " (" + res.Detail + ")"
	}
	return s
}

// bodyMessage extracts a short human hint from a JSON error body's
// "message" field ({"message":"Bad credentials"} → "Bad credentials").
func bodyMessage(body []byte) string {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	if len(m.Message) > 200 {
		return m.Message[:200] + "…"
	}
	return m.Message
}

// stableIDString renders a probe's stable-id field as a string. It accepts a
// JSON string, or a JSON number represented as json.Number (preserving the
// literal digit string).
//
// The guard cannot happen on a decoded float64, because encoding/json's
// float decoder is lossy: 9007199254740993 (2^53+1, not exactly representable
// in IEEE 754 double) decodes to 9007199254740992 before this function is
// ever called. Detecting out-of-range or fractional values requires the
// original digit string, which json.Decoder.UseNumber() preserves as
// json.Number. A fractional or out-of-int64-range value returns "" rather
// than a truncation: a wrong-but-plausible id would bind an authorization
// edge to the wrong principal, which is worse than no edge at all.
//
// Note: Exponent notation like 5.83231e5 is legal JSON but rejected here.
// An id is a digit string; fail closed when a provider sends one in a
// form that loses precision or is non-standard.
func stableIDString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		// ParseInt rejects fractional values, out-of-int64-range values, and
		// exponent forms in one step, returning an error on all of them.
		if _, err := strconv.ParseInt(t.String(), 10, 64); err != nil {
			return ""
		}
		return t.String()
	default:
		return ""
	}
}

// VerifyCredential is the single live-verification choke point every entry point
// calls after the ValidateToken format gate. Resolution:
//
//  1. provider has a registered builtin flow → flow.Verify decides
//  2. no builtin, catalog declares verify: and the value is a bearer → the
//     generic declarative probe runs
//  3. anything else → unsupported
//
// It never returns an error and always populates Detail: a flow error is
// downgraded to indeterminate, because a broken probe must warn, not block.
//
// It is also the one place ProviderID is stamped onto the result. Doing it here
// rather than in each arm means the id a caller reads is always the provider
// that RAN the check, whichever arm produced the verdict.
func VerifyCredential(ctx context.Context, prov *provider.Provider, value StoreValue) VerifyResult {
	if prov == nil {
		return VerifyResult{Status: VerifyUnsupported, Detail: "credential has no associated provider; verification unavailable"}
	}
	res := verifyCredential(ctx, prov, value)
	res.ProviderID = prov.ID
	return res
}

// verifyCredential is VerifyCredential's arm resolution, split out so the
// ProviderID stamp above cannot be forgotten on a future arm.
func verifyCredential(ctx context.Context, prov *provider.Provider, value StoreValue) VerifyResult {
	if prov.Builtin != "" {
		if flow, ok := Get(prov.Builtin); ok {
			res, err := flow.Verify(ctx, VerifyRequest{Provider: prov, Value: value})
			if err != nil {
				return VerifyResult{Status: VerifyIndeterminate, Detail: fmt.Sprintf("verification errored: %v", err)}
			}
			if res.Status == "" {
				res.Status = VerifyIndeterminate
			}
			if res.Status != VerifyValid && res.Detail == "" {
				res.Detail = "provider " + prov.ID + " reported " + string(res.Status) + " with no detail"
			}
			return res
		}
	}
	if prov.Verify != nil && value.Bearer != "" {
		return VerifyHTTPBearer(ctx, prov, value.Bearer)
	}
	return VerifyResult{Status: VerifyUnsupported, Detail: "no verification available for provider " + prov.ID}
}
