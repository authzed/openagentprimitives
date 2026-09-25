package artifactview

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// errNoViewParams is returned by resolveViewParams when a request carries
// NEITHER a signed passthrough link NOR both unsigned artifact params — the
// caller maps it to the same 403 "link invalid or expired" as a bad signature.
var errNoViewParams = errors.New("artifactview: no signed link and no artifactId+sessionRef params")

// resolveViewParams resolves the (artifactID, sessionRef, backLink) for an
// artifact-view request from EITHER of the two ways this page can be
// addressed:
//
//   - a SIGNED passthrough link (d+sig), minted for one subject with a TTL at
//     the moment a known person clicks — the channelsd/Slack path; or
//   - a DURABLE link naming the resource (artifactId+sessionRef), composed by
//     channelkinds.ComposeArtifactViewURL and safe to write anywhere, because
//     it carries no capability at all. It is what a GitHub check run's details
//     link uses, and what a platform admin's own address bar uses.
//
// Neither form is the authorization boundary. The signature scopes a link to
// one recipient; it does not decide who may view the artifact — the downstream
// CheckView does, against the AUTHENTICATED cookie subject, for both forms
// alike. That is what lets the durable form exist: a reader who forwards the
// URL forwards no access with it, because the URL never carried any.
//
// On the signed path a bad signature is a hard error — it does NOT fall through
// to the durable path. Otherwise both artifactId and sessionRef must be
// non-empty; backLink is "" for the durable path, which names no originating
// thread. Every caller still runs CheckView on the result before serving
// anything.
func resolveViewParams(av Deps, q url.Values) (artifactID, sessionRef, backLink string, err error) {
	// A genuine signed link ALWAYS carries both d and sig, so require both to
	// select the signed branch. A stray/partial d-or-sig on an otherwise-durable
	// request then falls through to the artifactId/sessionRef params (still
	// CheckView-gated) instead of hard-403ing; a real signed link is unaffected.
	if q.Get("d") != "" && q.Get("sig") != "" {
		return av.VerifyLink(q.Get("d") + "." + q.Get("sig"))
	}
	artifactID, sessionRef = q.Get("artifactId"), q.Get("sessionRef")
	if artifactID == "" || sessionRef == "" {
		return "", "", "", errNoViewParams
	}
	return artifactID, sessionRef, "", nil
}

// urlsFor mints ONE content-capability token for a render and returns both the
// sandbox-origin host URL (the shell frames this; the host page self-embeds the
// content from the same token) and the raw content URL (the host swaps its inner
// frame to this on a revision change). Tokens are short-TTL and minted fresh per
// push so each push carries a live capability.
//
// Fails closed when the configured sandbox base is not an http(s) origin. Both
// URLs are framed by the shell, and hostUrl lands on an iframe carrying
// sandbox="allow-scripts allow-same-origin" (ui/ArtifactView.tsx), so whatever
// origin these resolve to gets script execution plus same-origin access to
// itself. Two inputs must never reach that sink:
//
//   - a non-http(s) base ("javascript:…"), which would execute in the trusted
//     shell origin; and
//   - an EMPTY base, which is a LIVE state: `oap install` seeds the external-URL
//     ConfigMap with "" whenever it manages external access, and webd warns and
//     keeps serving until the real value lands. Concatenation would then yield
//     the shell-RELATIVE "/artifact-host?ct=…", loading sandbox content in the
//     trusted origin and collapsing the two-origin split.
//
// webui.StrictOrigin maps both to "", and every caller degrades the viewer on
// the error rather than framing anything.
func urlsFor(av Deps, ns, sess, renderName, artifactID string) (hostURL, contentURL string, err error) {
	base := sandboxOrigin(av)
	if base == "" {
		return "", "", fmt.Errorf("artifactview: sandbox base URL %q is not an http(s) origin", av.SandboxBaseURL())
	}
	ct, err := av.SignContentToken(ns, sess, renderName, artifactID)
	if err != nil {
		return "", "", err
	}
	return base + "/artifact-host?ct=" + ct, base + "/content?ct=" + ct, nil
}

// sandboxOrigin reduces the configured sandbox base URL to a bare
// "scheme://host" origin, or "" when it is not an http(s) origin.
//
// One helper for every read of Deps.SandboxBaseURL in this package, so the
// URL-minting sink (urlsFor) and the props sink (shellPageBuild's
// SandboxOrigin, which the client uses as a postMessage target and compares
// against event.origin) can never disagree about what the sandbox origin is.
func sandboxOrigin(av Deps) string { return webui.StrictOrigin(av.SandboxBaseURL()) }

// splitRef splits a "<ns>/<name>" session ref.
func splitRef(ref string) (ns, name string) {
	ns, name, _ = strings.Cut(ref, "/")
	return ns, name
}
