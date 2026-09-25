// Package refresh runs RFC 6749 refresh-token grants against an oauth
// credential's stored token_endpoint, writing the new access_token /
// refresh_token / expires_at back to the Secret atomically. Failures
// preserve the existing Secret values.
//
// Redemptions are single-flighted on the identity of the UPSTREAM
// CREDENTIAL and the write-back is optimistically locked; see
// redemptions and writeBack for why both layers are needed.
package refresh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// defaultClient is the http.Client used for token-endpoint requests. It
// is SSRF-guarded: the token_endpoint is read from a Secret whose value
// can be influenced by remote OAuth discovery, so the dialer refuses
// private/loopback/link-local destinations. Tests override via
// SetHTTPClient.
var defaultClient = safehttp.Client()

// maxTokenResponseBytes bounds the token-endpoint response we buffer. The
// endpoint is as remote-influenced as the URL defaultClient guards, so an
// unbounded read there hands that same input a memory-exhaustion lever. 1 MiB
// is far above any real response — even one carrying several large JWTs — and
// far below anything that hurts.
const maxTokenResponseBytes = 1 << 20

// ErrDecodeResponse signals that the token endpoint returned a 2xx
// response body that we could not parse as a valid token response.
// Callers (controllers, the CLI) can match this with errors.Is to
// distinguish parse failures from network or HTTP-status errors.
var ErrDecodeResponse = errors.New("refresh: decode response")

// SetHTTPClient overrides the HTTP client (test seam).
func SetHTTPClient(c *http.Client) { defaultClient = c }

// MaterialSecretName returns the name of the SIBLING Secret that may hold a
// credential's RFC 6749 REDEMPTION MATERIAL — token_endpoint, client_id,
// client_secret — next to the Secret holding its tokens.
//
// It is a separate object because Kubernetes RBAC has no per-key granularity: a
// `get` on a Secret returns every key in it. A userPassthrough session's runner
// must read the token Secret, so co-locating the redemption material there would
// hand the least-trusted process in the system a self-contained, offline-usable
// refresh grant for the user's upstream account — long-lived, usable after the
// session is gone, unaffected by broker invalidation, invisible to toolguard,
// the audit log and SpiceDB. The split is what makes the runner's grant
// expressible as "may read the access token, may not redeem it".
//
// The name is a lookup hint, not proof: see MaterialSecretLabel.
func MaterialSecretName(tokenSecretName string) string {
	return tokenSecretName + "-refresh"
}

// MaterialSecretLabel marks a Secret as redemption material for the credential
// it is named after. It is what makes the lookup safe, because credential names
// are free-form: an AgentIdentity carrying both "x" and "x-refresh" produces
// master Secrets whose names are exactly MaterialSecretName's input and output,
// so a name-only lookup would redeem x's refresh token using x-refresh's client
// — authenticating as the wrong OAuth client. A Secret at the derived name that
// does not carry this label is ignored, and a writer must refuse to overwrite
// one (see pkg/platform/identity/useridentity).
const MaterialSecretLabel = "agentprimitives.authzed.com/oauth-refresh-material"

