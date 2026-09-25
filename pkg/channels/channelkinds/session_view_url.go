package channelkinds

import (
	"fmt"
	"net/url"
	"strings"
)

// ComposeSessionViewURL composes the durable session-view page URL
// "<base>/session-view/<ns>/<name>" for sessionRef ("ns/name"). This is the
// ONE place the shape is defined; every SessionViewMinter calls it rather than
// composing the path inline.
//
// Returns ("", nil) when base is empty — webd's external-URL ConfigMap exists
// but is not populated yet — which callers treat as "not configured yet" and
// surface loudly rather than composing a broken link. Returns an error only
// for a malformed sessionRef, which is a caller bug, not a config gap.
func ComposeSessionViewURL(base, sessionRef string) (string, error) {
	ns, name, ok := strings.Cut(sessionRef, "/")
	if !ok || ns == "" || name == "" {
		return "", fmt.Errorf("malformed sessionRef %q (want \"ns/name\")", sessionRef)
	}
	trimmed := strings.TrimRight(base, "/")
	if trimmed == "" {
		return "", nil
	}
	return trimmed + "/session-view/" + url.PathEscape(ns) + "/" + url.PathEscape(name), nil
}
