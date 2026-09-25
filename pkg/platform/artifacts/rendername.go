package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// The name an ArtifactRender CR is created under, and its inverse.
//
// Both meta tools that create a render CR (artifact_prepare, and
// artifact_offer_view for a preview child) mint the name here rather than
// formatting it themselves, so there is exactly ONE spelling of the format in
// the repo. That matters because the name is also a model-facing value: it is
// the `handle` artifact_prepare returns and the handle artifact_await and
// ResolveRevisionTarget accept back, so a replay has to be able to RECOGNIZE
// one in a recorded result (IsRenderName) as well as produce one. Two spellings
// — a formatter here and a matcher in the test-support package — would drift on
// the first reword, and the symptom would be a captured bundle silently failing
// to pin a handle it then diverges on.

// renderNamePrefix opens every ArtifactRender CR name.
const renderNamePrefix = "ar-"

// renderNameSuffixLen is how many hex digits close one. Six, not the sixteen
// every memory-kind id carries: the name is scoped to a session's renders
// rather than to the cluster, and a k8s object name has 253 bytes to hold a
// session name as well.
const renderNameSuffixLen = 6

// RenderName mints the name for a new ArtifactRender CR belonging to session.
//
// Random rather than derived: the CR is created before anything durable records
// it, so there is no earlier value to derive from, and two renders of the same
// artifact in one session must not collide.
func RenderName(session string) string {
	b := make([]byte, renderNameSuffixLen)
	_, _ = rand.Read(b)
	return renderNamePrefix + session + "-" + hex.EncodeToString(b)[:renderNameSuffixLen]
}

// NewRenderName mints the name for a new ArtifactRender CR belonging to
// session, through the service's seam.
//
// The seam exists for the same reason NewArtifactID's does, and is described in
// full on WithRenderNameMinter: a whole-session REPLAY hands back the very
// handles the captured run returned to the model. nil — every production
// binary — is RenderName.
func (s *Service) NewRenderName(session string) string {
	if s.newRenderName == nil {
		return RenderName(session)
	}
	return s.newRenderName(session)
}

// IsRenderName reports whether s is a name RenderName produced.
//
// The inverse of the format above and deliberately adjacent to it. Matching is
// on the WHOLE string, and the session segment must be non-empty, so a bare
// "ar-abcdef" (no session) is not one.
//
// Used by the replay's bundle format to recognize a render handle inside a
// recorded tool result, which is how a handle gets pinned rather than
// re-minted. It cannot be a prefix test: "ar-" is short enough to open plenty
// of prose, and the fixed-width hex tail is what makes the match specific.
func IsRenderName(s string) bool {
	if !strings.HasPrefix(s, renderNamePrefix) {
		return false
	}
	rest := s[len(renderNamePrefix):]
	if len(rest) < renderNameSuffixLen+2 { // at least one session char, the '-', and the tail
		return false
	}
	sep := len(rest) - renderNameSuffixLen - 1
	if rest[sep] != '-' {
		return false
	}
	session := rest[:sep]
	if session == "" {
		return false
	}
	if !isDNS1123(session) {
		return false
	}
	return isLowerHex(rest[sep+1:])
}

// isDNS1123 reports whether s is made only of the bytes a k8s object name may
// contain. The session segment is one, so anything else is not a render name
// however well the tail matches.
func isDNS1123(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			continue
		}
		return false
	}
	return true
}

// isLowerHex reports whether every byte is a lowercase hex digit.
func isLowerHex(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