// redemptionMaterial resolves the endpoint + client identity a refresh is
// redeemed with, SIBLING-FIRST with a fallback to sec's own keys.
//
// The fallback applies to exactly one condition — the sibling is genuinely
// ABSENT — which is the co-located shape used by AgentIdentity credentials and
// IdP-identity Secrets.
//
// FAIL-CLOSED: a sibling read that fails for any OTHER reason is refused, never
// degraded to the fallback. A userPassthrough runner holds `get` on the master
// Secrets by name and nothing else, so its Get of the derived name comes back
// FORBIDDEN, not NotFound; falling back would hand it the co-located keys the
// split exists to deny — and since the same Role denies it the write-back, it
// would consume a rotating refresh token upstream, fail to persist the
// replacement, and wedge the credential permanently. A caller denied the
// redemption material is denied the write-back too. Transient failures are
// refused for the same reason in weaker form: a retry costs a reconcile, a
// burned refresh token costs a re-link.
//
// A Secret at the derived name WITHOUT MaterialSecretLabel is some other
// credential's; it is ignored (its client would authenticate as the wrong
// client) and the co-located fallback still applies — this credential has no
// sibling.
func redemptionMaterial(ctx context.Context, c client.Reader, key types.NamespacedName,
	sec *corev1.Secret) (endpoint, clientID, clientSecret string, err error) {

	var sib corev1.Secret
	sibKey := types.NamespacedName{Namespace: key.Namespace, Name: MaterialSecretName(key.Name)}
	switch gerr := c.Get(ctx, sibKey, &sib); {
	case gerr == nil:
		if _, ok := sib.Labels[MaterialSecretLabel]; !ok {
			// Some other credential's Secret happens to sit at this name; break
			// to the co-located fallback, which is correct because this
			// credential has no sibling. Redeeming with the impostor's client
			// would authenticate as the wrong client.
			log.FromContext(ctx).Info("refresh: Secret at the refresh-material name is not refresh material; ignoring it",
				"namespace", sibKey.Namespace, "secret", sibKey.Name)
			break
		}
		return string(sib.Data["token_endpoint"]), string(sib.Data["client_id"]), string(sib.Data["client_secret"]), nil
	case apierrors.IsNotFound(gerr):
		// Expected for a co-located credential; not an error condition.
	default:
		return "", "", "", fmt.Errorf(
			"refresh: refresh-material Secret %s/%s is unreadable (%w); refusing to redeem from "+
				"co-located keys — a caller denied the redemption material is denied the write-back "+
				"too, and would consume a refresh token it cannot persist",
			sibKey.Namespace, sibKey.Name, gerr)
	}
	return string(sec.Data["token_endpoint"]), string(sec.Data["client_id"]), string(sec.Data["client_secret"]), nil
}

// redemptions collapses CONCURRENT redemptions of one upstream credential
// into a single token-endpoint round trip.
//
// What must not be redeemed twice is neither this process nor the Secret object
// — it is the upstream credential. Presenting one refresh token twice is a
// reuse: a rotating-refresh-token provider answers the second with
// invalid_grant, and one implementing RFC 6819 §5.2.2.3 reuse detection revokes
// the whole token family, killing the credential until a manual re-link. So the
// flight is keyed on the CREDENTIAL's identity (credentialKey), not on
// namespace/secretName: two Runs against disjoint Secrets carrying the same
// credential must still collapse to one redemption.
//
// Only the HTTP round trip runs inside the flight; each caller then writes the
// shared result into its own Secret. One redemption, N write-backs — so no
// Secret is left holding a refresh token the provider has retired.
//
// singleflight drops the key on completion, so nothing grows unboundedly, and a
// later Run reads the ROTATED token and computes a different key. The flight
// body runs on the leader's goroutine with the leader's context, so a cancelled
// leader fails its followers; they surface the error and retry.
//
// Residual, deliberately not closed: a caller that read its Secret before a
// flight completed but calls Do after it finished computes the old key and
// re-presents the consumed token. Closing it needs a bounded post-flight memo
// holding live token material in memory, or a coordinated durable store. The
// optimistic-locked write-back bounds the damage meanwhile — the stale writer
// can no longer clobber fresher material.
var redemptions singleflight.Group

// credentialKey is the flight key: the RFC 6749 identity of a redemption —
// what is being redeemed, at whom, as whom. client_secret is deliberately
// excluded because it is an authenticator, not an identity; including it
// would split the flight for a public/confidential pair of copies of one
// credential, which is exactly the pair that most needs collapsing.
//
// The key is a hash so that no credential material reaches a map key, a log
// line, or a panic dump.
func credentialKey(endpoint, clientID, refreshToken string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + clientID + "\x00" + refreshToken))
	return hex.EncodeToString(sum[:])
}

