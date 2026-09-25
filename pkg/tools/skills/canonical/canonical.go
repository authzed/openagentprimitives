// Package canonical parses and manipulates canonical skill names. A canonical
// name is the stable, collision-free identity of a skill, derived from its git
// provenance (or a reserved "local" authority for hand-authored skills):
//
//	<repo-locator>//<subpath>[@<ref>]   (git-sourced)
//	local//<name>                       (hand-authored, cluster)
//	local/<namespace>//<name>           (hand-authored, namespaced)
//
// The "//" is the literal separator between the repo locator and the in-repo
// subpath (Terraform-style). The version, when present, is part of the
// identity: two different "@<ref>" values are two different skills.
package canonical

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/authz/pinning"
)

// Name is a parsed canonical skill name.
type Name struct {
	Authority string // repo-locator (git) or "local"/"local/<namespace>"
	Subpath   string // in-repo path to the skill dir, or the hand-authored name
	Ref       string // tag/branch/sha; "" when unpinned
	IsLocal   bool   // true for the reserved "local" authority
}

// PinStrength classifies how tightly a name is pinned, syntactically. Whether a
// PinNamed ref is a (movable) tag or a (rolling) branch can only be known after
// git resolution. It is an alias of the shared pinning.Strength so skill pin
// strengths and the generic dependency-pinning machinery share one vocabulary.
type PinStrength = pinning.Strength

const (
	PinFrozen   = pinning.StrengthFrozen   // @<sha>: immutable
	PinNamed    = pinning.StrengthNamed    // @<tag-or-branch>: refined at resolve time
	PinUnpinned = pinning.StrengthUnpinned // no @ref: rolling
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// Parse parses an already-normalized canonical name. It rejects un-normalized
// inputs (URL schemes, trailing .git) so the stored identity is canonical.
func Parse(s string) (Name, error) {
	if s == "" {
		return Name{}, fmt.Errorf("empty canonical name")
	}
	if strings.Contains(s, "://") {
		return Name{}, fmt.Errorf("canonical name must not contain a URL scheme: %q", s)
	}
	idx := strings.Index(s, "//")
	if idx < 0 {
		return Name{}, fmt.Errorf("canonical name missing '//' repo/subpath separator: %q", s)
	}
	authority, rest := s[:idx], s[idx+2:]
	if authority == "" {
		return Name{}, fmt.Errorf("canonical name has empty authority: %q", s)
	}
	if strings.HasSuffix(authority, ".git") {
		return Name{}, fmt.Errorf("canonical name authority must not end in .git: %q", s)
	}
	var ref string
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		ref, rest = rest[at+1:], rest[:at]
		if ref == "" {
			return Name{}, fmt.Errorf("canonical name has empty ref after '@': %q", s)
		}
	}
	if rest == "" {
		return Name{}, fmt.Errorf("canonical name has empty subpath: %q", s)
	}
	return Name{
		Authority: authority,
		Subpath:   rest,
		Ref:       ref,
		IsLocal:   authority == "local" || strings.HasPrefix(authority, "local/"),
	}, nil
}

// String renders the canonical name back to its string form.
func (n Name) String() string {
	s := n.Authority + "//" + n.Subpath
	if n.Ref != "" {
		s += "@" + n.Ref
	}
	return s
}

// PinStrength classifies the ref syntactically.
func (n Name) PinStrength() PinStrength {
	switch {
	case n.Ref == "":
		return PinUnpinned
	case shaRe.MatchString(n.Ref):
		return PinFrozen
	default:
		return PinNamed
	}
}

// LastSegment is the skill directory's basename — the value the frontmatter
// "name" must match.
func (n Name) LastSegment() string {
	seg := n.Subpath
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	return seg
}

var unsafeRe = regexp.MustCompile(`[^a-z0-9.-]+`)

// SafeSlug derives a deterministic, RFC1123-safe metadata.name from the
// canonical name: "<sanitized-last-segment>-<8 hex of sha256(canonical)>". The
// hash suffix keeps it unique across repos/versions that share a last segment.
func (n Name) SafeSlug() string {
	base := strings.Trim(unsafeRe.ReplaceAllString(strings.ToLower(n.LastSegment()), "-"), "-.")
	if base == "" {
		base = "skill"
	}
	sum := sha256.Sum256([]byte(n.String()))
	slug := base + "-" + hex.EncodeToString(sum[:])[:8]
	if len(slug) > 253 {
		slug = slug[:253]
	}
	return slug
}

// MatchPattern reports whether a concrete canonical skill name matches a ceiling
// pattern. A pattern is either an exact string or ends in "*"/"**" (a prefix
// wildcard): "github.com/org/**" matches any name with that prefix;
// "repo//skills/x@*" matches any version; "*" alone matches everything. This is
// deliberately prefix-only (not full shell-glob) — it covers the "whole org" and
// "any version" ceiling use cases without arbitrary-glob complexity.
func MatchPattern(pattern, name string) bool {
	switch {
	case pattern == name:
		return true
	case strings.HasSuffix(pattern, "**"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "**"))
	case strings.HasSuffix(pattern, "*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return false
	}
}

// Normalize reduces a repo URL to a canonical repo-locator: strips the scheme
// (or scp-like git@host: form), userinfo, trailing slash, and trailing .git,
// so a SkillSource's repo URL yields the authority half of a canonical name.
func Normalize(repoURL string) string {
	s := strings.TrimSpace(repoURL)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else if at := strings.Index(s, "@"); at >= 0 && strings.Contains(s[at:], ":") {
		// scp-like: git@github.com:org/repo.git
		s = strings.Replace(s[at+1:], ":", "/", 1)
	}
	if at := strings.Index(s, "@"); at >= 0 {
		if slash := strings.Index(s, "/"); slash == -1 || at < slash {
			s = s[at+1:] // drop leftover userinfo@
		}
	}
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	return strings.TrimSuffix(s, "/")
}
