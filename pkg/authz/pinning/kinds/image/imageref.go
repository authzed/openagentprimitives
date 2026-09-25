// Pure OCI image ref helpers. These carry the subtle host-port-vs-tag
// parsing: a ":" in the registry host portion (port number) is not a tag
// separator. The tag separator is a ":" only when it appears AFTER the last
// "/" in the ref.
package image

import "strings"

// SplitRef splits an OCI image ref into (repo, tag, digest). Exactly one of
// tag/digest may be non-empty; a bare ref has neither. A ":" in the registry
// host (port) is not a tag separator — only a ":" appearing AFTER the last
// "/" is treated as the tag separator.
func SplitRef(ref string) (repo, tag, digest string) {
	// Digest takes precedence: "@" always separates repo from digest.
	if at := strings.Index(ref, "@"); at >= 0 {
		return ref[:at], "", ref[at+1:]
	}
	// Find the last "/"; a ":" after it is the tag separator.
	lastSlash := strings.LastIndex(ref, "/")
	// Search for ":" only in the part after the last slash.
	suffix := ref[lastSlash+1:]
	if colon := strings.Index(suffix, ":"); colon >= 0 {
		// colon is within suffix; the full index in ref is lastSlash+1+colon
		splitAt := lastSlash + 1 + colon
		return ref[:splitAt], ref[splitAt+1:], ""
	}
	return ref, "", ""
}

// WithDigest rewrites ref to launch-by-digest form "<repo>@<digest>",
// dropping any tag. Digest must be "sha256:…".
func WithDigest(ref, digest string) string {
	repo, _, _ := SplitRef(ref)
	return repo + "@" + digest
}

// DigestFromImageID extracts "sha256:…" from a kubelet containerStatuses
// imageID ("docker-pullable://repo@sha256:…", "repo@sha256:…", or bare
// "sha256:…"). Returns "" when no digest is present.
func DigestFromImageID(imageID string) string {
	// Strip the docker-pullable:// scheme if present.
	s := strings.TrimPrefix(imageID, "docker-pullable://")
	// After stripping, look for "@sha256:" — the digest follows "@".
	if at := strings.Index(s, "@"); at >= 0 {
		after := s[at+1:]
		if strings.HasPrefix(after, "sha256:") {
			return after
		}
		return ""
	}
	// Bare digest: the entire string is "sha256:…".
	if strings.HasPrefix(s, "sha256:") {
		return s
	}
	return ""
}