// tokenResponse is the subset of the RFC 6749 token-endpoint response we
// persist.
type tokenResponse struct {
	// SECRET: the new bearer token. Empty is a protocol error — post rejects it.
	AccessToken string `json:"access_token"`
	// SECRET: the rotated refresh token; empty means the provider did not rotate,
	// so the stored one stays valid and is kept.
	RefreshToken string `json:"refresh_token"`
	// Lifetime of AccessToken in seconds; 0 or absent means "never expires" per
	// RFC 6749, which clears any stored expires_at rather than keeping a stale one.
	ExpiresIn int `json:"expires_in"`
	// Token scheme the provider issued under ("Bearer"); read but not persisted.
	TokenType string `json:"token_type"`
}

// Run performs an RFC 6749 refresh against cred's stored token_endpoint
// using its stored refresh_token + client_id (+ client_secret, when the
// Secret carries one — confidential clients require it). On success,
// writes new access_token, refresh_token (if returned), and expires_at
// back to the Secret. On failure, returns the error and leaves the
// Secret unchanged.
//
// Run returns nil — without having written anything — when a peer refreshed
// the same credential concurrently and won the write-back race. That is
// correct for every caller because none consumes a value from Run: each
// re-reads the Secret afterwards and so observes the peer's fresher
// material. See writeBack.
func Run(ctx context.Context, c client.Client, namespace string,
	cred spiceboxv1alpha1.AgentCredential) error {
	k, err := credkindregistry.Get(cred.Type)
	if err != nil {
		return fmt.Errorf("refresh: %q: %w", cred.Name, err)
	}
	if !k.NeedsRefresh() || cred.OAuth == nil {
		return fmt.Errorf("refresh: %q does not need refresh (type=%s)", cred.Name, cred.Type)
	}
	key := types.NamespacedName{Namespace: namespace, Name: cred.OAuth.SecretRef.Name}
	var sec corev1.Secret
	if err := c.Get(ctx, key, &sec); err != nil {
		return fmt.Errorf("refresh: get Secret: %w", err)
	}

	rt := string(sec.Data["refresh_token"])
	endpoint, clientID, clientSecret, err := redemptionMaterial(ctx, c, key, &sec)
	if err != nil {
		return err
	}
	if rt == "" || endpoint == "" {
		return fmt.Errorf("refresh: %q missing refresh_token, or no readable token_endpoint "+
			"in %s/%s or its refresh-material Secret %s", cred.Name, key.Namespace, key.Name,
			MaterialSecretName(key.Name))
	}
	// Captured before the round trip: the write-back uses it to tell "a peer
	// already refreshed this credential" apart from "the Secret changed for
	// an unrelated reason".
	priorAccessToken := string(sec.Data["access_token"])

	tok, err := redeem(ctx, credentialKey(endpoint, clientID, rt), endpoint, clientID, clientSecret, rt)
	if err != nil {
		return err
	}
	return writeBack(ctx, c, key, cred.Name, &sec, priorAccessToken, tok)
}

// redeem runs the token-endpoint round trip under the shared flight for this
// upstream credential, so N concurrent callers produce exactly one POST.
func redeem(ctx context.Context, flightKey, endpoint, clientID, clientSecret, refreshToken string) (tokenResponse, error) {
	v, err, _ := redemptions.Do(flightKey, func() (any, error) {
		return post(ctx, endpoint, clientID, clientSecret, refreshToken)
	})
	if err != nil {
		return tokenResponse{}, err
	}
	tok, ok := v.(tokenResponse)
	if !ok {
		// Unreachable unless post's signature drifts; fail loudly rather
		// than write a zero-valued token over a live credential.
		return tokenResponse{}, fmt.Errorf("refresh: internal: redemption returned %T, want tokenResponse", v)
	}
	return tok, nil
}

