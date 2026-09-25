//go:build e2e

package artifact_test

import (
	"context"
	"errors"
	"github.com/authzed/openagentprimitives/test/e2e"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/memory"
	artifactkind "github.com/authzed/openagentprimitives/pkg/memory/kinds/artifact"
	"github.com/authzed/openagentprimitives/pkg/memory/spicedbauthorizer"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/platform/identityd"
	"github.com/authzed/openagentprimitives/pkg/web/webui"
	"github.com/authzed/openagentprimitives/pkg/web/webui/artifactview"
)

// TestArtifactViewHTTP locks down webd's artifact live-view HTTP authorization
// path end to end against the real SpiceDB + composed schema. The full path is:
//
//	browser GET /artifact-view?d=&sig= (no cookie)
//	  → webd 302 → identityd /oidc/login (trust-link fallback)
//	  → identityd mints the idd_session cookie from the link Subject
//	  → 302 back to /artifact-view WITH the cookie
//	  → webd verifies the cookie, runs CheckArtifactView, renders (200)/denies.
//
// Three recent production bugs lived in this chain, so this covers the happy
// path AND the failure modes:
//
//  1. Full happy path: a cookie-less GET follows the redirect chain. This
//     identityd is built with InsecureTrustLinks (no cluster IdP, no
//     authenticator), so it mints a cookie from the link Subject without any
//     IdP or channel-kind OIDC. The participant — who holds artifact#view via
//     the written artifact#parent edge — gets a 200 with the rendered shell.
//     Proves "trust-link fallback → valid cookie" + "artifact#parent →
//     CheckArtifactView true" together. A real install without that door
//     refuses the link instead; sign-in policy is identityd's own tests.
//  2. Stranger denied: a directly-minted cookie for a subject who is NOT a
//     session participant gets a 403 from CheckArtifactView.
//  3. Raw-email cookie → 500: a cookie whose Subject is the RAW email (not the
//     canonical base64) makes CheckArtifactView error on the invalid SpiceDB
//     object-id ('@'/'.' are rejected) — the regression guard for the
//     canonicalization bug. This documents exactly why the link Subject MUST be
//     canonical.
//  4. Bad/expired link → 403: garbage d/sig with a valid participant cookie
//     fails VerifyLink and 403s before any authz check.
//
// webd + identityd are composed on ONE httptest host (production rehosts
// identityd on webd's trusted origin) so the real redirect chain runs through
// the real webui.Server + real identityd.Server. All three signing roles
// (channelsd link-minter, identityd cookie-minter, webd verifier) share ONE
// HMAC key; iss/aud are pinned per role exactly as production wires them.
//
// AgentDir applies the leakage-e2e bundle only to trigger Guardian's schema
// composition (the embedded base schema carries agentsession + artifact); the
// agent itself is never driven. All names are fake per AGENTS.md.
func TestArtifactViewHTTP(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-leakage-e2e"),
		DefaultTimeout: 30 * time.Second,
	})
	h.MCP.OnTool("get_record", func(_ map[string]any) any { return map[string]any{"id": "x"} })
	h.MCP.OnTool("get_record_uuid", func(_ map[string]any) any { return map[string]any{"uuid": "u"} })
	h.MCP.OnTool("unmapped_tool", func(_ map[string]any) any { return map[string]any{"ok": true} })
	h.WaitForAgentClassValid("leakage-e2e", 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const (
		ns         = "default"
		sessName   = "artifact-view-http"
		sessionID  = ns + "/" + sessName
		artifactID = "artifact-e2eviewhttp"
	)
	ctx := context.Background()

	// Canonical SpiceDB subjects (base64 of the email) — never raw emails;
	// SpiceDB object-ids reject @/. — the bug case #3 reproduces.
	participant := e2e.CanonicalForFakeEmail("alice@example.com")
	stranger := e2e.CanonicalForFakeEmail("mallory@example.com")

	// === SpiceDB setup (mirrors TestArtifactViewAuthzChain verbatim). ===
	// A session whose owner is the participant → the participant has
	// agentsession#interact, and so artifact#view once parent is written.
	e2e.WriteRel(t, h, "agentsession", sessionID, "owner", "user", participant.String(), "")

	// Before the parent edge exists, even the participant cannot view it —
	// proves the later true is real, not trivially passing.
	h.AssertSpiceDB("artifact:"+artifactID, "view", participant.Subject(), false)

	// Persist the artifact through the REAL authorizer as the SYSTEM caller the
	// runner uses; this writes the caller-independent artifact#parent edge.
	az := spicedbauthorizer.New(h.SpiceDB)
	require.NoError(t, az.AuthorizePut(
		memory.WithCaller(ctx, "system:channelsd"),
		memory.Entry{
			Scope: memory.Scope{Kind: "session", ID: sessionID},
			Kind:  artifactkind.KindName,
			ID:    artifactID,
		}), "authorizer must write artifact#parent for a system caller")

	// The participant can now view; a stranger cannot.
	h.AssertSpiceDB("artifact:"+artifactID, "view", participant.Subject(), true)
	h.AssertSpiceDB("artifact:"+artifactID, "view", stranger.Subject(), false)

	// The two asserts above use FullyConsistent reads. The production
	// artifact-view path (internal/cmd/webd) reads with minimize_latency
	// (CheckArtifactView fullyConsistent=false), whose snapshot can lag the
	// just-written artifact#parent edge under suite contention. Wait for that
	// reader to settle before driving the HTTP happy path, so case 1's 200
	// doesn't race eventual consistency — the flake this guards.
	require.Eventually(t, func() bool {
		ok, err := h.SpiceDB.CheckArtifactView(ctx, artifactID, participant, false)
		return err == nil && ok
	}, 30*time.Second, 100*time.Millisecond,
		"minimize_latency view of artifact#parent must settle before the HTTP path")

	// === Shared HMAC key across all three roles. ===
	key := make([]byte, 32) // all-zero is fine for a test; the key is shared, not secret
	// channelsd mints the artifact-view link (iss=channelsd, aud=webd).
	linkMinter := passthroughlink.New(key,
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceWebd))
	// identityd mints the idd_session cookie (iss=identityd, aud=identityd) and
	// verifies the incoming link (any iss/aud — it's a locator). webd's
	// authenticate verifies the cookie with the same iss/aud expectations, and
	// Deps.VerifyLink verifies the link with channelsd/webd expectations. One
	// signer object covers identityd's cookie-mint role AND webd's verify role
	// because Verify takes the expected iss/aud as call options.
	iddSigner := passthroughlink.New(key,
		passthroughlink.WithIssuer(passthroughlink.IssuerIdentityd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))

	// === A minimal AgentSession CR for the link's SessionRef. ===
	// identityd's /oidc/login resolveKindForSessionRef Gets this and reads
	// spec.inputChannel.kind. Since identityd's Authenticators map is EMPTY,
	// any kind takes the trust-link fallback regardless of the value.
	require.NoError(t, h.K8s.Create(ctx, &spiceboxv1alpha1.AgentSession{
		ObjectMeta: metav1.ObjectMeta{Name: sessName, Namespace: ns},
		Spec: spiceboxv1alpha1.AgentSessionSpec{
			Class:  "leakage-e2e",
			Prompt: spiceboxv1alpha1.PromptSource{Inline: "noop"},
			InputChannel: &spiceboxv1alpha1.ChannelBinding{
				Name: "ch", Kind: "slack", Key: "thread:c:t", Capabilities: []string{"text"},
			},
		},
	}), "create AgentSession CR for the link SessionRef")

	// === Mint the artifact-view link channelsd would produce. ===
	// Subject is the CANONICAL participant id — the cookie minted from it must
	// authenticate as the participant (case #1 / #2 contrast).
	// SubjectVerified=true mirrors how channelsd's viewlink.Minter stamps it for
	// a channel-verified identity (e.g. a Slack workspace email lookup). It
	// records what the minter knew about the subject; it does NOT authenticate
	// the link's bearer, so it is not what gets this case its cookie — the
	// server's InsecureTrustLinks door is.
	linkRaw, err := linkMinter.Mint(passthroughlink.Payload{
		Purpose:         passthroughlink.PurposeArtifactView,
		ArtifactID:      artifactID,
		SessionRef:      sessionID,
		Subject:         identity.Subject(participant.String()),
		SubjectVerified: true,
		ExpiresAt:       time.Now().Add(10 * time.Minute).Unix(),
	})
	require.NoError(t, err, "mint artifact-view link")
	d, sig, ok := strings.Cut(linkRaw, ".")
	require.True(t, ok, "minted link must split into d.sig")
	linkQuery := "d=" + d + "&sig=" + sig

	// === Compose webd + identityd on ONE httptest host. ===
	ts := startComposedWebdIdentityd(t, h.SpiceDB, h.K8s, iddSigner)
	tsHost := strings.TrimPrefix(ts.URL, "http://")

	// ---- Case 1: full happy path (cookie-less, follows the redirect chain). ----
	t.Run("happy path: cookie-less GET → trust-link fallback → 200 rendered shell", func(t *testing.T) {
		jar, err := cookiejar.New(nil)
		require.NoError(t, err, "cookiejar")
		client := &http.Client{Jar: jar} // default CheckRedirect follows up to 10 hops

		resp, err := client.Get(ts.URL + "/artifact-view?" + linkQuery)
		require.NoError(t, err, "GET /artifact-view (follow redirects)")
		body := readHTTPBody(t, resp)

		require.Equal(t, http.StatusOK, resp.StatusCode,
			"the participant must reach the rendered shell after the redirect chain; body=%s", body)
		// The React document embeds the artifact-view app and bootstrap props.
		// ArtifactMeta returns no name so artifactName falls back to the artifact id.
		assert.Contains(t, body, `data-app="artifact-view"`, "happy path renders the artifact-view React app")
		assert.Contains(t, body, `"contentUrl":`, "bootstrap props carry the minted content URL")
		assert.Contains(t, body, artifactID, "bootstrap props contain the artifact id as the fallback artifactName")
		// The idd_session cookie was minted by the trust-link fallback and stuck.
		var gotCookie bool
		for _, c := range jar.Cookies(resp.Request.URL) {
			if c.Name == "idd_session" {
				gotCookie = true
			}
		}
		assert.True(t, gotCookie, "the trust-link fallback must have set the idd_session cookie")
	})

	// ---- Cases 2-4: precise single-response control (no redirect following). ----
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// mintCookie mints an idd_session cookie value for the given subject as
	// identityd would (iss=identityd, aud=identityd via iddSigner's defaults).
	mintCookie := func(subject string) string {
		raw, err := iddSigner.Mint(passthroughlink.Payload{
			Subject:   identity.Subject(subject),
			ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
		})
		require.NoError(t, err, "mint idd_session cookie for %s", subject)
		return raw
	}

	// getWithCookie issues a GET to path on the trusted host with exactly the
	// supplied idd_session cookie and returns (status, body) of the immediate
	// (un-followed) response.
	getWithCookie := func(t *testing.T, path, cookieVal string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		require.NoError(t, err, "build request")
		req.Host = tsHost
		req.AddCookie(&http.Cookie{Name: "idd_session", Value: cookieVal})
		resp, err := noRedirect.Do(req)
		require.NoError(t, err, "do request")
		return resp.StatusCode, readHTTPBody(t, resp)
	}

	t.Run("stranger cookie → 403 you-do-not-have-access", func(t *testing.T) {
		status, body := getWithCookie(t, "/artifact-view?"+linkQuery, mintCookie(stranger.String()))
		assert.Equal(t, http.StatusForbidden, status, "stranger must be denied; body=%s", body)
		assert.Contains(t, body, `data-app="system"`, "denied request renders the system error page")
		assert.Contains(t, body, "You do not have access",
			"denied request must report the access denial")
	})

	t.Run("raw-email cookie → 500 authorization-error (canonicalization regression)", func(t *testing.T) {
		// Subject is the RAW email, not the canonical base64. CheckArtifactView
		// builds artifact:<id>#view@user:<alice@example.com>; the '@'/'.' make
		// SpiceDB reject the object-id, so the check ERRORS → 500. This is the
		// exact reason the link Subject must always be canonical.
		status, body := getWithCookie(t, "/artifact-view?"+linkQuery, mintCookie("alice@example.com"))
		assert.Equal(t, http.StatusInternalServerError, status,
			"a raw-email subject must error the SpiceDB check; body=%s", body)
		assert.Contains(t, body, "Authorization error",
			"the 500 must surface the authorization-error body")
	})

	t.Run("bad/expired link → 403 invalid-or-expired", func(t *testing.T) {
		// Garbage d/sig with an OTHERWISE-valid participant cookie: VerifyLink
		// fails before any authz check, so it 403s with the invalid-link body.
		status, body := getWithCookie(t, "/artifact-view?d=AA&sig=BB", mintCookie(participant.String()))
		assert.Equal(t, http.StatusForbidden, status, "a bad link must 403; body=%s", body)
		assert.Contains(t, body, "invalid or has expired",
			"a bad/expired link must report invalid-or-has-expired")
	})
}