// post performs the RFC 6749 refresh_token grant. It is the only part of a
// refresh that talks to the provider, and the only part that runs inside the
// single-flight.
func post(ctx context.Context, endpoint, clientID, clientSecret, refreshToken string) (tokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if clientID != "" {
		form.Set("client_id", clientID)
	}
	// Confidential clients (e.g. HubSpot's MCP auth apps) reject the
	// refresh_token grant with invalid_client unless the client_secret
	// is presented. Public (PKCE-only) clients store no client_secret,
	// so the key is omitted entirely for them.
	if clientSecret != "" {
		form.Set("client_secret", clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("refresh: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := defaultClient.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("refresh: POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes))
	if err != nil {
		return tokenResponse{}, fmt.Errorf("refresh: read %s response: %w", endpoint, err)
	}
	if resp.StatusCode/100 != 2 {
		return tokenResponse{}, fmt.Errorf("refresh: %s returned %d: %s", endpoint, resp.StatusCode, truncate(string(body), 200))
	}
	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return tokenResponse{}, fmt.Errorf("%w: %w", ErrDecodeResponse, err)
	}
	if tok.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("refresh: response missing access_token")
	}
	return tok, nil
}

// writeBack persists a redeemed token into sec, under an optimistic lock.
//
// The lock covers the cross-process half of the race, which no in-process guard
// can see: the runner's broker JIT-refreshes the same master Secrets the
// operator's reconcilers do, and every runner pod for one user refreshes at once
// when a shared credential expires. A precondition-free merge patch can never
// 409, so the slower writer would silently overwrite the faster writer's NEWER
// refresh token with its own consumed one, wedging the credential permanently.
//
// On conflict, reconcile against INTENT rather than re-redeeming: a blind
// RetryOnConflict would re-read the Secret, find the peer's brand-new refresh
// token, and redeem THAT — manufacturing the second redemption this package
// exists to prevent.
func writeBack(ctx context.Context, c client.Client, key types.NamespacedName, credName string,
	sec *corev1.Secret, priorAccessToken string, tok tokenResponse) error {
	// apply mutates only the keys we own; every other key on the Secret
	// (token_endpoint, client_id, client_secret, scope) is left untouched.
	apply := func(s *corev1.Secret) {
		if s.Data == nil {
			s.Data = map[string][]byte{}
		}
		s.Data["access_token"] = []byte(tok.AccessToken)
		if tok.RefreshToken != "" {
			s.Data["refresh_token"] = []byte(tok.RefreshToken)
		}
		if tok.ExpiresIn > 0 {
			s.Data["expires_at"] = []byte(time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).UTC().Format(time.RFC3339))
		} else {
			// Server omitted expires_in → "never expires" per RFC 6749.
			// Clear any stale value from a prior refresh; otherwise credresolve
			// would still gate on the old timestamp and fail with ErrExpired.
			delete(s.Data, "expires_at")
		}
	}

	original := sec.DeepCopy()
	apply(sec)
	err := c.Patch(ctx, sec, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	if err == nil {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return fmt.Errorf("refresh: patch Secret: %w", err)
	}

	// Someone else wrote this Secret between our read and our write.
	var fresh corev1.Secret
	if gerr := c.Get(ctx, key, &fresh); gerr != nil {
		return fmt.Errorf("refresh: re-read Secret after write-back conflict: %w", gerr)
	}
	if string(fresh.Data["access_token"]) != priorAccessToken {
		// A peer refreshed this credential concurrently. The caller's intent
		// — "make this credential fresh" — is already satisfied, and its
		// material is newer than ours, so writing ours would move the
		// credential BACKWARDS onto a refresh token the provider may have
		// already retired. Leave it alone and report success; every caller
		// re-reads the Secret and will observe the peer's material.
		log.FromContext(ctx).Info(
			"refresh: peer refreshed this credential concurrently; keeping the peer's newer material",
			"namespace", key.Namespace, "secret", key.Name, "credential", credName)
		return nil
	}

	// The Secret changed for some unrelated reason (a label, an annotation).
	// Our material is still the freshest, so re-apply it once against the
	// current resourceVersion. A second conflict is returned rather than
	// retried, so a hot-looping writer cannot spin us here.
	freshOriginal := fresh.DeepCopy()
	apply(&fresh)
	if perr := c.Patch(ctx, &fresh, client.MergeFromWithOptions(freshOriginal, client.MergeFromWithOptimisticLock{})); perr != nil {
		return fmt.Errorf("refresh: patch Secret after write-back conflict: %w", perr)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