// startComposedWebdIdentityd mounts a real webui.Server (with the artifact-view
// WebUI) and a real identityd.Server on ONE httptest host, mirroring
// production's rehost of identityd on webd's trusted origin. It returns the
// started test server both surfaces dispatch against.
//
// identityd's Authenticators map is EMPTY and no ClusterIdentityProvider is
// configured, so case 1 relies on the InsecureTrustLinks door in handleOIDCLogin.
// Cases 2-4 send pre-minted cookies
// directly and never hit /oidc/login. The webui host getters return the httptest
// host:port (trusted == sandbox, fine for this auth test); beginLogin's base URL
// is the full ts.URL so the 302 lands on /oidc/login on the same host.
func startComposedWebdIdentityd(
	t *testing.T,
	spdb interface {
		CheckArtifactView(ctx context.Context, artifactID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
		CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	},
	k8s client.Client,
	iddSigner *passthroughlink.Signer,
) *httptest.Server {
	t.Helper()

	// Bind unstarted first so ts.URL (and thus the host getters) is computable
	// before we construct the handlers that close over it.
	var webuiServer *webui.Server
	var identitydServer *identityd.Server
	mux := http.NewServeMux()
	ts := httptest.NewUnstartedServer(mux)
	ts.Start()
	t.Cleanup(ts.Close)

	baseURL := ts.URL                              // e.g. http://127.0.0.1:PORT
	host := strings.TrimPrefix(baseURL, "http://") // 127.0.0.1:PORT (matches r.Host)
	hostGetter := func() string { return host }    // webui.hostOnly strips the :port

	// webd's authenticate: verify the idd_session cookie, return the subject.
	// Replicates internal/cmd/webd/main.go's authenticate verbatim.
	authenticate := func(r *http.Request) (string, bool) {
		c, err := r.Cookie("idd_session")
		if err != nil || c.Value == "" {
			return "", false
		}
		p, err := iddSigner.Verify(c.Value,
			passthroughlink.WithExpectedIssuer(passthroughlink.IssuerIdentityd),
			passthroughlink.WithExpectedAudience(passthroughlink.AudienceIdentityd))
		if err != nil || p.Subject == "" {
			return "", false
		}
		return p.Subject.String(), true
	}

	// webd's beginLogin: cookie-less GET with d+sig → 302 to identityd
	// /oidc/login with a `next` that returns to the originally-requested page.
	// Replicates internal/cmd/webd/main.go's beginLogin verbatim: next is a same-origin
	// RELATIVE path (RequestURI = "/path?query") so identityd's safeNext guard
	// (open-redirect defense) accepts it.
	beginLogin := func(r *http.Request) (string, bool) {
		q := r.URL.Query()
		d, sig := q.Get("d"), q.Get("sig")
		if d == "" || sig == "" {
			return "", false
		}
		b := strings.TrimRight(baseURL, "/")
		next := r.URL.RequestURI() // relative path only — safeNext requires this
		return b + "/oidc/login?d=" + url.QueryEscape(d) + "&sig=" + url.QueryEscape(sig) +
			"&next=" + url.QueryEscape(next), true
	}

	deps := &composedAVDeps{
		spdb:         spdb,
		cookieSigner: iddSigner,
		trustedURL:   baseURL,
		sandboxURL:   baseURL,
		logger:       logr.Discard(),
	}

	var err error
	webuiServer, err = webui.NewServer(
		authenticate, beginLogin,
		hostGetter, hostGetter, // trusted == sandbox host (this auth test only)
		func(string) bool { return true }, // sharedOriginOK: collapse origins onto one host
		deps,
		[]webui.WebUI{artifactview.New()},
	)
	require.NoError(t, err, "build webui server")

	identitydServer = identityd.NewServer(identityd.Deps{
		K8s:             k8s,
		LinkSigner:      iddSigner,
		ExternalBaseURL: func() string { return baseURL },
		Authenticators:  map[string]channelkinds.WebAuthenticator{}, // EMPTY → trust-link fallback
		// No cluster IdP and no authenticator: without this the link is refused
		// rather than trusted, which is the fail-closed default for a real
		// install. This scenario is exercising webd's authorization chain, not
		// identityd's sign-in policy, so it opts into the dev-only door.
		InsecureTrustLinks: true,
	})

	// identityd owns /oidc/*; webd owns everything else (incl. /artifact-view).
	mux.Handle("/oidc/", identitydServer.Handler())
	mux.Handle("/", webuiServer)

	return ts
}

// errLinkPurpose / errLinkMissingRefs mirror the (unexported) error conditions
// internal/cmd/webd's artifactViewDeps.VerifyLink returns; the exact text is immaterial
// to the test (only that VerifyLink fails closed when they trip).
var (
	errLinkPurpose     = errors.New("link purpose is not artifact_view")
	errLinkMissingRefs = errors.New("link missing artifactId/sessionRef")
)

// composedAVDeps is a test-local artifactview.Deps with the TWO real methods
// (VerifyLink + CheckView) that the HTTP authorization path exercises, plus
// renderable stubs (copied from artifactview's fakeAV) so the happy path
// reaches a 200-rendered shell. It mirrors internal/cmd/webd's artifactViewDeps for
// VerifyLink/CheckView.
type composedAVDeps struct {
	spdb interface {
		CheckArtifactView(ctx context.Context, artifactID string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
		CheckInteract(ctx context.Context, ns, name string, canonicalID identity.CanonicalUserID, fullyConsistent bool) (bool, error)
	}
	cookieSigner *passthroughlink.Signer
	trustedURL   string
	sandboxURL   string
	logger       logr.Logger
}

// VerifyLink — real: verify the channelsd→webd link (iss=channelsd, aud=webd),
// require the artifact_view purpose + artifactId/sessionRef. Mirrors
// internal/cmd/webd/main.go's artifactViewDeps.VerifyLink.
func (d *composedAVDeps) VerifyLink(raw string) (string, string, string, error) {
	p, err := d.cookieSigner.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	if err != nil {
		return "", "", "", err
	}
	if p.Purpose != passthroughlink.PurposeArtifactView {
		return "", "", "", errLinkPurpose
	}
	if p.ArtifactID == "" || p.SessionRef == "" {
		return "", "", "", errLinkMissingRefs
	}
	return p.ArtifactID, p.SessionRef, p.BackLink, nil
}

// CheckView — real: the exact gate internal/cmd/webd calls. Strips the user: prefix
// like every other SpiceDB-id consumer.
func (d *composedAVDeps) CheckView(ctx context.Context, artifactID, subject string) (bool, error) {
	return d.spdb.CheckArtifactView(ctx, artifactID, identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"), "test fixture"), false)
}

// CheckInteract — real, mirroring internal/cmd/webd: agentsession#interact gates the
// session-scoped live mirrors, fullyConsistent so a just-granted relationship
// is visible immediately. The revision stream stays on CheckView alone.
func (d *composedAVDeps) CheckInteract(ctx context.Context, ns, sess, subject string) (bool, error) {
	return d.spdb.CheckInteract(ctx, ns, sess, identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"), "test fixture"), true)
}

// --- renderable stubs (copied from artifactview's fakeAV) so the shell
//     reaches 200 once CheckView passes. ---

func (d *composedAVDeps) ArtifactMeta(_ context.Context, _, _, _ string) (string, string, error) {
	return "", "", nil // no name → shell falls back to the artifact id
}
func (d *composedAVDeps) ChannelKind(_ context.Context, _, _ string) (string, error) {
	return "", nil // no thread-link icon needed for this auth test
}
func (d *composedAVDeps) ResolveRender(_ context.Context, _, _, _ string) (string, error) {
	return "ar-1", nil
}
func (d *composedAVDeps) ContentRender(_ context.Context, _, _, _ string) (string, bool, error) {
	return "ar-1", true, nil // standalone artifact in this auth test: render ready
}
func (d *composedAVDeps) RenderIsBundledOnly(_ context.Context, _, _, _ string) bool {
	return false
}

// RenderKind: this suite drives GET /artifact-view (VerifyLink/CheckView
// authorization), never GET /content, so rewriteArtifactRefs's RenderKind
// call is not on any path these subtests exercise. "html" is still the
// correct constant, not a guess: ContentRender/ResolveRender always resolve
// to renderName "ar-1", and FetchRender/FetchRenderBundle stub that same
// render's bytes as "<html>...</html>" with mime "text/html" — so "html" is
// what this fake actually composes for the only renderName it ever serves.
func (d *composedAVDeps) RenderKind(_ context.Context, _, _, _ string) (string, error) {
	return "html", nil
}

// PreviewChildRender — standalone auth test: it never reaches the bundled-only
// older-revision branch, so a harmless "not generated" stub suffices.
func (d *composedAVDeps) PreviewChildRender(_ context.Context, _, _, _ string) (string, bool, error) {
	return "", false, nil
}
func (d *composedAVDeps) SignContentToken(_, _, _, _ string) (string, error) { return "tok", nil }
func (d *composedAVDeps) VerifyContentToken(_ string) (string, string, string, error) {
	return "default", "s1", "ar-1", nil
}

// ResolveAssetURL / VerifyAssetToken: this suite exercises the
// VerifyLink/CheckView authorization path for the shell, not asset-ref
// rewriting — harmless stubs, same rationale as the other renderable stubs
// below.
func (d *composedAVDeps) ResolveAssetURL(_ context.Context, _, _, _ string) (string, bool, error) {
	return "", false, nil
}
func (d *composedAVDeps) VerifyAssetToken(_ string) (string, string, string, error) {
	return "", "", "", nil
}
func (d *composedAVDeps) FetchRender(_ context.Context, _, _, _ string) ([]byte, string, error) {
	return []byte("<html><head></head><body>hi</body></html>"), "text/html", nil
}

// FetchRenderBundle: this suite exercises the VerifyLink/CheckView
// authorization chain, not the download path's bundle-vs-raw
// distinction — a harmless stub matching FetchRender.
func (d *composedAVDeps) FetchRenderBundle(_ context.Context, _, _, _ string) ([]byte, string, error) {
	return []byte("<html><head></head><body>hi</body></html>"), "text/html", nil
}
func (d *composedAVDeps) ServeTransform(_ context.Context, _, _, _ string, content []byte) []byte {
	return content
}
func (d *composedAVDeps) ListRevisions(_ context.Context, _, _, _ string) ([]artifactview.RevisionMeta, error) {
	return nil, nil
}
func (d *composedAVDeps) WatchSessionStatus(_ context.Context, _, _ string) (<-chan artifactview.StatusSnapshot, error) {
	return nil, nil // no live status stream in this auth test
}
func (d *composedAVDeps) WatchMessages(_ context.Context, _, _ string) (<-chan artifactview.MirrorMessage, error) {
	return nil, nil // no chat mirror stream in this auth test
}
func (d *composedAVDeps) SessionViews(_ context.Context, _, _ string) []string {
	return nil // no session_views interactions in this auth test (read-only viewer)
}
func (d *composedAVDeps) TrustedOrigin() string  { return d.trustedURL }
func (d *composedAVDeps) SandboxBaseURL() string { return d.sandboxURL }
func (d *composedAVDeps) Logger() logr.Logger    { return d.logger }

// Compile-time guard: composedAVDeps MUST implement the full artifactview.Deps
// interface. The webui server only requires webui.Deps at compile time and casts
// to artifactview.Deps at runtime (fail-closed to no routes on a miss), so a
// dropped method silently 404s the viewer instead of failing the build — which
// is exactly what happened when a method was lost during an unrelated edit. This
// assertion turns that class of drift back into a compile error.
var _ artifactview.Deps = (*composedAVDeps)(nil)

// readHTTPBody reads + closes resp.Body and returns it as a string for
// substring assertions.
func readHTTPBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response body")
	return string(b)
}
